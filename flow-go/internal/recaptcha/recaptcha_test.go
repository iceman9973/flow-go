package recaptcha

import (
	"context"
	"testing"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/cdp"
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
		p := Build(mode, nil, nil, nil, nil, "")
		if p == nil {
			t.Errorf("mode %q: Build returned nil", mode)
			continue
		}
		if name := p.Name(); name == "" {
			t.Errorf("mode %q: provider has no name", mode)
		}
	}
}

// TestAutoChainIsServerSideOnly pins the default to the transport.
//
// The chain used to lead with `flow.captcha` and the broker, which meant every
// generation needed an extension attached and a tab sitting on a Flow project.
// The transport turns out to be sufficient — the defect was that it reused a
// single-use token — so the default asks the browser for nothing.
//
// This is the assertion that would have caught the regression the other way
// round: a provider creeping back into the default chain is exactly what makes
// the browser load-bearing again, and it would do so silently.
func TestAutoChainIsServerSideOnly(t *testing.T) {
	for _, mode := range []string{"auto", "", "http"} {
		built := Build(mode, nil, cdp.New(nil),
			func() string { return "https://example.test/project/x" },
			func() *cdp.Client { return cdp.New(nil) }, "")

		chain, ok := built.(*Chain)
		if !ok {
			t.Fatalf("mode %q: expected a chain, got %T", mode, built)
		}
		for _, provider := range chain.providers {
			switch provider.Name() {
			case "flow.captcha", "broker":
				t.Errorf("mode %q: %q is in the default chain; the browser is not "+
					"supposed to be asked for a token unless the caller opts in",
					mode, provider.Name())
			}
		}
		if name := chain.providers[0].Name(); name != "http" {
			t.Errorf("mode %q: first provider is %q, want http", mode, name)
		}
	}
}

// TestBrokerModeOptsIntoThePage is the other half: the page path has to remain
// reachable, and in the order that works — the broker cannot mint through a Flow
// bridge, which offers named operations rather than `cdp.evaluate`, so the
// extension's own operation has to come first.
func TestBrokerModeOptsIntoThePage(t *testing.T) {
	resolver := func() *cdp.Client { return cdp.New(nil) }
	built := Build("broker", nil, cdp.New(nil),
		func() string { return "https://example.test/project/x" }, resolver, "")

	chain, ok := built.(*Chain)
	if !ok {
		t.Fatalf("broker mode should build a chain, got %T", built)
	}
	if len(chain.providers) == 0 {
		t.Fatal("the chain is empty")
	}
	if name := chain.providers[0].Name(); name != "flow.captcha" {
		t.Errorf("first provider is %q, want flow.captcha", name)
	}

	var sawBroker, sawHTTP bool
	for _, provider := range chain.providers {
		switch provider.Name() {
		case "broker":
			sawBroker = true
		case "http":
			sawHTTP = true
		}
	}
	if !sawBroker {
		t.Error("the broker must be in the broker chain: a generic bridge has no flow.captcha")
	}
	if !sawHTTP {
		t.Error("the transport must stay behind the page providers as a fallback")
	}
}

// TestAutoChainOmitsTheFlowProviderWithoutAResolver keeps the old behaviour
// reachable: a caller that cannot resolve a client gets the chain it had before.
func TestAutoChainOmitsTheFlowProviderWithoutAResolver(t *testing.T) {
	built := Build("auto", nil, cdp.New(nil), func() string { return "https://example.test/project/x" }, nil, "")

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
