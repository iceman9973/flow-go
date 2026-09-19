package recaptcha

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kodelyx/cdp-control/cdp"
)

// TestGotoRecaptchaPageIsANoOpWithoutAURL pins the optional part of the fix.
//
// The navigation is only possible when the caller can name a page that loads the
// client. A caller that cannot must keep the previous behaviour rather than fail,
// so both an absent resolver and an empty one return without touching the tab.
func TestGotoRecaptchaPageIsANoOpWithoutAURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resolve PageURLResolver
	}{
		{"nil resolver", nil},
		{"empty resolver", func() string { return "" }},
		{"whitespace resolver", func() string { return "   " }},
	} {
		// A client with no connection: if the code reached the tab it would fail,
		// so returning nil here is the proof it short-circuited.
		p := NewBroker(cdp.New(nil), "", tc.resolve)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := p.gotoRecaptchaPage(ctx)
		cancel()

		if err != nil {
			t.Errorf("%s: expected a no-op, got %v", tc.name, err)
		}
	}
}

// TestHasRecaptchaIsFalseWithoutAConnection keeps the probe honest about its
// failure mode. It gates a navigation, so an error must read as "not there"
// rather than as "there" — reporting true would skip the navigation that is the
// whole point of the check.
func TestHasRecaptchaIsFalseWithoutAConnection(t *testing.T) {
	p := NewBroker(cdp.New(nil), "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if p.hasRecaptcha(ctx) {
		t.Error("a disconnected client cannot report a loaded reCAPTCHA client")
	}
}

// TestNewBrokerKeepsTheResolver pins that the resolver actually reaches the
// provider. Without it the navigation silently never happens, which is exactly
// the bug this fix addresses.
func TestNewBrokerKeepsTheResolver(t *testing.T) {
	p := NewBroker(cdp.New(nil), "", func() string { return "https://example.test/project/x" })
	if p.pageURL == nil {
		t.Fatal("the resolver was dropped")
	}
	if got := p.pageURL(); got != "https://example.test/project/x" {
		t.Errorf("resolver returned %q", got)
	}
}

// TestBuildFallsBackWithoutABroker pins the chain assembly around the new
// argument: a missing client must still produce a usable provider rather than a
// nil one, in every mode.
func TestBuildFallsBackWithoutABroker(t *testing.T) {
	for _, mode := range []string{"", "auto", "broker", "http"} {
		p := Build(mode, nil, nil, nil, nil)
		if p == nil {
			t.Errorf("mode %q: Build returned nil", mode)
			continue
		}
		if name := p.Name(); name == "" {
			t.Errorf("mode %q: provider has no name", mode)
		}
	}
}

// TestBuildNamesTheBrokerWhenPresent is the other half: with a client the chain
// must actually include the broker, so the high-score path is not silently
// dropped by the signature change.
func TestBuildNamesTheBrokerWhenPresent(t *testing.T) {
	p := Build("auto", nil, cdp.New(nil), func() string { return "https://example.test/project/x" }, nil)
	if p == nil {
		t.Fatal("Build returned nil")
	}
	if name := p.Name(); !strings.Contains(name, "broker") {
		t.Errorf("the auto chain should include the broker, got %q", name)
	}
}

// TestAutoChainPrefersTheFlowOperation pins the order of the automatic chain.
//
// The broker reaches the page with `cdp.evaluate`, which is exactly the surface a
// Flow extension does not expose — it offers named operations instead. So with a
// Flow bridge attached the broker cannot mint at all, and the extension's own
// captcha operation has to come first. It was implemented, advertised, and called
// by nothing until this.
//
// The broker stays in the chain regardless: a generic bridge has no `flow.captcha`,
// and that case still needs it.
func TestAutoChainPrefersTheFlowOperation(t *testing.T) {
	resolver := func() *cdp.Client { return nil }
	built := Build("auto", nil, cdp.New(nil), func() string { return "https://example.test/project/x" }, resolver)

	chain, ok := built.(*Chain)
	if !ok {
		t.Fatalf("auto should build a chain, got %T", built)
	}
	if len(chain.providers) == 0 {
		t.Fatal("the chain is empty")
	}
	if name := chain.providers[0].Name(); name != "flow.captcha" {
		t.Errorf("first provider is %q, want flow.captcha — the broker cannot mint through a "+
			"Flow bridge, so the extension's own operation has to be tried first", name)
	}

	var sawBroker bool
	for _, provider := range chain.providers {
		if provider.Name() == "broker" {
			sawBroker = true
		}
	}
	if !sawBroker {
		t.Error("the broker must stay in the chain for the case where a generic bridge is attached")
	}
}

// TestAutoChainOmitsTheFlowProviderWithoutAResolver keeps the old behaviour
// reachable: a caller that cannot resolve a client gets the chain it had before.
func TestAutoChainOmitsTheFlowProviderWithoutAResolver(t *testing.T) {
	built := Build("auto", nil, cdp.New(nil), func() string { return "https://example.test/project/x" }, nil)

	chain, ok := built.(*Chain)
	if !ok {
		t.Fatalf("auto should build a chain, got %T", built)
	}
	for _, provider := range chain.providers {
		if provider.Name() == "flow.captcha" {
			t.Error("no resolver means no flow.captcha provider; it could not mint anything")
		}
	}
}

// TestFlowProviderFailsCleanly covers the two ways there is no client to ask.
//
// A provider that returned an empty token with a nil error would be read by the
// chain as a success and stop the walk — sending no token at all while a working
// fallback sat behind it.
func TestFlowProviderFailsCleanly(t *testing.T) {
	if _, err := NewFlow(nil).Token(context.Background(), "IMAGE_GENERATION"); err == nil {
		t.Error("a nil resolver must return an error, not an empty token")
	}
	if _, err := NewFlow(func() *cdp.Client { return nil }).Token(context.Background(), "IMAGE_GENERATION"); err == nil {
		t.Error("a resolver that yields no client must return an error")
	}
}
