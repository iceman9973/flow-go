package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/flowapi"
	"github.com/kodelyx/flow-go/flow-go/internal/pool"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

// The browser identity and the page tokens used to be files in the cookies
// directory and are settings now. These pin the two ways that can go wrong
// quietly: a value that does not survive the round trip, and a legacy file that
// is deleted before it has been read.

// legacyCookieDir points the file helpers at a fresh directory, so the migration
// can be exercised without going anywhere near the real cookies directory.
func legacyCookieDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLOW_COOKIE_DIR", dir)
	return dir
}

// newStoredEngine is the minimum an engine needs for the persistence paths: a
// store to write to, and the pool every real engine has.
func newStoredEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Engine{pool: pool.New(), store: st}
}

func TestFingerprintRoundTripsThroughTheStore(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	want := &flowapi.BrowserFingerprint{
		UserAgent: "UA/round-trip",
		Language:  "en-GB",
		SecChUa:   `"Chromium";v="153"`,
		Platform:  `"macOS"`,
		Mobile:    "?0",
	}
	e.saveFingerprint(want)

	got := e.loadFingerprint()
	if got == nil {
		t.Fatal("loadFingerprint returned nil after saveFingerprint")
	}
	if *got != *want {
		t.Fatalf("round trip lost fields:\n got %+v\nwant %+v", got, want)
	}

	// A store-backed save must not leave a file behind — the whole point of the
	// move is that `cookies/` holds cookies and nothing else.
	if _, err := os.Stat(fingerprintFile()); !os.IsNotExist(err) {
		t.Fatalf("saveFingerprint wrote %s", fingerprintFile())
	}
}

func TestALegacyFingerprintFileIsMigratedAndRemoved(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	// The shape the old code wrote. The type has no json tags, so these are the
	// Go field names — exactly what an existing install has on disk.
	legacy := []byte(`{"UserAgent":"UA/legacy","Language":"en-US","Platform":"\"Windows\""}`)
	if err := os.WriteFile(fingerprintFile(), legacy, 0o600); err != nil {
		t.Fatalf("seed the legacy file: %v", err)
	}

	got := e.loadFingerprint()
	if got == nil || got.UserAgent != "UA/legacy" || got.Language != "en-US" {
		t.Fatalf("the legacy file was not adopted: %+v", got)
	}

	raw, found, err := e.store.Setting(store.SettingKeyBrowserFingerprint)
	if err != nil || !found {
		t.Fatalf("the fingerprint did not reach the store (found=%v err=%v)", found, err)
	}
	var stored flowapi.BrowserFingerprint
	if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored.UserAgent != "UA/legacy" {
		t.Fatalf("what reached the store is not what was in the file: %q", raw)
	}

	if _, err := os.Stat(fingerprintFile()); !os.IsNotExist(err) {
		t.Fatalf("the migrated file was left behind at %s", fingerprintFile())
	}
}

func TestAStoredFingerprintWinsOverALegacyFile(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	e.saveFingerprint(&flowapi.BrowserFingerprint{UserAgent: "UA/stored"})
	if err := os.WriteFile(fingerprintFile(), []byte(`{"UserAgent":"UA/legacy"}`), 0o600); err != nil {
		t.Fatalf("seed the legacy file: %v", err)
	}

	got := e.loadFingerprint()
	if got == nil || got.UserAgent != "UA/stored" {
		t.Fatalf("the store must win over a leftover file: %+v", got)
	}

	// Nothing was migrated, so nothing may be removed — deleting a file that was
	// never read would lose whatever is in it.
	if _, err := os.Stat(fingerprintFile()); err != nil {
		t.Fatalf("a file that was not migrated must be left alone: %v", err)
	}
}

func TestPageTokensRoundTripThroughTheStore(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	e.savePageTokens(pageTokenSet{At: "at-value", Fsid: "-1234"})

	got := e.loadPageTokens()
	if got.At != "at-value" || got.Fsid != "-1234" {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if _, err := os.Stat(pageTokensFile()); !os.IsNotExist(err) {
		t.Fatalf("savePageTokens wrote %s", pageTokensFile())
	}
}

func TestAnEmptyPageTokenPairIsNotPersisted(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	e.savePageTokens(pageTokenSet{At: "at-value", Fsid: "-1234"})
	e.savePageTokens(pageTokenSet{})

	got := e.loadPageTokens()
	if got.At != "at-value" || got.Fsid != "-1234" {
		t.Fatalf("an empty pair overwrote a stored one: %+v", got)
	}
}

func TestALegacyPageTokensFileIsMigratedAndRemoved(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	legacy := []byte(`{"at":"at/legacy","fsid":"-9876"}`)
	if err := os.WriteFile(pageTokensFile(), legacy, 0o600); err != nil {
		t.Fatalf("seed the legacy file: %v", err)
	}

	got := e.loadPageTokens()
	if got.At != "at/legacy" || got.Fsid != "-9876" {
		t.Fatalf("the legacy file was not adopted: %+v", got)
	}

	raw, found, err := e.store.Setting(store.SettingKeyPageTokens)
	if err != nil || !found {
		t.Fatalf("the page tokens did not reach the store (found=%v err=%v)", found, err)
	}
	if raw != string(legacy) {
		t.Fatalf("the stored value is not the file's own bytes: %q", raw)
	}

	if _, err := os.Stat(pageTokensFile()); !os.IsNotExist(err) {
		t.Fatalf("the migrated file was left behind at %s", pageTokensFile())
	}
}
