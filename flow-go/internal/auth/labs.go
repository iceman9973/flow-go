// Package auth turns browser cookies into a usable Flow access token.
//
// This is the piece that removes the browser from the critical path. In the
// Python engine the extension had to observe a live `Authorization: Bearer ya29.`
// request on labs.google and hand the header to the backend, which meant every
// token rotation needed a browser tab. Here the token is minted by calling the
// Labs session endpoint with the cookies, so a browser is only needed when the
// cookies themselves go stale.
//
// Caching rules (these exist because the Python version got them wrong):
//
//   - A cached token is keyed on the cookie jar's hash. New cookies invalidate
//     the token immediately, so a token can never outlive the credentials it was
//     minted from.
//   - A cached token also carries a hard TTL well below Google's ~60 minute
//     bearer lifetime, so a token is never trusted just because nothing has
//     complained yet.
//   - A cached token is dropped the moment any caller reports it unauthenticated,
//     via Invalidate.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/httpx"
)

// Session is a minted Labs session.
type Session struct {
	AccessToken string
	TokenType   string
	ExpiresAt   time.Time
	Email       string
	UserID      string
	Sku         string
	Scopes      []string
	Projects    []Project
	MintedAt    time.Time
	// CookieHash is the jar fingerprint this session was minted from.
	CookieHash string
	// UpstreamError is the `error` field Labs returned alongside the token.
	// In practice this is usually ACCESS_TOKEN_REFRESH_NEEDED, which means the
	// cookies are getting old and a browser re-sync will be needed soon — but a
	// working token is still issued. Surfaced rather than swallowed so the
	// condition is visible before it turns into a failure.
	UpstreamError string
}

// Project is a Flow project as advertised by Labs.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Usable reports whether the session is still within its local TTL.
//
// The local TTL is authoritative. Labs advertises an `expires` value that is
// routinely already in the past — it tracks the NextAuth session rather than the
// bearer token — while still issuing a usable access token. Treating that as an
// authoritative expiry would re-mint on every request. So an advertised expiry
// is only honoured when it is genuinely in the future relative to when the token
// was minted.
func (s *Session) Usable(now time.Time, ttl time.Duration) bool {
	if s == nil || s.AccessToken == "" {
		return false
	}
	if ttl > 0 && now.Sub(s.MintedAt) > ttl {
		return false
	}
	if !s.ExpiresAt.IsZero() && s.ExpiresAt.After(s.MintedAt) &&
		now.After(s.ExpiresAt.Add(-60*time.Second)) {
		return false
	}
	return true
}

// Provider mints and caches access tokens for one cookie jar.
type Provider struct {
	jar *cookiejar.Jar
	hc  *httpx.Client

	mu         sync.Mutex
	cached     *Session
	lastErr    error
	lastErrAt  time.Time
	refreshCnt int64
	// accountIndex selects which signed-in Google account to act as.
	//
	// Google distinguishes its own accounts with the `authuser` query parameter,
	// and with none every request resolves to the first signed-in account. So a
	// browser switch alone changes nothing here: without the index the engine
	// keeps minting the original account's session while reading a project out of
	// the newly selected one.
	accountIndex int
}

// NewProvider builds a token provider for a cookie jar.
func NewProvider(jar *cookiejar.Jar, hc *httpx.Client) *Provider {
	return &Provider{jar: jar, hc: hc}
}

// AccountIndex returns the signed-in account this provider acts as.
func (p *Provider) AccountIndex() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accountIndex
}

// SetAccountIndex switches which signed-in account to act as, discarding the
// cached token so the next mint belongs to the new one.
func (p *Provider) SetAccountIndex(index int) {
	if index < 0 {
		index = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accountIndex = index
	p.cached = nil
}

// Jar returns the cookie jar backing this provider.
func (p *Provider) Jar() *cookiejar.Jar { return p.jar }

// SetJar swaps in a fresh jar, discarding any cached token.
func (p *Provider) SetJar(jar *cookiejar.Jar) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jar = jar
	p.cached = nil
}

// Invalidate drops the cached token. Called whenever an upstream call comes back
// unauthenticated, so the next request re-mints instead of replaying a dead token.
func (p *Provider) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cached = nil
}

// RefreshCount reports how many times a token has been minted. Useful for tests
// and for spotting a token that is being re-minted on every single call.
func (p *Provider) RefreshCount() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refreshCnt
}

// LastError returns the most recent mint failure, if any.
func (p *Provider) LastError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

// Session returns a usable session, minting one if the cache is cold or stale.
func (p *Provider) Session(ctx context.Context) (*Session, error) {
	p.mu.Lock()
	jar := p.jar
	ttl := time.Duration(config.SessionTTL) * time.Second
	now := time.Now()

	if p.cached != nil &&
		p.cached.CookieHash == jar.Hash() &&
		p.cached.Usable(now, ttl) {
		s := p.cached
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()

	return p.mint(ctx, jar)
}

// AccessToken is the convenience accessor used by the API client.
func (p *Provider) AccessToken(ctx context.Context) (string, error) {
	s, err := p.Session(ctx)
	if err != nil {
		return "", err
	}
	return s.AccessToken, nil
}

// ForceRefresh mints a new session regardless of cache state.
func (p *Provider) ForceRefresh(ctx context.Context) (*Session, error) {
	p.mu.Lock()
	jar := p.jar
	p.mu.Unlock()
	return p.mint(ctx, jar)
}

func (p *Provider) mint(ctx context.Context, jar *cookiejar.Jar) (*Session, error) {
	return p.mintWith(ctx, jar, false)
}

// sessionRefreshNeeded is the flag Labs returns alongside a token when its own
// session has gone stale. The token it hands back is Labs-scoped and the
// generation API rejects it, so this is not a warning to ignore.
const sessionRefreshNeeded = "ACCESS_TOKEN_REFRESH_NEEDED"

func (p *Provider) mintWith(ctx context.Context, jar *cookiejar.Jar, alreadyRebuilt bool) (*Session, error) {
	if jar == nil || jar.Count() == 0 {
		return nil, fmt.Errorf("auth: no cookies loaded")
	}
	if !jar.HasAuthCookies() {
		return nil, fmt.Errorf(
			"auth: cookie jar has no Labs credential cookie (looked for %s); "+
				"re-sync cookies from the browser",
			strings.Join(cookiejar.AuthCookieNames[:4], ", "))
	}

	// An explicitly supplied token short-circuits the exchange. This exists so a
	// deployment can be driven without cookies at all when that is preferable.
	if tok := strings.TrimSpace(os.Getenv("FLOW_ACCESS_TOKEN")); tok != "" {
		s := &Session{
			AccessToken: strings.TrimPrefix(tok, "Bearer "),
			TokenType:   "Bearer",
			MintedAt:    time.Now(),
			CookieHash:  jar.Hash(),
		}
		p.store(s)
		return s, nil
	}

	sessionURL := config.LabsBase + config.LabsSessionPath
	// The account index travels as a query parameter. Without it the endpoint
	// always answers for the first signed-in account, whatever the browser is
	// currently showing.
	if index := p.AccountIndex(); index > 0 {
		sessionURL += "?authuser=" + strconv.Itoa(index)
	}
	req := &httpx.Request{
		Method:  "GET",
		URL:     sessionURL,
		Cookies: jar.HeaderForDomain(sessionURL),
		Headers: map[string]string{
			"Accept":           "application/json, text/plain, */*",
			"Referer":          config.FlowUIBase,
			"Origin":           config.LabsBase,
			"Sec-Fetch-Dest":   "empty",
			"Sec-Fetch-Mode":   "cors",
			"Sec-Fetch-Site":   "same-origin",
			"X-Requested-With": "XMLHttpRequest",
		},
		DisableQUIC: true,
	}

	resp, err := p.hc.Do(ctx, req)
	if err != nil {
		return nil, p.recordErr(fmt.Errorf("auth: session request failed: %w", err))
	}
	if resp.Status != 200 {
		return nil, p.recordErr(fmt.Errorf(
			"auth: session endpoint returned %d (cookies are stale or the account is signed out)",
			resp.Status))
	}

	session, err := parseSession(resp.Body, jar.Hash())
	if err != nil {
		return nil, p.recordErr(err)
	}

	// A stale Labs session is flagged rather than failed. Labs still returns a
	// token, but a Labs-scoped one that the generation API rejects with 401 — so
	// this is the one condition worth acting on immediately instead of waiting
	// for the first 401 to come back. Rebuilding the session cookie from the
	// Google cookies is the fix, and it is pure HTTP: no browser is involved.
	if session.UpstreamError == sessionRefreshNeeded && !alreadyRebuilt && sessionRebuildEnabled() {
		log.Printf("auth: Labs reports the session needs refreshing — rebuilding it from the Google cookies")
		rebuilt, rebuildErr := p.rebuildSession(ctx, jar, session.Email)
		if rebuildErr != nil {
			log.Printf("auth: session rebuild failed (%v); continuing with the token Labs returned", rebuildErr)
		} else {
			return p.mintWith(ctx, rebuilt, true)
		}
	}

	p.store(session)
	log.Printf("auth: minted access token for %s (local TTL %ds, %d projects)",
		displayOr(session.Email, "account"),
		config.SessionTTL,
		len(session.Projects))
	if session.UpstreamError != "" {
		log.Printf("auth: Labs reported %q — the cookies still work, but a browser re-sync "+
			"will be needed once they stop", session.UpstreamError)
	}
	return session, nil
}

func (p *Provider) store(s *Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cached = s
	p.lastErr = nil
	p.refreshCnt++
}

// rebuildSession runs the NextAuth + Google OAuth handshake to mint a fresh
// session cookie, then swaps it into the provider so the retry uses it.
func (p *Provider) rebuildSession(ctx context.Context, jar *cookiejar.Jar, email string) (*cookiejar.Jar, error) {
	token, err := RefreshSessionToken(ctx, jar, p.hc, email)
	if err != nil {
		return nil, err
	}

	rebuilt := WithSessionToken(jar, token)
	if !rebuilt.HasAuthCookies() {
		return nil, fmt.Errorf("auth: the rebuilt jar carries no credential cookie")
	}

	p.mu.Lock()
	p.jar = rebuilt
	p.mu.Unlock()

	return rebuilt, nil
}

// sessionRebuildEnabled lets the rebuild be switched off with
// FLOW_SESSION_REBUILD=off, for a deployment that would rather see the raw
// upstream error than have the engine try to repair the session itself.
func sessionRebuildEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLOW_SESSION_REBUILD"))) {
	case "off", "false", "0", "no", "disabled":
		return false
	}
	return true
}

func (p *Provider) recordErr(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastErr = err
	p.lastErrAt = time.Now()
	return err
}

/* ------------------------------------------------------------------ *
 * Response parsing
 * ------------------------------------------------------------------ */

// parseSession extracts the access token from a Labs session payload.
//
// The payload shape is not formally documented and has drifted before, so the
// parser walks the document looking for known key names at any depth rather than
// binding to one struct. That keeps a rename from silently producing an empty
// token, which is the failure mode that is hardest to debug.
func parseSession(body []byte, cookieHash string) (*Session, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		trimmed := strings.TrimSpace(string(body))
		if trimmed == "{}" || trimmed == "" {
			return nil, fmt.Errorf("auth: Labs returned an empty session — cookies are not signed in")
		}
		return nil, fmt.Errorf("auth: session payload is not JSON: %w", err)
	}

	if len(doc) == 0 {
		return nil, fmt.Errorf("auth: Labs returned an empty session — cookies are not signed in")
	}

	token := firstString(doc, "access_token", "accessToken", "token")
	if token == "" {
		return nil, fmt.Errorf("auth: session payload contained no access token")
	}

	s := &Session{
		AccessToken: strings.TrimPrefix(token, "Bearer "),
		TokenType:   firstString(doc, "token_type", "tokenType"),
		MintedAt:    time.Now(),
		CookieHash:  cookieHash,
	}
	if s.TokenType == "" {
		s.TokenType = "Bearer"
	}
	s.UpstreamError = firstString(doc, "error", "errorMessage", "error_description")

	// Only trust an advertised expiry that is actually in the future. Labs
	// returns an `expires` that is frequently already past (it is the NextAuth
	// session's expiry, not the bearer token's) while still handing back a
	// working token. When it is not usable, leave ExpiresAt zero so the local
	// TTL is the only bound — which is the one bound we actually control.
	if exp := firstString(doc, "expires", "expiry", "expires_at", "expiresAt"); exp != "" {
		if t, err := parseTime(exp); err == nil && t.After(s.MintedAt) {
			s.ExpiresAt = t
		}
	}

	if scope := firstString(doc, "scope", "scopes"); scope != "" {
		s.Scopes = strings.Fields(scope)
	}

	if user, ok := firstMap(doc, "user", "account", "profile"); ok {
		s.Email = firstString(user, "email", "mail")
		s.UserID = firstString(user, "id", "sub", "userId", "user_id")
	}
	if s.Email == "" {
		s.Email = firstString(doc, "email")
	}
	if s.UserID == "" {
		s.UserID = firstString(doc, "sub", "user_id", "userId")
	}

	s.Sku = firstString(doc, "sku", "subscription", "tier", "userPaygateTier")

	for _, key := range []string{"projects", "flowProjects", "items"} {
		if list, ok := doc[key].([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					proj := Project{
						ID:   firstString(m, "id", "projectId", "name"),
						Name: firstString(m, "displayName", "title", "name"),
					}
					if proj.ID != "" {
						s.Projects = append(s.Projects, proj)
					}
				}
			}
		}
	}

	return s, nil
}

// firstString returns the first non-empty string found under any of keys,
// searching the top level first and then one level of nesting.
func firstString(doc map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := doc[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	for _, nested := range doc {
		m, ok := nested.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range keys {
			if v, ok := m[key]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func firstMap(doc map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if v, ok := doc[key]; ok {
			if m, ok := v.(map[string]any); ok {
				return m, true
			}
		}
	}
	return nil, false
}

func parseTime(value string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
		time.RFC1123,
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	// Numeric epoch seconds, sometimes returned as a string.
	if n, err := json.Number(value).Int64(); err == nil && n > 0 {
		return time.Unix(n, 0), nil
	}
	return time.Time{}, fmt.Errorf("auth: unrecognised timestamp %q", value)
}

func displayOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// ResolveProjectID returns the project to generate into, preferring an explicit
// value, then the first project advertised by Labs, then the configured default.
func (s *Session) ResolveProjectID(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if s != nil && len(s.Projects) > 0 && s.Projects[0].ID != "" {
		return s.Projects[0].ID
	}
	return config.DefaultProject
}

// ProjectIDFromFlowURL extracts a project ID from a Flow URL, which is the
// "base info" the browser can cheaply supply alongside cookies.
//
// Accepts both shapes the product has used:
//
//	https://labs.google/fx/tools/flow/project/<id>   (legacy)
//	https://flow.google.com/project/<id>             (current)
func ProjectIDFromFlowURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, part := range parts {
		if part == "project" && i+1 < len(parts) {
			return sanitizeID(parts[i+1])
		}
	}
	// Fallback: the last segment of a flow path, if it looks like an ID.
	if len(parts) > 0 && strings.Contains(u.Path, "/flow/") {
		return sanitizeID(parts[len(parts)-1])
	}
	return ""
}

func sanitizeID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return ""
		}
	}
	if len(value) < 6 || len(value) > 128 {
		return ""
	}
	return value
}
