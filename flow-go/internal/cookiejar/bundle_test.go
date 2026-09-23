package cookiejar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bundle is one account in one file. The property that matters most is that
// adding it did not break the shapes already on disk: three different formats
// share the cookie directory, and two of them use the same `cookies` key with
// different types.

func bundleCookie(name, value string) Cookie {
	return Cookie{Domain: ".google.com", Path: "/", Name: name, Value: value}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestABundleRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account_test.json")

	want := &Bundle{
		AccountID: "acct-hb4069beef31e",
		ProjectID: "72c23f1e-22c5-45ac-a143-36f4c8f3625d",
		At:        "AIQ-s5jRGKlcU_Tnk5-Fb5Sqm0XU",
		Fsid:      "933442678266528993",
		Fingerprint: &Fingerprint{
			UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/153.0.0.0",
			SecChUa:   `"Google Chrome";v="153"`,
			Platform:  "macOS",
			Language:  "en-US",
		},
		Cookies: []Cookie{bundleCookie("SAPISID", "sapisid-value")},
	}
	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadBundleFile(path)
	if err != nil {
		t.Fatalf("LoadBundleFile: %v", err)
	}

	if got.AccountID != want.AccountID || got.ProjectID != want.ProjectID {
		t.Errorf("account/project = %q/%q, want %q/%q",
			got.AccountID, got.ProjectID, want.AccountID, want.ProjectID)
	}
	if got.At != want.At || got.Fsid != want.Fsid {
		t.Errorf("tokens = %q/%q, want %q/%q", got.At, got.Fsid, want.At, want.Fsid)
	}
	if got.Fingerprint == nil || got.Fingerprint.UserAgent != want.Fingerprint.UserAgent {
		t.Errorf("fingerprint = %+v, want %+v", got.Fingerprint, want.Fingerprint)
	}
	if got.Fingerprint.SecChUa != want.Fingerprint.SecChUa {
		t.Errorf("sec-ch-ua = %q, want %q", got.Fingerprint.SecChUa, want.Fingerprint.SecChUa)
	}
	if len(got.Cookies) != 1 || got.Cookies[0].Value != "sapisid-value" {
		t.Errorf("cookies = %+v, want the one written", got.Cookies)
	}
}

// TestTheFileSpellsTheKeysTheWayTheFormatDoes pins the wire spelling. These
// names are read by other tools, so a Go field rename must not silently change
// the file.
func TestTheFileSpellsTheKeysTheWayTheFormatDoes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account_test.json")
	bundle := &Bundle{
		AccountID:   "acct-1",
		ProjectID:   "project-1",
		At:          "at-1",
		Fsid:        "fsid-1",
		Fingerprint: &Fingerprint{UserAgent: "UA", SecChUa: "CH", Platform: "macOS", Language: "en-US"},
		Cookies:     []Cookie{bundleCookie("SAPISID", "v")},
	}
	if err := bundle.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, key := range []string{
		`"account_id"`, `"project_id"`, `"at"`, `"fsid"`,
		`"fingerprint"`, `"user_agent"`, `"sec_ch_ua"`, `"platform"`, `"language"`, `"cookies"`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("the file is missing %s:\n%s", key, raw)
		}
	}
}

func TestLoadFileReadsABundlesCookies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account_test.json")
	bundle := &Bundle{Cookies: []Cookie{bundleCookie("SAPISID", "from-a-bundle")}}
	if err := bundle.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	jar, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if jar.Count() != 1 {
		t.Fatalf("count = %d, want 1", jar.Count())
	}
	if got := jar.Cookies()[0].Value; got != "from-a-bundle" {
		t.Errorf("value = %q, want from-a-bundle", got)
	}
}

// TestLoadFileStillReadsEveryOlderShape is the backward-compatibility half. All
// three of these exist in the wild — a bare array is what the bridge wrote until
// today, and the other two are hand-written dumps.
func TestLoadFileStillReadsEveryOlderShape(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name     string
		content  string
		wantName string
	}{
		{
			name:     "bare array",
			content:  `[{"domain":".google.com","path":"/","name":"SAPISID","value":"array-value"}]`,
			wantName: "SAPISID",
		},
		{
			// The legacy object whose `cookies` is a *string*, which is the shape
			// that shares a key with the bundle and differs in type.
			name:     "legacy header string",
			content:  `{"cookies":"SAPISID=string-value; SID=other","updated_at":1700000000}`,
			wantName: "SAPISID",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, dir, strings.ReplaceAll(tc.name, " ", "_")+".json", tc.content)

			jar, err := LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			found := false
			for _, ck := range jar.Cookies() {
				if ck.Name == tc.wantName {
					found = true
				}
			}
			if !found {
				t.Errorf("no %s in the loaded jar: %+v", tc.wantName, jar.Cookies())
			}
		})
	}
}

// TestLoadBundleFileAcceptsALegacyArray keeps the two entry points consistent:
// a caller that wants the bundle should not have to know whether the file on
// disk predates bundles.
func TestLoadBundleFileAcceptsALegacyArray(t *testing.T) {
	path := writeFile(t, t.TempDir(), "account_legacy.json",
		`[{"domain":".google.com","path":"/","name":"SAPISID","value":"legacy"}]`)

	bundle, err := LoadBundleFile(path)
	if err != nil {
		t.Fatalf("LoadBundleFile: %v", err)
	}
	if len(bundle.Cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(bundle.Cookies))
	}
	// Nothing to adopt, and that has to read as "absent" rather than as a
	// zero-valued identity someone might use.
	if !bundle.Fingerprint.Empty() {
		t.Errorf("fingerprint = %+v, want empty", bundle.Fingerprint)
	}
	if bundle.ProjectID != "" || bundle.At != "" {
		t.Errorf("project/tokens = %q/%q, want empty", bundle.ProjectID, bundle.At)
	}
}

func TestAFingerprintWithNoUserAgentIsEmpty(t *testing.T) {
	var missing *Fingerprint
	if !missing.Empty() {
		t.Error("a nil fingerprint must read as empty")
	}
	if !(&Fingerprint{Platform: "macOS"}).Empty() {
		t.Error("a fingerprint with no user agent must read as empty")
	}
	if (&Fingerprint{UserAgent: "UA"}).Empty() {
		t.Error("a fingerprint with a user agent must not read as empty")
	}
}

/* ------------------------------------------------------------------ *
 * Complete — the gate every registration path goes through
 * ------------------------------------------------------------------ */

// What Complete rejects matters as much as what it accepts. Every case below is
// something a run genuinely cannot do without, and every one of them fails
// *silently* when it is missing — an unauthenticated call, a batchexecute
// request that primes for a token the server will not hand over, a captcha token
// spent under a client it was not minted for. That silence is the whole reason
// the gate exists rather than the failure being left to whatever notices first.
func TestCompleteRequiresEverythingARunNeeds(t *testing.T) {
	full := func() *Bundle {
		return &Bundle{
			ProjectID:   "project-1",
			At:          "at-value",
			Fsid:        "-1234",
			Fingerprint: &Fingerprint{UserAgent: "UA", SecChUa: "CH", Platform: "macOS"},
			Cookies:     []Cookie{bundleCookie("SAPISID", "sapisid-value")},
		}
	}

	if !full().Complete() {
		t.Fatal("a bundle with a credential, both tokens and an identity must be complete")
	}

	cases := []struct {
		name   string
		mutate func(*Bundle)
	}{
		{"no cookies at all", func(b *Bundle) { b.Cookies = nil }},
		{"cookies but nothing that authenticates", func(b *Bundle) {
			b.Cookies = []Cookie{bundleCookie("_ga", "GA1.1.000000000.0000000000")}
		}},
		{"no anti-CSRF token", func(b *Bundle) { b.At = "" }},
		{"no f.sid", func(b *Bundle) { b.Fsid = "" }},
		{"no fingerprint", func(b *Bundle) { b.Fingerprint = nil }},
		{"a fingerprint with no user agent", func(b *Bundle) {
			b.Fingerprint = &Fingerprint{Platform: "macOS"}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := full()
			tc.mutate(b)
			if b.Complete() {
				t.Errorf("Complete() = true with %s: %+v", tc.name, b)
			}
		})
	}
}

func TestANilBundleIsNotComplete(t *testing.T) {
	var missing *Bundle
	if missing.Complete() {
		t.Error("a nil bundle must not read as complete")
	}
}

// The case that changes behaviour, and the one to check first if a dump that
// used to work stops being registered.
//
// A bare cookie array still *loads* as a bundle — that compatibility is
// deliberate and older files depend on it — but it carries a credential and
// nothing else, so it is not complete and every registration path skips it.
// That is the intended trade: such a file generates nothing and says nothing
// about why, and a skipped account in the log is more use than a silent failure
// per generation.
func TestALegacyCookieArrayIsNotComplete(t *testing.T) {
	path := writeFile(t, t.TempDir(), "account_legacy.json",
		`[{"domain":".google.com","path":"/","name":"SAPISID","value":"legacy"}]`)

	bundle, err := LoadBundleFile(path)
	if err != nil {
		t.Fatalf("LoadBundleFile: %v", err)
	}
	if len(bundle.Cookies) != 1 {
		t.Fatalf("cookies = %d, want the array's one", len(bundle.Cookies))
	}
	if bundle.Complete() {
		t.Error("a bare cookie array has neither tokens nor an identity, so it is not complete")
	}
}

// Complete() must not be satisfiable by a bundle that only *looks* full. The
// fingerprint is checked through Empty() rather than against nil, because a
// decoded struct with every field zero is what a renamed or mistyped key leaves
// behind — present, and worth nothing.
func TestAZeroValuedFingerprintIsNotComplete(t *testing.T) {
	b := &Bundle{
		At:          "at-value",
		Fsid:        "-1234",
		Fingerprint: &Fingerprint{},
		Cookies:     []Cookie{bundleCookie("SAPISID", "v")},
	}
	if b.Complete() {
		t.Error("a fingerprint struct with no user agent must not satisfy the gate")
	}
}
