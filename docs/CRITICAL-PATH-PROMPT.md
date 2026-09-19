# Critical-path prompt — make the bridge token actually enforce

Paste into an AI coding agent working in the `flow-go` repo. Two fixes only, both in the
token-auth path that was just added. Nothing else. Background: `docs/BRIDGE-TOKEN-FINDINGS.md`.

---

You are in the Go repo `flow-go` with a companion MV3 Chrome extension at `../browser-Cdp/extension/`.
The extension dials out over a loopback WebSocket and exposes CDP 1.3.

Bridge-token auth was just added (`internal/bridge/bridge.go` `EnsureToken`/`claim`,
`internal/cdp/client.go` `ListenOptions.authorize`). **It enforces nothing today.** Two bugs,
both in that path. Read the code first; my line numbers are approximate.

**1. `AllowTokenless` is copied by value, so the window never closes.**
`bridge.Listen()` snapshots the flag into a local and passes it by value into
`cdp.ListenOptions` (~`bridge.go:178`). `claim()` then sets `b.allowTokenless = false`
(~`bridge.go:157`) — but the running listener holds a struct copy, and `authorize()` reads
the flag off that copy (`client.go:270`). The mutation never lands, so the tokenless window
stays open until the process restarts.

Fix: make it a live read — change `AllowTokenless bool` to `AllowTokenless func() bool` and
call it inside `authorize()`; or share one `*atomic.Bool` between `Bridge` and
`ListenOptions`.

**2. The extension never presents a token, so pairing never completes.**
`claim()` is only reachable via `OnAuthenticated`, which `Listen` calls only when
`authorize()` reported a *proven* token (`client.go:230`). `authorize()` returns
`authenticated == false` on the tokenless path (`client.go:271`). And the extension has no
token support at all:

```
grep -n "bridgeToken\|authToken" ../browser-Cdp/extension/config.js ../browser-Cdp/extension/background.js   # no matches
```

So: tokenless connect → not authenticated → `claim()` never runs →
`data/bridge-token.claimed` never written → tokenless allowed forever. The backend already
pushes `bridgeToken` via `config.set` and `saveConfig()` already persists it to
`chrome.storage.local`; nothing reads it.

Fix, extension side only:
- `../browser-Cdp/extension/config.js` — add `bridgeToken: ''` to `DEFAULTS`.
- `../browser-Cdp/extension/background.js` — in `connect()`, append `?token=<bridgeToken>` to
  `cfg.bridgeUrl` when the field is non-empty, and re-dial once after the first `config.set`
  delivers a token.

**Constraints.** No new dependencies. `gofmt` clean; `go build ./...` and `go test ./...`
pass; `node --check` on every JS file. Do not remove the full-access defaults in
`../browser-Cdp/extension/config.js` — the extension is deliberately generic.

**Verify.** With a pairing marker present, a tokenless upgrade gets 401 and a `?token=`
upgrade completes the `ping` handshake. Before the fix, both succeed.

**Output.** Short report: files changed, how you verified, what you could not verify. Do not
paste whole files.
