// Package cli implements the flow-go command line.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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

	closeLog := initUnifiedLogging()
	if closeLog != nil {
		defer closeLog()
	}

	if len(args) == 0 {
		fmt.Print(usage())
		return 0
	}

	log.Printf("cli: run %s", strings.Join(args, " "))

	switch args[0] {
	case "bridge", "serve", "server":
		return runBridge(args[1:])
	case "generate", "video":
		return runGenerate(args[1:])
	case "image":
		return runImage(args[1:])
	case "edit":
		return runEdit(args[1:])
	case "upload-video", "upload":
		return runUploadVideo(args[1:])
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
  edit                  Edit an existing video
  upload-video, upload  Upload a local video and print its media ID
  batch-all             Generate one image per account in cookies/account_*.json
  projects              List the account's Flow projects
  stats                 Print database statistics
  export [file]         Export statistics as JSON
  cookies               Show cookie and credential status
  version               Print the version

COMMON FLAGS
  --project-id          Flow project ID (default: resolved from the session)
  --proxy               Route upstream traffic through one exit IP
                        (default: $FLOW_PROXY)
  --captcha             reCAPTCHA strategy: auto | broker | http | off
                        (default: $FLOW_RECAPTCHA, else auto)
  --db                  Database path
  --cookies             Run as one named account, e.g.
                        cookies/account_<key>.json. Without it the freshest
                        account file in cookies/ is used.

  A .env file is read at start-up and its values become these flags' defaults,
  so an explicit flag always wins over the file. See .env.example for the whole
  set, including the rate limits and the timing bounds.

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
  --prompt              Prompt text (required, or give it as a positional
                        argument)
  --duration            4 | 6 | 8 | 10                 (default 10)
  --quality             360p | 720p                    (default 720p)
  --count               1..4                           (default 1)
  --start-image         Local image path or media ID; makes it image-to-video
  --end-image           Local image path or media ID
  --reference           Reference image, repeatable; makes it
                        reference-to-video and selects the reference model
  --aspect              landscape | 16:9 | portrait | 9:16 (default landscape)
  --no-download         Skip writing the result to output/

  --resolution and --seed exist on the legacy aisandbox transport and not on
  the batchexecute one. Passing one is reported on stderr and the run continues
  without it, so a script carrying a stale flag still produces its video.

  SHORTHAND
  The options above may also be given as tokens after the prompt, in any order:
    flow-go generate "cyberpunk car in neon rain" 9:16 8s 720p x2
    9:16 | 16:9 | portrait | landscape    aspect ratio
    4s | 6s | 8s | 10s                    duration
    360p | 720p                           quality
    x1..x4 | 1x..4x                       count
  Each token must be its own argument, and only the tail is read — a quoted
  prompt is one argument, so "a study in 4:3" is left alone. A token that
  contradicts a flag is an error, not a silent preference.

IMAGE FLAGS
  --prompt              Prompt text (required, or give it as a positional
                        argument)
  --model               harbor_seal | narwhal | gem_pix_2
                        (default: $IMAGE_MODEL, else narwhal)
  --count               Accepted, but only 1 is honoured: the RPC returns one
                        asset per call
  --aspect              landscape | 16:9 | portrait | 9:16 | square | 1:1 |
                        4:3 | 3:4                     (default landscape)
  --no-download         Skip writing the result to output/

  SHORTHAND
  The options above may also be given as tokens after the prompt, in any order:
    flow-go image "cute origami owl" 1:1 x2
    1:1 | 16:9 | 9:16 | 4:3 | 3:4         aspect ratio
    x1..x4 | 1x..4x                       count
  Duration and quality are not image options, so "8s" and "720p" stay in the
  prompt here.

EDIT FLAGS
  --source              The asset to edit: a media ID, a content ID, or a path
                        to a local video. It may be given positionally instead —
                        as a first argument that is a file, or as the first of
                        two arguments, the second being the prompt.
  --prompt              What to change (required, or give it as a positional
                        argument)
  --model               Defaults to abra_edit
  --no-download         Skip writing the result to output/
  --force-upload        Upload the file again even if it is already in this
                        project

  A local video is uploaded first, by a two-step Google Cloud Storage resumable
  upload, and the media ID it returns is what the edit runs on. That is the only
  way a local file reaches a project: the image path carries its bytes inside a
  batchexecute payload, and video has no equivalent there.

  A file already in this project is reused rather than sent again, matched on
  SHA-256 of its contents — so editing one video repeatedly costs a single
  upload. See UPLOAD-VIDEO below for what that cache does and does not promise.

  An edit cannot move accounts: its source is an asset inside the current
  account's project, so a switch would take the project with it.

UPLOAD-VIDEO
  flow-go upload-video <file.mp4>     mp4, m4v, mov, webm or mkv
  flow-go upload <file.mp4>           the same command
  --force-upload                      send the file again even if it is already
                                      in this project

  Prints the media ID, which is what edit --source takes.

  A file already uploaded into this project is reused rather than sent again,
  matched on SHA-256 of its contents rather than on its path — so editing one
  video repeatedly costs a single upload, and editing a file in place sends the
  new bytes instead of reusing the old media ID. A hit is never re-checked, so if
  the media ID has since gone — the project was cleared, the asset removed —
  --force-upload is the way past it.
EXAMPLES
  flow-go bridge
  flow-go image "a single red paper boat"
  flow-go image --prompt "a red cube" --cookies cookies/account_ab12cd34ef56.json
  flow-go generate "a paper boat on a river" --duration 8
  flow-go generate "slow push in" --start-image ./frame.png --quality 360p
  flow-go generate "keep the style" --reference ./style-a.png --reference ./style-b.png
  flow-go generate "cyberpunk car in neon rain" 9:16 8s 720p x2
  flow-go generate "golden sunset over mountains" 4s 360p x2
  flow-go image "cute origami owl" 1:1 x2
  flow-go upload-video clip.mp4
  flow-go edit clip.mp4 "make it a cybernetic neon city"
  flow-go edit --source <content-id> "make the boat drift slowly to the left"
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
	// The two env-backed defaults below are why this file has to be read after
	// config.LoadEnv: a flag's default is the value the flag takes when it is
	// not passed, so binding the environment into the default is what makes
	// `.env` work at all. An explicit flag still wins, which is the point —
	// `FLOW_PROXY` sets the baseline and `--proxy` overrides it for one run.
	//
	// Both were documented in .env.example and read by nothing before this, so
	// a value set there was silently ignored.
	fs.StringVar(&c.proxy, "proxy", envDefault("FLOW_PROXY", ""),
		"proxy URL for upstream traffic (from $FLOW_PROXY)")
	fs.StringVar(&c.captcha, "captcha", envDefault("FLOW_RECAPTCHA", "auto"),
		"reCAPTCHA strategy: auto|broker|http|off (from $FLOW_RECAPTCHA, else auto)")
	fs.StringVar(&c.db, "db", "", "database path")
	fs.StringVar(&c.email, "email", "", "account email, used as the OAuth login hint")
	fs.StringVar(&c.cookies, "cookies", "",
		"cookie file to run as, e.g. cookies/account_<key>.json (default: the usual search)")
}

// envDefault returns the environment variable's value, or fallback when it is
// unset or empty.
//
// Empty counts as unset deliberately. `FLOW_PROXY=` in a .env file is how that
// file says "not configured", and treating it as a real value would set the
// flag's default to the empty string — which happens to be the same thing here,
// but the rule matters for a knob whose fallback is not empty, such as
// FLOW_RECAPTCHA and its `auto`. Without it, an empty assignment would silently
// disable the strategy rather than leave the default in place.
func envDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
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
	// --resolution and --seed exist on the legacy aisandbox transport and not
	// on the batchexecute one, which is the only one that works. They are still
	// declared, and still named when passed — but the run continues without
	// them. Failing was defensible only while the alternative was silence, and
	// the caller's next move was always to drop the flag and retry, which is a
	// move this command can make for them.
	//
	// --aspect is no longer in that group. The video payload has an aspect slot
	// and it is wired, so passing this now changes the render rather than being
	// reported and dropped.
	aspect := fs.String("aspect", "", "landscape | 16:9 | portrait | 9:16")
	resolution := fs.String("resolution", "", "ignored — not implemented on the batchexecute transport")
	seed := fs.Int64("seed", 0, "ignored — not implemented on the batchexecute transport")
	// --reference is a real flag now. It used to be declared purely so that
	// passing it could be reported as ignored, even though the engine has had a
	// working reference-to-video path (MZZa6b / abra_r2v_*) all along — the
	// command simply never called it.
	var references stringList
	fs.Var(&references, "reference",
		"reference image path or media ID; repeatable, and selects reference-to-video")
	// Flags and the positional prompt may be interleaved, so
	// `flow-go generate "a paper boat" --duration 8` applies both rather than
	// folding the flag into the prompt.
	positional := parseInterspersed(fs, args)

	// Trailing shorthand — `"…" 9:16 8s 720p x2` — is peeled off before the
	// prompt is assembled, so none of it can end up inside the prompt text.
	opts, positional, err := extractTrailingOptions(*prompt, positional, videoShorthand)
	if err != nil {
		return fail(err)
	}

	// The prompt may be given positionally, so `flow-go generate "a paper boat"`
	// works.
	*prompt = promptFromArgs(*prompt, positional)
	if strings.TrimSpace(*prompt) == "" {
		return fail(fmt.Errorf("--prompt is required (or give the prompt as an argument)"))
	}
	if err := mergeShorthand(fs, opts, duration, quality, count, aspect,
		config.VideoAspectValue); err != nil {
		return fail(err)
	}
	// Refused here, before the engine is built, so a typo costs nothing: an
	// aspect the server does not recognise is accepted and renders nothing, so
	// the alternative is discovering it from an empty result minutes later.
	if strings.TrimSpace(*aspect) != "" {
		if _, ok := config.VideoAspectValue(*aspect); !ok {
			return fail(fmt.Errorf("--aspect %q is not a video aspect; use one of %s",
				*aspect, strings.Join(config.VideoAspectNames, ", ")))
		}
	}
	warnIgnoredFlags(ignoredGenerateFlags(*resolution, *seed))

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

	// Reference-to-video is a different capability from conditioning on a start
	// frame — a different RPC and a different payload shape — so it is selected
	// by supplying references rather than combined with them. A submission
	// carrying both would go to one RPC with the other's conditioning in slots
	// that RPC does not read, and the server accepts that and answers with
	// nothing rather than an error.
	if len(references) > 0 {
		if *startImage != "" || *endImage != "" {
			return fail(fmt.Errorf("--reference cannot be combined with --start-image or " +
				"--end-image: reference-to-video and frame conditioning are different " +
				"submissions"))
		}
		if *quality != "720p" {
			// The reference model family has no quality axis wired up — the keys
			// are abra_r2v_<n>s, with no _360p variant in use — so there is
			// nothing to apply the flag to. Reported rather than silently
			// dropped, which is the convention every other unimplemented field
			// follows here.
			warnIgnoredFlags([]string{"--quality (reference-to-video has no quality variant)"})
		}
		if strings.TrimSpace(*aspect) != "" {
			// The reference payload is not the text-to-video layout the aspect
			// slot belongs to: it puts the prompt at index 0 and the reference
			// images at index 1, where the text-to-video one puts them last. So
			// there is no verified slot to write. Reported rather than silently
			// dropped — the flag is applied on the text-to-video path, which
			// makes a silent drop here the more surprising of the two.
			warnIgnoredFlags([]string{"--aspect (reference-to-video has no aspect slot)"})
		}
		return runReferenceGenerate(ctx, a, *prompt, references, *duration, !*noDownload)
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
		Aspect:     *aspect,
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

// runReferenceGenerate submits a reference-to-video generation.
//
// Split from runGenerate because the two submissions share almost nothing: a
// different engine entry point, a different model family, and no --count — the
// reference RPC submits one render per call.
func runReferenceGenerate(ctx context.Context, a *app.App, prompt string,
	references []string, duration int, download bool) int {

	fmt.Printf("generating from %d reference image(s) at %ds\n", len(references), duration)

	outcome, err := a.Engine.GenerateVideoFromReferencesViaBatch(ctx, engine.BatchReferenceRequest{
		Prompt:     prompt,
		References: references,
		Duration:   duration,
		Wait:       download,
		Download:   download,
	})
	if err != nil {
		return fail(err)
	}
	printJSON(outcome)
	return 0
}

/* ------------------------------------------------------------------ *
 * edit
 * ------------------------------------------------------------------ */

// runEdit edits an existing asset with abra_edit.
//
// The source may be an asset id — a media id or a content id, which the engine
// resolves to the content id the RPC wants — or a path to a local video, which is
// uploaded first. Both forms take the same flag, because which one you have is
// not something the caller should have to say.
func runEdit(args []string) int {
	fs := flag.NewFlagSet("edit", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	source := fs.String("source", "",
		"the asset to edit: a media ID, a content ID, or a path to a local video")
	prompt := fs.String("prompt", "", "what to change")
	model := fs.String("model", "", "edit model (default abra_edit)")
	noDownload := fs.Bool("no-download", false, "skip writing the result to disk")
	forceUpload := fs.Bool("force-upload", false,
		"upload the file again even if it is already in this project")
	// Flags and the two positional arguments may be interleaved, so
	// `flow-go edit clip.mp4 "make it neon" --force-upload` applies all three.
	positional := parseInterspersed(fs, args)

	// Both the source and the prompt may be given positionally, so
	// `flow-go edit clip.mp4 "make it neon"` reads the way it should.
	*source, *prompt = resolveEditArgs(*source, *prompt, positional)

	if strings.TrimSpace(*source) == "" {
		return fail(fmt.Errorf("--source is required: a media ID, a content ID, or a path to a " +
			"local video (given positionally or with --source)"))
	}

	// A source that is neither a file nor something the RPC can address is
	// reported here, where the reason is still visible. Passing it through would
	// fail later as "not in the project listing", which names the symptom and
	// sends the reader looking at their project rather than at their argument.
	if !isLocalFile(*source) {
		if strings.HasPrefix(*source, "http://") || strings.HasPrefix(*source, "https://") {
			return fail(fmt.Errorf("--source takes a media ID, a content ID, or a local video "+
				"path, and %s is a URL — download it first", *source))
		}
		if looksLikePath(*source) {
			return fail(fmt.Errorf("%s is not a file; check the path, or pass a media ID or a "+
				"content ID", *source))
		}
	}
	if strings.TrimSpace(*prompt) == "" {
		return fail(fmt.Errorf("--prompt is required (or give the prompt as an argument)"))
	}

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	attachBridge(ctx, a)

	if err := bootstrap(ctx, a); err != nil {
		return fail(err)
	}

	// A local file is uploaded first; anything else is already an id and is
	// passed through. The test is on the source, not on the flag it came from, so
	// `--source clip.mp4` and a positional `clip.mp4` behave identically.
	editSource := *source
	if isLocalFile(editSource) {
		info, statErr := os.Stat(editSource)
		if statErr != nil {
			return fail(fmt.Errorf("read %s: %w", editSource, statErr))
		}
		// "Resolving" rather than "uploading", because it may not upload: a file
		// already in this project is reused from the cache, and a line promising
		// an upload followed by a line saying there wasn't one reads as a bug.
		fmt.Printf("resolving %s (%.1f MB) to a project media ID...\n", filepath.Base(editSource),
			float64(info.Size())/(1024*1024))

		uploaded, uploadErr := a.Engine.UploadVideo(ctx, editSource,
			engine.UploadOptions{Force: *forceUpload})
		if uploadErr != nil {
			return fail(uploadErr)
		}
		if uploaded.Cached {
			fmt.Printf("  already in this project — reusing media %s\n", uploaded.MediaID)
		} else {
			fmt.Printf("  uploaded as media %s\n", uploaded.MediaID)
		}
		editSource = uploaded.MediaID
	}

	fmt.Printf("editing %s\n", short(editSource))

	outcome, err := a.Engine.EditVideoViaBatch(ctx, engine.BatchEditRequest{
		Source:   editSource,
		Prompt:   *prompt,
		Model:    *model,
		Wait:     !*noDownload,
		Download: !*noDownload,
	})
	if err != nil {
		return fail(err)
	}
	printJSON(outcome)
	return 0
}

// resolveEditArgs decides which positional argument is the source and which is
// the prompt.
//
// Two signals, and they cover different cases. A first argument that is a file
// that exists is a source whatever else is on the line, which is what keeps a
// one-word prompt unambiguous — a prompt is not a path to something on disk.
// Failing that, two or more arguments with nothing named is source-then-prompt,
// because a prompt is one argument in practice: that is what lets a media id be
// given positionally without `--source`.
//
// `--source` short-circuits both: a caller who named it has made a decision, so
// every positional argument is the prompt.
//
// Split out from the command so the rule can be exercised directly. Running it
// through `Run` reaches the bridge, which waits five seconds to become its host
// before failing — five seconds per case for a decision that touches no network.
func resolveEditArgs(source, prompt string, positional []string) (string, string) {
	if strings.TrimSpace(source) == "" && len(positional) > 0 {
		switch {
		case isLocalFile(positional[0]):
			source = positional[0]
			positional = positional[1:]
		case len(positional) >= 2:
			source = positional[0]
			positional = positional[1:]
		}
	}
	return source, promptFromArgs(prompt, positional)
}

// isLocalFile reports whether a command-line argument names a file that exists.
//
// Directories are not files for this purpose: `flow-go edit ./clips "make it
// neon"` should treat `./clips` as a prompt-shaped argument rather than as a
// source, and the upload would refuse a directory anyway.
func isLocalFile(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	info, err := os.Stat(value)
	return err == nil && !info.IsDir()
}

// looksLikePath reports whether a source argument is shaped like a filesystem
// path rather than an asset id.
//
// Only a separator is decisive, and that is deliberate. An id is a uuid or a hex
// token with no separator in it, so seeing one settles it; guessing from a file
// extension would mean duplicating the video-extension list here, and a bare
// missing filename reading as an id is a smaller failure than a real id being
// refused.
func looksLikePath(value string) bool {
	return strings.ContainsAny(value, `/\`)
}

/* ------------------------------------------------------------------ *
 * upload-video
 * ------------------------------------------------------------------ */

// runUploadVideo puts a local video into the account's Flow project and prints
// the media id.
//
// It exists because the upload and the edit are separable: a video uploaded once
// can be edited repeatedly, and re-uploading it for every attempt would be slow
// and would leave a copy of the file in the project each time. That reasoning is
// what the upload cache now enforces for `edit` too, so this command is mostly a
// way to get the id on its own.
func runUploadVideo(args []string) int {
	fs := flag.NewFlagSet("upload-video", flag.ExitOnError)
	var common commonFlags
	common.bind(fs)
	forceUpload := fs.Bool("force-upload", false,
		"upload again even if this file is already in the project")
	_ = fs.Parse(args)

	positional := fs.Args()
	if len(positional) == 0 {
		return fail(fmt.Errorf("a video file is required: flow-go upload-video <file.mp4>"))
	}
	if len(positional) > 1 {
		return fail(fmt.Errorf("upload-video takes one file, and got %d", len(positional)))
	}
	path := positional[0]

	a, err := common.build()
	if err != nil {
		return fail(err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	attachBridge(ctx, a)

	if err := bootstrap(ctx, a); err != nil {
		return fail(err)
	}

	if info, statErr := os.Stat(path); statErr == nil {
		fmt.Printf("resolving %s (%.1f MB) to a project media ID...\n", filepath.Base(path),
			float64(info.Size())/(1024*1024))
	}

	result, err := a.Engine.UploadVideo(ctx, path, engine.UploadOptions{Force: *forceUpload})
	if err != nil {
		return fail(err)
	}
	if result.Cached {
		fmt.Printf("  already in this project — reusing media %s\n", result.MediaID)
	}
	printJSON(result)
	return 0
}

// ignoredGenerateFlags names the generate flags the batchexecute transport does
// not apply, so the caller is told rather than left to notice the result differs
// from what they asked for.
//
// --reference and --aspect are deliberately absent: both are applied now, the
// first by routing to the reference-to-video submission rather than to the
// frame-conditioned one, the second by the video payload's aspect slot.
func ignoredGenerateFlags(resolution string, seed int64) []string {
	var out []string
	if strings.TrimSpace(resolution) != "" {
		out = append(out, "--resolution")
	}
	if seed != 0 {
		out = append(out, "--seed")
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

// promptFromArgs resolves a prompt from the flag and any positional arguments,
// so `flow-go image "a red boat"` works as well as `--prompt "a red boat"`.
//
// An explicit --prompt wins when both are supplied. The positional form is a
// convenience and a caller who names the flag has made a decision; silently
// preferring the trailing words would make the flag unreliable.
//
// The positional words are joined with single spaces because the shell has
// already split the prompt into them, and the server sees one string either way.
func promptFromArgs(flagValue string, rest []string) string {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	return strings.Join(rest, " ")
}

// parseInterspersed parses args so that flags and positional arguments may be
// interleaved, and returns the positional arguments in the order they appeared.
//
// The standard flag package stops at the first non-flag argument. That makes
// `generate "a paper boat" --duration 8` parse no flags at all and then hand
// "--duration 8" to the prompt — so the duration is ignored *and* the prompt is
// wrong, silently, with a render as the only feedback. Every example in this
// command's own help text is written in that order, and it is the order anyone
// types.
//
// This walks the remainder instead: parse, take one positional, parse what is
// left, until nothing remains. Flags keep last-wins semantics, and a repeatable
// flag such as --reference accumulates exactly as it does when given up front.
//
// A bare "--" still ends flag parsing, so a caller can pass something that looks
// like a flag; everything after it is positional.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	head, tail := args, []string(nil)
	for i, arg := range args {
		if arg == "--" {
			head, tail = args[:i], args[i+1:]
			break
		}
	}

	var positional []string
	rest := head
	for {
		if err := fs.Parse(rest); err != nil {
			break
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		// Parse stopped here, so this argument is positional. Take it and keep
		// walking, so the flags after it are still read.
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	return append(positional, tail...)
}

/* ------------------------------------------------------------------ *
 * trailing shorthand
 *
 * `flow-go generate "a cyberpunk warrior" 9:16 8s 720p x2` is how a person
 * writes the command. These helpers read those tokens off the end of the
 * argument list, before the prompt is assembled, so none of them can end up
 * inside the prompt text.
 * ------------------------------------------------------------------ */

// tokenKind says which option a shorthand token sets.
type tokenKind int

const (
	tokenAspect tokenKind = iota
	tokenDuration
	tokenQuality
	tokenCount
)

// shorthandToken is one recognised trailing token.
type shorthandToken struct {
	Kind     tokenKind
	Aspect   string // as spelled, e.g. "9:16"
	Duration int
	Quality  string
	Count    int
}

// trailingOptions is what the trailing tokens resolved to. A zero field means no
// token supplied it, which is unambiguous here because no token carries a zero:
// durations are 4..10 and counts are 1..4.
type trailingOptions struct {
	Aspect   string
	Duration int
	Quality  string
	Count    int
}

// shorthandSpec says which token classes a command accepts.
//
// Duration and quality are video only. On the image command "8s" and "720p" are
// plausible endings for a prompt, and reading them as options would cut the
// prompt and change the request. Aspect and count apply to both.
type shorthandSpec struct {
	Duration     bool
	Quality      bool
	Count        bool
	AspectLookup func(string) (int, bool)
}

// videoShorthand and imageShorthand are the two token sets. They differ in the
// classes and in the aspect table, which is what keeps `3:4` an image aspect and
// a prompt word on the video path.
var (
	videoShorthand = shorthandSpec{
		Duration:     true,
		Quality:      true,
		Count:        true,
		AspectLookup: config.VideoAspectValue,
	}
	imageShorthand = shorthandSpec{
		Count:        true,
		AspectLookup: config.ImageAspectValue,
	}
)

// videoQualities are the quality tokens the shorthand accepts.
//
// A literal, because the parser has to match a token exactly and
// NormalizeVideoQuality cannot: it answers "720p" for anything it does not
// recognise, so it cannot tell a token from a word. A test keeps this list and
// config.VideoCosts from drifting apart.
var videoQualities = []string{"360p", "720p"}

// extractTrailingOptions peels recognised shorthand tokens off the END of the
// positional arguments, and reports both what they set and what is left as the
// prompt.
//
// Only the tail is peeled: the first argument from the end that is not a token
// ends the scan, so `flow-go generate "a paper boat" 8s tomorrow` peels nothing
// rather than reaching past "tomorrow" for the "8s". A prompt is never picked
// apart from the middle.
//
// At least one positional stays behind as the prompt unless --prompt names it,
// so `flow-go generate 8s` keeps "8s" as the prompt rather than leaving nothing
// to generate — the same rule the ratio shorthand already followed.
func extractTrailingOptions(promptFlag string, positional []string, spec shorthandSpec) (trailingOptions, []string, error) {
	minKeep := 1
	if strings.TrimSpace(promptFlag) != "" {
		minKeep = 0
	}

	var opts trailingOptions
	end := len(positional)
	for end > minKeep {
		token, ok := classifyShorthand(positional[end-1], spec)
		if !ok {
			break
		}
		if err := opts.apply(token, spec); err != nil {
			return trailingOptions{}, positional, err
		}
		end--
	}
	return opts, positional[:end], nil
}

// classifyShorthand reports what a positional argument means as a shorthand
// token, if anything.
//
// The match is exact and on the whole token. That is the property the whole
// feature rests on: a prompt is normally one quoted string, so
// `flow-go image "a study in 4:3"` is a single argument that matches nothing,
// while a token standing alone is read as an option. A substring or suffix match
// would eat the tail of that prompt instead.
func classifyShorthand(token string, spec shorthandSpec) (shorthandToken, bool) {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return shorthandToken{}, false
	}

	if spec.AspectLookup != nil {
		if _, ok := spec.AspectLookup(trimmed); ok {
			return shorthandToken{Kind: tokenAspect, Aspect: trimmed}, true
		}
	}
	if spec.Duration {
		if seconds, ok := parseDurationToken(trimmed); ok {
			return shorthandToken{Kind: tokenDuration, Duration: seconds}, true
		}
	}
	if spec.Quality {
		if quality, ok := parseQualityToken(trimmed); ok {
			return shorthandToken{Kind: tokenQuality, Quality: quality}, true
		}
	}
	if spec.Count {
		if n, ok := parseCountToken(trimmed); ok {
			return shorthandToken{Kind: tokenCount, Count: n}, true
		}
	}
	return shorthandToken{}, false
}

// parseDurationToken reads "4s", "6s", "8s" and "10s", in any case.
//
// The accepted seconds come from config.Durations, so "12s" is not a duration
// token at all — it stays in the prompt rather than being refused as an
// unsupported length. A prompt may legitimately end in "12s", and refusing the
// whole run over it would make that prompt unusable.
func parseDurationToken(token string) (int, bool) {
	lower := strings.ToLower(strings.TrimSpace(token))
	if !strings.HasSuffix(lower, "s") {
		return 0, false
	}
	seconds, err := strconv.Atoi(strings.TrimSuffix(lower, "s"))
	if err != nil {
		return 0, false
	}
	for _, allowed := range config.Durations {
		if seconds == allowed {
			return seconds, true
		}
	}
	return 0, false
}

// parseQualityToken reads "360p" and "720p", in any case.
func parseQualityToken(token string) (string, bool) {
	for _, quality := range videoQualities {
		if strings.EqualFold(strings.TrimSpace(token), quality) {
			return quality, true
		}
	}
	return "", false
}

// parseCountToken reads "x2" and "2x", in any case, for 1..4.
//
// Both spellings are accepted because both get typed. A number outside 1..4 is
// not a count token, for the same reason "12s" is not a duration token: the
// prompt has to keep it.
func parseCountToken(token string) (int, bool) {
	lower := strings.ToLower(strings.TrimSpace(token))
	var digits string
	switch {
	case strings.HasPrefix(lower, "x"):
		digits = lower[1:]
	case strings.HasSuffix(lower, "x"):
		digits = lower[:len(lower)-1]
	default:
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || n > 4 {
		return 0, false
	}
	return n, true
}

// apply folds one token in, refusing a second token of the same class that
// contradicts the first.
//
// `8s 4s` is two instructions, not a preference order. Quietly honouring one of
// them would render something the caller did not ask for, which is the failure
// this codebase spends its time removing.
func (o *trailingOptions) apply(token shorthandToken, spec shorthandSpec) error {
	switch token.Kind {
	case tokenAspect:
		if o.Aspect != "" && !aspectsAgree(o.Aspect, token.Aspect, spec.AspectLookup) {
			return fmt.Errorf("the trailing %s and %s disagree; give one of them",
				o.Aspect, token.Aspect)
		}
		o.Aspect = token.Aspect
	case tokenDuration:
		if o.Duration != 0 && o.Duration != token.Duration {
			return fmt.Errorf("the trailing %ds and %ds disagree; give one of them",
				o.Duration, token.Duration)
		}
		o.Duration = token.Duration
	case tokenQuality:
		if o.Quality != "" && o.Quality != token.Quality {
			return fmt.Errorf("the trailing %s and %s disagree; give one of them",
				o.Quality, token.Quality)
		}
		o.Quality = token.Quality
	case tokenCount:
		if o.Count != 0 && o.Count != token.Count {
			return fmt.Errorf("the trailing x%d and x%d disagree; give one of them",
				o.Count, token.Count)
		}
		o.Count = token.Count
	}
	return nil
}

// aspectsAgree reports whether two aspect spellings name the same ratio.
//
// Compared by resolved value rather than as strings, so `--aspect 9:16` beside a
// trailing `portrait` is the same request written twice and not a contradiction.
func aspectsAgree(a, b string, lookup func(string) (int, bool)) bool {
	av, aok := lookup(a)
	bv, bok := lookup(b)
	return aok && bok && av == bv
}

// flagWasSet reports whether the caller named a flag, as opposed to it sitting
// at its default.
//
// The distinction is the reason this exists at all: --duration defaults to 10 and
// --quality to "720p", so a command carrying a trailing `4s 360p` would otherwise
// look like a contradiction with flags nobody passed, and every shorthand token
// would be refused.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// mergeShorthand folds the trailing shorthand into the flag values.
//
// A flag the caller set explicitly conflicts with a token that disagrees; a flag
// left at its default is simply not an instruction. Equal values are never an
// error — refusing `--duration 8 "…" 8s` would punish a caller for saying the
// same thing twice.
//
// A nil target is a flag this command does not have, and is skipped: the image
// command has no --duration or --quality, and its token set never fills them
// either.
func mergeShorthand(fs *flag.FlagSet, opts trailingOptions,
	duration *int, quality *string, count *int, aspect *string,
	lookup func(string) (int, bool)) error {

	if opts.Duration != 0 && duration != nil {
		if flagWasSet(fs, "duration") && *duration != opts.Duration {
			return fmt.Errorf("--duration %d and the trailing %ds disagree; give one of them",
				*duration, opts.Duration)
		}
		*duration = opts.Duration
	}
	if opts.Quality != "" && quality != nil {
		if flagWasSet(fs, "quality") && *quality != opts.Quality {
			return fmt.Errorf("--quality %s and the trailing %s disagree; give one of them",
				*quality, opts.Quality)
		}
		*quality = opts.Quality
	}
	if opts.Count != 0 && count != nil {
		if flagWasSet(fs, "count") && *count != opts.Count {
			return fmt.Errorf("--count %d and the trailing x%d disagree; give one of them",
				*count, opts.Count)
		}
		*count = opts.Count
	}
	if opts.Aspect != "" && aspect != nil {
		if flagWasSet(fs, "aspect") {
			if !aspectsAgree(*aspect, opts.Aspect, lookup) {
				return fmt.Errorf("--aspect %s and the trailing %s disagree; give one of them, "+
					"or make them the same ratio", *aspect, opts.Aspect)
			}
			// They agree, and the flag keeps its spelling: it is the more
			// explicit of the two, so `--aspect 9:16 "…" portrait` should read
			// back as what the caller typed rather than as the alias. The other
			// three classes hold canonical values, so assigning is a no-op there
			// and they do not need the same care.
		} else {
			*aspect = opts.Aspect
		}
	}
	return nil
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
	model := fs.String("model", envDefault("IMAGE_MODEL", ""),
		"harbor_seal, narwhal, or gem_pix_2 (from $IMAGE_MODEL, else narwhal)")
	noDownload := fs.Bool("no-download", false, "skip writing the result to disk")
	// --seed is not implemented on the batchexecute transport; declared, and
	// named when passed, but the run continues without it.
	//
	// --aspect is applied now, at request[4] of the image payload — the slot a
	// hardcoded constant used to occupy, whose value turned out to be the
	// default aspect rather than a mode flag.
	aspect := fs.String("aspect", "", "landscape | 16:9 | portrait | 9:16 | square | 1:1 | 4:3 | 3:4")
	seed := fs.Int64("seed", 0, "ignored — not implemented on the batchexecute transport")
	// Flags and the positional prompt may be interleaved, so
	// `flow-go image "a red boat" --model narwhal` applies both.
	positional := parseInterspersed(fs, args)

	// Trailing shorthand — `flow-go image "an origami owl" 1:1 x2` — is peeled
	// off before the prompt is assembled, so none of it can end up inside the
	// prompt text. The image token set is narrower: no duration and no quality.
	opts, positional, err := extractTrailingOptions(*prompt, positional, imageShorthand)
	if err != nil {
		return fail(err)
	}

	// The prompt may be given positionally, so `flow-go image "a red boat"`
	// works.
	*prompt = promptFromArgs(*prompt, positional)
	if strings.TrimSpace(*prompt) == "" {
		return fail(fmt.Errorf("--prompt is required (or give the prompt as an argument)"))
	}
	if err := mergeShorthand(fs, opts, nil, nil, count, aspect,
		config.ImageAspectValue); err != nil {
		return fail(err)
	}
	// Validated here, before the engine is built, so a typo costs nothing. The
	// server does not reject an aspect it does not recognise — it accepts the
	// submission and renders nothing — so the alternative is diagnosing it from
	// an empty result minutes later.
	if strings.TrimSpace(*aspect) != "" {
		if _, ok := config.ImageAspectValue(*aspect); !ok {
			return fail(fmt.Errorf("--aspect %q is not an image aspect; use one of %s",
				*aspect, strings.Join(config.ImageAspectNames, ", ")))
		}
	}
	warnIgnoredFlags(ignoredGenerateFlags("", *seed))
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
		Aspect:   *aspect,
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
	model := fs.String("model", envDefault("IMAGE_MODEL", "narwhal"),
		"image model: narwhal|lite|pro (from $IMAGE_MODEL, else narwhal)")
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
	log.Printf("cli: error: %v", err)
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return 1
}

// initUnifiedLogging configures log to write to both stderr and the unified
// log file (data/flow.log), avoiding duplicate writes if stderr is already redirected.
func initUnifiedLogging() func() {
	logPath := config.LogPath()
	if logPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil
	}

	var w io.Writer = f
	stderrStat, err1 := os.Stderr.Stat()
	fileStat, err2 := f.Stat()
	if err1 == nil && err2 == nil && !os.SameFile(stderrStat, fileStat) {
		w = io.MultiWriter(os.Stderr, f)
	}

	log.SetOutput(w)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	return func() {
		_ = f.Sync()
		_ = f.Close()
	}
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
