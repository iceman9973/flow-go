package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/bridge"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/pool"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

/* ------------------------------------------------------------------ *
 * Fixtures
 * ------------------------------------------------------------------ */

// credentialCookie is one the Labs session exchange depends on. The name is
// taken from the real list rather than invented, because "has credentials" is
// decided by name and a fixture with a made-up one would pass while the check it
// is testing did nothing.
func credentialCookie(expiry time.Time) cookiejar.Cookie {
	c := cookiejar.Cookie{Domain: ".google.com", Name: "SAPISID", Path: "/", Value: "x"}
	if !expiry.IsZero() {
		c.ExpirationDate = float64(expiry.Unix())
	}
	return c
}

// analyticsCookie is deliberately one that carries no session: a jar full of
// these is what a signed-out browser has, and the check must not read it as a
// usable credential set.
func analyticsCookie() cookiejar.Cookie {
	return cookiejar.Cookie{Domain: ".google.com", Name: "_ga", Path: "/", Value: "GA1.2.3"}
}

func writeJar(t *testing.T, path string, cookies ...cookiejar.Cookie) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := cookiejar.FromCookies(cookies, "test").Save(path); err != nil {
		t.Fatalf("writing the jar at %s: %v", path, err)
	}
}

// isolatedPoints configures both directories the cookie lookup reads, so a test
// never sees the developer's real cookies.
func isolatedPoints(t *testing.T) (cookieDir, dataDir string) {
	t.Helper()
	base := t.TempDir()
	cookieDir = filepath.Join(base, "cookies")
	dataDir = filepath.Join(base, "data")
	t.Setenv("FLOW_COOKIE_DIR", cookieDir)
	t.Setenv("FLOW_DATA_DIR", dataDir)
	return cookieDir, dataDir
}

/* ------------------------------------------------------------------ *
 * The verdict
 * ------------------------------------------------------------------ */

func TestSummariseBlocksOnlyOnAFatalCheck(t *testing.T) {
	// The distinction is the whole point of the Fatal flag: the engine is
	// designed to run with no browser, so "no extension attached" must not make
	// a machine that can generate report that it cannot.
	checks := []doctorCheck{
		{Name: "cookies", OK: true},
		{Name: "browser", OK: false},
	}

	ready, summary := summarise(checks)
	if !ready {
		t.Fatalf("a missing browser made the machine unready: %q", summary)
	}
	if !strings.Contains(summary, "browser") {
		t.Errorf("the summary does not name the check needing attention: %q", summary)
	}
}

func TestSummariseReportsNotReadyForAFatalCheck(t *testing.T) {
	checks := []doctorCheck{
		{Name: "cookies", OK: true},
		{Name: "session", OK: false, Fatal: true},
		{Name: "browser", OK: false},
	}

	ready, summary := summarise(checks)
	if ready {
		t.Fatalf("a dead session reported the machine as ready: %q", summary)
	}
	if !strings.Contains(summary, "session") {
		t.Errorf("the summary does not name the blocking check: %q", summary)
	}
}

func TestSummariseDoesNotReprintTheDetail(t *testing.T) {
	// The detail is already on its own line with the action under it. A verdict
	// that repeats it is a second place for the same fact to be wrong.
	checks := []doctorCheck{{
		Name:   "cookies",
		OK:     false,
		Fatal:  true,
		Detail: "/var/folders/bf/tt77c0y1561ch214h97s09y00000gn/T/a-very-long-path/cookies.json",
	}}

	_, summary := summarise(checks)
	if strings.Contains(summary, "/var/folders") {
		t.Errorf("the verdict reprinted the detail: %q", summary)
	}
}

func TestSummariseIsCleanWhenEverythingPasses(t *testing.T) {
	ready, summary := summarise([]doctorCheck{{Name: "cookies", OK: true}, {Name: "session", OK: true}})
	if !ready {
		t.Fatalf("ready=false with every check passing: %q", summary)
	}
	if !strings.Contains(summary, "generation requests will be accepted") {
		t.Errorf("unexpected summary: %q", summary)
	}
}

func TestDoctorMarkerSeparatesWarnFromFail(t *testing.T) {
	cases := []struct {
		name  string
		check doctorCheck
		want  string
	}{
		{"passing", doctorCheck{OK: true}, "ok"},
		{"non-fatal failure", doctorCheck{OK: false}, "warn"},
		{"fatal failure", doctorCheck{OK: false, Fatal: true}, "fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctorMarker(tc.check); got != tc.want {
				t.Errorf("doctorMarker = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionActionNeverNamesAnEndpointThatIsNotListening(t *testing.T) {
	// POST /v1/bridge/refresh exists only on a running server. Telling a caller
	// with no server to call it sends them to a refused connection, which is
	// worse than saying nothing at all.
	withoutServer := sessionAction(false, false)
	if strings.Contains(withoutServer, "/v1/bridge/refresh") {
		t.Errorf("the no-server action names a server-only endpoint: %q", withoutServer)
	}
	if !strings.Contains(withoutServer, "serve") {
		t.Errorf("the no-server action does not say how to get one: %q", withoutServer)
	}

	if got := sessionAction(true, true); !strings.Contains(got, "/v1/bridge/refresh") {
		t.Errorf("with a server and a browser attached the action should be the refresh: %q", got)
	}
	if got := sessionAction(true, false); !strings.Contains(got, "attach") {
		t.Errorf("with a server and no browser the action should be to attach one: %q", got)
	}
}

/* ------------------------------------------------------------------ *
 * The checks
 * ------------------------------------------------------------------ */

func TestCheckCookiesPrefersTheRunningServersJar(t *testing.T) {
	// A server that is spending credits is proof its jar works. A local file
	// check that disagrees with it makes the report contradict itself.
	cookieDir, _ := isolatedPoints(t)
	writeJar(t, filepath.Join(cookieDir, "cookies.json"), credentialCookie(time.Now().Add(time.Hour)))

	srv := &serverSnapshot{}
	srv.Health.Bridge = bridge.Status{CookieCount: 15, HasCredentials: true}

	got := checkCookies(srv)
	if !got.OK {
		t.Fatalf("checkCookies reported a failure against a healthy server jar: %+v", got)
	}
	if !strings.Contains(got.Detail, "server's jar") {
		t.Errorf("the detail does not say which jar it read: %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "CLI would read") {
		t.Errorf("the detail does not report the CLI's own copy: %q", got.Detail)
	}
}

func TestCheckCookiesStillReportsWhenTheServerJarHasNoCredentials(t *testing.T) {
	isolatedPoints(t)
	srv := &serverSnapshot{}
	srv.Health.Bridge = bridge.Status{CookieCount: 15, HasCredentials: false}

	got := checkCookies(srv)
	if got.OK || !got.Fatal {
		t.Fatalf("a server jar with no credentials should be a fatal finding: %+v", got)
	}
	if !strings.Contains(got.Detail, "none of them are credentials") {
		t.Errorf("the detail does not say what is wrong: %q", got.Detail)
	}
}

func TestCheckCookiesFailsWhenTheOnlyCookiesAreNotCredentials(t *testing.T) {
	// The failure this guards: a signed-out browser still hands over plenty of
	// cookies, and a count of them looks like a session.
	cookieDir, _ := isolatedPoints(t)
	writeJar(t, filepath.Join(cookieDir, "cookies.json"), analyticsCookie(), analyticsCookie())

	got := checkCookies(nil)
	if got.OK || !got.Fatal {
		t.Fatalf("a jar with no credential cookie should be fatal: %+v", got)
	}
	if !strings.Contains(got.Detail, "credentials") {
		t.Errorf("the detail does not name the problem: %q", got.Detail)
	}
}

func TestCheckCookiesFailsOnAnExpiredCredential(t *testing.T) {
	cookieDir, _ := isolatedPoints(t)
	writeJar(t, filepath.Join(cookieDir, "cookies.json"), credentialCookie(time.Now().Add(-time.Hour)))

	got := checkCookies(nil)
	if got.OK || !got.Fatal {
		t.Fatalf("an expired credential should be fatal: %+v", got)
	}
	if !strings.Contains(got.Detail, "expired") {
		t.Errorf("the detail does not say the credential expired: %q", got.Detail)
	}
}

func TestCheckCookiesPassesWithAValidCredential(t *testing.T) {
	cookieDir, _ := isolatedPoints(t)
	writeJar(t, filepath.Join(cookieDir, "cookies.json"), credentialCookie(time.Now().Add(time.Hour)))

	got := checkCookies(nil)
	if !got.OK {
		t.Fatalf("a valid credential was reported as a failure: %+v", got)
	}
}

func TestCheckCookiesReportsTheAbsenceWithoutAServer(t *testing.T) {
	isolatedPoints(t)

	got := checkCookies(nil)
	if got.OK || !got.Fatal {
		t.Fatalf("no cookie file should be fatal with no server: %+v", got)
	}
	// Both candidates are named, because which one is missing is the first thing
	// to check when a run behaves like it holds an old session.
	if !strings.Contains(got.Detail, "cookies.json") {
		t.Errorf("the detail does not name the paths it looked at: %q", got.Detail)
	}
}

func TestCheckBrowserIsNotFatalWithoutAnExtension(t *testing.T) {
	// This is the boundary the whole Fatal flag exists for. The engine takes
	// cookies, a token and a fingerprint from a snapshot and runs with no
	// browser at all; a missing browser costs the ability to recover a dead
	// session, not the ability to generate.
	got := checkBrowser(bridge.Status{Connected: false})
	if got.OK {
		t.Fatalf("no extension should not be reported as passing: %+v", got)
	}
	if got.Fatal {
		t.Errorf("no extension is not fatal, and marking it so would make a working machine report as broken")
	}
	if got.Action == "" {
		t.Error("a failing check with no action is a dead end")
	}
}

func TestCheckBrowserNamesAGenericBridge(t *testing.T) {
	// The generic CDP bridge attaches to the same port and advertises no flow.*
	// operations. Generation still works through it, but the captcha broker does
	// not, so rounding it to "connected" hides a real difference.
	got := checkBrowser(bridge.Status{Connected: true, FlowOperations: false})
	if !got.OK {
		t.Fatalf("a generic bridge is still a connected browser: %+v", got)
	}
	if !strings.Contains(got.Detail, "generic") {
		t.Errorf("the detail does not distinguish a generic bridge: %q", got.Detail)
	}

	flow := checkBrowser(bridge.Status{
		Connected: true, Version: "1.0.0", FlowOperations: true,
		ExtensionOps: []string{"flow.captcha", "flow.projects"},
	})
	if !strings.Contains(flow.Detail, "1.0.0") || !strings.Contains(flow.Detail, "2 flow operations") {
		t.Errorf("the flow-bridge detail is missing what it advertises: %q", flow.Detail)
	}
}

func TestCheckServerFlagsCompetingExtensions(t *testing.T) {
	// Two signed-in profiles take turns owning the bridge's one cookie jar, so
	// calls report the wrong account. The server reports it; this is where it
	// becomes visible without reading JSON.
	srv := &serverSnapshot{}
	srv.Health.Status = "ok"
	srv.Health.FlowExtensions = []string{"127.0.0.1:1", "127.0.0.1:2"}

	got := checkServer(srv)
	if got.OK {
		t.Fatalf("two Flow extensions should not pass: %+v", got)
	}
	if got.Fatal {
		t.Error("competing extensions are a configuration problem, not a dead engine")
	}
}

func TestCheckServerReportsTheHealthStatusAndAccount(t *testing.T) {
	srv := &serverSnapshot{}
	srv.Health.Status = "ok"
	srv.Status.AccountID = "acct-0c960ba36880"
	srv.Status.Pool = pool.Stats{TotalWorkers: 1}

	got := checkServer(srv)
	if !got.OK {
		t.Fatalf("a healthy server was reported as failing: %+v", got)
	}
	for _, want := range []string{"status ok", "acct-0c960ba36880", "1 worker"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the detail is missing %q: %q", want, got.Detail)
		}
	}
}

func TestCheckDatabaseReportsANotYetCreatedFileAsPassing(t *testing.T) {
	// The engine creates the database on first write, so its absence is not a
	// fault. Reporting it as one would tell a fresh install it is broken.
	path := filepath.Join(t.TempDir(), "absent", "flow.db")

	got := checkDatabase(path, nil)
	if !got.OK {
		t.Fatalf("an absent database should not be a failure: %+v", got)
	}
	if !strings.Contains(got.Detail, "first write") {
		t.Errorf("the detail does not say the engine will create it: %q", got.Detail)
	}
}

func TestCheckDatabaseNamesEveryStatusBucket(t *testing.T) {
	// The buckets are summed individually, so a status left out of this line
	// disappears from the report without making the total look wrong — which is
	// exactly how nine of fifteen rows went uncounted before.
	dir := t.TempDir()
	path := filepath.Join(dir, "flow.db")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("creating the file: %v", err)
	}

	stats := &store.SystemStats{
		TotalGenerations: 15, Succeeded: 6, Failed: 0, InFlight: 0, Empty: 9,
	}
	got := checkDatabase(path, stats)
	if !got.OK {
		t.Fatalf("checkDatabase failed: %+v", got)
	}
	for _, want := range []string{"15 generations", "6 succeeded", "0 failed", "9 empty"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the detail is missing %q: %q", want, got.Detail)
		}
	}
}

func TestStatsOfTreatsAnUnansweredStatsEndpointAsAbsent(t *testing.T) {
	// A server that answers /health and then fails /stats is running an older
	// build. That is a real thing that happens, and it must not be reported as a
	// database with zero rows in it.
	empty := &serverSnapshot{}
	if got := statsOf(empty); got != nil {
		t.Errorf("an unanswered /stats produced statistics: %+v", got)
	}

	present := &serverSnapshot{}
	present.Stats.Database = store.SystemStats{DatabaseFile: "/x/flow.db", TotalGenerations: 3}
	if got := statsOf(present); got == nil || got.TotalGenerations != 3 {
		t.Errorf("an answered /stats was discarded: %+v", got)
	}
}

/* ------------------------------------------------------------------ *
 * The shared cookie lookup
 * ------------------------------------------------------------------ */

func TestLoadCookieJarPrefersTheCookieDir(t *testing.T) {
	// Both files exist and the engine reads the first. A second copy in the data
	// directory is exactly the stale one that makes a run behave like it holds
	// an old session, so which was read is worth reporting.
	cookieDir, dataDir := isolatedPoints(t)
	writeJar(t, filepath.Join(cookieDir, "cookies.json"), credentialCookie(time.Now().Add(time.Hour)))
	writeJar(t, filepath.Join(dataDir, "cookies.json"), analyticsCookie())

	path, jar, err := loadCookieJar()
	if err != nil {
		t.Fatalf("loadCookieJar: %v", err)
	}
	if path != filepath.Join(cookieDir, "cookies.json") {
		t.Errorf("loadCookieJar read %s, want the cookie dir copy", path)
	}
	if !jar.HasAuthCookies() {
		t.Error("loadCookieJar returned the analytics copy from the data dir")
	}
}

func TestLoadCookieJarFallsBackToTheDataDir(t *testing.T) {
	_, dataDir := isolatedPoints(t)
	writeJar(t, filepath.Join(dataDir, "cookies.json"), credentialCookie(time.Now().Add(time.Hour)))

	path, _, err := loadCookieJar()
	if err != nil {
		t.Fatalf("loadCookieJar: %v", err)
	}
	if path != filepath.Join(dataDir, "cookies.json") {
		t.Errorf("loadCookieJar read %s, want the data dir copy", path)
	}
}

func TestLoadCookieJarErrorsWhenNeitherExists(t *testing.T) {
	isolatedPoints(t)

	if _, _, err := loadCookieJar(); err == nil {
		t.Fatal("loadCookieJar succeeded with no cookie file at either candidate")
	}
}

func TestCookieCandidatesAreBothReported(t *testing.T) {
	cookieDir, dataDir := isolatedPoints(t)

	got := cookieCandidates()
	if len(got) != 2 {
		t.Fatalf("expected two candidates, got %d", len(got))
	}
	if got[0] != filepath.Join(cookieDir, "cookies.json") {
		t.Errorf("the first candidate is %s, want the cookie dir", got[0])
	}
	if got[1] != filepath.Join(dataDir, "cookies.json") {
		t.Errorf("the second candidate is %s, want the data dir", got[1])
	}
}

func TestDatabasePathHonoursTheFlag(t *testing.T) {
	t.Setenv("FLOW_DATA_DIR", t.TempDir())

	explicit := &commonFlags{db: "/tmp/somewhere/flow.db"}
	if got := databasePath(explicit); got != "/tmp/somewhere/flow.db" {
		t.Errorf("databasePath ignored --db: %q", got)
	}

	// A whitespace-only flag is not a path, and treating it as one would send
	// the reader to a file that cannot exist.
	blank := &commonFlags{db: "   "}
	if got := databasePath(blank); strings.TrimSpace(got) == "" || got == "   " {
		t.Errorf("databasePath accepted a blank --db: %q", got)
	}
}
