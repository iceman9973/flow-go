package engine

import "testing"

// The ranking exists because the three sources are not interchangeable: two are
// instructions and one is discovery, and the discovery step costs a navigation.
func TestChooseProjectIDRanksExplicitOverEverything(t *testing.T) {
	asked := false
	ask := func() string {
		asked = true
		return "from-browser"
	}

	id, source := chooseProjectID("explicit-id", "configured-id", ask)

	if id != "explicit-id" {
		t.Fatalf("id = %q, want the explicit one", id)
	}
	if source != projectSourceExplicit {
		t.Fatalf("source = %q, want %q", source, projectSourceExplicit)
	}
	if asked {
		t.Fatal("the browser was asked even though the caller named a project")
	}
}

// The point of FLOW_PROJECT_ID: a configured project means the engine never has
// to open an editor tab to learn where it is.
func TestChooseProjectIDSkipsTheBrowserWhenConfigured(t *testing.T) {
	asked := false
	ask := func() string {
		asked = true
		return "from-browser"
	}

	id, source := chooseProjectID("", "configured-id", ask)

	if id != "configured-id" {
		t.Fatalf("id = %q, want the configured one", id)
	}
	if source != projectSourceConfigured {
		t.Fatalf("source = %q, want %q", source, projectSourceConfigured)
	}
	if asked {
		t.Fatal("the browser was asked despite a configured project id")
	}
}

// With nothing configured, discovery is the only way left.
func TestChooseProjectIDFallsBackToTheBrowser(t *testing.T) {
	id, source := chooseProjectID("", "", func() string { return "from-browser" })

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
			id, source := chooseProjectID("", "", ask)

			if id != "" {
				t.Fatalf("id = %q, want empty", id)
			}
			if source != projectSourceNone {
				t.Fatalf("source = %q, want %q", source, projectSourceNone)
			}
		})
	}
}
