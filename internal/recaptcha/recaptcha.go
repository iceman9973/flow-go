// Package recaptcha supplies reCAPTCHA Enterprise tokens for Flow generation calls.
//
// Flow's generation endpoints carry a `recaptchaContext` block inside
// `clientContext`, and the upstream rejects a request whose token is missing or
// stale with `403 PUBLIC_ERROR_UNUSUAL_ACTIVITY`. So unlike the Python engine
// this port replaces — which always sent an empty token — a real token is
// needed.
//
// Three strategies are provided, in increasing order of effort:
//
//	BrokerProvider — asks the attached extension to run the site's own
//	                 grecaptcha.enterprise.execute() in a signed-in tab. Highest
//	                 score, since it runs in a real page.
//	HTTPProvider   — speaks the reCAPTCHA Enterprise anchor/reload protocol over
//	                 plain HTTP. No browser, no captcha-solving service.
//	EmptyProvider  — sends no token. Kept as a terminating fallback and to
//	                 reproduce the old behaviour; the upstream rejects it.
//
// Chain tries each in order and returns the first usable token. A chain of
// (Broker, HTTP, Empty) is the recommended configuration: the browser only
// participates in the one step where it genuinely adds value.
package recaptcha

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kodelyx/cdp-control/cdp"
	"github.com/kodelyx/flow-go/internal/httpx"
)

// Actions Flow uses to scope a token to an operation. The value is sent as the
// widget's `sa` parameter.
const (
	ActionVideo = "VIDEO_GENERATION"
	ActionImage = "IMAGE_GENERATION"
)

// DefaultSiteKey is the reCAPTCHA Enterprise site key used by the Labs FX apps.
// Overridable because Google rotates these and a stale key must not require a
// rebuild.
const DefaultSiteKey = "6LdsFiUsAAAAAIjVDZcuLhaHiDn5nnHVXVRQGeMV"

// Enterprise endpoints and the origin the token is scoped to.
const (
	// recaptchaBase is a var rather than a const so a test can point it at a
	// local server. Without that, exercising the anchor/reload exchange means
	// talking to Google, which is neither hermetic nor free.
	// recaptchaOrigin is the site the widget is embedded in. The token is scoped
	// to it, so a wrong value is a mismatch the assessment can see.
	//
	// This said "https://labs.google" and the app has since moved to
	// flow.google.com — the same move that retired the legacy REST surface. The
	// stale value survived because nothing here compared it against where the
	// requests actually go.
	recaptchaOrigin = "https://flow.google.com"
	// recaptchaUA is pinned to a real Chrome build. The widget scores the request
	// fingerprint, so a generic UA measurably lowers the score.
	recaptchaUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

var recaptchaBase = "https://www.google.com/recaptcha/enterprise"

// recaptchaCO is the base64-encoded origin the widget expects, derived from
// recaptchaOrigin so the two cannot drift apart again.
//
// The trailing dot is part of the observed value rather than a typo, and is kept
// because it was captured that way.
var recaptchaCO = base64.RawURLEncoding.EncodeToString([]byte(recaptchaOrigin+":443")) + "."

// minTokenLength is what a real Enterprise token looks like. Anything shorter is
// a placeholder or a failure page, and submitting one is worse than submitting
// nothing because it spends an attempt on a guaranteed rejection.
const minTokenLength = 500

var (
	// releaseVersionRe pulls the release id out of the widget bundle. The widget
	// rejects a stale version, so it cannot be hardcoded.
	releaseVersionRe = regexp.MustCompile(`/recaptcha/releases/([^/]+)/recaptcha__`)

	// anchorTokenPatterns cover the shapes the anchor page has used. The
	// JSON-ish form is the one in use now; the attribute forms are kept because
	// the page has flipped between them before.
	anchorTokenPatterns = []*regexp.Regexp{
		regexp.MustCompile(`id="recaptcha-token"\s+value="([^"]+)"`),
		regexp.MustCompile(`value="([^"]+)"\s+id="recaptcha-token"`),
		regexp.MustCompile(`"recaptcha-token"\s*,\s*"([^"]+)"`),
	}

	// reloadTokenRe is the fallback when the reload payload will not parse as
	// JSON.
	reloadTokenRe = regexp.MustCompile(`\["rresp","([^"]+)"`)

	// antiJSONPrefix is the XSSI guard Google prefixes JSON responses with.
	antiJSONPrefix = regexp.MustCompile(`^\)\]\}'\s*`)
)

// Provider yields a token for a given action. An empty string with a nil error
// means "send no token".
type Provider interface {
	Token(ctx context.Context, action string) (string, error)
	Name() string
}

/* ------------------------------------------------------------------ *
 * EmptyProvider
 * ------------------------------------------------------------------ */

// EmptyProvider always returns no token.
//
// It exists to reproduce the Python engine's behaviour and to keep the chain
// terminating. In practice Flow rejects a tokenless generation, so a chain that
// falls through to this has failed, not succeeded.
type EmptyProvider struct{}

// Token returns an empty token.
func (EmptyProvider) Token(context.Context, string) (string, error) { return "", nil }

// Name identifies the provider in logs.
func (EmptyProvider) Name() string { return "empty" }

/* ------------------------------------------------------------------ *
 * HTTPProvider
 * ------------------------------------------------------------------ */

// HTTPProvider speaks the reCAPTCHA Enterprise anchor/reload protocol directly.
type HTTPProvider struct {
	hc        *httpx.Client
	siteKey   string
	origin    string
	userAgent string
}

// NewHTTP builds an HTTP provider.
func NewHTTP(hc *httpx.Client, siteKey, origin string) *HTTPProvider {
	if siteKey == "" {
		siteKey = os.Getenv("FLOW_RECAPTCHA_SITE_KEY")
	}
	if siteKey == "" {
		siteKey = DefaultSiteKey
	}
	if origin == "" {
		origin = recaptchaOrigin
	}
	return &HTTPProvider{
		hc:      hc,
		siteKey: siteKey,
		origin:  origin,
		// The pinned default, replaced by WithUserAgent when the caller knows the
		// real client — which a process with a browser, or one that took a
		// fingerprint from a process that had one, always does.
		userAgent: recaptchaUA,
	}
}

// Name identifies the provider in logs.
func (p *HTTPProvider) Name() string { return "http" }

// WithUserAgent sets the client this provider claims to be.
//
// The widget scores the client that asks for a token, and the token is then
// checked against the client that spends it — so a provider that declares one
// machine and hands the token to a process presenting another is a mismatch the
// assessment can see. An empty value leaves the pinned default in place, which is
// what a caller with no browser to read from has to accept.
func (p *HTTPProvider) WithUserAgent(userAgent string) *HTTPProvider {
	if strings.TrimSpace(userAgent) != "" {
		p.userAgent = userAgent
	}
	return p
}

// Token returns a fresh token.
//
// It used to cache one for two minutes, on the reasoning that Enterprise tokens
// are short-lived and a batch should not pay for a round trip per item. That
// reasoning is wrong in the way that matters: **a reCAPTCHA token is
// single-use**. It is verified once, and any later call that presents the same
// one is rejected — silently, with an empty frame rather than an error, which is
// why this survived so long.
//
// The cache made every generation after the first inside a two-minute window
// fail. It was only ever survivable because a CLI run is a fresh process, so the
// command line accidentally minted a new token every time; a server, which is
// where this was meant to be used, did not.
func (p *HTTPProvider) Token(ctx context.Context, action string) (string, error) {
	return p.fetch(ctx, action)
}

func (p *HTTPProvider) fetch(ctx context.Context, action string) (string, error) {
	// Step 1: the widget's release version, read from the bundle it is served
	// from. A hardcoded version is rejected, so this round trip is mandatory.
	version, err := p.releaseVersion(ctx)
	if err != nil {
		return "", err
	}

	// Step 2: anchor. Returns an HTML page carrying a one-time challenge token.
	anchorURL := recaptchaBase + "/anchor?" + url.Values{
		"ar":         {"1"},
		"k":          {p.siteKey},
		"co":         {recaptchaCO},
		"hl":         {"en"},
		"v":          {version},
		"size":       {"invisible"},
		"anchor-ms":  {"20000"},
		"execute-ms": {"30000"},
		"cb":         {randomCallback()},
	}.Encode()

	anchorResp, err := p.hc.Do(ctx, &httpx.Request{
		Method: "GET",
		URL:    anchorURL,
		Headers: map[string]string{
			"User-Agent":      p.userAgent,
			"Accept-Language": "en-US,en;q=0.9",
			"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			"Referer":         p.origin + "/",
			"Sec-Fetch-Dest":  "iframe",
			"Sec-Fetch-Mode":  "navigate",
			"Sec-Fetch-Site":  "cross-site",
		},
		DisableQUIC: true,
	})
	if err != nil {
		return "", fmt.Errorf("recaptcha: anchor request failed: %w", err)
	}
	if anchorResp.Status != 200 {
		return "", fmt.Errorf("recaptcha: anchor returned %d", anchorResp.Status)
	}

	challenge, err := parseAnchorToken(anchorResp.Text())
	if err != nil {
		return "", err
	}

	// Step 3: reload. Exchanges the challenge for a real assessment token. The
	// action goes in `sa`, not `action`.
	reloadURL := recaptchaBase + "/reload?" + url.Values{"k": {p.siteKey}}.Encode()

	form := url.Values{
		"v":      {version},
		"reason": {"q"},
		"c":      {challenge},
		"k":      {p.siteKey},
		"co":     {recaptchaCO},
		"hl":     {"en"},
		"size":   {"invisible"},
		"sa":     {action},
	}

	reloadResp, err := p.hc.Do(ctx, &httpx.Request{
		Method: "POST",
		URL:    reloadURL,
		Body:   []byte(form.Encode()),
		Headers: map[string]string{
			"User-Agent":      p.userAgent,
			"Accept-Language": "en-US,en;q=0.9",
			"Content-Type":    "application/x-www-form-urlencoded",
			"Accept":          "*/*",
			"Origin":          "https://www.google.com",
			"Referer":         anchorURL,
			"Sec-Fetch-Dest":  "empty",
			"Sec-Fetch-Mode":  "cors",
			"Sec-Fetch-Site":  "same-origin",
		},
		DisableQUIC: true,
	})
	if err != nil {
		return "", fmt.Errorf("recaptcha: reload request failed: %w", err)
	}
	if reloadResp.Status != 200 {
		return "", fmt.Errorf("recaptcha: reload returned %d", reloadResp.Status)
	}

	token, err := parseReloadToken(reloadResp.Text())
	if err != nil {
		return "", err
	}
	if len(token) < minTokenLength {
		return "", fmt.Errorf(
			"recaptcha: the token is only %d chars, which is a placeholder rather than a real "+
				"assessment; the widget is rejecting this client", len(token))
	}
	return token, nil
}

// releaseVersion fetches the widget bundle and reads its release id.
func (p *HTTPProvider) releaseVersion(ctx context.Context) (string, error) {
	resp, err := p.hc.Do(ctx, &httpx.Request{
		Method: "GET",
		URL:    recaptchaBase + ".js?render=" + p.siteKey,
		Headers: map[string]string{
			"User-Agent":      p.userAgent,
			"Accept-Language": "en-US,en;q=0.9",
			"Accept":          "*/*",
			"Referer":         p.origin + "/",
		},
		DisableQUIC: true,
	})
	if err != nil {
		return "", fmt.Errorf("recaptcha: could not fetch the widget bundle: %w", err)
	}
	if resp.Status != 200 {
		return "", fmt.Errorf("recaptcha: the widget bundle returned %d", resp.Status)
	}

	match := releaseVersionRe.FindStringSubmatch(resp.Text())
	if len(match) < 2 {
		return "", fmt.Errorf("recaptcha: could not read the release version from the widget bundle")
	}
	return match[1], nil
}

/* ------------------------------------------------------------------ *
 * BrokerProvider
 * ------------------------------------------------------------------ */

// PageURLResolver returns the URL of a page that has the site's reCAPTCHA client
// loaded, or "" when none is known.
//
// It is a function rather than a string because the caller learns the project id
// after the provider is built, and the editor URL depends on it.
type PageURLResolver func() string

// BrokerProvider asks the attached extension to run the page's own
// grecaptcha.enterprise.execute(). This is the highest-scoring strategy, because
// it runs in a real signed-in page with the account's own fingerprint.
type BrokerProvider struct {
	client  *cdp.Client
	siteKey string
	timeout time.Duration
	// pageURL supplies a page that loads the reCAPTCHA client. Optional: without
	// it the broker uses whatever tab it is given.
	pageURL PageURLResolver
}

// NewBroker builds a broker provider driven by a browser-Cdp client.
func NewBroker(client *cdp.Client, siteKey string, pageURL PageURLResolver) *BrokerProvider {
	if siteKey == "" {
		siteKey = os.Getenv("FLOW_RECAPTCHA_SITE_KEY")
	}
	if siteKey == "" {
		siteKey = DefaultSiteKey
	}
	return &BrokerProvider{client: client, siteKey: siteKey, timeout: 30 * time.Second, pageURL: pageURL}
}

// Name identifies the provider in logs.
func (p *BrokerProvider) Name() string { return "broker" }

// recaptchaProbe reports whether the page has the enterprise client loaded.
//
// Evaluated in the page rather than inferred from the URL, because the same
// origin serves pages both with and without it.
const recaptchaProbe = `(() => !!(window.grecaptcha && window.grecaptcha.enterprise && window.grecaptcha.enterprise.execute))()`

// hasRecaptcha reports whether the attached tab can currently mint a token.
func (p *BrokerProvider) hasRecaptcha(ctx context.Context) bool {
	// An empty action makes this a probe: the extension answers whether the page
	// has the client, without minting anything.
	out, err := p.client.FlowCaptcha(ctx, "", recaptchaProbe)
	if err != nil {
		return false
	}
	return out.Available
}

// gotoRecaptchaPage navigates the attached tab to a page that loads the client
// and waits for it to appear.
//
// A no-op when no page is configured, so a caller that has not supplied one
// keeps the previous behaviour rather than failing.
func (p *BrokerProvider) gotoRecaptchaPage(ctx context.Context) error {
	if p.pageURL == nil {
		return nil
	}
	target := strings.TrimSpace(p.pageURL())
	if target == "" {
		return nil
	}

	nav := fmt.Sprintf("(() => { location.href = %s; return 'nav'; })()", strconv.Quote(target))
	if err := p.client.FlowNavigate(ctx, target, nav); err != nil {
		return fmt.Errorf("recaptcha: could not open %s: %w", target, err)
	}

	// Poll rather than sleep a fixed amount: the page is ready when it says so,
	// and the load time varies with whatever else the tab is doing.
	deadline := time.Now().Add(p.timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		if p.hasRecaptcha(ctx) {
			return nil
		}
	}
	return fmt.Errorf("recaptcha: %s never loaded the reCAPTCHA client", target)
}

// Token evaluates grecaptcha.enterprise.execute in the attached tab.
func (p *BrokerProvider) Token(ctx context.Context, action string) (string, error) {
	if p.client == nil || !p.client.Connected() {
		return "", fmt.Errorf("recaptcha: the broker has no browser-Cdp connection")
	}

	// The client has to be present in the page, not merely on the right origin:
	// flow.google.com serves both pages that load it — the project editor — and
	// pages that do not, such as the account and landing pages.
	//
	// Minting from a page without it fails, and it cannot be injected to fix
	// that: the site's CSP requires a TrustedScriptURL, so a <script src> is
	// refused outright. That was the bug here — the broker attached to whichever
	// target was first, that target redirected to a page without the client, and
	// every mint failed into the low-score fallback.
	//
	// So probe for it, attach if nothing is attached yet, and navigate to a page
	// that has it when the current one does not.
	if !p.hasRecaptcha(ctx) {
		if _, attachErr := p.client.Attach(ctx, 0); attachErr != nil {
			return "", fmt.Errorf("recaptcha: the broker could not attach to a tab: %w", attachErr)
		}
	}
	if !p.hasRecaptcha(ctx) {
		if err := p.gotoRecaptchaPage(ctx); err != nil {
			return "", err
		}
	}

	// The script waits for grecaptcha.enterprise.ready() before executing, and
	// injects the enterprise bundle when the page has not loaded it yet.
	//
	// Skipping the ready() wait is the subtle failure mode: execute() still
	// returns a token, but one produced before the client finished initialising,
	// and the assessment behind it scores low enough that Flow rejects the
	// generation as PUBLIC_ERROR_UNUSUAL_ACTIVITY. A token of the right length is
	// therefore not evidence that this path worked.
	expression := fmt.Sprintf(`(async () => {
  const siteKey = %q;
  const action = %q;
  try {
    if (!window.grecaptcha || !window.grecaptcha.enterprise) {
      await new Promise((resolve, reject) => {
        const s = document.createElement('script');
        s.src = 'https://www.google.com/recaptcha/enterprise.js?render=' + encodeURIComponent(siteKey);
        s.onload = resolve;
        s.onerror = () => reject(new Error('could not load the reCAPTCHA enterprise bundle'));
        document.head.appendChild(s);
      });
    }
    await new Promise(resolve => window.grecaptcha.enterprise.ready(resolve));
    const token = await window.grecaptcha.enterprise.execute(siteKey, {action});
    return {token};
  } catch (e) {
    return {error: String(e)};
  }
})()`, p.siteKey, action)

	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	out, err := p.client.FlowCaptcha(callCtx, action, expression)
	if err != nil {
		return "", fmt.Errorf("recaptcha: the broker mint failed: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("recaptcha: %s", out.Error)
	}
	if len(out.Token) < minTokenLength {
		return "", fmt.Errorf("recaptcha: the broker returned a %d-char token, too short to be real", len(out.Token))
	}
	return out.Token, nil
}

/* ------------------------------------------------------------------ *
 * Chain
 * ------------------------------------------------------------------ */

// Chain tries providers in order and returns the first usable token.
type Chain struct {
	providers []Provider
}

// NewChain builds a chain from the given providers.
func NewChain(providers ...Provider) *Chain {
	return &Chain{providers: providers}
}

// Token walks the chain.
func (c *Chain) Token(ctx context.Context, action string) (string, error) {
	var lastErr error
	for _, p := range c.providers {
		token, err := p.Token(ctx, action)
		if err == nil && token != "" {
			log.Printf("recaptcha: token acquired via %s (%d chars)", p.Name(), len(token))
			return token, nil
		}
		if err != nil {
			lastErr = err
			log.Printf("recaptcha: provider %s failed: %v", p.Name(), err)
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", nil
}

// Name identifies the chain in logs.
func (c *Chain) Name() string {
	names := make([]string, 0, len(c.providers))
	for _, p := range c.providers {
		names = append(names, p.Name())
	}
	return "chain(" + strings.Join(names, ",") + ")"
}

/* ------------------------------------------------------------------ *
 * FlowProvider
 * ------------------------------------------------------------------ */

// FlowProvider mints a token through the Flow extension's own `flow.captcha`
// operation.
//
// The broker reaches the page with `cdp.evaluate`, which is precisely what a Flow
// extension does not expose — it offers named operations instead. So a profile
// running only the Flow bridge had no working broker: every mint fell through to
// the HTTP fallback, and the extension's own captcha operation was never called by
// anything in the backend at all.
//
// The client is resolved per call rather than captured. The broker holds whichever
// client was current at start-up, which goes stale as soon as that extension
// reconnects or another takes over — and a stale client reads as "no connection"
// while a perfectly good one is attached.
type FlowProvider struct {
	current func() *cdp.Client
}

// NewFlow builds a provider over the Flow bridge's own captcha operation.
func NewFlow(current func() *cdp.Client) *FlowProvider {
	return &FlowProvider{current: current}
}

func (p *FlowProvider) Name() string { return "flow.captcha" }

func (p *FlowProvider) Token(ctx context.Context, action string) (string, error) {
	if p.current == nil {
		return "", fmt.Errorf("recaptcha: no bridge to resolve a client from")
	}
	client := p.current()
	if client == nil || !client.Connected() {
		return "", fmt.Errorf("recaptcha: no extension attached")
	}

	result, err := client.FlowCaptcha(ctx, action, "")
	if err != nil {
		return "", err
	}
	if result.Error != "" {
		return "", fmt.Errorf("recaptcha: the page refused to mint: %s", result.Error)
	}
	if result.Token == "" {
		return "", fmt.Errorf("recaptcha: the page minted an empty token")
	}
	return result.Token, nil
}

// Build assembles the recommended provider for the current environment.
//
// With a Flow extension attached the chain is flow.captcha -> broker -> http ->
// empty. Without one it falls back to broker -> http -> empty, and without a
// browser at all to http -> empty. Set FLOW_RECAPTCHA=off to force the empty
// provider.
//
// pageURL tells the broker where to find a page that loads the reCAPTCHA client.
// It may be nil, in which case the broker uses whatever tab it is handed.
//
// current resolves the attached extension at call time. It may be nil, and a
// provider built from it fails cleanly rather than minting nothing.
func Build(mode string, hc *httpx.Client, broker *cdp.Client, pageURL PageURLResolver, current func() *cdp.Client, userAgent string) Provider {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off", "none", "empty":
		return EmptyProvider{}
	case "http":
		return NewChain(NewHTTP(hc, "", "").WithUserAgent(userAgent), EmptyProvider{})
	case "broker":
		// The explicit opt-in to the browser. Kept because it is the only path
		// that scores from a real page, and because a caller who asks for it
		// should get it rather than a silent fallback to the transport.
		providers := make([]Provider, 0, 4)
		if current != nil {
			providers = append(providers, NewFlow(current))
		}
		if broker != nil {
			providers = append(providers, NewBroker(broker, "", pageURL))
		}
		providers = append(providers, NewHTTP(hc, "", "").WithUserAgent(userAgent), EmptyProvider{})
		return NewChain(providers...)
	default: // "auto"
		// The transport, and only the transport.
		//
		// This used to try `flow.captcha` and the broker first, on the belief
		// that a page-minted token scored better. It does, and it was also the
		// reason a browser had to be attached and a tab had to be sitting on a
		// Flow project for every generation. The HTTP provider turns out to be
		// sufficient — the real defect was that it reused a single-use token —
		// so the browser is no longer asked for anything on this path.
		//
		// Anyone who wants the page token can still have it: `--captcha broker`.
		return NewChain(NewHTTP(hc, "", "").WithUserAgent(userAgent), EmptyProvider{})
	}
}

/* ------------------------------------------------------------------ *
 * Parsing helpers
 * ------------------------------------------------------------------ */

// parseAnchorToken extracts the challenge token from the anchor page.
func parseAnchorToken(html string) (string, error) {
	for _, pattern := range anchorTokenPatterns {
		if match := pattern.FindStringSubmatch(html); len(match) > 1 {
			return htmlUnescape(match[1]), nil
		}
	}
	return "", fmt.Errorf("recaptcha: the anchor response carried no challenge token")
}

// parseReloadToken extracts the assessment token from the reload payload.
func parseReloadToken(body string) (string, error) {
	raw := strings.TrimSpace(antiJSONPrefix.ReplaceAllString(strings.TrimSpace(body), ""))

	var arr []any
	if err := json.Unmarshal([]byte(raw), &arr); err == nil {
		// Preferred shape: the array contains the literal "rresp" followed by
		// the token.
		for i, value := range arr {
			if value != "rresp" || i+1 >= len(arr) {
				continue
			}
			if token, ok := arr[i+1].(string); ok && token != "" {
				return token, nil
			}
		}
		if len(arr) > 1 {
			if token, ok := arr[1].(string); ok && token != "" {
				return token, nil
			}
		}
	}

	if match := reloadTokenRe.FindStringSubmatch(raw); len(match) > 1 {
		return match[1], nil
	}
	return "", fmt.Errorf("recaptcha: the reload response carried no assessment token")
}

// htmlUnescape resolves the entities that appear in attribute values.
func htmlUnescape(value string) string {
	return strings.NewReplacer(
		"&amp;", "&",
		"&#39;", "'",
		"&quot;", `"`,
		"&lt;", "<",
		"&gt;", ">",
	).Replace(value)
}

// randomCallback produces the `cb` cache-buster the widget expects.
func randomCallback() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 12)
	for i := range out {
		out[i] = alphabet[rand.Intn(len(alphabet))]
	}
	return string(out)
}
