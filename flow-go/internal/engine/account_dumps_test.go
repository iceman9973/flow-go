package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// A dump is named `account_<id>.json` and nothing else in the directory counts.
//
// The plain `cookies.json` is the one that matters. It holds the full jar for the
// account the engine already acts as, so treating it as a dump would register
// that account a second time — and because Register replaces in place, the second
// worker would overwrite the first with an identically-built one and leave no
// trace of the mistake.
func TestAccountDumpLabel(t *testing.T) {
	accepted := map[string]string{
		"account_0.json":           "0",
		"account_alice.json":       "alice",
		"account_a@b.example.json": "a@b.example",
		"account_1.2.3.json":       "1.2.3",
		"account_.json.json":       ".json",
	}
	for name, want := range accepted {
		got, ok := accountDumpLabel(name)
		if !ok {
			t.Errorf("accountDumpLabel(%q) rejected a dump", name)
			continue
		}
		if got != want {
			t.Errorf("accountDumpLabel(%q) = %q, want %q", name, got, want)
		}
	}

	rejected := []string{
		"cookies.json",       // the full jar, not one account's
		"account_.json",      // an empty id names no account
		"account_0.JSON",     // the extension is matched exactly
		"account_0.json.bak", // a backup is not a dump
		"account_0.txt",      // a different format
		"account_0",          // no extension at all
		"myaccount_0.json",   // the prefix is anchored, not a suffix match
		"fingerprint.json",
		"page-tokens.json",
		"bridge-token",
		"bridge-token.claimed",
	}
	for _, name := range rejected {
		if got, ok := accountDumpLabel(name); ok {
			t.Errorf("accountDumpLabel(%q) = %q, want it rejected", name, got)
		}
	}
}

// A directory with no dumps is not an error. It is the state of every setup
// driven by the extension, so the caller has to be able to carry straight on.
func TestDiscoverAccountJarsOnAMissingDirectory(t *testing.T) {
	dumps, err := discoverAccountJars(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("a missing cookie directory must not be an error: %v", err)
	}
	if len(dumps) != 0 {
		t.Errorf("dumps = %v, want none", dumps)
	}
}

// Only dumps come back, they come back in label order, and a directory that
// merely looks like a dump is skipped — the cookie directory is a real directory
// an operator can put things in.
func TestDiscoverAccountJars(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		// Everything the real cookie directory holds, none of which is a dump.
		"cookies.json", "fingerprint.json", "page-tokens.json",
		"bridge-token", "bridge-token.claimed",
		// The dumps, deliberately written out of order.
		"account_zeta.json", "account_alpha.json", "account_mid.json",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "account_dir.json"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	dumps, err := discoverAccountJars(dir)
	if err != nil {
		t.Fatalf("discoverAccountJars: %v", err)
	}

	labels := make([]string, 0, len(dumps))
	for _, d := range dumps {
		labels = append(labels, d.Label)
		if filepath.Dir(d.Path) != dir {
			t.Errorf("dump %q has path %q, want it inside %s", d.Label, d.Path, dir)
		}
		if filepath.Base(d.Path) != "account_"+d.Label+".json" {
			t.Errorf("dump %q has path %q, which does not name it", d.Label, d.Path)
		}
	}

	want := []string{"alpha", "mid", "zeta"}
	if len(labels) != len(want) {
		t.Fatalf("labels = %v, want %v — three dumps and nothing else", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Fatalf("labels = %v, want %v", labels, want)
		}
	}
}
