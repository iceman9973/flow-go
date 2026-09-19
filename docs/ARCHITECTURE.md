# Architecture

## The problem being solved

`flow-agent` (Python) worked, but it was structurally dependent on a browser:

- Every generation call went through a Chrome extension. `flow_engine/bridge.py`
  `api_request()` sent a message over a WebSocket and waited for the extension to
  perform the HTTP call.
- The access token was **captured**, not minted. The extension observed a live
  `Authorization: Bearer ya29.` request on `labs.google` and forwarded the header
  to the backend.
- `direct_client.py` — written specifically to remove the browser from the path —
  was never imported outside its own test.

Consequence: if the token went stale, the only recovery was manual browser
interaction, and the system could deadlock permanently.

This port inverts that. The browser supplies cookies; Go derives everything else.

## Layers

```
                    ┌──────────────────────────────────────┐
   Chrome           │  browser-Cdp extension               │  generic: any site,
   (signed in)      │  · attach to an allowed tab          │  chrome.debugger
                    │  · read cookies in a scoped domain   │
                    │  · run CDP commands on request       │
                    ├──────────────────────────────────────┤
                    │  Flow Go Bridge                      │  narrow: Flow only,
                    │  · the same, minus cdp.evaluate      │  no debugger
                    │  · flow.at / flow.captcha / flow.upscale
                    └───────────────┬──────────────────────┘
                                    │ WebSocket, extension dials out
                                    │ both extensions hardcode ws://127.0.0.1:9222,
                                    │ so exactly one backend can own the port
                                    ▼
   cdp-control/bridge  ── protocol multiplexer (hosts the socket), token pairing,
   cdp-control/cdp        allowlists, cookie sync, session refresh, tab control
   cdp-control/cookiejar  cookie model, domain scoping, stable hashing
   (external module, imported via a local `replace`)
                                    │
                                    ▼
   internal/auth       ── Labs session endpoint → access token, cached by jar hash
   internal/httpx      ── Chrome-impersonating transport (uTLS + HTTP/3 → HTTP/2)
   internal/recaptcha  ── flow.captcha → broker → http → empty
   internal/batchexecute ── the RPC transport the app actually uses
   internal/flowapi    ── legacy aisandbox REST, error classification, retry
   internal/pool       ── account routing, failover, circuit breaking
   internal/store      ── SQLite WAL
   internal/engine     ── orchestration
   internal/server     ── HTTP API
```

Each layer depends only downward. `engine` is the only place that combines them.

The three `cdp-control/*` packages are **not** part of this module. They live in the
sibling `browser-Cdp` project and are pulled in by a relative `replace` in `go.mod`,
which means flow-go cannot be built without that checkout beside it. The two
projects also ship separate extensions; see `README.md` for which is which.

## Data flow for one video

The video path is **batchexecute, and synchronous** — `GenerateVideoViaBatch` does
the whole thing inside the request. The legacy `flowapi` path (`SubmitVideo` +
detached goroutine + `pool.Execute`) is still in the tree but is not what
`POST /v1/videos/generations` calls, because the aisandbox REST surface it needs
is dead: its key is referrer-restricted to `labs.google` and refuses the app's own
origin.

1. `POST /v1/videos/generations` → `engine.GenerateVideoViaBatch`.
2. Model and quality are resolved. The **model key is the authority on quality**:
   `abra_t2v_4s_360p` is a 360p render whatever the `quality` field says, and
   pricing it off the field would quote the 720p cost for a 360p render.
3. A job row is written as `submitted`.
4. The cookie jar and a batchexecute client are built for the current `authuser`.
5. **Affordability gate.** `planVideo` reads the balance and compares it with
   `VideoCosts[duration][quality] × count`. If it does not cover the render, the
   other signed-in accounts are scanned and the engine moves to the one with the
   most credits that can pay, then re-plans. Still short → **402** and nothing is
   sent. An unaffordable request is **refused**, not quietly rendered at a cheaper
   quality: a cheaper render is a different render than the one asked for, and for
   `abra_t2v_4s_360p` it is no render at all.
6. `conditionImageID` resolves any start/end image to its **content id** — the
   conditioning RPCs take the content id, not the media id.
7. A reCAPTCHA token is minted. This is deliberately **after** the gate: a token is
   a browser round trip, and minting one for a render the balance cannot cover
   wastes it and buries the real reason behind a captcha failure.
8. `client.GenerateVideo` picks its RPC id from the conditioning — `YhhmEf` for
   text-to-video, `nprQif` / `eb1hJf` for image conditioning, `MZZa6b` for
   references. A correct payload sent to the wrong id is **accepted and returns
   nothing**.
9. Media ids are parsed out of the response frames.
10. The balance is read again. `credits_spent` is recorded as the **delta**, not
    the table's figure — only the balance moving proves the render was charged.
11. If `wait` / `download` was asked for, `collectVideo` polls `ResolveVideoURL`
    until the signed URL appears.
12. The file is streamed to `output/` with cookies attached to every redirect hop.

Polling does **not** go through the pool: it is read-only and can take minutes, and
holding a generation slot for it would stall the pool.

### The two ids, and why a wrong one is invisible

An asset carries several uuids and they are not interchangeable. Getting one wrong
is never rejected — the RPC answers `200` with a `null` payload, which is
indistinguishable from a render that has not finished:

| RPC | Takes |
| --- | --- |
| `as29s` (media detail) | the **content id** |
| `SPrCad` (image upscale), `nprQif` / `eb1hJf` conditioning | the **content id** |
| `/project/<id>/edit/<X>` | the media id |
| `p0UkFb` (video upscale) | **both** |

**Which field holds the content id depends on the listing shape**, and this is
where it went wrong:

| shape | media id | content id |
| --- | --- | --- |
| flat — `[content-id, project-id, media-id, type-code, …]` | `row[2]` | `row[0]` |
| nested — `[media-id, null, null, [title, ts, …, content-id, ?, ts], project]` | `row[0]` | `detail[4]` |

The nested pair was parsed **backwards** — `detail[4]` was labelled the media id and
`detail[5]` the content id. The upload response is what settles it: it reports
`media_id` and `content_id` separately, and the content id is the one that lands at
`detail[4]`.

That single swap produced two failures that both looked like something else:

1. **Every finished render looked like one that never finished.** `ResolveVideoURL`
   handed `as29s` the wrong uuid; it answered `null`; the poll read that as "not
   ready" and waited out its full timeout while the video was downloadable the whole
   time. `README.md` carried the same wrong claim in its id table, which is how the
   code came to be written that way.
2. **Every uploaded image was invisible.** The row matcher required both `detail[4]`
   and `detail[5]`, and an upload has no `detail[5]` — it has no derived variant —
   so the row was dropped entirely and image-to-video from an upload reported "not
   in the project listing" for an asset that was in the listing.


## Token lifecycle

```
cookies ──hash──▶ cache key
   │
   └─▶ GET https://labs.google/fx/api/auth/session
            │
            └─▶ { access_token, expires, error, user }
                     │
                     ├─ cached with a local TTL (default 2400s)
                     ├─ invalidated when the cookie hash changes
                     └─ dropped on any unauthenticated response
```

Two subtleties, both learned from the live endpoint:

**The advertised `expires` is not authoritative.** Labs returns an `expires` that
tracks the *NextAuth session*, not the bearer token, and it is routinely already
in the past while a working token is still issued. Trusting it would mark every
freshly minted session as expired and re-mint on every request. The parser only
stores an expiry that is genuinely in the future; otherwise the local TTL is the
only bound.

**`error` is informational, not fatal.** The payload includes
`"error": "ACCESS_TOKEN_REFRESH_NEEDED"` alongside a usable token. It is
surfaced in the log rather than swallowed, because it is the early warning that
the cookies are aging out.

## Error classification

The single most important function in the codebase is
`flowapi.APIError.Unauthenticated()`. It recognises every shape an expired
credential can take:

| Response | Counts as auth failure? |
| --- | --- |
| `401` | yes |
| `407` | yes |
| `403` + `UNAUTHENTICATED` / `AUTH_ERROR` / "invalid credentials" | yes |
| `503` + `NO_FLOW_KEY` / `NO_TOKEN` / `UNAUTHENTICATED` | **yes** |
| `503` + "Service temporarily unavailable" | no — genuine outage |
| `403` + `QUOTA_EXCEEDED` | no — not fixable by a new token |

The `503 NO_FLOW_KEY` row is the one the Python version missed, and missing it is
what deadlocked the system: the extension self-heals a `401` by clearing its own
token, after which every call returns `503`, which the old check read as a
transient outage — so the force-refresh path never ran.

## Worker pool

Selection order for a job costing `cost` credits:

1. Free-tier workers that can afford it, least busy first, round-robin tiebreak.
   Free credits renew daily, so they are spent before paid ones.
2. Paid workers that can afford it, least busy first.
3. Anything idle, as a last resort.

An **unknown** balance counts as affordable. Refusing to schedule a worker whose
credits have simply not been polled yet would strand capacity. That is defensible
for a pooled path and wrong for one with a hard, known cost — which is why the
video path does not rely on it.

A worker is parked for a cooldown after `FailureThreshold` consecutive failures.
A success resets the streak. `Execute` excludes already-attempted workers, so a
failing job cannot loop on the same account, and it stops as soon as every
worker has been tried rather than waiting out the acquisition deadline.

**Which paths actually use it.** `pool.Execute` is called by `upsample`,
`UploadImage`, `GenerateImage`, and the legacy `GenerateVideo`. It is **not** called
by `GenerateVideoViaBatch` — the one video path the HTTP API uses — because the
pool's `Worker` carries a `flowapi.Client`, which the batchexecute path has no use
for. Account selection for video is done by `switchToAffordableAccount` in the
engine instead. That is a deliberate choice and it does leave two selection
mechanisms in the tree; the honest summary is that the pool routes images and
upscales, and the engine routes video.

Two consequences worth knowing:

- **Image generation is free**, which is why `GenerateImage` and `UploadImage` pass
  `cost = 0` — that is deliberate, not a missing gate. Verified live: an image
  render leaves the balance unchanged. The same `cost <= 0` shortcut is what makes
  `Affordable` return true unconditionally on those paths, which is the correct
  answer for a free operation. Only video has a pre-flight gate, because only video
  costs anything.
- `Worker.Affordable` treats an **unknown** balance as affordable, so a pooled path
  will accept an account whose balance has simply not been read. For video that no
  longer matters — the gate reads the balance itself — but it does mean the pool's
  own affordability check is advisory rather than binding.

`Bootstrap` calls `pool.Retain(accountID)` before registering, so a switch does not
leave the previous account's worker behind. Without it the pool accumulates
accounts the engine can no longer route to, and `/status` sums a balance that is no
longer available.

## Persistence

SQLite with WAL, `modernc.org/sqlite` (pure Go, no cgo), `MaxOpenConns(1)` so
there is a single writer and no `SQLITE_BUSY`.

Two rules, both written because the Python version broke them:

- **No seeding.** Nothing writes rows except the running engine. Tests open a
  temporary file per test.
- **No invented numbers.** `Stats()` aggregates what is present. An empty
  database reports zero rather than a plausible total.

One driver detail worth recording: an aggregate expression such as
`MIN(created_at)` carries no declared column type, so the driver returns it as
text even though the column is `DATETIME`. Reading it directly into `time.Time`
fails. `parseSQLiteTime` handles both shapes.

## Defects carried over from the Python engine

Each of these was reproduced from `VERIFICATION-REPORT.md` and is fixed here,
with a regression test where the fix is behavioural.

| # | Defect | Fix | Test |
| --- | --- | --- | --- |
| 1 | `_is_unauthenticated` only matched `401`, so `503 NO_FLOW_KEY` never triggered the force-refresh path and the pipeline deadlocked | `APIError.Unauthenticated()` covers 401/407/403/503 shapes | `TestUnauthenticatedCoversNoFlowKey` |
| 2 | The stale key cache was never cleared, so `/health` advertised a usable credential the extension did not have | Token cache is keyed on the cookie jar hash and invalidated on any auth failure | `TestSessionJarSwapInvalidates` |
| 3 | `has_flow_key()` reported a server-side cache, not the actual state | `bridge.Status().HasCredentials` is derived from the live cookie jar | `TestHasAuthCookies` |
| 4 | `tests/test_worker_pool.py` had no isolation and wrote fixtures into the production database | Tests open a private temporary database; `Open` requires an explicit path | `TestStoresAreIsolated` |
| 5 | The worker pool was dead code — `register_worker` / `acquire_worker` / `execute_with_failover` had no production callers | The pool is wired for images, uploads and upscales. The video path does not use it (see Worker pool above), which is a deliberate exception rather than an oversight | `TestExecuteFailover`, `TestFreeTierIsSpentFirst` |
| 6 | `/stats` reported 1600 credits of test fixtures as live analytics | Statistics aggregate real rows only; credits are `NULL` until observed | `TestEmptyDatabaseReportsZeros`, `TestCreditsAreNotInvented` |
| 7 | `direct_client.py` was never wired in, so the "reduced browser dependency" claim was false | The browser is out of the generation path entirely | verified live: token minted without a browser |
| 8 | The nested listing's id pair was parsed backwards, so `as29s` was handed the wrong uuid. A wrong uuid is answered with a `null` payload and no error, so every finished render looked like one that never finished | `ParseProjectAssets` maps the nested shape as `row[0]` = media id, `detail[4]` = content id; `ResolveVideoURL` passes the content id | `TestParseProjectAssetsReadsTheNestedListingShape`; verified live: a render that had "never resolved" downloaded immediately |
| 9 | An uploaded image was dropped by the row matcher, which required both `detail[4]` and `detail[5]` — and an upload has no `detail[5]`, having no derived variant | `isAssetRow` requires only `detail[4]` for the nested shape | `TestParseProjectAssetsReadsAnUploadedImage` |
| 10 | `"no video URL … yet (still rendering?)"` was not wrapped as retryable, so the poll treated a still-rendering video as permanently failed and gave up after 10s | `ErrAssetNotReady` + `RetryableResolveError`, which covers both stages of a render | `TestRetryableResolveErrorCoversBothStagesOfARender` |
| 11 | Every signed-in account was labelled with the first one's email, because the address came from the authuser-blind session endpoint | The address comes from the account-scoped `o30O0e` profile RPC | `TestFindProfileReadsIdentityFromItsTrueShape` |
| 12 | The selected account index lived in memory only, so every restart reverted to index 0 — which is how a 1-credit account came to look like the only one | The index is written to the `settings` table and restored at construction | `TestSetSettingRoundTripsAndReplaces` |
| 13 | Nothing checked whether an account could afford a render. The server accepts an uncovered submission and answers with no media, so an empty wallet presented as a broken request | A pre-flight gate refuses with **402** and the arithmetic, and moves to another signed-in account when one can pay | `TestDecideVideoPlanRefusesWhenNothingFits`, `TestBestAffordableAccountPicksTheRichestThatCanPay` |
| 14 | `Bootstrap` registered a worker on every account switch and removed none, so the pool accumulated accounts the engine could no longer route to and summed their balances as available | `pool.Retain(accountID)` before registering | `TestRetainDropsThePreviousAccount` |
| 15 | The same gate **downgraded** to 360p when the balance would not cover 720p, on the reasoning that a cheaper render beats none. For `abra_t2v_4s_360p` it does not: that key accepts a submission, returns a media id and never produces an asset, so the downgrade turned a request that would have worked into seven minutes of polling and nothing, with no error to explain it | The downgrade is gone. An unaffordable request is refused immediately, and a caller who wants 360p asks for it | `TestDecideVideoPlanRefusesRatherThanDowngrading` |
| 16 | An id was resolved from the project listing exactly once. An upload returns its ids **before** the listing carries the row — measured at about eight seconds — so conditioning on an upload that had just succeeded reported "not in the project listing" for an asset that was there by the time anyone looked | `awaitAsset` retries while the asset is missing, for a 30s window, and is used by both `conditionImageID` and the two `findAsset` callers. A failed listing call is still returned at once — waiting does not fix it | `TestAwaitAssetRetriesUntilTheListingCatchesUp`, `TestAwaitAssetReturnsAnErrorImmediately` |

Defect 7 in the report was stated as "two of five phases produced dead code". Here
the equivalent claim is load-bearing and verified: the browser participates only
in cookie reading and, when needed, a session refresh.

## Bridge hardening

An audit of the extension/backend pair (`docs/CDP-ACCESS-AUDIT.md`) surfaced four
issues. Three are fixed; one is deliberately left alone.

| Issue | Resolution |
| --- | --- |
| The backend never pushed a CDP deny-list, so a caller could attach to the allowed tab and run `Network.getAllCookies` — the tab allowlist limits which tab you attach to, not which method you then run | `DefaultBlockedMethods` is pushed on every connect, and the applied scope is logged |
| The WebSocket had no authentication. `isExtensionOrigin` accepts a missing `Origin` header, and every non-browser local process sends none — so any program on the machine could drive the user's tabs | A 32-byte token is generated at `data/bridge-token` (0600) and required on the upgrade. Pairing is trust-on-first-use, and the tokenless window closes permanently once an extension proves it holds the token |
| The scope push was silent and permanent, surviving after flow-go disconnects | The applied scope is logged on every connect, and the READMEs state plainly that connecting flow-go re-scopes the extension and that it persists |
| `google.com` in the cookie scope is broader than the stated goal | Left as-is. The Labs session mint was verified to work with `labs.google` cookies alone, but the generation path never completed, so nothing justifies narrowing it. Changing a security-relevant scope on an unverified guess would be worse than the current documented uncertainty |

One detail worth recording: the first implementation of `authorize` returned
`(authenticated, ok)` where `ok` meant "a token was supplied" rather than "the
caller is authorized". A wrong token therefore returned `ok == true` and was
admitted. The test caught it. The function now returns
`(authenticated, authorized)` and every wrong-token case is covered.

## Verification status

**Video generation works end to end and is verified**, for both text-to-video and
image-to-video:

- 4s 720p text-to-video reaches `status: "ready"` in ~42s, resolves its signed URL
  and downloads a playable MP4 to `output/`. 7 credits.
- 4s **360p image-to-video** from an uploaded still does the same in ~194s for
  **4 credits** — the cheapest render the cost table offers. The conditioning image
  is uploaded over `maseQ` first and the engine resolves its content id from the
  listing.

In both cases `credits_spent` is recorded from the balance delta, not from the cost
table.

That matters for the "open item" this section used to carry, which said the
generation endpoints should be treated as unverified. The blocker it described —
Labs marking the session `ACCESS_TOKEN_REFRESH_NEEDED` and issuing a Labs-scoped
token that `aisandbox` rejects with `401` — is real, but it is a blocker for the
**legacy aisandbox REST path only**. The working path does not use it: batchexecute
is authenticated by `SAPISIDHASH` over the Google cookies, never asks for a Labs
bearer token, and so never touches that exchange.

What is still open, in order of effort:

1. **`SPrCad` (image upscale) cannot run over the Go transport.** It is rejected
   `PUBLIC_ERROR_UNUSUAL_ACTIVITY` regardless of TLS profile, so it runs in the
   page. This is a genuine, unexplained difference and the only operation that
   still needs the browser for more than credentials.
2. **`row.SKU` is authuser-blind, and there is no source that is not.** It is read
   from the same session endpoint that made every account's email look alike, so
   every row carries the default account's tier. Unlike the email this was not
   fixable by switching source — both alternatives were checked and neither works:
   the `nzlgx` credits payload is a flat array of numbers with no tier in it
   (`[[7,3,8,1,null,7]]`, measured per account), and the legacy aisandbox
   `/v1/credits` answers for a *different* signed-in account entirely. The field is
   kept and documented as not-per-account rather than removed, and the
   account-selection policy deliberately ignores free-tier preference because of it.
   Spending renewable credits before paid ones is the right policy; it simply cannot
   be applied when every account looks the same tier.
