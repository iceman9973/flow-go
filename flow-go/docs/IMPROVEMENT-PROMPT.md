# Improvement prompt v2 — browser-Cdp / flow-go bridge

> **Where the extension is now.** This prompt refers to the extension at
> `../browser-Cdp/extension/`, which assumed a checkout beside this repo. The
> code now lives in the [Browser-cdp](https://github.com/kodelyx/Browser-cdp)
> repo and can be cloned anywhere; the relative paths below are kept as they
> were written, because this is a record of a task that was already run.
>
> The Go half is no longer a sibling either — it is the module dependency
> `github.com/kodelyx/Browser-cdp/cdp-control`.

Revised 2026-09-18. Replaces v1, which went stale within minutes: v1's Problem 1
(`blockedMethods` deny-list) and Problem 2's backend half were implemented by a concurrent
session while v1 was being written. Evidence: `docs/BRIDGE-TOKEN-FINDINGS.md`.

**Status of v1's four problems:** 1 — done (shipped as `DefaultBlockedMethods`, 15 methods,
a superset of what v1 asked for). 2 — backend done, extension missing. 3 — open. 4 — open.

Copy everything below the rule into an AI coding agent working in the `flow-go` repo.

---

You are working in the Go repo `flow-go` (module `github.com/kodelyx/flow-go`) with a
companion MV3 Chrome extension at `../browser-Cdp/extension/`. The extension dials out over a loopback
WebSocket and exposes `chrome.debugger` (CDP 1.3), `chrome.tabs`, and `chrome.cookies`.

Bridge-token auth was recently added to the Go side (`internal/bridge/bridge.go`
`EnsureToken`/`claim`, `internal/cdp/client.go` `ListenOptions.authorize`). It is currently
**inert** — it enforces nothing. Fix that first, then the four smaller items. Read the code
before changing it; do not trust my line numbers.

**1. `AllowTokenless` never closes the pairing window.** `bridge.Listen()` copies the flag
by value into `ListenOptions`, so `claim()` setting `b.allowTokenless = false` is invisible
to the running listener — the tokenless window stays open until restart. Make it a live
read: `AllowTokenless func() bool` called inside `authorize()`, or one shared `*atomic.Bool`.

**2. The extension cannot present a token, so pairing never completes.** `claim()` is only
reachable via `OnAuthenticated`, which `Listen` calls only on a *proven* match
(`client.go:230`). The extension has no token support — `grep bridgeToken` over `config.js`
and `background.js` returns nothing. So: tokenless connect → not authenticated → `claim()`
never runs → `.claimed` never written → tokenless allowed forever. The backend already
pushes `bridgeToken` and `saveConfig()` already persists it; nothing uses it. Fix the
extension: add `bridgeToken: ''` to `DEFAULTS` in `config.js`, and in `connect()` append
`?token=<bridgeToken>` to `cfg.bridgeUrl` when set, re-dialling once after `config.set`
first delivers a token. Do not remove the full-access defaults — the extension is
deliberately generic, scoping belongs to the backend.

**3. Narrow the cookie scope off `google.com`.** `DefaultCookieDomains`
(`internal/app/app.go:30`) is `["labs.google", "google.com"]`, but its own comment claims it
is "deliberately the minimum that produces a working Labs session". Narrow to
`["labs.google"]` and run a real session refresh. Note `.google.com` is not `google.com` —
check whether the auth cookies are host-only. If you cannot verify without a live signed-in
browser, say so and leave it with a comment rather than guessing.

**4. Make the scope push visible and reversible.** The push is silent and, because config
lives in `chrome.storage.local`, it persists after flow-go disconnects — with no way back.
Log the applied scope on **every** push, not just on change, and add a documented reset:
either a `flow bridge reset-scope` CLI subcommand (match `internal/cli/cli.go`) or a
`/v1/bridge/reset-scope` route in `internal/server/server.go`. Say why you picked one.
Update both READMEs to state that connecting flow-go re-scopes the extension permanently.

**5. Auto-detach when the tab navigates out of scope.** `chrome.tabs.onUpdated`
(`../browser-Cdp/extension/background.js:574`) updates state and emits `tab.navigated` but never
re-checks `urlAllowed()`. A tab that navigates outside `targetUrlPrefixes` stays attached
and keeps streaming CDP events. When scope is non-empty and the new URL is outside it,
detach and emit `cdp.detached` with reason `out-of-scope`.

**6. Sequence numbers on pushed events.** `cdp.event` is best-effort and silently drops
under load. Add a monotonically increasing `seq` to every pushed and buffered event so a
backend can detect gaps.

**Constraints.** No new third-party dependencies. `gofmt` clean; `go build ./...` and
`go test ./...` pass. `node --check` passes on every JS file. Match the comment style — this
codebase explains *why* above non-obvious code, not *what*. The known `Errno 48` test
failures when a backend holds the bridge port are environmental; do not "fix" them by
changing the port.

**Acceptance.** (1) With a pairing marker present, a tokenless upgrade is rejected 401 and a
`?token=` upgrade succeeds. (2) `Network.getAllCookies` over the bridge errors instead of
returning cookies. (3) A tab navigating outside scope is detached and
`cdp.detached{reason:"out-of-scope"}` is emitted. (4) Pushed events carry a gapless `seq`.
(5) Build and tests pass.

**Output.** A short report: what changed, file by file; what you verified and how; what you
could not verify and why. Do not paste whole files back.
