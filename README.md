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
tokens, a reCAPTCHA token, and one in-page upscale call — and does the rest in Go.

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
| POST | `/v1/videos/upscale` | Upscale a finished video to 1080p (`p0UkFb`, submit then poll) |
| POST | `/v1/images/upscale` | Resolve an image at 2K or 4K (`SPrCad`, run in the browser) |
| POST | `/v1/bridge/refresh` | Ask the browser to renew its session, then re-sync |
| GET | `/v1/jobs` | Recent jobs |
| GET | `/v1/jobs/:id` | One job |
| GET | `/v1/media` | Recent generated media |
| GET | `/v1/accounts` | Tracked accounts |
| GET | `/v1/accounts/credits` | Email, name and balance of every signed-in account (`?max=N`, default 4) |
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
`authuser`; `session.Sku` is still read from the blind endpoint and is **not**
trustworthy per account.

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
no media, which reads as a broken request rather than an empty wallet. When 720p
does not fit but 360p does, the render is downgraded rather than refused, and the
response says so (`quality`, `credits_cost`, `quality_downgraded`).

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
| `SPrCad` (image upscale) | content id |
| `p0UkFb` (video upscale) | **both** — content at `request[0]`, media at `request[4]` |
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
carries the ids in its first row: `[content-id, project, media-id, "CAE", ...]`.

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
batchexecute: SPrCad returned 401; refreshing the session and retrying
bridge: attached to https://flow.google.com/project/… for a session refresh
```

Diagnostics. These exist because a rejected RPC says nothing about *which* part
was wrong, so the only way through is to vary one thing at a time:

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/debug/headers` | The exact headers and Cookie a call would send, so they can be diffed against the browser's |
| POST | `/v1/debug/captcha` | Mint a reCAPTCHA token and hand it back, to replay it from the page |
| POST | `/v1/debug/events-raw` | Raw CDP events. `requestWillBeSentExtraInfo` is the only place the browser's *real* headers, Cookie included, are visible |
| POST | `/v1/debug/image-upscale` | `SPrCad` over the Go transport, with the raw response body. Takes `header_overrides`, `header_order`, `tls_profile`, `protocol_racing`, `build_label`, `at_token`, `source_path`, `session_id`, `use_quic`, `drop_credentials` |
| POST | `/v1/debug/video-upscale` | `p0UkFb` over the Go transport, returning the queued asset id |
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
flow-go generate --prompt "..." --duration 8 --resolution 1080p
flow-go image --prompt "..." --aspect square
flow-go stats                          # database statistics
flow-go export stats.json              # full JSON export
flow-go cookies                        # cookie and credential status
```

`flow-go cookies` never prints a cookie value — only counts, names on request,
and whether credentials are present.

## The two extensions

They are **separate projects** and neither is a subset of the other. Loading both is
fine and is the normal debugging setup.

| | `flow-go-extension/` | `../browser-Cdp/extension/` |
| --- | --- | --- |
| Chrome name | **Flow Go Bridge** | **browser-Cdp** |
| Belongs to | this repo | the `browser-Cdp` project |
| Surface | a fixed list of Flow operations (`flow.at`, `flow.captcha`, `flow.upscale`, …) | arbitrary CDP: `cdp.call`, `cdp.evaluate` |
| `debugger` permission | **no** | yes |
| Host access | Google hosts only | `<all_urls>` |
| Empty scope means | refuse (fail-closed) | allow (fail-open) |

**flow-go uses the narrow one.** It needs cookies, a page token, a reCAPTCHA token
and one in-page upscale call — all of which are named operations. Arbitrary CDP is a
debugging convenience, not a requirement, and the narrow extension is the one that
cannot be talked into driving an unrelated site.

Load it as unpacked from `flow-go/flow-go-extension/`. The generic one is worth
loading too when you need `cdp.evaluate` to see what a page actually looks like; it
connects to the same backend.

Both dial `ws://127.0.0.1:9222`, so **exactly one backend can own that port**. The
backend tells them apart from the `ops` list each reports on `ping`, and prefers the
narrow one as `current` when both are attached. The generic one is still reachable
by address — `/v1/debug/cookies {"all": true}` lists every attached client.

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
| `--captcha` | CLI flag, **not** an environment variable: `auto` (default), `broker`, `http`, or `off`. Setting `FLOW_RECAPTCHA` does nothing. |
| `WS_PORT` / `HTTP_PORT` | Environment variables. Extension bridge and API ports. |
| `ACCOUNT_INDEX` | Environment variable. Seeds which signed-in account to act as. Only a seed — once `/v1/accounts/switch` has been used, the stored index wins, so a deliberate choice survives a restart. |
| `ACCOUNT_SCAN_LIMIT` | Environment variable, default 6. How many account indices are examined when looking for one that can pay. Chrome permits ten, but the scan costs a session, a profile and a balance read per index and runs on the request path. |
| `FLOW_ACCESS_TOKEN` | Environment variable. Supply a bearer token directly, bypassing cookie minting. |
| `FLOW_SESSION_REBUILD` | Environment variable. `off` disables the pure-Go session rebuild, leaving the raw upstream error visible. |

## Design notes

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
| reCAPTCHA token from the browser broker | works |
| 27 RPCs mapped, all returning real data | works |
| **Credits** (`/v1/credits`) — matches the UI's balance exactly | **works** |
| **Image generation + download** (`/v1/images/generations`) | **works** |
| **Video generation + download** (`/v1/videos/generations`) | **works** — text-to-video, first-frame-only, and first+last |
| Media listed back out of the project | works |
| **Image upscale** (`/v1/images/upscale`) | **works** — 1376x768 -> 2752x1536, verified with ffprobe |
| **Video upscale** (`/v1/videos/upscale`) | **works** — 1280x720 -> 1920x1080, verified with ffprobe |

No API key. No bearer token. No `aisandbox-pa.googleapis.com`.

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

### Upscaling — solved

The media viewer's download menu is what gave this away — it offers **270p
(Animated GIF)**, **720p (Original size)** and **1080p (Upscaled)**. (Images also
offer 4K; this account's video menu does not.) Clicking 1080p captures the RPC:
**`p0UkFb`**.

```
[[[ [null, "<content-id>"], null, 2, null,
     [null, "<media-id>", null, null, "<uuid>"],
     null, 2, null × 24,
     "<upsampler-model>" ]],
 [null, 22, null, null, null, "<project-id>", null, null, null, null,
  ["<recaptcha-token>", 1]],
 ["<call-uuid>"]]
```

Four details in that payload are load-bearing, and every one of them was wrong in
the first attempt:

| | Correct | First attempt |
|---|---|---|
| request array length | **32** (model at index 31) | 35 (model at 34) |
| `request[0][1]` | the source asset's **content id** | a fresh uuid |
| `request[2]` | `2` | `1` |
| third top-level element | **`["<call-uuid>"]`** | absent |

The failure mode is what made this hard: with the wrong shape the server returns
`200`, a well-formed empty result, and renders nothing. There is no error to
notice. The model keys are `veo_3_1_upsampler_1080p` and `veo_3_1_upsampler_4k`.

**The reply is a submission acknowledgement, not a result.** It carries no URL — it
names the render it queued, `<content-id>_upsampled`. That id is then polled for in
the project listing until it appears, and resolved to a URL like any other video.
Reading that reply as "no output" is exactly what made this RPC look broken.

Measured end to end with `ffprobe`:

| | Width × Height |
|---|---|
| Original (720p) | 1280 × 720 |
| `p0UkFb` 1080p | **1920 × 1080** |

Unlike the image upscale, this one runs **over the Go transport** — no browser
needed beyond the captcha broker.

#### The project-listing parser was broken, and it hid the whole thing

Worth recording because it caused a silent 7-minute timeout rather than an error.

A listing row is:

```
[content-id, project-id, media-id, type-code, null, detail, ...]
```

The parser had two faults. It read the media id from `row[0]` (that is the
*content* id) and the content id from `row[3][4]` (that is a string, `"CAE"` for an
original or `"CAI"` for a derived asset). Worse, `findEntryList` *identified rows*
by requiring `row[3]` to be an array — which no row is — so every listing parsed
to **nothing at all**.

The consequence was invisible: `waitForNewVideo` and `waitForUpscaledAsset` simply
polled an empty list until their timeout, and the only symptom was a job that
stayed `submitted`. Anything that resolves a media id to a URL was affected, since
`ResolveVideoURL` reads the same listing.

#### …and the URL regex could not match an upscaled asset

Fixing the listing exposed a third fault. The signed-URL matcher captured the
content id as exactly 36 hex-or-dash characters:

```go
`https://flow-content\.google/(image|video)/([0-9a-fA-F-]{36})\?[^"\\\s]*`
```

An upscaled asset's id is `451f2cef-…_upsampled` — 46 characters, with an
underscore — so **no upscaled URL ever matched**, and `MediaDetail` kept reporting
"still rendering". The fix allows the optional suffix:

```go
`https://flow-content\.google/(image|video)/([0-9a-fA-F-]{36}(?:_[a-z]+)?)\?[^"\\\s]*`
```

#### The last trap: an asset and its upscales share one media id

With the URL resolving, the endpoint reported success and downloaded the **source**
video — 1.9 MB at 720p. Both rows carry the same media id, so a media-id lookup
returns whichever comes first, which is the original. The upscale has to be
resolved by its own *content* id.

That is three independent faults stacked on one feature, and none of them produced
an error. The end-to-end result is now 1280×720 → **1920×1080** in about 8 seconds.

### Image resolution — solved, and it is not a download-URL trick

An image's download menu offers exactly three choices:

```
1K | Original size
2K | Upscaled
4K | Upscaled
```

The labels are literal. **1K is the size the asset already has** and needs no call
at all; only the two "Upscaled" entries do anything.

Clicking **2K** sends one RPC: **`SPrCad`**.

```
["<content-id>", <selector>, [null, 22, null, null, null, "<project-id>",
                              null, null, null, null, ["<captcha>", 1]]]
```

**The response carries the image itself**, as base64 JPEG inside the frame — not a
URL. That is the whole mechanism, and it explains two things that had looked
contradictory: why no new project asset appears (nothing is re-created), and why
the download is a `blob:` (the page decodes the base64 and saves it). There is no
higher-resolution URL variant to request; the bytes arrive in the RPC response.

Measured with `ffprobe`, on an image generated at 1376x768:

| | Width x Height | Bytes |
|---|---|---|
| Original (the asset's own URL) | 1376 x 768 | 89,864 |
| `SPrCad` selector `1` ("2K") | **2752 x 1536** | 315,871 |

2752 = 1376 x 2 exactly.

The selector is 1-based over the *upscaled* options, not an index over the menu:

| Selector | Menu item | Result |
|---|---|---|
| `1` | 2K \| Upscaled | works — 2x the original |
| `2` | 4K \| Upscaled | `PUBLIC_ERROR_MODEL_ACCESS_DENIED` — 4K is gated, this account has no entitlement |
| `0` | never sent by the app | returns the same 2752x1536 image as `1` |

Only 2K has been produced end to end. 4K is unverified beyond confirming it is
refused for want of entitlement.

`POST /v1/images/upscale` implements this. See below for why it runs in the page.

#### Both upscales need the source's *content* id, and rows are not interchangeable

A caller holds a media id — it is what a generation returns and what the editor
URL carries — while `SPrCad` takes a **content id**. They are different values, so
the upscale endpoints resolve the content id from the project listing when only a
media id is given. (`as29s` takes the content id too — it was the *listing parser*
that had the two swapped, not the RPC table. See "Which field is the content id
depends on the listing shape" above.)

That resolution has a trap. One media id can have **several rows**: the original
and each derived asset share it. They are not interchangeable, and picking the
wrong one fails silently:

| Row for media `8e637e6c…` | Type code | `SPrCad` |
|---|---|---|
| `036d5a66-…` | `CAE` | works — returns the image |
| `003fe030-…` | `CAI` | **no image data at all** |

So the original is selected explicitly by its type code, `CAE`, rather than by
taking the first row that matches. The meaning of the codes is inferred from
behaviour, not documented: for the same media id, `SPrCad` answers the `CAE` row
and returns nothing for the `CAI` one.

### The image upscale runs in the browser, deliberately

The Go transport **cannot** make this call. Sending the identical request from Go
is rejected with `PUBLIC_ERROR_UNUSUAL_ACTIVITY` while the same captcha token and
payload succeed from the page. That was established by elimination — each of the
following was varied in turn and made no difference:

- the `bl` build label (absent, present, and the browser's own value)
- the `f.sid` session id
- the `at` anti-CSRF token, including the page's own `SNlM0e`
- the `source-path` shape
- the header set, including a byte-for-byte replica of **every** header the
  browser sends (`accept-encoding`, `priority`, `sec-ch-ua-*`, `x-browser-*`,
  `x-client-data`), and separately with `Authorization` removed
- the cookie jar (the same jar the browser produced)
- HTTP/2 and HTTP/3

The captcha token is not the problem: a token minted by this engine's own provider
was replayed from the page and returned the image.

The **TLS fingerprint** was the obvious next suspect and it is **not** the cause
either — six profiles including a Firefox one, HTTP/3, and the browser's exact
header order all produce the identical rejection. The full table is under "What is
not done yet" below. Tellingly, the generation RPC over the same Go transport
*does* succeed, so `SPrCad` applies a stricter client check than generation does.

So the request is made by the page, which is already required for the captcha
broker. This is a real constraint, not a shortcut: **image upscaling needs the
browser attached.** The Go-side implementation is kept in
`internal/batchexecute/client.go` (`UpscaleImage`) and is correct as far as the
protocol goes, but it will be rejected until the transport can match the browser's
fingerprint.

### What is not done yet

- **4K, on both kinds of asset.** The code path is wired (`Resolution4K`,
  `UpscaleModel4K`) but cannot be exercised on this account: an image 4K upscale is
  refused with `PUBLIC_ERROR_MODEL_ACCESS_DENIED`, and the video download menu does
  not offer 4K at all. 4K is gated on a higher plan, so this is entitlement rather
  than a missing RPC.
- **The image upscale needs the browser attached.** `SPrCad` is rejected over the
  Go transport while the identical call succeeds from the page. This is the only
  place the Go transport is not sufficient, and it is a property of the RPC, not a
  gap in the implementation: `p0UkFb` over the same transport works.

  The obvious theory — that the transport's fingerprint is simply too old — was
  tested and does **not** hold. Do not spend time advancing the TLS profile:

  | Varied | Result |
  |---|---|
  | TLS profile: `chrome_152`, `chrome_144`, `chrome_133`, `chrome_120` | `UNUSUAL_ACTIVITY` |
  | TLS profile: `firefox_135`, `firefox_120` — a *completely different* fingerprint | `UNUSUAL_ACTIVITY`, identical |
  | HTTP/3 via `WithProtocolRacing` (the library's own h3, not Go's stdlib) | `UNUSUAL_ACTIVITY` |
  | The browser's **exact** header order, plus its complete header set | `UNUSUAL_ACTIVITY` |

  A rejection that does not move when the fingerprint changes by that much is not a
  fingerprint check. Note also that `tls-client` ships no Chrome 153 profile —
  `chrome_152` is its newest — but adjacent Chrome releases have effectively
  identical ClientHellos, so there would be nothing to gain even if it did.

  What remains is something in the transport this code does not control: the
  frame-level details of a real Chrome connection, or a check that ties the
  captcha assessment to the connection that minted it — which a different process
  cannot satisfy by construction. `httpx.WithProfile` and
  `httpx.WithProtocolRacing` are kept because they are how this was established,
  and they are useful for any future fingerprint question.
- **Aspect ratio and resolution.** The app's composer exposes them (16:9 / 9:16,
  360p / 720p, and an Image/Video and Frames/Ingredients toggle), so the UI
  mapping is known — the payload positions are not. Until they are, the endpoints
  **refuse** the fields rather than dropping them; see below.
- **First-frame-only and reference-to-video.** First+last-frame and first-only both
  work now; reference-to-video and edit do not, and the model catalog names them all:

  | Composer mode | Model keys | RPC | Status |
  |---|---|---|---|
  | Frames, first + last | `omni_flash_i2v_*_first_last*`, `veo_3_1_i2v_*_fl` | `nprQif` | **works** |
  | Frames, first only | `abra_i2v_*`, `veo_3_1_i2v_*` (no `_fl`) | `eb1hJf` | **works** |
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

`HTrJv` returns **155 model keys**, and the naming carries the capability:

```
abra_t2v_4s / _6s / _8s / _10s          text to video, by duration
abra_t2v_8s_360p                        ... the _360p suffix is resolution
veo_3_1_t2v_fast_portrait               ... _portrait IS the aspect ratio
abra_i2v_*                              image to video, first frame only
omni_flash_i2v_8s_first_last_360p       image to video, first + last
veo_3_1_i2v_s_fast_4s_fl                ... _fl is the same thing
abra_r2v_* / veo_3_1_r2v_*              reference images
abra_edit / abra_edit_360p              video edit
veo_3_1_upsampler_1080p / _4k           upscalers
veo_3_1_extend_* / _interpolation_*     extend and interpolate
```

**Resolution is the `_360p` suffix**, which is why the upscalers are separate keys
rather than a parameter.

For the `veo_*` keys, **aspect ratio is a model key too** — `_portrait` against a
landscape default. The `abra_*` and `omni_flash_*` keys have no `_portrait` variants.

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

| Capability | `omni_flash` | `abra` | `veo` |
|---|---|---|---|
| Text to video | — | `abra_t2v_*` | `veo_3_1_t2v_*` |
| Image to video, first frame only | — | `abra_i2v_*` | `veo_3_1_i2v_*` |
| Image to video, first + last | **`omni_flash_i2v_*_first_last*`** | — | `veo_3_1_i2v_*_fl` |
| Reference images | — | `abra_r2v_*` | `veo_3_1_r2v_*` |
| Video edit | — | `abra_edit` | — |
| Upscaler | `omni_upsampler_360p` | — | `veo_3_1_upsampler_*` |

**Nothing in this implementation uses a `veo` model.** Text-to-video is
`abra_t2v_*` and image-to-video is `omni_flash_i2v_*_first_last_*`; the `veo_3_1_*`
keys are listed here only so it is clear they are a separate family and not needed.

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
| Cost | — | 4s=7, 6s=10, 8s=12, 10s=15 credits; 1080p upscale free, 4K=50 |

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

Cookies, a signed-in Flow editor tab for the reCAPTCHA broker, and the one
in-page call `SPrCad` needs. Generation itself never travels through it. See
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
│   ├── pool/                   worker pool (images, uploads, upscales)
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
