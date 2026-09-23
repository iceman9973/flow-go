// Package cli implements the flow-go command line.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/app"
	"github.com/kodelyx/flow-go/flow-go/internal/auth"
	"github.com/kodelyx/flow-go/flow-go/internal/batchexecute"
	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/engine"
	"github.com/kodelyx/flow-go/flow-go/internal/flowapi"
	"github.com/kodelyx/flow-go/flow-go/internal/httpx"
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
	case "bridge", "serve", "server":
		return runBridge(args[1:])
	case "generate", "video":
		return runGenerate(args[1:])
	case "image":
		return runImage(args[1:])
	case "batch-all":
		return runBatchAll(args[1:])
	case "projects":
		return runProjects(args[1:])
	case "stats":
		return runStats(args[1:])
	case "doctor":
		return runDoctor(args[1:])
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

The browser supplies cookies and base information, and nothing else. Access
tokens, project resolution, generation, polling, downloads and storage all
happen here, in Go, over HTTPS. No generation request travels through a browser,
and none travels through an HTTP server either.

USAGE
  flow-go <command> [flags]

COMMANDS
  bridge                Listen for the Flow Bridge extension and persist what it
                        sends into cookies/account_<key>.json. This is the whole
                        of that process: no HTTP API, no generation.
  doctor                Diagnose whether this machine can generate right now
  generate              Generate a video
  image                 Generate an image
  batch-all             Generate one image per account in cookies/account_*.json
  projects              List the account's Flow projects
  stats                 Print database statistics
  export [file]         Export statistics as JSON
  cookies               Show cookie and credential status
  version               Print the version

COMMON FLAGS
  --project-id          Flow project ID (default: resolved from the session)
  --proxy               Route upstream traffic through one exit IP
  --captcha             reCAPTCHA strategy: auto | broker | http | off
  --db                  Database path
  --cookies             Run as one named account, e.g.
                        cookies/account_<key>.json. Without it the freshest
                        account file in cookies/ is used.

DOCTOR FLAGS
  --probe               Make one authenticated upstream call to prove the
                        session works                        (default true)
  --json                Print the diagnosis as JSON
  --timeout             How long the whole diagnosis may take (default 90s)

  Exits 0 when every required check passed and 1 when one did not, so it can
  gate a script. Nothing that can be read off the disk waits behind a network
  call.

  Doctor attaches the bridge and bootstraps the engine, which is the same
  start-up a generation run performs — including the account identity it adopts
  into the database. That write is idempotent, and it is the reason the check
  can answer honestly rather than by inspection.

GENERATE FLAGS
  --prompt              Prompt text (required)
  --duration            4 | 6 | 8 | 10                 (default 10)
  --quality             360p | 720p                    (default 720p)
  --count               1..4                           (default 1)
  --start-image         Local image path or media ID; makes it image-to-video
  --end-image           Local image path or media ID
  --no-download         Skip writing the result to output/

  --aspect, --resolution, --seed and --reference exist on the legacy aisandbox
  transport and not on the batchexecute one. Passing one is reported on stderr
  and the run continues without it, so a script carrying a stale flag still
  produces its video.

IMAGE FLAGS
  --prompt              Prompt text (required)
  --model               harbor_seal | narwhal | gem_pix_2
  --count               Accepted, but only 1 is honoured: the RPC returns one
                        asset per call
  --no-download         Skip writing the result to output/

EXAMPLES
  flow-go bridge
  flow-go image --prompt "a single red paper boat"
  flow-go image --prompt "a red cube" --cookies cookies/account_ab12cd34ef56.json
  flow-go generate --prompt "a paper boat on a river" --duration 8
  flow-go generate --prompt "slow push in" --start-image ./frame.png --quality 360p
  flow-go batch-all --prompt "a paper boat" --model narwhal
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
	// cookies is an explicit cookie file to run as, e.g.
	// `cookies/account_27b104997fa0.json`. Empty means the usual search.
	//
	// It is a flag rather than an env var because its whole purpose is to run
	// one command as one named account, and a setting that persists would make
	// the next command silently use the previous account.
	cookies string
}

func (c *commonFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.projectID, "project-id", "", "Flow project ID")
	fs.StringVar(&c.proxy, "proxy", "", "proxy URL for upstream traffic")
	fs.StringVar(&c.captcha, "captcha", "auto", "reCAPTCHA strategy: auto|broker|http|off")
	fs.StringVar(&c.db, "db", "", "database path")
	fs.StringVar(&c.email, "email", "", "account email, used as the OAuth login hint")
	fs.StringVar(&c.cookies, "cookies", "",
		"cookie file to run as, e.g. cookies/account_<key>.json (default: the usual search)")
}

func (c *commonFlags) build() (*app.App, error) {
	return app.Build(app.Config{
		ProjectID:   c.projectID,
		ProxyURL:    c.proxy,
		CaptchaMode: c.captcha,
		DBPath:      c.db,
		CookieFile:  c.cookies,
	})
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
	return attachBridgeWithin(ctx, a, bridgeAttachWindow, false)
}

// attachBridgeWithin is attachBridge with the window and the reporting under the
// caller's control.
//
// Two callers want two different things from the same sequence. A generation run
// wants the progress lines, because a five-second pause with no output on it
// reads as a hang. `doctor` wants silence: its output is a column-aligned
// checklist, and a line printed from inside a goroutine lands wherever the
// scheduler decides — which, in practice, is the middle of the checklist.
func attachBridgeWithin(ctx context.Context, a *app.App, window time.Duration, quiet bool) bool {
	go func() {
		if err := a.Bridge.Listen(ctx); err != nil && !quiet {
			// Expected whenever a server owns the port; the caller falls back.
			fmt.Printf("  bridge           not started (%v)\n", err)
		}
	}()

	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if a.Bridge.Connected() {
			if !quiet {
				fmt.Printf("  bridge           extension attached, cookies read live\n")
			}
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

// runBridge is the whole of the bridge process: a WebSocket listener, and the
// account bundles it writes.
//
// There is deliberately no HTTP surface and no `--port`. The bridge receives; the
// CLI generates. A generation request that travelled through a listening socket
// would have to be authenticated, would have to name an account, and would make
// the process holding the browser the process that spends the credits — three
// things this split exists to avoid.
func runBridge(args []string) int {
	fs := flag.NewFlagSet("bridge", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	_ = fs.Parse(args)

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("flow-go %s\n", Version)
	fmt.Printf("  extension      ws://127.0.0.1:%d  (load extension/ in Chrome)\n", config.WSPort)
	fmt.Printf("  cookies        %s\n", config.CookieDir())
	fmt.Printf("  database       %s\n", a.Store.Path())
	fmt.Printf("  output         %s\n", config.OutputDir())
	fmt.Println()
	fmt.Println("Listening for the Flow Bridge extension. Bundles are written to")
	fmt.Println("cookies/account_<key>.json as each profile connects; generate with")
	fmt.Println("`flow-go image` or `flow-go generate` in another shell.")

	if err := a.RunBridge(ctx); err != nil {
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
	// declared, and still named when passed — but the run continues without
	// them. Failing was defensible only while the alternative was silence, and
	// the caller's next move was always to drop the flag and retry, which is a
	// move this command can make for them.
	aspect := fs.String("aspect", "", "ignored — not implemented on the batchexecute transport")
	resolution := fs.String("resolution", "", "ignored — not implemented on the batchexecute transport")
	seed := fs.Int64("seed", 0, "ignored — not implemented on the batchexecute transport")
	var references stringList
	fs.Var(&references, "reference",
		"ignored — reference-to-video has no batchexecute transport; use --start-image")
	_ = fs.Parse(args)

	if *prompt == "" {
		return fail(fmt.Errorf("--prompt is required"))
	}
	warnIgnoredFlags(ignoredGenerateFlags(*aspect, *resolution, *seed, references))

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Try to become the bridge host, so a run with no bundle yet can still take
	// cookies from the browser. When the `bridge` command already owns the port
	// this is a no-op, and the account comes from cookies/ instead.
	attachBridge(ctx, a)

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

// ignoredGenerateFlags names the generate flags the batchexecute transport does
// not apply, so the caller is told rather than left to notice the result differs
// from what they asked for.
func ignoredGenerateFlags(aspect, resolution string, seed int64, references []string) []string {
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

// warnIgnoredFlags reports flags that will not be applied and lets the run
// continue.
//
// To stderr, so a warning can never be mistaken for part of the result: the
// stdout of these commands is JSON that callers pipe into jq.
func warnIgnoredFlags(names []string) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "  ignoring %s — not implemented on the batchexecute transport\n",
		strings.Join(names, ", "))
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
	// Not implemented on the batchexecute transport; declared, and named when
	// passed, but the run continues without them.
	aspect := fs.String("aspect", "", "ignored — not implemented on the batchexecute transport")
	seed := fs.Int64("seed", 0, "ignored — not implemented on the batchexecute transport")
	_ = fs.Parse(args)

	if *prompt == "" {
		return fail(fmt.Errorf("--prompt is required"))
	}
	warnIgnoredFlags(ignoredGenerateFlags(*aspect, "", *seed, nil))
	if *count != 1 {
		// The image RPC takes one prompt and returns one asset. Reporting it and
		// continuing beats failing the run: the caller gets the image they asked
		// for, plus a note that the other three are not coming.
		warnIgnoredFlags([]string{fmt.Sprintf("--count %d (one asset per call)", *count)})
	}

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Try to become the bridge host, so a run with no bundle yet can still take
	// cookies from the browser. When the `bridge` command already owns the port
	// this is a no-op, and the account comes from cookies/ instead.
	attachBridge(ctx, a)

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
 * projects
 * ------------------------------------------------------------------ */

// runProjects lists the account's Flow projects over the transport, or creates
// one.
//
// This is the browser-free way to a project id: no tab is opened and no editor
// URL is read. `--new` goes further and is the browser-free way to *make* one —
// it calls the RPC the app's New project button would have called.
func runProjects(args []string) int {
	fs := flag.NewFlagSet("projects", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	asJSON := fs.Bool("json", false, "print raw JSON")
	create := fs.Bool("new", false, "create a project and print its id")
	label := fs.String("label", "", "display name for --new (defaults to the date and time)")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	attachBridge(ctx, a)

	if err := bootstrap(ctx, a); err != nil {
		return fail(err)
	}

	if *create {
		project, err := a.Engine.CreateProject(ctx, *label)
		if err != nil {
			return fail(err)
		}
		if *asJSON {
			printJSON(project)
			return 0
		}
		fmt.Println()
		fmt.Println("  flow-go — project created")
		fmt.Println("  " + strings.Repeat("-", 52))
		fmt.Printf("  %-24s %s\n", "project", project.ID)
		if project.Label != "" {
			fmt.Printf("  %-24s %s\n", "label", project.Label)
		}
		fmt.Println()
		fmt.Println("  Use it with --project-id, or set FLOW_PROJECT_ID.")
		fmt.Println()
		return 0
	}

	projects, err := a.Engine.ListProjects(ctx)
	if err != nil {
		return fail(err)
	}

	if *asJSON {
		printJSON(projects)
		return 0
	}

	fmt.Println()
	fmt.Println("  flow-go — projects")
	fmt.Println("  " + strings.Repeat("-", 52))
	if len(projects) == 0 {
		fmt.Println("  no projects on this account")
		fmt.Println()
		fmt.Println("  Create one with: flow-go projects --new")
		fmt.Println()
		return 0
	}
	for i, project := range projects {
		marker := " "
		if i == 0 {
			// The listing is most-recently-modified first, and this is the one a
			// run would pick when it has not been told which to use.
			marker = "*"
		}
		modified := "unknown"
		if !project.Modified.IsZero() {
			modified = project.Modified.Format(time.RFC3339)
		}
		fmt.Printf("  %s %-40s %-18s %s\n", marker, project.ID, project.Label, modified)
	}
	fmt.Println()
	fmt.Println("  * is the project a run picks when none is named.")
	fmt.Println("    Pin one with --project-id or FLOW_PROJECT_ID.")
	fmt.Println()
	return 0
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
 * batch-all
 * ------------------------------------------------------------------ */

// runBatchAll generates one image per account, one account at a time.
//
// Deliberately sequential, and deliberately one engine per account. These files
// do not share a session — each `account_<key>.json` is a different signed-in
// identity — so a single engine would have its cookies replaced by whichever
// account booted last, and every image after the first would silently belong to
// the wrong one. Opening an engine per file is what keeps them apart, and it is
// also what makes the run forgiving: one account failing leaves the rest alone.
//
// The per-account plumbing is `--cookies`, which is why this needs no new
// generation path — each iteration is an ordinary single-account run.
func runBatchAll(args []string) int {
	fs := flag.NewFlagSet("batch-all", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	prompt := fs.String("prompt", "", "prompt to generate (required)")
	model := fs.String("model", "narwhal", "image model: narwhal|lite|pro")
	download := fs.Bool("download", true, "write the results to output/")
	_ = fs.Parse(args)

	if strings.TrimSpace(*prompt) == "" {
		fmt.Fprintln(os.Stderr, "batch-all: --prompt is required")
		return 2
	}

	paths, err := engine.DiscoverAccountJars(config.CookieDir())
	if err != nil {
		return fail(fmt.Errorf("batch-all: list account cookie files: %w", err))
	}
	if len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "batch-all: no account_*.json in %s — run `flow-go bridge` "+
			"with the extension attached so it writes them\n", config.CookieDir())
		return 1
	}

	ctx := context.Background()
	fmt.Printf("\n  flow-go — batch-all over %d account(s)\n", len(paths))
	fmt.Println("  " + strings.Repeat("-", 52))

	var ok, skipped, failed int
	for i, path := range paths {
		fmt.Printf("  [%d/%d] %s\n", i+1, len(paths), filepath.Base(path))

		// Checked here rather than left to the engine: a file with no credential
		// cookie has no session to act as, and the failure it produces upstream
		// reads like a server fault rather than like an unusable input.
		jar, loadErr := cookiejar.LoadFile(path)
		if loadErr != nil {
			fmt.Printf("        skipped: %v\n", loadErr)
			skipped++
			continue
		}
		if !jar.HasAuthCookies() {
			fmt.Printf("        skipped: no credential cookies in the file\n")
			skipped++
			continue
		}

		// Then the rest of what a run needs, which lives on the bundle. The two
		// checks are split rather than folded into one `Complete()` because they
		// say different things: a file with no credential cookie is a different
		// mistake from one whose sync never had a Flow tab open, and an operator
		// acts on them differently.
		//
		// Cookies alone cannot generate. Without the page tokens batchexecute has
		// to prime for a token, and without the identity a captcha token is
		// rejected with no error at all — both of which surface upstream as a
		// server fault rather than as an unusable input. Skipping here names the
		// real cause before a credit is spent on discovering it.
		bundle, bundleErr := cookiejar.LoadBundleFile(path)
		if bundleErr != nil {
			fmt.Printf("        skipped: %v\n", bundleErr)
			skipped++
			continue
		}
		if !bundle.Complete() {
			fmt.Printf("        skipped: bundle incomplete (missing tokens or fingerprint)\n")
			skipped++
			continue
		}

		perAccount := common
		perAccount.cookies = path

		a, buildErr := perAccount.build()
		if buildErr != nil {
			fmt.Printf("        failed to open: %v\n", buildErr)
			failed++
			continue
		}

		if bootErr := bootstrap(ctx, a); bootErr != nil {
			fmt.Printf("        bootstrap failed: %v\n", bootErr)
			_ = a.Close()
			failed++
			continue
		}

		outcome, genErr := a.Engine.GenerateImageViaBatch(ctx, engine.BatchImageRequest{
			Prompt:   *prompt,
			Model:    *model,
			Download: *download,
		})
		_ = a.Close()

		if genErr != nil {
			fmt.Printf("        FAILED: %v\n", genErr)
			failed++
			continue
		}

		ok++
		if len(outcome.Files) == 0 {
			fmt.Printf("        SUCCESS but nothing was downloaded (account %s)\n", outcome.AccountID)
			continue
		}
		for _, file := range outcome.Files {
			fmt.Printf("        SUCCESS -> %s (account %s)\n", file.Path, outcome.AccountID)
		}
	}

	fmt.Println()
	fmt.Printf("  %d succeeded, %d skipped, %d failed\n\n", ok, skipped, failed)
	if failed > 0 {
		return 1
	}
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
	// reports the jar a run would actually use. The list lives in doctor.go now
	// that two commands depend on it; keeping a second copy here is how the two
	// would come to disagree.
	candidates := cookieCandidates()

	// An explicit --cookies names the file, so this reports that one rather than
	// whatever the default search would have found. Without this the flag looked
	// honoured on every command except the one whose whole job is reading a jar.
	var path string
	var jar *cookiejar.Jar
	if common.cookies != "" {
		path = common.cookies
		jar, _ = cookiejar.LoadFile(path)
		if jar == nil {
			fmt.Printf("  could not read --cookies %s\n", path)
			fmt.Println()
			return 1
		}
	} else {
		path, jar, _ = loadCookieJar()
	}

	if path == "" {
		fmt.Printf("  no cookies at %s\n", strings.Join(candidates, " or "))
		fmt.Println()
		fmt.Println("  Options:")
		fmt.Println("    1. Load extension/ in the browser and run `flow-go bridge`; the")
		fmt.Println("       extension hands over cookies and the bridge writes")
		fmt.Println("       cookies/account_<key>.json.")
		fmt.Println("    2. Write a JSON array of cookies to one of those paths yourself.")
		fmt.Println()
		fmt.Println("  `flow-go doctor` checks this and everything downstream of it.")
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
