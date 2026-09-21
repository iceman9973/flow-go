// Package engine orchestrates generation.
//
// It is the only place that combines the four moving parts:
//
//	bridge   — cookies and base information, from the browser
//	auth     — access tokens, minted in Go from those cookies
//	flowapi  — the upstream Flow calls
//	pool     — which account runs the job
//	store    — what happened, recorded for real
//
// Every generation goes through the pool, and every outcome is written to the
// database. Nothing here is decorative.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/kodelyx/Browser-cdp/cdp-control/bridge"
	"github.com/kodelyx/Browser-cdp/cdp-control/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/auth"
	"github.com/kodelyx/flow-go/flow-go/internal/batchexecute"
	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/flowapi"
	"github.com/kodelyx/flow-go/flow-go/internal/httpx"
	"github.com/kodelyx/flow-go/flow-go/internal/pool"
	"github.com/kodelyx/flow-go/flow-go/internal/recaptcha"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

// Options configures the engine.
type Options struct {
	// ProjectID is the Flow project to generate into. This is the caller's
	// explicit choice and outranks every other source. Empty means "work it
	// out", which asks the browser and then falls back to DefaultProjectID.
	ProjectID string
	// DefaultProjectID is used only when the caller named no project and the
	// browser could not supply one. It exists so a browserless run can skip
	// project discovery entirely: with a project id configured, the engine never
	// has to open an editor tab just to learn which project it is in.
	//
	// It deliberately ranks *below* the browser. A remembered id goes stale — a
	// project belongs to one signed-in account, and one from another account
	// does not open — so a live page is the better answer whenever there is one.
	DefaultProjectID string
	// ProxyURL routes all of this account's traffic through one exit IP.
	ProxyURL string
	// CaptchaMode selects the reCAPTCHA strategy: auto, broker, http, or off.
	CaptchaMode string
	// AccountID labels this account. Defaults to a hash of the cookie jar.
	AccountID string
	// AccountIndex is the signed-in Google account to act as, as an `authuser`
	// index. It only seeds a database that has never recorded a choice; once
	// /v1/accounts/switch has been used, the stored index wins. A negative value
	// is clamped to zero.
	AccountIndex int
	// AtToken and Fsid are the page tokens a browserless process cannot read for
	// itself. A caller that has taken a SessionSnapshot from a process with a
	// browser supplies them here, and they are used only when no bridge is
	// attached — a live page is always the better source.
	AtToken string
	Fsid    string
	// Fingerprint is the browser identity to present when no bridge is attached,
	// taken from a SessionSnapshot for the same reason as AtToken and Fsid.
	//
	// It is not decoration. A reCAPTCHA-bearing call is checked against the
	// client the assessment was made for, so a process that mints a token and
	// then presents a generic Chrome profile is rejected as unusual activity —
	// which reads as a captcha failure but is a fingerprint mismatch. Without
	// this, a browserless run can mint a perfectly good token and still have it
	// thrown away, and the only clue is an empty result.
	Fingerprint *flowapi.BrowserFingerprint
}

// Engine is the running system.
type Engine struct {
	store  *store.Store
	pool   *pool.Pool
	bridge *bridge.Bridge
	hc     *httpx.Client
	opts   Options

	mu        sync.RWMutex
	accountID string
	projectID string
	client    *flowapi.Client
	captcha   recaptcha.Provider
	// accountIndex is the signed-in Google account the engine acts as, sent as
	// `authuser` on every upstream call. Zero is the first signed-in account.
	accountIndex int
	// fingerprint is the browser identity adopted at start-up. Every upstream
	// call that carries a reCAPTCHA token must present it, or the assessment
	// does not line up with the request.
	fingerprint *flowapi.BrowserFingerprint
	// atToken is the page's anti-CSRF token, read from the browser once at
	// start-up and given to every batchexecute client. The browser sends it on
	// its first request; the engine cannot, and its fallback — a priming round
	// trip that expects the token back in a 400 — never fires when the server
	// answers 401 instead, so without this the token is never learned.
	atToken string
	// fsid is the page's `f.sid`, which the app sends on every batchexecute
	// request. Read once at start-up and inherited by every client, because a
	// call that omits it is a call the app would never have made.
	fsid string
	// hasSession records whether a Labs session was minted at boot. The bearer
	// path needs one and the cookie-authenticated path does not, so the engine
	// comes up either way; this says which of the two is actually available,
	// rather than leaving it to be discovered from a call that fails.
	hasSession bool
	ready      bool
	lastError  string
}

// BearerPathAvailable reports whether the bearer path has a credential.
//
// It is reported rather than left implicit because the engine boots without one
// on purpose: the cookie-authenticated path is the one that works against
// flow.google.com, and refusing to boot over a credential that path never uses
// took the whole engine down. Exposing it keeps that decision visible instead of
// silent.
func (e *Engine) BearerPathAvailable() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.hasSession
}

// CompetingExtensions reports the narrow (Flow-capable) extensions attached at
// once, sorted by address.
//
// More than one is a configuration the engine cannot serve coherently, and it is
// easy to walk into: load the extension in a second Chrome profile and both
// profiles are now attached, each signed into a different Google account.
//
// The bridge holds one cookie jar and `Current()` is whichever client connected
// last, so the account and the project move under a process that has already
// built its client. That is worse than an error — calls succeed and report the
// wrong account — which is why this is worth naming rather than tolerating.
//
// Surface() is used rather than a probe: it is a cached answer, so a health check
// does not pay a round trip to ask.
func (e *Engine) CompetingExtensions() []string {
	if e.bridge == nil {
		return nil
	}
	var narrow []string
	for addr, client := range e.bridge.Clients() {
		if client != nil && client.Surface().FlowOperations {
			narrow = append(narrow, addr)
		}
	}
	sort.Strings(narrow)
	return narrow
}

// warnOnCompetingExtensions says so once, loudly, when more than one narrow
// extension is attached.
func (e *Engine) warnOnCompetingExtensions() {
	narrow := e.CompetingExtensions()
	if len(narrow) < 2 {
		return
	}

	current := ""
	if client := e.bridge.Current(); client != nil {
		for _, addr := range narrow {
			if e.bridge.Clients()[addr] == client {
				current = addr
				break
			}
		}
	}

	log.Printf("engine: %d Flow extensions are attached at once (%s). The bridge holds "+
		"one cookie jar and uses whichever connected last, so the account and the project "+
		"will move under this process — results will look right and belong to the other "+
		"account. Close the extension, or the Flow tab, in every browser profile but one. "+
		"Using %s.", len(narrow), strings.Join(narrow, ", "), current)
}

// Fingerprint returns the browser identity adopted at start-up, or nil when the
// browser could not be reached and the generic Chrome profile is in use.
func (e *Engine) Fingerprint() *flowapi.BrowserFingerprint {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.fingerprint
}

// NewBatchexecuteClient builds a batchexecute client carrying this engine's
// browser identity, for callers outside the engine (the debug endpoints).
func (e *Engine) NewBatchexecuteClient(jar *cookiejar.Jar, hc *httpx.Client) *batchexecute.Client {
	return e.newBatchexecuteClient(jar, hc)
}

// newBatchexecuteClient builds a batchexecute client that presents the browser
// identity adopted at start-up.
//
// The identity is not optional decoration. A captcha-bearing call that goes out
// under a different user-agent or sec-ch-ua is rejected with
// PUBLIC_ERROR_UNUSUAL_ACTIVITY, which reads as a captcha failure but is really
// a fingerprint mismatch. Read-only calls are unaffected either way.
func (e *Engine) newBatchexecuteClient(jar *cookiejar.Jar, hc *httpx.Client) *batchexecute.Client {
	client := batchexecute.New(jar, hc)

	e.mu.RLock()
	fp := e.fingerprint
	index := e.accountIndex
	e.mu.RUnlock()

	// Account selection is a query parameter, so every client has to carry it.
	client.SetAuthUser(index)

	if fp != nil {
		client.SetFingerprint(batchexecute.Fingerprint{
			UserAgent: fp.UserAgent,
			SecChUa:   fp.SecChUa,
			Platform:  fp.Platform,
			Mobile:    fp.Mobile,
			Language:  fp.Language,
		})
	}

	// Open with the same first request the browser makes. Without the token the
	// client has to prime, and the priming handshake depends on the server
	// answering 400 with the token in the body — which it does not do when it
	// answers 401 instead, leaving every later call rejected for want of a token
	// it was never given.
	e.mu.RLock()
	at := e.atToken
	sid := e.fsid
	e.mu.RUnlock()
	if at != "" {
		client.SeedToken(at)
	}
	if sid != "" {
		client.SetSessionID(sid)
	}

	// A 401 means the session behind the cookies has expired, which happens to a
	// long-running engine. Re-sync from the browser and re-mint the access token,
	// then hand the fresh jar back so the client can swap it in and retry.
	//
	// Without this a 401 is terminal: every later call fails the same way until
	// an operator calls /v1/bridge/refresh by hand.
	client.SetUnauthorizedHandler(func(ctx context.Context) (*cookiejar.Jar, error) {
		if e.bridge == nil || !e.bridge.Connected() {
			return nil, fmt.Errorf("engine: the session expired and no browser is attached to refresh it")
		}
		jar, err := e.bridge.RefreshSession(ctx)
		if err != nil {
			return nil, err
		}
		if err := e.Bootstrap(ctx); err != nil {
			return nil, err
		}
		return jar, nil
	})

	return client
}

// New builds an engine. Call Bootstrap to make it usable.
func New(st *store.Store, br *bridge.Bridge, opts Options) (*Engine, error) {
	hc, err := httpx.New(
		httpx.WithTimeout(time.Duration(config.RequestTimeout)*time.Second),
		httpx.WithProxy(opts.ProxyURL),
	)
	if err != nil {
		return nil, err
	}

	return &Engine{
		store:        st,
		pool:         pool.New(),
		bridge:       br,
		hc:           hc,
		opts:         opts,
		accountIndex: storedAccountIndex(st, opts.AccountIndex),
	}, nil
}

// storedAccountIndex resolves which signed-in account the engine should act as
// when it starts.
//
// The stored value wins, because it is the record of a deliberate choice made
// through /v1/accounts/switch and the whole point of storing it is that the
// choice outlives the process. The configured value is only the seed for a
// database that has never recorded one — that is, the first run.
//
// A read that fails is logged and treated as unset rather than taken as zero:
// falling back silently is what made this setting necessary in the first place,
// and a failed read is not evidence that the answer is the first account.
func storedAccountIndex(st *store.Store, configured int) int {
	if st != nil {
		raw, found, err := st.Setting(store.SettingKeyAccountIndex)
		switch {
		case err != nil:
			log.Printf("engine: could not read the stored account index (%v); "+
				"falling back to the configured one", err)
		case found:
			index, convErr := strconv.Atoi(strings.TrimSpace(raw))
			if convErr != nil {
				log.Printf("engine: the stored account index %q is not a number (%v); "+
					"falling back to the configured one", raw, convErr)
			} else if index < 0 {
				log.Printf("engine: the stored account index %d is negative; "+
					"falling back to the configured one", index)
			} else {
				return index
			}
		}
	}
	if configured < 0 {
		return 0
	}
	return configured
}

// Store exposes the database.
func (e *Engine) Store() *store.Store { return e.store }

// Pool exposes the worker pool.
func (e *Engine) Pool() *pool.Pool { return e.pool }

// Bridge exposes the extension bridge.
func (e *Engine) Bridge() *bridge.Bridge { return e.bridge }

// Ready reports whether an account has been loaded and a token minted.
func (e *Engine) Ready() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.ready
}

// LastError returns the most recent bootstrap or generation error.
func (e *Engine) LastError() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastError
}

// AccountID returns the active account label.
func (e *Engine) AccountID() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.accountID
}

// ProjectID returns the project the engine resolved at bootstrap.
func (e *Engine) ProjectID() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.projectID
}

// captchaPageURL returns a page that loads the site's reCAPTCHA client.
//
// The project editor does; the account and landing pages do not, and the site's
// CSP blocks injecting the bundle, so a broker that lands on one of those cannot
// mint at all. Handing the broker this URL lets it navigate somewhere that can.
//
// Resolved lazily: the provider is built before the project id is known, but a
// token is only ever asked for after bootstrap has set it.
func (e *Engine) captchaPageURL() string {
	id := e.ProjectID()
	if id == "" {
		return ""
	}
	// The account has to be in the path too: the same project id under a
	// different account is a 404, and a 404 page has no reCAPTCHA client on it.
	if index := e.AccountIndex(); index > 0 {
		return fmt.Sprintf("https://flow.google.com/u/%d/project/%s", index, id)
	}
	return "https://flow.google.com/project/" + id
}

// AccountIndex returns the signed-in Google account the engine acts as.
func (e *Engine) AccountIndex() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.accountIndex
}

// SetAccountIndex switches which signed-in Google account the engine acts as, and
// re-bootstraps so the session, the account row and the project all belong to it.
//
// A browser can hold several accounts at once. They share one cookie jar, so the
// only thing that distinguishes them is the `authuser` index — which means a
// switch is not something the cookies can carry, and a project read from the
// browser's current tab will belong to a different account than the session
// unless this is set to match.
func (e *Engine) SetAccountIndex(ctx context.Context, index int) error {
	if index < 0 {
		index = 0
	}
	e.mu.Lock()
	e.accountIndex = index
	e.mu.Unlock()

	// Record the choice before re-bootstrapping, and treat a failed write as a
	// failure of the switch rather than a detail: the caller asked for this
	// account to be the one in use, and a switch that silently does not survive
	// the next restart is the bug this exists to fix.
	if e.store != nil {
		if err := e.store.SetSetting(store.SettingKeyAccountIndex, strconv.Itoa(index)); err != nil {
			return fmt.Errorf("engine: account %d selected but not recorded: %w", index, err)
		}
	}

	log.Printf("engine: switching to signed-in account %d", index)
	return e.Bootstrap(ctx)
}

// AccountCredits is one signed-in account's balance.
type AccountCredits struct {
	// Index is the `authuser` index, which is how the browser's account list is
	// addressed.
	Index int `json:"index"`
	// SignedIn reports whether a session could be minted for this index at all.
	SignedIn bool   `json:"signed_in"`
	Email    string `json:"email,omitempty"`
	// Name is the account's display name. It is carried because several accounts
	// in one browser routinely share a display name, so the address alone is not
	// always enough to tell two rows apart.
	Name string `json:"name,omitempty"`
	// Credits is nil when the balance could not be read. It used to be a plain
	// int, so a failed read and a genuine zero were both reported as 0 — and
	// adding the rows up produced a confident, wrong total.
	Credits *int `json:"credits"`
	// SKU is the subscription tier, and it is **not per-account**.
	//
	// It is read from the labs session endpoint, which answers for the default
	// account whatever `authuser` says — the same blindness that made every row
	// report the first account's email. Unlike the email there is no replacement:
	// the `nzlgx` credits payload is a flat array of numbers with no tier in it
	// (`[[7,3,8,1,null,7]]`), and the legacy aisandbox `/v1/credits` answers for a
	// different signed-in account entirely.
	//
	// So every row carries the same value, and it belongs to whichever account the
	// browser is showing. It is kept because it is still the tier of *an* account
	// on this machine, but nothing should branch on it per row — which is why the
	// account-selection policy ignores free-tier preference.
	SKU   string `json:"sku,omitempty"`
	Error string `json:"error,omitempty"`
	// FallbackIndices is how many trailing indices were dropped because they
	// repeated this row. `authuser` past the number of signed-in accounts falls
	// back to the default, so a request for ten indices returns the default
	// account once and then repeats it; those repeats are not accounts and must
	// not be counted. Zero when there were none.
	FallbackIndices int `json:"fallback_indices,omitempty"`
}

// AccountsCredits reads the balance of every signed-in Google account.
//
// Each account needs its own session minted, so this is deliberately separate
// from the single-account /v1/credits rather than folded into it: one call here
// is several round trips, and a caller that only wants the active account should
// not pay for them.
func (e *Engine) AccountsCredits(ctx context.Context, max int) ([]AccountCredits, error) {
	// Ten is the ceiling because ten is the limit: a Chrome profile holds at most
	// ten signed-in Google accounts, so an index past that cannot resolve to
	// anything but the default. The guard exists to bound the loop, and it is set
	// to the real bound rather than to a number that felt roomy — an earlier
	// revision raised it to 25 on no evidence at all.
	if max <= 0 || max > 10 {
		max = 4
	}
	if !e.Ready() {
		return nil, fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	jar := e.bridge.Jar()
	if jar == nil {
		return nil, fmt.Errorf("engine: no cookies loaded")
	}

	out := make([]AccountCredits, 0, max)
	for i := 0; i < max; i++ {
		row := AccountCredits{Index: i}

		provider := auth.NewProvider(jar, e.hc)
		provider.SetAccountIndex(i)
		session, err := provider.Session(ctx)
		if err != nil {
			row.Error = err.Error()
			out = append(out, row)
			continue
		}
		row.SignedIn = true
		row.SKU = session.Sku

		client := e.newBatchexecuteClient(jar, e.hc)
		client.SetAuthUser(i)
		// Drop the seeded anti-CSRF token for this call.
		//
		// It was read from the page for the account the browser is showing, and
		// sending it with `authuser` pointing at a different account is answered
		// with 400. Clearing it makes the client prime for its own token instead,
		// which is the handshake it already knows how to do.
		client.SeedToken("")

		// Take the address from the profile RPC, not from the session.
		//
		// The labs session endpoint answers for the default account whatever
		// `authuser` says, so `session.Email` is the same string for every index —
		// which is how three distinct accounts came to be reported under one
		// address. The profile RPC is account-scoped and is the only source that
		// follows `authuser`.
		if prof, err := client.Profile(ctx, batchexecute.CallOptions{
			SourcePath: "/", BuildLabel: config.BuildLabel(),
		}); err == nil {
			row.Email = prof.Email
			row.Name = prof.Name
		} else if row.Error == "" {
			row.Error = "profile: " + err.Error()
		}

		credits, err := client.Credits(ctx, batchexecute.CallOptions{
			SourcePath: "/", BuildLabel: config.BuildLabel(),
		})
		if err != nil {
			row.Error = err.Error()
		} else {
			row.Credits = &credits
		}
		out = append(out, row)
	}

	// Drop the trailing run that repeats the first row.
	//
	// An `authuser` index past the number of signed-in accounts falls back to the
	// default, so asking for ten indices returns the default account once and
	// then repeats it. Those repeats are not accounts and must not be summed, and
	// they are at the end by construction.
	//
	// This deliberately does not deduplicate interior rows. Two real accounts can
	// hold the same balance, and collapsing on the balance alone would merge them
	// and understate the total — which is the failure the earlier email-based
	// dedupe produced, in the other direction. Only the trailing fallback is
	// removed, and the count of what was dropped is reported.
	kept := len(out)
	for kept > 1 && sameAccountRow(out[kept-1], out[0]) {
		kept--
	}
	fallback := len(out) - kept
	out = out[:kept]

	for i := range out {
		out[i].FallbackIndices = fallback
	}
	return out, nil
}

// sameAccountRow reports whether two rows describe the same reading.
func sameAccountRow(a, b AccountCredits) bool {
	if a.Email != b.Email || a.Error != b.Error {
		return false
	}
	switch {
	case a.Credits == nil && b.Credits == nil:
		return true
	case a.Credits == nil || b.Credits == nil:
		return false
	default:
		return *a.Credits == *b.Credits
	}
}

// TotalCredits sums the balances that were actually read, and reports how many
// accounts that covered. A balance that could not be read is left out rather
// than counted as zero — the difference between "no credits" and "unknown" is
// the whole reason this returns two numbers.
func TotalCredits(rows []AccountCredits) (total int, counted int) {
	for _, row := range rows {
		if row.Credits != nil {
			total += *row.Credits
			counted++
		}
	}
	return total, counted
}

// AccessToken returns the current access token.
//
// Exposed for the browser-submit diagnostic, which needs the engine's own
// credential so its request can be compared against one made from the page. The
// token is a bearer credential; callers must not log it.
func (e *Engine) AccessToken(ctx context.Context) (string, error) {
	e.mu.RLock()
	client := e.client
	e.mu.RUnlock()
	if client == nil {
		return "", fmt.Errorf("engine: not ready")
	}
	return client.Session().AccessToken(ctx)
}

// Credits reads the account's real Flow credit balance over batchexecute.
//
// This is the authoritative figure. The legacy aisandbox endpoint reports a
// different number, and the token used for it can belong to a different signed-in
// account entirely, so it is not a source to trust.
func (e *Engine) Credits(ctx context.Context) (int, error) {
	jar := e.bridge.Jar()
	if jar == nil {
		return 0, fmt.Errorf("engine: no cookies loaded")
	}
	client := e.newBatchexecuteClient(jar, e.hc)
	return client.Credits(ctx, batchexecute.CallOptions{
		SourcePath: "/",
		BuildLabel: config.BuildLabel(),
	})
}

// RawCredits returns the upstream credits response unparsed.
//
// Exposed for the diagnostic endpoint: the field names are undocumented, so
// trusting a parsed balance without seeing the payload is how a daily allocation
// gets mistaken for a real balance.
func (e *Engine) RawCredits(ctx context.Context) (json.RawMessage, error) {
	e.mu.RLock()
	client := e.client
	e.mu.RUnlock()
	if client == nil {
		return nil, fmt.Errorf("engine: not ready")
	}

	result, err := client.Get(ctx, config.Endpoints["get_credits"], "")
	if err != nil {
		return nil, err
	}
	return json.RawMessage(result.Body), nil
}

// RawCreditsRPC returns the batchexecute credits response for one signed-in
// account, unparsed.
//
// Distinct from RawCredits, which calls the **legacy aisandbox** endpoint and so
// reports whatever account that credential belongs to — which the README records
// as a different signed-in account from the one the app session uses. This one goes
// over the same transport and the same `authuser` as a real balance read, so it can
// be asked about a specific account.
func (e *Engine) RawCreditsRPC(ctx context.Context, authUser int) ([]json.RawMessage, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	jar := e.bridge.Jar()
	if jar == nil {
		return nil, fmt.Errorf("engine: no cookies loaded")
	}

	client := e.newBatchexecuteClient(jar, e.hc)
	client.SetAuthUser(authUser)
	// The seeded anti-CSRF token belongs to the account the page is showing, and
	// sending it alongside a different `authuser` is answered with 400.
	if authUser != 0 {
		client.SeedToken("")
	}
	return client.CreditsFrames(ctx, batchexecute.CallOptions{
		SourcePath: "/", BuildLabel: config.BuildLabel(),
	})
}

// SessionSnapshot is the browser-derived state one process hands to another.
//
// The browser is reachable through exactly one host — whichever process owns the
// bridge port — and everything the engine needs from it is short-lived: the
// cookies rotate, and the page tokens only exist in a loaded page. A process with
// no browser can still work, but only against a copy, and a copy goes stale on
// Google's schedule rather than on a timer anyone here controls.
//
// So the copy is taken on demand instead of on an interval: the process that has
// the browser serves this, and the one that does not asks for it at start-up. That
// leaves no window in which the copy is older than the run using it.
type SessionSnapshot struct {
	Cookies []cookiejar.Cookie `json:"cookies"`
	// At is the page's anti-CSRF token and Fsid its `f.sid`. Both are page-only,
	// so a browserless process cannot obtain them by any other route.
	At   string `json:"at,omitempty"`
	Fsid string `json:"fsid,omitempty"`
	// ProjectID is the project the browser is sitting on. Carrying it means a
	// caller does not have to be told one, which is the other thing that
	// otherwise forces a browser.
	ProjectID string `json:"project_id,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	// Index is the `authuser` index the snapshot was taken for.
	Index int `json:"account_index"`
	// Fingerprint is the browser identity the snapshot was taken under.
	//
	// Carried for the same reason as At and Fsid, and it was the missing one:
	// the page tokens alone are not enough for a captcha-bearing call, because
	// the assessment is checked against the client that made it. A browserless
	// process that presents a generic Chrome profile has its token rejected, and
	// the rejection is silent.
	Fingerprint *flowapi.BrowserFingerprint `json:"fingerprint,omitempty"`
}

// SessionSnapshot reads the current browser-derived state.
func (e *Engine) SessionSnapshot(ctx context.Context) (*SessionSnapshot, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	jar := e.bridge.Jar()
	if jar == nil {
		return nil, fmt.Errorf("engine: no cookies loaded")
	}

	e.mu.RLock()
	snapshot := &SessionSnapshot{
		Cookies:   jar.Cookies(),
		At:        e.atToken,
		Fsid:      e.fsid,
		ProjectID: e.projectID,
		AccountID: e.accountID,
		Index:     e.accountIndex,
	}
	fingerprint := e.fingerprint
	e.mu.RUnlock()

	// Read outside the lock: reading the bridge can block, and holding the
	// engine lock across it would stall every other caller.
	if fingerprint == nil {
		fingerprint = e.browserFingerprint(ctx)
	}
	snapshot.Fingerprint = fingerprint
	return snapshot, nil
}

// ListProjects returns the projects belonging to the account the engine acts as.
//
// This is the browser-free route to a project id. It goes out over the same
// transport every generation uses, so it needs cookies and nothing else — no
// tab, no navigation, no editor URL to read.
//
// Ordered by the server, most recently modified first, which makes the first
// entry the project the account was last working in.
func (e *Engine) ListProjects(ctx context.Context) ([]batchexecute.Project, error) {
	jar := e.bridge.Jar()
	if jar == nil {
		return nil, fmt.Errorf("engine: no cookies loaded")
	}

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client := e.newBatchexecuteClient(jar, e.hc)
	client.SetAuthUser(e.AccountIndex())

	return client.ProjectList(callCtx, batchexecute.CallOptions{SourcePath: "/"})
}

// CreateProject creates a Flow project and returns it.
//
// This is how a run gets a project that the app can also see, with no browser
// involved. It matters more than it sounds: a uuid minted by hand is accepted
// for generation but never appears in the listing, so before this the only way
// to make a *findable* project was to click New project in the browser.
func (e *Engine) CreateProject(ctx context.Context, label string) (batchexecute.Project, error) {
	jar := e.bridge.Jar()
	if jar == nil {
		return batchexecute.Project{}, fmt.Errorf("engine: no cookies loaded")
	}

	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	client := e.newBatchexecuteClient(jar, e.hc)
	client.SetAuthUser(e.AccountIndex())

	return client.CreateProject(callCtx, label)
}

// emptySubmissionHint explains the most likely cause of a video submission that
// comes back empty.
//
// The old wording sent the reader to "the model key and the RPC it went to", and
// a later revision told them to attach the extension so the page could mint a
// token. Both were wrong, and the second was actively misleading: the transport
// mints a perfectly good token, and the thing that made it look broken was that
// the token was presented twice — once by a cache in the provider, and once by a
// retry in the transport that resent a payload with the token already inside it.
// Both are fixed, and the hint names both so the next person does not have to
// rediscover the class.
//
// An empty frame means Flow accepted the request and did nothing, so the causes
// are all of that shape — something about the request was understood and
// declined, silently.
func (e *Engine) emptySubmissionHint() string {
	e.mu.RLock()
	provider := e.captcha
	e.mu.RUnlock()

	name := "unknown"
	if provider != nil {
		name = provider.Name()
	}

	// Ordered by how often each has been the answer.
	return fmt.Sprintf("The reCAPTCHA token came from %q. A token is single-use, so one "+
		"presented twice produces exactly this — Flow verifies it once and answers an "+
		"empty frame on every later call. That has happened two ways: a cache in the "+
		"provider, and a retry that resent a payload with the token already inside it. "+
		"Both are fixed, so if this is a token problem it is a new one of the same "+
		"shape: something is handing out or resending a token it has already used. "+
		"Check next that the project exists on the account in use: generating into a "+
		"project that is not there is accepted the same silent way. Only then suspect "+
		"the model key and the RPC it went to.", name)
}

// CaptchaToken obtains a fresh reCAPTCHA token for the given action.
func (e *Engine) CaptchaToken(ctx context.Context, action string) (string, error) {
	e.mu.RLock()
	provider := e.captcha
	e.mu.RUnlock()
	if provider == nil {
		return "", fmt.Errorf("engine: no reCAPTCHA provider configured")
	}
	return provider.Token(ctx, action)
}

// captchaOptions adds the captcha refresher to a captcha-carrying call.
//
// The refresher is set here and nowhere else, and that is the point. A token is
// single-use, so a retry that resends the payload presents a spent one and Flow
// answers an empty frame rather than an error — no status to check, no message to
// read, and the only symptom a generation that quietly did nothing. That is what
// happened twice, once through a cache and once through a retry, and it cost hours
// both times because nothing said so.
//
// So a captcha-carrying call does not hand-build its options. It passes them
// through here, and the refresher is present by construction. A new call site that
// builds batchexecute.CallOptions by hand now stands out against its neighbours
// rather than looking like the other four.
//
// The caller keeps control of the rest: the four generation calls set a source path
// and a build label, and the upload sets neither. Folding those into the helper
// would have changed what the upload sends, which is not this refactor's business.
//
// The action must match the payload's: a token minted for the wrong action is
// rejected, and rejected the same silent way.
func (e *Engine) captchaOptions(action string, opts batchexecute.CallOptions) batchexecute.CallOptions {
	opts.RefreshCaptcha = func(ctx context.Context) (string, error) {
		return e.CaptchaToken(ctx, action)
	}
	return opts
}

/* ------------------------------------------------------------------ *
 * Bootstrap
 * ------------------------------------------------------------------ */

// Bootstrap loads cookies (from the extension if one is connected, otherwise
// from the persisted copy), mints a token, and registers the account.
//
// It is safe to call repeatedly: a later call refreshes the cookie set and
// re-registers the worker, which is how a re-synced browser session is picked up
// without restarting the process.
func (e *Engine) Bootstrap(ctx context.Context) error {
	jar, source, err := e.loadJar(ctx)
	if err != nil {
		e.setError(err)
		return err
	}

	// Which signed-in account this boot is for. Read once and carried through,
	// so the session, the project and every later call agree on it.
	index := e.AccountIndex()

	provider := auth.NewProvider(jar, e.hc)
	// The provider has to know which signed-in account to mint for, or it
	// silently mints the first one while the project came from another.
	provider.SetAccountIndex(index)

	// The Labs session is the bearer path's credential, and the bearer path is
	// the legacy one. The app moved from labs.google to flow.google.com — which
	// now answers 308 for the old route — and a browser signed in there carries
	// Google's account cookies and no Labs NextAuth session at all. The path that
	// works, batchexecute authenticated by SAPISIDHASH over those cookies, never
	// asks for the session.
	//
	// Refusing to boot over it made the whole engine unusable, including the
	// cookie-authenticated path that was fine, so the failure is recorded and the
	// boot continues. Calls that genuinely need a bearer token still fail, with
	// their own error, at the point they need it.
	session, sessionErr := provider.Session(ctx)
	hasSession := sessionErr == nil
	if sessionErr != nil {
		log.Printf("engine: no Labs session (%v); the bearer path is unavailable, "+
			"continuing on the cookie-authenticated path", sessionErr)
		session = &auth.Session{}
	}

	// One browser can hold several signed-in accounts and they share a cookie
	// jar, so the hash alone cannot tell them apart — the index is part of the
	// identity, and without it two accounts collapse into one row.
	accountID := e.opts.AccountID
	if accountID == "" {
		accountID = shortID(jar.Hash())
		if index > 0 {
			accountID += fmt.Sprintf("-u%d", index)
		}
	}

	// Read the browser identity first, because two things below depend on it:
	// the captcha provider presents it when it mints a token, and every call
	// this engine makes has to present the same identity as the token's.
	browserFP := e.browserFingerprint(ctx)

	// The resolver, not a resolved client: whichever extension is attached can
	// change after this point, and a provider holding a captured client reports
	// "no connection" while a perfectly good one is attached.
	//
	// The user agent goes in because the widget scores the client that asks for a
	// token, and a token minted under one client and spent under another is the
	// mismatch this engine's own notes warn about. It was pinned to a Windows
	// build while the browser here is macOS, so the HTTP provider declared a
	// different machine than the one the generation call came from.
	captchaProvider := recaptcha.Build(e.opts.CaptchaMode, e.hc, e.bridge.Current(),
		e.captchaPageURL, e.bridge.Current, userAgentOf(browserFP))

	// Prefer the account's own project list over navigating a browser to read an
	// editor URL: both are live, and only one of them drives a tab. The browser
	// remains the fallback for when the listing is rejected.
	projectID, projectSource := chooseProjectID(e.opts.ProjectID, e.opts.DefaultProjectID,
		func() string { return e.projectFromRPC(ctx, jar, index) },
		func() string { return e.projectFromBrowser(ctx, index) })
	if projectSource == projectSourceConfigured {
		log.Printf("engine: using the configured project %s (FLOW_PROJECT_ID); no source was consulted", projectID)
	}

	// The page's anti-CSRF token, read once for the same reason: every
	// batchexecute client has to open with the same first request the browser
	// makes. Optional — without it the client falls back to the priming round
	// trip, which is what it did before the extension could supply it.
	atToken, fsid := e.pageTokens(ctx)

	client := flowapi.New(provider, e.hc, flowapi.Options{
		ProjectID:   projectID,
		AccountID:   accountID,
		Captcha:     captchaProvider,
		ProxyURL:    e.opts.ProxyURL,
		Fingerprint: browserFP,
	})

	worker := pool.NewWorker(accountID, client)
	// Drop the previous account's worker first. Bootstrap runs again on every
	// switch, and keeping the old one would leave the pool holding an account the
	// engine can no longer route to — and reporting its balance as available.
	e.pool.Retain(accountID)
	e.pool.Register(worker)

	// Record the account. Credits are left unknown until a real check runs —
	// writing a plausible-looking number here is exactly how the Python version
	// ended up reporting test fixtures as live balances.
	if err := e.store.UpsertAccount(store.Account{
		AccountID:  accountID,
		CookieHash: jar.Hash(),
		SKU:        session.Sku,
		Status:     "active",
	}); err != nil {
		log.Printf("engine: could not record account: %v", err)
	}

	e.mu.Lock()
	e.accountID = accountID
	e.projectID = projectID
	e.client = client
	e.captcha = captchaProvider
	e.fingerprint = browserFP
	e.atToken = atToken
	e.fsid = fsid
	e.hasSession = hasSession
	e.ready = true
	e.lastError = ""
	e.mu.Unlock()

	log.Printf("engine: ready — account %s, %d cookies (%s), project %s, captcha %s",
		accountID, jar.Count(), source,
		projectID, captchaProvider.Name())

	// Last, so every extension that is going to connect has. A second narrow
	// extension makes everything above provisional, and the operator needs to
	// know before they read a result rather than after.
	e.warnOnCompetingExtensions()

	return nil
}

func (e *Engine) loadJar(ctx context.Context) (*cookiejar.Jar, string, error) {
	// Prefer a live browser, since its cookies are the freshest.
	if client := e.bridge.Current(); client != nil && client.Connected() {
		jar, err := e.bridge.SyncCookies(ctx, client)
		if err == nil && jar.Count() > 0 {
			// The jar's own source, not a literal: which bridge answered is
			// exactly what this line is for, and a constant said otherwise.
			return jar, jar.Source(), nil
		}
		if err != nil {
			log.Printf("engine: live cookie sync failed (%v), falling back to the persisted copy", err)
		}
	}

	// Fall back to a persisted copy so the engine keeps working with the browser
	// closed. This is the point of syncing cookies in the first place.
	//
	// Two locations, and they are **not the same file**. The bridge writes the jar
	// it syncs into its own data directory; `CookieDir` is where an operator is
	// told to drop a manual dump. Reading only the second one meant the fallback
	// was whatever had been placed there by hand — which for a browser-driven
	// setup is nothing — so the engine loaded a copy from hours earlier and every
	// call answered 401. The synced file is tried first because it is the fresher
	// of the two by construction.
	candidates := []string{
		filepath.Join(config.DataDir(), "cookies.json"),
		filepath.Join(config.CookieDir(), "cookies.json"),
	}

	var tried []string
	for _, path := range candidates {
		jar, err := cookiejar.LoadFile(path)
		if err == nil {
			return jar, path, nil
		}
		if !os.IsNotExist(err) {
			return nil, "", fmt.Errorf("engine: could not read %s: %w", path, err)
		}
		tried = append(tried, path)
	}
	return nil, "", fmt.Errorf(
		"engine: no cookies available. Open the browser with the Flow Go Bridge "+
			"extension (extension/) loaded, or place a cookie dump at %s",
		strings.Join(tried, " or "))
}

func (e *Engine) setError(err error) {
	e.mu.Lock()
	e.lastError = err.Error()
	e.mu.Unlock()
}

// Where a boot's project id came from.
const (
	projectSourceExplicit   = "explicit"
	projectSourceConfigured = "configured"
	projectSourceRPC        = "rpc"
	projectSourceBrowser    = "browser"
	projectSourceNone       = "none"
)

// chooseProjectID picks the Flow project a boot runs against.
//
// Four sources, and the ranking is deliberate:
//
//	explicit   — the caller named it. Outranks everything.
//	configured — FLOW_PROJECT_ID. Also an instruction, which is why a set value
//	             stops the search: neither callback runs.
//	rpc        — the account's project list over the transport. Live truth, and
//	             it costs an HTTP call rather than a tab navigation.
//	browser    — the open editor's URL. Also live truth, but it is the only
//	             source that has to drive a browser to answer.
//
// The rpc source sits above the browser because the two are equally current and
// only one of them navigates. The browser stays as the last resort rather than
// being deleted: it is the only source that reports where the *user* is rather
// than where the account has been, and it still works when the listing call is
// rejected.
//
// askRPC and askBrowser are callbacks rather than values because both have side
// effects — an HTTP call and a tab navigation — and neither should happen when
// the answer is already known. askRPC runs first and askBrowser only if it comes
// back empty.
//
// A project id that is wrong fails visibly rather than silently: a project
// belongs to one signed-in account, so another account's id does not open.
func chooseProjectID(explicit, configured string, askRPC, askBrowser func() string) (id, source string) {
	if explicit != "" {
		return explicit, projectSourceExplicit
	}
	if configured != "" {
		return configured, projectSourceConfigured
	}
	if askRPC != nil {
		if id := askRPC(); id != "" {
			return id, projectSourceRPC
		}
	}
	if askBrowser != nil {
		if id := askBrowser(); id != "" {
			return id, projectSourceBrowser
		}
	}
	return "", projectSourceNone
}

// userAgentOf reads the user agent out of a fingerprint that may be nil.
func userAgentOf(fp *flowapi.BrowserFingerprint) string {
	if fp == nil {
		return ""
	}
	return fp.UserAgent
}

// projectFromRPC reads the account's project list over the transport and returns
// its most recent entry.
//
// Returns "" rather than an error when the listing fails or the account has no
// projects, because this is one of several sources and a caller that has to
// distinguish "failed" from "empty" would only re-report both as "try the next
// one".
func (e *Engine) projectFromRPC(ctx context.Context, jar *cookiejar.Jar, index int) string {
	if jar == nil {
		return ""
	}

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client := e.newBatchexecuteClient(jar, e.hc)
	client.SetAuthUser(index)

	projects, err := client.ProjectList(callCtx, batchexecute.CallOptions{
		SourcePath: "/",
	})
	if err != nil {
		log.Printf("engine: could not list projects over the transport (%v); trying the browser", err)
		return ""
	}
	if len(projects) == 0 {
		log.Printf("engine: the project listing is empty for this account")
		return ""
	}

	// The listing is most-recently-modified first, so the first row is the
	// project the account was last working in — the same one an editor tab would
	// have been showing.
	log.Printf("engine: %d project(s) listed over the transport; taking %s (modified %s)",
		len(projects), projects[0].ID, projects[0].Modified.Format(time.RFC3339))
	return projects[0].ID
}

// projectFromBrowser asks the extension to open a Flow project editor and reads
// the project ID off the resulting URL. Returns "" when the browser cannot help,
// in which case the caller falls back to the configured default.
func (e *Engine) projectFromBrowser(ctx context.Context, accountIndex int) string {
	if e.bridge == nil || !e.bridge.Connected() {
		return ""
	}

	tabCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	tab, err := e.bridge.EnsureProjectTab(tabCtx, accountIndex)
	if err != nil {
		// Say what actually happens. This used to claim it was "falling back
		// to <config.DefaultProject>", and it was not: it returns "" and the
		// caller does not apply the default, so the engine came up with no
		// project at all and every generation failed at "no project id
		// resolved". A log line that describes a fallback which does not happen
		// sends the reader looking for the wrong problem — which is the same
		// failure this codebase keeps recording.
		log.Printf("engine: no Flow project could be determined from the browser (%v). "+
			"Falling back to the configured project, if there is one; otherwise open a "+
			"project in the browser once, or set FLOW_PROJECT_ID", err)
		return ""
	}

	id := auth.ProjectIDFromFlowURL(tab.URL)
	if id == "" {
		return ""
	}
	log.Printf("engine: using Flow project %s, read from %s", id, tab.URL)
	return id
}

// browserFingerprint reads the browser's request identity so generation calls
// can present it. Returns nil when the browser cannot be reached, in which case
// the client falls back to a generic Chrome profile — fine for read-only calls,
// but generation requests will be rejected as unusual activity.
// pageTokensFile is where the page's anti-CSRF token and session id are kept
// between runs, beside the cookie cache and for the same reason: a process with
// no browser has to present the same opening request the page would have made.
func pageTokensFile() string {
	return filepath.Join(config.DataDir(), "page-tokens.json")
}

// pageTokenSet is the pair of page-only values every batchexecute request
// carries, in the shape they are persisted.
type pageTokenSet struct {
	At   string `json:"at,omitempty"`
	Fsid string `json:"fsid,omitempty"`
}

func savePageTokens(tokens pageTokenSet) {
	if tokens.At == "" && tokens.Fsid == "" {
		return
	}
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(pageTokensFile(), data, 0o600); err != nil {
		log.Printf("engine: could not persist the page tokens: %v", err)
	}
}

func loadPageTokens() pageTokenSet {
	data, err := os.ReadFile(pageTokensFile())
	if err != nil {
		return pageTokenSet{}
	}
	var tokens pageTokenSet
	if err := json.Unmarshal(data, &tokens); err != nil {
		return pageTokenSet{}
	}
	return tokens
}

// pageTokens reads the two values the app puts on every batchexecute request:
// the anti-CSRF token it sends in the body, and the session id it sends in the
// query string. Only the page has either.
//
// Both are optional in principle — without the token the client primes for one,
// which is what it did before the extension could answer this; without the
// session id the call goes out as it always did. In practice the priming path is
// not a substitute for a captcha-bearing generation: a browserless run that had
// the fingerprint and the cookies but neither of these came back empty, and the
// same run with both from a session snapshot succeeded. So they are persisted
// alongside the fingerprint rather than left to be re-derived.
func (e *Engine) pageTokens(ctx context.Context) (at, fsid string) {
	if e.bridge == nil || !e.bridge.Connected() {
		// No browser to read from. A SessionSnapshot is the best source, because
		// whoever supplied it is running right now.
		if e.opts.AtToken != "" || e.opts.Fsid != "" {
			return e.opts.AtToken, e.opts.Fsid
		}
		// Then the persisted copy, which is what lets a run with no browser at
		// all present the same opening request the page would have.
		if persisted := loadPageTokens(); persisted.At != "" || persisted.Fsid != "" {
			log.Printf("engine: adopting the persisted page tokens (at %d chars, f.sid %d chars)",
				len(persisted.At), len(persisted.Fsid))
			return persisted.At, persisted.Fsid
		}
		return "", ""
	}
	client := e.bridge.Current()
	if client == nil || !client.Connected() {
		return "", ""
	}

	atCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	raw, err := client.FlowAt(atCtx, "")
	if err != nil {
		log.Printf("engine: could not read the page's batchexecute tokens (%v); "+
			"batchexecute will prime for a token instead", err)
		return "", ""
	}

	var out struct {
		At   string `json:"at"`
		Fsid string `json:"fsid"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Printf("engine: the page tokens had an unexpected shape: %v", err)
		return "", ""
	}

	// Persist so the next run does not need this page.
	savePageTokens(pageTokenSet{At: out.At, Fsid: out.Fsid})

	if out.At == "" {
		log.Printf("engine: the page did not carry an anti-CSRF token; " +
			"batchexecute will prime for one instead")
	} else {
		log.Printf("engine: seeded the batchexecute anti-CSRF token from the page (%d chars)", len(out.At))
	}
	if out.Fsid == "" {
		log.Printf("engine: the page did not carry an f.sid; calls will go out without one")
	} else {
		log.Printf("engine: adopted the page's f.sid (%d chars)", len(out.Fsid))
	}
	return out.At, out.Fsid
}

// fingerprintFile is where the browser identity is kept between runs, beside the
// cookie cache for the same reason: a process with no browser has to be able to
// present the client its token was minted for, and a session snapshot is only
// available while another process is running to hand one over.
func fingerprintFile() string {
	return filepath.Join(config.DataDir(), "fingerprint.json")
}

// saveFingerprint persists the browser identity, best-effort.
func saveFingerprint(fp *flowapi.BrowserFingerprint) {
	if fp == nil || fp.UserAgent == "" {
		return
	}
	data, err := json.MarshalIndent(fp, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(fingerprintFile(), data, 0o600); err != nil {
		log.Printf("engine: could not persist the fingerprint: %v", err)
	}
}

// loadFingerprint reads a previously persisted browser identity.
func loadFingerprint() *flowapi.BrowserFingerprint {
	data, err := os.ReadFile(fingerprintFile())
	if err != nil {
		return nil
	}
	var fp flowapi.BrowserFingerprint
	if err := json.Unmarshal(data, &fp); err != nil || fp.UserAgent == "" {
		return nil
	}
	return &fp
}

func (e *Engine) browserFingerprint(ctx context.Context) *flowapi.BrowserFingerprint {
	if e.bridge == nil || !e.bridge.Connected() {
		// No browser to read from. A SessionSnapshot is the best source, because
		// whoever supplied it is running right now.
		if e.opts.Fingerprint != nil {
			log.Printf("engine: adopting the fingerprint from the session snapshot — %s",
				truncate(e.opts.Fingerprint.UserAgent, 70))
			return e.opts.Fingerprint
		}

		// Then the persisted copy, which is what makes a standalone run possible
		// at all. Without it the provider falls back to its pinned user agent,
		// which is a *different machine* from this one — and a captcha token
		// minted under one client and spent under another is rejected with no
		// error at all, just an empty result.
		if fp := loadFingerprint(); fp != nil {
			log.Printf("engine: adopting the persisted fingerprint — %s",
				truncate(fp.UserAgent, 70))
			return fp
		}

		log.Printf("engine: no browser, no snapshot and no persisted fingerprint; " +
			"a captcha-bearing call will present the pinned default and is likely to " +
			"come back empty. Run once with the browser attached to record one")
		return nil
	}

	fpCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	fp, err := e.bridge.Fingerprint(fpCtx)
	if err != nil {
		log.Printf("engine: could not read the browser fingerprint (%v); "+
			"generation requests will not match the reCAPTCHA assessment", err)
		return nil
	}

	adopted := &flowapi.BrowserFingerprint{
		UserAgent: fp.UserAgent,
		Language:  fp.Language,
		SecChUa:   fp.Brands,
		Platform:  fp.PlatformFull,
		Mobile:    fp.Mobile,
	}
	// Persist it so the next run does not need this browser at all.
	saveFingerprint(adopted)

	log.Printf("engine: adopting the browser fingerprint — %s", truncate(fp.UserAgent, 70))
	return adopted
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}

// captchaNeedsPage reports whether the configured captcha mode asks the browser
// for its token rather than minting one over the transport.
//
// Only `broker` does. The default mints server-side, which is why a generation
// no longer has to have a Flow tab open anywhere.
func (e *Engine) captchaNeedsPage() bool {
	return strings.EqualFold(strings.TrimSpace(e.opts.CaptchaMode), "broker")
}

// ensureProjectTab makes sure the browser is on a Flow editor before a
// generation is submitted. Best effort: if it fails, the HTTP reCAPTCHA provider
// still runs, but the higher-scoring broker path will not.
func (e *Engine) ensureProjectTab(ctx context.Context) {
	if e.bridge == nil || !e.bridge.Connected() {
		return
	}
	tabCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	tab, err := e.bridge.EnsureProjectTab(tabCtx, e.AccountIndex())
	if err != nil {
		log.Printf("engine: could not prepare a Flow editor tab: %v", err)
		return
	}
	log.Printf("engine: Flow editor ready at %s", tab.URL)
}

func (e *Engine) clearError() {
	e.mu.Lock()
	e.lastError = ""
	e.mu.Unlock()
}

func shortID(hash string) string {
	if len(hash) < 12 {
		return "account"
	}
	return "acct-" + hash[:12]
}

/* ------------------------------------------------------------------ *
 * Generation
 * ------------------------------------------------------------------ */

// VideoRequest is a caller-facing video generation request.
type VideoRequest struct {
	Prompt   string
	Aspect   string
	Duration int
	Count    int
	Seed     *int64
	// JobID lets a caller supply its own identifier, so an asynchronous submit
	// can create the tracking row before the worker starts. Empty generates one.
	JobID string
	// StartImage is a local file path or an already-uploaded media ID. Set it to
	// switch from text-to-video to image-to-video.
	StartImage string
	// EndImage, combined with StartImage, produces a first/last-frame
	// interpolation.
	EndImage string
	// ReferenceImages are media IDs or local file paths used for consistency.
	ReferenceImages []string
	// Resolution requests an upsampled output: "", "720p", "1080p", or "4k".
	Resolution string
	// Download writes finished media to disk when true.
	Download bool
	// AudioPreference is forwarded as audioFailurePreference.
	AudioPreference string
}

// VideoOutcome is the result of a video generation.
type VideoOutcome struct {
	JobID    string        `json:"job_id"`
	Account  string        `json:"account_id"`
	MediaIDs []string      `json:"media_ids"`
	Files    []MediaFile   `json:"files,omitempty"`
	Credits  int           `json:"credits_remaining"`
	Elapsed  time.Duration `json:"-"`
	ElapsedS float64       `json:"elapsed_seconds"`
	Status   string        `json:"status"`
}

// MediaFile is a downloaded asset.
type MediaFile struct {
	MediaID    string `json:"media_id"`
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	Resolution string `json:"resolution,omitempty"`
}

// GenerateVideo submits a video job, waits for it, optionally upsamples it, and
// optionally downloads the result.
func (e *Engine) GenerateVideo(ctx context.Context, req VideoRequest) (*VideoOutcome, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("engine: a prompt is required")
	}
	if req.Count < 1 {
		req.Count = 1
	}
	if req.Count > config.MaxCount {
		req.Count = config.MaxCount
	}
	if req.Duration == 0 {
		req.Duration = config.DefaultDuration
	}

	jobID := req.JobID
	if jobID == "" {
		jobID = uuid.NewString()
	}
	start := time.Now()

	// Resolve any local file inputs to Flow media IDs before submitting, so a
	// slow upload does not hold a pool slot.
	startMediaID, endMediaID, refIDs, err := e.resolveImageInputs(ctx, req)
	if err != nil {
		return nil, err
	}

	// Label the row with the model the conditioning actually selects rather than
	// the per-duration text-to-video default, so an image job is not recorded as
	// a text one. Descriptive only — the submission resolves its own model.
	recordedModel := config.VideoModels[req.Duration]
	if model, ok := config.VideoModelFor(req.Duration, startMediaID != "" || len(refIDs) > 0, endMediaID != ""); ok {
		recordedModel = model
	}

	rowID, err := e.store.RecordGeneration(store.Generation{
		JobID:    jobID,
		Kind:     "video",
		Prompt:   req.Prompt,
		Model:    recordedModel,
		Duration: req.Duration,
		Aspect:   req.Aspect,
		Count:    req.Count,
		Status:   "submitted",
	})
	if err != nil {
		log.Printf("engine: could not record the job: %v", err)
	}

	cost := config.CreditsPerVideo[req.Duration] * req.Count

	// The widget only exists in a Flow editor, so the browser has to be sitting
	// on one before a page-minted token can be asked for.
	//
	// This used to run unconditionally, which meant every generation navigated a
	// tab whether or not the token needed a page. The default chain mints over
	// the transport and needs nothing from the browser, so the navigation is now
	// tied to the mode that actually wants it.
	if e.captchaNeedsPage() {
		e.ensureProjectTab(ctx)
	}

	var outcome *flowapi.VideoResult
	var accountID string

	poolErr := e.pool.Execute(ctx, cost, func(ctx context.Context, worker *pool.Worker) error {
		var callErr error
		switch {
		case startMediaID != "" && endMediaID != "":
			outcome, callErr = worker.Client.GenerateVideoFirstLast(ctx, toAPIRequest(req), startMediaID, endMediaID)
		case startMediaID != "":
			outcome, callErr = worker.Client.GenerateVideoFromImage(ctx, toAPIRequest(req), startMediaID)
		case len(refIDs) > 0:
			outcome, callErr = worker.Client.GenerateVideoFromReferences(ctx, toAPIRequest(req), refIDs)
		default:
			outcome, callErr = worker.Client.GenerateVideo(ctx, toAPIRequest(req))
		}
		if callErr == nil {
			accountID = worker.ID
		}
		return callErr
	})

	if poolErr != nil {
		e.finishJob(jobID, "failed", nil, start, poolErr)
		_ = e.store.RecordAccountOutcome(accountID, true, poolErr.Error())
		return nil, poolErr
	}

	result := &VideoOutcome{
		JobID:    jobID,
		Account:  accountID,
		MediaIDs: outcome.MediaIDs,
		Credits:  outcome.RemainingCredits,
	}

	// Poll outside the pool: polling is read-only and cheap, and holding a
	// generation slot for several minutes would stall the whole pool.
	for _, mediaID := range outcome.MediaIDs {
		status, err := e.waitFor(ctx, mediaID)
		if err != nil {
			log.Printf("engine: %s did not finish: %v", shortID(mediaID), err)
			result.Status = "timeout"
			continue
		}
		if !status.Succeeded() {
			log.Printf("engine: %s finished as %s", shortID(mediaID), status.MediaGenerationStatus)
			result.Status = strings.ToLower(strings.TrimPrefix(status.MediaGenerationStatus, "MEDIA_GENERATION_STATUS_"))
			continue
		}
		result.Status = "succeeded"
	}

	// Optional second pass for a higher-resolution output.
	resolution := config.NativeVideoResolution
	if tier, err := flowapi.NormalizeResolution(req.Resolution); err != nil {
		return nil, err
	} else if tier != "" {
		upsampled, upErr := e.upsample(ctx, outcome.MediaIDs, req.Aspect, tier, req.Seed)
		if upErr != nil {
			log.Printf("engine: upsample to %s failed, keeping the 720p output: %v", tier, upErr)
		} else {
			result.MediaIDs = upsampled
			resolution = tier
			for _, mediaID := range upsampled {
				if _, err := e.waitFor(ctx, mediaID); err != nil {
					log.Printf("engine: upsampled %s did not finish: %v", shortID(mediaID), err)
				}
			}
		}
	}

	if req.Download {
		files, dlErr := e.downloadAll(ctx, result.MediaIDs, "video", resolution, req.Prompt, rowID)
		if dlErr != nil {
			log.Printf("engine: download problem: %v", dlErr)
		}
		result.Files = files
	}

	result.Elapsed = time.Since(start)
	result.ElapsedS = result.Elapsed.Seconds()

	credits := outcome.RemainingCredits
	e.finishJob(jobID, result.Status, &credits, start, nil)
	_ = e.store.RecordAccountOutcome(accountID, false, "")
	e.clearError()

	return result, nil
}

// SubmitVideo starts a video generation in the background and returns its job ID
// immediately, so an HTTP caller is not held open for the minutes a generation
// takes. Poll the job for progress.
func (e *Engine) SubmitVideo(ctx context.Context, req VideoRequest) (string, error) {
	if !e.Ready() {
		return "", fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return "", fmt.Errorf("engine: a prompt is required")
	}
	if req.JobID == "" {
		req.JobID = uuid.NewString()
	}

	// Descriptive label only; see the note in GenerateVideo.
	recordedModel := config.VideoModels[req.Duration]
	if model, ok := config.VideoModelFor(req.Duration, req.StartImage != "" || len(req.ReferenceImages) > 0, req.EndImage != ""); ok {
		recordedModel = model
	}

	// Create the tracking row now, so a poll issued immediately after submit
	// finds the job rather than a 404.
	if _, err := e.store.RecordGeneration(store.Generation{
		JobID:    req.JobID,
		Kind:     "video",
		Prompt:   req.Prompt,
		Model:    recordedModel,
		Duration: req.Duration,
		Aspect:   req.Aspect,
		Count:    req.Count,
		Status:   "submitted",
	}); err != nil {
		return "", err
	}

	go func() {
		// Detach from the request context: the HTTP handler returns immediately,
		// and cancelling that request must not abort a generation the account has
		// already paid for.
		bg, cancel := context.WithTimeout(
			context.Background(),
			time.Duration(config.PollTimeout+180)*time.Second,
		)
		defer cancel()
		if _, err := e.GenerateVideo(bg, req); err != nil {
			log.Printf("engine: job %s failed: %v", shortID(req.JobID), err)
		}
	}()

	return req.JobID, nil
}

func toAPIRequest(req VideoRequest) flowapi.VideoRequest {
	return flowapi.VideoRequest{
		Prompt:          req.Prompt,
		Aspect:          req.Aspect,
		Duration:        req.Duration,
		Count:           req.Count,
		Seed:            req.Seed,
		AudioPreference: req.AudioPreference,
	}
}

func (e *Engine) waitFor(ctx context.Context, mediaID string) (flowapi.MediaStatus, error) {
	e.mu.RLock()
	client := e.client
	e.mu.RUnlock()
	if client == nil {
		return flowapi.MediaStatus{}, fmt.Errorf("engine: no active client")
	}
	return client.WaitForMedia(ctx, mediaID)
}

func (e *Engine) upsample(ctx context.Context, mediaIDs []string, aspect, tier string, seed *int64) ([]string, error) {
	var out []string
	err := e.pool.Execute(ctx, config.CreditsPerUpsample[tier], func(ctx context.Context, worker *pool.Worker) error {
		out = nil
		for _, mediaID := range mediaIDs {
			ids, err := worker.Client.UpsampleVideo(ctx, mediaID, aspect, tier, seed, "")
			if err != nil {
				return err
			}
			out = append(out, ids...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resolveImageInputs converts local paths in the request into Flow media IDs.
func (e *Engine) resolveImageInputs(ctx context.Context, req VideoRequest) (string, string, []string, error) {
	upload := func(value string) (string, error) {
		if value == "" {
			return "", nil
		}
		if !looksLikePath(value) {
			return value, nil // already a media ID
		}
		return e.UploadImage(ctx, value)
	}

	start, err := upload(req.StartImage)
	if err != nil {
		return "", "", nil, err
	}
	end, err := upload(req.EndImage)
	if err != nil {
		return "", "", nil, err
	}

	refs := make([]string, 0, len(req.ReferenceImages))
	for _, ref := range req.ReferenceImages {
		id, err := upload(ref)
		if err != nil {
			return "", "", nil, err
		}
		if id != "" {
			refs = append(refs, id)
		}
	}
	return start, end, refs, nil
}

func looksLikePath(value string) bool {
	if strings.ContainsAny(value, `/\`) {
		return true
	}
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp"} {
		if strings.HasSuffix(strings.ToLower(value), ext) {
			return true
		}
	}
	return false
}

// UploadImage sends a local image to Flow and returns its media ID.
func (e *Engine) UploadImage(ctx context.Context, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("engine: read %s: %w", path, err)
	}

	mimeType := mimeFor(path)
	var mediaID string

	err = e.pool.Execute(ctx, 0, func(ctx context.Context, worker *pool.Worker) error {
		var upErr error
		mediaID, upErr = worker.Client.UploadImage(ctx, data, mimeType)
		return upErr
	})
	if err != nil {
		return "", err
	}

	_, _ = e.store.RecordMedia(store.Media{
		MediaID:  mediaID,
		Kind:     "image",
		FileName: filepath.Base(path),
		FilePath: path,
		Bytes:    int64(len(data)),
	})

	log.Printf("engine: uploaded %s as %s", filepath.Base(path), shortID(mediaID))
	return mediaID, nil
}

func mimeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	default:
		return "application/octet-stream"
	}
}

/* ------------------------------------------------------------------ *
 * Images
 * ------------------------------------------------------------------ */

// ImageRequest is a caller-facing image generation request.
type ImageRequest struct {
	Prompt       string
	Aspect       string
	Count        int
	Seed         *int64
	Model        string
	ReferenceIDs []string
	Download     bool
}

// ImageOutcome is the result of an image generation.
type ImageOutcome struct {
	JobID    string                `json:"job_id"`
	Account  string                `json:"account_id"`
	Images   []flowapi.ImageResult `json:"images"`
	Files    []MediaFile           `json:"files,omitempty"`
	ElapsedS float64               `json:"elapsed_seconds"`
	Status   string                `json:"status"`
}

// GenerateImage submits an image job and downloads the results.
func (e *Engine) GenerateImage(ctx context.Context, req ImageRequest) (*ImageOutcome, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("engine: a prompt is required")
	}

	jobID := uuid.NewString()
	start := time.Now()

	rowID, err := e.store.RecordGeneration(store.Generation{
		JobID:  jobID,
		Kind:   "image",
		Prompt: req.Prompt,
		Model:  req.Model,
		Aspect: req.Aspect,
		Count:  req.Count,
		Status: "submitted",
	})
	if err != nil {
		log.Printf("engine: could not record the job: %v", err)
	}

	var images []flowapi.ImageResult
	var accountID string

	// Image generation is free on the tiers this targets, so the cost gate is 0.
	poolErr := e.pool.Execute(ctx, 0, func(ctx context.Context, worker *pool.Worker) error {
		var callErr error
		images, callErr = worker.Client.GenerateImage(ctx, flowapi.ImageRequest{
			Prompt:       req.Prompt,
			Aspect:       req.Aspect,
			Count:        req.Count,
			Seed:         req.Seed,
			Model:        req.Model,
			ReferenceIDs: req.ReferenceIDs,
		})
		if callErr == nil {
			accountID = worker.ID
		}
		return callErr
	})

	if poolErr != nil {
		e.finishJob(jobID, "failed", nil, start, poolErr)
		_ = e.store.RecordAccountOutcome(accountID, true, poolErr.Error())
		return nil, poolErr
	}

	outcome := &ImageOutcome{
		JobID:   jobID,
		Account: accountID,
		Images:  images,
		Status:  "succeeded",
	}

	if req.Download {
		for _, img := range images {
			if img.ImageURL == "" {
				continue
			}
			file, err := e.downloadOne(ctx, img.ImageURL, img.MediaID, "image", "", req.Prompt, rowID)
			if err != nil {
				log.Printf("engine: image download failed: %v", err)
				continue
			}
			outcome.Files = append(outcome.Files, file)
		}
	}

	outcome.ElapsedS = time.Since(start).Seconds()
	e.finishJob(jobID, outcome.Status, nil, start, nil)
	_ = e.store.RecordAccountOutcome(accountID, false, "")
	e.clearError()

	return outcome, nil
}

/* ------------------------------------------------------------------ *
 * Upscaling
 * ------------------------------------------------------------------ */

// BatchVideoRequest is a caller-facing video request for the batchexecute path.
type BatchVideoRequest struct {
	Prompt string
	// Model is the video key: abra_t2v_{4,6,8,10}s, or the image-conditioned
	// equivalent.
	Model string
	// Duration is seconds; used to derive the model when Model is empty.
	Duration int
	// Quality is "360p" or "720p" (the default). It rides on the model key as a
	// `_360p` suffix, and it is the expensive axis: a 4s render costs 4 credits at
	// 360p against 7 at 720p, which decides whether an account can submit at all.
	Quality   string
	Count     int
	ProjectID string
	// Wait blocks until the render finishes, then resolves and downloads it.
	Wait bool
	// Download writes the finished video to output/. Implies Wait.
	Download bool
	// StartImage and EndImage are project media ids the video is conditioned on.
	// Setting either switches the submission into image mode.
	StartImage string
	EndImage   string
	// StartFrame and EndFrame are the per-image crop values. The app sends them
	// with every condition image and a submission without them returns empty.
	StartFrame []float64
	EndFrame   []float64
}

// BatchVideoOutcome is the result of a video submission.
type BatchVideoOutcome struct {
	JobID     string      `json:"job_id"`
	AccountID string      `json:"account_id"`
	ProjectID string      `json:"project_id"`
	Model     string      `json:"model"`
	MediaIDs  []string    `json:"media_ids"`
	URLs      []string    `json:"urls,omitempty"`
	Files     []MediaFile `json:"files,omitempty"`
	Credits   int         `json:"credits_remaining"`
	ElapsedS  float64     `json:"elapsed_seconds"`
	Status    string      `json:"status"`
	// Quality is the quality the render was submitted at, reported so a caller can
	// confirm it got what it asked for. The engine does not substitute a different
	// quality: an unaffordable request is refused rather than quietly rendered
	// cheaper, because a cheaper render is a different render and — for at least one
	// key — no render at all.
	Quality string `json:"quality,omitempty"`
	// CreditsCost is what the submission was expected to cost, per the cost
	// table. It is the number the affordability check used, so a caller can see
	// the budget the engine worked to rather than inferring it.
	CreditsCost int `json:"credits_cost,omitempty"`
	// RawFrames carries the unparsed response frames, and only when the parse
	// produced no media. An empty result is indistinguishable from a parser that
	// missed a new shape unless the payload is visible, and this is the only
	// place it exists — so it is kept for exactly that case rather than dropped.
	RawFrames []json.RawMessage `json:"raw_frames,omitempty"`
}

// creditsReadTimeout bounds the balance read that follows a submission.
//
// The read is bookkeeping and cannot change the outcome, so it must never be able
// to hold the request open. Twenty seconds is generous for one RPC and short
// enough that a stuck one is noticed rather than waited on.
const creditsReadTimeout = 20 * time.Second

// videoPlan is a video submission's cost and whether the account can pay it.
type videoPlan struct {
	Model   string
	Quality string
	// Cost is the total for the request: the per-render cost times the count.
	Cost int
	// Balance is the balance observed while planning, and Known says whether it
	// was actually read. Balance zero with Known false means "not read", never
	// "empty" — the two are different answers and only one of them is a reason
	// to refuse.
	Balance int
	Known   bool
	// Reason explains a refusal. It is empty when the plan is affordable, so a
	// caller tests Reason rather than a separate flag.
	Reason string
}

// Affordable reports whether the plan can be submitted.
func (p videoPlan) Affordable() bool { return p.Reason == "" }

// planVideo works out what a submission will cost and whether the account can
// pay for it.
//
// This check exists because the server does not make it. A submission the
// balance cannot cover is accepted and answered with no media rather than an
// error, so a 1-credit account against a 7-credit render presents as a broken
// request — and the cost was previously consulted only *after* that empty
// answer, to choose which log line to write. Deciding it before the call means
// the request is either affordable or refused, and a reCAPTCHA is never spent on
// a render that cannot be paid for.
func (e *Engine) planVideo(ctx context.Context, client *batchexecute.Client, req BatchVideoRequest, model, quality string) videoPlan {
	duration := req.Duration
	if duration == 0 {
		duration = config.DefaultDuration
	}
	count := req.Count
	if count < 1 {
		count = 1
	}

	perRender, known := config.VideoCost(duration, quality)
	if !known {
		// An unrecorded pair is not a refusal. The table covers the durations the
		// app offers, and a model named outright can sit outside it — making the
		// table the limit on what can be generated would be a worse failure than
		// the one being fixed.
		log.Printf("engine: no recorded cost for %ds at %s; the balance cannot be checked "+
			"before submitting", duration, quality)
		return videoPlan{Model: model, Quality: quality}
	}

	creditCtx, cancel := context.WithTimeout(ctx, creditsReadTimeout)
	balance, err := client.Credits(creditCtx, batchexecute.CallOptions{
		SourcePath: "/", BuildLabel: config.BuildLabel(),
	})
	cancel()
	if err != nil {
		// A failed read is not evidence of an empty wallet. Proceed, and say so —
		// treating an unreadable balance as zero would refuse work the account can
		// pay for.
		log.Printf("engine: could not read the balance before submitting (%v); the "+
			"affordability check cannot be made, so the submission proceeds", err)
		return videoPlan{Model: model, Quality: quality, Cost: perRender * count}
	}

	return decideVideoPlan(duration, count, model, quality, balance)
}

// decideVideoPlan is the whole affordability decision, given a balance that has
// already been read.
//
// It is separated from the read so it can be tested without a network: the
// arithmetic here is what decides whether a render is submitted, downgraded or
// refused, and it is the part that has to be right.
func decideVideoPlan(duration, count int, model, quality string, balance int) videoPlan {
	perRender, known := config.VideoCost(duration, quality)
	if !known {
		// An unrecorded pair is not a refusal; see planVideo.
		return videoPlan{Model: model, Quality: quality, Balance: balance, Known: true}
	}

	plan := videoPlan{
		Model:   model,
		Quality: quality,
		Cost:    perRender * count,
		Balance: balance,
		Known:   true,
	}
	if balance >= plan.Cost {
		return plan
	}

	// Refuse. The request asked for this quality and the account cannot pay for
	// it, so say so and stop.
	//
	// This used to downgrade to 360p when that fit, on the reasoning that a cheaper
	// render is better than none. It is not: `abra_t2v_4s_360p` accepts a
	// submission, returns a media id, and never produces an asset, so the downgrade
	// converted a request that would have worked at 720p into seven minutes of
	// polling and nothing at all — with no error to explain it. A refusal is
	// immediate and actionable.
	//
	// The downgrade was also unnecessary. A caller who wants 360p sets
	// `quality: "360p"`; silently substituting a different render than the one
	// asked for is a different thing from serving the request.
	plan.Reason = fmt.Sprintf(
		"insufficient credits: %ds at %s costs %d (%d per render ×%d) and the account has %d",
		duration, quality, plan.Cost, perRender, count, balance)
	return plan
}

// AccountChoice is which signed-in account would pay for a job of a given cost.
type AccountChoice struct {
	Cost int `json:"cost"`
	// Current is the account in use right now.
	Current int `json:"current_index"`
	// Accounts is every signed-in account the scan reached, so the decision can
	// be checked against the numbers it was made from rather than taken on trust.
	Accounts []AccountCredits `json:"accounts"`
	// ChosenIndex is the account that would be used, or -1 when none can pay.
	ChosenIndex int `json:"chosen_index"`
	// ChosenBalance is that account's balance, and is only meaningful when
	// ChosenIndex is not -1.
	ChosenBalance int `json:"chosen_balance,omitempty"`
	// WouldSwitch says the engine is not already on the chosen account.
	WouldSwitch bool `json:"would_switch"`
}

// ChooseAccountFor reports which signed-in account would pay for a job of the
// given cost, without moving to it.
//
// Read-only, and separate from the switch on purpose: it is how the decision can
// be inspected — and verified — without spending anything or changing which
// account is in use.
func (e *Engine) ChooseAccountFor(ctx context.Context, cost int) (*AccountChoice, error) {
	if cost < 0 {
		cost = 0
	}
	rows, err := e.AccountsCredits(ctx, config.AccountScanLimit)
	if err != nil {
		return nil, err
	}

	choice := &AccountChoice{
		Cost:     cost,
		Current:  e.AccountIndex(),
		Accounts: rows,
	}
	choice.ChosenIndex, choice.ChosenBalance = bestAffordableAccount(rows, cost)
	choice.WouldSwitch = choice.ChosenIndex != -1 && choice.ChosenIndex != choice.Current
	return choice, nil
}

// switchToAffordableAccount moves the engine onto a signed-in account that can
// pay for a job, when the one in use cannot.
//
// Returns true only when the engine is now on a *different* account. False
// covers both "the current account is already fine" and "nothing signed in can
// pay" — the caller re-plans either way and refuses if the plan is still
// unaffordable, so the two do not need to be told apart.
//
// The switch is a full Bootstrap, because the session, the account row and the
// project have to move together: a project belonging to one account submitted
// under another's session comes back empty. That is also why this is the whole
// mechanism rather than a second one — Bootstrap already knows how to resolve a
// project for a given account, so there is nothing new to invent.
//
// The chosen account is recorded, not just used for this request. The engine
// converges on an account that can pay, which is the point: sitting on one that
// cannot only guarantees the next refusal.
func (e *Engine) switchToAffordableAccount(ctx context.Context, cost int) bool {
	choice, err := e.ChooseAccountFor(ctx, cost)
	if err != nil {
		log.Printf("engine: could not read the signed-in balances to find an account that "+
			"can cover %d credits (%v)", cost, err)
		return false
	}

	if choice.ChosenIndex == -1 {
		log.Printf("engine: no signed-in account can cover %d credits; scanned %s",
			cost, describeBalances(choice.Accounts))
		return false
	}
	if !choice.WouldSwitch {
		return false
	}

	log.Printf("engine: account %d cannot cover %d credits, so the engine is moving to "+
		"account %d, which holds %d",
		choice.Current, cost, choice.ChosenIndex, choice.ChosenBalance)
	if err := e.SetAccountIndex(ctx, choice.ChosenIndex); err != nil {
		log.Printf("engine: could not move to account %d (%v); staying on account %d",
			choice.ChosenIndex, err, choice.Current)
		return false
	}
	return true
}

// bestAffordableAccount returns the index and balance of the signed-in account
// with the most credits that can still cover cost.
//
// Returns -1 when none can. An account whose balance could not be read is not a
// candidate: `Credits` is nil precisely when the read failed, and treating that
// as a balance would move the engine onto an account on no evidence.
//
// It is separate from the scan so the choice can be tested without a network —
// this is the decision that picks which wallet gets charged.
func bestAffordableAccount(rows []AccountCredits, cost int) (index, balance int) {
	index, balance = -1, 0
	for _, row := range rows {
		if !row.SignedIn || row.Credits == nil || *row.Credits < cost {
			continue
		}
		if index == -1 || *row.Credits > balance {
			index, balance = row.Index, *row.Credits
		}
	}
	return index, balance
}

// describeBalances renders a scan for a log line.
func describeBalances(rows []AccountCredits) string {
	if len(rows) == 0 {
		return "nothing readable"
	}
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		balance := "unknown"
		if row.Credits != nil {
			balance = strconv.Itoa(*row.Credits)
		}
		parts = append(parts, fmt.Sprintf("%d=%s", row.Index, balance))
	}
	return "accounts " + strings.Join(parts, ", ")
}

// canMoveAccounts reports whether a request may be served by a different
// signed-in account than the one in use.
//
// Moving accounts moves the **project** — they are per-account, which is why
// switching re-bootstraps — and any asset id the caller supplied is scoped to the
// project of the account that supplied it. So a request conditioned on an existing
// asset cannot be moved: the id is not in the new account's project, and the
// render fails after a long wait spent looking for it.
//
// This is not theoretical. An image generated on one account was used as the
// start frame of a request that the engine then moved to a second account to
// afford; resolution retried its full window against the wrong project and failed
// with "not in the project listing" for an asset that was in the listing all
// along, just not that one.
//
// A request with no conditioning carries no such id and can be served anywhere.
func canMoveAccounts(req BatchVideoRequest) bool {
	return strings.TrimSpace(req.StartImage) == "" && strings.TrimSpace(req.EndImage) == ""
}

// GenerateVideoViaBatch submits a video generation over batchexecute.
func (e *Engine) GenerateVideoViaBatch(ctx context.Context, req BatchVideoRequest) (*BatchVideoOutcome, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("engine: a prompt is required")
	}

	model := req.Model
	if model == "" {
		key, ok := config.VideoModelFor(req.Duration, req.StartImage != "", req.EndImage != "")
		if !ok {
			duration := req.Duration
			if duration == 0 {
				duration = config.DefaultDuration
			}
			return nil, fmt.Errorf("engine: unsupported duration %ds; use one of %v", duration, config.Durations)
		}
		model = key
	}

	// The quality rides on the model key as a `_360p` suffix, and it is the
	// expensive axis rather than a cosmetic one: a 4s render costs 4 credits at
	// 360p against 7 at 720p. Applying it after the override means a caller who
	// names a model outright still gets the quality they asked for.
	quality := config.NormalizeVideoQuality(req.Quality)
	model = config.VideoModelQuality(model, quality)

	// The model key is the authority on quality, not the field.
	//
	// A caller can name a model outright, and `abra_t2v_4s_360p` is a 360p render
	// whatever `quality` says. Pricing it off the field alone would quote the
	// 720p cost for a 360p render, which overstates what the account needs and
	// refuses requests that would in fact have been served.
	if strings.HasSuffix(model, "_360p") {
		quality = "360p"
	}

	projectID := req.ProjectID
	if projectID == "" {
		projectID = e.ProjectID()
	}
	if projectID == "" {
		return nil, fmt.Errorf("engine: no project id resolved")
	}

	jobID := uuid.NewString()
	start := time.Now()

	rowID, err := e.store.RecordGeneration(store.Generation{
		JobID:    jobID,
		Kind:     "video",
		Prompt:   req.Prompt,
		Model:    model,
		Duration: req.Duration,
		Count:    req.Count,
		Status:   "submitted",
	})
	if err != nil {
		log.Printf("engine: could not record the job: %v", err)
	}
	_ = rowID

	jar := e.bridge.Jar()
	if jar == nil {
		err := fmt.Errorf("engine: no cookies loaded")
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, err
	}

	client := e.newBatchexecuteClient(jar, e.hc)

	// Keep the pair as requested. A retry against another account has to re-plan
	// from what was asked for, not from a downgrade the previous account forced —
	// otherwise a richer account is handed the cheaper render it never needed.
	requestedModel, requestedQuality := model, quality

	// Decide whether this account can pay for the render before anything is spent
	// on the request.
	//
	// This runs ahead of the reCAPTCHA mint deliberately: a token is a browser
	// round trip, and minting one for a render the balance cannot cover wastes it
	// and buries the real reason behind a captcha failure.
	plan := e.planVideo(ctx, client, req, requestedModel, requestedQuality)
	if !plan.Affordable() && canMoveAccounts(req) {
		// The account in use cannot pay. Another signed-in account might, and
		// preferring the one that can is the whole reason for having several — so
		// look before refusing.
		if e.switchToAffordableAccount(ctx, plan.Cost) {
			// The switch re-bootstrapped, so both the client and the project now
			// belong to the account that was just left behind and have to be
			// rebuilt before anything is sent under them.
			if switchedJar := e.bridge.Jar(); switchedJar != nil {
				jar = switchedJar
				client = e.newBatchexecuteClient(jar, e.hc)
				if id := e.ProjectID(); id != "" {
					projectID = id
				}
				plan = e.planVideo(ctx, client, req, requestedModel, requestedQuality)
			}
		}
	}
	if !plan.Affordable() {
		err := fmt.Errorf("engine: %s", plan.Reason)
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, err
	}
	model, quality = plan.Model, plan.Quality

	// startImage/endImage take content ids, not media ids. Resolve whatever the
	// caller supplied so a freshly uploaded file's media id also works.
	startImage, err := e.conditionImageID(ctx, req.StartImage)
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, err
	}
	endImage, err := e.conditionImageID(ctx, req.EndImage)
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, err
	}

	captcha, err := e.CaptchaToken(ctx, recaptcha.ActionVideo)
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, fmt.Errorf("engine: could not obtain a reCAPTCHA token: %w", err)
	}

	frames, err := client.GenerateVideo(ctx, batchexecute.GenerateVideoRequest{
		ProjectID:    projectID,
		Model:        model,
		Prompt:       req.Prompt,
		Count:        req.Count,
		CaptchaToken: captcha,
		StartImage:   startImage,
		EndImage:     endImage,
		StartFrame:   req.StartFrame,
		EndFrame:     req.EndFrame,
	}, e.captchaOptions(recaptcha.ActionVideo, batchexecute.CallOptions{
		SourcePath: "/project/" + projectID,
		BuildLabel: config.BuildLabel(),
	}))
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		_ = e.store.RecordAccountOutcome(e.AccountID(), true, err.Error())
		return nil, err
	}

	outcome := &BatchVideoOutcome{
		JobID:       jobID,
		AccountID:   e.AccountID(),
		ProjectID:   projectID,
		Model:       model,
		Quality:     quality,
		CreditsCost: plan.Cost,
		Status:      "submitted",
	}
	for _, frame := range frames {
		outcome.MediaIDs = append(outcome.MediaIDs, batchexecute.ParseGeneratedMediaIDs(frame.Payload)...)
	}
	if len(outcome.MediaIDs) == 0 {
		outcome.Status = "empty"
		for _, frame := range frames {
			outcome.RawFrames = append(outcome.RawFrames, frame.Payload)
		}
	}

	// The response carries the balance alongside the media, so record it rather
	// than leaving the pool's figure stale.
	//
	// It gets its own deadline. This is bookkeeping — it cannot change the outcome,
	// and the media ids are already in hand — but on the request context it could
	// block the whole submission. That is how a video that had been accepted sat
	// at "submitted" with no log line and nothing to show for it: the request was
	// waiting on a balance read that nobody needed.
	creditCtx, cancelCredits := context.WithTimeout(ctx, creditsReadTimeout)
	credits, credErr := client.Credits(creditCtx, batchexecute.CallOptions{
		SourcePath: "/", BuildLabel: config.BuildLabel(),
	})
	cancelCredits()
	if credErr == nil {
		outcome.Credits = credits
	} else {
		log.Printf("engine: could not read the balance after submitting (%v); the submission "+
			"itself is unaffected", credErr)
	}

	// Video renders asynchronously, so the URL only exists once the render is
	// done. Polling is opt-in because it can take minutes.
	if (req.Wait || req.Download) && len(outcome.MediaIDs) > 0 {
		urls, files := e.collectVideo(ctx, client, projectID, outcome.MediaIDs, req.Prompt, rowID, req.Download)
		outcome.URLs = urls
		outcome.Files = files
		if len(urls) > 0 {
			outcome.Status = "ready"
		}
	}

	// What the render actually cost, as a delta rather than the table's figure.
	//
	// The table says what the request was *expected* to cost; only the balance
	// moving proves it was charged. The delta is also the only way the table
	// itself gets checked against reality — `credits_spent` was previously
	// written as NULL on every job, so the recorded costs had never been
	// confirmed by a single real submission.
	var spent *int
	if plan.Known && credErr == nil && credits < plan.Balance {
		delta := plan.Balance - credits
		spent = &delta
	}

	outcome.ElapsedS = time.Since(start).Seconds()
	e.finishJob(jobID, outcome.Status, spent, start, nil)
	_ = e.store.RecordAccountOutcome(e.AccountID(), false, "")

	if len(outcome.MediaIDs) == 0 {
		// Say what it cost and what is left.
		//
		// The server accepts a submission the balance cannot cover and answers
		// with no media rather than an error, so "submitted 0 videos" was the
		// whole report — which reads as a broken request. It sent someone looking
		// for a wrong argument for hours when the answer was an empty wallet.
		//
		// The affordability check has already passed by this point, so the
		// balance is no longer a candidate cause and the line says so. Reporting
		// it as the likely culprit here would send the next reader after the same
		// wrong answer.
		balance := "unknown"
		if plan.Known {
			balance = fmt.Sprintf("%d", plan.Balance)
		}
		if plan.Cost > 0 {
			log.Printf("engine: nothing submitted for %s — it costs %d credits at %s and the "+
				"account has %s, which was checked and covers it; so the balance is not the "+
				"cause. %s",
				model, plan.Cost, quality, balance, e.emptySubmissionHint())
		} else {
			log.Printf("engine: nothing submitted for %s at %s — the cost of that pair is not "+
				"recorded, so the balance cannot be ruled in or out. %s",
				model, quality, e.emptySubmissionHint())
		}
		return outcome, nil
	}

	log.Printf("engine: submitted %d video(s) as %s in %.1fs",
		len(outcome.MediaIDs), model, outcome.ElapsedS)
	return outcome, nil
}

// BatchEditRequest asks for an edit of an existing asset.
type BatchEditRequest struct {
	// Source is the asset to edit, as a media id or a content id. A media id is
	// resolved to its content id before submitting.
	Source string
	// Prompt describes the edit, e.g. "make the boat drift slowly to the left".
	Prompt string
	// Model defaults to config.EditModel.
	Model     string
	ProjectID string
	// Wait polls until the render is downloadable. Download implies Wait.
	Wait     bool
	Download bool
}

// EditVideoViaBatch submits a video edit over batchexecute.
//
// abra_edit is the video-to-video model, and the app reaches it from the
// Ingredients composer mode once an existing asset is attached. The submission
// goes to jIps6 — a different RPC *and* a different payload shape from a
// generation: the source sits in its own block ahead of the prompt. Sending a
// generation layout here is accepted and returns an empty result, which is what
// made this look unimplemented rather than mis-shaped.
func (e *Engine) EditVideoViaBatch(ctx context.Context, req BatchEditRequest) (*BatchVideoOutcome, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	if strings.TrimSpace(req.Source) == "" {
		return nil, fmt.Errorf("engine: a source asset is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("engine: a prompt is required")
	}

	model := req.Model
	if model == "" {
		model = config.EditModel
	}
	projectID := req.ProjectID
	if projectID == "" {
		projectID = e.ProjectID()
	}
	if projectID == "" {
		return nil, fmt.Errorf("engine: no project id resolved")
	}

	jobID := uuid.NewString()
	start := time.Now()

	rowID, err := e.store.RecordGeneration(store.Generation{
		JobID:  jobID,
		Kind:   "video",
		Prompt: req.Prompt,
		Model:  model,
		Status: "submitted",
	})
	if err != nil {
		log.Printf("engine: could not record the job: %v", err)
	}
	_ = rowID

	captcha, err := e.CaptchaToken(ctx, recaptcha.ActionVideo)
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, fmt.Errorf("engine: could not obtain a reCAPTCHA token: %w", err)
	}

	jar := e.bridge.Jar()
	if jar == nil {
		err := fmt.Errorf("engine: no cookies loaded")
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, err
	}
	client := e.newBatchexecuteClient(jar, e.hc)

	source, err := e.conditionImageID(ctx, req.Source)
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, err
	}

	frames, err := client.GenerateVideoEdit(ctx, batchexecute.EditVideoRequest{
		ProjectID:    projectID,
		SourceID:     source,
		Prompt:       req.Prompt,
		Model:        model,
		CaptchaToken: captcha,
	}, e.captchaOptions(recaptcha.ActionVideo, batchexecute.CallOptions{
		SourcePath: "/project/" + projectID,
		BuildLabel: config.BuildLabel(),
	}))
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		_ = e.store.RecordAccountOutcome(e.AccountID(), true, err.Error())
		return nil, err
	}

	outcome := &BatchVideoOutcome{
		JobID:     jobID,
		AccountID: e.AccountID(),
		ProjectID: projectID,
		Model:     model,
		Status:    "submitted",
	}
	for _, frame := range frames {
		outcome.MediaIDs = append(outcome.MediaIDs, batchexecute.ParseGeneratedMediaIDs(frame.Payload)...)
	}
	if len(outcome.MediaIDs) == 0 {
		outcome.Status = "empty"
	}

	if credits, credErr := client.Credits(ctx, batchexecute.CallOptions{
		SourcePath: "/", BuildLabel: config.BuildLabel(),
	}); credErr == nil {
		outcome.Credits = credits
	}

	if (req.Wait || req.Download) && len(outcome.MediaIDs) > 0 {
		urls, files := e.collectVideo(ctx, client, projectID, outcome.MediaIDs, req.Prompt, rowID, req.Download)
		outcome.URLs = urls
		outcome.Files = files
		if len(urls) > 0 {
			outcome.Status = "ready"
		}
	}

	outcome.ElapsedS = time.Since(start).Seconds()
	e.finishJob(jobID, outcome.Status, nil, start, nil)
	_ = e.store.RecordAccountOutcome(e.AccountID(), false, "")

	log.Printf("engine: submitted an edit of %s as %s in %.1fs",
		shortID(source), model, outcome.ElapsedS)
	return outcome, nil
}

// BatchReferenceRequest asks for a generation conditioned on reference images.
type BatchReferenceRequest struct {
	// References are the reference assets, as media ids or content ids. Media ids
	// are resolved to content ids before submitting.
	References []string
	Prompt     string
	// Model defaults to the reference model for Duration.
	Model     string
	Duration  int
	ProjectID string
	// Wait polls until the render is downloadable. Download implies Wait.
	Wait     bool
	Download bool
}

// GenerateVideoFromReferencesViaBatch submits a reference-image generation.
//
// This is the `abra_r2v_*` family, and it is the last capability to be found
// because it is the one that hides best: an `abra_r2v_*` model sent to the
// single-image RPC is *accepted and returns a media id*, so a probe looks like a
// success. The real submission goes to MZZa6b with a payload that puts the prompt
// at index 0 and the reference list at index 1 — not the i2v arrangement.
func (e *Engine) GenerateVideoFromReferencesViaBatch(ctx context.Context, req BatchReferenceRequest) (*BatchVideoOutcome, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("engine: a prompt is required")
	}
	if len(req.References) == 0 {
		return nil, fmt.Errorf("engine: at least one reference image is required")
	}

	model := req.Model
	if model == "" {
		key, ok := config.ReferenceVideoModelFor(req.Duration)
		if !ok {
			duration := req.Duration
			if duration == 0 {
				duration = config.DefaultDuration
			}
			return nil, fmt.Errorf("engine: unsupported duration %ds; use one of %v", duration, config.Durations)
		}
		model = key
	}
	projectID := req.ProjectID
	if projectID == "" {
		projectID = e.ProjectID()
	}
	if projectID == "" {
		return nil, fmt.Errorf("engine: no project id resolved")
	}

	jobID := uuid.NewString()
	start := time.Now()

	rowID, err := e.store.RecordGeneration(store.Generation{
		JobID:    jobID,
		Kind:     "video",
		Prompt:   req.Prompt,
		Model:    model,
		Duration: req.Duration,
		Status:   "submitted",
	})
	if err != nil {
		log.Printf("engine: could not record the job: %v", err)
	}
	_ = rowID

	captcha, err := e.CaptchaToken(ctx, recaptcha.ActionVideo)
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, fmt.Errorf("engine: could not obtain a reCAPTCHA token: %w", err)
	}

	jar := e.bridge.Jar()
	if jar == nil {
		err := fmt.Errorf("engine: no cookies loaded")
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, err
	}
	client := e.newBatchexecuteClient(jar, e.hc)

	// The RPC takes content ids, the same as the condition images do.
	refs := make([]string, 0, len(req.References))
	for _, ref := range req.References {
		resolved, err := e.conditionImageID(ctx, ref)
		if err != nil {
			e.finishJob(jobID, "failed", nil, start, err)
			return nil, err
		}
		refs = append(refs, resolved)
	}

	frames, err := client.GenerateVideoFromReferences(ctx, batchexecute.ReferenceVideoRequest{
		ProjectID:    projectID,
		Model:        model,
		Prompt:       req.Prompt,
		References:   refs,
		CaptchaToken: captcha,
	}, e.captchaOptions(recaptcha.ActionVideo, batchexecute.CallOptions{
		SourcePath: "/project/" + projectID,
		BuildLabel: config.BuildLabel(),
	}))
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		_ = e.store.RecordAccountOutcome(e.AccountID(), true, err.Error())
		return nil, err
	}

	outcome := &BatchVideoOutcome{
		JobID:     jobID,
		AccountID: e.AccountID(),
		ProjectID: projectID,
		Model:     model,
		Status:    "submitted",
	}
	for _, frame := range frames {
		outcome.MediaIDs = append(outcome.MediaIDs, batchexecute.ParseGeneratedMediaIDs(frame.Payload)...)
	}
	if len(outcome.MediaIDs) == 0 {
		outcome.Status = "empty"
	}

	if credits, credErr := client.Credits(ctx, batchexecute.CallOptions{
		SourcePath: "/", BuildLabel: config.BuildLabel(),
	}); credErr == nil {
		outcome.Credits = credits
	}

	if (req.Wait || req.Download) && len(outcome.MediaIDs) > 0 {
		urls, files := e.collectVideo(ctx, client, projectID, outcome.MediaIDs, req.Prompt, rowID, req.Download)
		outcome.URLs = urls
		outcome.Files = files
		if len(urls) > 0 {
			outcome.Status = "ready"
		}
	}

	outcome.ElapsedS = time.Since(start).Seconds()
	e.finishJob(jobID, outcome.Status, nil, start, nil)
	_ = e.store.RecordAccountOutcome(e.AccountID(), false, "")

	log.Printf("engine: submitted %d reference(s) as %s in %.1fs",
		len(refs), model, outcome.ElapsedS)
	return outcome, nil
}

// LabsAccessToken mints (or returns a cached) Labs access token.
//
// Diagnostics only: it exists so the legacy aisandbox REST surface can be probed
// from outside the process, which is the only way to establish whether that
// surface still works before building on it.
func (e *Engine) LabsAccessToken(ctx context.Context) (string, error) {
	jar := e.bridge.Jar()
	if jar == nil {
		return "", fmt.Errorf("engine: no cookies loaded")
	}
	return auth.NewProvider(jar, e.hc).AccessToken(ctx)
}

// UploadImageViaBatch uploads an image into the project over batchexecute and
// returns its media id, which is what a generation conditions on.
//
// This is the batchexecute counterpart of the legacy REST upload. The legacy one
// answers 200 and returns an id, but that id is invisible to everything on this
// transport, so it cannot be used to condition a generation.
func (e *Engine) UploadImageViaBatch(ctx context.Context, data []byte, mimeType, fileName string) (mediaID, contentID string, err error) {
	if !e.Ready() {
		return "", "", fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	if len(data) == 0 {
		return "", "", fmt.Errorf("engine: refusing to upload an empty file")
	}

	projectID := e.ProjectID()
	if projectID == "" {
		return "", "", fmt.Errorf("engine: no project id resolved")
	}

	jar := e.bridge.Jar()
	if jar == nil {
		return "", "", fmt.Errorf("engine: no cookies loaded")
	}

	captcha, err := e.CaptchaToken(ctx, recaptcha.ActionImage)
	if err != nil {
		return "", "", fmt.Errorf("engine: could not obtain a reCAPTCHA token: %w", err)
	}

	client := e.newBatchexecuteClient(jar, e.hc)
	mediaID, contentID, err = client.UploadMedia(ctx, batchexecute.UploadMediaRequest{
		ProjectID:    projectID,
		Data:         data,
		MimeType:     mimeType,
		FileName:     fileName,
		CaptchaToken: captcha,
	}, e.captchaOptions(recaptcha.ActionImage, batchexecute.CallOptions{}))
	if err != nil {
		return "", "", err
	}

	log.Printf("engine: uploaded %s as media %s (content %s)", fileName, shortID(mediaID), shortID(contentID))
	return mediaID, contentID, nil
}

// assetResolveWindow bounds how long an id is retried while the project listing
// catches up with a just-completed upload.
//
// An upload returns its ids before the listing carries the row — measured at about
// eight seconds — so conditioning on an upload that had just succeeded reported
// "not in the project listing" for an asset that was there by the time anyone
// looked. The window is deliberately short: a genuinely absent id is still a client
// error and should be reported as one rather than waited on.
//
// Both this and the interval are variables so a test can shrink them; thirty
// seconds of real waiting in a unit test is not a test anyone runs.
var (
	assetResolveWindow   = 30 * time.Second
	assetResolveInterval = 3 * time.Second
)

// awaitAsset retries resolve until it finds the asset or the window closes.
//
// Shared by both id-to-content-id paths, because either can be handed an id from an
// upload that has not reached the listing yet. `resolve` reports whether it found
// the asset; an error from it is returned at once, since a failed listing call is
// not something waiting will fix.
func awaitAsset(ctx context.Context, resolve func() (bool, error)) (bool, error) {
	deadline := time.Now().Add(assetResolveWindow)
	for {
		found, err := resolve()
		if err != nil || found {
			return found, err
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-time.After(assetResolveInterval):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// conditionImageID resolves whatever the caller supplied into the id a generation
// actually needs — the asset's *content* id.
//
// The difference is easy to miss: the app sends content ids in
// startImage/endImage, and a submission carrying a media id instead is accepted
// and returns an empty result. A caller who has just uploaded a file naturally
// holds the media id, so both are accepted here and mapped.
//
// An id that is not in the listing yet is retried rather than rejected, for the
// upload reason above.
func (e *Engine) conditionImageID(ctx context.Context, id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", nil
	}
	if !e.Ready() {
		return "", fmt.Errorf("engine: not ready — call Bootstrap first")
	}

	jar := e.bridge.Jar()
	if jar == nil {
		return "", fmt.Errorf("engine: no cookies loaded")
	}
	client := e.newBatchexecuteClient(jar, e.hc)

	resolved := ""
	found, err := awaitAsset(ctx, func() (bool, error) {
		assets, err := client.ProjectAssets(ctx, e.ProjectID())
		if err != nil {
			return false, err
		}
		for _, asset := range assets {
			if asset.ContentID == id {
				resolved = id // already a content id
				return true, nil
			}
		}
		for _, asset := range assets {
			if asset.MediaID == id {
				resolved = asset.ContentID
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf(
			"engine: %s is not in the project listing, so its content id cannot be resolved", id)
	}
	return resolved, nil
}

// ResolveContentID looks up an asset's content id from its media id.
//
// Callers normally hold a media id — it is what a generation returns and what the
// UI URL carries — while the media-detail and upscale RPCs take the content id.
// The two differ, so this is the bridge between them.
func (e *Engine) ResolveContentID(ctx context.Context, mediaID string) (string, error) {
	if strings.TrimSpace(mediaID) == "" {
		return "", fmt.Errorf("engine: a media id is required")
	}
	if !e.Ready() {
		return "", fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	projectID := e.ProjectID()
	if projectID == "" {
		return "", fmt.Errorf("engine: no project id resolved")
	}
	jar := e.bridge.Jar()
	if jar == nil {
		return "", fmt.Errorf("engine: no cookies loaded")
	}

	client := e.newBatchexecuteClient(jar, e.hc)

	var asset batchexecute.ProjectAsset
	found := false
	if _, err := awaitAsset(ctx, func() (bool, error) {
		var resolveErr error
		asset, found, resolveErr = e.findAsset(ctx, client, projectID, mediaID)
		return found, resolveErr
	}); err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf(
			"engine: %s is not in the project listing, so its content id cannot be resolved", mediaID)
	}
	return asset.ContentID, nil
}

// findAsset resolves a media id to its project asset, which carries the content
// id the upscale RPC needs. The two ids differ for videos, so the caller's media
// id cannot stand in for it.
//
// One media id can have several rows — an original and each derived asset share
// it — and they are not interchangeable. Picking the first match hands back a
// derived asset, which the upscale RPC answers with nothing at all, so the
// original is preferred explicitly.
//
// `found` is separate from `err` so a caller can tell "this id is not in the
// listing" — which is worth waiting on after an upload — from "the listing call
// failed", which is not.
func (e *Engine) findAsset(ctx context.Context, client *batchexecute.Client, projectID, mediaID string) (asset batchexecute.ProjectAsset, found bool, err error) {
	assets, err := client.ProjectAssets(ctx, projectID)
	if err != nil {
		return batchexecute.ProjectAsset{}, false, err
	}

	var first, unsuffixed *batchexecute.ProjectAsset
	for i := range assets {
		if assets[i].MediaID != mediaID {
			continue
		}
		if first == nil {
			first = &assets[i]
		}
		if assets[i].TypeCode == batchexecute.AssetTypeOriginal {
			return assets[i], true, nil
		}
		if unsuffixed == nil && !strings.HasSuffix(assets[i].ContentID, "_upsampled") {
			unsuffixed = &assets[i]
		}
	}
	if unsuffixed != nil {
		return *unsuffixed, true, nil
	}
	if first != nil {
		return *first, true, nil
	}
	return batchexecute.ProjectAsset{}, false, nil
}

// waitForNewVideo polls the project listing until a video that was not there
// before appears, and returns its id.
//
// Kept for the generation path, which genuinely cannot know the id in advance.
func (e *Engine) waitForNewVideo(ctx context.Context, client *batchexecute.Client, projectID, sourceID string) []string {
	before := map[string]bool{}
	if assets, err := client.ProjectAssets(ctx, projectID); err == nil {
		for _, asset := range assets {
			before[asset.MediaID] = true
		}
	}

	deadline := time.Now().Add(time.Duration(config.PollTimeout) * time.Second)
	interval := time.Duration(config.PollInterval) * time.Second

	for time.Now().Before(deadline) {
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return nil
		}

		assets, err := client.ProjectAssets(ctx, projectID)
		if err != nil {
			continue
		}
		for _, asset := range assets {
			if before[asset.MediaID] || asset.MediaID == sourceID {
				continue
			}
			// The new asset must itself be downloadable before it counts.
			if _, err := client.ResolveVideoURL(ctx, projectID, asset.MediaID); err != nil {
				continue
			}
			return []string{asset.MediaID}
		}
	}
	return nil
}

// collectVideo waits for each submitted video to become downloadable, then
// optionally saves it.
//
// It polls the project listing rather than any status endpoint, because that is
// where the render surfaces: the media-detail RPC only returns a video URL once
// the render is finished.
func (e *Engine) collectVideo(ctx context.Context, client *batchexecute.Client, projectID string,
	mediaIDs []string, prompt string, rowID int64, download bool) ([]string, []MediaFile) {

	deadline := time.Now().Add(time.Duration(config.PollTimeout) * time.Second)
	interval := time.Duration(config.PollInterval) * time.Second

	pending := make(map[string]bool, len(mediaIDs))
	for _, id := range mediaIDs {
		pending[id] = true
	}

	var urls []string
	var files []MediaFile
	var failed []string
	// The last reason each id could not be resolved, kept so an expiry can say
	// why instead of only that it ran out of time.
	//
	// Every error here used to be discarded as "not ready yet", which is right for
	// a render in flight and wrong for a permanent one — a wrong project, a media
	// id that never landed, an RPC that moved. Both look identical from the
	// outside, and the wait expiring is exactly when the difference matters.
	lastErr := make(map[string]string, len(mediaIDs))

	for len(pending) > 0 && time.Now().Before(deadline) {
		for mediaID := range pending {
			url, err := client.ResolveVideoURL(ctx, projectID, mediaID)
			if err != nil {
				lastErr[mediaID] = err.Error()

				// A render in flight fails this in two stages, and both are worth
				// waiting for: first the asset is missing from the listing, then it
				// appears while its URL is still being produced. Anything else — a
				// media-detail call that answers nothing, a listing that moved, an id
				// that never landed — is permanent, and polling it for the full
				// timeout turns a ten-second failure into a seven-minute one.
				if !batchexecute.RetryableResolveError(err) {
					log.Printf("engine: giving up on %s — %v", shortID(mediaID), err)
					delete(pending, mediaID)
					failed = append(failed, mediaID)
				}
				continue
			}
			delete(pending, mediaID)
			urls = append(urls, url)

			if download {
				file, dlErr := e.downloadGenerated(ctx, batchexecute.GeneratedMedia{
					MediaID: mediaID, URL: url, Kind: "video",
				}, "video", prompt, rowID)
				if dlErr != nil {
					log.Printf("engine: could not save video %s: %v", shortID(mediaID), dlErr)
					continue
				}
				files = append(files, file)
			}
		}

		if len(pending) == 0 {
			break
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return urls, files
		}
	}

	if len(failed) > 0 {
		log.Printf("engine: %d video(s) will never resolve — the render may well have "+
			"finished, and the media is reachable in the app; what failed is the lookup "+
			"from here", len(failed))
	}
	if len(pending) > 0 {
		log.Printf("engine: %d video(s) still unresolved after %ds", len(pending), config.PollTimeout)
		for mediaID := range pending {
			// A render that is genuinely in flight reports "not in the project
			// listing yet", so anything else is a real failure worth naming.
			log.Printf("engine:   %s — last reason: %s", shortID(mediaID), lastErr[mediaID])
		}
	}
	return urls, files
}

/* ------------------------------------------------------------------ *
 * Generation over batchexecute
 * ------------------------------------------------------------------ */

// BatchImageRequest is a caller-facing image request for the batchexecute path.
type BatchImageRequest struct {
	Prompt string
	// Model is the image enum: NARWHAL (Nano Banana), HARBOR_SEAL (Nano Banana 2
	// Lite), or GEM_PIX_2 (Nano Banana Pro).
	Model string
	// ProjectID overrides the resolved project.
	ProjectID string
	// Download writes the result to output/.
	Download bool
}

// BatchOutcome is the result of a batchexecute generation.
type BatchOutcome struct {
	JobID     string                        `json:"job_id"`
	AccountID string                        `json:"account_id"`
	ProjectID string                        `json:"project_id"`
	Model     string                        `json:"model"`
	Media     []batchexecute.GeneratedMedia `json:"media"`
	Files     []MediaFile                   `json:"files,omitempty"`
	ElapsedS  float64                       `json:"elapsed_seconds"`
	Status    string                        `json:"status"`
}

// GenerateImageViaBatch generates an image over the batchexecute transport.
//
// This is the working path. The REST client in flowapi targets
// aisandbox-pa.googleapis.com, which the current app no longer uses and whose key
// blocks the app's own origin; it is kept only for the endpoints that still work
// there (credits) and as a record of what was tried.
func (e *Engine) GenerateImageViaBatch(ctx context.Context, req BatchImageRequest) (*BatchOutcome, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("engine: a prompt is required")
	}

	model := batchImageModel(req.Model)
	projectID := req.ProjectID
	if projectID == "" {
		projectID = e.ProjectID()
	}
	if projectID == "" {
		return nil, fmt.Errorf("engine: no project id resolved")
	}

	jobID := uuid.NewString()
	start := time.Now()

	rowID, err := e.store.RecordGeneration(store.Generation{
		JobID:  jobID,
		Kind:   "image",
		Prompt: req.Prompt,
		Model:  model,
		Status: "submitted",
	})
	if err != nil {
		log.Printf("engine: could not record the job: %v", err)
	}

	// A fresh token per call. The page mints one per generation, and the value
	// is embedded in the request, so reusing one across submissions is not
	// something the app ever does.
	captcha, err := e.CaptchaToken(ctx, recaptcha.ActionImage)
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, fmt.Errorf("engine: could not obtain a reCAPTCHA token: %w", err)
	}

	jar := e.bridge.Jar()
	if jar == nil {
		err := fmt.Errorf("engine: no cookies loaded")
		e.finishJob(jobID, "failed", nil, start, err)
		return nil, err
	}

	client := e.newBatchexecuteClient(jar, e.hc)
	media, err := client.GenerateMedia(ctx, batchexecute.GenerateRequest{
		ProjectID:    projectID,
		Model:        model,
		Prompt:       req.Prompt,
		CaptchaToken: captcha,
	}, e.captchaOptions(recaptcha.ActionImage, batchexecute.CallOptions{
		SourcePath: "/project/" + projectID,
		BuildLabel: config.BuildLabel(),
	}))
	if err != nil {
		e.finishJob(jobID, "failed", nil, start, err)
		_ = e.store.RecordAccountOutcome(e.AccountID(), true, err.Error())
		return nil, err
	}

	outcome := &BatchOutcome{
		JobID:     jobID,
		AccountID: e.AccountID(),
		ProjectID: projectID,
		Model:     model,
		Media:     media,
		Status:    "succeeded",
	}
	if len(media) == 0 {
		// Accepted but nothing came back. The app's own call returns the asset
		// inline, so this means the request was a no-op — worth surfacing rather
		// than reporting success on an empty result.
		outcome.Status = "empty"
	}

	if req.Download {
		for _, item := range media {
			if item.URL == "" {
				continue
			}
			file, dlErr := e.downloadGenerated(ctx, item, "image", req.Prompt, rowID)
			if dlErr != nil {
				log.Printf("engine: could not download %s: %v", shortID(item.MediaID), dlErr)
				continue
			}
			outcome.Files = append(outcome.Files, file)
		}
	}

	outcome.ElapsedS = time.Since(start).Seconds()
	e.finishJob(jobID, outcome.Status, nil, start, nil)
	_ = e.store.RecordAccountOutcome(e.AccountID(), false, "")

	log.Printf("engine: generated %d image(s) in %.1fs", len(media), outcome.ElapsedS)
	return outcome, nil
}

// batchImageModel resolves a friendly model name to the enum the RPC expects.
func batchImageModel(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "narwhal", "nano_banana", "standard", "nano_banana_2":
		return "NARWHAL"
	case "harbor_seal", "lite", "nano_banana_2_lite":
		return "HARBOR_SEAL"
	case "gem_pix_2", "pro", "nano_banana_pro":
		return "GEM_PIX_2"
	}
	// Already an enum.
	return strings.ToUpper(name)
}

// downloadGenerated fetches a signed asset URL and records it.
//
// The signed URL needs no credentials — it was verified to return 200 with no
// headers at all — so this does not attach cookies.
func (e *Engine) downloadGenerated(ctx context.Context, item batchexecute.GeneratedMedia, kind, prompt string, generationRow int64) (MediaFile, error) {
	resp, err := e.hc.Do(ctx, &httpx.Request{
		Method:      "GET",
		URL:         item.URL,
		DisableQUIC: true,
		Headers: map[string]string{
			"Accept":  "image/*,video/*,*/*",
			"Referer": batchexecute.Origin + "/",
		},
	})
	if err != nil {
		return MediaFile{}, err
	}
	if resp.Status != 200 {
		return MediaFile{}, fmt.Errorf("download returned %d", resp.Status)
	}
	if len(resp.Body) < 1000 {
		return MediaFile{}, fmt.Errorf("only %d bytes returned", len(resp.Body))
	}

	// Take the extension from the response, not from the requested kind. Flow
	// serves images as JPEG regardless of what was asked for, so a hardcoded
	// ".png" produces a mislabelled file that other tools will refuse to open.
	ext := extensionForContentType(resp.Header.Get("Content-Type"), kind)
	path := filepath.Join(config.OutputDir(), shortID(item.MediaID)+ext)

	if err := os.WriteFile(path, resp.Body, 0o644); err != nil {
		return MediaFile{}, err
	}

	file := MediaFile{MediaID: item.MediaID, Path: path, Bytes: int64(len(resp.Body))}
	e.recordMedia(generationRow, file, kind, prompt, item.URL)
	log.Printf("engine: saved %s (%.1f MB, %s)", filepath.Base(path),
		float64(file.Bytes)/(1024*1024), resp.Header.Get("Content-Type"))
	return file, nil
}

// extensionForContentType maps a media content type to a file extension,
// falling back to the kind when the header is missing or unrecognised.
func extensionForContentType(contentType, kind string) string {
	switch {
	case strings.Contains(contentType, "png"):
		return ".png"
	case strings.Contains(contentType, "jpeg"), strings.Contains(contentType, "jpg"):
		return ".jpg"
	case strings.Contains(contentType, "webp"):
		return ".webp"
	case strings.Contains(contentType, "mp4"), strings.Contains(contentType, "video"):
		return ".mp4"
	}
	if kind == "image" {
		return ".jpg"
	}
	return ".mp4"
}

/* ------------------------------------------------------------------ *
 * Download helpers
 * ------------------------------------------------------------------ */

func (e *Engine) downloadAll(ctx context.Context, mediaIDs []string, kind, resolution, prompt string, generationRow int64) ([]MediaFile, error) {
	e.mu.RLock()
	client := e.client
	e.mu.RUnlock()
	if client == nil {
		return nil, fmt.Errorf("engine: no active client")
	}

	var files []MediaFile
	var firstErr error

	for _, mediaID := range mediaIDs {
		preferUpsampled := resolution != "" && resolution != config.NativeVideoResolution
		url, err := client.MediaURL(ctx, mediaID, preferUpsampled)
		if err != nil {
			// Fall back to the legacy inline endpoint before giving up.
			data, legacyErr := client.LegacyMediaBase64(ctx, mediaID)
			if legacyErr != nil {
				if firstErr == nil {
					firstErr = err
				}
				log.Printf("engine: could not resolve a URL for %s: %v", shortID(mediaID), err)
				continue
			}
			path := e.outputPath(kind, mediaID, resolution)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			file := MediaFile{MediaID: mediaID, Path: path, Bytes: int64(len(data)), Resolution: resolution}
			files = append(files, file)
			e.recordMedia(generationRow, file, kind, prompt, "")
			continue
		}

		file, err := e.downloadOne(ctx, url, mediaID, kind, resolution, prompt, generationRow)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		files = append(files, file)
	}

	return files, firstErr
}

func (e *Engine) downloadOne(ctx context.Context, url, mediaID, kind, resolution, prompt string, generationRow int64) (MediaFile, error) {
	e.mu.RLock()
	client := e.client
	e.mu.RUnlock()
	if client == nil {
		return MediaFile{}, fmt.Errorf("engine: no active client")
	}

	path := e.outputPath(kind, mediaID, resolution)
	written, err := client.Download(ctx, url, path)
	if err != nil {
		return MediaFile{}, err
	}

	file := MediaFile{MediaID: mediaID, Path: path, Bytes: written, Resolution: resolution}
	e.recordMedia(generationRow, file, kind, prompt, url)
	return file, nil
}

func (e *Engine) outputPath(kind, mediaID, resolution string) string {
	name := shortID(mediaID)
	if resolution != "" && resolution != config.NativeVideoResolution {
		name += "_" + resolution
	}
	ext := ".mp4"
	if kind == "image" {
		ext = ".png"
	}
	return filepath.Join(config.OutputDir(), name+ext)
}

func (e *Engine) recordMedia(generationRow int64, file MediaFile, kind, prompt, url string) {
	var genID *int64
	if generationRow > 0 {
		genID = &generationRow
	}
	if _, err := e.store.RecordMedia(store.Media{
		GenerationID: genID,
		MediaID:      file.MediaID,
		Kind:         kind,
		Prompt:       prompt,
		FileName:     filepath.Base(file.Path),
		FilePath:     file.Path,
		URL:          url,
		Resolution:   file.Resolution,
		Bytes:        file.Bytes,
	}); err != nil {
		log.Printf("engine: could not record media: %v", err)
	}
}

func (e *Engine) finishJob(jobID, status string, credits *int, start time.Time, err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	if err := e.store.FinishGeneration(jobID, status, credits, time.Since(start).Milliseconds(), message); err != nil {
		log.Printf("engine: could not finalise job %s: %v", shortID(jobID), err)
	}
}

/* ------------------------------------------------------------------ *
 * Maintenance
 * ------------------------------------------------------------------ */

// RefreshCredits updates every worker's balance from upstream.
func (e *Engine) RefreshCredits(ctx context.Context) {
	e.pool.RefreshCredits(ctx)

	for _, w := range e.pool.Workers() {
		credits, known := w.Credits()
		var value *int
		if known {
			value = &credits
		}
		if err := e.store.UpsertAccount(store.Account{
			AccountID:  w.ID,
			SKU:        w.SKU(),
			Credits:    value,
			Status:     "active",
			CookieHash: "",
		}); err != nil {
			log.Printf("engine: could not record credits for %s: %v", w.ID, err)
		}
	}
}
