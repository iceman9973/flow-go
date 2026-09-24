package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/app"
	"github.com/kodelyx/flow-go/flow-go/internal/bridge"
	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/engine"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

/* ------------------------------------------------------------------ *
 * doctor
 * ------------------------------------------------------------------ */

// doctorAttachWindow is how long doctor waits for the extension.
//
// Shorter than a generation run's window on purpose. A run has nothing else to do
// and every reason to wait; doctor is answering a question and has other checks
// to get through, so it takes the fast answer. The extension dials in
// immediately when it is loaded, and the 30-second alarm it also runs on is not
// worth waiting for to change one word of one line.
const doctorAttachWindow = 3 * time.Second

// doctorCheck is one line of the diagnosis.
//
// The command is a list of these rather than a paragraph of prose because the
// question it answers — "can this machine generate right now?" — is decided by
// whether any *required* check failed, and a reader who has to parse four
// sentences to find that out has been handed a report instead of an answer.
type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	// Fatal marks a check whose failure means generation cannot work at all.
	// The distinction is real and not cosmetic: the engine is designed to run
	// with no browser attached, so "no extension" is a finding about the
	// environment, while "the upstream rejected the session" is a finding about
	// the thing the caller is trying to do.
	Fatal bool `json:"fatal,omitempty"`
	// Action is what to do about it. Non-empty only when OK is false, because an
	// action attached to a healthy check is noise, and noise in a field like
	// this teaches the reader to skip it.
	Action string `json:"action,omitempty"`
}

// doctorReport is everything the command learned, so the JSON and text renderers
// cannot disagree about what was found.
type doctorReport struct {
	Version string        `json:"version"`
	Ready   bool          `json:"ready"`
	Summary string        `json:"summary"`
	Checks  []doctorCheck `json:"checks"`
	// Paths is reported separately from the checks: it is context for reading
	// the rest, not a thing that can pass or fail.
	Paths doctorPaths `json:"paths"`
}

type doctorPaths struct {
	DataDir   string `json:"data_dir"`
	CookieDir string `json:"cookie_dir"`
	OutputDir string `json:"output_dir"`
	Database  string `json:"database"`
}

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	asJSON := fs.Bool("json", false, "print the diagnosis as JSON")
	probe := fs.Bool("probe", true, "make one authenticated upstream call to prove the session works")
	timeout := fs.Duration("timeout", 90*time.Second, "how long the whole diagnosis may take")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	report := diagnose(ctx, &common, *probe)

	if *asJSON {
		printJSON(report)
		if report.Ready {
			return 0
		}
		return 1
	}

	renderDoctor(report)
	if report.Ready {
		return 0
	}
	return 1
}

// diagnose collects the checks.
//
// There is no server to ask any more. The bridge process listens on the WebSocket
// and writes bundles; every generation is a CLI run reading one of them. So this
// always diagnoses the way a real run behaves — attach the bridge, bootstrap, make
// one authenticated call — which is also what keeps the report honest: a
// diagnostic that inspects state its own run path never touches is a second
// implementation of the question, and the two drift until doctor says yes and the
// run says no.
func diagnose(ctx context.Context, common *commonFlags, probe bool) doctorReport {
	report := doctorReport{
		Version: Version,
		Paths: doctorPaths{
			DataDir:   config.DataDir(),
			CookieDir: config.CookieDir(),
			OutputDir: config.OutputDir(),
			Database:  databasePath(common),
		},
	}

	report.Checks = append(report.Checks, checkCookies())
	report.Checks = append(report.Checks, diagnoseLocally(ctx, common, probe, report.Paths.Database)...)

	report.Ready, report.Summary = summarise(report.Checks)
	return report
}

// diagnoseLocally answers the browser, session and database questions the way a
// real CLI run would: attach the bridge, bootstrap, and make one authenticated
// call.
//
// It runs the same start-up sequence as `generate` deliberately. A doctor that
// inspects state its own run path never touches is a second implementation of
// the question, and the two drift until the doctor says yes and the run says no.
//
// The cost of that fidelity is a side effect worth knowing about: bootstrapping
// adopts an account identity, which writes to the accounts table. It is the same
// write a generation makes, and it is idempotent — but a diagnostic that changes
// state is worth saying out loud, so the help text says it.
func diagnoseLocally(ctx context.Context, common *commonFlags, probe bool, dbPath string) []doctorCheck {
	a, err := common.build()
	if err != nil {
		return []doctorCheck{
			{Name: "browser", OK: false, Detail: "the engine could not be assembled: " + err.Error(),
				Action: "check that the data directory is writable"},
			{Name: "session", OK: false, Fatal: true, Detail: "not checked — the engine did not build"},
			checkDatabase(dbPath, nil),
		}
	}
	defer a.Close()

	attachBridgeWithin(ctx, a, doctorAttachWindow, true)

	status := a.Bridge.Status()
	browser := checkBrowser(status, a.Engine.CompetingExtensions())

	if err := bootstrap(ctx, a); err != nil {
		return []doctorCheck{
			browser,
			{Name: "session", OK: false, Fatal: true, Detail: err.Error(),
				Action: sessionAction(status.Connected)},
			checkDatabase(dbPath, localStats(a)),
		}
	}

	session := doctorCheck{
		Name:   "session",
		OK:     true,
		Detail: "bootstrapped from the cookie file; upstream not probed (--probe=false)",
	}
	if probe {
		credits, err := a.Engine.Credits(ctx)
		switch {
		case err != nil:
			session = doctorCheck{Name: "session", OK: false, Fatal: true, Detail: err.Error(),
				Action: sessionAction(status.Connected)}
		default:
			session.Detail = fmt.Sprintf("upstream accepted the session — %d credits available", credits)
		}
	}

	return []doctorCheck{browser, session, checkDatabase(dbPath, localStats(a))}
}

// summarise reduces the checks to a verdict and one sentence.
//
// A failed non-fatal check does not make the machine unready — it makes it
// unready *to recover*, which is worth saying and is not the same thing.
//
// The sentence names the failing checks rather than repeating their detail. The
// detail is already on the line above with the action under it, and a verdict
// that reprints a path is a second place for the same fact to be wrong.
func summarise(checks []doctorCheck) (bool, string) {
	var blocking, attention []string
	for _, c := range checks {
		if c.OK {
			continue
		}
		if c.Fatal {
			blocking = append(blocking, c.Name)
		} else {
			attention = append(attention, c.Name)
		}
	}

	switch {
	case len(blocking) > 0:
		return false, fmt.Sprintf("not ready — %s did not pass; the action is under that check",
			strings.Join(blocking, ", "))
	case len(attention) > 0:
		return true, fmt.Sprintf("ready — generation works; %s needs attention",
			strings.Join(attention, ", "))
	default:
		return true, "ready — generation requests will be accepted"
	}
}

/* ------------------------------------------------------------------ *
 * The checks
 * ------------------------------------------------------------------ */

// checkCookies reports the credentials that would actually be used.
//
// It reads the account file a run would read, which is now the only thing there
// is: the bridge process holds no session of its own to ask, and a generation is
// a separate process that loads one bundle from disk.
func checkCookies() doctorCheck {
	path, jar, err := loadCookieJar()
	if err != nil {
		return doctorCheck{
			Name:   "cookies",
			OK:     false,
			Fatal:  true,
			Detail: "no cookie file at " + strings.Join(cookieCandidates(), " or "),
			Action: "load flow-go/extension in Chrome and run `flow-go bridge` so it " +
				"writes cookies/account_<key>.json",
		}
	}

	if !jar.HasAuthCookies() {
		return doctorCheck{
			Name: "cookies", OK: false, Fatal: true,
			Detail: fmt.Sprintf("%d in %s, but none of them are credentials", jar.Count(), path),
			Action: "sign in to Flow in the browser the extension is loaded in",
		}
	}

	detail := fmt.Sprintf("%d in %s, credentials present", jar.Count(), path)
	if expiry, ok := jar.EarliestExpiry(); ok && time.Now().After(expiry) {
		return doctorCheck{
			Name: "cookies", OK: false, Fatal: true,
			Detail: detail + fmt.Sprintf(", but the earliest expired %s", expiry.Format(time.RFC3339)),
			Action: "re-sync from the browser",
		}
	}
	return doctorCheck{Name: "cookies", OK: true, Detail: detail}
}

// checkBrowser reports the extension, and is deliberately not fatal: a run works
// from a persisted bundle with no browser attached at all. What a missing browser
// costs is the ability to *recover* a dead session, and that is what sessionAction
// says.
//
// competing is the engine's list of attached Flow-capable extensions, and more than
// one is worth flagging rather than rounding to "connected". The bridge holds a
// single connection and `Current()` is whichever profile attached last, so a result
// can belong to the other profile. It is not the configuration it used to be,
// though: each attached profile is registered as its own account and the pool
// routes between them, so the old advice to close every profile but one would throw
// away working accounts.
func checkBrowser(status bridge.Status, competing []string) doctorCheck {
	if !status.Connected {
		return doctorCheck{
			Name:   "browser",
			OK:     false,
			Detail: "no extension attached",
			Action: "load flow-go/extension in Chrome, signed in to Flow",
		}
	}

	detail := "extension connected"
	if status.Version != "" {
		detail += " (" + status.Version + ")"
	}
	detail += fmt.Sprintf(", %d cookies from the page", status.CookieCount)
	if status.FlowOperations {
		detail += fmt.Sprintf(", %d flow operations", len(status.ExtensionOps))
	} else {
		// The generic CDP bridge attaches to the same port and advertises no
		// flow.* operations. Generation still works, but the captcha broker does
		// not, so this is worth naming rather than rounding to "connected".
		detail += ", but no flow operations — generic bridge only"
	}

	if len(competing) > 1 {
		return doctorCheck{
			Name: "browser", OK: false,
			Detail: detail + fmt.Sprintf("; %d Flow extensions attached, each registered as its own account",
				len(competing)),
			Action: "fine for the pool, but a run follows whichever profile connected last; " +
				"close all but one if the result has to belong to a known account",
		}
	}
	return doctorCheck{Name: "browser", OK: true, Detail: detail}
}

func checkDatabase(path string, stats *store.SystemStats) doctorCheck {
	exists, size := fileFacts(path)
	if !exists {
		return doctorCheck{
			Name: "database", OK: true,
			Detail: "not created yet at " + path + " — the engine creates it on first write",
		}
	}
	if stats == nil {
		return doctorCheck{Name: "database", OK: true,
			Detail: fmt.Sprintf("%s (%s)", path, humanBytes(size))}
	}

	// Every status is named, including `empty`, because the buckets are summed
	// individually and a status left out of the list disappears from the total
	// without making it look wrong.
	return doctorCheck{
		Name: "database", OK: true,
		Detail: fmt.Sprintf("%s (%s), %d generations — %d succeeded / %d failed / %d empty",
			path, humanBytes(size), stats.TotalGenerations,
			stats.Succeeded, stats.Failed, stats.Empty),
	}
}

// sessionAction is the recovery step for a session the upstream will not accept.
//
// There is one step now, and it depends only on whether the browser is attached:
// a dead session can only be renewed from the page that owns it, and the page is
// only reachable through the extension. `flow-go bridge` is what puts the fresh
// cookies where the next run will read them. This used to name
// `POST /v1/bridge/refresh`, which no longer exists — telling a caller to POST to
// an endpoint that is not listening is worse than saying nothing.
func sessionAction(browserAttached bool) string {
	if browserAttached {
		return "the extension is attached: let the Flow tab reload, then run `flow-go bridge` " +
			"to write fresh cookies, and retry"
	}
	return "load flow-go/extension in Chrome signed in to Flow, run `flow-go bridge` so it " +
		"writes a bundle, then retry"
}

// localStats reads the aggregates straight from the store. Only called once the
// engine has been built, so the database is known to exist.
func localStats(a *app.App) *store.SystemStats {
	stats, err := a.Store.Stats()
	if err != nil {
		return nil
	}
	return &stats
}

/* ------------------------------------------------------------------ *
 * Shared cookie lookup
 * ------------------------------------------------------------------ */

// cookieCandidates are the two paths the engine will try, in the order it tries
// them.
//
// It lives in one place because `cookies` and `doctor` both exist to answer
// "which jar would a run actually use?", and two copies of the list is how they
// start answering it differently. Reading only CookieDir reported a file the
// engine never loaded once the bridge began writing its own.
func cookieCandidates() []string {
	return []string{
		filepath.Join(config.CookieDir(), "cookies.json"),
		filepath.Join(config.DataDir(), "cookies.json"),
	}
}

// loadCookieJar returns the first candidate that reads, and its path.
func loadCookieJar() (string, *cookiejar.Jar, error) {
	// The per-account files first, because they are the source of truth now: the
	// bridge writes one per signed-in profile and no longer writes a shared file
	// at all. Newest first, so this reports the account that synced most recently
	// rather than whichever filename happens to sort first.
	if dumps, err := engine.DiscoverAccountJars(config.CookieDir()); err == nil {
		for _, path := range dumps {
			jar, loadErr := cookiejar.LoadFile(path)
			if loadErr == nil && jar.Count() > 0 {
				return path, jar, nil
			}
		}
	}

	// `cookies.json` last, and only for a file put there by hand: nothing writes
	// it any more, so finding one means an operator placed it.
	var lastErr error
	for _, candidate := range cookieCandidates() {
		jar, err := cookiejar.LoadFile(candidate)
		if err == nil {
			return candidate, jar, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no cookie file found")
	}
	return "", nil, lastErr
}

// databasePath is where this invocation would put the database: --db when given,
// otherwise the configured default.
func databasePath(common *commonFlags) string {
	if strings.TrimSpace(common.db) != "" {
		return common.db
	}
	return config.DBPath()
}

func fileFacts(path string) (bool, int64) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false, 0
	}
	return true, info.Size()
}

/* ------------------------------------------------------------------ *
 * Rendering
 * ------------------------------------------------------------------ */

func renderDoctor(r doctorReport) {
	fmt.Println()
	fmt.Printf("  flow-go %s — doctor\n", r.Version)
	fmt.Println("  " + strings.Repeat("-", 52))

	fmt.Printf("  %-17s %s\n", "data dir", r.Paths.DataDir)
	fmt.Printf("  %-17s %s\n", "cookie dir", r.Paths.CookieDir)
	fmt.Printf("  %-17s %s\n", "output dir", r.Paths.OutputDir)
	fmt.Printf("  %-17s %s\n", "log file", config.LogPath())
	fmt.Println()

	for _, c := range r.Checks {
		fmt.Printf("  %-5s %-17s %s\n", doctorMarker(c), c.Name, c.Detail)
		if c.Action != "" {
			fmt.Printf("  %-5s %-17s %s\n", "", "next", c.Action)
		}
	}

	fmt.Println()
	fmt.Println("  " + r.Summary)
	fmt.Println()
}

// doctorMarker is the first column: a word, not a glyph, so the output is
// greppable and `grep fail` works in a terminal whose font has no opinion about
// check marks.
func doctorMarker(c doctorCheck) string {
	switch {
	case c.OK:
		return "ok"
	case c.Fatal:
		return "fail"
	default:
		return "warn"
	}
}
