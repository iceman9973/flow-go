package flowapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/auth"
	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/httpx"
	"github.com/kodelyx/flow-go/flow-go/internal/recaptcha"
)

// BrowserFingerprint is the request identity to present upstream.
//
// It is captured from the browser that produced the reCAPTCHA token, and it must
// be used verbatim. A generation request whose user-agent or sec-ch-ua does not
// match the client the assessment was made for is rejected with
// 403 PUBLIC_ERROR_UNUSUAL_ACTIVITY, which reads as a captcha failure but is
// really a fingerprint mismatch.
type BrowserFingerprint struct {
	UserAgent string
	Language  string
	SecChUa   string
	Platform  string
	Mobile    string
}

// Options configures a Flow client.
type Options struct {
	// ProjectID is the Flow project to generate into. Empty falls back to the
	// first project advertised by Labs, then to config.DefaultProject.
	ProjectID string
	// Tier is the paygate tier to declare. Empty derives it from the account sku.
	Tier string
	// AccountID labels this client in logs and metrics.
	AccountID string
	// Captcha supplies reCAPTCHA tokens. Nil means no token.
	Captcha recaptcha.Provider
	// ProxyURL routes this account's traffic through one exit IP.
	ProxyURL string
	// Fingerprint is the browser identity to present. Nil falls back to the
	// generic Chrome profile, which is fine for read-only calls but will get
	// generation requests rejected.
	Fingerprint *BrowserFingerprint
	// DisableRateLimit bypasses the per-account spacing. Only for tests.
	DisableRateLimit bool
}

// Client is a single account's view of the Flow API.
type Client struct {
	auth *auth.Provider
	hc   *httpx.Client
	opts Options

	limiter *limiter

	mu       sync.Mutex
	lastSku  string
	requests int64
	failures int64
}

// Result is a decoded upstream response.
type Result struct {
	Status  int
	Data    map[string]any
	Body    []byte
	Elapsed time.Duration
	// Attempts records how many HTTP calls were made, including token retries.
	Attempts int
}

// JSON unmarshals the response body into out.
func (r *Result) JSON(out any) error {
	if len(r.Body) == 0 {
		return fmt.Errorf("flow: empty response body")
	}
	return json.Unmarshal(r.Body, out)
}

// ErrorBody returns the parsed error envelope, if the response carried one.
func (r *Result) ErrorBody() *ErrorBody {
	if len(r.Body) == 0 {
		return nil
	}
	var wrapper struct {
		Error *ErrorBody `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &wrapper); err != nil {
		return nil
	}
	return wrapper.Error
}

// New builds a client for one account.
func New(provider *auth.Provider, hc *httpx.Client, opts Options) *Client {
	return &Client{
		auth:    provider,
		hc:      hc,
		opts:    opts,
		limiter: newLimiter(config.MaxConcurrentRequests, time.Duration(config.RequestMinInterval*float64(time.Second))),
	}
}

// AccountID returns the label this client was built with.
func (c *Client) AccountID() string { return c.opts.AccountID }

// Stats reports request and failure counts for observability.
func (c *Client) Stats() (requests, failures int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests, c.failures
}

// Session exposes the underlying token provider so callers can inspect it.
func (c *Client) Session() *auth.Provider { return c.auth }

/* ------------------------------------------------------------------ *
 * Request execution
 * ------------------------------------------------------------------ */

// Call issues a POST against a Flow endpoint, injecting auth, client context,
// and a captcha token, and self-healing once on an auth failure.
func (c *Client) Call(ctx context.Context, endpoint string, body any, captchaAction string) (*Result, error) {
	return c.call(ctx, "POST", endpoint, body, captchaAction)
}

// Get issues a GET against a Flow endpoint.
func (c *Client) Get(ctx context.Context, endpoint string, captchaAction string) (*Result, error) {
	return c.call(ctx, "GET", endpoint, nil, captchaAction)
}

// mintCaptcha fetches the token an endpoint needs, when it needs one.
//
// The two ways this comes back empty are not the same thing, and the error is
// what tells them apart:
//
//   - A provider that is deliberately off answers with an empty token and *no*
//     error. That is a configuration the operator asked for, and the request
//     goes out without one.
//   - A provider that failed answers with an error, and that stops the request.
//     It used to be logged and the call sent anyway, on the theory that Flow
//     accepts an empty token for most endpoints — and it does, for most. When it
//     does not, the refusal is a generic 400/403 that never mentions the
//     captcha, so the cause is invisible and the generation is already spent.
func mintCaptcha(ctx context.Context, provider recaptcha.Provider, action string) (string, error) {
	if action == "" || provider == nil {
		return "", nil
	}
	token, err := provider.Token(ctx, action)
	if err != nil {
		return "", fmt.Errorf("captcha mint failed: %w", err)
	}
	return token, nil
}

func (c *Client) call(ctx context.Context, method, endpoint string, body any, captchaAction string) (*Result, error) {
	if !c.opts.DisableRateLimit {
		if err := c.limiter.acquire(ctx); err != nil {
			return nil, err
		}
		defer c.limiter.release()
	}

	var lastErr error
	// Two attempts total: the first with the current token, the second after
	// forcing a fresh one. A second auth failure is a real credential problem,
	// not a stale token, so it is reported rather than retried forever.
	for attempt := 1; attempt <= 2; attempt++ {
		result, err := c.attempt(ctx, method, endpoint, body, captchaAction)
		if err == nil {
			c.recordSuccess()
			result.Attempts = attempt
			return result, nil
		}

		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Unauthenticated() && attempt == 1 {
			log.Printf("flow[%s]: %v — minting a fresh token and retrying", c.opts.AccountID, apiErr)
			c.auth.Invalidate()
			if _, refreshErr := c.auth.ForceRefresh(ctx); refreshErr != nil {
				log.Printf("flow[%s]: token refresh failed: %v", c.opts.AccountID, refreshErr)
				c.recordFailure()
				return nil, fmt.Errorf("%w (token refresh also failed: %v)", apiErr, refreshErr)
			}
			lastErr = apiErr
			continue
		}

		c.recordFailure()
		return nil, err
	}

	c.recordFailure()
	return nil, lastErr
}

func (c *Client) attempt(ctx context.Context, method, endpoint string, body any, captchaAction string) (*Result, error) {
	start := time.Now()

	session, err := c.auth.Session(ctx)
	if err != nil {
		return nil, err
	}

	token := session.AccessToken
	projectID := session.ResolveProjectID(c.opts.ProjectID)
	tier := c.resolveTier(session)

	// Only fetched when the endpoint actually uses one, and fatal when it fails
	// to arrive. See mintCaptcha for why "deliberately off" and "broken" are
	// different states and only one of them may be quiet.
	captchaToken, err := mintCaptcha(ctx, c.opts.Captcha, captchaAction)
	if err != nil {
		return nil, err
	}

	payload, err := c.injectContext(body, projectID, tier, captchaToken)
	if err != nil {
		return nil, err
	}

	fullURL, err := c.buildURL(endpoint, projectID)
	if err != nil {
		return nil, err
	}

	req := &httpx.Request{
		Method:  method,
		URL:     fullURL,
		Headers: c.headers(token),
		Cookies: c.auth.Jar().HeaderForDomain(config.LabsBase),
	}
	if method != "GET" {
		req.Body = payload
	}
	// POSTs are not retried over QUIC: a second protocol on a non-idempotent
	// call risks submitting a generation twice.
	req.DisableQUIC = method != "GET"

	resp, err := c.hc.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("flow: %s %s: %w", method, endpoint, err)
	}

	result := &Result{
		Status:  resp.Status,
		Body:    resp.Body,
		Elapsed: time.Since(start),
	}
	if len(resp.Body) > 0 {
		_ = json.Unmarshal(resp.Body, &result.Data)
	}

	if resp.Status < 200 || resp.Status >= 300 {
		return result, classify(resp.Status, resp.Body)
	}
	return result, nil
}

// injectContext rewrites the request body so every clientContext block carries
// the resolved project, tier, and captcha token.
//
// The body is round-tripped through JSON rather than patched field by field:
// generation payloads nest clientContext in several places and new nestings
// appear without notice, so a generic walk is more durable than a struct edit.
func (c *Client) injectContext(body any, projectID, tier, captchaToken string) ([]byte, error) {
	if body == nil {
		return nil, nil
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("flow: encode request body: %w", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Not an object (an array, or a pre-encoded payload): send as-is.
		return raw, nil
	}

	ctx := buildClientContext(projectID, tier, captchaToken)

	if _, ok := doc["clientContext"]; ok {
		doc["clientContext"] = ctx
	}
	if reqs, ok := doc["requests"].([]any); ok {
		for _, item := range reqs {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if _, hasCtx := m["clientContext"]; hasCtx {
				m["clientContext"] = ctx
			}
		}
	}

	return json.Marshal(doc)
}

// resolveTier picks the paygate tier to declare in clientContext.
//
// This deliberately does NOT downgrade a free-tier account to
// PAYGATE_TIER_NOT_PAID. The Python engine did that, and the working reference
// implementation omits the field entirely; declaring "not paid" appears to route
// the request onto a stricter path and is a plausible contributor to the
// PUBLIC_ERROR_UNUSUAL_ACTIVITY rejections. Override with FLOW_PAYGATE_TIER.
func (c *Client) resolveTier(session *auth.Session) string {
	if c.opts.Tier != "" {
		return c.opts.Tier
	}
	if override := strings.TrimSpace(os.Getenv("FLOW_PAYGATE_TIER")); override != "" {
		return override
	}

	c.mu.Lock()
	if session != nil && session.Sku != "" {
		c.lastSku = session.Sku
	}
	c.mu.Unlock()

	return config.ClientCtx.Tier
}

func (c *Client) buildURL(endpoint, projectID string) (string, error) {
	path := endpoint
	if strings.Contains(path, "{project_id}") {
		path = strings.ReplaceAll(path, "{project_id}", projectID)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	u, err := url.Parse(config.APIBase + path)
	if err != nil {
		return "", fmt.Errorf("flow: bad endpoint %q: %w", endpoint, err)
	}
	q := u.Query()
	q.Set("key", config.APIKey())
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// headers builds the request headers, preferring the captured browser identity.
//
// Generation requests are fingerprinted. Sending a plausible-looking but
// different UA than the one the reCAPTCHA token was minted under is the single
// most common cause of a spurious UNUSUAL_ACTIVITY rejection.
func (c *Client) headers(token string) map[string]string {
	ua := config.UserAgents[0]
	if len(config.UserAgents) > 1 && time.Now().UnixNano()%2 == 0 {
		ua = config.UserAgents[1]
	}
	secChUa := httpx.ChromeSecChUA
	platform := httpx.ChromePlatform
	mobile := "?0"
	acceptLanguage := "en-US,en;q=0.9"

	if fp := c.opts.Fingerprint; fp != nil {
		if fp.UserAgent != "" {
			ua = fp.UserAgent
		}
		if fp.SecChUa != "" {
			secChUa = fp.SecChUa
		}
		if fp.Platform != "" {
			platform = fp.Platform
		}
		if fp.Mobile != "" {
			mobile = fp.Mobile
		}
		if fp.Language != "" {
			acceptLanguage = fp.Language
		}
	}

	headers := map[string]string{
		"Authorization":      "Bearer " + token,
		"Content-Type":       "text/plain;charset=UTF-8",
		"Accept":             "*/*",
		"Accept-Language":    acceptLanguage,
		"Origin":             config.ClientCtx.Origin,
		"Referer":            config.ClientCtx.Origin + "/",
		"User-Agent":         ua,
		"sec-ch-ua":          secChUa,
		"sec-ch-ua-mobile":   mobile,
		"sec-ch-ua-platform": platform,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "cross-site",
	}

	// The API key is HTTP-referrer restricted. Sending `https://labs.google/`
	// satisfies that restriction, but the app itself now runs on
	// flow.google.com — and a request whose claimed origin does not match the
	// origin the reCAPTCHA assessment was made for is a strong candidate for the
	// PUBLIC_ERROR_UNUSUAL_ACTIVITY rejection. FLOW_REFERER lets this be tested
	// without a rebuild: a URL to send, or "none" to omit both headers the way a
	// fetch with referrerPolicy "no-referrer" would.
	switch ref := strings.TrimSpace(os.Getenv("FLOW_REFERER")); ref {
	case "":
		// keep the defaults above
	case "none":
		delete(headers, "Referer")
		delete(headers, "Origin")
	default:
		headers["Referer"] = ref
		headers["Origin"] = strings.TrimSuffix(ref, "/")
	}

	return headers
}

func (c *Client) recordSuccess() {
	c.mu.Lock()
	c.requests++
	c.mu.Unlock()
}

func (c *Client) recordFailure() {
	c.mu.Lock()
	c.requests++
	c.failures++
	c.mu.Unlock()
}

/* ------------------------------------------------------------------ *
 * Error classification
 * ------------------------------------------------------------------ */

// classify turns a non-2xx response into a typed APIError.
func classify(status int, body []byte) error {
	apiErr := &APIError{Status: status, Message: strings.TrimSpace(string(body))}

	var wrapper struct {
		Error *ErrorBody `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Error != nil {
		if wrapper.Error.Message != "" {
			apiErr.Message = wrapper.Error.Message
		}
		apiErr.Reason = wrapper.Error.Reason()
		if wrapper.Error.Status != "" {
			apiErr.Code = wrapper.Error.Status
		} else if wrapper.Error.Code != 0 {
			apiErr.Code = fmt.Sprintf("%d", wrapper.Error.Code)
		}
	}
	if apiErr.Message == "" {
		apiErr.Message = fmt.Sprintf("HTTP %d", status)
	}
	return apiErr
}

// IsUnauthenticated reports whether err is an auth failure that a fresh token
// could fix. Exposed so callers (and the pool) can make failover decisions
// without importing the concrete error type.
func IsUnauthenticated(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Unauthenticated()
	}
	return false
}

// IsRetryable reports whether err is worth retrying, possibly on another account.
func IsRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	// Transport failures are retryable by nature.
	return err != nil
}

// IsOutOfCredits reports whether err is the account running dry.
func IsOutOfCredits(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.OutOfCredits()
	}
	return false
}

/* ------------------------------------------------------------------ *
 * Rate limiting
 * ------------------------------------------------------------------ */

// limiter enforces both a concurrency cap and a minimum spacing between request
// starts for one account. Google throttles accounts that fire bursts at Flow,
// and the throttle shows up as UNUSUAL_ACTIVITY rather than a clean 429.
type limiter struct {
	sem      chan struct{}
	interval time.Duration

	mu     sync.Mutex
	lastAt time.Time
}

func newLimiter(concurrency int, interval time.Duration) *limiter {
	if concurrency < 1 {
		concurrency = 1
	}
	return &limiter{
		sem:      make(chan struct{}, concurrency),
		interval: interval,
	}
}

func (l *limiter) acquire(ctx context.Context) error {
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	if l.interval <= 0 {
		return nil
	}

	l.mu.Lock()
	now := time.Now()
	wait := l.lastAt.Add(l.interval).Sub(now)
	if wait < 0 {
		wait = 0
	}
	l.lastAt = now.Add(wait)
	l.mu.Unlock()

	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			<-l.sem
			return ctx.Err()
		}
	}
	return nil
}

func (l *limiter) release() {
	select {
	case <-l.sem:
	default:
	}
}
