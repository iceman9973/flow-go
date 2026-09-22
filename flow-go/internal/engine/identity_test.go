package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/auth"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/pool"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

// identityJar builds a jar shaped like a signed-in Google profile.
//
// `rotating` seeds exactly the cookies that are expected to rotate — the
// per-service sessions and the timestamp cookies — so two jars with the same
// sapisid and different rotating values model one account before and after a
// rotation. That is the situation that produced six account rows in under two
// hours on a real machine.
func identityJar(sapisid, rotating string) *cookiejar.Jar {
	return identityJarVarying(sapisid, rotating, "psid-1")
}

// identityJarVarying additionally varies __Secure-1PSID, which is *inside* the
// anchor set. It models a core cookie moving while the credential does not,
// which is the case the re-anchor rule exists for — the anchor changes, so the
// plain lookup misses and continuity has to be proven another way.
func identityJarVarying(sapisid, rotating, securePsid string) *cookiejar.Jar {
	return cookiejar.FromCookies([]cookiejar.Cookie{
		{Name: "SID", Value: "sid-value", Domain: ".google.com", Path: "/"},
		{Name: "HSID", Value: "hsid-value", Domain: ".google.com", Path: "/"},
		{Name: "SSID", Value: "ssid-value", Domain: ".google.com", Path: "/"},
		{Name: "APISID", Value: "apisid-value", Domain: ".google.com", Path: "/"},
		{Name: "SAPISID", Value: sapisid, Domain: ".google.com", Path: "/"},
		{Name: "__Secure-1PSID", Value: securePsid, Domain: ".google.com", Path: "/"},
		{Name: "__Secure-3PSID", Value: "psid-3", Domain: ".google.com", Path: "/"},
		{Name: "OSID", Value: "osid-" + rotating, Domain: "flow.google.com", Path: "/"},
		{Name: "__Secure-OSID", Value: "osid-secure-" + rotating, Domain: "flow.google.com", Path: "/"},
		{Name: "__Secure-1PSIDTS", Value: "ts1-" + rotating, Domain: ".google.com", Path: "/"},
		{Name: "__Secure-3PSIDTS", Value: "ts3-" + rotating, Domain: ".google.com", Path: "/"},
		{Name: "LSID", Value: "lsid-" + rotating, Domain: "accounts.google.com", Path: "/"},
	}, "test")
}

func identityEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// A pool, because a real engine always has one — New() builds it — and a
	// helper that omits it is not modelling the thing under test. Losing the
	// session drains the pool, so an engine without one is a state that cannot
	// occur and should not be what the tests exercise.
	return &Engine{store: st, pool: pool.New(), opts: opts}
}

// TestAnchorIgnoresTheRotatingCookies is the whole point of the change.
//
// The fixture asserts that the two jars really are different — the old identity
// was a hash of the entire jar, so it would have produced two accounts here. The
// anchor must not.
func TestAnchorIgnoresTheRotatingCookies(t *testing.T) {
	before := identityJar("sapisid-A", "before")
	after := identityJar("sapisid-A", "after")

	if before.Hash() == after.Hash() {
		t.Fatal("the fixture is wrong: the two jars must differ, or this proves nothing")
	}

	keyBefore, idBefore, _ := identityAnchor(before, nil)
	keyAfter, idAfter, _ := identityAnchor(after, nil)

	if keyBefore != keyAfter {
		t.Errorf("anchor moved across a rotation of the excluded cookies: %q -> %q",
			keyBefore, keyAfter)
	}
	if idBefore != idAfter {
		t.Errorf("label moved across a rotation: %q -> %q", idBefore, idAfter)
	}
}

// TestAnchorFollowsTheCredential: a different SAPISID is a different session, so
// it must produce a different anchor. Without this the anchor would be stable
// for the wrong reason — by ignoring everything.
func TestAnchorFollowsTheCredential(t *testing.T) {
	a, _, _ := identityAnchor(identityJar("sapisid-A", "x"), nil)
	b, _, _ := identityAnchor(identityJar("sapisid-B", "x"), nil)

	if a == b {
		t.Error("two different SAPISID values produced the same anchor")
	}
}

// TestIdentityMatchesWhenTheRotatingCookiesMove is the reported defect end to
// end: the second boot sees different cookies and must land on the same account
// rather than writing a row beside it.
//
// This is the anchor doing the work — no re-anchor is needed, because the
// cookies that moved are outside the anchor set. The two tests below this one
// cover the harder case where a core cookie moves too.
func TestIdentityMatchesWhenTheRotatingCookiesMove(t *testing.T) {
	e := identityEngine(t, Options{})

	first := identityJar("sapisid-A", "before")
	original := e.resolveIdentity(first, nil, 0)
	e.recordIdentity(original, first, "", 0)

	rotated := identityJar("sapisid-A", "after")
	again := e.resolveIdentity(rotated, nil, 0)

	if again.AccountID != original.AccountID {
		t.Errorf("account id changed across a cookie rotation: %q -> %q — this is the churn",
			original.AccountID, again.AccountID)
	}
	if again.Key != original.Key {
		t.Errorf("anchor moved across a rotation of the excluded cookies: %q -> %q",
			original.Key, again.Key)
	}

	accounts, err := e.store.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Errorf("the rotation produced %d account rows, want 1", len(accounts))
	}
}

// TestIdentityReAnchorsWhenACoreCookieMoves covers the case the anchor cannot
// handle on its own: a cookie *inside* the anchor set rotated, so the lookup
// misses. SAPISID is unchanged, which proves the session is the same, so the
// existing row is re-anchored instead of a duplicate being minted.
func TestIdentityReAnchorsWhenACoreCookieMoves(t *testing.T) {
	e := identityEngine(t, Options{})

	first := identityJarVarying("sapisid-A", "x", "psid-before")
	original := e.resolveIdentity(first, nil, 0)
	e.recordIdentity(original, first, "", 0)

	rotated := identityJarVarying("sapisid-A", "x", "psid-after")
	if rotated.Hash() == first.Hash() {
		t.Fatal("the fixture is wrong: the jars must differ")
	}

	again := e.resolveIdentity(rotated, nil, 0)

	if again.AccountID != original.AccountID {
		t.Errorf("account id = %q, want %q — the re-anchor rule did not fire",
			again.AccountID, original.AccountID)
	}
	if again.Source != identitySourceReAnchored {
		t.Errorf("source = %q, want %q", again.Source, identitySourceReAnchored)
	}
	if again.Key == original.Key {
		t.Error("the anchor did not move, so this test is not exercising the re-anchor path")
	}

	accounts, err := e.store.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Errorf("the rotation produced %d account rows, want 1", len(accounts))
	}

	// The anchor that was left behind must still be on record, so the move is
	// auditable rather than silent.
	if accounts[0].IdentityKey != again.Key {
		t.Errorf("stored identity_key = %q, want %q", accounts[0].IdentityKey, again.Key)
	}
}

// TestIdentityMintsWhenTheCredentialChanged is the other half of the rule: a
// genuinely different account must not be absorbed into an existing one. A row
// with a different SAPISID is not a candidate, so this mints.
func TestIdentityMintsWhenTheCredentialChanged(t *testing.T) {
	e := identityEngine(t, Options{})

	first := identityJar("sapisid-A", "before")
	original := e.resolveIdentity(first, nil, 0)
	e.recordIdentity(original, first, "", 0)

	other := identityJar("sapisid-B", "before")
	switched := e.resolveIdentity(other, nil, 0)

	if switched.AccountID == original.AccountID {
		t.Errorf("a different SAPISID was merged into %q — two accounts became one",
			original.AccountID)
	}
	if switched.Source != identitySourceCookieCore {
		t.Errorf("source = %q, want %q — this is a new identity, not a rotation",
			switched.Source, identitySourceCookieCore)
	}
}

// TestIdentityAdoptsALegacyRowOnce covers the upgrade path. A row written before
// fingerprints existed carries no evidence either way, so it is adopted — and
// labelled as an adoption, so nobody later reads it as proof.
func TestIdentityAdoptsALegacyRowOnce(t *testing.T) {
	e := identityEngine(t, Options{})

	// A row as an older version would have left it: a legacy anchor, no
	// fingerprint, index 0.
	index := 0
	if err := e.store.UpsertAccount(store.Account{
		AccountID:    "acct-legacy",
		CookieHash:   "deadbeef",
		Status:       "active",
		IdentityKey:  "legacy:deadbeef",
		LastAuthuser: &index,
	}); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	jar := identityJar("sapisid-A", "now")
	got := e.resolveIdentity(jar, nil, 0)

	if got.AccountID != "acct-legacy" {
		t.Errorf("account id = %q, want the legacy row to be adopted", got.AccountID)
	}
	if got.Source != identitySourceAdoptedLegacy {
		t.Errorf("source = %q, want %q", got.Source, identitySourceAdoptedLegacy)
	}

	// Adopting again from the same jar must be an ordinary match, not a second
	// adoption.
	e.recordIdentity(got, jar, "", 0)
	second := e.resolveIdentity(jar, nil, 0)
	if second.AccountID != "acct-legacy" {
		t.Errorf("second resolution produced %q, want the adopted row", second.AccountID)
	}
	if second.Source == identitySourceAdoptedLegacy {
		t.Error("the row was adopted twice; the second pass should be a plain match")
	}
}

// TestIdentityIsStableAcrossRepeatedBoots is the plain case: nothing changed, so
// nothing should.
func TestIdentityIsStableAcrossRepeatedBoots(t *testing.T) {
	e := identityEngine(t, Options{})
	jar := identityJar("sapisid-A", "same")

	first := e.resolveIdentity(jar, nil, 0)
	e.recordIdentity(first, jar, "", 0)

	for i := 0; i < 3; i++ {
		next := e.resolveIdentity(jar, nil, 0)
		if next.AccountID != first.AccountID {
			t.Fatalf("boot %d produced %q, want %q", i, next.AccountID, first.AccountID)
		}
	}

	accounts, err := e.store.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Errorf("repeated boots produced %d rows, want 1", len(accounts))
	}
}

// TestIdentityPrefersAnExplicitOverride: a supplied id is a decision, not an
// inference, and must win over anything derived.
func TestIdentityPrefersAnExplicitOverride(t *testing.T) {
	e := identityEngine(t, Options{AccountID: "acct-chosen"})
	got := e.resolveIdentity(identityJar("sapisid-A", "x"), nil, 0)

	if got.AccountID != "acct-chosen" {
		t.Errorf("account id = %q, want the supplied label", got.AccountID)
	}
	if got.Source != identitySourceOverride {
		t.Errorf("source = %q, want %q", got.Source, identitySourceOverride)
	}
}

// TestIdentityUsesTheSessionWhenThereIsOne: a GAIA id is stable for the life of
// the account, so it outranks anything derived from cookies.
func TestIdentityUsesTheSessionWhenThereIsOne(t *testing.T) {
	e := identityEngine(t, Options{})
	got := e.resolveIdentity(identityJar("sapisid-A", "x"), &auth.Session{UserID: "1029384756"}, 0)

	if got.AccountID != "acct-g1029384756" {
		t.Errorf("account id = %q, want acct-g1029384756", got.AccountID)
	}
	if got.Source != identitySourceSession {
		t.Errorf("source = %q, want %q", got.Source, identitySourceSession)
	}
}

// TestIdentityNeverPutsTheEmailInTheLabel: the label is written to logs and to
// the database, so an email has to be hashed rather than stored in the clear.
func TestIdentityNeverPutsTheEmailInTheLabel(t *testing.T) {
	e := identityEngine(t, Options{})
	got := e.resolveIdentity(identityJar("sapisid-A", "x"),
		&auth.Session{Email: "Someone@Example.com"}, 0)

	if got.Source != identitySourceSession {
		t.Fatalf("source = %q, want %q", got.Source, identitySourceSession)
	}
	if got.AccountID == "" ||
		strings.Contains(strings.ToLower(got.AccountID), "someone") ||
		strings.Contains(strings.ToLower(got.AccountID), "example.com") {
		t.Errorf("account id %q leaks the address", got.AccountID)
	}
}

// TestIdentitySeparatesAccountsSharingAJar: two accounts signed into one browser
// share one cookie jar, so the index has to be part of the identity or they
// collapse into a single row.
func TestIdentitySeparatesAccountsSharingAJar(t *testing.T) {
	e := identityEngine(t, Options{})
	jar := identityJar("sapisid-A", "x")

	first := e.resolveIdentity(jar, nil, 0)
	second := e.resolveIdentity(jar, nil, 1)

	if first.AccountID == second.AccountID {
		t.Errorf("index 0 and index 1 both resolved to %q", first.AccountID)
	}
	if first.Key == second.Key {
		t.Errorf("index 0 and index 1 share the anchor %q", first.Key)
	}
}
