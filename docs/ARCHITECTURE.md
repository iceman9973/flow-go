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
   Chrome           │  browser-Cdp extension               │
   (signed in)      │  · attach to an allowed tab          │
                    │  · read cookies in a scoped domain   │
                    │  · run CDP commands on request       │
                    └───────────────┬──────────────────────┘
                                    │ WebSocket, extension dials out
                                    ▼
   internal/cdp        ── protocol multiplexer (hosts the socket)
   internal/bridge     ── allowlists, cookie sync, session refresh
                                    │
                                    ▼
   internal/cookiejar  ── cookie model, domain scoping, stable hashing
   internal/auth       ── Labs session endpoint → access token, cached by jar hash
   internal/httpx      ── Chrome-impersonating transport (uTLS + HTTP/3 → HTTP/2)
   internal/recaptcha  ── broker → http → empty
   internal/flowapi    ── Flow endpoints, error classification, retry
   internal/pool       ── account routing, failover, circuit breaking
   internal/store      ── SQLite WAL
   internal/engine     ── orchestration
   internal/server     ── HTTP API
```

Each layer depends only downward. `engine` is the only place that combines them.

## Data flow for one video

1. `POST /v1/videos/generations` → `engine.SubmitVideo` creates a job row and
   returns a `job_id` immediately.
2. A detached goroutine calls `engine.GenerateVideo`.
3. Local image inputs are uploaded first (`flowapi.UploadImage`) so a slow upload
   does not hold a pool slot.
4. `pool.Execute` acquires a worker (free-tier-first, least-busy, affordable,
   not circuit-broken) and calls the appropriate generator.
5. `flowapi.Client.call` mints or reuses a token, injects `clientContext` with
   the resolved project, tier and captcha token, and issues the request.
6. On an auth failure it drops the token, re-mints once, and retries. On any
   other retryable failure `pool.Execute` fails over to another account.
7. Polling happens **outside** the pool — it is read-only and can take minutes,
   and holding a generation slot for it would stall the pool.
8. If a resolution above 720p was requested, a second upsampler pass runs and the
   upsampled media IDs replace the originals.
9. Media is streamed to disk with cookies attached to every redirect hop.
10. The job, media, and per-account outcome are recorded.

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
credits have simply not been polled yet would strand capacity.

A worker is parked for a cooldown after `FailureThreshold` consecutive failures.
A success resets the streak. `Execute` excludes already-attempted workers, so a
failing job cannot loop on the same account, and it stops as soon as every
worker has been tried rather than waiting out the acquisition deadline.

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
| 5 | The worker pool was dead code — `register_worker` / `acquire_worker` / `execute_with_failover` had no production callers | Every generation goes through `pool.Execute` | `TestExecuteFailover`, `TestFreeTierIsSpentFirst` |
| 6 | `/stats` reported 1600 credits of test fixtures as live analytics | Statistics aggregate real rows only; credits are `NULL` until observed | `TestEmptyDatabaseReportsZeros`, `TestCreditsAreNotInvented` |
| 7 | `direct_client.py` was never wired in, so the "reduced browser dependency" claim was false | The browser is out of the generation path entirely | verified live: token minted without a browser |

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

## Open item

The cookie → Labs-session step works and is verified live. The Labs-session →
`aisandbox` step does not, because Labs marks the session
`ACCESS_TOKEN_REFRESH_NEEDED` and issues a Labs-scoped token that `aisandbox`
rejects with `401`.

Recovery options, in order of effort:

1. **Browser session refresh** — `POST /v1/bridge/refresh`. No code change; the
   site renews its own session. Needs the extension reloaded so the improved
   tab-wait logic is active.
2. **NextAuth token exchange** — derive a fresh
   `__Secure-next-auth.session-token` from the Google cookies, as
   `flow2api`'s `session_login.py` does. This is the substantive remaining work
   and would remove the browser from the auth path entirely.

Until one is done and a real generation has been observed to complete, the
generation endpoints should be treated as unverified.
