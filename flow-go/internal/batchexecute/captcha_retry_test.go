package batchexecute

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/httpx"
)

// The priming handshake resends a request. So does a session refresh after a
// 401. Both used to resend the payload they were given — and the payload has the
// captcha token baked into it.
//
// **A reCAPTCHA token is single-use.** So the retry presented one the first
// attempt had already spent, and the server answered an empty frame rather than
// an error: no status to check, no message to read, and the only visible symptom
// was a generation that produced nothing. It happened on every boot, because the
// seeded `at` is stale by then and the first call always primes.
//
// These tests pin the property that makes a retry safe: the second request must
// carry a *different* captcha token, not the same one again.

// primingServer answers the first request with the rejection that carries the
// anti-CSRF token, and every later one with a well-formed frame. It records what
// each request actually sent.
func primingServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		requests = append(requests, r.PostForm.Get("f.req"))

		if len(requests) == 1 {
			// What the priming attempt looks like: refused, with the token the
			// client is meant to echo back.
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `[["xsrf","anti-csrf-token"]]`)
			return
		}
		fmt.Fprint(w, ")]}'\n\n[[\"wrb.fr\",\"UpteDb\",\"[]\",null,null,null,\"generic\"]]\n")
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// pointAt redirects the client at a test server for the duration of a test.
func pointAt(t *testing.T, server *httptest.Server) {
	t.Helper()
	original := Origin
	Origin = server.URL
	t.Cleanup(func() { Origin = original })
}

func testClient(t *testing.T) *Client {
	t.Helper()
	hc, err := httpx.New()
	if err != nil {
		t.Fatalf("httpx.New: %v", err)
	}
	jar := jarWith(cookiejar.Cookie{
		Domain: ".google.com", Path: "/", Name: "SAPISID", Value: "v",
	})
	return New(jar, hc)
}

// The payload builder, with the token the client hands it. An empty token means
// "the one you already have", which is what a first attempt passes.
func payloadWithToken(token string) (any, error) {
	if token == "" {
		token = "token-0"
	}
	return []any{token}, nil
}

func TestARetryCarriesAFreshCaptchaToken(t *testing.T) {
	server, requests := primingServer(t)
	pointAt(t, server)

	minted := 0
	opts := CallOptions{
		RefreshCaptcha: func(context.Context) (string, error) {
			minted++
			return fmt.Sprintf("token-%d", minted), nil
		},
	}

	if _, err := testClient(t).call(context.Background(), "UpteDb", opts, payloadWithToken); err != nil {
		t.Fatalf("call: %v", err)
	}

	if len(*requests) != 2 {
		t.Fatalf("the server saw %d requests, want 2 — the priming handshake", len(*requests))
	}
	if (*requests)[0] == (*requests)[1] {
		t.Fatal("the retry resent the payload it was given, captcha token and all. " +
			"A reCAPTCHA token is single-use, so the server answers an empty frame " +
			"rather than an error and the generation silently produces nothing")
	}
	if !strings.Contains((*requests)[1], "token-1") {
		t.Errorf("the retry did not carry a freshly minted token:\n  %s", (*requests)[1])
	}
	if minted != 1 {
		t.Errorf("minted %d tokens, want exactly 1 — one per attempt after the first", minted)
	}
}

// Without a refresher the old behaviour stands, and this records it rather than
// leaving it to be rediscovered: a call that carries a captcha but no way to
// re-mint one resends the spent token.
func TestWithoutARefresherTheRetryResendsTheToken(t *testing.T) {
	server, requests := primingServer(t)
	pointAt(t, server)

	if _, err := testClient(t).call(context.Background(), "UpteDb", CallOptions{}, payloadWithToken); err != nil {
		t.Fatalf("call: %v", err)
	}

	if len(*requests) != 2 {
		t.Fatalf("the server saw %d requests, want 2", len(*requests))
	}
	if (*requests)[0] != (*requests)[1] {
		t.Error("without a refresher the payload should be resent unchanged; " +
			"if this fails the builder is being called with a token it did not receive")
	}
}

// The refresher is offered to every retry, whether or not the payload carries a
// captcha — call cannot see inside the payload, so it cannot tell. That is the
// builder's contract: a builder whose payload has no token ignores the argument
// it is handed, and the request goes out unchanged.
func TestTheRefresherIsOfferedToEveryRetry(t *testing.T) {
	server, requests := primingServer(t)
	pointAt(t, server)

	called := 0
	opts := CallOptions{
		RefreshCaptcha: func(context.Context) (string, error) {
			called++
			return "unused", nil
		},
	}

	// A builder that takes no token, as a captcha-free call's builder would.
	payload := []any{"no captcha here"}
	if _, err := testClient(t).call(context.Background(), "UpteDb", opts,
		func(string) (any, error) { return payload, nil }); err != nil {
		t.Fatalf("call: %v", err)
	}

	if len(*requests) != 2 {
		t.Fatalf("the server saw %d requests, want 2", len(*requests))
	}
	if called != 1 {
		t.Errorf("the refresher ran %d times, want 1 — once, for the retry", called)
	}
	// The builder ignored the token, so the payload is byte-identical. This is
	// the property that makes the refresher safe to offer unconditionally.
	if (*requests)[0] != (*requests)[1] {
		t.Error("a builder that ignores its token should produce the same payload on every attempt")
	}
}

// A refresher that fails must fail the call rather than send a spent token.
func TestAFailingRefresherFailsTheCall(t *testing.T) {
	server, requests := primingServer(t)
	pointAt(t, server)

	opts := CallOptions{
		RefreshCaptcha: func(context.Context) (string, error) {
			return "", fmt.Errorf("the mint is down")
		},
	}

	_, err := testClient(t).call(context.Background(), "UpteDb", opts, payloadWithToken)
	if err == nil {
		t.Fatal("a failing refresher should fail the call, not resend the spent token")
	}
	if !strings.Contains(err.Error(), "fresh captcha") {
		t.Errorf("the error should say what failed: %v", err)
	}
	if len(*requests) != 1 {
		t.Errorf("the server saw %d requests, want 1 — the retry must not go out", len(*requests))
	}
}
