package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/app"
	"github.com/kodelyx/flow-go/flow-go/internal/bridge"
	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/engine"
	"github.com/kodelyx/flow-go/flow-go/internal/pool"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

/* ------------------------------------------------------------------ *
 * doctor
 * ------------------------------------------------------------------ */

// doctorAttachWindow is how long doctor waits for the extension when no server
// owns the bridge port.
//
// Shorter than a generation run's window on purpose. A run has nothing else to do
// and every reason to wait; doctor is answering a question and has other checks
// to get through, so it takes the fast answer. The extension dials in
// immediately when it is loaded, and the 30-second alarm it also runs on is not
// worth waiting for to change one word of one line.
const doctorAttachWindow = 3 * time.Second

// doctorServerTimeout bounds the "is a server up?" question.
//
// Short because the answer is a connect to localhost: a server that cannot
// answer /health in this long is not going to answer usefully, and a doctor that
// hangs is worse than one that says it could not tell.
const doctorServerTimeout = 3 * time.Second

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

// diagnose collects the checks, asking a running server first.
//
// The server comes first because almost everything else is reported differently
// depending on whether one is running: it holds the live cookie jar, the live
// session, and the only /v1/bridge/refresh. Asking costs a connect to localhost,
// which fails immediately when nothing is listening, so the checks that can be
// read off the disk are not waiting behind a timeout.
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

	srv, up := probeServer(ctx)

	// Passing the server through matters: when one is up it is the process that
	// will make the calls, so its jar is the one that decides this check. Reading
	// only the local file made the two halves of the report contradict each
	// other — a "no cookie file" failure sitting next to a server happily
	// spending credits.
	report.Checks = append(report.Checks, checkCookies(srv))

	if up {
		report.Checks = append(report.Checks, checkServer(srv))
		report.Checks = append(report.Checks, checkBrowser(srv.Health.Bridge))
		report.Checks = append(report.Checks, checkSessionViaServer(ctx, srv, probe))
		report.Checks = append(report.Checks, checkDatabase(report.Paths.Database, statsOf(srv)))
	} else {
		report.Checks = append(report.Checks, doctorCheck{
			Name: "server",
			OK:   true,
			Detail: fmt.Sprintf("not running on 127.0.0.1:%d — the engine runs without one",
				config.HTTPPort),
		})
		report.Checks = append(report.Checks, diagnoseLocally(ctx, common, probe, report.Paths.Database)...)
	}

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
// state is worth saying out loud, so the help text says it. Nothing here runs
// when a server is up, because then the server is asked instead.
func diagnoseLocally(ctx context.Context, common *commonFlags, probe bool, dbPath string) []doctorCheck {
	// A server was already ruled out, so this cannot adopt a session and the
	// write it would otherwise perform on cookies.json cannot happen here. It is
	// still called because --project-id and --email come through the same path a
	// run would use.
	adoptSessionFor(ctx, common)

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
	browser := checkBrowser(status)

	if err := bootstrap(ctx, a); err != nil {
		return []doctorCheck{
			browser,
			{Name: "session", OK: false, Fatal: true, Detail: err.Error(),
				Action: sessionAction(false, status.Connected)},
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
				Action: sessionAction(false, status.Connected)}
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
// srv is non-nil when a server is running, and then it is the server's jar that
// decides the check: that is the process which will make the calls. The local
// file is still reported alongside, because it is what a CLI run would use and
// the two can legitimately differ — which is worth seeing rather than averaging
// over.
func checkCookies(srv *serverSnapshot) doctorCheck {
	if srv != nil && srv.Health.Bridge.CookieCount > 0 {
		detail := fmt.Sprintf("%d in the running server's jar", srv.Health.Bridge.CookieCount)
		if !srv.Health.Bridge.HasCredentials {
			return doctorCheck{
				Name: "cookies", OK: false, Fatal: true,
				Detail: detail + ", but none of them are credentials",
				Action: "sign in to Flow in the browser the extension is loaded in",
			}
		}
		if path, jar, err := loadCookieJar(); err == nil {
			detail += fmt.Sprintf("; the CLI would read %d from %s", jar.Count(), path)
		} else {
			detail += "; a CLI run alongside it would find no cookie file"
		}
		return doctorCheck{Name: "cookies", OK: true, Detail: detail}
	}

	path, jar, err := loadCookieJar()
	if err != nil {
		return doctorCheck{
			Name:   "cookies",
			OK:     false,
			Fatal:  true,
			Detail: "no cookie file at " + strings.Join(cookieCandidates(), " or "),
			Action: "load flow-go/extension in Chrome and run `flow-go serve`, " +
				"or POST a cookie dump to /api/sync-cookies",
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

func checkServer(s *serverSnapshot) doctorCheck {
	detail := fmt.Sprintf("running on 127.0.0.1:%d — status %s", config.HTTPPort, s.Health.Status)
	if s.Status.AccountID != "" {
		detail += ", account " + s.Status.AccountID
	}
	if n := s.Status.Pool.TotalWorkers; n > 0 {
		detail += fmt.Sprintf(", %d worker(s)", n)
	}

	// More than one Flow extension attached is still worth flagging, but it is no
	// longer the configuration it was. Each attached profile is now registered as
	// its own account, so the pool routes between them and they all appear in the
	// worker list — the old advice to close every profile but one would throw away
	// working accounts. What is still single-account is the engine's own batch
	// path, which reads one jar and follows whichever profile connected last.
	if len(s.Health.FlowExtensions) > 1 {
		return doctorCheck{
			Name: "server", OK: false,
			Detail: detail + fmt.Sprintf(" — %d Flow extensions attached, each registered as its own account",
				len(s.Health.FlowExtensions)),
			Action: "fine for the pool, but a batch result follows whichever profile connected last; " +
				"close all but one if it has to belong to a known account",
		}
	}
	return doctorCheck{Name: "server", OK: true, Detail: detail}
}

// checkBrowser reports the extension, and is deliberately not fatal: the engine
// takes cookies, an access token and a fingerprint from a snapshot and runs with
// no browser at all. What a missing browser costs is the ability to *recover* a
// dead session, and that is what sessionAction says.
func checkBrowser(status bridge.Status) doctorCheck {
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
	return doctorCheck{Name: "browser", OK: true, Detail: detail}
}

func checkSessionViaServer(ctx context.Context, s *serverSnapshot, probe bool) doctorCheck {
	// The server has already recorded a dead session, so there is nothing to
	// probe: the answer is in hand and costs no round trip. This is the field
	// that exists precisely because `ready` stays true once it has been true.
	if failure := s.Health.SessionFailure; failure != "" {
		return doctorCheck{Name: "session", OK: false, Fatal: true, Detail: failure,
			Action: sessionAction(true, s.Health.Bridge.Connected)}
	}
	if !s.Health.Ready {
		return doctorCheck{
			Name: "session", OK: false, Fatal: true,
			Detail: "the server has not bootstrapped an account yet",
			Action: "load the extension and sign in, or POST /api/sync-cookies with a cookie dump",
		}
	}
	if !probe {
		return doctorCheck{Name: "session", OK: true,
			Detail: "the server reports ready; upstream not probed (--probe=false)"}
	}

	// The server makes the call, not this process: it holds the live session,
	// and a second process asking the same question would answer it with a
	// cookie file that is one rotation out of date.
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var credits creditsResponse
	if err := serverGet(probeCtx, "/v1/credits", &credits); err != nil {
		return doctorCheck{Name: "session", OK: false, Fatal: true, Detail: err.Error(),
			Action: sessionAction(true, s.Health.Bridge.Connected)}
	}
	return doctorCheck{Name: "session", OK: true,
		Detail: fmt.Sprintf("upstream accepted the session — %d credits available", credits.Credits)}
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
// It depends on where the session can be renewed from, which is what decides the
// step rather than the wording of it: POST /v1/bridge/refresh exists only on a
// running server, and the extension only helps if it is attached. Telling a
// caller to POST to an endpoint that is not listening is worse than saying
// nothing.
func sessionAction(serverUp, browserAttached bool) string {
	switch {
	case serverUp && browserAttached:
		return "call POST /v1/bridge/refresh to have the page renew its own session, then retry"
	case serverUp:
		return "attach the browser extension, then call POST /v1/bridge/refresh and retry"
	default:
		return "load flow-go/extension in Chrome, then start the server with `flow-go serve`"
	}
}

/* ------------------------------------------------------------------ *
 * Talking to a running server
 * ------------------------------------------------------------------ */

type serverSnapshot struct {
	Health serverHealth
	Status serverStatus
	Stats  serverStats
}

type serverHealth struct {
	Status              string        `json:"status"`
	Ready               bool          `json:"ready"`
	BearerPathAvailable bool          `json:"bearer_path_available"`
	Bridge              bridge.Status `json:"bridge"`
	FlowExtensions      []string      `json:"flow_extensions"`
	Error               string        `json:"error"`
	SessionFailure      string        `json:"session_failure"`
}

type serverStatus struct {
	Ready               bool          `json:"ready"`
	BearerPathAvailable bool          `json:"bearer_path_available"`
	AccountID           string        `json:"account_id"`
	Bridge              bridge.Status `json:"bridge"`
	Pool                pool.Stats    `json:"pool"`
	Error               string        `json:"error"`
	SessionFailure      string        `json:"session_failure"`
}

type serverStats struct {
	Database store.SystemStats `json:"database"`
	Pool     pool.Stats        `json:"pool"`
	Bridge   bridge.Status     `json:"bridge"`
}

type creditsResponse struct {
	Credits   int    `json:"credits"`
	AccountID string `json:"account_id"`
	ProjectID string `json:"project_id"`
	Source    string `json:"source"`
	Error     string `json:"error"`
}

// probeServer asks a locally running server what it knows, and reports whether
// one answered at all.
//
// /health is the gate, because it is the endpoint that exists to be cheap and to
// always answer: a failure there means "no server", not "a broken server". The
// other two are best-effort — a server answering /health and then failing /stats
// is a running older build, which is a real thing that happens and is not a
// reason to report that no server is running.
func probeServer(ctx context.Context) (*serverSnapshot, bool) {
	ctx, cancel := context.WithTimeout(ctx, doctorServerTimeout)
	defer cancel()

	var health serverHealth
	if err := serverGet(ctx, "/health", &health); err != nil {
		return nil, false
	}

	s := &serverSnapshot{Health: health}
	_ = serverGet(ctx, "/status", &s.Status)
	_ = serverGet(ctx, "/stats", &s.Stats)
	return s, true
}

// statsOf returns the server's database aggregates, or nil when the server did
// not answer /stats.
func statsOf(s *serverSnapshot) *store.SystemStats {
	if s.Stats.Database.DatabaseFile == "" && s.Stats.Database.TotalGenerations == 0 {
		return nil
	}
	return &s.Stats.Database
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

func serverGet(ctx context.Context, path string, into any) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", config.HTTPPort, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The body carries the reason for a 5xx, and the reason is the finding —
		// "502" alone would send the reader to the server log for something the
		// server already said out loud.
		var failure struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&failure) == nil && failure.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, failure.Error)
		}
		return fmt.Errorf("%s returned %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
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
