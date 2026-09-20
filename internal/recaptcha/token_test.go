package recaptcha

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kodelyx/flow-go/internal/httpx"
)

// testClient is the transport the provider needs; a nil one panics.
func testClient(t *testing.T) *httpx.Client {
	t.Helper()
	client, err := httpx.New()
	if err != nil {
		t.Fatalf("httpx.New: %v", err)
	}
	return client
}

// The exchange the transport provider runs, against a local server.
//
// The bug this guards against cost hours and was invisible: the provider cached
// its token for two minutes, and a reCAPTCHA token is single-use. So the first
// generation in a process worked and every one after it inside the window came
// back as an empty frame — 200, no error, no charge. Nothing in the response
// said why, and the command line hid it entirely because a CLI run is a fresh
// process and minted a new token every time.
//
// The property worth pinning is therefore not "a token comes back" but "a
// *different* token comes back on the second call". A cache would pass the
// first assertion and fail the second.

// fakeRecaptcha serves the three endpoints the provider walks, handing out a
// distinct token per reload so reuse is detectable.
func fakeRecaptcha(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()

	var reloads int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/enterprise.js"):
			// The release id the provider extracts and replays.
			fmt.Fprint(w, `var x="/recaptcha/releases/TESTRELEASE/recaptcha__en.js";`)

		case strings.HasSuffix(r.URL.Path, "/anchor"):
			fmt.Fprint(w, `<input id="recaptcha-token" value="challenge-token">`)

		case strings.HasSuffix(r.URL.Path, "/reload"):
			n := atomic.AddInt64(&reloads, 1)
			// Comfortably over minTokenLength, and different every time.
			fmt.Fprintf(w, ")]}'\n\n[\"rresp\",%q]", strings.Repeat(fmt.Sprintf("t%d", n), 400))

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &reloads
}

// pointAt redirects the provider at a test server for the duration of a test.
func pointAt(t *testing.T, server *httptest.Server) {
	t.Helper()
	// The base carries a path because releaseVersion appends ".js" to it
	// directly — the real value ends in "/recaptcha/enterprise", so a bare host
	// would produce "host.js" and fail to parse.
	original := recaptchaBase
	recaptchaBase = server.URL + "/recaptcha/enterprise"
	t.Cleanup(func() { recaptchaBase = original })
}

func TestTokenMintsFreshOnEveryCall(t *testing.T) {
	server, reloads := fakeRecaptcha(t)
	pointAt(t, server)

	provider := NewHTTP(testClient(t), "SITEKEY", "https://example.test")
	ctx := context.Background()

	first, err := provider.Token(ctx, "IMAGE_GENERATION")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := provider.Token(ctx, "IMAGE_GENERATION")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if first == second {
		t.Error("both calls returned the same token — a reCAPTCHA token is single-use, " +
			"so the second call would be verified against a token that has already been " +
			"spent and Flow answers an empty frame with no error")
	}
	if got := atomic.LoadInt64(reloads); got != 2 {
		t.Errorf("the server saw %d reloads, want 2 — the second call was served from "+
			"something other than a fresh mint", got)
	}
}

// A third call, because the failure mode was "everything after the first".
func TestTokenKeepsMintingPastTheFirst(t *testing.T) {
	server, _ := fakeRecaptcha(t)
	pointAt(t, server)

	provider := NewHTTP(testClient(t), "SITEKEY", "https://example.test")
	ctx := context.Background()

	seen := make(map[string]bool)
	for i := 0; i < 3; i++ {
		token, err := provider.Token(ctx, "VIDEO_GENERATION")
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if seen[token] {
			t.Fatalf("call %d repeated a token", i+1)
		}
		seen[token] = true
	}
}

// The transport has to be usable with no browser anywhere in sight, which is the
// whole point of it: this is the provider a headless run depends on.
func TestTokenNeedsNoBrowser(t *testing.T) {
	server, _ := fakeRecaptcha(t)
	pointAt(t, server)

	// No cdp client, no resolver, no extension — just the transport.
	provider := Build("auto", testClient(t), nil, nil, nil, "")

	token, err := provider.Token(context.Background(), "IMAGE_GENERATION")
	if err != nil {
		t.Fatalf("the default chain failed with no browser attached: %v", err)
	}
	if len(token) < minTokenLength {
		t.Errorf("token is %d chars, want at least %d", len(token), minTokenLength)
	}
}
