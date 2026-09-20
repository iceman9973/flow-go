package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/httpx"
)

// SessionTokenCookie is the long-lived Labs session cookie. Exchanging it at the
// session endpoint is what produces an access token, and refreshing it is what
// recovers from ACCESS_TOKEN_REFRESH_NEEDED.
const SessionTokenCookie = "__Secure-next-auth.session-token"

// Labs NextAuth endpoints used to rebuild the session from Google cookies.
const (
	labsAuthBase    = "https://labs.google/fx"
	csrfPath        = "/api/auth/csrf"
	signinPath      = "/api/auth/signin/google"
	callbackMarker  = "labs.google/fx/api/auth/callback/google"
	googleAccounts  = "https://accounts.google.com/"
	googleOAuthHost = "accounts.google.com"
)

// maxOAuthHops bounds the Google redirect chain. The reference implementation
// allows 12; anything beyond that means the flow is not converging.
const maxOAuthHops = 12

// maxCallbackHops bounds the post-callback redirect chain that finally sets the
// session cookie.
const maxCallbackHops = 5

// SessionLoginError reports a failure to rebuild the Labs session.
type SessionLoginError struct {
	Message string
	// Transient marks a network-level failure that a retry might clear, as
	// opposed to the cookies being genuinely unusable.
	Transient bool
}

func (e *SessionLoginError) Error() string { return e.Message }

func loginError(format string, args ...any) *SessionLoginError {
	return &SessionLoginError{Message: fmt.Sprintf(format, args...)}
}

/* ------------------------------------------------------------------ *
 * Cookie header helpers
 * ------------------------------------------------------------------ */

// mergeSetCookies folds response Set-Cookie values into a name→value map.
func mergeSetCookies(dst map[string]string, header map[string][]string) {
	for _, raw := range setCookieValues(header) {
		pair := raw
		if i := strings.Index(raw, ";"); i >= 0 {
			pair = raw[:i]
		}
		name, value, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		dst[name] = strings.TrimSpace(value)
	}
}

// setCookieValues reads Set-Cookie from the response header map. The transport
// returns a plain map, so the multi-value case is read directly.
func setCookieValues(header map[string][]string) []string {
	for key, values := range header {
		if strings.EqualFold(key, "Set-Cookie") {
			return values
		}
	}
	return nil
}

// extractSessionCookie pulls the session token out of a Set-Cookie header.
func extractSessionCookie(header map[string][]string) string {
	prefix := SessionTokenCookie + "="
	for _, raw := range setCookieValues(header) {
		if !strings.HasPrefix(raw, prefix) {
			continue
		}
		rest := raw[len(prefix):]
		if i := strings.Index(rest, ";"); i >= 0 {
			rest = rest[:i]
		}
		return strings.TrimSpace(rest)
	}
	return ""
}

func cookieHeader(cookies map[string]string) string {
	parts := make([]string, 0, len(cookies))
	for name, value := range cookies {
		if value == "" {
			continue
		}
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

/* ------------------------------------------------------------------ *
 * HTML redirect extraction
 * ------------------------------------------------------------------ */

var htmlRedirectPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)content\s*=\s*["']?\d+\s*;\s*url\s*=\s*([^"'>\s]+)`),
	regexp.MustCompile(`(?i)location\.(?:href|replace)\s*\(\s*["']([^"']+)["']`),
	regexp.MustCompile(`(?i)location\s*=\s*["']([^"']+)["']`),
	regexp.MustCompile(`(?i)<form[^>]*action\s*=\s*["']([^"']+)["']`),
	regexp.MustCompile(`(https://labs\.google/fx/api/auth/callback/google[^"'<>\s]*)`),
}

var continueParamPattern = regexp.MustCompile(`[&?]continue=([^"'<>\s&]+)`)

// extractRedirectFromHTML finds the next URL in an interstitial page. Google's
// SSO returns 200 with a self-submitting form or a meta refresh rather than a
// 302, so the body has to be parsed.
func extractRedirectFromHTML(body string) string {
	for _, pattern := range htmlRedirectPatterns {
		if match := pattern.FindStringSubmatch(body); len(match) > 1 {
			return match[1]
		}
	}
	if match := continueParamPattern.FindStringSubmatch(body); len(match) > 1 {
		if decoded, err := url.QueryUnescape(match[1]); err == nil {
			return decoded
		}
		return match[1]
	}
	return ""
}

/* ------------------------------------------------------------------ *
 * The login flow
 * ------------------------------------------------------------------ */

// RefreshSessionToken rebuilds a Labs session cookie from Google identity
// cookies, using the NextAuth + Google OAuth redirect chain.
//
// This exists because Labs invalidates its session server-side. When that
// happens the session endpoint still returns a token, but a Labs-scoped one that
// the generation API rejects with 401, and it flags the response
// ACCESS_TOKEN_REFRESH_NEEDED. Only the site itself can renew the session — and
// this is that renewal, performed over plain HTTP with the Google cookies the
// browser already gave us. No browser is involved.
//
// The caller gets back a fresh session-token value to place in the jar.
func RefreshSessionToken(ctx context.Context, jar *cookiejar.Jar, hc *httpx.Client, email string) (string, error) {
	if jar == nil {
		return "", loginError("no cookies loaded")
	}

	googleCookies := googleCookieMap(jar)
	if !hasGoogleIdentityCookies(googleCookies) {
		return "", loginError(
			"Google identity cookies are missing; the refresh needs at least one of " +
				"SID, HSID, SSID, APISID, SAPISID. Re-sync cookies from a signed-in browser.")
	}

	labsCookies := labsCookieMap(jar)

	// Step 1: CSRF token, which also plants the NextAuth state cookies.
	csrfToken, err := fetchCSRFToken(ctx, hc, labsCookies)
	if err != nil {
		return "", err
	}

	// Step 2: ask NextAuth to start a Google sign-in. It answers with the OAuth
	// URL rather than a redirect.
	oauthURL, err := startGoogleSignin(ctx, hc, labsCookies, csrfToken)
	if err != nil {
		return "", err
	}
	if email != "" {
		oauthURL = withLoginHint(oauthURL, email)
	}

	// Step 3: walk Google's SSO chain with the Google cookies until it hands
	// back the Labs callback URL.
	callbackURL, err := followGoogleOAuth(ctx, hc, oauthURL, googleCookies)
	if err != nil {
		return "", err
	}

	// Step 4: hit the callback. Its Set-Cookie carries the new session token.
	token, err := completeCallback(ctx, hc, callbackURL, labsCookies)
	if err != nil {
		return "", err
	}

	log.Printf("auth: rebuilt the Labs session cookie from Google cookies")
	return token, nil
}

func googleCookieMap(jar *cookiejar.Jar) map[string]string {
	out := map[string]string{}
	for _, ck := range jar.ForDomain("https://accounts.google.com/") {
		out[ck.Name] = ck.Value
	}
	return out
}

func labsCookieMap(jar *cookiejar.Jar) map[string]string {
	out := map[string]string{}
	for _, ck := range jar.ForDomain(config.LabsBase + "/") {
		out[ck.Name] = ck.Value
	}
	return out
}

func hasGoogleIdentityCookies(cookies map[string]string) bool {
	for _, name := range []string{"SID", "HSID", "SSID", "APISID", "SAPISID"} {
		if cookies[name] != "" {
			return true
		}
	}
	return false
}

func fetchCSRFToken(ctx context.Context, hc *httpx.Client, labsCookies map[string]string) (string, error) {
	resp, err := hc.Do(ctx, &httpx.Request{
		Method:      "GET",
		URL:         labsAuthBase + csrfPath,
		Cookies:     cookieHeader(labsCookies),
		DisableQUIC: true,
		Headers: map[string]string{
			"Accept":  "application/json, text/plain, */*",
			"Referer": labsAuthBase,
		},
	})
	if err != nil {
		return "", &SessionLoginError{Message: "could not reach the Labs CSRF endpoint: " + err.Error(), Transient: true}
	}
	if resp.Status != 200 {
		return "", loginError("CSRF endpoint returned %d", resp.Status)
	}
	mergeSetCookies(labsCookies, resp.Header)

	var payload struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil || payload.CSRFToken == "" {
		return "", loginError("the CSRF response carried no csrfToken")
	}
	return payload.CSRFToken, nil
}

func startGoogleSignin(ctx context.Context, hc *httpx.Client, labsCookies map[string]string, csrfToken string) (string, error) {
	form := url.Values{
		"csrfToken":   {csrfToken},
		"callbackUrl": {labsAuthBase},
		"json":        {"true"},
	}

	resp, err := hc.Do(ctx, &httpx.Request{
		Method: "POST",
		URL:    labsAuthBase + signinPath,
		Body:   []byte(form.Encode()),
		Headers: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
			"Accept":       "application/json, text/plain, */*",
			"Referer":      labsAuthBase,
			"Origin":       config.LabsBase,
		},
		Cookies:     cookieHeader(labsCookies),
		DisableQUIC: true,
	})
	if err != nil {
		return "", &SessionLoginError{Message: "could not reach the Labs sign-in endpoint: " + err.Error(), Transient: true}
	}
	if resp.Status != 200 {
		return "", loginError("signin/google returned %d", resp.Status)
	}
	mergeSetCookies(labsCookies, resp.Header)

	var payload struct {
		Redirect string `json:"redirect"`
		URL      string `json:"url"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return "", loginError("signin/google returned a non-JSON body")
	}
	if payload.Redirect == "" {
		payload.Redirect = payload.URL
	}
	if payload.Redirect == "" {
		return "", loginError("signin/google did not return an OAuth URL")
	}
	return payload.Redirect, nil
}

// withLoginHint pins the OAuth flow to a specific account, which matters when
// the browser profile has several Google accounts signed in.
func withLoginHint(rawURL, email string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	query := parsed.Query()
	query.Set("login_hint", email)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func followGoogleOAuth(ctx context.Context, hc *httpx.Client, startURL string, googleCookies map[string]string) (string, error) {
	currentURL := startURL
	googleCookieHeader := cookieHeader(googleCookies)

	for hop := 0; hop < maxOAuthHops; hop++ {
		referer := googleAccounts
		if hop == 0 {
			referer = config.LabsBase + "/"
		}

		resp, err := hc.Do(ctx, &httpx.Request{
			Method:      "GET",
			URL:         currentURL,
			Cookies:     googleCookieHeader,
			DisableQUIC: true,
			Headers: map[string]string{
				"Accept":         "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
				"Referer":        referer,
				"Sec-Fetch-Dest": "document",
				"Sec-Fetch-Mode": "navigate",
			},
		})
		if err != nil {
			return "", &SessionLoginError{Message: fmt.Sprintf("Google OAuth hop %d failed: %v", hop+1, err), Transient: true}
		}

		if location := resp.Header.Get("Location"); location != "" {
			if strings.Contains(location, callbackMarker) {
				return location, nil
			}
			currentURL = location
			continue
		}

		if resp.Status == 200 {
			body := resp.Text()
			if strings.Contains(body, "signin/rejected") {
				return "", loginError(
					"Google rejected the protocol sign-in; the Google cookies are expired or the " +
						"account is flagged. Sign in again in the browser and re-sync.")
			}
			if extracted := extractRedirectFromHTML(body); extracted != "" {
				resolved := resolveReference(currentURL, extracted)
				if strings.Contains(resolved, callbackMarker) {
					return resolved, nil
				}
				currentURL = resolved
				continue
			}
		}

		return "", loginError("Google OAuth returned no usable redirect (HTTP %d)", resp.Status)
	}

	return "", loginError("Google OAuth exceeded %d hops without reaching the Labs callback", maxOAuthHops)
}

func completeCallback(ctx context.Context, hc *httpx.Client, callbackURL string, labsCookies map[string]string) (string, error) {
	currentURL := callbackURL

	for hop := 0; hop < maxCallbackHops; hop++ {
		resp, err := hc.Do(ctx, &httpx.Request{
			Method:      "GET",
			URL:         currentURL,
			Cookies:     cookieHeader(labsCookies),
			DisableQUIC: true,
			Headers: map[string]string{
				"Accept":         "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
				"Referer":        googleAccounts,
				"Sec-Fetch-Dest": "document",
				"Sec-Fetch-Mode": "navigate",
			},
		})
		if err != nil {
			return "", &SessionLoginError{Message: "Labs callback failed: " + err.Error(), Transient: true}
		}

		if token := extractSessionCookie(resp.Header); token != "" {
			return token, nil
		}
		mergeSetCookies(labsCookies, resp.Header)

		location := resp.Header.Get("Location")
		if location == "" {
			break
		}
		currentURL = resolveReference(currentURL, location)
	}

	return "", loginError("the Labs callback did not set a session cookie")
}

// resolveReference resolves a possibly relative URL against a base.
func resolveReference(base, reference string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return reference
	}
	refURL, err := url.Parse(reference)
	if err != nil {
		return reference
	}
	return baseURL.ResolveReference(refURL).String()
}

/* ------------------------------------------------------------------ *
 * Jar integration
 * ------------------------------------------------------------------ */

// WithSessionToken returns a copy of the jar with the session cookie replaced.
// A new jar is returned rather than mutating in place so the cookie hash — and
// therefore any cached token keyed on it — changes as one unit.
func WithSessionToken(jar *cookiejar.Jar, token string) *cookiejar.Jar {
	if jar == nil || token == "" {
		return jar
	}

	cookies := jar.Cookies()
	replaced := false
	for i := range cookies {
		if cookies[i].Name == SessionTokenCookie {
			cookies[i].Value = token
			replaced = true
			break
		}
	}
	if !replaced {
		cookies = append(cookies, cookiejar.Cookie{
			Domain:         "labs.google",
			Path:           "/",
			Name:           SessionTokenCookie,
			Value:          token,
			Secure:         true,
			HTTPOnly:       true,
			SameSite:       "Lax",
			ExpirationDate: float64(time.Now().Add(30 * 24 * time.Hour).Unix()),
		})
	}

	return cookiejar.FromCookies(cookies, jar.Source()+"+refreshed")
}
