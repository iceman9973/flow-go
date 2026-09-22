package flowapi

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubCaptcha answers however a test tells it to, and counts the asking.
type stubCaptcha struct {
	token string
	err   error
	calls int
}

func (s *stubCaptcha) Token(_ context.Context, _ string) (string, error) {
	s.calls++
	return s.token, s.err
}

func (s *stubCaptcha) Name() string { return "stub" }

// A provider that is deliberately off and a provider that failed both produce an
// empty token; only the error tells them apart. Collapsing that distinction is
// what made a dead captcha look like a disabled one — the request went out
// tokenless either way, and the refusal that came back named neither.

func TestAMintFailureIsReturnedWithItsCause(t *testing.T) {
	provider := &stubCaptcha{err: errors.New("the broker is down")}

	_, err := mintCaptcha(context.Background(), provider, recaptchaActionVideo)
	if err == nil {
		t.Fatal("a failed mint must be an error, not an empty token")
	}
	if !strings.Contains(err.Error(), "captcha mint failed") {
		t.Errorf("error %q should name the captcha mint", err)
	}
	if !strings.Contains(err.Error(), "the broker is down") {
		t.Errorf("error %q should carry the underlying cause", err)
	}
}

func TestAnEmptyTokenWithNoErrorIsTheProviderBeingOff(t *testing.T) {
	provider := &stubCaptcha{token: ""}

	token, err := mintCaptcha(context.Background(), provider, recaptchaActionVideo)
	if err != nil {
		t.Fatalf("a provider that is deliberately off must not be an error: %v", err)
	}
	if token != "" {
		t.Errorf("token = %q, want empty", token)
	}
	if provider.calls != 1 {
		t.Errorf("the provider was called %d times, want 1", provider.calls)
	}
}

func TestAMintedTokenIsPassedThrough(t *testing.T) {
	provider := &stubCaptcha{token: "token-value"}

	token, err := mintCaptcha(context.Background(), provider, recaptchaActionVideo)
	if err != nil {
		t.Fatalf("mintCaptcha: %v", err)
	}
	if token != "token-value" {
		t.Errorf("token = %q, want token-value", token)
	}
}

// TestAnEndpointThatUsesNoCaptchaNeverMints matters because minting is not free:
// it drives the extension, and doing it for an endpoint that ignores the token
// spends that work for nothing.
func TestAnEndpointThatUsesNoCaptchaNeverMints(t *testing.T) {
	provider := &stubCaptcha{err: errors.New("this must not be reached")}

	token, err := mintCaptcha(context.Background(), provider, "")
	if err != nil {
		t.Fatalf("an endpoint with no captcha action must not mint: %v", err)
	}
	if token != "" {
		t.Errorf("token = %q, want empty", token)
	}
	if provider.calls != 0 {
		t.Errorf("the provider was called %d times, want 0", provider.calls)
	}
}

func TestNoProviderIsNotAFailure(t *testing.T) {
	token, err := mintCaptcha(context.Background(), nil, recaptchaActionVideo)
	if err != nil {
		t.Fatalf("a nil provider must not be an error: %v", err)
	}
	if token != "" {
		t.Errorf("token = %q, want empty", token)
	}
}
