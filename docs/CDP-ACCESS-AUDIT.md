# browser-Cdp — Is it pure CDP access?

**Audit date:** 2026-09-18
**Scope:** `../browser-Cdp/extension/` (extension) + `flow-go/internal/cdp`, `internal/bridge` (backend side)
**Question asked:** does this give *pure* CDP access, or is something being faked/hidden?

> Note: no file in the workspace matches `scene#16` / `Daily Development`, so this audit
> is against the actual shipped code rather than a spec document.

---

## Verdict

**Yes — it is genuine, unfiltered Chrome DevTools Protocol access. No backdoor, no phone-home, no fake layer.**

Three separate claims, each verified separately:

| Claim | Verdict | Basis |
|---|---|---|
| Real CDP (not a re-implementation or a wrapper that only *looks* like CDP) | ✅ true | `chrome.debugger` + CDP 1.3, method string passed through untouched |
| Full access by default (equivalent to `--remote-debugging-port`) | ✅ true | empty allowlists by default |
| Hidden egress / telemetry / remote config | ✅ **none found** | only outbound socket is the loopback WebSocket |

One thing *is* worth knowing, and it is not deception — it is configuration:
**the running `flow-go` backend narrows the extension down to `labs.google` and never
un-narrows it.** Details in "Caveat" below.

---

## 1. Is the CDP real?

Yes. It uses Chrome's own debugger API — the same protocol `--remote-debugging-port`
speaks, not a hand-rolled imitation.

`manifest.json`:
```json
"permissions": ["debugger", "tabs", "cookies", "alarms", "storage", "clipboardWrite"],
"host_permissions": ["<all_urls>"],
"minimum_chrome_version": "116"
```

`background.js` — attach at CDP **1.3**, which is the current protocol revision:
```js
await chrome.debugger.attach({tabId}, '1.3');          // :215
result = await chrome.debugger.sendCommand(            // :416
  {tabId: attachedTabId}, method, params.params || {}
);
```

The critical line is the `sendCommand` call: **`method` goes straight through.** The only
filter in the entire path is an optional deny-list, and its default is empty:

```js
if ((config.blockedMethods || []).includes(method)) {   // background.js:409
  throw new Error(`CDP method ${method} is blocked...`);
}
```
```js
blockedMethods: [],   // config.js:53 — "Empty by default for full raw-CDP access"
```

`cdp.evaluate` is a thin wrapper over `Runtime.evaluate` with `returnByValue` /
`awaitPromise` / `userGesture` exposed as flags — not a restricted sandbox.

Events are real too: `chrome.debugger.onEvent` is subscribed and every event from the
attached tab is queued (2000-deep) and pushed upstream as `cdp.event`
(`background.js:546`).

**Default posture is full access**, and the defaults are honest about it:
```js
targetUrlPrefixes: [],   // empty = every tab attachable
cookieDomains:     [],   // empty = every cookie readable/writable
blockedMethods:    [],   // empty = every CDP method callable
```
That is byte-for-byte the access a raw `--remote-debugging-port` gives.

---

## 2. Is there anything hidden? (the "chodu panti" check)

Grepped the whole extension for every egress and dynamic-code pattern. Result:

| Pattern searched | Hits |
|---|---|
| `fetch(` | **0** |
| `XMLHttpRequest` | **0** |
| `navigator.sendBeacon` | **0** |
| `eval(` / `new Function(` | **0** |
| `atob(` / `btoa(` | **0** |
| `innerHTML` / `document.write` | **0** |
| `importScripts` / `chrome.scripting.executeScript` | **0** |
| `webRequest` / `proxy` / `nativeMessaging` / `history` / `bookmarks` / `downloads` | **0** |

The **only** outbound connection in the entire codebase:
```js
socket = new WebSocket(cfg.bridgeUrl);   // background.js:498
```
and the default is loopback:
```js
bridgeUrl: 'ws://127.0.0.1:9222'         // config.js:17
```

That is enforced twice — in code, and again at the browser level by the manifest CSP,
which hard-blocks any non-loopback destination:
```json
"connect-src 'self' ws://127.0.0.1:* ws://localhost:*"
```

So: cookies can only leave the browser to a process already running on this machine.
There is no remote server, no analytics, no config fetched from the internet, no
obfuscated blob. The `README.md` also openly documents that this is a de-branded fork of
the Weavy bridge and lists exactly what was stripped — that is disclosure, not concealment.

The one deliberate dynamic-execution surface is `cdp.evaluate` → `Runtime.evaluate`, and
that *is* the feature: it runs inside the attached tab, which is what CDP is for.

---

## 3. Caveat — what the backend does with it

The extension *offers* full access. The `flow-go` backend **does not use** full access, and
it permanently re-scopes the extension on connect.

`internal/bridge/bridge.go` → `PushConfig()`, called automatically on every connect:
```go
patch := cdp.Config{
    TargetURLPrefixes: b.Targets,        // ["https://labs.google/fx/tools/flow", .../flow/]
    CookieDomains:     b.CookieDomains,  // ["labs.google", "google.com"]
}
```

Because config is persisted in `chrome.storage.local`, **this scope survives after
flow-go disconnects.** A freshly loaded extension is full-access; once flow-go has touched
it even once, it stays pinned to `labs.google` until something calls `config.reset`.

Also, flow-go uses only **4 of the 17** available operations:

| Op | Used by flow-go? | Where |
|---|---|---|
| `ping` | ✅ | handshake |
| `config.set` | ✅ | `PushConfig` |
| `cookies.list` | ✅ | `SyncCookies` — the main data path |
| `tab.attach` | ✅ | `RefreshSession` (wakes tab so the site renews its own session) |
| `cdp.evaluate` | ✅ | `internal/recaptcha/recaptcha.go:283` — runs `grecaptcha.enterprise.execute()` |
| `cdp.call` | ❌ never called | library only |
| `ReadEvents` | ❌ never called | library only |
| `tabs.open` / `ListTabs` / `Detach` / `EvaluateString` | ❌ never called | library only |

So generation traffic does **not** flow through the browser — the browser supplies cookies
and one reCAPTCHA token, and the engine does the rest over HTTP. That is a design choice,
not a limitation of the bridge.

---

## 4. The one genuine gap

**`blockedMethods` is never pushed by flow-go** (verified: zero `BlockedMethods` references
in the Go tree), while the cookie scope *is* pushed. That combination leaves a bypass:

- `cookies.list` / `cookies.get` / `cookies.set` are properly scoped to
  `labs.google` + `google.com` — reads outside it are filtered, writes rejected
  (`background.js:266–326`).
- But raw CDP is **not** scoped. With an empty `blockedMethods`, a local caller can attach
  to the allowed tab and issue `Network.getAllCookies` or `Storage.getCookies`, which return
  **every cookie in the browser profile**, not just the allowlisted domains.

The tab allowlist does not save you here: it restricts *which tab you attach to*, not *which
CDP method you then run*. The README acknowledges this exact hole and prescribes the fix
("put `Network.getCookies`, `Network.setCookie`, `Storage.getCookies` and friends into
`blockedMethods`") — flow-go just never does it.

Second, related point: the WebSocket has **no authentication**. `isExtensionOrigin`
(`internal/cdp/client.go:206`) accepts `chrome-extension://` origins *and any client that
sends no `Origin` header* — which is every non-browser local process. So anything already
running on this machine can connect to `127.0.0.1:9222` and drive the bridge. The code and
README both say this explicitly ("the exposure is limited to local processes, but treat it
accordingly"), so it is disclosed rather than hidden — but it is real.

---

## 5. Recommendations

1. **Push a `blockedMethods` deny-list** from `PushConfig()` alongside the cookie scope —
   `Network.getCookies`, `Network.getAllCookies`, `Network.setCookie`,
   `Storage.getCookies`, `Storage.setCookies`. This closes the §4 bypass for one line of code.
2. **Bind the bridge to a shared secret.** A `token` field checked on connect would stop
   arbitrary local processes from attaching. Loopback-only is not isolation on a shared machine.
3. **Decide whether `google.com` belongs in `DefaultCookieDomains`.** It is broader than the
   stated goal ("the minimum that produces a working Labs session") and pulls in identity
   cookies beyond the Labs host.
4. **Be explicit that flow-go re-scopes the extension permanently.** If you ever want the
   extension back at full access, something must call `config.reset`.

---

## Bottom line

The extension is exactly what it says it is: **pure CDP 1.3 over `chrome.debugger`, full
access by default, one loopback WebSocket, nothing else.** No hidden endpoints, no
telemetry, no obfuscation, no fake protocol layer. The interesting findings are not
deception — they are (a) the backend silently narrows the scope and leaves it narrowed, and
(b) `blockedMethods` is never set, so raw CDP can read cookies the scoped API would refuse.
