# The server-side reCAPTCHA

Everything about minting a Flow reCAPTCHA token without a browser: how it works,
the one bug that made it look broken for hours, and a checklist for when a
generation comes back empty.

Read this before changing anything in `internal/recaptcha/`, and before
concluding that the transport "does not work".

---

## TL;DR

The token is minted over the HTTP transport, with no browser and no extension.
`auto` (the default) and `http` both mean this. It works.

```go
// internal/recaptcha/recaptcha.go
default: // "auto"
        return NewChain(NewHTTP(hc, "", "").WithUserAgent(userAgent), EmptyProvider{})
```

**A reCAPTCHA token is single-use.** It is verified once; every later call that
presents it is rejected. Anything that caches, retries or replays one will fail
in a way that looks like something else entirely.

---

## The failure signature to recognise

This is the shape of almost every problem in this area:

```
engine: nothing submitted for abra_t2v_4s_360p — it costs 4 credits at 360p and the
account has 31, which was checked and covers it; so the balance is not the cause.
The reCAPTCHA token came from "http". A token is single-use, so one presented twice
produces exactly this — Flow verifies it once and answers an empty frame on every
later call. That has happened two ways: a cache in the provider, and a retry that
resent a payload with the token already inside it. Both are fixed, so if this is a
token problem it is a new one of the same shape: something is handing out or
resending a token it has already used. ...
```

The response is:

```
HTTP 200
{"frames": [null], "status": "submitted"}
```

**`200`, an empty frame, no charge, no error.** Flow accepted the request and did
nothing. There is no status code and no message to branch on, which is exactly why
this is worth a document.

### What an empty frame means, and what it does not

| Symptom | Means |
| --- | --- |
| `frames: [null]`, `status: submitted`, no charge | the request was understood and declined, silently |
| `PUBLIC_ERROR_UNUSUAL_ACTIVITY` | a client check, on the RPC itself — `SPrCad` was the only one, and it has been removed |
| `401` | the session, not the captcha |
| an error from the provider | the mint failed; the chain will have fallen through to `empty` |

An empty frame is **never** the captcha being absent. If the chain fell all the way
to `EmptyProvider` there would be a log line saying so, and the call would fail
differently.

---

## How the token is minted

Three round trips, all to `https://www.google.com/recaptcha/enterprise`:

| Step | Request | Why |
| --- | --- | --- |
| 1 | `GET {base}.js?render=<siteKey>` | read the widget's release id out of the bundle |
| 2 | `GET {base}/anchor?ar=1&k=…&co=…&hl=en&v=<release>&size=invisible&…` | returns HTML carrying a one-time challenge token |
| 3 | `POST {base}/reload?k=<siteKey>` with `v, reason=q, c=<challenge>, k, co, hl, size, sa=<action>` | exchanges the challenge for the assessment token |

Notes that matter:

- **The release id cannot be hardcoded.** The widget rejects a stale version, so
  step 1 is mandatory. It is extracted with
  `/recaptcha/releases/([^/]+)/recaptcha__` and was confirmed to match what the
  page itself loads.
- **The action goes in `sa`, not `action`.** A token minted for the wrong action is
  rejected.
- `co` is the base64 of the embedding origin. It is **derived** from
  `recaptchaOrigin` so the two cannot drift apart; it was written out by hand once
  and went stale from `labs.google` to `flow.google.com` without anything noticing.
- **The user agent is a parameter, not a constant.** `WithUserAgent` sets it from
  the browser's own fingerprint when one is available. A captcha-bearing call is
  checked against the client its token was minted for, so a provider that declares
  one machine and hands the token to another is a mismatch. It was not what
  unblocked anything — see below — but it is right.
- Tokens are ~2300–2500 characters and start with `0cAFcWeA`. `minTokenLength` is
  500; anything shorter is a placeholder and submitting it wastes an attempt.

---

## The bug that made this look broken

`HTTPProvider.Token` cached its token for two minutes:

```go
if p.token != "" && time.Now().Before(p.expiry) {
        return p.token, nil      // <-- wrong
}
```

The reasoning was reasonable and is written in the comment: Enterprise tokens are
short-lived, and a batch should not pay for a round trip per item. The premise is
false — **a token is single-use** — so the effect was that in a long-lived process
the *first* generation worked and **every one after it inside the window silently
did nothing**.

### Why it survived so long

The command line hid it completely. A CLI run is a fresh process, so it minted a
new token every time and looked fine. The server — the thing this was built for —
did not.

That asymmetry sent the investigation in the wrong direction repeatedly: it looked
like a scoring problem, then like a user-agent problem, then like an account
problem, because the evidence available (the CLI) was the one place the bug could
not appear.

### What this does not explain

The failures at 08:46–08:48 were a **fresh process making one call**, so no cache
was involved, and they still came back as an empty frame. Between those runs and the
working ones the account, its cookies and its project all changed, and the old
account's project listing had gone empty more than once.

A generation against a project that is not on the account is accepted the same
silent way, which is the likeliest explanation — but it is **untested**, and it is
recorded here as a gap rather than folded into the story. The cache is a real bug
and removing it is what made the server-side path work; those standalone failures
have a separate cause that was never isolated.

### The fix

`Token` now mints on every call. There is no cache, and no fields left to add one
back by accident.

```go
func (p *HTTPProvider) Token(ctx context.Context, action string) (string, error) {
        return p.fetch(ctx, action)
}
```

### Proof

Two consecutive calls through `/v1/debug/batchexecute-generate`, which used to
return `[null]` for both:

```
call 1: captcha_len 2361 -> 392023ba-5f57-4cf0-…
call 2: captcha_len 2297 -> 64daa2f5-ecc4-49cd-…
```

And end to end, with `flow.captcha failed: no extension attached` in the log:

| | Result |
| --- | --- |
| image, server | `status: succeeded`, 25.7s |
| video, server | `status: **ready**`, 28.7s, 4 credits |
| image, standalone CLI, no server and no bridge | `status: succeeded`, 26.7s |

### The second single-use trap: the retry

The cache was not the only way to present a token twice. `post` has two paths that
send a request more than once, and **both resent the payload they were handed** —
which has the token baked into it:

- **the priming handshake.** The first attempt to a fresh process is answered `400`
  with an anti-CSRF token, and the second attempt echoes it. Two requests, one
  payload, one token.
- **the session refresh.** A `401` runs the installed refresher and repeats the
  priming sequence. Same shape, hit less often.

The first one is why **the first generation after every restart produced nothing**.
The seeded `at` is stale by the time the process boots, so the first call always
primed — and always spent its single-use token on the attempt that primed. It read
as flakiness because it was intermittent in the only way that mattered: it happened
once per boot, then stopped.

This is the same failure signature as the cache and it was fixed separately, so if
you are reading this because a generation came back empty, **check both**. A retry
that carries a token is the one that is easy to miss, because the retry is in the
transport and the token is in the payload.

`post` now takes a builder rather than a built envelope, and `call` has two forms:
`CallWith` for a payload that does not change, and the builder form for one that
does. Every attempt after the first asks `CallOptions.RefreshCaptcha` for a fresh
token and rebuilds the payload with it. A refresher that fails fails the call rather
than falling back to the spent token — a silent retry with a dead token is worse than
an error, because there is nothing to read.

```go
envelopeJSON, err := envelope(ctx, sent > 0)   // sent > 0 means "this is a retry"
```

The five captcha-carrying requests have `withCaptcha` setters, and each engine call
site routes its options through `Engine.captchaOptions(action, opts)`, which sets the
refresher and nothing else. **The action has to match** — `ActionVideo` for video,
`ActionImage` for upload and image generation — because a token minted for the wrong
action is rejected, the same silent way.

The helper exists so a sixth captcha-carrying call cannot forget the refresher. It
used to be five hand-copied closures; now there is one assignment in the package, and
a call site that hand-builds `batchexecute.CallOptions` stands out against its
neighbours. `internal/engine/captcha_options_test.go` pins it.

---

## Checklist: a generation came back empty

Work down this list. The first and third have been the answer and were confirmed;
the second is the likeliest explanation for a case that was never isolated, and is
marked as such.

1. **Was the token presented twice?** A cache, a retry, a batch, a stored token —
   anything that sends the same one twice. Two distinct mechanisms have caused this
   and both are fixed: `Token` caching for two minutes, and the transport resending
   a payload on a retry. Check that `Token` is called per submission **and** that a
   captcha-carrying call has `RefreshCaptcha` set.
   *This is the one that cost hours, twice.*
2. **Does the project exist on the account in use?** Generating into a project that
   is not there is accepted the same silent way. Confirm with `flow-go projects`,
   and remember the account is whichever the jar holds.
3. **Is more than one browser profile attached?** The bridge holds one cookie jar
   and `Current()` is whichever client connected last, so the account and the
   project move under a running process. `/health` reports `flow_extensions`; **two
   entries is the broken configuration.**
4. **Is the model key and RPC pair right for the quality asked for?** A video model
   sent through the image RPC is accepted and does nothing.
5. **Did the mint itself fail?** Look for `token acquired via …` in the log. No such
   line means the chain fell through to `empty`.

### Reading the log

```
engine: ready — account acct-…, 17 cookies (…), project …, captcha chain(http,empty)
recaptcha: token acquired via http (2361 chars)
engine: generated 1 image(s) in 25.7s
```

- `captcha chain(http,empty)` — server-side. **This is the default and it is what
  you want.**
- `captcha chain(flow.captcha,broker,http,empty)` — `--captcha broker`; the browser
  is being asked for the token.
- `token acquired via http (N chars)` — the transport minted it. A plausible `N` is
  roughly 2300–2500.
- `provider http failed: …` followed by another provider — the transport could not
  mint and something else answered. Read the error.
- `batchexecute: <rpc> primed; retrying` — the priming handshake fired and the retry
  went out. **This is normal on the first call after a boot.** When the call carries
  a captcha, the retry has rebuilt its payload with a freshly minted token — that is
  the fix, and this is the line that says the retry path was taken.
- `batchexecute: <rpc> returned 401; refreshing the session and retrying` — the
  session expired and the refresher ran. If this repeats on every call the cookies
  are not being replaced; reload the extension.

---

## Ruled out — do not re-test these

Each of these was tested and made no difference. They are recorded so the next
person does not spend the time.

| Suspect | Finding |
| --- | --- |
| Widget release version | provider extracts `zqB-6Xpbd3lCIvi7Tr2D0pob`; the page loads the same. Matches. |
| User agent | both the pinned Windows build and the browser's real macOS one produced the same result. Kept because it is correct, not because it fixed anything. |
| The `co` origin parameter | tested as `labs.google` and as `flow.google.com`; identical outcome. |
| The account | failed on both signed-in accounts. |
| The project | failed on a fresh project and on an established one. |
| The request builder | the same builder succeeds with a page token and failed with a transport one — which is what pointed at the token. |
| Token shape | page and transport tokens share the `0cAFcWeA` prefix and are the same length class. Nothing structural is missing. |

### A dead end worth recording

The page mints through `POST /recaptcha/enterprise/clr?k=<siteKey>` with a 1908-byte
protobuf body — field 1 is the site key, field 2 is 1864 bytes of opaque state.
**Two mints produce byte-identical bodies**, so replaying it looks obviously viable.

It is not. The response is empty, **including for the page itself**. The token is
assembled in JS by `enterprise.js`; no request returns it. There is no cookie to
carry and no request to replay — but none of that matters, because the
anchor/reload flow above is sufficient.

---

## Tests

`internal/recaptcha/token_test.go` runs the real anchor/reload exchange against an
`httptest.Server`. `recaptchaBase` is a `var` rather than a `const` for exactly
this reason — without it, exercising the flow means talking to Google.

The property pinned is not "a token comes back" but **"a different token comes back
on the second call"**. A cache passes the first assertion and fails the second.

| Test | Guards |
| --- | --- |
| `TestTokenMintsFreshOnEveryCall` | two calls, two tokens, and the server saw two reloads |
| `TestTokenKeepsMintingPastTheFirst` | three calls, no repeats — the failure mode was "everything after the first" |
| `TestTokenNeedsNoBrowser` | the default chain works with no cdp client anywhere |
| `TestAutoChainIsServerSideOnly` | no page provider creeps back into the default chain |
| `TestBrokerModeOptsIntoThePage` | the page path stays reachable, and in the order that works |
| `TestOriginParameterMatchesTheOrigin` | `co` stays derived from the origin |

`internal/batchexecute/captcha_retry_test.go` covers the other half — the retry that
used to resend a spent token. `Origin` is a `var` there for the same reason
`recaptchaBase` is: the priming handshake is driven against an `httptest.Server`
rather than against Google.

| Test | Guards |
| --- | --- |
| `TestARetryCarriesAFreshCaptchaToken` | the retry rebuilds the payload, and mints exactly once |
| `TestWithoutARefresherTheRetryResendsTheToken` | the old behaviour, pinned — a captcha-carrying call with no refresher still resends |
| `TestTheRefresherIsOfferedToEveryRetry` | a builder that ignores its token produces the same payload, so offering the refresher unconditionally is safe |
| `TestAFailingRefresherFailsTheCall` | a failed mint fails the call instead of retrying with a dead token, and the retry does not go out |

### Verify the tests have teeth

A test for a silent bug must be shown to fail. Both suites were checked this way;
do the same if you change either.

**The cache** — temporarily reintroduce one in `Token` and run them:

```
--- FAIL: TestTokenMintsFreshOnEveryCall
    both calls returned the same token — a reCAPTCHA token is single-use, ...
    the server saw 1 reloads, want 2 — the second call was served from something
    other than a fresh mint
--- FAIL: TestTokenKeepsMintingPastTheFirst
    call 2 repeated a token
```

**The retry** — change `envelope(ctx, sent > 0)` to `envelope(ctx, false)` in
`post`. Three of the four fail; the fourth passes either way by design, because it
pins the fallback:

```
2026/09/21 10:30:53 batchexecute: UpteDb primed; retrying
--- FAIL: TestARetryCarriesAFreshCaptchaToken
    the retry resent the payload it was given, captcha token and all. A reCAPTCHA
    token is single-use, so the server answers an empty frame rather than an error
    and the generation silently produces nothing
--- FAIL: TestTheRefresherIsOfferedToEveryRetry
    the refresher ran 0 times, want 1 — once, for the retry
--- FAIL: TestAFailingRefresherFailsTheCall
    a failing refresher should fail the call, not resend the spent token
```

If they pass with either bug restored, they are not testing anything.

---

## Modes

| `--captcha` | Chain | Browser |
| --- | --- | --- |
| `auto` (default) | `http → empty` | not asked |
| `http` | `http → empty` | not asked |
| `broker` | `flow.captcha → broker → http → empty` | **required** |
| `off` | `empty` | not asked |

`auto` used to lead with `flow.captcha` and the broker, on the belief that a
page-minted token scored better. It does, and it was also why every generation
needed an extension attached and a tab parked on a Flow project.

`ensureProjectTab` is tied to `broker` mode. It used to navigate a tab before every
generation whether or not the token needed a page.

---

## What still needs a browser

**Nothing.** Project list, project create, image generation, video generation,
poll, resolve and download all run with no browser attached.

Image upscaling was the last holdout and has been removed. It was the one RPC that
answered `PUBLIC_ERROR_UNUSUAL_ACTIVITY` over the transport while the identical
call succeeded from the page — a stricter client check on that RPC, not a
credential problem and not the fingerprint, since the request went out under the
browser's real identity, echoed back in the response, and was still rejected.
`use_quic` made no difference either.
