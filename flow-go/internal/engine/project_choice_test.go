package engine

import "testing"

// The ranking exists because the sources are not interchangeable: two are
// instructions, one is a remembered answer, and only one of the discovery routes
// drives a browser.

func TestChooseProjectIDRanksExplicitOverEverything(t *testing.T) {
	rpcAsked, browserAsked := false, false

	id, source := chooseProjectID("explicit-id", "bundle-id", "configured-id",
		func() string { rpcAsked = true; return "from-rpc" },
		func() string { browserAsked = true; return "from-browser" })

	if id != "explicit-id" {
		t.Fatalf("id = %q, want the explicit one", id)
	}
	if source != projectSourceExplicit {
		t.Fatalf("source = %q, want %q", source, projectSourceExplicit)
	}
	if rpcAsked || browserAsked {
		t.Fatal("a source was consulted even though the caller named a project")
	}
}

// The point of FLOW_PROJECT_ID: a configured project means nothing is searched
// for, so no tab is navigated.
//
// A bundle is passed here deliberately. It is a remembered value, and a
// remembered value goes stale — a project belongs to one signed-in account, and
// one from another account does not open — so an operator's instruction for this
// run has to outrank a leftover from the last one.
func TestChooseProjectIDSkipsEverySearchWhenConfigured(t *testing.T) {
	rpcAsked, browserAsked := false, false

	id, source := chooseProjectID("", "bundle-id", "configured-id",
		func() string { rpcAsked = true; return "from-rpc" },
		func() string { browserAsked = true; return "from-browser" })

	if id != "configured-id" {
		t.Fatalf("id = %q, want the configured one", id)
	}
	if source != projectSourceConfigured {
		t.Fatalf("source = %q, want %q", source, projectSourceConfigured)
	}
	if rpcAsked || browserAsked {
		t.Fatal("a source was consulted despite a configured project id")
	}
}

// TestChooseProjectIDUsesTheBundleWithoutAsking is what the bundle is for: a
// project the account file already recorded should cost neither a round trip nor
// a tab navigation.
func TestChooseProjectIDUsesTheBundleWithoutAsking(t *testing.T) {
	rpcAsked, browserAsked := false, false

	id, source := chooseProjectID("", "bundle-id", "",
		func() string { rpcAsked = true; return "from-rpc" },
		func() string { browserAsked = true; return "from-browser" })

	if id != "bundle-id" {
		t.Fatalf("id = %q, want the one the file recorded", id)
	}
	if source != projectSourceBundle {
		t.Fatalf("source = %q, want %q", source, projectSourceBundle)
	}
	if rpcAsked || browserAsked {
		t.Fatal("a source was consulted even though the account file answered")
	}
}

// The listing is live truth and costs an HTTP call, so it is preferred over the
// navigation — and the browser must not be reached once it has answered.
func TestChooseProjectIDPrefersTheListingOverTheBrowser(t *testing.T) {
	browserAsked := false

	id, source := chooseProjectID("", "", "",
		func() string { return "from-rpc" },
		func() string { browserAsked = true; return "from-browser" })

	if id != "from-rpc" {
		t.Fatalf("id = %q, want the listing's answer", id)
	}
	if source != projectSourceRPC {
		t.Fatalf("source = %q, want %q", source, projectSourceRPC)
	}
	if browserAsked {
		t.Fatal("the browser was navigated even though the listing answered")
	}
}

// A rejected listing still has to fall through, or a transport problem would
// take the browser route away with it.
func TestChooseProjectIDFallsBackToTheBrowserWhenTheListingFails(t *testing.T) {
	id, source := chooseProjectID("", "", "",
		func() string { return "" },
		func() string { return "from-browser" })

	if id != "from-browser" {
		t.Fatalf("id = %q, want the browser's answer", id)
	}
	if source != projectSourceBrowser {
		t.Fatalf("source = %q, want %q", source, projectSourceBrowser)
	}
}

func TestChooseProjectIDReportsNothingWhenEverySourceIsEmpty(t *testing.T) {
	for name, ask := range map[string]func() string{
		"no browser at all": nil,
		"browser said no":   func() string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			id, source := chooseProjectID("", "", "", func() string { return "" }, ask)

			if id != "" {
				t.Fatalf("id = %q, want empty", id)
			}
			if source != projectSourceNone {
				t.Fatalf("source = %q, want %q", source, projectSourceNone)
			}
		})
	}
}
