# What we need from the browser

Short answer: **cookies. That is the whole list.**

Everything else this document used to ask for has either moved into the Go process
or turned out not to be needed at all. The history is at the bottom, because the
two things that were hardest to remove are also the two most likely to be
re-raised.

---

## 1. Cookies — for `google.com`, `accounts.google.com` and `flow.google.com`

That is the whole credential. Specifically **`SAPISID`**, which becomes the
`Authorization: SAPISIDHASH …` header:

```
sha1("<unix-seconds> <SAPISID> https://flow.google.com")
```

The browser hands these over once and they are cached to `data/cookies.json`, so
the engine keeps running after Chrome closes. `loadJar` prefers a live browser and
falls back to that file, which is what makes the fallback real rather than
aspirational.

**No API key. No bearer token.**

### How long the cached cookies last

Longer than the risk suggests, and shorter than the expiry dates say.

| Cookie | Expires |
| --- | --- |
| `__Secure-next-auth.session-token` (labs.google) | ~4 days |
| `ACCOUNT_CHOOSER` | ~3 months |
| `SAPISID`, `SID`, `__Secure-1PSID`, `__Secure-3PSID`, `OSID` | **~12–13 months** |

Those dates are not the limit. What actually ends a session is `__Secure-1PSIDTS`
rotating, Google invalidating it server-side, or a plain `401` — and **with no
browser attached a 401 is terminal**: `the session expired and no browser is
attached to refresh it`. The 4-day Labs token is not a limit either, because the
engine rebuilds it from the Google cookies on every boot.

So: **expect it to work until the first 401**, which has been many hours and on
paper is months. Re-sync once a week rather than trusting the dates.

Two read-only commands check it without spending credits, and both run with the
browser closed:

```bash
WS_PORT=9299 ./flow-go cookies     # want: has credentials: yes
WS_PORT=9299 ./flow-go projects    # 0 means the session is stale
```

## 2. Nothing else

The project id used to be read off the tab URL. It is now listed over the
transport (`UpteDb`) and can be created over it too — `flow-go projects --new`.
`at` and `f.sid` come from the page when one is attached and are persisted
otherwise. The browser identity is read once and persisted to
`data/fingerprint.json`.

## Checklist to run it

1. Chrome, signed in to the Google account that has Flow access.
2. **The Flow Go Bridge extension loaded** — `flow-go/flow-go-extension/`. That is
   the narrow one and the one this backend is built against: Flow hosts only, no
   `debugger` permission, and a fixed list of operations. The generic extension in
   the [Browser-cdp](https://github.com/kodelyx/Browser-cdp) repo also works and is
   what you load when you need `cdp.evaluate` for debugging, but it is a separate
   project and flow-go does not require it — clone that repo anywhere and load its
   `extension/` directory. Both dial `ws://127.0.0.1:9222`.
3. `flow-go serve`.

No Flow tab has to be open, and the extension can disconnect once the cookies have
synced. A second browser profile must not be left attached — see
`README.md`, "Two browser profiles must not both be connected".

---

## What we no longer need

| Thing | Why it is gone |
| --- | --- |
| The API key `AIzaSyBtrm0o5ab1c-…` | Referrer-restricted to `labs.google`; the app moved to `flow.google.com` and this key blocks it. The app uses no key at all. |
| A `ya29.` bearer token | The app authenticates with cookies plus `SAPISIDHASH`. The token-minting path (`internal/auth`, the NextAuth + OAuth handshake) is unnecessary for generation. |
| The `aisandbox-pa.googleapis.com` REST surface | Legacy. The app talks to `flow.google.com/_/AiSandboxAngularFrontend/data/batchexecute`. |
| **A live Flow editor tab** | See below. |
| **The project id from the tab URL** | Listed and created over the transport. |
| **Image upscaling** | Removed. It was the last call that had to run in the page. |

---

## The two that took the longest

Both were listed here as unavoidable. Both turned out to be solvable, and the
reason each looked impossible is worth keeping.

### The reCAPTCHA token, and the `0cAFcWeA…` blob

This document called the blob "the last unknown" — a ~1000-character value at
`[1][0][7][10][0]` of the payload that existed only in the page's memory.

**It is the reCAPTCHA token.** Nothing more. It is minted over the HTTP transport
with no browser at all, via the widget's own anchor/reload exchange, and the whole
account is in `docs/RECAPTCHA.md` — including the one-line bug that made it look
impossible for hours: the provider cached a token that is single-use.

### Image upscaling

`SPrCad` really did have to run in the page. The transport was rejected with
`PUBLIC_ERROR_UNUSUAL_ACTIVITY` even with the payload, URL, query parameters and
both ids matching a request captured from the app itself byte for byte, so it was a
client-level check and nothing the client could vary got past it.

The feature was removed rather than kept for the sake of one browser dependency.
The detail is in git history: `git log --all --oneline -- '*upscale*'`.
