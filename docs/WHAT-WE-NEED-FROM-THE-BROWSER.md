# What we need from the browser

Short answer: **three things**, and only the third one is still a hard dependency.

## 1. Cookies — for `labs.google` and `google.com`

That is the whole credential. Specifically **`SAPISID`**, which becomes the
`Authorization: SAPISIDHASH …` header:

```
sha1("<unix-seconds> <SAPISID> https://flow.google.com")
```

The browser hands these over once and they are cached to
`cookies/cookies.json`, so the engine keeps running after Chrome closes.

**No API key. No bearer token.** Both of those turned out to be dead ends — see
"what we no longer need" below.

## 2. The project ID

Read off the tab URL: `https://flow.google.com/project/<project-id>`.

The engine can also discover it — it scrapes the project list from the landing
page — but the tab URL is the cheap way.

## 3. A live Flow editor tab — the one thing still unavoidable

Needed for two jobs, and this is the only reason the browser is still in the
picture:

- **The reCAPTCHA broker** runs the page's own
  `grecaptcha.enterprise.execute()` in a signed-in tab, which scores far better
  than any token we can mint ourselves.
- **The `0cAFcWeA…` session-context blob.** This is the last unknown. It sits at
  `[1][0][7][10][0]` of the generation payload, is about 1000 characters, and
  exists **only in the page's memory** — it is not in any RPC response, in
  `localStorage`, in `sessionStorage`, in a cookie, or on any `window` global.
  Without it, a replayed generation returns `401`.

Once that blob can be obtained or reproduced outside the page, requirement 3
disappears and the browser drops to "cookies only", which was the original goal.

## What we no longer need

| Thing | Why it is gone |
| --- | --- |
| The API key `AIzaSyBtrm0o5ab1c-…` | Referrer-restricted to `labs.google`; the app moved to `flow.google.com` and this key blocks it. The app uses no key at all. |
| A `ya29.` bearer token | The app authenticates with cookies plus `SAPISIDHASH`. The whole token-minting path (`internal/auth`, the NextAuth + OAuth handshake) is now unnecessary for generation. |
| The `aisandbox-pa.googleapis.com` REST surface | Legacy. The app talks to `flow.google.com/_/AiSandboxAngularFrontend/data/batchexecute`. |

## Checklist to run it

1. Chrome, signed in to the Google account that has Flow access.
2. **The Flow Go Bridge extension loaded** — `flow-go/flow-go-extension/`. That is the
   narrow one and the one this backend is built against: Flow hosts only, no
   `debugger` permission, and a fixed list of operations. The generic
   `../browser-Cdp/extension/` also works and is what you load when you need
   `cdp.evaluate` for debugging, but it is a separate project and flow-go does not
   require it. Both dial `ws://127.0.0.1:9222`.
3. A Flow project tab open — the engine will open one if it is not.
4. `flow-go serve`.

That is it. The extension reads cookies, opens/attaches to the editor tab, and
does nothing else.

> **Status update (2026-09-19).** Requirement 3 is now narrower than this document
> originally claimed. Video generation completes end to end without the
> session-context blob — a 4s 720p render reaches `status: ready`, resolves its
> signed URL and downloads. The blob at `[1][0][7][10][0]` belongs to the
> **image** RPC (`RPCIDGenerate`), not to the video RPCs, so if it is still
> required it is required there. The live tab remains needed for the reCAPTCHA
> broker regardless.
