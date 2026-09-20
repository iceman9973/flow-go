package auth

import (
	"strings"
	"testing"
	"time"
)

// TestParseSessionStaleExpiryIgnoresAdvertisedExpiry is the regression test for a
// real defect found against the live endpoint.
//
// Labs returns a payload shaped like:
//
//	{"access_token":"ya29...","expires":"2026-08-28T22:01:36.000Z",
//	 "error":"ACCESS_TOKEN_REFRESH_NEEDED","user":{...}}
//
// The `expires` value is the NextAuth session's expiry, not the bearer token's,
// and it is routinely already in the past while a working token is still issued.
// Trusting it would mark every freshly minted session as expired and re-mint on
// every single request.
func TestParseSessionStaleExpiryIgnoresAdvertisedExpiry(t *testing.T) {
	body := []byte(`{
	  "user": {"email": "someone@example.com", "id": "12345"},
	  "expires": "2026-08-28T22:01:36.000Z",
	  "access_token": "ya29.a0AdMD6ExampleTokenValue",
	  "error": "ACCESS_TOKEN_REFRESH_NEEDED"
	}`)

	session, err := parseSession(body, "hash")
	if err != nil {
		t.Fatalf("parseSession failed: %v", err)
	}

	if session.AccessToken != "ya29.a0AdMD6ExampleTokenValue" {
		t.Errorf("AccessToken = %q", session.AccessToken)
	}
	if session.Email != "someone@example.com" {
		t.Errorf("Email = %q, want someone@example.com", session.Email)
	}
	if session.UserID != "12345" {
		t.Errorf("UserID = %q, want 12345", session.UserID)
	}
	if session.UpstreamError != "ACCESS_TOKEN_REFRESH_NEEDED" {
		t.Errorf("UpstreamError = %q, want ACCESS_TOKEN_REFRESH_NEEDED", session.UpstreamError)
	}

	// The past expiry must have been discarded, not stored.
	if !session.ExpiresAt.IsZero() {
		t.Errorf("a past `expires` must not be stored as an expiry, got %s", session.ExpiresAt)
	}

	// And the session must be considered usable, governed by the local TTL.
	if !session.Usable(time.Now(), 40*time.Minute) {
		t.Error("a freshly minted session with a stale advertised expiry must be usable")
	}
}

func TestParseSessionHonoursFutureExpiry(t *testing.T) {
	future := time.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339)
	body := []byte(`{"access_token":"ya29.future","expires":"` + future + `"}`)

	session, err := parseSession(body, "hash")
	if err != nil {
		t.Fatalf("parseSession failed: %v", err)
	}
	if session.ExpiresAt.IsZero() {
		t.Fatal("a genuinely future expiry should be stored")
	}
	if !session.Usable(time.Now(), 2*time.Hour) {
		t.Error("the session should be usable before its expiry")
	}
}

func TestUsableLocalTTLWins(t *testing.T) {
	session := &Session{
		AccessToken: "ya29.x",
		MintedAt:    time.Now().Add(-2 * time.Hour),
	}

	if session.Usable(time.Now(), 40*time.Minute) {
		t.Error("a session older than the local TTL must not be usable")
	}
	if !session.Usable(time.Now(), 3*time.Hour) {
		t.Error("a session inside a longer TTL should be usable")
	}
}

func TestUsableRejectsEmptyToken(t *testing.T) {
	now := time.Now()
	if (&Session{MintedAt: now}).Usable(now, time.Hour) {
		t.Error("a session with no access token must not be usable")
	}

	var nilSession *Session
	if nilSession.Usable(now, time.Hour) {
		t.Error("a nil session must not be usable")
	}
}

// TestParseSessionEmptyMeansSignedOut covers the `{}` response that Labs returns
// when the cookies are not authenticated. It must be a clear error, not a
// silent empty token.
func TestParseSessionEmptyMeansSignedOut(t *testing.T) {
	_, err := parseSession([]byte(`{}`), "hash")
	if err == nil {
		t.Fatal("an empty session payload should be an error")
	}
	if !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("the error should explain that the cookies are not signed in, got: %v", err)
	}
}

func TestParseSessionMissingToken(t *testing.T) {
	_, err := parseSession([]byte(`{"user":{"email":"a@b.c"}}`), "hash")
	if err == nil {
		t.Fatal("a payload with no access token should be an error")
	}
	if !strings.Contains(err.Error(), "no access token") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestParseSessionNestedToken(t *testing.T) {
	// The token sometimes arrives nested under the user object rather than at
	// the top level. The parser walks one level down for this reason.
	body := []byte(`{"user":{"email":"a@b.c","access_token":"ya29.nested"}}`)
	session, err := parseSession(body, "hash")
	if err != nil {
		t.Fatalf("parseSession failed: %v", err)
	}
	if session.AccessToken != "ya29.nested" {
		t.Errorf("AccessToken = %q, want ya29.nested", session.AccessToken)
	}
}

func TestParseSessionBearerPrefixStripped(t *testing.T) {
	body := []byte(`{"access_token":"Bearer ya29.prefixed"}`)
	session, err := parseSession(body, "hash")
	if err != nil {
		t.Fatalf("parseSession failed: %v", err)
	}
	if session.AccessToken != "ya29.prefixed" {
		t.Errorf("AccessToken = %q, want the Bearer prefix stripped", session.AccessToken)
	}
	if session.TokenType != "Bearer" {
		t.Errorf("TokenType = %q, want the Bearer default", session.TokenType)
	}
}

func TestParseSessionProjects(t *testing.T) {
	body := []byte(`{
	  "access_token":"ya29.x",
	  "projects":[{"id":"proj-1","name":"First"},{"id":"proj-2","name":"Second"}]
	}`)
	session, err := parseSession(body, "hash")
	if err != nil {
		t.Fatalf("parseSession failed: %v", err)
	}
	if len(session.Projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(session.Projects))
	}
	if session.Projects[0].ID != "proj-1" {
		t.Errorf("Projects[0].ID = %q, want proj-1", session.Projects[0].ID)
	}
}

func TestResolveProjectID(t *testing.T) {
	// Explicit wins.
	s := &Session{Projects: []Project{{ID: "from-session"}}}
	if got := s.ResolveProjectID("explicit"); got != "explicit" {
		t.Errorf("ResolveProjectID = %q, want explicit", got)
	}

	// Then the session's first project.
	if got := s.ResolveProjectID(""); got != "from-session" {
		t.Errorf("ResolveProjectID = %q, want from-session", got)
	}

	// Then the configured default.
	empty := &Session{}
	if got := empty.ResolveProjectID(""); got == "" {
		t.Error("ResolveProjectID should fall back to the default project, got empty")
	}
}

func TestProjectIDFromFlowURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://labs.google/fx/tools/flow/project/0143adf4-5864-4cb4-abb5-fe4254ad0dc7", "0143adf4-5864-4cb4-abb5-fe4254ad0dc7"},
		{"https://labs.google/fx/tools/flow/project/abc123def456", "abc123def456"},
		{"https://labs.google/fx/tools/flow", ""},
		{"https://labs.google/fx/tools/flow/project/ab", ""}, // too short to be an ID
		{"not a url at all", ""},
		{"", ""},
	}

	for _, tc := range cases {
		if got := ProjectIDFromFlowURL(tc.url); got != tc.want {
			t.Errorf("ProjectIDFromFlowURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestParseTime(t *testing.T) {
	for _, value := range []string{
		"2026-09-18T13:59:05Z",
		"2026-09-18T13:59:05.000Z",
		"2026-09-18T13:59:05+05:30",
		"2026-09-18 13:59:05",
	} {
		if _, err := parseTime(value); err != nil {
			t.Errorf("parseTime(%q) failed: %v", value, err)
		}
	}

	if _, err := parseTime("definitely not a time"); err == nil {
		t.Error("parseTime should reject a non-timestamp")
	}
}

// TestSessionJarSwapInvalidates confirms that new cookies invalidate a cached
// token, which is what stops a token outliving the credentials it came from.
func TestSessionJarSwapInvalidates(t *testing.T) {
	provider := &Provider{}
	provider.store(&Session{AccessToken: "ya29.old", MintedAt: time.Now()})

	if provider.cached == nil {
		t.Fatal("expected a cached session")
	}

	provider.Invalidate()
	if provider.cached != nil {
		t.Error("Invalidate should drop the cached session")
	}
}
