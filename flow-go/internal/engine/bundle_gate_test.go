package engine

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/bridge"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
)

// Only an account that can actually generate gets registered.
//
// A bundle that loads but is missing a credential, the page tokens or the
// browser identity produces failures that are *all* silent — an unauthenticated
// call, a batchexecute request that primes for a token the server never hands
// over, a captcha token spent under a client it was not minted for. The gate is
// therefore the only place an operator is ever told why an account is missing,
// which is why the log line is asserted and not just the outcome.
//
// What is covered here, and what is not: `Bundle.Complete` itself is exercised
// exhaustively in internal/cookiejar/bundle_test.go, and the skip path of
// `registerAccountDumps` is driven end to end below. The *accept* path of that
// function, and `registerConnectedExtensions` in full, are not driven — both go
// through `resolveAccount`, which mints a Labs session over the real network
// (`config.LabsBase` is a const, so it cannot be pointed at a test server), and
// a unit test that calls Google is worse than no unit test. That is a real gap
// rather than an oversight.

// captureLogs redirects the standard logger for the duration of a test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	original := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(original) })
	return &buf
}

/* ------------------------------------------------------------------ *
 * The dump gate
 * ------------------------------------------------------------------ */

// A file holding a credential and nothing else — no `at`/`f.sid`, no identity —
// is the shape a sync leaves behind when no Flow tab was open, and the shape
// every bare cookie array has. It is skipped, and the log says why.
//
// The assertion that makes this non-vacuous is the pool size, not the log line.
// `pool.Register` is reachable only through `resolveAccount`, so a dropped or
// inverted gate puts a worker in the pool and fails here. It is also why this
// test touches no network while it passes: the gate returns before a session is
// ever minted. Under a mutation it will reach for Google and take however long
// that takes — the failing case, which does not matter.
func TestAnIncompleteDumpIsNotRegistered(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_COOKIE_DIR", dir)

	// A credential and no page tokens: exactly what a sync writes for a profile
	// whose Flow tab was never opened.
	bundle := &cookiejar.Bundle{
		AccountID: "acct-incomplete",
		Cookies:   identityJar("sapisid-value", "rotating-value").Cookies(),
	}
	if err := bundle.Save(filepath.Join(dir, "account_incomplete.json")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	e := newStoredEngine(t)
	logs := captureLogs(t)

	e.registerAccountDumps(context.Background(), nil, nil)

	if e.pool.Size() != 0 {
		t.Fatalf("pool holds %d workers; an incomplete dump must not be registered", e.pool.Size())
	}
	want := "skipping account account_incomplete.json: incomplete bundle"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("the skip was not explained.\nwant a line containing %q\ngot:\n%s", want, logs.String())
	}
}

// An empty dump is named as empty rather than as incomplete, because the two are
// different mistakes: a half-written file is not a sync that had no Flow tab.
//
// The fixture looks odd and is deliberate. A file with no cookies in it cannot be
// loaded at all — `LoadFile` rejects an empty array, an empty header and a
// missing key alike with "unsupported cookie format" — so the only route to a
// jar with a count of zero is a header string with no `=` anywhere in it, which
// is what a truncated legacy dump looks like. That makes this branch narrow, and
// it is worth knowing it is narrow rather than dead: `Complete()` would reject
// such a file too, so the guard is redundant with it, but this is the one that
// names the actual problem.
func TestAnEmptyDumpIsReportedAsEmptyRatherThanIncomplete(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_COOKIE_DIR", dir)

	if err := os.WriteFile(filepath.Join(dir, "account_empty.json"),
		[]byte(`{"cookies":"novalue"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	e := newStoredEngine(t)
	logs := captureLogs(t)

	e.registerAccountDumps(context.Background(), nil, nil)

	if e.pool.Size() != 0 {
		t.Fatalf("pool holds %d workers; an empty dump must not be registered", e.pool.Size())
	}
	if got := logs.String(); !strings.Contains(got, "it holds no cookies") {
		t.Errorf("an empty dump should be named as empty, not as incomplete:\n%s", got)
	}
}

/* ------------------------------------------------------------------ *
 * Cookie isolation
 * ------------------------------------------------------------------ */

// The engine generates as the account it bootstrapped as, not as whichever
// extension synced last.
//
// That is the bug this accessor exists for: the bridge holds one jar and
// `SyncCookies` overwrites it once per attached profile, so after a boot that
// registered several, `bridge.Jar()` answers with the last one's cookies. A
// generation submitted as the primary account would then go out under a
// different profile's session, which upstream reads as unusual activity rather
// than as a mismatched identity — so it presents as an intermittent failure with
// nothing in the log to point at it.
func TestJarPrefersTheEnginesOwnOverTheBridges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_COOKIE_DIR", dir)

	// The bridge's jar: the freshest account file in its data directory, which
	// stands in for "some other profile synced last".
	other := &cookiejar.Bundle{
		AccountID: "acct-other",
		Cookies:   identityJar("other-sapisid", "other-rotating").Cookies(),
	}
	if err := other.Save(filepath.Join(dir, "account_other.json")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	br := bridge.NewBridge(nil, nil, dir)
	if br.Jar() == nil {
		t.Fatal("the bridge should have found the persisted account file")
	}

	mine := identityJar("mine-sapisid", "mine-rotating")
	e := &Engine{bridge: br, primaryJar: mine}

	got := e.Jar()
	if got != mine {
		t.Fatalf("Jar() did not return the engine's own jar")
	}
	if got == br.Jar() {
		t.Fatal("Jar() handed back the bridge's jar, which belongs to another account")
	}
}

// Before Bootstrap has captured one there is nothing of the engine's own, and
// the bridge is what is left. It is a fallback, not a source: the point of the
// test above is that it stops being consulted once a boot has happened.
func TestJarFallsBackToTheBridgeBeforeBoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_COOKIE_DIR", dir)

	bundle := &cookiejar.Bundle{
		AccountID: "acct-other",
		Cookies:   identityJar("other-sapisid", "other-rotating").Cookies(),
	}
	if err := bundle.Save(filepath.Join(dir, "account_other.json")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	e := &Engine{bridge: bridge.NewBridge(nil, nil, dir)}
	if e.Jar() == nil {
		t.Fatal("with no jar of its own yet, Jar() must fall back to the bridge")
	}
}

// No bridge and no boot is no cookies, not a nil dereference.
func TestJarWithNoBridgeAndNoBootIsNil(t *testing.T) {
	e := &Engine{}
	if e.Jar() != nil {
		t.Error("Jar() = non-nil with neither a bridge nor a captured jar")
	}
}
