# flow-go

A Google Flow (Labs FX) generation engine written in Go.

The browser supplies **cookies and base information**. Everything else — access
tokens, project resolution, generation, polling, upscaling, downloads, storage —
happens in this process. No generation request travels through a browser.

```
Chrome ──(cookies + page tokens + captcha)──▶ Flow Go Bridge extension
   (or browser-Cdp, the generic one)                 │  WebSocket, extension dials out
                                                     ▼
                                       flow-go serve ── hosts the bridge on :9222
                                                     │
                          cookies ──▶ SAPISIDHASH    │  (no API key, no bearer token)
                                                     ▼
                    flow.google.com/_/AiSandboxAngularFrontend/data/batchexecute
                                                     │
                          gate ──▶ submit ──▶ poll ──▶ resolve ──▶ download
                                                     │
                                                     ▼
                                             SQLite (WAL) + output/
```

## Why

The Python engine this replaces (`flow-agent`) routed **every** generation call
through a Chrome extension, and the extension had to observe a live
`Authorization: Bearer ya29.` request on `labs.google` to hand the backend a
token. That made the browser a hard dependency on the critical path, and when the
token went stale the pipeline deadlocked.

This port keeps the browser only where it is genuinely required — cookies, page
tokens and a reCAPTCHA token — and does the rest in Go.

## Quick start

```bash
# 1. Build
go build -o flow-go .

# 2. Load flow-go-extension/ as an unpacked extension
#    chrome://extensions -> Developer mode -> Load unpacked -> select flow-go-extension/
#    (Optionally also load ../browser-Cdp/extension/ — see "The two extensions".)

# 3. Run
./flow-go serve
```

The extension dials in on `ws://127.0.0.1:9222`. The server pushes its allowlists
to the extension on connect, syncs cookies, reads the page tokens, and comes
ready. The HTTP API listens on `http://127.0.0.1:8200`.

```bash
curl -s localhost:8200/health | jq
```

## What the browser is used for

Exactly two things:

| Supplied by the browser | Everything else (Go) |
| --- | --- |
| Cookie values for `labs.google` and `google.com` | Access-token minting and caching |
| The current Flow tab URL (project ID) | Project resolution |
| — | Generation submission |
| — | Status polling |
| — | Upscaling |
| — | Media download |
| — | Persistence and analytics |

The cookie scope is enforced by the extension in code, not by convention: a
cookie outside the configured domains is filtered from reads and rejected on
writes, and the backend pushes an explicit CDP deny-list so the scope cannot be
bypassed with `Network.getAllCookies`. See "Bridge security" below.

### Bridge security

Two controls matter, and both are the backend's responsibility — `browser-Cdp`
itself ships permissive, because it is a generic tool and scoping belongs to
whoever drives it.

**A shared token.** The origin check alone is not a boundary: a non-browser local
process omits the `Origin` header and passes it. So the backend generates a token
on first run at `data/bridge-token` (mode `0600`) and requires it on the
WebSocket upgrade. Pairing is trust-on-first-use, because the extension cannot
learn the token before it can connect: the first tokenless connection is
accepted, the token is delivered over it, and once the extension reconnects with
the token the window closes permanently. The backend logs a warning while it is
still unpaired.

To re-pair, either paste the token from `data/bridge-token` into the extension's
popup, or delete `data/bridge-token.claimed` and restart. The popup is quicker and
needs no restart.

Re-pairing comes up more often than it looks: Chrome clears `chrome.storage.local`
when an unpacked extension is removed and added again, so a reinstalled extension
returns with no token at all. It cannot be told anything either — the upgrade is
refused before a message could be sent — so the recovery has to start from the
extension side. That is what the popup's token field is for. The backend explains
each distinct refusal once rather than once per retry, because a rejected extension
retries on a timer and would otherwise bury the one line worth reading.

**An explicit CDP deny-list.** `DefaultBlockedMethods` in
`internal/bridge/bridge.go` is pushed on every connect. Without it a caller could
attach to an allowed tab and then run `Network.getAllCookies`, which returns every
cookie in the browser profile — the tab allowlist limits which tab you attach to,
not which method you then run.

**The scope push is permanent.** The extension persists config in
`chrome.storage.local`, so narrowing survives after flow-go disconnects. That is
intentional (the extension should not silently widen back), but it means
connecting flow-go re-scopes the extension. The applied scope is logged on every
connect so it is never invisible. To undo it, reset the extension's config — the
popup has no editor by design, so clear `browserCdp.config` from
`chrome.storage.local` in DevTools, or reinstall the extension.

## HTTP API

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/health` | Liveness, bridge state, readiness |
| GET | `/status` | Account, pool, and bridge snapshot |
| GET | `/stats` | Database aggregates and pool statistics |
| GET | `/v1/workers` | Worker pool detail |
| GET | `/v1/credits` | Refresh and return account balances |
| POST | `/v1/videos/generations` | Submit a video generation |
| POST | `/v1/images/generations` | Submit an image generation |
| POST | `/v1/bridge/refresh` | Ask the browser to renew its session, then re-sync |
| GET | `/v1/jobs` | Recent jobs |
| GET | `/v1/jobs/:id` | One job |
| GET | `/v1/media` | Recent generated media |
| GET | `/v1/accounts` | Tracked accounts |
| GET | `/v1/accounts/credits` | Email, name and balance of every signed-in account (`?max=N`, default 4) |
| GET | `/v1/projects` | The account's Flow projects, over the transport. `most_recent` is what a run picks when none is named |
| GET | `/v1/accounts/affordable` | Which signed-in account would pay for a job (`?cost=N`) — read-only |
| POST | `/v1/accounts/switch` | Act as another signed-in account (`{"index": N}`), and remember it |
| POST | `/api/sync-cookies` | Push a cookie dump directly (extension fallback) |
| GET | `/output/*` | Generated files |

### Several accounts in one browser

A browser can hold multiple signed-in Google accounts at once, and they **share one
cookie jar**. The only thing that distinguishes them is Google's `authuser` index —
the same number that appears as `/u/<n>/` in the URL. With no index, every request
resolves to the **first** signed-in account.

That is why switching accounts in the browser alone changes nothing here: the project
is read from the tab's URL, so it moves, while the session stays on account 0. The
result is a project belonging to one account submitted under another's session, which
the server answers with an empty frame rather than an error.

```
GET  /v1/accounts/credits?max=3
  u/0: 1 credit    <first>@gmail.com    Akash Yadav
  u/1: 11 credits  <second>@gmail.com   Akash Yadav
  u/2: 25 credits  <third>@gmail.com    Bumika

POST /v1/accounts/switch  {"index": 2}
  index=2  account=acct-…-u2  project=a9153ea6-…
```

(Addresses are placeholders. The point is that the three are **different** — they used
to be one address repeated three times, which reads as "these accounts share an email"
rather than as a bug.)

**The address comes from the profile RPC, not from the session.** The labs session
endpoint answers for the *default* account whatever `authuser` says, so reading the
address from it stamps every signed-in account with the first one's. That is what
this listing used to do — the three rows above were one address repeated three
times, which reads as "these accounts share an email" rather than as a bug. The
`o30O0e` profile RPC is account-scoped and is the only source that follows
`authuser`.

**The tier has no per-account source, and the listing says so.** `sku` is still read
from that same blind endpoint, so every row carries the default account's tier.
There is no replacement to be had: the `nzlgx` credits payload is a flat array of
numbers with no tier in it — `[[7,3,8,1,null,7]]`, measured per account — and the
legacy aisandbox `/v1/credits` answers for a different signed-in account entirely.
It is kept because it is still the tier of *an* account on this machine, but nothing
branches on it per row. In particular the account-selection policy deliberately
ignores free-tier preference: spending renewable credits before paid ones is the
right policy and cannot be applied when every account looks the same tier.

**The choice is remembered.** `/v1/accounts/switch` writes the index to the
`settings` table, so a restart comes back on the account that was chosen rather
than on index 0. `ACCOUNT_INDEX` only seeds a database that has never recorded one.

**A job that the current account cannot pay for moves the engine.** Before a video
is submitted its cost is known — `VideoCosts[duration][quality] × count` — and the
balance is read. If it does not cover the render, the other signed-in accounts are
scanned and the engine moves to the one with the most credits that can, then
re-plans. `GET /v1/accounts/affordable?cost=N` reports that decision without
performing it:

```
GET /v1/accounts/affordable?cost=15
  current_index: 0   chosen_index: 2   chosen_balance: 25   would_switch: true
```

If nothing can pay, the submission is refused with **402** and the arithmetic in the
message, and nothing is sent — the server would otherwise accept it and answer with
no media, which reads as a broken request rather than an empty wallet. The engine
does **not** substitute a cheaper quality: a caller who wants 360p asks for 360p.

**A request conditioned on an existing asset is never moved.** Projects are
per-account, so a `start_image` or `end_image` id belongs to the project of the
account that supplied it; moving the engine to a richer account leaves that id
pointing at a project the new account cannot see. Measured: an image generated on
one account was used as the start frame of a request the engine moved to a second
account to afford, and resolution retried its full window before failing with "not
in the project listing" for an asset that was in the listing — just not that one. A
conditioned request that the current account cannot pay for is refused instead. A
request with no conditioning carries no such id and can be served anywhere, which is
the case this exists for.

Switching re-bootstraps, because the session, the account row and the project all
have to move together. Projects are per-account, so the engine opens
`flow.google.com/u/<n>/` and reads that account's project list rather than reusing a
project id remembered from another — a remembered one **404s**, and a 404 page has no
reCAPTCHA client on it either.

Index 0 is the default and is **omitted from the query rather than sent** as
`authuser=0`: the two are not the same request to every Google endpoint, and every
call before this existed sent nothing.

### Content id vs media id — the distinction that fails silently

One asset carries **two** identifiers, and the project listing returns both:

```
row[0] = content id   ← what the RPCs take
row[2] = media id     ← what the UI and the editor URL use
```

| Call | Takes |
|---|---|
| `nprQif` / `eb1hJf` (image-to-video) | content id, in `startImage`/`endImage` |
| `as29s` (media detail) | content id |
| `/project/<id>/edit/<X>` | media id |
| `maseQ` (upload) | returns **both** |

Passing the wrong one is accepted with a `200` and produces nothing, which is how
this was found: an image-to-video submission carrying a freshly uploaded file's
**media id** returned `empty` in 4 seconds, while the same call with a content id
rendered normally. `POST /v1/media/upload` therefore returns both, and the video
endpoint resolves either into the content id before submitting.

#### Which field is the content id depends on the listing shape

This is where it goes wrong, and it goes wrong silently — every id here is a uuid,
so a swapped pair still looks like a valid asset and only fails later, at the RPC
that wanted the other one.

| shape | media id | content id |
|---|---|---|
| flat — `[content-id, project-id, media-id, type-code, …]` | `row[2]` | `row[0]` |
| nested — `[media-id, null, null, [title, ts, …, content-id, ?, ts], project]` | `row[0]` | `detail[4]` |

The nested pair was read **backwards** in the parser: `detail[4]` was called the
media id and `detail[5]` the content id. It is the other way round, and the upload
response is what proves it — it reports `media_id` and `content_id` separately, and
the content id is the one that lands at `detail[4]`.

Two consequences, both of which looked like something else:

- **`as29s` was handed the wrong uuid.** It is not rejected; it answers `200` with
  a `null` payload, which is indistinguishable from a render that has not
  finished. Every completed video therefore looked like a video that never
  completed, and the poll waited out its full timeout before giving up.
- **An uploaded image was invisible.** `detail[5]` is `null` for an upload — it has
  no derived variant — and the row matcher required both ids, so every uploaded
  asset was dropped and conditioning on one reported it as "not in the project
  listing" while it sat right there in the listing.

### Uploading a local file

`POST /v1/media/upload` takes a `path` (the CLI's case) or `data_base64`, uploads
over batchexecute (`maseQ`) and returns the media and content ids. This is what
makes a local file usable at all:

```
POST /v1/media/upload          {"path": "my-image.jpg"}
  -> media_id, content_id
POST /v1/videos/generations    {"start_image": "<either id>", ...}
  -> a video conditioned on that image
```

The upload argument, captured from the app:

```
[context-block, <base64 image>, "<mime type>", 1, null, null, null, null,
 "<file name>", null, <uuid>, <uuid>]
```

The app stores the name with spaces replaced by underscores, and the response
carries the ids in its first row: `[media-id, project, …, content-id, …]`.

**An upload is not in the listing the moment it returns.** The call hands back its
ids immediately, but the project listing takes several seconds to carry the row —
measured at about eight. Conditioning on an id straight after the upload therefore
reported "not in the project listing" for an asset that was in the listing by the
time anyone looked.

Id resolution now retries while the asset is missing, for a 30-second window.
Measured live: an id that is not in the listing takes **35s** to be refused, where
it used to fail in under 3s — so the retry is doing the waiting rather than the
caller. A genuinely absent id still fails, and still fails as a client error rather
than as a timeout.

### The legacy upload does not help

`start_image` and `end_image` are **project media ids**. An asset must already be in
the Flow project — generated there, or uploaded through the Flow UI — before it can
be conditioned on. There is no upload path on the batchexecute transport.

The previous Python engine did have one (`upload_image` → `/v1/flow/uploadImage`,
base64 in, media id out), and flow-go still carries an implementation of it in
`flowapi.UploadImage` / `Engine.UploadImage`. **It does not help**, and this is worth
recording because it looks like it should:

| Call | Result |
|---|---|
| `POST https://aisandbox-pa.googleapis.com/v1/flow/uploadImage` | **200**, returns a media id |
| That media id in the batchexecute project listing | **absent** |
| That media id via `as29s` | **empty** |

So the legacy upload writes to the same separate store the legacy generation does,
and its media id is invisible to everything on the batchexecute side. Uploading
there and conditioning a batchexecute video on the result cannot work.

The app itself must upload over batchexecute, or to a signed URL it obtains from
one. That request has not been captured yet, and it is the one thing standing
between "condition on an existing asset" and "condition on a file of your choosing".

### Image-to-video, and the RPC that is not the one you expect

`POST /v1/videos/generations` accepts `start_image` / `end_image` (project media
ids) plus `start_frame` / `end_frame`, and conditions the video on them.

The payload was captured by driving the composer with a first and last frame set.
Against the text-to-video layout it differs in three ways:

```
[prompt-block, model, 1, null, <start>, <end>, [null,null,null,null,<uuid>,<uuid>]]
                     ^                          ^
                     mode 1, not 2             the uuid block moves from [4] to [6]
```

Each image slot is `[null, <media-id>, null, null, null, [null, a, b, c]]`, where the
trailing three numbers are the crop the app sends with every condition image. Their
meaning is not documented; they are passed through rather than interpreted.

**The trap is that the RPC id changes with the mode.** There are three, not two:

| conditioning | RPC |
|---|---|
| text only | `YhhmEf` |
| start frame only | **`eb1hJf`** |
| start + end frame | **`nprQif`** |

Sending the correct payload to the wrong one returns `200` with an empty result — no
error, nothing rendered. The payload matching the capture byte-for-byte was therefore
*not* sufficient, and `TestBuildVideoArgumentMatchesCapture` and
`TestBuildVideoArgumentMatchesStartOnlyCapture` now pin that comparison against
embedded fixtures so a regression cannot slip through the same way. `VideoRPCID` picks
the id from the conditioning and `TestVideoRPCID` asserts each case.

Measured end to end: a first+last submission returned a 360x640, 8-second video, and a
first-only submission now returns media ids as well.

### A stale session recovers by itself

A long-running engine eventually gets a `401` from batchexecute: the cookies behind
the session expire while the process keeps going. That used to be terminal — every
later call failed the same way until an operator ran `/v1/bridge/refresh` by hand.

The client now handles it. On a `401` it clears the cached anti-CSRF token, re-syncs
cookies from the browser, re-mints the access token, swaps in the new jar, and
repeats the priming sequence once. It is bounded: at most two priming sequences,
and one refresh per call.

The path is reachable on demand rather than only when a session happens to expire —
`drop_credentials` on the debug endpoint strips the session cookies for one call,
which is how it was verified:

```
batchexecute: as29s returned 401; refreshing the session and retrying
bridge: attached to https://flow.google.com/project/… for a session refresh
```

Diagnostics. These exist because a rejected RPC says nothing about *which* part
was wrong, so the only way through is to vary one thing at a time:

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/debug/headers` | The exact headers and Cookie a call would send, so they can be diffed against the browser's |
| POST | `/v1/debug/captcha` | Mint a reCAPTCHA token and hand it back, to replay it from the page |
| POST | `/v1/debug/events-raw` | Raw CDP events. `requestWillBeSentExtraInfo` is the only place the browser's *real* headers, Cookie included, are visible |
| GET | `/v1/debug/credits-rpc` | The raw **batchexecute** credits payload for one account (`?authuser=N`). Unlike `/v1/debug/credits-raw`, which hits the legacy endpoint and reports whichever account that credential belongs to, this uses the transport and `authuser` a real balance read uses — so it can be pointed at a specific account. It is how the tier question was settled: the payload is `[[7,3,8,1,null,7]]`, numbers only, no tier |
| POST | `/v1/bridge/eval` | Evaluate an expression in the attached tab |
| POST | `/v1/bridge/cdp` | Issue an arbitrary CDP command |

### Video generation

```bash
curl -s -X POST localhost:8200/v1/videos/generations \
  -H 'content-type: application/json' \
  -d '{
    "prompt": "a paper boat on a river at dawn",
    "aspect": "landscape",
    "duration": 8,
    "count": 1,
    "resolution": "1080p",
    "wait": false,
    "download": true
  }'
```

Returns `202` with a `job_id`. Poll `GET /v1/jobs/{id}`.

Set `"wait": true` to block until the generation finishes instead. Durations are
`4`, `6`, `8`, `10`. `resolution` accepts `720p` (native), `1080p`, or `4k`;
anything above 720p runs a second upsampler pass over the finished video, which
is the same pass the Flow UI's high-resolution download performs.

Image inputs accept either a local file path or an existing media ID:

```bash
curl -s -X POST localhost:8200/v1/videos/generations \
  -H 'content-type: application/json' \
  -d '{"prompt": "slow push in", "start_image": "./frame.png", "duration": 6}'
```

### Image generation

```bash
curl -s -X POST localhost:8200/v1/images/generations \
  -H 'content-type: application/json' \
  -d '{"prompt": "a single red paper boat", "aspect": "square", "model": "narwhal"}'
```

Aspects: `landscape`, `4x3`, `square`, `3x4`, `portrait`. Models:
`harbor_seal`, `narwhal`, `gem_pix_2`.

## CLI

```bash
flow-go serve                          # API + extension bridge
flow-go generate --prompt "..." --duration 8 --quality 720p
flow-go generate --prompt "..." --start-image photo.jpg   # image-to-video
flow-go image --prompt "..." --model narwhal
flow-go projects                       # the account's projects, over the transport
flow-go stats                          # database statistics
flow-go export stats.json              # full JSON export
flow-go cookies                        # cookie and credential status
```

Aspect is not a flag. It follows the model family, and the family follows the
conditioning: text-to-video renders landscape, image-to-video renders portrait,
at both qualities. `--aspect`, `--resolution`, `--seed` and `--reference` are
declared so the error names them, and are rejected rather than silently ignored —
a request that looks accepted and renders the wrong shape is worse than a refusal.

`flow-go cookies` never prints a cookie value — only counts, names on request,
and whether credentials are present. It reads the same two candidates the engine
does, in the same order (`data/cookies.json` first, then `cookies/cookies.json`),
and names the one it did *not* use. An old copy sitting beside a fresh one is the
first thing to check when a run behaves like it is holding yesterday's session,
so the command says which file a run would actually load.

### Where a run gets its session

The browser is reachable through exactly one host, because the bridge port admits
one. That leaves three cases, and a run picks whichever applies:

| Situation | What happens |
| --- | --- |
| A server is running | The CLI asks it for a session snapshot and persists that, so its copy is never older than the run using it |
| Nothing is listening on the port | The CLI listens itself. The extension connects to *it*, and cookies are read from the live page |
| No browser either | Falls back to the persisted copy, which decays on Google's schedule |

The middle row is the one that does not decay, and it is verified end to end —
server stopped, no `--project-id`, nothing but the prompt:

```
bridge: listening for a browser extension on ws://127.0.0.1:9222 (token required)
cdp: extension connected from 127.0.0.1:64619 (authenticated=true)
bridge: extension offers 19 operations including the Flow surface — using the narrow calls
bridge: synced 17 cookies (10 credential cookies, 0 unrelated dropped)
  bridge           extension attached, cookies read live
engine: using Flow project e5d6409a-…, read from https://flow.google.com/u/0/project/…
engine: seeded the batchexecute anti-CSRF token from the page (42 chars)
engine: adopted the page's f.sid (19 chars)
engine: ready — account acct-0f68addbf5a1, 17 cookies (flow-go-extension)
recaptcha: token acquired via flow.captcha (2510 chars)
```

Two extensions dial the same port, so both may connect; the bridge prefers the
narrow Flow one and the line above shows it doing so. The generic one connects
first and is used only until the Flow extension arrives. Note the account id:
it is derived from the cookie set, and the rotating `__Secure-1PSIDTS` changes it,
so a live read and a file read can legitimately produce different ids for the
same signed-in user.

## The two extensions

They are **separate projects** and neither is a subset of the other. Loading both is
fine and is the normal debugging setup.

| | `flow-go-extension/` | `../browser-Cdp/extension/` |
| --- | --- | --- |
| Chrome name | **Flow Go Bridge** | **browser-Cdp** |
| Belongs to | this repo | the `browser-Cdp` project |
| Surface | a fixed list of Flow operations (`flow.at`, `flow.captcha`, …) | arbitrary CDP: `cdp.call`, `cdp.evaluate` |
| `debugger` permission | **no** | yes |
| Host access | Google hosts only | `<all_urls>` |
| Empty scope means | refuse (fail-closed) | allow (fail-open) |

**flow-go uses the narrow one.** It needs cookies, a page token, a reCAPTCHA token
and the page-minted captcha — all of which are named operations. Arbitrary CDP is a
debugging convenience, not a requirement, and the narrow extension is the one that
cannot be talked into driving an unrelated site.

Load it as unpacked from `flow-go/flow-go-extension/`. The generic one is worth
loading too when you need `cdp.evaluate` to see what a page actually looks like; it
connects to the same backend.

Both dial `ws://127.0.0.1:9222`, so **exactly one backend can own that port**. The
backend tells them apart from the `ops` list each reports on `ping`, and prefers the
narrow one as `current` when both are attached. The generic one is still reachable
by address — `/v1/debug/cookies {"all": true}` lists every attached client.

### Two browser profiles must not both be connected

This is the one setup that silently produces nonsense, and it is easy to walk into:
load the extension in a second Chrome profile and now **two narrow extensions** are
attached, from two different Google accounts.

The bridge holds **one** cookie jar, and `Current()` is whichever client connected
last. So the account and the project flip depending on who synced most recently, and
the engine is left holding a client it built at boot while the jar underneath it has
changed. Observed directly, same process, same minute:

```
09:12:15 auth: minted access token for kiak9622@gmail.com      <- the old profile
09:12:16 engine: 1 project(s) listed; taking d57b3c78-…        <- the old account's
09:12:17 GET /v1/projects -> the new account's 16 projects      <- the new profile
```

and then a generation that reported the **old** account id and the **old** project
while the jar held the **new** account's cookies.

`/v1/debug/cookies {"all": true, "domain": "flow.google.com"}` is how to see it —
three clients, two of them with `flow_operations: true`:

| addr | flow operations |
| --- | --- |
| `127.0.0.1:62783` | yes |
| `127.0.0.1:62784` | no — the generic bridge |
| `127.0.0.1:62788` | yes |

**Close the extension, or the Flow tab, in every profile but one.** Until then every
result is unreliable, and the failures look like bugs in this engine rather than like
two accounts taking turns.

A second profile also has no bridge token, so its first connection is refused:

```
cdp: rejected an upgrade from 127.0.0.1:62587 — no token was presented and pairing is
closed, so the extension has none stored.
```

Paste the contents of `data/bridge-token` into that profile's extension popup. The
alternative — deleting `data/bridge-token.claimed` and restarting — reopens a
one-connection tokenless window, but whichever extension reconnects first wins that
race, so pasting is the deterministic fix.

The bridge itself is **not part of this module**. `cdp-control/{bridge,cdp,cookiejar}`
live in the `browser-Cdp` project and are pulled in by a relative `replace` in
`go.mod`, which means flow-go cannot be built without that checkout beside it.
`flow-go serve` hosts the socket on that port — the standalone `cdp-control` binary
hosts the same library and is not needed alongside it. See `browser-Cdp/README.md`
for the protocol.

Its popup is deliberately two things — a status line and a **Copy AI prompt**
button. That button copies a self-contained brief (endpoint, full protocol, live
allowlists, worked examples, operating rules) that can be pasted into any AI
assistant so it can connect and drive the bridge.

## Configuration

Copy `.env.example` to `.env`. Everything has a working default.

The settings that matter most:

| Setting | Purpose |
| --- | --- |
| `--proxy` | CLI flag. Route upstream traffic through one exit IP. Flow scores on IP consistency, so a stable proxy measurably improves success. |
| `--captcha` | CLI flag, **not** an environment variable: `auto` (default), `broker`, `http`, or `off`. Setting `FLOW_RECAPTCHA` does nothing. `auto` and `http` both mint over the transport and need no browser; `broker` opts into the page token. |
| `WS_PORT` / `HTTP_PORT` | Environment variables. Extension bridge and API ports. |
| `ACCOUNT_INDEX` | Environment variable. Seeds which signed-in account to act as. Only a seed — once `/v1/accounts/switch` has been used, the stored index wins, so a deliberate choice survives a restart. |
| `ACCOUNT_SCAN_LIMIT` | Environment variable, default 6. How many account indices are examined when looking for one that can pay. Chrome permits ten, but the scan costs a session, a profile and a balance read per index and runs on the request path. |
| `FLOW_PROJECT_ID` | Environment variable. The project to generate into. Setting it stops every search, including the listing call. Ranking: `--project-id`, then this, then `RPCIDProjectList`, then the open editor's URL. |
| `FLOW_ACCESS_TOKEN` | Environment variable. Supply a bearer token directly, bypassing cookie minting. |
| `FLOW_SESSION_REBUILD` | Environment variable. `off` disables the pure-Go session rebuild, leaving the raw upstream error visible. |

## Design notes

### What the browser is for, and what the API is for

The generation itself never goes through the browser. Every RPC — submit, poll,
media detail, credits, project list, project create — is a `POST` to batchexecute
from the Go process, authenticated by `SAPISIDHASH` over cookies. The browser is
not a proxy; it is a key holder.

What used to need a loaded page, and what still does:

| What | State |
| --- | --- |
| reCAPTCHA token | **avoidable** — the HTTP provider works server-side, once it stops reusing a single-use token |
| Project id | **avoidable** — listed and created over the transport |
| `at` and `f.sid` | avoidable — persisted, and the client primes for `at` without them |
| Current cookies | not avoidable, but a snapshot or the cache carries them across processes |
| Browser identity | not avoidable, but persisted (`data/fingerprint.json`) |

So a run with no browser can create a project, list projects, read credits, upload,
generate an image, generate a video, poll it, resolve it and download it. Verified:
`acct-dd4c92d5-9b5.mp4`, `status: "ready"`, with `flow.captcha failed: no extension
attached` in the log — the browser was not consulted.

Nothing on this list needs a page any more. Upscaling was the last holdout and it
has been removed — see "Upscaling — removed" for what it cost and why it could not
be moved to the transport.

Note that `ensureProjectTab` still runs before every generation and navigates a
tab when a bridge is attached. It is now belt-and-braces rather than a
requirement: it keeps the higher-scoring page token in play when a browser is
there, and a browserless run simply falls through to the HTTP provider. It
navigates the tab already attached rather than opening a new one, so repeated
switches do not accumulate tabs.

### Where a project id comes from

Four sources, ranked. The first two are instructions; the last two are searches,
and only one of those drives a browser.

| Rank | Source | Costs |
| --- | --- | --- |
| 1 | `--project-id` | nothing |
| 2 | `FLOW_PROJECT_ID` | nothing |
| 3 | `RPCIDProjectList` (`UpteDb`) | one HTTP call |
| 4 | the open editor's URL | a tab navigation |

The listing is preferred over the browser because both are equally current and
only one of them navigates. The browser is kept as the last resort rather than
deleted: it is the only source that reports where the *user* is rather than where
the account has been, and it still answers when the listing call is rejected.

```
engine: 2 project(s) listed over the transport; taking e5d6409a-… (modified 2026-07-12T18:11:04+05:30)
```

Rows are selected by shape rather than by offset, and a project with no assets
carries no poster — which is why `Thumbnail` and `LastAssetID` are optional. An
account with no projects answers with an empty list, not an error; that case
panicked the parser once, so it is tested.

**An empty listing is a real answer.** Verified against the app itself: when
`UpteDb` came back empty, `flow.google.com/u/0/` showed only "New project" — the
two agree. So empty means "this account has none", and it falls through to the
browser for the same reason a failed call does: neither can name a project. In
that state *both* routes are empty, and the remedy is to create a project or set
`FLOW_PROJECT_ID` — the browser is not a better source, it is the same source.

### Creating a project

The standing note here was "there is no project-create RPC". That was wrong, and
it stayed wrong because of *where* the search was done: clicking **New project**
mints a uuid in the page and navigates to `/project/<uuid>`, with no request
beside the click. Looking at the click, there was nothing to find.

The call exists — `jHPbke` — and the app does not make it. It was captured by
instrumenting the page's `fetch` and `XMLHttpRequest` rather than polling the
extension's event buffer, which rotates within seconds and lost the request every
time. The argument, decoded from that capture:

```
["projects/*", [null, ["<label>"]], [null, 22]]
```

`22` is the same constant every generation's context block carries. The label is
a display name — the app uses the local date and time — and the listing shows it
verbatim. The response is `[<project-id>, [<label>]]`, so the id is handed back
rather than having to be re-listed.

```bash
flow-go projects --new                     # create, and print the id
flow-go projects --new --label "brief 3"   # with your own display name
curl -X POST localhost:8200/v1/projects -d '{"label":"brief 3"}'
```

**The server also accepts a uuid that has never existed.** A minted one works for
generation immediately, with no registration step — verified with a freshly
generated uuid, which returned 200 and a signed content URL. But such a project
is **invisible to the app**: it never appears in the listing, and navigating a
tab to it does not make it appear. That is the difference the create call makes,
and the reason to prefer it over minting by hand.

An empty listing is a real answer, not an error. Verified against the app itself:
when `UpteDb` came back empty, `flow.google.com/u/0/` showed only "New project" —
the two agree. In that state both the listing and the browser are empty, so
`projects --new` is the way out; the browser is not a better source, it is the
same source.

### Token caching

A minted token is keyed on a hash of the cookie jar and carries a local TTL. New
cookies invalidate the token immediately, so a token can never outlive the
credentials it came from. Any call that comes back unauthenticated drops the
token and re-mints once.

### Error classification

`flowapi.APIError.Unauthenticated()` treats **all** shapes an expired credential
can take as auth failures: `401`, `407`, `403` with an auth-shaped code or
reason, and — critically — `503` carrying `NO_FLOW_KEY`. The Python version only
matched a bare `401`, so once its extension cleared its own token every later
call came back as a generic `503` and the self-heal path never ran. The pipeline
stayed broken until a restart. There is a regression test for exactly this.

### Worker pool

Every generation goes through the pool. In the Python version the pool existed
but nothing called it — `register_worker`, `acquire_worker`, and
`execute_with_failover` had no production callers, so "multi-account failover"
was display-only. Here it routes: least-busy selection, free-tier-before-paid,
affordability gating, circuit breaking after repeated failures, and failover to
another account on a retryable error.

### Statistics

Every figure reported is an aggregate over rows actually present. An empty
database reports zero. The Python version's `/stats` reported 1600 credits and 8
accounts that were entirely rows left behind by `tests/test_worker_pool.py`,
which wrote to the production database because it had no isolation. Tests here
use a temporary file per test.

## Status: it works

**Verified end to end on 2026-09-18.** `POST /v1/images/generations` generates an
image over the batchexecute transport and writes it to disk:

```
$ curl -s -X POST localhost:8200/v1/images/generations \
    -H 'content-type: application/json' \
    -d '{"prompt":"a red paper lantern floating on dark water at night","model":"narwhal"}'

status:  succeeded
model:   NARWHAL
elapsed: 26.1s
media:   460ddc37-2e6c-44c0-9183-169ebe306536
file:    output/acct-460ddc37-2e6.jpg   89864 bytes   1376x768 JPEG
```

| Capability | State |
| --- | --- |
| Extension connects, pairs, pushes scope | works |
| 158 cookies synced, 10 credential cookies | works |
| Flow project discovered from the browser tab | works |
| Flow editor tab opened and attached automatically | works |
| reCAPTCHA token from the browser broker | works — and for video it is **required**; see below |
| 27 RPCs mapped, all returning real data | works |
| **Credits** (`/v1/credits`) — matches the UI's balance exactly | **works** |
| **Image generation + download** (`/v1/images/generations`) | **works** |
| **Video generation + download** (`/v1/videos/generations`) | **works** — text-to-video, first-frame-only, and first+last |
| Media listed back out of the project | works |

No API key. No bearer token. No `aisandbox-pa.googleapis.com`.

### The reCAPTCHA token: server-side works, and a cache made it look broken

> **See [`docs/RECAPTCHA.md`](docs/RECAPTCHA.md)** for the full account — the mint
> exchange, the failure signature to recognise, a checklist for an empty
> generation, and everything already ruled out so it is not re-tested.

> **Correction, twice over.** An earlier revision claimed the user agent was the
> fix; that did not reproduce and was retracted. The retraction then claimed the
> HTTP provider was inherently intermittent and the page token was the only
> dependable one. **That was also wrong.** The real cause was a token cache in
> `HTTPProvider`, and with it removed the server-side path works — image, video,
> and a run with no browser attached at all.

The token can be minted two ways, and the default is the one that needs nothing:

| Provider | Needs a browser | Used by |
| --- | --- | --- |
| `http` (anchor/reload protocol) | **no** | `auto` (the default) and `http` |
| `flow.captcha` (page) | yes | `broker`, and only when asked for |

`auto` used to lead with `flow.captcha` and the broker, on the belief that a
page-minted token scored better. It does — and it was also why every generation
needed an extension attached and a tab parked on a Flow project. The transport
turns out to be sufficient, so the default now asks the browser for nothing, and
`ensureProjectTab` only runs in `broker` mode. Anyone who wants the page token can
still have it with `--captcha broker`.

#### What was actually wrong

`HTTPProvider` cached its token for two minutes:

```go
if p.token != "" && time.Now().Before(p.expiry) {
        return p.token, nil
}
```

**A reCAPTCHA token is single-use.** It is verified once and every later call
presenting it is rejected — with an empty frame and no error, which is why this
survived so long and why it was mistaken for a scoring problem. The cache meant
that in a long-lived process, the first generation succeeded and every one after
it inside the window silently did nothing.

The command line hid it completely: a CLI run is a fresh process, so it minted a
new token every time and appeared fine. The server — the thing this was built
for — did not. Two consecutive calls now return real media ids where they used to
return `[null]`:

```
call 1: captcha_len 2361 -> 392023ba-5f57-4cf0-…
call 2: captcha_len 2297 -> 64daa2f5-ecc4-49cd-…
```

#### Verified server-side, no browser involved

```
recaptcha: provider flow.captcha failed: recaptcha: no extension attached
recaptcha: token acquired via http (2340 chars)
engine: saved acct-7f3d13f8-874.jpg (0.1 MB, image/jpeg)
engine: generated 1 image(s) in 26.7s
```

and through the server, into a project created over the transport:

| | Result |
| --- | --- |
| image | `acct-02dec3f7-b35.jpg`, `status: succeeded`, 25.7s |
| video | `acct-dd4c92d5-9b5.mp4`, `status: **ready**`, 28.7s, 4 credits |

#### What is still not explained

The failures at 08:46–08:48 — a fresh CLI process, one call, so no cache involved
— are **not** accounted for by this. Between those runs and the working ones the
account, its cookies and its project all changed, and the old account's project
listing had gone empty more than once, so a generation against a project that no
longer existed would have produced exactly the same empty frame. That is the
likeliest explanation and it is not proven; the honest statement is that the
cache was a real bug, fixing it made the server-side path work, and the earlier
standalone failures have a separate and untested cause.

#### The user agent, kept

`WithUserAgent` stays. A captcha-bearing call is checked against the client its
token was minted for, so presenting the browser's own identity is right on
principle even though it was not what unblocked anything. Same for the origin
parameter, which is derived from `recaptchaOrigin` so the two cannot drift apart.

#### What does not work, and why it is worth knowing

The page mints through `POST https://www.google.com/recaptcha/enterprise/clr?k=<siteKey>`
with a 1908-byte protobuf body — field 1 is the site key, field 2 is 1864 bytes of
opaque state. Two mints produce **byte-identical** bodies, so replay looks
obviously viable.

It is not. The response is empty, **including for the page itself**. The token is
assembled in JS by `enterprise.js` from that challenge plus client-side signals;
no request returns it. There is no cookie to carry and no request to replay —
but none of that matters, because the anchor/reload flow the provider already
uses is sufficient.

This matters because the failure is otherwise silent. Flow accepts the request,
answers `200`, returns an empty frame and charges nothing. There is no error to
catch and no status to check, so it reads exactly like a wrong model key or a
wrong RPC id. The diagnostic now names the provider and the two causes that have
actually produced it:

```
engine: nothing submitted for abra_t2v_4s_360p — it costs 4 credits at 360p and the
account has 31, which was checked and covers it; so the balance is not the cause.
The reCAPTCHA token came from "http". A token is single-use, so a reused or cached
one produces exactly this — Flow verifies it once and answers an empty frame on
every later call. Check next that the project exists on the account in use:
generating into a project that is not there is accepted the same silent way. Only
then suspect the model key and the RPC it went to.
```

### Video

Video uses a **different RPC**, which is why sending a video model through the
image RPC is accepted and silently does nothing. `YhhmEf`, decoded from the app in
Video mode:

```
[[[ [null, null, [[[prompt]]]], "<model>", 2, null,
     [null, null, null, null, "<uuid>", "<uuid>"] ],   // one per variation
  ...],
 [null, 22, null, null, null, "<project-id>", null, null, null, null,
  ["<recaptcha-token>", 1]],
 ["<batch-uuid>", 2]]
```

The prompt block sits at `[0]` rather than `[8]`, and the context block at `[1]` is
the same shape the image RPC uses, reCAPTCHA token included.

Verified: `POST /v1/videos/generations` with `{"duration":4}` returned media id
`12aaf31c-…` in 7.8s and moved the balance 357 → 350, so a 4-second video costs 7
credits. The media id and prompt were then read back out of the project.

Video is **asynchronous**, so the URL only exists once the render finishes. A
third RPC resolves it: **`as29s`**, whose `source-path` must carry an
`/edit/<media-id>` suffix, and whose payload is the asset's **content id** — for
the nested listing shape that is `detail[4]`, which is a different uuid from the
media id the generation returned **and** from the third id at `detail[5]`.
Passing either of the others is answered with a `null` payload and no error, so the
wrong id is indistinguishable from a render that is still going. Its response
carries both the poster (`/image/<content-id>`) and the asset
(`/video/<content-id>`), each separately signed, so filtering by kind matters:
taking the first URL would download the poster as if it were the video.

Verified end to end:

```
$ curl -s -X POST localhost:8200/v1/videos/generations \
    -H 'content-type: application/json' \
    -d '{"prompt":"a small paper boat drifting on a calm river at sunrise","duration":4}'

status:  ready
model:   abra_t2v_4s
media:   1370d7c6-6e2f-4ebb-b2fa-b40da7204012
url:     https://flow-content.google/video/451f2cef-…?Signature=_1-roTgWXucAhuQnbSb3VJ7M8qs
file:    output/acct-1370d7c6-6e2.mp4   1938886 bytes
credits: 343   (350 → 343, so a 4s video costs 7)
elapsed: 40.7s  (submit + wait for the render + download)

$ file output/acct-1370d7c6-6e2.mp4
ISO Media, MP4 Base Media v1 [ISO 14496-12:2003]
```

#### It broke, and why it was hard to see

That block was true, then stopped being true, and nothing said so. Two defects,
either of which alone would have hidden the other:

1. **The listing's id pair was read backwards.** The parser called `detail[4]` the
   media id and `detail[5]` the content id; it is the other way round. So
   `ResolveVideoURL` handed `as29s` the wrong uuid, and the call does not reject a
   wrong uuid — it answers `200` with a `null` payload — so the code read it as
   "not ready yet" and kept waiting.
2. **The "not ready" error was not marked retryable.** `"no video URL … yet (still
   rendering?)"` was not wrapped, so the poll treated a still-rendering video as a
   permanent failure and abandoned it after **10 seconds**.

Defect 1 made the poll wait forever for a URL it was never going to be given.
Defect 2 made it give up instantly. Fixing only one still fails: with just defect 2
fixed the poll waits the full 420s and then reports "still unresolved"; with only
defect 1 fixed it abandons a render that was one second from ready.

The render had been completing the whole time. Recovered after the fix:

```
$ file output/paper-boat-4s-720p.mp4
ISO Media, MP4 Base Media v1 [ISO 14496-12:2003]      1934912 bytes
```

And the first clean run after it:

```
status:  ready          (was: submitted, after 438s of failing)
media:   74c50a39-fe1f-4ff5-9536-4682c8efd61e
file:    output/acct-74c50a39-fe1.mp4   1640064 bytes
credits: 11  (18 → 11, so a 4s 720p video costs 7)
elapsed: 41.6s
```

### Upscaling — removed

Image and video upscaling were removed. Both worked; the reason for removing them
is worth one paragraph each, because the shape of the work is not obvious from the
absence of the code.

**`SPrCad` (image, 2K / 4K) ran inside the attached tab.** The Go transport was
rejected with `PUBLIC_ERROR_UNUSUAL_ACTIVITY` even with the payload, the URL, the
query parameters and both ids matching a request captured from the app itself,
byte for byte — including the media id in position 0, a source path naming only
the project, a timestamped captcha pair, and `bl` and `f.sid` present. So it was a
client-level check, not a malformed request, and nothing the client could vary got
past it. The browser was genuinely required.

**`p0UkFb` (video, 1080p / 4K) ran over the transport** and needed no browser.

The full elimination is in git history —
`git log --all --oneline -- '*upscale*'` — and is worth reading before anyone
tries to bring the transport path back.

**Two things survived the removal on purpose.** `httpx.WithProfile` and
`httpx.WithProtocolRacing` are now called by nothing but their own tests, and they
are kept: they are the only way to vary the TLS fingerprint and the protocol at
runtime, and this project has hit that question twice already. A rejection that
does not move when the fingerprint changes is not a fingerprint check, and without
these there is no way to establish that. Everything else the upscale diagnosis
needed — the per-request header order and QUIC toggles on the batchexecute client,
the raw-body helper, the operation poller, the credential-stripping jar — went with
the routes that used it.

**One thing that outlived the feature and still matters:** an asset and its
upscales share one media id, so a media-id lookup returns whichever row comes
first. `ResolveContentID` has to pick by type code — `CAE` for the original — and
`MediaDetail` still depends on that. The listing parser and the URL regex were both
broken in ways that only showed up through the upscale path, and both are still
covered by tests in `internal/batchexecute`.


### What is not done yet

- **Aspect ratio and resolution.** The app's composer exposes them (16:9 / 9:16,
  360p / 720p, and an Image/Video and Frames/Ingredients toggle), so the UI
  mapping is known — the payload positions are not. Until they are, the endpoints
  **refuse** the fields rather than dropping them; see below.
- **First-frame-only and reference-to-video.** First+last-frame and first-only both
  work now; reference-to-video and edit do not, and the model catalog names them all:

  | Composer mode | Model keys | RPC | Status |
  |---|---|---|---|
  | Frames, first + last | `omni_flash_i2v_*_first_last*` | `nprQif` | **works** |
  | Frames, first only | `abra_i2v_*` | `eb1hJf` | **works** |
  | Ingredients (existing asset) | `abra_edit` | `jIps6` | **works** — `POST /v1/videos/edit` |
  | Reference images | `abra_r2v_*` | `MZZa6b` | **works** — `POST /v1/videos/reference` |

  **`MZZa6b` is the reference-image RPC, and it hides better than any of the others.**
  An `abra_r2v_*` model sent to `eb1hJf` — the single-image RPC — is **accepted and
  returns a media id**, so a probe looks like a success while nothing of the kind
  happens. That produced a wrong conclusion here once; the corrected note is that
  `eb1hJf` serves `abra_i2v_*` *and* accepts `abra_r2v_*` without doing r2v, and the
  real path is this one.

  The captured payload, from four reference images:

  ```
  [ [null, null, [[[prompt]]]],                  <- prompt at index 0
    [[null, "<id>"], [null, "<id>"], ...],       <- references at index 1
    "abra_r2v_4s", 2, null,
    [null, null, null, null, <uuid>, <uuid>],
    null, null, null, null,
    [["d351dd3c-0a12-1522-0000-000000000000"]] ] <- fixed, see referenceTail
  ```

  Note the layout matches neither of the others: the i2v shape puts the prompt at
  index 2 with images in trailing slots, the edit shape is five elements, and this
  one front-loads both the prompt and a *list* of references.

  Verified live: the capture came from a browser session whose project then showed
  the finished video, so the shape is complete rather than merely accepted.
  `TestBuildReferenceArgumentMatchesCapture` pins it byte-for-byte, and
  `TestReferenceShapeIsNotTheImageShape` asserts the two layouts cannot be confused.

  **`abra_edit` is the video-to-video model.** The Ingredients composer mode
  attaches an existing project asset, and the submission it builds names
  `abra_edit` and goes to `jIps6`. That looks like an image-to-video path until the
  source id is resolved: the one in the capture, `b8864807-…`, is a **video** in
  the project, which is what identifies the model. Replaying the captured payload
  returns a media id with either a video or an image content id as the source.

  The edit payload is shaped differently from a generation — the source sits in its
  own block at index 0, *ahead* of the prompt, and the request carries five elements
  rather than the generation's longer list:

  ```
  [ [null, "<content-id>", 0, 192],
    [null, null, [[[prompt]]]],
    "abra_edit",
    2,
    [null, null, null, null, <uuid>, <uuid>] ]
  ```

  A generation layout sent to `jIps6` is accepted and returns an empty result, so
  the two builders must not be mixed up; `TestBuildEditArgumentMatchesCapture` pins
  the comparison against the capture.

  **First-only was never a payload problem.** A submission goes to one of three RPC
  ids purely on the *shape* of its conditioning, and first-only has its own:

  | conditioning | RPC |
  |---|---|
  | text only | `YhhmEf` |
  | start frame only | `eb1hJf` |
  | start + end frame | `nprQif` |

  Sending a correct first-only payload to `nprQif` — which is what six earlier
  attempts did, across two models, with and without a crop, and with the unused slot
  padded — is **accepted with a 200 and returns an empty result**. The server does
  not reject a payload sent to the wrong id; it just does nothing, which is why every
  payload-level check kept passing while the feature was dead.

  The capture that settled it came from driving the composer with only the Start chip
  filled and reading the request off CDP (`Network.setBlockedURLs` blocks it at the
  network layer, so nothing is generated and no credits are spent, while
  `Network.requestWillBeSent` still reports the URL and body). See
  `docs/captures/start-only-i2v.json`.

  Three ids now exist as constants and `VideoRPCID` picks between them; `config.VideoModelFor`
  picks the matching model for the same three cases, so a caller that supplies images
  and no model no longer gets a `abra_t2v_*` key.

### The model catalog is the reference for what exists

`HTrJv` returns **159 model keys**. The naming carries the capability, and the keys
fall into families — only some of which this engine uses:

```
abra_t2v_4s / _6s / _8s / _10s          text to video, by duration      ← used
abra_t2v_8s_360p                        ... the _360p suffix is resolution
abra_i2v_*                              image to video, first frame only ← used
omni_flash_i2v_8s_first_last_360p       image to video, first + last     ← used
abra_r2v_*                              reference images
abra_edit / abra_edit_360p              video edit
                                        (no upsampler keys — upscaling removed)
```

**The engine selects from `abra_*` and `omni_flash_*` only.** The catalog also holds
a large `veo_*` family — `veo_3_1_t2v_*`, `veo_3_1_i2v_*`, `veo_3_1_r2v_*`,
`veo_3_1_extend_*`, `veo_3_1_interpolation_*` — with capabilities this engine does
not implement (extend, interpolate, object insertion and removal, camera control)
and, for some, an aspect pair via a `_portrait` suffix. None of those are wired up,
and naming one outright is not a supported path. The only `veo_*` keys in use are
the two upsamplers, which are a second pass rather than a generation model.

**Resolution is the `_360p` suffix**, which is why the model keys carry it
rather than a parameter.

#### Aspect is decided by the model family; `_360p` is only resolution

Measured, all four on the same account:

| request | model | output | |
|---|---|---|---|
| text-to-video, 720p | `abra_t2v_4s` | **1280×720** | landscape |
| text-to-video, 360p | `abra_t2v_4s_360p` | **640×360** | landscape, half |
| image-to-video, 720p | `abra_i2v_4s` | **720×1280** | portrait |
| image-to-video, 360p | `abra_i2v_4s_360p` | **360×640** | portrait, half |

**The family decides the aspect and the suffix decides the size.** `abra_t2v_*` is
landscape at both qualities and `abra_i2v_*` is portrait at both, and `_360p` halves
the pixels either way — it does not rotate anything.

That matters because it is the opposite of what this section said twice. The first
reading blamed `_360p` for the portrait output after comparing a 720p text-to-video
against a 360p image-to-video — two variables, and the wrong one was blamed. The
second reading kept the blame and added a claim that `abra_t2v_4s_360p` never
renders, which was also wrong: its earlier failures were a stale session and a
cookie file that was hours out of date, not the model.

**The catalog says why.** Neither family carries an aspect variant — only duration
and `_360p` — so within `abra_*` the aspect is not selectable, and it follows from
the conditioning instead: text conditioning renders landscape, image conditioning
renders portrait, and the source image's own shape does not carry across.

There is no aspect parameter to reach for instead. `BatchVideoRequest` has no
`Aspect` field, and the composer's 16:9 / 9:16 toggle does not reach the request
either (below) — the model key is the only place a choice could be expressed, and
this family has nothing to express it with.

**`veo_*` keys are not used here.** The catalog does carry an aspect pair for them —
`veo_3_1_i2v_s_fast_4s` beside `veo_3_1_i2v_s_fast_4s_portrait` — but the engine
never selects a `veo_*` generation key, and naming one outright is not a supported
path. The only `veo_*` keys the engine does use are the **upsamplers**
(`veo_3_1_upsampler_1080p` / `_4k`), which are a second pass over a finished render
rather than a generation model.

#### `abra_t2v_4s_360p` renders — the earlier failures were not the model

This section previously said the key never renders, on the strength of one 440s
submission that returned a media id and never produced an asset, plus six older
attempts that had also failed. That conclusion was wrong. Run again:

```
POST /v1/videos/generations  {"duration":4,"quality":"360p"}
  -> model  abra_t2v_4s_360p
  -> status ready, 640x360, 592 KB, 37.8s, 4 credits
```

The failures were environmental: a session that needed refreshing and a cookie file
that was **eight and a half hours out of date** (`loadJar` read `cookies/cookies.json`
while the bridge wrote `data/cookies.json`). A render that is accepted and never
lands is what a dead session looks like from here, and it was recorded as a property
of the model.

So **the cheapest video is 4s at 360p for 4 credits**, and it is a landscape render
at half the size of 720p. The cost gate refuses rather than downgrading, which is
still right for a different reason: substituting a quality the caller did not ask
for is a different render, and it hid a genuine failure behind a plausible one.

Neither the `abra_*` nor the `omni_flash_*` keys — the only families this engine
selects from — carry an aspect variant. The catalog has a `_portrait` suffix on some
`veo_*` keys, but those are not used here.

#### The composer's aspect toggle does not reach the request at all

The composer has a 16:9 / 9:16 toggle (`button[radio]`, and it reports `9:16` as
checked on this project). Two things were measured about it:

**Clicking it sends no request.** After toggling to 16:9 the only traffic was the
`WuwhI` page-view beacon. So the choice lives in client state and, if it is persisted
at all, is not persisted at the moment it is made.

**It does not change the generation payload.** Capturing a text-to-video submission
with the toggle on 16:9 gives a payload identical to the one captured with it on 9:16
— same array, same `[2]` mode, same nulls, same trailing `[uuid, 2]`. Nothing moved.

**And it does not change the output.** Text-to-video renders **1280x720** with the
toggle on 9:16; the only portrait videos this project has produced came from the
`omni_flash` image-to-video model, and that one renders 360x640 no matter what.

So aspect is a property of the **model key**, not a request field, and the toggle is
a preview control. A landscape image-to-video needs a model that offers one; the
`omni_flash` first+last keys do not.

#### The frame crop does not set the aspect either, and that model is always portrait

An earlier note here suggested the crop rectangle selects the output aspect, on the
arithmetic that `0.6582 - 0.3418 = 0.3164` and `1376 x 0.3164 = 435`, and
`435 / 768 = 0.567` — a 9:16 crop of a 16:9 source. **That was tested and is wrong.**

Three first+last runs, same model, same two 1376x768 sources:

| `start_frame` / `end_frame` | Output |
|---|---|
| omitted | 360x640 |
| `[0.3418, 1, 0.6582]` (the capture's values) | 360x640 |
| `[0, 1, 1]` (full width) | 360x640 |

So the crop has no observable effect on the output, and
`omni_flash_i2v_*_first_last_360p` renders 360x640 regardless of landscape sources.
Where a landscape image-to-video comes from is not known — the `veo_3_1_i2v_*`
keys without `_portrait` are the obvious candidates, but nothing here uses them.

#### The families do not overlap, and `omni_flash` is narrow

Worth knowing before picking a model: `omni_flash` covers **only** first+last-frame
image-to-video. Everything else is the `abra` family.

| Capability | `omni_flash` | `abra` |
|---|---|---|
| Text to video | — | `abra_t2v_*` |
| Image to video, first frame only | — | `abra_i2v_*` |
| Image to video, first + last | **`omni_flash_i2v_*_first_last*`** | — |
| Reference images | — | `abra_r2v_*` |
| Video edit | — | `abra_edit` |

**Nothing in this implementation uses a `veo` model.** Text-to-video is
`abra_t2v_*` and image-to-video is `abra_i2v_*` / `omni_flash_i2v_*_first_last_*`.
The catalog holds a large `veo_*` family with capabilities this engine does not
implement, and it is deliberately not selected from — see "The model catalog is the
reference for what exists" above.

The catalog is worth querying before guessing at any capability:

```
POST /v1/debug/batchexecute  {rpc: "HTrJv", payload: "[]"}
```

### Unsupported options are refused, not ignored

Both generation endpoints used to accept fields they then dropped on the floor.
`/v1/images/generations` took `count`, `aspect`, `seed` and `reference_images` and
forwarded none of them; `/v1/videos/generations` took `model`, `aspect`,
`resolution`, `seed`, `start_image`, `end_image`, `reference_images` and
`audio_preference` and forwarded none of them either.

The failure mode is the one that has cost this project the most: **a `200` with a
different result than the caller asked for.** Ask for four images, get one, and
nothing says why.

They now return `400` naming the offending fields:

```
unsupported option(s): count — not implemented on the batchexecute transport
```

`model`, `count`, `start_image` and `end_image` on the video endpoint are *not*
refused — the engine accepts all of them, so they are forwarded. `model` was
previously accepted by the request type and dropped by the handler, which made it
silently inert; the condition images were refused until their payload was captured
and implemented.

### The legacy `aisandbox` REST surface is alive — and separate

The Python engine this port replaces used `https://aisandbox-pa.googleapis.com`.
This README previously said that surface "cannot work". **That is wrong**, and the
correction matters because it is the obvious place to look for the missing
generators.

What a live probe showed:

| Call | Result |
|---|---|
| `GET /v1/credits` (Bearer + `labs.google` referrer) | **200** — a separate meter: 50 credits, `G1_FREEMIUM` |
| `POST /v1/video:batchAsyncGenerateVideoText` with an **empty** captcha | `403 reCAPTCHA evaluation failed` |
| The same call with a **real** captcha token in `clientContext.recaptchaContext.token` | **200** — media created, `remainingCredits: 43`, `projectId` = ours |

So the old engine's failure had a single cause: **it sent an empty captcha token**.
With one, the surface answers.

**But its output is invisible to the Flow web app.** The video it created reports
`MEDIA_GENERATION_STATUS_SUCCESSFUL` under our project id, and never appears in the
batchexecute project listing — polled repeatedly. The two surfaces are separate
stores: REST cannot see media created over batchexecute either (`GET /v1/media/<id>`
answers `400 INVALID_ARGUMENT`). Generating there would produce videos the user
cannot see in Flow, so it is not a route to the missing features.

It is still the best **semantic** reference available, and it is where the field
names below come from — `flow_engine/generators/*.py` in the sibling `flow-agent`
checkout:

| Concept | Field | Values |
|---|---|---|
| Aspect ratio | `aspectRatio` | `VIDEO_ASPECT_RATIO_PORTRAIT`, `VIDEO_ASPECT_RATIO_LANDSCAPE` |
| Resolution | upsampler model + enum | `veo_3_1_upsampler_1080p`/`_4k`, `VIDEO_RESOLUTION_1080P`/`_4K` |
| Image input | `startImage.mediaId`, `endImage.mediaId` | i2v and first+last-frame |
| Reference input | `referenceImages[].mediaId` + `imageUsageType` | `IMAGE_USAGE_TYPE_ASSET` |
| Video input (edit) | `videoInput.mediaId` + `startFrameIndex`/`endFrameIndex` | `abra_edit` |
| Video model keys | `videoModelKey` | `abra_t2v_{4,6,8,10}s`, `abra_edit` |
| Image models | — | `NARWHAL`, `HARBOR_SEAL`, `GEM_PIX_2` |
| Cost | — | 4s=7, 6s=10, 8s=12, 10s=15 credits |

That reference gives the **semantics** of every missing feature. It does not give the
batchexecute wire format, which is positional and still needs a capture.

### The path that got here

Every step below was a wrong assumption that a live probe corrected.

1. **The API key was assumed dead.** `AIzaSyBtrm0o5ab1c-…`, inherited from the
   Python engine, is referrer-restricted to `labs.google` — and the app had moved to
   `flow.google.com`. The key is not dead, though; see the section above.
2. **The app does not use that API at all.** Capturing the page's own traffic
   showed exactly one backend call:
   `POST https://flow.google.com/_/AiSandboxAngularFrontend/data/batchexecute`.
3. **Auth is cookies plus `SAPISIDHASH`** — `sha1("<seconds> <SAPISID> <origin>")`.
   The entire bearer-token path was unnecessary.
4. **An un-tokened call returns `400` with an `xsrf` token in the body.** That is
   the first half of a handshake, not an error.
5. **The RPC catalog was read off the page-load traffic** — 16 ids, all working.
6. **The generation RPC is `ogiZ0b`**, captured by driving the editor over CDP
   (type into `.ProseMirror`, click `button[aria-label="Start generation"]`).
7. **The mystery `0cAFcWeA…` blob is the reCAPTCHA token.** ~2500 characters, in
   both `ogiZ0b` and `WuwhI`, present in no RPC response, no storage, no cookie
   and no global — because the page mints it at generation time. Matching its
   prefix and length against tokens from the page's own `grecaptcha` settled it.
8. **The prompt nests three levels deep, not four.** With four the server returns
   `200` and generates nothing — a silent no-op no status code reveals.
9. **The signed media URL must be unescaped before it is extracted.** It carries
   `\u003d` for `=`, so pulling it out early truncates the signature and the
   download fails with a 403 that looks like an auth problem.
10. **Flow serves images as JPEG**, so the file extension comes from the response
    content type rather than from what was requested.
11. **The credits endpoint was reporting the wrong account's balance.** The
    legacy aisandbox `/v1/credits` said 50. The real balance is read from the
    `nzlxg` RPC, which returns `[412, 1, 2, 2, null, 412]` — and 412 is exactly
    what the Flow account menu shows. The old endpoint's token belonged to a
    *different* signed-in Google account than the one the app session uses, so it
    was never going to agree.
12. **The nested listing's id pair was read backwards.** `detail[4]` was called the
    media id and `detail[5]` the content id; it is the reverse. Every id is a uuid,
    so the swap was invisible — the asset looked valid and only the RPC that wanted
    the other id failed, and it failed by answering `200` with a `null` payload. A
    finished render was therefore indistinguishable from one that never finished.
    This is the one that cost the most: it hid a *working* pipeline behind a
    seven-minute wait, twice, for 14 credits.
13. **An uploaded image was rejected by the row matcher**, because it required both
    `detail[4]` and `detail[5]` and an upload has no `detail[5]` — it has no
    derived variant. So every uploaded asset was invisible, and image-to-video from
    an upload reported "not in the project listing" for an asset that was in the
    listing.
13. **"No video URL yet" was not marked retryable.** The poll only waited on
    `ErrAssetNotListed`; the other half of the same wait — listed, URL not ready —
    was treated as permanent, so a render was abandoned after 10 seconds. Fixing
    either one alone still fails.
14. **The session endpoint is `authuser`-blind.** It answers for the *default*
    account whatever index you pass, so reading an address from it labels every
    signed-in account with the first one's. Three distinct accounts were reported
    under one email, which reads as "they share an email" rather than as a bug.
15. **A deliberate account choice did not survive a restart.** The index lived in
    memory, so every restart reverted to index 0 — which is how a 1-credit account
    came to look like the only one available.
16. **Nothing checked whether the account could afford the render.** The server
    accepts an uncovered submission and answers with no media, so an empty wallet
    presents as a broken request. It now refuses with `402` and the arithmetic, and
    moves to another signed-in account when one can pay.

Ten of those sixteen would have been invisible to a passing test suite. Each has a
regression test where the fix is behavioural.

### What the browser is still needed for

Cookies. That is the whole list: the captcha is minted over the transport, projects
are listed and created over it, and generation never travels through the page. See
`docs/WHAT-WE-NEED-FROM-THE-BROWSER.md`.

## Layout

```
flow-go/
├── main.go                     entry point
├── flow-go-extension/          the narrow Flow bridge (this repo's extension)
├── internal/
│   ├── app/                    assembly and lifecycle
│   ├── auth/                   Labs session → access token
│   ├── batchexecute/           the RPC transport the app actually uses
│   ├── cli/                    command line
│   ├── config/                 endpoints, models, credits, ports
│   ├── engine/                 orchestration
│   ├── flowapi/                legacy aisandbox REST client
│   ├── httpx/                  Chrome-impersonating transport
│   ├── pool/                   worker pool (images, uploads)
│   ├── recaptcha/              reCAPTCHA Enterprise strategies
│   ├── server/                 HTTP API
│   └── store/                  SQLite persistence
├── docs/ARCHITECTURE.md
└── .env.example
```

Not in this module: `cdp-control/{bridge,cdp,cookiejar}` and the generic
`browser-Cdp/extension/`. Both belong to the sibling `browser-Cdp` project and are
consumed through a relative `replace` in `go.mod`.

## Responsible use

This drives undocumented Google endpoints with your own signed-in account. Use it
only with accounts you are entitled to use, keep request rates conservative (the
defaults are), and respect the target service's terms. The rate limiter and
startup cooldown exist because Google throttles accounts that fire bursts, and
the throttle surfaces as a soft `UNUSUAL_ACTIVITY` block rather than a clean
error.
