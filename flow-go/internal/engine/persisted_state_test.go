package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/flowapi"
	"github.com/kodelyx/flow-go/flow-go/internal/pool"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

// The browser identity and the page tokens used to be settings in SQLite — one
// engine-wide row each — and are account-bundle properties now. These pin the
// ways that can go wrong quietly:
//
//   - a value that does not survive the trip out of the file it is read from,
//   - an account's own value losing to a leftover file, which is the mismatch
//     that gets a captcha token rejected with no error at all, and
//   - a legacy file being deleted, or a settings row being written, by code that
//     is supposed to have stopped doing either.
//
// That last one is why every case below reads the settings table: the keys are
// gone from the package, so the strings are written out here, and a row
// reappearing under either name is the regression this file exists to catch.

// The two keys these were stored under. Deliberately literals rather than
// constants: there is nothing left in the package to import them from, and that
// is the point.
const (
	legacyFingerprintSetting = "browser_fingerprint"
	legacyPageTokensSetting  = "page_tokens"
)

// legacyCookieDir points the file helpers at a fresh directory, so the legacy
// files can be exercised without going anywhere near the real cookies directory.
func legacyCookieDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLOW_COOKIE_DIR", dir)
	return dir
}

// newStoredEngine is the minimum an engine needs for the persistence paths: a
// store to prove nothing is written to, and the pool every real engine has.
func newStoredEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Engine{pool: pool.New(), store: st}
}

// assertSettingUnset fails when anything has written the named setting.
func assertSettingUnset(t *testing.T, e *Engine, key string) {
	t.Helper()
	raw, found, err := e.store.Setting(key)
	if err != nil {
		t.Fatalf("reading setting %q: %v", key, err)
	}
	if found {
		t.Fatalf("setting %q was written (%q); neither of these lives in SQLite any more", key, raw)
	}
}

// bundleWith writes an account bundle holding the given identity and page
// tokens, and returns the path it landed at.
//
// A cookie goes in because a bundle with none is not read as a bundle:
// LoadBundleFile takes that shape for a legacy cookie array and returns one
// holding only the credential, which would make every case here pass for the
// wrong reason.
func bundleWith(t *testing.T, fp *cookiejar.Fingerprint, at, fsid string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account_deadbeef.json")
	bundle := &cookiejar.Bundle{
		AccountID:   "acct-1",
		Fingerprint: fp,
		At:          at,
		Fsid:        fsid,
		Cookies: []cookiejar.Cookie{
			{Name: "SAPISID", Value: "sapisid-value", Domain: ".google.com", Path: "/"},
		},
	}
	if err := bundle.Save(path); err != nil {
		t.Fatalf("writing the bundle: %v", err)
	}
	return path
}

// loadedBundle reads back what bundleWith wrote, so the cases exercise the real
// reader rather than the struct that was just built in memory.
func loadedBundle(t *testing.T, path string) *cookiejar.Bundle {
	t.Helper()
	bundle, err := cookiejar.LoadBundleFile(path)
	if err != nil {
		t.Fatalf("LoadBundleFile(%s): %v", path, err)
	}
	return bundle
}

/* ------------------------------------------------------------------ *
 * The browser identity
 * ------------------------------------------------------------------ */

func TestABundleFingerprintIsAdoptedAndNothingIsStored(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	bundle := loadedBundle(t, bundleWith(t, &cookiejar.Fingerprint{
		UserAgent: "UA/bundle",
		Language:  "en-GB",
		SecChUa:   `"Chromium";v="153"`,
		Platform:  `"macOS"`,
		Mobile:    "?0",
	}, "", ""))

	got, source := persistedFingerprint(bundle)
	if got == nil {
		t.Fatal("persistedFingerprint returned nil for a bundle that carries one")
	}
	if got.UserAgent != "UA/bundle" || got.Language != "en-GB" ||
		got.SecChUa != `"Chromium";v="153"` || got.Platform != `"macOS"` || got.Mobile != "?0" {
		t.Fatalf("the bundle's identity did not survive the trip: %+v", got)
	}
	if source == "" {
		t.Fatal("no source was reported, so a log line cannot say where the identity came from")
	}
	if source == fingerprintFile() {
		t.Fatalf("source = %q, want the account's own file, not the legacy one", source)
	}

	assertSettingUnset(t, e, legacyFingerprintSetting)
}

func TestALegacyFingerprintFileIsAdoptedAndLeftInPlace(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	// The shape the old code wrote. The type has no json tags, so these are the
	// Go field names — exactly what an existing install has on disk.
	legacy := []byte(`{"UserAgent":"UA/legacy","Language":"en-US","Platform":"\"Windows\""}`)
	if err := os.WriteFile(fingerprintFile(), legacy, 0o600); err != nil {
		t.Fatalf("seed the legacy file: %v", err)
	}

	got, source := persistedFingerprint(nil)
	if got == nil || got.UserAgent != "UA/legacy" || got.Language != "en-US" {
		t.Fatalf("the legacy file was not adopted: %+v", got)
	}
	if source != fingerprintFile() {
		t.Fatalf("source = %q, want %q", source, fingerprintFile())
	}

	// Read, not consumed. Nothing has replaced it yet, so deleting it would be
	// the one way to lose a value the next run still needs — which is why the
	// old migration deleted the file only once the database held the value, and
	// why nothing deletes it now that there is no database to put it in.
	if _, err := os.Stat(fingerprintFile()); err != nil {
		t.Fatalf("the legacy file must be left in place: %v", err)
	}
	assertSettingUnset(t, e, legacyFingerprintSetting)
}

func TestABundleFingerprintWinsOverALegacyFile(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	// A leftover from before the move, belonging to whichever profile last ran
	// the old code — possibly a different signed-in account entirely.
	if err := os.WriteFile(fingerprintFile(), []byte(`{"UserAgent":"UA/legacy"}`), 0o600); err != nil {
		t.Fatalf("seed the legacy file: %v", err)
	}
	bundle := &cookiejar.Bundle{Fingerprint: &cookiejar.Fingerprint{UserAgent: "UA/bundle"}}

	got, source := persistedFingerprint(bundle)
	if got == nil || got.UserAgent != "UA/bundle" {
		t.Fatalf("the account's own identity must win over a leftover file: %+v", got)
	}
	if source == fingerprintFile() {
		t.Fatalf("source = %q, want the account's own file", source)
	}

	assertSettingUnset(t, e, legacyFingerprintSetting)
}

func TestNoFingerprintAnywhereIsNil(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	got, source := persistedFingerprint(nil)
	if got != nil || source != "" {
		t.Fatalf("nothing is recorded, so nothing may be reported: %+v from %q", got, source)
	}

	assertSettingUnset(t, e, legacyFingerprintSetting)
}

/* ------------------------------------------------------------------ *
 * The page tokens
 * ------------------------------------------------------------------ */

func TestABundlePageTokenPairIsAdoptedAndNothingIsStored(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	bundle := loadedBundle(t, bundleWith(t, nil, "at/bundle", "-1234"))

	got, source := persistedPageTokens(bundle)
	if got.At != "at/bundle" || got.Fsid != "-1234" {
		t.Fatalf("the bundle's pair did not survive the trip: %+v", got)
	}
	if source == "" {
		t.Fatal("no source was reported, so a log line cannot say where the pair came from")
	}
	if source == pageTokensFile() {
		t.Fatalf("source = %q, want the account's own file, not the legacy one", source)
	}

	assertSettingUnset(t, e, legacyPageTokensSetting)
}

func TestALegacyPageTokensFileIsAdoptedAndLeftInPlace(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	legacy := []byte(`{"at":"at/legacy","fsid":"-9876"}`)
	if err := os.WriteFile(pageTokensFile(), legacy, 0o600); err != nil {
		t.Fatalf("seed the legacy file: %v", err)
	}

	got, source := persistedPageTokens(nil)
	if got.At != "at/legacy" || got.Fsid != "-9876" {
		t.Fatalf("the legacy file was not adopted: %+v", got)
	}
	if source != pageTokensFile() {
		t.Fatalf("source = %q, want %q", source, pageTokensFile())
	}

	if _, err := os.Stat(pageTokensFile()); err != nil {
		t.Fatalf("the legacy file must be left in place: %v", err)
	}
	assertSettingUnset(t, e, legacyPageTokensSetting)
}

func TestABundlePageTokenPairWinsOverALegacyFile(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	if err := os.WriteFile(pageTokensFile(), []byte(`{"at":"at/legacy","fsid":"-1"}`), 0o600); err != nil {
		t.Fatalf("seed the legacy file: %v", err)
	}
	bundle := &cookiejar.Bundle{At: "at/bundle", Fsid: "-2"}

	got, source := persistedPageTokens(bundle)
	if got.At != "at/bundle" || got.Fsid != "-2" {
		t.Fatalf("the account's own pair must win over a leftover file: %+v", got)
	}
	if source == pageTokensFile() {
		t.Fatalf("source = %q, want the account's own file", source)
	}

	assertSettingUnset(t, e, legacyPageTokensSetting)
}

func TestAnEmptyPairIsNotReportedAsASource(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	// Every account registered from a live extension sync has this shape until
	// its page has been read. Reporting it as a source would have the caller log
	// that it adopted a pair it never got.
	got, source := persistedPageTokens(&cookiejar.Bundle{At: "", Fsid: ""})
	if got.At != "" || got.Fsid != "" || source != "" {
		t.Fatalf("an empty pair must not be reported as a source: %+v from %q", got, source)
	}

	// Half a pair is still a pair: the anti-CSRF token and the session id come
	// from the same page but either can be absent, and dropping the one that is
	// there is worse than carrying it alone.
	half, halfSource := persistedPageTokens(&cookiejar.Bundle{At: "at-only"})
	if half.At != "at-only" || half.Fsid != "" || halfSource == "" {
		t.Fatalf("a pair with one side missing must still be adopted: %+v from %q", half, halfSource)
	}

	assertSettingUnset(t, e, legacyPageTokensSetting)
}

/* ------------------------------------------------------------------ *
 * The wiring, not just the lookup
 * ------------------------------------------------------------------ */

// The priority order is: an explicit pair for this run, then the one recorded for
// this account, then the live page.
//
// The middle step is the fix, and it is the step that cannot be driven here.
// `bridge.Current()` answers with whichever profile attached *last*, so with two
// extensions connected the recorded pair — which belongs to the right account —
// has to beat the live one, which belongs to the wrong account. Proving that
// needs a *connected* bridge, and a `*cdp.Client` cannot be built without a real
// WebSocket handshake: `Bridge.Listen` is the only way a client ever appears, and
// it is not injectable. Note that the ordering is also invisible with a
// *disconnected* bridge — both orders reach the recorded pair by the same route —
// which is exactly why this gap cannot be closed by a cleverer fixture.
//
// What is asserted is the rest of the order, and the wiring: that the bundle
// Bootstrap just loaded is what pageTokens reads at all.
func TestPageTokensPriorityOrder(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	bundle := &cookiejar.Bundle{At: "at/bundle", Fsid: "-2"}

	// An explicit pair for this run wins outright. That is the ranking
	// chooseProjectID applies to a project, and it is what keeps the CLI's
	// --session path working.
	e.opts.AtToken = "at/explicit"
	e.opts.Fsid = "-9"
	if at, fsid := e.pageTokens(context.Background(), bundle); at != "at/explicit" || fsid != "-9" {
		t.Fatalf("pageTokens = (%q, %q), want the explicit pair", at, fsid)
	}

	// Without one, the account's own file. This is the assertion that fails if
	// Bootstrap ever stops handing pageTokens the bundle it just loaded.
	e.opts.AtToken = ""
	e.opts.Fsid = ""
	if at, fsid := e.pageTokens(context.Background(), bundle); at != "at/bundle" || fsid != "-2" {
		t.Fatalf("pageTokens = (%q, %q), want the pair recorded for this account", at, fsid)
	}

	// And with neither, nothing — rather than another account's pair, which is
	// the failure a single engine-wide settings row made possible.
	if at, fsid := e.pageTokens(context.Background(), nil); at != "" || fsid != "" {
		t.Fatalf("pageTokens = (%q, %q), want nothing when nothing is recorded", at, fsid)
	}

	assertSettingUnset(t, e, legacyPageTokensSetting)
}

// The fingerprint follows the same order, and carries the same untestable middle
// step — see TestPageTokensPriorityOrder for why. What is worth pinning here is
// that the recorded identity reaches the caller at all, since a captcha token
// minted under one client and spent under another is rejected with no error
// anywhere: it comes back as an empty result.
func TestBrowserFingerprintPriorityOrder(t *testing.T) {
	legacyCookieDir(t)
	e := newStoredEngine(t)

	bundle := &cookiejar.Bundle{Fingerprint: &cookiejar.Fingerprint{
		UserAgent: "UA/bundle", Language: "en-GB", Platform: `"macOS"`,
	}}

	// An explicit identity for this run wins.
	e.opts.Fingerprint = &flowapi.BrowserFingerprint{UserAgent: "UA/explicit"}
	if got := e.browserFingerprint(context.Background(), bundle); got == nil || got.UserAgent != "UA/explicit" {
		t.Fatalf("browserFingerprint = %+v, want the explicit identity", got)
	}

	// Without one, the account's own file, field for field — a partial adoption
	// is the mismatch this whole path exists to avoid.
	e.opts.Fingerprint = nil
	got := e.browserFingerprint(context.Background(), bundle)
	if got == nil {
		t.Fatal("browserFingerprint returned nil for a bundle that carries an identity")
	}
	if got.UserAgent != "UA/bundle" || got.Language != "en-GB" || got.Platform != `"macOS"` {
		t.Fatalf("the recorded identity did not survive the trip: %+v", got)
	}

	// And with neither, nil rather than the pinned default, so the caller can
	// say so rather than presenting a machine that is not this one.
	if got := e.browserFingerprint(context.Background(), nil); got != nil {
		t.Fatalf("browserFingerprint = %+v, want nil when nothing is recorded", got)
	}
}
