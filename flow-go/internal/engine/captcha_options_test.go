package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/batchexecute"
	"github.com/kodelyx/flow-go/flow-go/internal/recaptcha"
)

// stubProvider records the actions it was asked for and hands back a token that
// names the action, so a test can tell which one the caller used.
type stubProvider struct {
	actions []string
	err     error
}

func (p *stubProvider) Name() string { return "stub" }

func (p *stubProvider) Token(_ context.Context, action string) (string, error) {
	p.actions = append(p.actions, action)
	if p.err != nil {
		return "", p.err
	}
	return "token-for-" + action, nil
}

// The refresher must be present on every captcha-carrying call. Its absence is
// the bug that cost hours twice, and it is invisible: a retry without one
// presents a spent token, Flow answers an empty frame rather than an error, and
// the generation quietly does nothing.
func TestCaptchaOptionsAlwaysCarriesARefresher(t *testing.T) {
	e := &Engine{captcha: &stubProvider{}}

	opts := e.captchaOptions(recaptcha.ActionVideo, batchexecute.CallOptions{})

	if opts.RefreshCaptcha == nil {
		t.Fatal("no refresher: a retry on this call will resend a spent single-use " +
			"token, and the server answers an empty frame rather than an error")
	}
}

// The caller's options are not this helper's business. It adds the refresher and
// nothing else — the four generation calls set a source path and a build label,
// and the upload sets neither. Folding those in would change what the upload
// sends.
func TestCaptchaOptionsKeepsWhatTheCallerSet(t *testing.T) {
	e := &Engine{captcha: &stubProvider{}}

	given := batchexecute.CallOptions{
		SourcePath: "/project/proj-1",
		BuildLabel: "boq_labs-ai-sandbox-frontend_20260917.00_p0",
		SessionID:  "12345",
	}
	opts := e.captchaOptions(recaptcha.ActionVideo, given)

	if opts.SourcePath != given.SourcePath {
		t.Errorf("SourcePath = %q, want %q", opts.SourcePath, given.SourcePath)
	}
	if opts.BuildLabel != given.BuildLabel {
		t.Errorf("BuildLabel = %q, want %q", opts.BuildLabel, given.BuildLabel)
	}
	if opts.SessionID != given.SessionID {
		t.Errorf("SessionID = %q, want %q", opts.SessionID, given.SessionID)
	}
	if opts.RPCID != given.RPCID {
		t.Errorf("RPCID = %q, want %q", opts.RPCID, given.RPCID)
	}
}

// The upload is the one call that sets neither a source path nor a build label.
// A helper that defaulted them would start sending `bl` on uploads, which is a
// change to the request, not a refactor.
func TestCaptchaOptionsAddsNothingButTheRefresher(t *testing.T) {
	e := &Engine{captcha: &stubProvider{}}

	opts := e.captchaOptions(recaptcha.ActionImage, batchexecute.CallOptions{})

	if opts.SourcePath != "" {
		t.Errorf("SourcePath = %q, want empty — the upload sets none", opts.SourcePath)
	}
	if opts.BuildLabel != "" {
		t.Errorf("BuildLabel = %q, want empty — the upload sets none", opts.BuildLabel)
	}
	if opts.RefreshCaptcha == nil {
		t.Error("no refresher")
	}
}

// **The action has to match the payload's.** A token minted for the wrong action
// is rejected, and rejected the same silent way — so the action is worth pinning
// rather than trusting to a reader's eye at five call sites.
func TestTheRefresherAsksForTheActionItWasGiven(t *testing.T) {
	for _, action := range []string{recaptcha.ActionVideo, recaptcha.ActionImage} {
		t.Run(action, func(t *testing.T) {
			provider := &stubProvider{}
			e := &Engine{captcha: provider}

			opts := e.captchaOptions(action, batchexecute.CallOptions{})
			if opts.RefreshCaptcha == nil {
				// Guard rather than call: a nil deref panics the whole test
				// binary, which hides every other result in it. The absence of
				// the refresher is the thing under test, so it should be a
				// failure that reports, not a crash that truncates.
				t.Fatal("no refresher")
			}
			token, err := opts.RefreshCaptcha(context.Background())
			if err != nil {
				t.Fatalf("refresher: %v", err)
			}

			if len(provider.actions) != 1 {
				t.Fatalf("the provider was asked %d times, want 1", len(provider.actions))
			}
			if provider.actions[0] != action {
				t.Errorf("the provider was asked for %q, want %q — a token minted for the "+
					"wrong action is rejected, silently", provider.actions[0], action)
			}
			if !strings.Contains(token, action) {
				t.Errorf("token = %q, want one naming %q", token, action)
			}
		})
	}
}

// With no provider the refresher must fail loudly. A refresher that returned an
// empty token instead would send a call whose token is the empty string — which
// is exactly the silent shape this whole area is about.
func TestTheRefresherFailsLoudlyWithoutAProvider(t *testing.T) {
	e := &Engine{}
	opts := e.captchaOptions(recaptcha.ActionVideo, batchexecute.CallOptions{})
	if opts.RefreshCaptcha == nil {
		t.Fatal("no refresher")
	}

	token, err := opts.RefreshCaptcha(context.Background())
	if err == nil {
		t.Fatalf("a missing provider should fail the refresh, not return %q", token)
	}
	if !strings.Contains(err.Error(), "no reCAPTCHA provider") {
		t.Errorf("the error should say what is missing: %v", err)
	}
}

// A provider that fails must propagate, so batchexecute can fail the call rather
// than retry with a dead token.
func TestTheRefresherPropagatesAProviderFailure(t *testing.T) {
	provider := &stubProvider{err: fmt.Errorf("the mint is down")}
	e := &Engine{captcha: provider}

	opts := e.captchaOptions(recaptcha.ActionVideo, batchexecute.CallOptions{})
	if opts.RefreshCaptcha == nil {
		t.Fatal("no refresher")
	}
	if _, err := opts.RefreshCaptcha(context.Background()); err == nil {
		t.Fatal("a failing provider should fail the refresh")
	}
}
