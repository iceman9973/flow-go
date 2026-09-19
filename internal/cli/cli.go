// Package cli implements the flow-go command line.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kodelyx/cdp-control/cookiejar"
	"github.com/kodelyx/flow-go/internal/app"
	"github.com/kodelyx/flow-go/internal/auth"
	"github.com/kodelyx/flow-go/internal/batchexecute"
	"github.com/kodelyx/flow-go/internal/config"
	"github.com/kodelyx/flow-go/internal/engine"
	"github.com/kodelyx/flow-go/internal/flowapi"
	"github.com/kodelyx/flow-go/internal/httpx"
)

// Version is the build version.
const Version = "0.1.0"

// Run dispatches a command. Returns a process exit code.
func Run(args []string) int {
	config.LoadEnv()

	if len(args) == 0 {
		fmt.Print(usage())
		return 0
	}

	switch args[0] {
	case "serve", "server":
		return runServe(args[1:])
	case "generate", "video":
		return runGenerate(args[1:])
	case "image":
		return runImage(args[1:])
	case "stats":
		return runStats(args[1:])
	case "export":
		return runExport(args[1:])
	case "cookies":
		return runCookies(args[1:])
	case "session-refresh":
		return runSessionRefresh(args[1:])
	case "batchexecute":
		return runBatchExecute(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("flow-go %s\n", Version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage())
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage())
		return 2
	}
}

func usage() string {
	return `flow-go — Google Flow generation engine

The browser supplies cookies and base information. Everything else — access
tokens, project resolution, generation, polling, upscaling, downloads, storage —
happens here, in Go. No generation request travels through a browser.

USAGE
  flow-go <command> [flags]

COMMANDS
  serve                 Start the HTTP API and the browser-Cdp extension bridge
  generate              Generate a video
  image                 Generate an image
  stats                 Print database statistics
  export [file]         Export statistics as JSON
  cookies               Show cookie and credential status
  version               Print the version

COMMON FLAGS
  --project-id          Flow project ID (default: resolved from the session)
  --proxy               Route upstream traffic through one exit IP
  --captcha             reCAPTCHA strategy: auto | broker | http | off
  --db                  Database path

GENERATE FLAGS
  --prompt              Prompt text (required)
  --duration            4 | 6 | 8 | 10                 (default 10)
  --quality             360p | 720p                    (default 720p)
  --count               1..4                           (default 1)
  --start-image         Local image path or media ID; makes it image-to-video
  --end-image           Local image path or media ID
  --no-download         Skip writing the result to output/

  --aspect, --resolution, --reference and --seed exist on the legacy aisandbox
  transport and not on the batchexecute one. Asking for one is an error naming
  it, rather than a flag that is accepted and then ignored.

IMAGE FLAGS
  --prompt              Prompt text (required)
  --model               harbor_seal | narwhal | gem_pix_2
  --no-download         Skip writing the result to output/

EXAMPLES
  flow-go serve
  flow-go generate --prompt "a paper boat on a river" --duration 8
  flow-go generate --prompt "slow push in" --start-image ./frame.png --quality 360p
  flow-go image --prompt "a single red paper boat"
  flow-go stats
`
}

/* ------------------------------------------------------------------ *
 * Flags
 * ------------------------------------------------------------------ */

type commonFlags struct {
	projectID string
	proxy     string
	captcha   string
	db        string
	email     string
	// at and fsid are the page tokens taken from a running server's session, and
	// are not flags: they are short-lived page state, not something a caller
	// should be typing.
	at   string
	fsid string
}

func (c *commonFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.projectID, "project-id", "", "Flow project ID")
	fs.StringVar(&c.proxy, "proxy", "", "proxy URL for upstream traffic")
	fs.StringVar(&c.captcha, "captcha", "auto", "reCAPTCHA strategy: auto|broker|http|off")
	fs.StringVar(&c.db, "db", "", "database path")
	fs.StringVar(&c.email, "email", "", "account email, used as the OAuth login hint")
}

func (c *commonFlags) build() (*app.App, error) {
	return app.Build(app.Config{
		ProjectID:   c.projectID,
		ProxyURL:    c.proxy,
		CaptchaMode: c.captcha,
		DBPath:      c.db,
		AtToken:     c.at,
		Fsid:        c.fsid,
	})
}

// adoptRunningSession seeds this process from a server that has the browser.
//
// The bridge port admits exactly one host, and everything the engine takes from
// the browser is short-lived: the cookies rotate, and the page tokens exist only
// in a loaded page. A CLI running alongside the server therefore has two bad
// options — no browser at all, or a cookie file that was last written whenever the
// bridge happened to sync. This is the third: take a snapshot at start-up, so the
// copy is never older than the run using it.
//
// Best-effort by design. A CLI with no server running is a supported case, and it
// falls back to the persisted copy; the only cost of not asking is the staleness
// that was already there.
func adoptRunningSession(ctx context.Context) (projectID, at, fsid string, ok bool) {
	raw, err := os.ReadFile(filepath.Join(config.DataDir(), "bridge-token"))
	if err != nil {
		return "", "", "", false
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", "", "", false
	}

	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://127.0.0.1:%d/v1/session", config.HTTPPort)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", "", false
	}
	req.Header.Set("X-Bridge-Token", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", false
	}

	var snapshot struct {
		Cookies   []cookiejar.Cookie `json:"cookies"`
		At        string             `json:"at"`
		Fsid      string             `json:"fsid"`
		ProjectID string             `json:"project_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil || len(snapshot.Cookies) == 0 {
		return "", "", "", false
	}

	// Persist where the engine looks for it, so the bootstrap below picks it up
	// without needing a new path through the engine.
	jar := cookiejar.FromCookies(snapshot.Cookies, "server session")
	if err := jar.Save(filepath.Join(config.DataDir(), "cookies.json")); err != nil {
		return "", "", "", false
	}

	fmt.Printf("  session          taken from the running server (%d cookies)\n", jar.Count())
	return snapshot.ProjectID, snapshot.At, snapshot.Fsid, true
}

// adoptSessionFor fills in what a running server can supply, leaving anything the
// caller set explicitly alone. Returns whether a session was adopted, so the caller
// knows whether it still needs to look for a browser itself.
func adoptSessionFor(ctx context.Context, common *commonFlags) bool {
	projectID, at, fsid, ok := adoptRunningSession(ctx)
	if !ok {
		return false
	}
	if strings.TrimSpace(common.projectID) == "" {
		common.projectID = projectID
	}
	common.at, common.fsid = at, fsid
	return true
}

// bridgeAttachWindow bounds how long this process waits to become the bridge host.
//
// The extension reconnects on its own schedule — immediately after a disconnect,
// and on a 30-second alarm — so a process that starts listening cannot know how
// long it will be until one of those fires. Five seconds catches the common case
// and does not turn every browserless run into a wait.
const bridgeAttachWindow = 5 * time.Second

// attachBridge makes this process the bridge host, when it can be.
//
// This is the other half of not needing a browser to have a session: the port
// admits exactly one host, so if a server already has it the listen fails and the
// caller falls back to taking that server's session. If the port is free, the
// extension connects here instead and the engine reads cookies from the live page
// rather than from a snapshot — which is the same thing the server does, and the
// only version of this that does not decay.
//
// Best-effort and bounded: the extension may simply not be loaded, and the
// persisted copy is still there when it is not.
func attachBridge(ctx context.Context, a *app.App) bool {
	go func() {
		if err := a.Bridge.Listen(ctx); err != nil {
			// Expected whenever a server owns the port; the caller falls back.
			fmt.Printf("  bridge           not started (%v)\n", err)
		}
	}()

	deadline := time.Now().Add(bridgeAttachWindow)
	for time.Now().Before(deadline) {
		if a.Bridge.Connected() {
			fmt.Printf("  bridge           extension attached, cookies read live\n")
			return true
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return false
		}
	}
	return false
}

/* ------------------------------------------------------------------ *
 * serve
 * ------------------------------------------------------------------ */

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	port := fs.Int("port", config.HTTPPort, "HTTP API port")
	_ = fs.Parse(args)

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("flow-go %s\n", Version)
	fmt.Printf("  API            http://127.0.0.1:%d\n", *port)
	fmt.Printf("  extension      ws://127.0.0.1:%d  (load ../browser-Cdp/extension/ in Chrome)\n", config.WSPort)
	fmt.Printf("  database       %s\n", a.Store.Path())
	fmt.Printf("  output         %s\n", config.OutputDir())
	fmt.Println()
	fmt.Println("Waiting for the browser-Cdp extension to connect...")

	if err := a.Serve(ctx, *port); err != nil {
		return fail(err)
	}
	return 0
}

/* ------------------------------------------------------------------ *
 * generate
 * ------------------------------------------------------------------ */

func runGenerate(args []string) int {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	prompt := fs.String("prompt", "", "prompt text")
	duration := fs.Int("duration", config.DefaultDuration, "4, 6, 8, or 10")
	quality := fs.String("quality", "720p", "360p or 720p")
	count := fs.Int("count", 1, "number of variations")
	startImage := fs.String("start-image", "", "local image path or media ID")
	endImage := fs.String("end-image", "", "local image path or media ID")
	noDownload := fs.Bool("no-download", false, "skip writing the result to disk")
	// These four exist on the legacy aisandbox transport and not on the
	// batchexecute one, which is the only one that works. They are still
	// declared so that asking for one is an error naming it, rather than a flag
	// the parser accepts and the engine silently ignores — which is the failure
	// this codebase keeps recording.
	aspect := fs.String("aspect", "", "not implemented on the batchexecute transport")
	resolution := fs.String("resolution", "", "not implemented on the batchexecute transport")
	seed := fs.Int64("seed", 0, "not implemented on the batchexecute transport")
	var references stringList
	fs.Var(&references, "reference", "not implemented on the batchexecute transport")
	_ = fs.Parse(args)

	if *prompt == "" {
		return fail(fmt.Errorf("--prompt is required"))
	}
	if unsupported := unsupportedGenerateFlags(*aspect, *resolution, *seed, references); len(unsupported) > 0 {
		return fail(fmt.Errorf("not implemented on the batchexecute transport: %s",
			strings.Join(unsupported, ", ")))
	}

	// Seed from a running server before building, because the engine reads its
	// cookies and page tokens at construction.
	adopted := adoptSessionFor(context.Background(), &common)

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No server to take a session from, so try to be the bridge host ourselves —
	// that is how a run with no server still gets cookies from the browser
	// instead of from a snapshot.
	if !adopted {
		attachBridge(ctx, a)
	}

	if err := bootstrap(ctx, a); err != nil {
		return fail(err)
	}

	fmt.Printf("generating %d video(s) at %ds, %s\n", *count, *duration, *quality)

	// The batchexecute path, which is the one the app uses and the only one that
	// works. This used to call GenerateVideo, whose aisandbox REST surface is
	// legacy and cannot work at all — so the CLI was broken while the HTTP API,
	// which had already moved, was fine.
	outcome, err := a.Engine.GenerateVideoViaBatch(ctx, engine.BatchVideoRequest{
		Prompt:     *prompt,
		Duration:   *duration,
		Quality:    *quality,
		Count:      *count,
		StartImage: *startImage,
		EndImage:   *endImage,
		Wait:       !*noDownload,
		Download:   !*noDownload,
	})
	if err != nil {
		return fail(err)
	}
	printJSON(outcome)
	return 0
}

// unsupportedGenerateFlags names the generate flags the batchexecute transport
// does not implement, so asking for one fails with its name instead of being
// quietly dropped.
func unsupportedGenerateFlags(aspect, resolution string, seed int64, references []string) []string {
	var out []string
	if strings.TrimSpace(aspect) != "" {
		out = append(out, "--aspect")
	}
	if strings.TrimSpace(resolution) != "" {
		out = append(out, "--resolution")
	}
	if seed != 0 {
		out = append(out, "--seed")
	}
	if len(references) > 0 {
		out = append(out, "--reference")
	}
	return out
}

/* ------------------------------------------------------------------ *
 * image
 * ------------------------------------------------------------------ */

func runImage(args []string) int {
	fs := flag.NewFlagSet("image", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	prompt := fs.String("prompt", "", "prompt text")
	count := fs.Int("count", 1, "number of variations")
	model := fs.String("model", "", "harbor_seal, narwhal, or gem_pix_2")
	noDownload := fs.Bool("no-download", false, "skip writing the result to disk")
	// Not implemented on the batchexecute transport; declared so asking for one
	// is an error naming it rather than a silently ignored flag.
	aspect := fs.String("aspect", "", "not implemented on the batchexecute transport")
	seed := fs.Int64("seed", 0, "not implemented on the batchexecute transport")
	_ = fs.Parse(args)

	if *prompt == "" {
		return fail(fmt.Errorf("--prompt is required"))
	}
	if unsupported := unsupportedGenerateFlags(*aspect, "", *seed, nil); len(unsupported) > 0 {
		return fail(fmt.Errorf("not implemented on the batchexecute transport: %s",
			strings.Join(unsupported, ", ")))
	}
	if *count != 1 {
		// The image RPC takes one prompt and returns one asset. Saying so is
		// better than accepting the flag and returning a single image.
		return fail(fmt.Errorf("--count is not implemented on the batchexecute transport; " +
			"submit twice for two images"))
	}

	// Seed from a running server before building, because the engine reads its
	// cookies and page tokens at construction.
	adopted := adoptSessionFor(context.Background(), &common)

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No server to take a session from, so try to be the bridge host ourselves.
	if !adopted {
		attachBridge(ctx, a)
	}

	if err := bootstrap(ctx, a); err != nil {
		return fail(err)
	}

	// The batchexecute path, which is the one the HTTP API already uses.
	outcome, err := a.Engine.GenerateImageViaBatch(ctx, engine.BatchImageRequest{
		Prompt:   *prompt,
		Model:    *model,
		Download: !*noDownload,
	})
	if err != nil {
		return fail(err)
	}
	printJSON(outcome)
	return 0
}

// bootstrap makes sure the engine has a token. CLI commands have no extension
// bridge, so they run entirely from the persisted cookie file.
func bootstrap(ctx context.Context, a *app.App) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return a.Engine.Bootstrap(ctx)
}

/* ------------------------------------------------------------------ *
 * stats
 * ------------------------------------------------------------------ */

func runStats(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	asJSON := fs.Bool("json", false, "print raw JSON")
	_ = fs.Parse(args)

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	stats, err := a.Store.Stats()
	if err != nil {
		return fail(err)
	}

	if *asJSON {
		printJSON(stats)
		return 0
	}

	fmt.Println()
	fmt.Println("  flow-go — system statistics")
	fmt.Println("  " + strings.Repeat("-", 52))
	fmt.Printf("  %-24s %s\n", "database", stats.DatabaseFile)
	fmt.Printf("  %-24s %s\n", "engine", stats.Engine)
	fmt.Printf("  %-24s %d\n", "generations", stats.TotalGenerations)
	fmt.Printf("  %-24s %d succeeded / %d failed / %d in flight\n",
		"  breakdown", stats.Succeeded, stats.Failed, stats.InFlight)
	fmt.Printf("  %-24s %d video / %d image\n", "  by kind", stats.VideosGenerated, stats.ImagesGenerated)
	fmt.Printf("  %-24s %d\n", "media files", stats.TotalMedia)
	fmt.Printf("  %-24s %s\n", "media size", humanBytes(stats.MediaBytes))
	fmt.Printf("  %-24s %d\n", "upstream requests", stats.TotalRequests)
	fmt.Printf("  %-24s %d\n", "credits spent", stats.CreditsSpent)
	fmt.Printf("  %-24s %.0f ms\n", "average duration", stats.AvgElapsedMS)
	fmt.Printf("  %-24s %d (%d active)\n", "tracked accounts", stats.TrackedAccounts, stats.ActiveAccounts)
	if stats.FirstGeneration != nil && stats.LastGeneration != nil {
		fmt.Printf("  %-24s %s .. %s\n", "activity window",
			stats.FirstGeneration.Format(time.RFC3339), stats.LastGeneration.Format(time.RFC3339))
	}
	fmt.Println()
	fmt.Println("  Every figure above is an aggregate over the rows actually present.")
	fmt.Println()
	return 0
}

/* ------------------------------------------------------------------ *
 * export
 * ------------------------------------------------------------------ */

func runExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	_ = fs.Parse(args)

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	stats, err := a.Store.Stats()
	if err != nil {
		return fail(err)
	}
	accounts, _ := a.Store.ListAccounts()
	generations, _ := a.Store.RecentGenerations(500)
	media, _ := a.Store.RecentMedia(500)

	payload := map[string]any{
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"stats":       stats,
		"accounts":    accounts,
		"generations": generations,
		"media":       media,
	}

	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	if target == "" {
		target = filepath.Join(config.DataDir(), "export.json")
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return fail(err)
	}

	fmt.Printf("exported to %s\n", target)
	return 0
}

/* ------------------------------------------------------------------ *
 * cookies
 * ------------------------------------------------------------------ */

func runCookies(args []string) int {
	fs := flag.NewFlagSet("cookies", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	showNames := fs.Bool("names", false, "list cookie names (values are never printed)")
	_ = fs.Parse(args)

	// The same two candidates the engine tries, in the same order, so this
	// reports the jar a run would actually use. Reading only CookieDir reported
	// a file the engine never loaded once the bridge started writing its own.
	candidates := []string{
		filepath.Join(config.DataDir(), "cookies.json"),
		filepath.Join(config.CookieDir(), "cookies.json"),
	}

	var (
		path string
		jar  *cookiejar.Jar
		err  error
	)
	for _, candidate := range candidates {
		jar, err = cookiejar.LoadFile(candidate)
		if err == nil {
			path = candidate
			break
		}
	}

	if path == "" {
		fmt.Printf("  no cookies at %s\n", strings.Join(candidates, " or "))
		fmt.Println()
		fmt.Println("  Options:")
		fmt.Println("    1. Load flow-go/flow-go-extension/ in the browser and run `flow-go serve`;")
		fmt.Println("       the extension hands over cookies automatically.")
		fmt.Println("    2. POST a cookie dump to /api/sync-cookies.")
		fmt.Println("    3. Write a JSON array of cookies to one of those paths yourself.")
		return 1
	}

	fmt.Println()
	fmt.Printf("  %-24s %s\n", "cookie file", path)
	if len(candidates) > 1 {
		// Which of the two was read is the first thing to check when a run
		// behaves like it is holding an old session.
		for _, other := range candidates {
			if other == path {
				continue
			}
			if info, statErr := os.Stat(other); statErr == nil {
				fmt.Printf("  %-24s %s (not used)\n", "other copy", fmt.Sprintf("%s, %s", other, info.ModTime().Format(time.RFC3339)))
			}
		}
	}
	fmt.Printf("  %-24s %d\n", "cookies", jar.Count())
	fmt.Printf("  %-24s %s\n", "has credentials", yesNo(jar.HasAuthCookies()))
	fmt.Printf("  %-24s %s\n", "jar hash", short(jar.Hash()))

	if expiry, ok := jar.EarliestExpiry(); ok {
		fmt.Printf("  %-24s %s\n", "earliest expiry", expiry.Format(time.RFC3339))
		if time.Now().After(expiry) {
			fmt.Println()
			fmt.Println("  The credential cookies have expired. Re-sync from the browser.")
		}
	} else {
		fmt.Printf("  %-24s %s\n", "earliest expiry", "session cookies only")
	}

	if *showNames {
		fmt.Println()
		fmt.Println("  cookie names:")
		for _, name := range jar.Names() {
			fmt.Printf("    %s\n", name)
		}
	}
	fmt.Println()
	return 0
}

/* ------------------------------------------------------------------ *
 * session-refresh
 * ------------------------------------------------------------------ */

// runSessionRefresh rebuilds the Labs session cookie from the Google cookies
// using the NextAuth + OAuth handshake, then reports whether the resulting token
// is accepted by the generation API.
//
// This is the diagnostic for the failure mode where the session endpoint returns
// a token but flags ACCESS_TOKEN_REFRESH_NEEDED, and the generation API then
// rejects it with 401.
func runSessionRefresh(args []string) int {
	fs := flag.NewFlagSet("session-refresh", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	verify := fs.Bool("verify", true, "check the new token against the Flow API")
	_ = fs.Parse(args)

	path := filepath.Join(config.CookieDir(), "cookies.json")
	jar, err := cookiejar.LoadFile(path)
	if err != nil {
		return fail(fmt.Errorf("could not read %s: %w", path, err))
	}

	fmt.Printf("  cookies        %d\n", jar.Count())
	fmt.Printf("  credentials    %s\n", yesNo(jar.HasAuthCookies()))

	hc, err := httpx.New(
		httpx.WithTimeout(time.Duration(config.RequestTimeout)*time.Second),
		httpx.WithProxy(common.proxy),
	)
	if err != nil {
		return fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fmt.Println("  rebuilding the Labs session from the Google cookies...")
	rebuilt, err := auth.RefreshSessionToken(ctx, jar, hc, common.email)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n  session rebuild failed: %v\n", err)
		return 1
	}

	fmt.Printf("  new session token  %d chars\n", len(rebuilt))

	newJar := auth.WithSessionToken(jar, rebuilt)
	if err := newJar.Save(path); err != nil {
		return fail(fmt.Errorf("could not save the rebuilt cookies: %w", err))
	}
	fmt.Printf("  written to     %s\n", path)

	if !*verify {
		return 0
	}

	// Mint a token from the rebuilt session and see whether the generation API
	// accepts it. Without this the command would report success on a token that
	// still gets rejected.
	provider := auth.NewProvider(newJar, hc)
	session, err := provider.ForceRefresh(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n  could not mint a token from the rebuilt session: %v\n", err)
		return 1
	}
	fmt.Printf("  minted token   %d chars", len(session.AccessToken))
	if session.Email != "" {
		fmt.Printf(" for %s", session.Email)
	}
	fmt.Println()
	if session.UpstreamError != "" {
		fmt.Printf("  upstream says  %s\n", session.UpstreamError)
	}

	client := flowapi.New(provider, hc, flowapi.Options{AccountID: "verify"})
	if _, _, err := client.Credits(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "\n  the Flow API still rejects the token: %v\n", err)
		fmt.Fprintln(os.Stderr, "  The session cookie was rebuilt, but the account may need to sign in again in the browser.")
		return 1
	}

	fmt.Println("  Flow API       token accepted")
	return 0
}

/* ------------------------------------------------------------------ *
 * batchexecute
 * ------------------------------------------------------------------ */

// runBatchExecute issues one batchexecute RPC and prints the response.
//
// This is the transport the current Flow app uses. Running it against a known
// RPC id is the fastest way to tell whether cookie authentication is working,
// independently of any generation logic.
func runBatchExecute(args []string) int {
	fs := flag.NewFlagSet("batchexecute", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	rpc := fs.String("rpc", batchexecute.RPCIDProjectList, "RPC id to invoke")
	payload := fs.String("payload", "", "JSON argument for the RPC")
	sourcePath := fs.String("source-path", "/project", "app route the call is made from")
	buildLabel := fs.String("bl", "", "build label (the app's bl parameter)")
	sessionID := fs.String("f-sid", "", "session id (the app's f.sid parameter)")
	out := fs.String("out", "", "write the raw frames to this file instead of printing them")
	list := fs.Bool("list", false, "list the discovered RPC ids and exit")
	_ = fs.Parse(args)

	if *list {
		fmt.Println()
		fmt.Println("  Discovered Flow RPC ids (all verified live against a signed-in account):")
		fmt.Println()
		fmt.Printf("  %-9s %-42s %s\n", "ID", "PURPOSE", "PAYLOAD")
		fmt.Printf("  %-9s %-42s %s\n", "---", "-------", "-------")
		for _, entry := range batchexecute.KnownRPCs {
			fmt.Printf("  %-9s %-42s %s\n", entry.ID, entry.Name, entry.Payload)
		}
		fmt.Println()
		fmt.Println("  Example:")
		fmt.Println("    flow-go batchexecute --rpc HTrJv --payload '[]' \\")
		fmt.Println("      --source-path /project/<project-id> \\")
		fmt.Println("      --bl boq_labs-ai-sandbox-frontend_20260917.00_p0")
		fmt.Println()
		return 0
	}

	path := filepath.Join(config.CookieDir(), "cookies.json")
	jar, err := cookiejar.LoadFile(path)
	if err != nil {
		return fail(fmt.Errorf("could not read %s: %w", path, err))
	}

	hc, err := httpx.New(
		httpx.WithTimeout(time.Duration(config.RequestTimeout)*time.Second),
		httpx.WithProxy(common.proxy),
	)
	if err != nil {
		return fail(err)
	}

	client := batchexecute.New(jar, hc)

	if client.SAPISID() == "" {
		fmt.Fprintln(os.Stderr, "  no SAPISID cookie found — re-sync cookies from a signed-in browser")
		return 1
	}
	auth, err := client.Authorization()
	if err != nil {
		return fail(err)
	}
	fmt.Printf("  origin      %s\n", batchexecute.Origin)
	fmt.Printf("  rpc         %s\n", *rpc)
	fmt.Printf("  auth        %s\n", short(auth))

	var arg any
	if *payload != "" {
		if err := json.Unmarshal([]byte(*payload), &arg); err != nil {
			return fail(fmt.Errorf("--payload is not valid JSON: %w", err))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	frames, err := client.CallWith(ctx, *rpc, arg, batchexecute.CallOptions{
		SourcePath: *sourcePath,
		BuildLabel: *buildLabel,
		SessionID:  *sessionID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n  %v\n", err)
		return 1
	}

	fmt.Printf("  frames      %d\n\n", len(frames))

	if *out != "" {
		var buf strings.Builder
		for i, frame := range frames {
			fmt.Fprintf(&buf, "# frame %d (rpc %s)\n", i+1, frame.RPCID)
			var pretty any
			if err := json.Unmarshal(frame.Payload, &pretty); err == nil {
				data, _ := json.MarshalIndent(pretty, "", "  ")
				buf.Write(data)
			} else {
				buf.Write(frame.Payload)
			}
			buf.WriteString("\n\n")
		}
		if err := os.WriteFile(*out, []byte(buf.String()), 0o644); err != nil {
			return fail(err)
		}
		fmt.Printf("  written to  %s (%d bytes)\n", *out, buf.Len())
		return 0
	}

	for i, frame := range frames {
		fmt.Printf("  --- frame %d (rpc %s) ---\n", i+1, frame.RPCID)
		if len(frame.Payload) == 0 {
			fmt.Println("  (empty payload)")
			continue
		}
		var pretty any
		if err := json.Unmarshal(frame.Payload, &pretty); err == nil {
			data, _ := json.MarshalIndent(pretty, "  ", "  ")
			fmt.Printf("  %s\n", truncateOutput(string(data), 1500))
		} else {
			fmt.Printf("  %s\n", truncateOutput(string(frame.Payload), 1500))
		}
	}
	return 0
}

func truncateOutput(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "\n  ... (truncated)"
}

/* ------------------------------------------------------------------ *
 * Helpers
 * ------------------------------------------------------------------ */

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func printJSON(value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not render output: %v\n", err)
		return
	}
	fmt.Println(string(data))
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return 1
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func short(hash string) string {
	if len(hash) <= 16 {
		return hash
	}
	return hash[:16]
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
