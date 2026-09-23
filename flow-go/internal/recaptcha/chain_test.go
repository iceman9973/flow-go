package recaptcha

import (
	"context"
	"fmt"
	"testing"
)

// stubProvider is a Provider with a scripted answer.
type stubProvider struct {
	name  string
	token string
	err   error
}

func (s stubProvider) Token(context.Context, string) (string, error) { return s.token, s.err }
func (s stubProvider) Name() string                                  { return s.name }

// TestChainLastProviderNamesTheWinner is the observability half of the refusal
// escalation.
//
// Name() reports the whole chain — "chain(http,empty)" — which says what was
// *tried* and not what won. When a token is refused, the question that matters is
// whether this run actually used the browser or whether the broker failed and the
// chain fell through to the transport. Before this, those two were distinguishable
// only by reading the log.
func TestChainLastProviderNamesTheWinner(t *testing.T) {
	// The first provider fails, so the second is the one that supplies the token.
	chain := NewChain(
		stubProvider{name: "flow.captcha", err: fmt.Errorf("no extension attached")},
		stubProvider{name: "http", token: "a-real-token"},
		stubProvider{name: "empty"},
	)

	if _, err := chain.Token(context.Background(), ActionVideo); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got := chain.LastProvider(); got != "http" {
		t.Errorf("LastProvider() = %q, want %q — the chain's own Name reports what it tried, "+
			"not what won", got, "http")
	}
}

// A chain that produces nothing must not keep naming whoever won last time. A
// stale answer here would send a reader after a token that was never minted.
func TestChainLastProviderIsClearedWhenNothingIsMinted(t *testing.T) {
	barren := NewChain(stubProvider{name: "empty"})

	if _, err := barren.Token(context.Background(), ActionVideo); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got := barren.LastProvider(); got != "" {
		t.Errorf("LastProvider() = %q on a chain that minted nothing, want an empty string", got)
	}
}

// The reporter interface is what lets the engine ask which strategy won without
// assuming it holds a chain — so a chain has to satisfy it.
func TestChainSatisfiesTheReporterInterface(t *testing.T) {
	var provider Provider = NewChain(stubProvider{name: "http", token: "t"})

	if _, ok := provider.(LastProviderReporter); !ok {
		t.Fatal("*Chain must satisfy LastProviderReporter")
	}
}

// The page provider must not fall back to the transport. Handing back another
// token from the source that was just refused would be asking the same question
// again, and the caller would have spent a round trip to learn nothing.
//
// A page provider with nothing to mint from answers with an empty token rather
// than an error, which is why the escalation path has to treat an empty token as
// a failure — passing it on would resend the token that was just refused.
func TestPageProviderHasNoTransportFallback(t *testing.T) {
	provider := NewPageProvider(nil, nil, nil)

	if _, ok := provider.(LastProviderReporter); !ok {
		t.Error("the page provider is a chain, so it should be able to report what won")
	}

	token, err := provider.Token(context.Background(), ActionVideo)
	if err != nil {
		t.Fatalf("a page provider with nothing attached should answer empty, not fail: %v", err)
	}
	if token != "" {
		t.Errorf("token = %q, want empty — there is no page to mint from", token)
	}
}
