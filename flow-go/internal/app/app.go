// Package app assembles the pieces into a running system.
package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/bridge"
	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/engine"
	"github.com/kodelyx/flow-go/flow-go/internal/flowapi"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
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
	"flow.google.com",
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
	// CookieFile names one cookie file to run as, instead of the usual search.
	// Empty means the engine looks for a live browser and then a persisted copy.
	CookieFile string
	// Jar is cookies the caller already holds in memory, which the engine uses
	// rather than putting them on disk. See engine.Options.Jar.
	Jar *cookiejar.Jar
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
	br := bridge.NewBridge(targets, domains, config.CookieDir())
	br.ListenAddr = fmt.Sprintf("127.0.0.1:%d", config.WSPort)

	eng, err := engine.New(st, br, engine.Options{
		ProjectID:    cfg.ProjectID,
		ProxyURL:     cfg.ProxyURL,
		CaptchaMode:  cfg.CaptchaMode,
		AccountIndex: config.AccountIndex,
		AtToken:      cfg.AtToken,
		Fsid:         cfg.Fsid,
		Fingerprint:  cfg.Fingerprint,
		CookieFile:   cfg.CookieFile,
		Jar:          cfg.Jar,
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

// clearPairingMarker removes what the bridge-token era left behind.
//
// The bridge is tokenless: it accepts any extension that connects, and there is
// no pairing step for a human to perform. An install upgrading from the token
// build still has `bridge-token` and `bridge-token.claimed` in the cookie
// directory, and neither is read any more — `Bridge.EnsureToken` is a no-op kept
// for compatibility. Deleting them at start-up is a migration, not a security
// step.
//
// It runs before the listener opens so that a stale marker is not the first thing
// a reader finds while debugging a connection.
func clearPairingMarker() {
	_ = os.Remove(filepath.Join(config.CookieDir(), "bridge-token.claimed"))
	_ = os.Remove(filepath.Join(config.CookieDir(), "bridge-token"))
}

// RunBridge starts the extension bridge and keeps it up until ctx is cancelled.
//
// **This is the whole of the process.** The WebSocket listener is its only
// surface: a connected Chrome profile is asked for its cookies, its page tokens
// and its browser identity, and the bridge writes them into
// `cookies/account_<key>.json` on the way in — `Bridge.SyncCookies` runs on every
// connection, so persistence does not depend on anything below. Every generation
// happens in a separate CLI run that reads one of those files, which is why there
// is no HTTP server here and nothing generates over the network.
//
// The engine is still bootstrapped once a browser shows up, and that is not a
// generation path: it is what registers the connected accounts and reads their
// credit balances, so `flow-go stats` has something current to report. Without it
// the accounts table would only move when an operator ran `flow-go credits`.
//
// One consequence of dropping the HTTP server worth knowing: `POST
// /api/sync-cookies` used to be the way to re-run that registration after a new
// profile connected. There is no replacement trigger, so a profile that attaches
// after this bootstrap writes its bundle (fine — that is the file every
// generation reads) but is not added to the accounts table until the next
// `flow-go credits` or a restart.
func (a *App) RunBridge(ctx context.Context) error {
	// Before the listener opens, because the pairing window is decided by
	// `EnsureToken` inside `Bridge.Listen` — clearing the marker afterwards would
	// leave the window shut for the whole run.
	clearPairingMarker()

	// The bridge runs for the whole process lifetime.
	bridgeErr := make(chan error, 1)
	go func() {
		if err := a.Bridge.Listen(ctx); err != nil {
			bridgeErr <- err
		}
	}()

	// Bootstrap once a browser shows up, in the background, so a slow or absent
	// browser never holds up the listener — which is the part that has to work.
	go func() {
		waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()

		if err := a.Bridge.WaitForExtension(waitCtx, 60*time.Second); err != nil {
			log.Printf("app: %v — listening anyway; a bundle is written when one connects", err)
		}

		bootCtx, bootCancel := context.WithTimeout(ctx, 60*time.Second)
		defer bootCancel()
		if err := a.Engine.Bootstrap(bootCtx); err != nil {
			log.Printf("app: engine bootstrap failed: %v", err)
			log.Printf("app: the bridge is still listening and still persisting bundles; " +
				"only the account and credit records are missing")
		}
	}()

	select {
	case err := <-bridgeErr:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		log.Println("app: shutting down")
	}
	return nil
}
