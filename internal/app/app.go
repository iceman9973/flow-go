// Package app assembles the pieces into a running system.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/kodelyx/cdp-control/bridge"
	"github.com/kodelyx/flow-go/internal/config"
	"github.com/kodelyx/flow-go/internal/engine"
	"github.com/kodelyx/flow-go/internal/flowapi"
	"github.com/kodelyx/flow-go/internal/server"
	"github.com/kodelyx/flow-go/internal/store"
)

// DefaultTargets is what the bridge is allowed to attach to, and the first entry
// is the URL it opens when it has no tab.
//
// Both hosts are listed because Labs redirects: `labs.google/fx/tools/flow` is
// the canonical entry point, but it lands on `flow.google.com`. An allowlist
// holding only the first entry means the extension opens a tab, watches it
// redirect out of scope, and then refuses to attach — which is exactly how this
// was found.
var DefaultTargets = []string{
	"https://labs.google/fx/tools/flow",
	"https://flow.google.com",
}

// DefaultCookieDomains is the cookie scope. It is deliberately the minimum that
// produces a working Labs session: the Labs host, the Google identity cookies it
// authenticates against, and the account chooser.
//
// `accounts.google.com` is there for one cookie, `ACCOUNT_CHOOSER`, which is the
// only place the browser records which Google accounts are signed in. Without it
// the engine can address `authuser=0` and nothing else, and every other signed-in
// account is invisible — which reads as "one account" rather than "one readable".
var DefaultCookieDomains = []string{
	"labs.google",
	"google.com",
	"accounts.google.com",
}

// Config configures an App.
type Config struct {
	ProjectID     string
	ProxyURL      string
	CaptchaMode   string
	Targets       []string
	CookieDomains []string
	DBPath        string
	// AtToken and Fsid are the page tokens to use when no browser is attached.
	// A process that has taken a SessionSnapshot from one that has a browser
	// supplies them; a live page always takes precedence.
	AtToken string
	Fsid    string
	// Fingerprint is the browser identity to present when no browser is attached,
	// taken from the same snapshot. A captcha-bearing call is checked against the
	// client its token was minted for, so a process without this presents a
	// generic profile and has its token silently discarded.
	Fingerprint *flowapi.BrowserFingerprint
}

// App holds the assembled system.
type App struct {
	Store  *store.Store
	Bridge *bridge.Bridge
	Engine *engine.Engine
}

// Build opens the database, creates the bridge, and constructs the engine. It
// does not start any listener and does not require cookies yet, so a CLI
// command that only reads statistics works with no browser present.
func Build(cfg Config) (*App, error) {
	if err := config.EnsureDirs(); err != nil {
		return nil, err
	}

	dbPath := cfg.DBPath
	if dbPath == "" {
		dbPath = config.DBPath()
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}

	targets := cfg.Targets
	if len(targets) == 0 {
		targets = DefaultTargets
	}
	domains := cfg.CookieDomains
	if len(domains) == 0 {
		domains = DefaultCookieDomains
	}

	// The bridge now owns where it keeps its files and where it listens; it used
	// to reach into this package's config for both, which is what kept it from
	// being a module of its own.
	br := bridge.NewBridge(targets, domains, config.DataDir())
	br.ListenAddr = fmt.Sprintf("127.0.0.1:%d", config.WSPort)

	eng, err := engine.New(st, br, engine.Options{
		ProjectID:    cfg.ProjectID,
		ProxyURL:     cfg.ProxyURL,
		CaptchaMode:  cfg.CaptchaMode,
		AccountIndex: config.AccountIndex,
		AtToken:      cfg.AtToken,
		Fsid:         cfg.Fsid,
		Fingerprint:  cfg.Fingerprint,
		// The env value is a default, not an instruction, so it ranks below the
		// browser. An explicit cfg.ProjectID still outranks both.
		DefaultProjectID: config.ProjectID,
	})
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	return &App{Store: st, Bridge: br, Engine: eng}, nil
}

// Close releases resources.
func (a *App) Close() error {
	if a.Store != nil {
		return a.Store.Close()
	}
	return nil
}

// Serve starts the extension bridge, brings the engine up, and runs the HTTP API
// until ctx is cancelled.
//
// Ordering matters: the bridge listener comes up first so the extension can
// connect and hand over cookies, and the engine is only bootstrapped once. If
// cookies are not available yet the engine reports "waiting for browser" rather
// than failing, and a later bootstrap can be triggered by POST /api/sync-cookies.
func (a *App) Serve(ctx context.Context, port int) error {
	// The bridge runs for the whole process lifetime.
	bridgeErr := make(chan error, 1)
	go func() {
		if err := a.Bridge.Listen(ctx); err != nil {
			bridgeErr <- err
		}
	}()

	// Bootstrap once a browser shows up, in the background, so a slow or absent
	// browser never blocks the API from starting.
	go func() {
		waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()

		if err := a.Bridge.WaitForExtension(waitCtx, 60*time.Second); err != nil {
			log.Printf("app: %v — starting without a live browser", err)
		}

		bootCtx, bootCancel := context.WithTimeout(ctx, 60*time.Second)
		defer bootCancel()
		if err := a.Engine.Bootstrap(bootCtx); err != nil {
			log.Printf("app: engine bootstrap failed: %v", err)
			log.Printf("app: the API is up but generation will return 503 until cookies are available")
			return
		}
	}()

	app := fiber.New(fiber.Config{
		AppName:   "flow-go",
		BodyLimit: 64 * 1024 * 1024,
	})
	server.RegisterRoutes(app, a.Engine, a.Bridge)

	shutdownErr := make(chan error, 1)
	go func() {
		<-ctx.Done()
		log.Println("app: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr <- app.ShutdownWithContext(shutdownCtx)
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	log.Printf("app: API listening on http://%s", addr)

	if err := app.Listen(addr); err != nil {
		if !errors.Is(err, os.ErrClosed) {
			return fmt.Errorf("app: server stopped: %w", err)
		}
	}

	select {
	case err := <-bridgeErr:
		if err != nil {
			return err
		}
	case err := <-shutdownErr:
		if err != nil {
			return err
		}
	default:
	}
	return nil
}
