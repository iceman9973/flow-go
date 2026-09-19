# Bridge token — state of play and two bugs

> ## Status: both bugs are fixed. Read this before the analysis below.
>
> Updated 2026-09-19. The analysis is kept because the reasoning is still the
> reference for how this was found, but **every path in it is wrong now and the two
> bugs no longer exist.**
>
> **The code moved.** `internal/bridge/` and `internal/cdp/` are not in this module.
> The bridge, the CDP client and the cookie jar live in the sibling `browser-Cdp`
> project as `cdp-control/{bridge,cdp,cookiejar}` and are imported by a relative
> `replace` in `go.mod`. Line numbers below refer to that code before it moved.
>
> | Finding | Status |
> |---|---|
> | **Bug A** — `AllowTokenless` captured by value, so the tokenless window never closed | **Fixed.** The field is a live read: `AllowTokenless func() bool`, called inside `authorize()` (`cdp-control/bridge/bridge.go`). The window closes the moment `claim()` runs. |
> | **Bug B** — the extension never presents a token, so pairing never completes | **Fixed.** `browser-Cdp/extension/config.js` has `bridgeToken: ''` in `DEFAULTS`, and `background.js`'s `socketUrl()` appends `?token=<bridgeToken>` when it is set. Pairing completes and `data/bridge-token.claimed` is written. |
> | Item 1 — version bump | Still `1.0.0`. Correct call: nothing changed on the wire. |
> | Item 2 — heartbeat for stale sockets | Still absent. Unverified whether the leak is real. |
> | Item 4 — `seq` on pushed events | Still absent. Events remain best-effort. |
> | Item 5 — scope-drift on navigate | **Still open, and still the strongest item.** `chrome.tabs.onUpdated` in `browser-Cdp/extension/background.js` updates state and emits `tab.navigated` but never re-checks `urlAllowed()`, so a tab that navigates out of `targetUrlPrefixes` stays attached and keeps streaming CDP events. |
> | Item 6 — docs parity | Fixed at the time; re-verify against the current READMEs. |
> | Item 7 — stale binary | Nothing to do. |
>
> **One consequence worth stating plainly**, because the analysis below treats the
> token as decorative: it is not, any more. A local process dialling
> `ws://127.0.0.1:9222` without the token is now refused. That matters here because
> **two** extensions dial that port and exactly one backend can own it — the token is
> also what lets both extensions follow the backend across a restart.

Written 2026-09-18. Findings on the **in-flight** token/auth work in `internal/bridge/`
and `internal/cdp/`, and a fact-check of the 7-item improvement list.

The repo is being edited concurrently while this was written (`internal/cdp/client.go`
grew from 547 to 616 lines mid-session), so verify against current `HEAD` before acting.

---

## Part 1 — fact-check of the 7-item list

| # | Claim | Verdict |
|---|---|---|
| 1 | Bump manifest `1.0.0` → `1.1.0` | **Valid, but skip for now.** `manifest.json:4` is still `1.0.0`. The stated reason ("behaviour changed fail-closed → fail-open") is not supported by anything in the code. Bump it when the extension actually changes on the wire, not before. |
| 2 | Stale-connection leak — add a heartbeat | **Partly valid.** `bridge.remove()` exists (`bridge.go:203`) but is only called on handshake failure (`bridge.go:108`). There is no heartbeat, so a socket that dies after a successful handshake leaves `b.current` pointing at a dead client until `Connected()` is consulted. The "3 ESTABLISHED sockets pile up" figure was not verified. |
| 3 | Single-client guard / token | **Backend already done. Extension missing. See Part 2.** |
| 4 | Event sequence numbers | **Valid.** No `seq` field anywhere in `background.js`. Events are best-effort and silently drop. |
| 5 | Scope-drift on navigate | **Valid, and the best item on the list.** `chrome.tabs.onUpdated` (`background.js:574`) updates state and emits `tab.navigated` but never re-checks `urlAllowed()`. A tab that navigates out of `targetUrlPrefixes` stays attached and keeps streaming CDP events. |
| 6 | Docs parity for `awaitPromise` / `userGesture` | **Half wrong.** `awaitPromise` was *already* documented in both `README.md:52` and `ai-prompt.js:124`. Only `userGesture` was missing — and so was `tabId`, which `cdp.evaluate` also accepts. **Fixed this session** (both files). |
| 7 | Rebuild `flow-go` — binary is stale | **False.** `POST /v1/bridge/refresh` exists in source (`internal/server/server.go:72`) **and** in the compiled binary. Binary mtime == source mtime. No rebuild needed. |

---

## Part 2 — the token work is currently inert

The backend side is well built: `EnsureToken()` generates a 32-byte token into
`data/bridge-token` (mode 0600), `authorize()` does a constant-time compare, and the token
is accepted from `?token=` or `Authorization: Bearer`. The intent is trust-on-first-use
pairing.

Two bugs mean it enforces nothing today. Neither one breaks the bridge — the tokenless path
always succeeds — which is exactly why they would go unnoticed.

### Bug A — `AllowTokenless` is captured by value, so the window never closes

`bridge.Listen()` snapshots the flag into a local and passes it **by value**:

```go
// internal/bridge/bridge.go:178-193
b.mu.RLock()
allowTokenless := b.allowTokenless      // copied here
b.mu.RUnlock()

return cdp.Listen(ctx, cdp.ListenOptions{
    Addr:           addr,
    Token:          b.token,
    AllowTokenless: allowTokenless,      // copied again into the struct
    ...
})
```

`claim()` then sets `b.allowTokenless = false` (`bridge.go:157`) — but the running `Listen`
closure holds `opts`, a struct copy taken at startup, and `authorize()` reads
`o.AllowTokenless` off that copy (`client.go:270`). The mutation is invisible to the live
server, so the tokenless window stays open until the process restarts.

**Fix:** stop copying a bool. Make the field a live read — e.g. change
`AllowTokenless bool` to `AllowTokenless func() bool` and call it inside `authorize()`, or
share a single `*atomic.Bool` between `Bridge` and `ListenOptions`.

### Bug B — pairing never completes, because the extension never presents a token

`claim()` is reachable only through `OnAuthenticated`, and `Listen` calls that only when
`authorize()` reported a *proven* token:

```go
// internal/cdp/client.go:215-235
authenticated, ok := opts.authorize(r)
...
if authenticated && opts.OnAuthenticated != nil {
    opts.OnAuthenticated(client)         // only on a real token match
}
```

`authorize()` returns `authenticated == false` for the tokenless path (`client.go:271`), so a
pairing connection does **not** claim. And the extension has no token support at all:

```
$ grep -n "bridgeToken\|authToken" ../browser-Cdp/extension/config.js ../browser-Cdp/extension/background.js
(no matches)
```

The chain therefore never starts:

1. Backend generates a token, `allowTokenless = true`.
2. Extension connects with no token → allowed → `authenticated == false`.
3. `claim()` not called → `data/bridge-token.claimed` never written.
4. Backend pushes `bridgeToken` via `config.set`; `saveConfig()` merges it blindly
   (`config.js:81-86`), so it **is** persisted to `chrome.storage.local` — and then ignored.
5. Extension reconnects with no token. Still allowed. Forever.

Net effect: the token is decorative. Any local process can still dial
`ws://127.0.0.1:9222` and drive the browser, which is the exact threat the token was added
to stop.

**Fix:** the extension has to participate.
- `../browser-Cdp/extension/config.js` — add `bridgeToken: ''` to `DEFAULTS`.
- `../browser-Cdp/extension/background.js` — in `connect()`, append `?token=<bridgeToken>` to
  `cfg.bridgeUrl` when the field is set, and re-dial once after `config.set` delivers a
  token for the first time.

---

## Recommended order

1. **Bug A** — one-line-ish, and without it Bug B's fix still leaves the window open until a restart.
2. **Bug B** — makes the token real.
3. **Item 5** (scope-drift auto-detach) — the strongest item on the original list; closes a live leak.
4. **Item 4** (`seq` on events) — cheap, and makes silent event loss detectable.
5. **Item 1** (version bump) — last, once the extension wire format actually changed.
6. **Item 2** (heartbeat) — only after confirming the socket leak is real.
7. **Item 7** — nothing to do.
