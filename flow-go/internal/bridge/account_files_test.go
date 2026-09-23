package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
)

// A per-account file is what lets two attached browser profiles keep their own
// cookies instead of one overwriting the other. These cover the naming, which is
// the part that can silently put two accounts on the same path.

func accountJar(sapisid string) *cookiejar.Jar {
	return cookiejar.FromCookies([]cookiejar.Cookie{
		{Domain: ".google.com", Path: "/", Name: "SAPISID", Value: sapisid},
		{Domain: ".google.com", Path: "/", Name: "SID", Value: "shared-session"},
	}, "test")
}

func TestAccountCookieFileIsNamedAfterTheAccount(t *testing.T) {
	b := NewBridge(nil, nil, t.TempDir())

	first := b.AccountCookieFile("aaaa11112222")
	second := b.AccountCookieFile("bbbb33334444")

	if first == second {
		t.Fatalf("two accounts share the path %s", first)
	}
	if base := filepath.Base(first); base != "account_aaaa11112222.json" {
		t.Errorf("base = %q, want account_aaaa11112222.json", base)
	}
	// Beside cookies.json: that is the directory the CLI reads and the one
	// discoverAccountJars scans, so a file anywhere else would never be found.
	if filepath.Dir(first) != filepath.Dir(b.cookieFile) {
		t.Errorf("%s is not in %s", first, filepath.Dir(b.cookieFile))
	}
}

// TestTwoAccountsLandInTwoFiles is the property the single shared file could not
// provide: syncing a second account must not erase the first.
func TestTwoAccountsLandInTwoFiles(t *testing.T) {
	b := NewBridge(nil, nil, t.TempDir())

	for _, sapisid := range []string{"first-account", "second-account"} {
		jar := accountJar(sapisid)
		key := cookiejar.AccountKey(jar)
		if key == "" {
			t.Fatalf("no key was derived for %s", sapisid)
		}
		if err := jar.Save(b.AccountCookieFile(key)); err != nil {
			t.Fatalf("save %s: %v", sapisid, err)
		}
	}

	entries, err := os.ReadDir(b.DataDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var accounts []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "account_") {
			accounts = append(accounts, e.Name())
		}
	}
	if len(accounts) != 2 {
		t.Fatalf("found %d account files (%v), want 2", len(accounts), accounts)
	}

	// And each holds its own account, not the one written last.
	for _, sapisid := range []string{"first-account", "second-account"} {
		key := cookiejar.AccountKey(accountJar(sapisid))
		loaded, err := cookiejar.LoadFile(b.AccountCookieFile(key))
		if err != nil {
			t.Fatalf("load %s: %v", sapisid, err)
		}
		found := false
		for _, ck := range loaded.Cookies() {
			if ck.Name == "SAPISID" && ck.Value == sapisid {
				found = true
			}
		}
		if !found {
			t.Errorf("the file for %s does not hold its own SAPISID", sapisid)
		}
	}
}

// TestResyncingAnAccountUpdatesItsFile is the other half of the naming rule: a
// rotation must not leave the account's earlier file behind as a second account.
func TestResyncingAnAccountUpdatesItsFile(t *testing.T) {
	b := NewBridge(nil, nil, t.TempDir())

	first := accountJar("steady-account")
	if err := first.Save(b.AccountCookieFile(cookiejar.AccountKey(first))); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// The same account, later, with a rotated session cookie alongside it.
	rotated := cookiejar.FromCookies([]cookiejar.Cookie{
		{Domain: ".google.com", Path: "/", Name: "SAPISID", Value: "steady-account"},
		{Domain: ".google.com", Path: "/", Name: "SID", Value: "shared-session"},
		{Domain: ".google.com", Path: "/", Name: "__Secure-1PSIDTS", Value: "rotated"},
	}, "test")
	if err := rotated.Save(b.AccountCookieFile(cookiejar.AccountKey(rotated))); err != nil {
		t.Fatalf("second save: %v", err)
	}

	entries, err := os.ReadDir(b.DataDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	accounts := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "account_") {
			accounts++
		}
	}
	if accounts != 1 {
		t.Fatalf("a re-sync produced %d account files, want 1 — the file name is not stable", accounts)
	}
}
