// Package batchexecute speaks Google's batchexecute RPC.
//
// This is the transport the current Flow app actually uses. Capturing the page's
// own network traffic showed exactly one backend call:
//
//	POST https://flow.google.com/_/AiSandboxAngularFrontend/data/batchexecute
//	     ?rpcids=WuwhI&source-path=%2Fproject%2F<project-id>
//
// authenticated with cookies and a SAPISIDHASH header — no API key, no bearer
// token. The aisandbox-pa.googleapis.com REST surface this project was originally
// ported against is the legacy one, and its key is referrer-restricted to
// labs.google, which is why generation could never succeed there.
//
// The same protocol is what free-gemini-api uses for Gemini
// (/_/BardChatUi/data/batchexecute), so this package is deliberately shaped like
// that client: build an f.req envelope, POST it as form data, then walk the
// wrb.fr response frames.
package batchexecute

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/kodelyx/cdp-control/cookiejar"
	"github.com/kodelyx/flow-go/internal/httpx"
)

// EndpointPath is the Flow app's batchexecute path.
const EndpointPath = "/_/AiSandboxAngularFrontend/data/batchexecute"

// Origin is the host the RPC is served from and bound to.
const Origin = "https://flow.google.com"

// RPCIDProjectList is the one RPC id observed in the app's own traffic. It is
// kept as a named constant because it is the only confirmed id, and it makes a
// useful smoke test for the transport.
const RPCIDProjectList = "WuwhI"

// RPC ids discovered by capturing the Flow app's own page-load traffic and
// replaying each one. Every entry below was verified to return HTTP 200 with real
// data against a live account.
//
// They are recorded because rediscovering them means re-capturing browser
// traffic, and because the response sizes are a useful signal: a few bytes means
// a flag, hundreds of kilobytes means a catalog.
const (
	// RPCIDProfile returns the signed-in user's profile.
	RPCIDProfile = "o30O0e"

	// RPCIDModelCatalog returns every generation model the account can use:
	// abra_i2v_{4,6,10}s, abra_edit, abra_edit_360p, and the image models
	// NARWHAL, HARBOR_SEAL, GEM_PIX_2, with their display labels.
	RPCIDModelCatalog = "HTrJv"

	// RPCIDAvailableModels returns the subset currently offered for a tool.
	RPCIDAvailableModels = "yBhWQ"

	// RPCIDGenerate submits a generation. This is the one the page only calls
	// when the user actually presses Generate, so it is absent from the page-load
	// traffic — it was captured by driving the UI over CDP and reading the
	// resulting request off the Network events.
	//
	// Its argument shape, decoded from a live capture:
	//
	//	[null,
	//	 [[null, null, null, <seed>, <count>, "<MODEL>", null,
	//	   [null, 22, null, null, null, "<project-id>", null, null, null, null,
	//	    ["<session-context-blob>", 1]],
	//	   [[[["<prompt>"]]]],              <- the prompt, in plaintext
	//	   null, null, null,
	//	   "<request-uuid>", "<request-uuid>"]],
	//	  1,
	//	  [<same context block again>],
	//	  ["<batch-uuid>"]]
	//
	// The model is an image enum ("NARWHAL") or a video key ("abra_t2v_8s").
	RPCIDGenerate = "ogiZ0b"

	// RPCIDGenerationStatus reports generation telemetry and progress. It carries
	// the MEDIA_GENERATION marker and the client environment flags.
	RPCIDGenerationStatus = "WuwhI"

	// RPCIDGenerateVideo submits a video generation.
	//
	// Structurally different from the image RPC, which is why sending a video
	// model through ogiZ0b is accepted and silently does nothing. Decoded from a
	// live capture of the app in Video mode:
	//
	//	[[[ [null, null, [[[prompt]]]], "<model>", 2, null,
	//	     [null, null, null, null, "<uuid>", "<uuid>"] ],   // one per variation
	//	  ...],
	//	 [null, 22, null, null, null, "<project-id>", null, null, null, null,
	//	  ["<recaptcha-token>", 1]],
	//	 ["<batch-uuid>", 2]]
	//
	// The context block at [1] is identical in shape to the image RPC's, including
	// the reCAPTCHA token.
	RPCIDGenerateVideo = "YhhmEf"

	// RPCIDUploadMedia uploads an image into the project and returns its media id.
	//
	// This is the only way to get a local file into Flow. The legacy REST surface
	// has an equivalent (`/v1/flow/uploadImage`) and it answers 200, but it writes
	// to a separate store: the media id it returns is invisible to batchexecute,
	// absent from the project listing and empty under as29s. Anything conditioned
	// on such an id cannot work.
	RPCIDUploadMedia = "maseQ"

	// RPCIDGenerateVideoImage submits a video generation conditioned on images.
	//
	// It is a *different* RPC from RPCIDGenerateVideo, not the same one with more
	// fields. Captured by driving the composer with a first and last frame set:
	// the submission went to nprQif while a text-only submission goes to YhhmEf.
	// Sending an image-conditioned payload to YhhmEf is accepted and returns an
	// empty result, which is why the layout matching byte-for-byte was not enough.
	RPCIDGenerateVideoImage = "nprQif"

	// RPCIDGenerateVideoImageStart submits an image-conditioned generation with a
	// single condition image.
	//
	// A third RPC again, and the one that made first-only i2v look impossible.
	// Captured by driving the composer with only the Start chip filled: the
	// submission went to eb1hJf carrying
	//
	//	[null, null, [[[prompt]]]], "abra_i2v_8s", 2, null,
	//	[null, "<media-id>", null, null, null, [null, null, 1, 1]],
	//	[null, null, null, null, "<uuid>", "<uuid>"]
	//
	// — the same outer layout nprQif uses, with a single image slot and a
	// different RPC id. Sending that exact payload to nprQif (or to YhhmEf)
	// is accepted and returns empty, which is why six earlier attempts at
	// first-only failed: the layout was right and the RPC was wrong.
	//
	// Verified: abra_i2v_8s and abra_i2v_8s_360p both return media ids here.
	//
	// It is also the reference-image RPC. The same call with an abra_r2v_* model
	// in place of an abra_i2v_* one returns media ids too, so the model key — not
	// the RPC or the payload — is what selects reference conditioning over a
	// start frame:
	//
	//	abra_r2v_8s @ eb1hJf      -> media id
	//	abra_r2v_8s @ YhhmEf      -> empty
	//	abra_r2v_8s @ nprQif      -> empty
	//
	// What is *not* established is whether the render is genuinely conditioned on
	// the image as a reference rather than as a start frame; that needs the output
	// compared against the input, and until then treat r2v as "accepted" rather
	// than "verified".
	RPCIDGenerateVideoImageStart = "eb1hJf"

	// RPCIDGenerateVideoReferences submits a generation conditioned on several
	// reference images.
	//
	// The last of the video submission RPCs to be captured, and the one that shows
	// why guessing at them is hopeless. `abra_r2v_*` sent to eb1hJf is *accepted
	// and returns a media id* — so a probe looks like a success — while the real
	// submission goes here. Captured from the app's own traffic:
	//
	//	[null, null, [[[prompt]]]],                 <- prompt at index 0
	//	[[null, "<id>"], [null, "<id>"], ...],      <- reference images at index 1
	//	"abra_r2v_4s", 2, null,
	//	[null, null, null, null, <uuid>, <uuid>],
	//	null, null, null, null,
	//	[["d351dd3c-0a12-1522-0000-000000000000"]]  <- fixed, see referenceTail
	//
	// Note the layout is *not* the i2v one: there the prompt sits at index 2 with
	// the images in trailing slots, here it sits at index 0 ahead of them, which is
	// the same arrangement the edit payload uses.
	RPCIDGenerateVideoReferences = "MZZa6b"

	// RPCIDVideoEdit extends or edits an existing video.
	//
	// Captured by accident while exploring the settings panel, and worth keeping:
	// it is the video-extension feature. Decoded:
	//
	//	[[[ [null, "<content-id>", 0, 96],
	//	    [null, null, [[[prompt]]]],
	//	    "abra_edit",
	//	    1,
	//	    [null, "<source-media-id>", null, null, "<uuid>"] ],
	//	  [null, 22, null, null, null, "<project-id>", null, null, null, null,
	//	   ["<recaptcha-token>", 1]]]
	//
	// Note the leading block: the asset's content id, then two numbers that are
	// presumably the trim window. Not wired into the API yet.
	RPCIDVideoEdit = "jIps6"

	// RPCIDUpscale renders a finished video at a higher resolution.
	//
	// Captured from the media viewer's download menu, which offers 270p (Animated
	// GIF), 720p (Original size), 1080p (Upscaled) and 4K (Upscaled, paid).
	// Decoded:
	//
	//	[[[ [null, "<operation-id>"], null, 1, null,
	//	     [null, "<source-media-id>", null, null, "<uuid>"],
	//	     null, 2, null × 27,
	//	     "<upsampler-model>" ]],
	//	 [null, 22, null, null, null, "<project-id>", null, null, null, null,
	//	  ["<recaptcha-token>", 1]]]
	//
	// The upsampler model key sits at index 34 of the 35-element request, after a
	// long run of nulls — which is why it is easy to miss. Keys come from the
	// model catalog: veo_3_1_upsampler_1080p and veo_3_1_upsampler_4k.
	//
	// Note this is a separate surface from the legacy REST upsampler the Python
	// engine called, which no longer works.
	RPCIDUpscale = "p0UkFb"

	// RPCIDOperation polls a long-running operation by id.
	//
	// Payload [null, null, [[<operation-id>]]]. Used to follow an upscale or an
	// edit to completion.
	RPCIDOperation = "jwpduf"

	// RPCIDImageUpscale resolves an image at a chosen resolution.
	//
	// This is the "instant" step in the download flow. The download menu offers
	// 1K (Original size), 2K (Upscaled) and 4K (Upscaled); picking anything other
	// than 1K runs this first, then the file is fetched. That is why clicking 2K
	// leaves no new project asset: nothing is created, the asset is simply served
	// at a larger size.
	//
	// Decoded:
	//
	//	["<content-id>", <resolution>, [null, 22, null, null, null, "<project-id>",
	//	 null, null, null, null, ["<recaptcha-token>", 1]]]
	//
	// Position [1] is the resolution selector; 1 was observed for the 2K choice.
	// Its exact scale is not certain, so it is passed through rather than mapped.
	RPCIDImageUpscale = "SPrCad"

	// RPCIDMediaDetail returns the signed URLs for one asset.
	//
	// Payload is the asset's **content id** — for the nested listing shape that is
	// `detail[4]` of the row, and for the flat one it is `row[0]`. Getting it
	// wrong is not rejected: the call answers 200 with a `null` payload and no
	// error, so a finished render reads exactly like one that is still going.
	//
	// The source-path must be "/project/<project-id>/edit/<media-id>", not the
	// plain project path, or the call is rejected. The response carries both the
	// poster (flow-content.google/image/<content-id>) and, for a video, the asset
	// itself (flow-content.google/video/<content-id>), each with its own signature.
	//
	// This is how a finished video is fetched. The generation call returns only an
	// id because video is asynchronous; the URL appears here once it is ready.
	RPCIDMediaDetail = "as29s"

	// RPCIDBatchOps accompanies a video submission and carries the operation ids
	// to poll. Payload: [null, null, [[<ids>], [<ids>]]].
	RPCIDBatchOps = "jwpduf"

	// RPCIDCredits returns the account's Flow credit balance.
	//
	// The response is a bare array like [412, 1, 2, 2, null, 412] — the balance
	// appears twice and the middle values are unknown flags. This is the real
	// balance; the legacy aisandbox /v1/credits endpoint reports something else
	// entirely, and for a different account, which is how a stale 50 was
	// mistaken for the truth.
	RPCIDCredits = "nzlxg"

	// RPCIDProjectContent returns the project's media and generation history,
	// including prompts and lh3.googleusercontent.com asset URLs. This is the
	// read side of the generation workflow.
	RPCIDProjectContent = "Zzl0ze"

	// RPCIDToolProject returns tool-scoped project metadata.
	RPCIDToolProject = "ngNC2"

	// RPCIDMediaCatalog returns a large project-scoped asset catalog.
	RPCIDMediaCatalog = "DTaVef"

	// RPCIDAssetPage pages through a project's assets.
	//
	// Decoded from a live call:
	//
	//	["projects/*", 21, "<page-token>", null, null, null, [1]]
	//
	// The third slot is a base64 page token and is null on the first page, so this
	// is the cursor the listing is walked with. `21` is likely a page size or a
	// resource type and was constant across the captures.
	RPCIDAssetPage = "UpteDb"

	// RPCIDProjectMeta returns project metadata.
	RPCIDProjectMeta = "mrlkwd"

	// RPCIDAssets returns a project-scoped asset listing.
	RPCIDAssets = "tRARke"

	// RPCIDFlags and friends return small feature-flag style responses.
	RPCIDFlags        = "NfrxTb"
	RPCIDCapabilities = "KV2T2d"
	RPCIDExperiments  = "LPzVkd"
	RPCIDMisc1        = "cPZSdc"
	RPCIDMisc2        = "Yizz8d"
	RPCIDMisc3        = "nzlxg"
	RPCIDMisc4        = "qJcgMc"
	RPCIDMisc5        = "ve2Lsc"
)

// KnownRPCs documents the discovered ids and their verified response sizes, for
// `flow-go batchexecute --list`.
var KnownRPCs = []struct {
	ID      string
	Name    string
	Payload string
}{
	{RPCIDProfile, "signed-in user profile", `[["me"],[[["person.photo","person.name","person.email"]],null,[1,7]]]`},
	{RPCIDGenerate, "SUBMIT A GENERATION (needs a session-context blob)", `<see RPCIDGenerate doc comment>`},
	{RPCIDGenerationStatus, "generation status / telemetry", `[[["MEDIA_GENERATION",["<uuid>",[<sec>,<ns>],null,<env flags>]]]]`},
	{RPCIDModelCatalog, "model catalog", `[]`},
	{RPCIDAvailableModels, "available models", `[]`},
	{RPCIDProjectContent, "project media + generation history", `["projects/<project-id>",null,null,null,[1]]`},
	{RPCIDToolProject, "tool-scoped project metadata", `["tools/PINHOLE/projects/<project-id>"]`},
	{RPCIDMediaCatalog, "project asset catalog", `[1]`},
	{RPCIDProjectMeta, "project metadata", `["<project-id>"]`},
	{RPCIDAssets, "project assets", `[]`},
	{RPCIDFlags, "feature flags", `[16]`},
	{RPCIDCapabilities, "capabilities", `[22]`},
	{RPCIDExperiments, "experiment config", `[[4,8,5,6,9]]`},
	{RPCIDMisc1, "misc query", `[]`},
	{RPCIDMisc2, "misc query", `[]`},
	{RPCIDMisc3, "misc query", `[]`},
	{RPCIDMisc4, "misc query", `[]`},
	{RPCIDMisc5, "misc query", `[]`},
}

// Fingerprint is the browser identity a request presents upstream.
//
// A captcha-bearing call is checked against the client the reCAPTCHA
// assessment was made for. The same token that succeeds under the browser's
// own user-agent and sec-ch-ua is rejected as PUBLIC_ERROR_UNUSUAL_ACTIVITY
// when the call goes out without them — an error that reads as a captcha
// failure but is really a fingerprint mismatch. Read-only calls are not
// checked, which is why this only bites the upscale and generation paths.
//
// Empty fields fall back to the generic Chrome profile for this host.
type Fingerprint struct {
	UserAgent string
	SecChUa   string
	Platform  string
	Mobile    string
	Language  string
}

// Client issues batchexecute calls with cookie authentication.
type Client struct {
	jar  *cookiejar.Jar
	hc   *httpx.Client
	reqs int64

	mu sync.Mutex
	// at is the anti-CSRF token the server hands out on a rejected first call.
	// It has to be echoed back in the form body on every subsequent call.
	at string
	// sessionID is the `f.sid` the app sends on every batchexecute request. It
	// comes from the page and is inherited by calls that do not name their own.
	sessionID string
	// fp is the identity to present. Zero value means "generic Chrome".
	fp Fingerprint
	// overrides are extra headers applied last. A header mapped to "" is
	// removed instead of set. Diagnostics only: it exists so a request can be
	// compared against the browser's own, header by header.
	overrides map[string]string
	// useQUIC allows the HTTP/3 path on these POSTs. The app's own batchexecute
	// calls go out over h3; the default transport keeps POSTs on HTTP/2.
	useQUIC bool
	// headerOrder overrides the header ordering. Diagnostics only: a header that
	// is absent from the order list is emitted in an arbitrary position, so a
	// faithful replay has to supply the order as well as the headers.
	headerOrder []string
	// onUnauthorized refreshes an expired session. Nil means a 401 is terminal.
	onUnauthorized UnauthorizedHandler
	// authUser selects which signed-in Google account these calls act as.
	//
	// Google tells its own endpoints apart per account with the `authuser` query
	// parameter, and with no parameter every call resolves to the *first*
	// signed-in account. So switching accounts in the browser changes nothing on
	// this transport unless the index travels with the request — which is why a
	// project belonging to one account and a session belonging to another is a
	// reachable state, and a broken one.
	authUser int
}

// SetAuthUser selects which signed-in Google account these calls act as.
//
// Zero is the default and needs no parameter, so it is emitted as nothing rather
// than as `authuser=0`.
func (c *Client) SetAuthUser(index int) {
	c.mu.Lock()
	c.authUser = index
	c.mu.Unlock()
}

// AuthUser returns the account index these calls act as.
func (c *Client) AuthUser() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authUser
}

// endpointURL builds the batchexecute URL for a query string, appending the
// account selection when it is not the default.
//
// Zero is omitted rather than sent: `authuser=0` and no parameter are not the
// same request to every Google endpoint, and every call before this existed sent
// nothing at all.
func (c *Client) endpointURL(rawQuery string) string {
	endpoint := Origin + EndpointPath + "?" + rawQuery
	if index := c.AuthUser(); index > 0 {
		endpoint += "&authuser=" + strconv.Itoa(index)
	}
	return endpoint
}

// SetHeaderOrder overrides the order headers are sent in.
//
// Diagnostics only. The default list omits headers the app sends (sec-fetch-*,
// x-same-domain), and an unlisted header lands wherever the map iterates to —
// which is not reproducible and not what the browser sends.
func (c *Client) SetHeaderOrder(order []string) {
	c.mu.Lock()
	c.headerOrder = order
	c.mu.Unlock()
}

// SetUseQUIC allows these calls to go out over HTTP/3.
//
// Only for calls that are safe to repeat: the QUIC path falls back to HTTP/2,
// which would submit a write twice.
func (c *Client) SetUseQUIC(enabled bool) {
	c.mu.Lock()
	c.useQUIC = enabled
	c.mu.Unlock()
}

// SetHeaderOverrides installs headers to apply after the built-in set. Mapping a
// header to "" removes it.
//
// Diagnostics only. Header differences are invisible in a rejected response —
// PUBLIC_ERROR_UNUSUAL_ACTIVITY says nothing about which header was wrong — so
// the only way to find the offending one is to vary them one at a time.
func (c *Client) SetHeaderOverrides(overrides map[string]string) {
	c.mu.Lock()
	c.overrides = overrides
	c.mu.Unlock()
}

// New builds a client for a cookie jar.
func New(jar *cookiejar.Jar, hc *httpx.Client) *Client {
	return &Client{jar: jar, hc: hc}
}

// SetFingerprint installs the browser identity to present upstream.
//
// Call this with the identity of the browser that minted the reCAPTCHA token
// whenever the client will issue a captcha-bearing call.
func (c *Client) SetFingerprint(fp Fingerprint) {
	c.mu.Lock()
	c.fp = fp
	c.mu.Unlock()
}

// requestHeaders builds the header set for one call, with the diagnostic
// overrides already folded in.
func (c *Client) requestHeaders(auth string) map[string]string {
	ua, secChUa, platform, mobile, language := c.identity()

	headers := map[string]string{
		"Authorization":      auth,
		"Content-Type":       "application/x-www-form-urlencoded;charset=UTF-8",
		"Accept":             "*/*",
		"Accept-Language":    language,
		"Origin":             Origin,
		"Referer":            Origin + "/",
		"X-Same-Domain":      "1",
		"User-Agent":         ua,
		"sec-ch-ua":          secChUa,
		"sec-ch-ua-mobile":   mobile,
		"sec-ch-ua-platform": platform,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-origin",
	}
	c.applyOverrides(headers)
	return headers
}

// sessionIDFor resolves the `f.sid` to send: the call's own when it names one,
// otherwise the client's.
//
// The app sends one on every batchexecute request — the live capture shows
// `f.sid` beside `bl` on all of them — and the engine sent none, because the
// field existed on CallOptions and only the CLI ever filled it. Carrying it on
// the client means every call inherits the page's value without each call site
// having to remember.
func (c *Client) sessionIDFor(opts CallOptions) string {
	if opts.SessionID != "" {
		return opts.SessionID
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// SetSessionID sets the `f.sid` sent with calls that do not name one.
func (c *Client) SetSessionID(id string) {
	c.mu.Lock()
	c.sessionID = id
	c.mu.Unlock()
}

// ResolvedHeaders reports the headers and Cookie value a call would go out
// with, for comparing against the browser's own request. Diagnostics only.
func (c *Client) ResolvedHeaders() (map[string]string, string) {
	auth, err := c.Authorization()
	if err != nil {
		auth = "<error: " + err.Error() + ">"
	}
	return c.requestHeaders(auth), c.jar.HeaderForDomain(Origin + "/")
}

// applyOverrides folds the diagnostic overrides into a header map. A header
// mapped to "" is removed, which is how a comparison run drops the
// Authorization header the browser does not send.
func (c *Client) applyOverrides(headers map[string]string) {
	c.mu.Lock()
	overrides := c.overrides
	c.mu.Unlock()

	for name, value := range overrides {
		if value != "" {
			headers[name] = value
			continue
		}
		for existing := range headers {
			if strings.EqualFold(existing, name) {
				delete(headers, existing)
			}
		}
	}
}

// identity resolves the headers to send, filling gaps from the generic Chrome
// profile so a request is never left with no user-agent at all.
func (c *Client) identity() (ua, secChUa, platform, mobile, language string) {
	c.mu.Lock()
	fp := c.fp
	c.mu.Unlock()

	ua, platform, secChUa = fp.UserAgent, fp.Platform, fp.SecChUa
	if ua == "" {
		ua = httpx.ChromeUA
	}
	if platform == "" {
		platform = httpx.ChromePlatform
	}
	if secChUa == "" {
		secChUa = httpx.ChromeSecChUA
	}
	mobile = fp.Mobile
	if mobile == "" {
		mobile = "?0"
	}
	language = fp.Language
	if language == "" {
		language = "en-US,en;q=0.9"
	}
	return ua, secChUa, platform, mobile, language
}

// SetJar swaps the cookie jar, e.g. after a session refresh. The cached
// anti-CSRF token is dropped with it, since it is bound to the session.
func (c *Client) SetJar(jar *cookiejar.Jar) {
	c.mu.Lock()
	c.jar = jar
	c.at = ""
	c.mu.Unlock()
}

// SeedToken pre-loads the anti-CSRF token so a call does not have to earn one
// with a rejected attempt first.
//
// Diagnostics only: the browser sends its token on the very first request, and
// the priming round trip is the one structural difference left between the two.
func (c *Client) SeedToken(token string) {
	c.mu.Lock()
	c.at = token
	c.mu.Unlock()
}

// Token returns the cached anti-CSRF token, if one has been obtained.
func (c *Client) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *Client) setToken(token string) {
	c.mu.Lock()
	c.at = token
	c.mu.Unlock()
}

// xsrfRe pulls the anti-CSRF token out of a rejected response.
var xsrfRe = regexp.MustCompile(`"xsrf"\s*,\s*"([^"]+)"`)

// extractXSRF finds the anti-CSRF token in a batchexecute response body.
//
// The server answers an un-tokened call with HTTP 400 and a body containing
// ["xsrf","<token>",[...]]. That is not an error to surface: it is the first half
// of the handshake, and the same call succeeds once the token is echoed back.
func extractXSRF(body string) string {
	if match := xsrfRe.FindStringSubmatch(body); len(match) > 1 {
		return match[1]
	}
	return ""
}

// SAPISID returns the SAPISID cookie, which the authorization header is derived
// from.
func (c *Client) SAPISID() string {
	if c.jar == nil {
		return ""
	}
	for _, ck := range c.jar.Cookies() {
		if ck.Name == "SAPISID" || ck.Name == "__Secure-3PAPISID" {
			if ck.Value != "" {
				return ck.Value
			}
		}
	}
	return ""
}

// Authorization builds the SAPISIDHASH header value.
//
// Google's scheme is `SAPISIDHASH <unix-seconds>_<sha1-hex>` over
// "<seconds> <SAPISID> <origin>". It proves possession of the cookie without
// transmitting it, and it is scoped to the origin, so the hash for
// flow.google.com cannot be replayed against another Google host.
func (c *Client) Authorization() (string, error) {
	sapisid := c.SAPISID()
	if sapisid == "" {
		return "", fmt.Errorf(
			"batchexecute: no SAPISID cookie in the jar; re-sync cookies from a signed-in browser")
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sum := sha1.Sum([]byte(ts + " " + sapisid + " " + Origin))
	return "SAPISIDHASH " + ts + "_" + hex.EncodeToString(sum[:]), nil
}

// Frame is one parsed wrb.fr response frame.
type Frame struct {
	// RPCID is the id echoed back by the server.
	RPCID string
	// Payload is the raw JSON of the frame's data element, when present.
	Payload json.RawMessage
}

// CallOptions carries the parameters the app sends alongside f.req.
//
// All three were read off the app's own traffic. They are not strictly required
// for every RPC, but omitting them makes the request visibly unlike the app's,
// and `source-path` is what scopes a call to a project.
type CallOptions struct {
	// SourcePath is the app route the call is made from, e.g. "/project/<id>".
	SourcePath string
	// BuildLabel is the `bl` parameter, e.g.
	// "boq_labs-ai-sandbox-frontend_20260917.00_p0".
	BuildLabel string
	// SessionID is the `f.sid` parameter from the page.
	SessionID string
	// RPCID overrides the RPC a call goes to. Diagnostics only: the mapping from
	// a submission's shape to its RPC is what several models hinge on, so it has
	// to be overridable to test one against another.
	RPCID string
}

// Call issues one RPC and returns its frames.
//
// rpcID names the server method; payload is the method's argument, which
// batchexecute expects as a JSON string nested inside the envelope. Passing nil
// sends an empty argument, which is enough for read-only methods.
func (c *Client) Call(ctx context.Context, rpcID string, payload any) ([]Frame, error) {
	return c.CallWith(ctx, rpcID, payload, CallOptions{})
}

// CallWith is Call with the app's request parameters supplied explicitly.
func (c *Client) CallWith(ctx context.Context, rpcID string, payload any, opts CallOptions) ([]Frame, error) {
	auth, err := c.Authorization()
	if err != nil {
		return nil, err
	}

	arg := "[]"
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("batchexecute: could not encode the payload: %w", err)
		}
		arg = string(encoded)
	}

	// The envelope is [[[rpcid, "<args>", null, "generic"]]]. The arguments are a
	// string, not an object — that nesting is part of the protocol.
	envelope := [][][]any{{{rpcID, arg, nil, "generic"}}}
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("batchexecute: could not encode the envelope: %w", err)
	}

	reqID := atomic.AddInt64(&c.reqs, 1)*100000 + 1000

	sourcePath := opts.SourcePath
	if sourcePath == "" {
		sourcePath = "/project"
	}

	query := url.Values{}
	query.Set("rpcids", rpcID)
	query.Set("source-path", sourcePath)
	query.Set("hl", "en")
	query.Set("rt", "c")
	query.Set("_reqid", strconv.FormatInt(reqID, 10))
	if opts.BuildLabel != "" {
		query.Set("bl", opts.BuildLabel)
	}
	if sid := c.sessionIDFor(opts); sid != "" {
		query.Set("f.sid", sid)
	}

	resp, err := c.post(ctx, rpcID, string(envelopeJSON), query.Encode(), auth)
	if err != nil {
		return nil, err
	}
	return ParseFrames(resp.Text())
}

// CallRaw performs the same call as CallWith but returns the response body
// verbatim.
//
// Diagnostics only. ParseFrames keeps only the wrb.fr frames and drops the
// payload of any frame whose data element is not a non-empty string, so an RPC
// that answers with an error frame and an RPC that genuinely returns nothing
// both look like a nil Payload. When that happens the raw body is the only place
// the reason is still visible.
func (c *Client) CallRaw(ctx context.Context, rpcID string, payload any, opts CallOptions) (string, error) {
	auth, err := c.Authorization()
	if err != nil {
		return "", err
	}

	arg := "[]"
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return "", fmt.Errorf("batchexecute: could not encode the payload: %w", err)
		}
		arg = string(encoded)
	}

	envelope := [][][]any{{{rpcID, arg, nil, "generic"}}}
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("batchexecute: could not encode the envelope: %w", err)
	}

	sourcePath := opts.SourcePath
	if sourcePath == "" {
		sourcePath = "/project"
	}

	query := url.Values{}
	query.Set("rpcids", rpcID)
	query.Set("source-path", sourcePath)
	query.Set("hl", "en")
	query.Set("rt", "c")
	query.Set("_reqid", strconv.FormatInt(atomic.AddInt64(&c.reqs, 1)*100000+1000, 10))
	if opts.BuildLabel != "" {
		query.Set("bl", opts.BuildLabel)
	}
	if sid := c.sessionIDFor(opts); sid != "" {
		query.Set("f.sid", sid)
	}

	resp, err := c.post(ctx, rpcID, string(envelopeJSON), query.Encode(), auth)
	if err != nil {
		return "", err
	}
	return resp.Text(), nil
}

// UnauthorizedHandler refreshes a session the server has rejected.
//
// It returns the fresh cookie jar so the client can swap it in, or nil to keep
// the current one. An error means the refresh itself failed and the call should
// not be retried.
type UnauthorizedHandler func(ctx context.Context) (*cookiejar.Jar, error)

// SetUnauthorizedHandler installs the session refresher.
//
// Without one, a stale session is terminal for the client. That is how a running
// engine behaves today: `/v1/credits` starts answering 401 and keeps answering
// 401 until an operator calls /v1/bridge/refresh by hand.
func (c *Client) SetUnauthorizedHandler(fn UnauthorizedHandler) {
	c.mu.Lock()
	c.onUnauthorized = fn
	c.mu.Unlock()
}

// post sends one batchexecute envelope.
//
// It handles two recoverable rejections, in this order:
//
//   - a 400 carrying the anti-CSRF token, which the next attempt echoes back;
//   - a 401, which means the session behind the cookies has expired. The
//     installed refresher is run once and the priming sequence is repeated
//     against the new session.
//
// Both are bounded: at most two priming sequences, so four requests, and the
// refresh is attempted at most once per call.
func (c *Client) post(ctx context.Context, rpcID, envelopeJSON, rawQuery, auth string) (*httpx.Response, error) {
	fullURL := c.endpointURL(rawQuery)
	headers := c.requestHeaders(auth)

	// send runs the priming sequence: the first attempt usually comes back 400
	// carrying the anti-CSRF token, which the second echoes back.
	send := func() (*httpx.Response, error) {
		for attempt := 1; attempt <= 2; attempt++ {
			body := url.Values{}
			body.Set("f.req", envelopeJSON)
			if token := c.Token(); token != "" {
				body.Set("at", token)
			}

			c.mu.Lock()
			useQUIC := c.useQUIC
			order := c.headerOrder
			c.mu.Unlock()
			if len(order) == 0 {
				order = httpx.ChromeHeaderOrder
			}

			resp, err := c.hc.Do(ctx, &httpx.Request{
				Method:      "POST",
				URL:         fullURL,
				Body:        []byte(body.Encode()),
				Headers:     headers,
				HeaderOrder: order,
				Cookies:     c.jar.HeaderForDomain(Origin + "/"),
				DisableQUIC: !useQUIC,
				AllowQUIC:   useQUIC,
			})
			if err != nil {
				return nil, fmt.Errorf("batchexecute: %s request failed: %w", rpcID, err)
			}

			if resp.Status == 200 {
				return resp, nil
			}

			// A rejected first attempt may be carrying the anti-CSRF token we
			// need. A 401 never does, so it falls through to the caller.
			if attempt == 1 {
				if token := extractXSRF(resp.Text()); token != "" {
					c.setToken(token)
					continue
				}
			}
			return resp, nil
		}
		return nil, fmt.Errorf("batchexecute: %s did not succeed after obtaining the anti-CSRF token", rpcID)
	}

	resp, err := send()
	if err != nil {
		return nil, err
	}
	if resp.Status == 200 {
		return resp, nil
	}

	if resp.Status != 401 {
		return nil, fmt.Errorf("batchexecute: %s returned %d: %s",
			rpcID, resp.Status, truncate(resp.Text(), 300))
	}

	// A 401 is the session, not the request.
	c.mu.Lock()
	refresher := c.onUnauthorized
	c.mu.Unlock()
	if refresher == nil {
		return nil, fmt.Errorf("batchexecute: %s returned 401 and no session refresher is installed: %s",
			rpcID, truncate(resp.Text(), 200))
	}

	log.Printf("batchexecute: %s returned 401; refreshing the session and retrying", rpcID)

	// The cached anti-CSRF token belongs to the session that just expired.
	c.mu.Lock()
	c.at = ""
	c.mu.Unlock()

	jar, refreshErr := refresher(ctx)
	if refreshErr != nil {
		return nil, fmt.Errorf("batchexecute: %s returned 401 and the session refresh failed: %w", rpcID, refreshErr)
	}
	if jar != nil {
		c.SetJar(jar)
	}

	// The Authorization header is derived from the cookies, so it is stale too.
	if fresh, authErr := c.Authorization(); authErr == nil {
		auth = fresh
	}
	headers = c.requestHeaders(auth)

	resp, err = send()
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("batchexecute: %s still returned %d after a session refresh: %s",
			rpcID, resp.Status, truncate(resp.Text(), 300))
	}
	return resp, nil
}

// upsampledSuffix is appended to the source asset's content id to name the
// upscaled render. It is what makes the id findable without trusting a path.
const upsampledSuffix = "_upsampled"

// ParseUpscaledAssetID pulls the new asset id out of an upscale response.
//
// The payload is `[[[["<content-id>_upsampled"],"",null,null,1]],323,[...]]`, so
// the id sits at [0][0][0][0] — four levels down, which is worth pinning because
// the value is a one-element array rather than a bare string.
//
// What it returns is a *submission acknowledgement*: the id names a render that
// does not exist yet, and its URL only appears once the render finishes. Treating
// this response as the finished asset — or as "no output", because it carries no
// URL — is the mistake that made this RPC look broken.
func ParseUpscaledAssetID(frames []Frame) (string, error) {
	for _, frame := range frames {
		if len(frame.Payload) == 0 {
			continue
		}
		var payload []any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			continue
		}
		if id := firstStringAt(payload, 0, 0, 0, 0); strings.HasSuffix(id, upsampledSuffix) {
			return id, nil
		}
		// The path above is the captured shape. Fall back to a search so a
		// re-nested response degrades to "found anyway" rather than "no output".
		if id := findUpsampledID(payload); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("batchexecute: the upscale response carried no asset id")
}

// findUpsampledID walks a decoded payload for the upscaled asset id. The suffix
// is unique to it, so the search cannot pick up a neighbouring value.
func findUpsampledID(value any) string {
	switch typed := value.(type) {
	case string:
		if strings.HasSuffix(typed, upsampledSuffix) {
			return typed
		}
	case []any:
		for _, item := range typed {
			if found := findUpsampledID(item); found != "" {
				return found
			}
		}
	}
	return ""
}

// firstStringAt walks a nested array by index and returns the string it finds,
// or "" if the path does not lead to one.
func firstStringAt(value any, path ...int) string {
	current := value
	for _, index := range path {
		list, ok := current.([]any)
		if !ok || index >= len(list) {
			return ""
		}
		current = list[index]
	}
	text, _ := current.(string)
	return text
}

// GenerateRequest describes a generation submission.
type GenerateRequest struct {
	// ProjectID is the project to generate into.
	ProjectID string
	// Model is the image enum ("NARWHAL", "HARBOR_SEAL", "GEM_PIX_2") or the
	// video key ("abra_t2v_8s").
	Model string
	// Prompt is the text prompt.
	Prompt string
	// Seed makes a take reproducible. Zero lets the server choose.
	Seed int64
	// CaptchaToken is a reCAPTCHA Enterprise token.
	//
	// This is the field that took longest to identify. It sits at [1][0][7][10][0]
	// and is about 2500 characters beginning with "0cAFcWeA". It is not in any RPC
	// response, in localStorage, in sessionStorage, in a cookie, or on any window
	// global — because it is not a session token at all. It is the reCAPTCHA
	// token, minted in the page at generation time and handed straight into the
	// request. Comparing its prefix and length against tokens produced by the
	// page's own grecaptcha is what settled it.
	CaptchaToken string
}

// generateMode is the flag at position [4] of a generation request. Observed as 3
// on every captured call; its meaning is unknown, so it is reproduced rather
// than guessed at.
const generateMode = 3

// toolContextID is the value at [7][1], observed as 22.
const toolContextID = 22

// Generate submits a generation and returns the response frames.
//
// The argument shape is reproduced from a live capture of the app's own request;
// see the RPCIDGenerate doc comment for the decoded layout.
func (c *Client) Generate(ctx context.Context, req GenerateRequest, opts CallOptions) ([]Frame, error) {
	if req.ProjectID == "" {
		return nil, fmt.Errorf("batchexecute: a project id is required")
	}
	if req.Model == "" {
		return nil, fmt.Errorf("batchexecute: a model is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("batchexecute: a prompt is required")
	}
	if req.CaptchaToken == "" {
		return nil, fmt.Errorf("batchexecute: a reCAPTCHA token is required")
	}

	seed := req.Seed
	if seed == 0 {
		seed = time.Now().UnixNano() % 1000000000
	}

	arg := buildGenerateArgument(req, seed)

	if opts.SourcePath == "" {
		opts.SourcePath = "/project/" + req.ProjectID
	}

	return c.CallWith(ctx, RPCIDGenerate, arg, opts)
}

// GenerateMedia submits a generation and returns the assets it produced.
//
// A successful call returns the asset inline, so unlike the legacy REST path
// there is no polling step for images.
func (c *Client) GenerateMedia(ctx context.Context, req GenerateRequest, opts CallOptions) ([]GeneratedMedia, error) {
	frames, err := c.Generate(ctx, req, opts)
	if err != nil {
		return nil, err
	}

	var out []GeneratedMedia
	for _, frame := range frames {
		out = append(out, ParseGeneratedMedia(frame.Payload)...)
	}
	return out, nil
}

// videoImageBlock is one condition image: the media id plus its crop rectangle.
//
// The crop is a trailing array of four, [null, left, 1, right]. The app always
// emits it, and encodes a full frame as [null, null, 1, 1] — so an absent crop is
// not the same as a missing array. Two live captures show both forms: a cropped
// submission carried [null, 0.3417..., 1, 0.6582...] and an uncropped one
// [null, null, 1, 1].
func videoImageBlock(mediaID string, frame []float64) []any {
	values := make([]any, 0, 4)
	if len(frame) > 0 {
		values = append(values, nil)
		for _, v := range frame {
			values = append(values, v)
		}
	} else {
		values = append(values, nil, nil, float64(1), float64(1))
	}
	return []any{nil, mediaID, nil, nil, nil, values}
}

// buildVideoArgument assembles the video generation payload.
//
// Separated from GenerateVideo so its shape can be asserted directly: the prompt
// path differs from the image RPC's, and getting it wrong is a silent no-op.
//
// The layout is measured from the app's own requests, not documented. Text-only:
//
//	[prompt-block, model, 2, null, [null,null,null,null,<uuid>,<uuid>]]
//
// Conditioned on images, each image takes its own slot *before* the uuid block and
// the mode becomes 1:
//
//	[prompt-block, model, 1, null, <start>, <end>, [null,null,null,null,<uuid>,<uuid>]]
//
// so the uuid block's index moves with the number of images. It is appended last
// rather than written at a fixed position for that reason.
func buildVideoArgument(req GenerateVideoRequest) []any {
	count := req.Count
	if count < 1 {
		count = 1
	}

	mode := videoModeText
	if req.StartImage != "" || req.EndImage != "" {
		mode = videoModeImage
	}
	if req.ModeOverride != nil {
		mode = *req.ModeOverride
	}

	// One entry per variation, each with its own uuid pair.
	requests := make([]any, 0, count)
	for i := 0; i < count; i++ {
		request := []any{
			// The prompt block sits at [0] rather than [8] as it does for images,
			// and nests three levels so the string lands at [0][0][2][0][0][0].
			// One level too many and the server accepts the request with a 200
			// while generating nothing.
			[]any{nil, nil, []any{[]any{[]any{req.Prompt}}}},
			req.Model,
			mode,
			nil,
		}
		// One slot per supplied image, in order, each before the uuid block. The
		// layout is the same for one image as for two; what differs is the RPC
		// (see RPCIDGenerateVideoImageStart). A first-only submission against
		// the *_first_last_ model still comes back empty — that model wants both
		// frames — but abra_i2v_<n>s renders from the start frame alone.
		if req.StartImage != "" {
			request = append(request, videoImageBlock(req.StartImage, req.StartFrame))
		}
		if req.EndImage != "" {
			request = append(request, videoImageBlock(req.EndImage, req.EndFrame))
		}
		request = append(request, []any{nil, nil, nil, nil, uuid.NewString(), uuid.NewString()})
		requests = append(requests, request)
	}

	contextBlock := []any{
		nil,
		toolContextID,
		nil, nil, nil,
		req.ProjectID,
		nil, nil, nil, nil,
		[]any{req.CaptchaToken, 1},
	}

	return []any{
		requests,
		contextBlock,
		[]any{uuid.NewString(), videoModeText},
	}
}

// buildGenerateArgument assembles the generation payload.
//
// It is separated from Generate so its shape can be asserted directly. The
// nesting depth of the prompt in particular is not something a live call can
// confirm: get it wrong and the server still answers 200 while generating
// nothing, so a unit test is the only cheap guard.
func buildGenerateArgument(req GenerateRequest, seed int64) []any {
	// The context block appears twice in the captured payload, byte for byte
	// identical, so it is built once and referenced twice.
	contextBlock := []any{
		nil,
		toolContextID,
		nil, nil, nil,
		req.ProjectID,
		nil, nil, nil, nil,
		[]any{req.CaptchaToken, 1},
	}

	request := []any{
		nil, nil, nil,
		seed,
		generateMode,
		req.Model,
		nil,
		contextBlock,
		// Three levels, not four: the prompt string must land at [8][0][0][0].
		// With an extra level a list sits there instead, and the server accepts
		// the request with a 200 while generating nothing.
		[]any{[]any{[]any{req.Prompt}}},
		nil, nil, nil,
		uuid.NewString(),
		uuid.NewString(),
	}

	return []any{
		nil,
		[]any{request},
		1,
		contextBlock,
		[]any{uuid.NewString()},
	}
}

// ParseGeneratedMediaIDs extracts the media ids from a generation response.
//
// Video is asynchronous: the response carries ids to poll rather than a signed
// URL, so ParseGeneratedMedia finds nothing there. The shape is
// [null, <credits>, [[<media-id>, null, null, [...], <project-id>], ...]].
func ParseGeneratedMediaIDs(payload json.RawMessage) []string {
	var doc []any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil
	}
	if len(doc) < 3 {
		return nil
	}

	entries, ok := doc[2].([]any)
	if !ok {
		return nil
	}

	seen := make(map[string]bool)
	var ids []string
	for _, entry := range entries {
		row, ok := entry.([]any)
		if !ok || len(row) == 0 {
			continue
		}
		id, ok := row[0].(string)
		if !ok || id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// Credits reads the account's Flow credit balance.
//
// Takes no argument, so it needs no project context.
func (c *Client) Credits(ctx context.Context, opts CallOptions) (int, error) {
	frames, err := c.CallWith(ctx, RPCIDCredits, nil, opts)
	if err != nil {
		return 0, err
	}

	// The payload is a flat array; the balance is the first numeric element.
	for _, frame := range frames {
		var values []any
		if err := json.Unmarshal(frame.Payload, &values); err != nil {
			continue
		}
		for _, value := range values {
			if n, ok := value.(float64); ok {
				return int(n), nil
			}
		}
	}
	return 0, fmt.Errorf("batchexecute: the credits response carried no numeric balance")
}

// Profile is the identity of the account a client is acting as.
type Profile struct {
	// Name is the display name the account publishes.
	Name string
	// PhotoURL is the account's avatar.
	PhotoURL string
	// Email is the account's address, and the only per-account identifier the
	// backend will admit to. Read it from here rather than from the labs session
	// endpoint: that endpoint answers for the default account whatever
	// `authuser` says, so taking the address from it stamps every signed-in
	// account with the first one's address.
	Email string
}

// profileArg is the captured argument for RPCIDProfile — ask for "me", and name
// the three fields wanted back.
var profileArg = []any{
	[]any{"me"},
	[]any{
		[]any{[]any{"person.photo", "person.name", "person.email"}},
		nil,
		[]any{1, 7},
	},
}

// Profile reads the identity of the account this client is acting as.
//
// Takes no argument and needs no project context, but it is account-scoped: the
// answer follows `authuser`, which is what makes it the one place a per-account
// address can be had.
func (c *Client) Profile(ctx context.Context, opts CallOptions) (Profile, error) {
	frames, err := c.CallWith(ctx, RPCIDProfile, profileArg, opts)
	if err != nil {
		return Profile{}, err
	}
	for _, frame := range frames {
		var payload any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			continue
		}
		if p, ok := findProfile(payload); ok {
			return Profile{Name: p.name, PhotoURL: p.photo, Email: p.email}, nil
		}
	}
	return Profile{}, fmt.Errorf("batchexecute: the profile response carried no identity")
}

// profile is the subset of the response this engine needs.
type profile struct {
	name  string
	photo string
	email string
}

// complete reports whether enough was found to identify an account. The name is
// required alongside the address because an address alone would accept a
// half-read response, and the avatar is optional — plenty of accounts have none.
func (p profile) complete() bool { return p.email != "" && p.name != "" }

// findProfile walks the profile response and collects the identity fields.
//
// The response is a deep, sparsely populated tree, and the three wanted values
// sit at unrelated positions inside it — name, avatar and address were observed
// at indices 2, 3 and 9 of one run. Each arrives as a one-element array wrapping
// a [<flags...>, "<value>", ...] pair, and nothing keys them: the flags in front
// are opaque and their width differs per field. So the values are picked out by
// what they hold rather than by where they sit — the address contains an @, the
// avatar is a googleusercontent URL, and the display name is whatever is left.
// That survives a column being added, which fixed offsets would not.
func findProfile(v any) (profile, bool) {
	var p profile
	collectProfile(v, &p)
	if !p.complete() {
		return profile{}, false
	}
	return p, true
}

func collectProfile(v any, p *profile) {
	arr, ok := v.([]any)
	if !ok {
		return
	}
	for _, entry := range arr {
		if s, ok := profileField(entry); ok {
			switch {
			case strings.Contains(s, "@"):
				if p.email == "" {
					p.email = s
				}
			case strings.Contains(s, "googleusercontent.com"):
				if p.photo == "" {
					p.photo = s
				}
			default:
				if p.name == "" {
					p.name = s
				}
			}
			continue
		}
		collectProfile(entry, p)
	}
}

// profileField returns the value of a single profile field: a one-element array
// wrapping a [<flags...>, "<value>", ...] pair. Anything else — a bare string, a
// flags-only array, a pair with no value — is not a field.
func profileField(entry any) (string, bool) {
	wrapper, ok := entry.([]any)
	if !ok || len(wrapper) != 1 {
		return "", false
	}
	pair, ok := wrapper[0].([]any)
	if !ok || len(pair) < 2 {
		return "", false
	}
	value, ok := pair[1].(string)
	if !ok || value == "" {
		return "", false
	}
	return value, true
}

// UploadMediaRequest is one image to upload into a project.
type UploadMediaRequest struct {
	ProjectID string
	// Data is the raw image.
	Data []byte
	// MimeType is the image's content type, e.g. "image/jpeg".
	MimeType string
	// FileName is the name the asset is stored under. The app sends the local
	// file's name with spaces replaced by underscores.
	FileName string
	// CaptchaToken is a reCAPTCHA token.
	CaptchaToken string
}

// UploadMedia uploads an image and returns its media id and content id.
//
// The argument is positional, captured from the app's own upload:
//
//	[context-block, <base64 image>, "<mime>", 1, null, null, null, null,
//	 "<file name>", null, <uuid>, <uuid>]
//
// The response carries the ids in its first row: [content-id, project,
// media-id, "CAE", ...]. The media id is the third element.
func (c *Client) UploadMedia(ctx context.Context, req UploadMediaRequest) (mediaID, contentID string, err error) {
	if req.ProjectID == "" {
		return "", "", fmt.Errorf("batchexecute: a project id is required")
	}
	if len(req.Data) == 0 {
		return "", "", fmt.Errorf("batchexecute: refusing to upload an empty file")
	}
	if req.MimeType == "" {
		return "", "", fmt.Errorf("batchexecute: a mime type is required")
	}
	if req.CaptchaToken == "" {
		return "", "", fmt.Errorf("batchexecute: a reCAPTCHA token is required")
	}

	arg := []any{
		[]any{
			nil, toolContextID, nil, nil, nil, req.ProjectID, nil, nil, nil, nil,
			[]any{req.CaptchaToken, 1},
		},
		base64.StdEncoding.EncodeToString(req.Data),
		req.MimeType,
		1,
		nil, nil, nil, nil,
		req.FileName,
		nil,
		uuid.NewString(),
		uuid.NewString(),
	}

	frames, err := c.CallWith(ctx, RPCIDUploadMedia, arg, CallOptions{
		SourcePath: "/project/" + req.ProjectID,
	})
	if err != nil {
		return "", "", err
	}
	return ParseUploadedMediaID(frames)
}

// ParseUploadedMediaID pulls the media id and content id out of an upload reply.
//
// The payload is [[content-id, project, media-id, "CAE", ...], ...], so the ids
// are the first and third elements of the first row.
func ParseUploadedMediaID(frames []Frame) (mediaID, contentID string, err error) {
	for _, frame := range frames {
		if len(frame.Payload) == 0 {
			continue
		}
		var payload []any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			continue
		}
		contentID = firstStringAt(payload, 0, 0)
		mediaID = firstStringAt(payload, 0, 2)
		if mediaID != "" {
			return mediaID, contentID, nil
		}
	}
	return "", "", fmt.Errorf("batchexecute: the upload response carried no media id")
}

// GenerateVideoRequest describes a video generation submission.
type GenerateVideoRequest struct {
	ProjectID string
	// Model is the video key: "abra_t2v_8s", "veo_3_1_t2v_fast_4s", or an
	// image-conditioned key such as "omni_flash_i2v_8s_first_last_360p".
	Model  string
	Prompt string
	// Count is how many variations to submit. Each becomes its own entry in the
	// requests array with its own uuid pair.
	Count int
	// CaptchaToken is a reCAPTCHA token, minted for the VIDEO_GENERATION action.
	CaptchaToken string
	// StartImage and EndImage are project media ids the video is conditioned on.
	// Setting either switches the request into image mode.
	StartImage string
	EndImage   string
	// StartFrame and EndFrame are the per-image crop values the app sends next to
	// a condition image, as three normalised numbers.
	//
	// The meaning is inferred, not documented: a captured first+last submission
	// carried [0.3418, 1, 0.6582] for both images, which reads like a centred
	// crop. Leaving them nil is the honest default — it sends no crop rather than
	// guessing at one and silently cutting the frame.
	StartFrame []float64
	EndFrame   []float64
	// ModeOverride replaces the mode flag at request[2]. Diagnostics only: the
	// flag is 2 for text and 1 for image-conditioned, and whether a third value
	// selects the reference-image behaviour is unknown.
	ModeOverride *int
}

// Video generation modes, read off the app's own requests.
//
// A text-only submission carries 2; one conditioned on images carries 1. It is not
// the number of inputs — the captured image request had two images and sent 1.
const (
	videoModeText  = 2
	videoModeImage = 1
)

// GenerateVideo submits a video generation and returns the response frames.
//
// The argument shape is reproduced from a live capture of the app's own request;
// see the RPCIDGenerateVideo doc comment for the decoded layout.
func (c *Client) GenerateVideo(ctx context.Context, req GenerateVideoRequest, opts CallOptions) ([]Frame, error) {
	if req.ProjectID == "" {
		return nil, fmt.Errorf("batchexecute: a project id is required")
	}
	if req.Model == "" {
		return nil, fmt.Errorf("batchexecute: a model is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("batchexecute: a prompt is required")
	}
	if req.CaptchaToken == "" {
		return nil, fmt.Errorf("batchexecute: a reCAPTCHA token is required")
	}

	count := req.Count
	if count < 1 {
		count = 1
	}

	arg := buildVideoArgument(req)

	if opts.SourcePath == "" {
		opts.SourcePath = "/project/" + req.ProjectID
	}

	// The RPC depends on the *shape* of the conditioning, not merely on whether
	// any is present: a start frame on its own goes to a third id, and sending
	// it to the first+last one is accepted with an empty result.
	rpcID := VideoRPCID(req)
	if opts.RPCID != "" {
		rpcID = opts.RPCID
	}
	return c.CallWith(ctx, rpcID, arg, opts)
}

// EditVideoRequest describes a video edit submission.
//
// The source is a *content* id — the kind in an asset's
// https://flow-content.google/video/<id> URL — not the media id a fresh upload
// returns. The edit capture's source, b8864807-…, resolves to one of the
// project's videos, which is what identifies abra_edit as the video-to-video
// model rather than an image-conditioned one.
type EditVideoRequest struct {
	ProjectID string
	// SourceID is the content id of the asset being edited.
	SourceID string
	// Prompt describes the edit, e.g. "make the boat drift slowly to the left".
	Prompt string
	// Model is the edit model key, normally "abra_edit".
	Model string
	// TrimStart and TrimEnd are the pair of numbers the app sends next to the
	// source id. The capture carried 0 and 192; their meaning is not documented
	// and they are passed through rather than interpreted.
	TrimStart int
	TrimEnd   int
	// CaptchaToken is a reCAPTCHA token, minted for the VIDEO_GENERATION action.
	CaptchaToken string
}

// buildEditArgument assembles the video edit payload.
//
// Measured from the app's own request, which is shaped differently from a
// generation: the source sits in its own block at index 0, *before* the prompt,
// and the request carries five elements rather than the generation's longer
// list. Decoded:
//
//	[ [null, "<content-id>", 0, 192],
//	  [null, null, [[[prompt]]]],
//	  "abra_edit",
//	  2,
//	  [null, null, null, null, <uuid>, <uuid>] ]
func buildEditArgument(req EditVideoRequest) []any {
	request := []any{
		[]any{nil, req.SourceID, req.TrimStart, req.TrimEnd},
		[]any{nil, nil, []any{[]any{[]any{req.Prompt}}}},
		req.Model,
		// The capture carried 2 here, the same value a text submission carries at
		// its mode slot. The two are not known to mean the same thing.
		videoModeText,
		[]any{nil, nil, nil, nil, uuid.NewString(), uuid.NewString()},
	}

	contextBlock := []any{
		nil,
		toolContextID,
		nil, nil, nil,
		req.ProjectID,
		nil, nil, nil, nil,
		[]any{req.CaptchaToken, 1},
	}

	return []any{
		[]any{request},
		contextBlock,
		[]any{uuid.NewString(), videoModeText},
	}
}

// GenerateVideoEdit submits a video edit and returns the response frames.
//
// Verified live: a replay of the captured payload returns a media id for both a
// video and an image content id as the source.
func (c *Client) GenerateVideoEdit(ctx context.Context, req EditVideoRequest, opts CallOptions) ([]Frame, error) {
	if req.ProjectID == "" {
		return nil, fmt.Errorf("batchexecute: a project id is required")
	}
	if req.SourceID == "" {
		return nil, fmt.Errorf("batchexecute: a source content id is required")
	}
	if req.Model == "" {
		return nil, fmt.Errorf("batchexecute: a model is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("batchexecute: a prompt is required")
	}
	if req.CaptchaToken == "" {
		return nil, fmt.Errorf("batchexecute: a reCAPTCHA token is required")
	}

	arg := buildEditArgument(req)

	if opts.SourcePath == "" {
		opts.SourcePath = "/project/" + req.ProjectID
	}
	rpcID := RPCIDVideoEdit
	if opts.RPCID != "" {
		rpcID = opts.RPCID
	}
	return c.CallWith(ctx, rpcID, arg, opts)
}

// referenceTail is the value the app puts at request[0][0][10] of a
// reference-image submission.
//
// The zero tail marks it as a fixed value rather than a per-request id — a
// generated uuid never ends in twelve zeros — and it was identical across the
// capture, so it is reproduced verbatim. What it selects is not known: a style, a
// preset, or a feature flag.
const referenceTail = "d351dd3c-0a12-1522-0000-000000000000"

// ReferenceVideoRequest describes a video generation conditioned on reference
// images.
type ReferenceVideoRequest struct {
	ProjectID string
	// Model is a reference-image key such as "abra_r2v_4s".
	Model  string
	Prompt string
	// References are project media ids, one per reference image. The capture
	// carried four; at least one is required.
	References []string
	// Count is how many variations to submit. Each becomes its own entry with its
	// own uuid pair.
	Count int
	// CaptchaToken is a reCAPTCHA token, minted for the VIDEO_GENERATION action.
	CaptchaToken string
}

// buildReferenceArgument assembles the reference-image payload.
//
// Measured from the app's own request. The prompt is at index 0 and the reference
// list at index 1 — the same front-loading the edit payload uses, and the opposite
// of the i2v shape, which puts the prompt at index 2 and the images in trailing
// slots:
//
//	[null, null, [[[prompt]]]],
//	[[null, "<id>"], [null, "<id>"], ...],
//	"abra_r2v_4s", 2, null,
//	[null, null, null, null, <uuid>, <uuid>],
//	null, null, null, null,
//	[["d351dd3c-0a12-1522-0000-000000000000"]]
func buildReferenceArgument(req ReferenceVideoRequest) []any {
	count := req.Count
	if count < 1 {
		count = 1
	}

	refs := make([]any, 0, len(req.References))
	for _, id := range req.References {
		refs = append(refs, []any{nil, id})
	}

	requests := make([]any, 0, count)
	for i := 0; i < count; i++ {
		requests = append(requests, []any{
			[]any{nil, nil, []any{[]any{[]any{req.Prompt}}}},
			refs,
			req.Model,
			// The capture carried 2 here, as the edit payload does.
			videoModeText,
			nil,
			[]any{nil, nil, nil, nil, uuid.NewString(), uuid.NewString()},
			nil, nil, nil, nil,
			[]any{[]any{referenceTail}},
		})
	}

	contextBlock := []any{
		nil,
		toolContextID,
		nil, nil, nil,
		req.ProjectID,
		nil, nil, nil, nil,
		[]any{req.CaptchaToken, 1},
	}

	return []any{
		requests,
		contextBlock,
		[]any{uuid.NewString(), videoModeText},
	}
}

// GenerateVideoFromReferences submits a reference-image generation and returns
// the response frames.
//
// Worth stating plainly because it cost a wrong conclusion once: sending an
// `abra_r2v_*` model to RPCIDGenerateVideoImageStart is also accepted and returns
// a media id, so a probe that "works" is not evidence the RPC is right. This is
// the captured one, and its payload is a different shape.
func (c *Client) GenerateVideoFromReferences(ctx context.Context, req ReferenceVideoRequest, opts CallOptions) ([]Frame, error) {
	if req.ProjectID == "" {
		return nil, fmt.Errorf("batchexecute: a project id is required")
	}
	if req.Model == "" {
		return nil, fmt.Errorf("batchexecute: a model is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("batchexecute: a prompt is required")
	}
	if len(req.References) == 0 {
		return nil, fmt.Errorf("batchexecute: at least one reference image is required")
	}
	if req.CaptchaToken == "" {
		return nil, fmt.Errorf("batchexecute: a reCAPTCHA token is required")
	}

	arg := buildReferenceArgument(req)

	if opts.SourcePath == "" {
		opts.SourcePath = "/project/" + req.ProjectID
	}
	rpcID := RPCIDGenerateVideoReferences
	if opts.RPCID != "" {
		rpcID = opts.RPCID
	}
	return c.CallWith(ctx, rpcID, arg, opts)
}

// VideoRPCID picks the RPC a video submission goes to from its conditioning.
//
// Three cases, three ids, and the distinction is not cosmetic: the server
// accepts a submission sent to the wrong one and answers with an empty result
// rather than an error, so a wrong id is indistinguishable from a bad payload
// unless the id itself is asserted. This is why first-frame-only i2v appeared
// broken for so long — the layout was right and the id was nprQif.
func VideoRPCID(req GenerateVideoRequest) string {
	switch {
	case req.EndImage != "":
		return RPCIDGenerateVideoImage
	case req.StartImage != "":
		return RPCIDGenerateVideoImageStart
	default:
		return RPCIDGenerateVideo
	}
}

// Upsampler model keys, as they appear in the model catalog.
const (
	UpscaleModel1080p = "veo_3_1_upsampler_1080p"
	UpscaleModel4K    = "veo_3_1_upsampler_4k"
)

// upscaleRequestLength is the size of the upscale request array. The payload is a
// fixed-length positional array with a long run of nulls, and the model key sits
// at the very end — so the length is part of the contract, not an accident.
//
// Measured from the app's own request: 32 elements, model at index 31. An earlier
// value of 35 (model at 34) was wrong and the server accepted the call without
// acting on it, which is the worst kind of failure — no error, no output.
const upscaleRequestLength = 32

// upscaleModelIndex is where the upsampler model key goes.
const upscaleModelIndex = 31

// UpscaleRequest describes an upscale submission.
type UpscaleRequest struct {
	ProjectID string
	// ContentID is the source asset's content id, which is not the same as its
	// media id. It goes at request[0] and the server derives the new asset's id
	// from it by appending "_upsampled".
	ContentID string
	// MediaID is the source video's media id.
	MediaID string
	// CallID is the uuid in the argument's trailing element, ["<uuid>"], and not
	// the id at request[0] — that position holds the source content id. An empty
	// value gets a fresh uuid, which is what the app sends per submission; set it
	// only to replay a capture exactly.
	CallID string
	// Model is the upsampler key. Empty defaults to the 1080p upsampler.
	Model string
	// CaptchaToken is a reCAPTCHA token minted for the VIDEO_GENERATION action.
	CaptchaToken string
}

// Upscale submits a higher-resolution render of a finished video.
func (c *Client) Upscale(ctx context.Context, req UpscaleRequest, opts CallOptions) ([]Frame, error) {
	if req.ProjectID == "" {
		return nil, fmt.Errorf("batchexecute: a project id is required")
	}
	if req.MediaID == "" {
		return nil, fmt.Errorf("batchexecute: a source media id is required")
	}
	if req.ContentID == "" {
		// The content id is not interchangeable with the media id: it goes at
		// request[0] and the server names the new asset after it.
		return nil, fmt.Errorf("batchexecute: the source asset's content id is required")
	}
	if req.CaptchaToken == "" {
		return nil, fmt.Errorf("batchexecute: a reCAPTCHA token is required")
	}

	arg := buildUpscaleArgument(req)

	if opts.SourcePath == "" {
		opts.SourcePath = "/project/" + req.ProjectID + "/edit/" + req.MediaID
	}
	return c.CallWith(ctx, RPCIDUpscale, arg, opts)
}

// buildUpscaleArgument assembles the upscale payload.
//
// Separated so the shape can be asserted: the array is positional and mostly
// nulls, with the model key at the very end.
func buildUpscaleArgument(req UpscaleRequest) []any {
	model := req.Model
	if model == "" {
		model = UpscaleModel1080p
	}

	// Start all-null and fill the positions the app uses.
	//
	// Every index below is read off the app's own request. They are not
	// interchangeable: the server accepts a payload with the right shape but the
	// wrong indices and simply does nothing, returning a well-formed empty
	// result rather than an error.
	request := make([]any, upscaleRequestLength)
	request[0] = []any{nil, req.ContentID}
	request[2] = 2
	request[4] = []any{nil, req.MediaID, nil, nil, uuid.NewString()}
	request[6] = 2
	request[upscaleModelIndex] = model

	contextBlock := []any{
		nil,
		toolContextID,
		nil, nil, nil,
		req.ProjectID,
		nil, nil, nil, nil,
		[]any{req.CaptchaToken, 1},
	}

	// The third element is a bare uuid in its own array. It is easy to miss —
	// the request is otherwise [request, context] like every other RPC.
	return []any{
		[]any{request},
		contextBlock,
		[]any{upscaleCallID(req)},
	}
}

// upscaleCallID is the trailing uuid on an upscale submission. The app sends a
// fresh one per call; an explicit value is honoured so a capture can be replayed
// byte-for-byte.
func upscaleCallID(req UpscaleRequest) string {
	if req.CallID != "" {
		return req.CallID
	}
	return uuid.NewString()
}

// OperationStatus polls a long-running operation.
func (c *Client) OperationStatus(ctx context.Context, projectID, operationID string) ([]Frame, error) {
	if operationID == "" {
		return nil, fmt.Errorf("batchexecute: an operation id is required")
	}
	return c.CallWith(ctx, RPCIDOperation,
		[]any{nil, nil, []any{[]any{operationID}}},
		CallOptions{SourcePath: "/project/" + projectID})
}

// ImageUpscaleRequest asks for an image at a chosen resolution.
type ImageUpscaleRequest struct {
	ProjectID string
	// MediaID is the image's media id, used to build the source-path.
	MediaID string
	// ContentID is the asset's storage id, which is what the RPC actually takes.
	ContentID string
	// Resolution is the selector sent at position [1]. 1 was observed for the 2K
	// option; the mapping to a scale is not certain.
	Resolution int
	// CaptchaToken is a reCAPTCHA token.
	CaptchaToken string
	// BuildLabel is the `bl` query parameter. The app always sends one, and the
	// generation RPC is routed by it, so leaving it off is a needless difference.
	BuildLabel string
	// SourcePath overrides the derived `source-path`. Empty means
	// "/project/<project>/edit/<media>". Diagnostics only.
	SourcePath string
	// SessionID is the `f.sid` query parameter. The app always sends one.
	SessionID string
}

// UpscaleImage resolves an image at a larger size and returns the assets.
//
// The download menu's "2K / Upscaled" choice runs this before fetching the file,
// which is why no new project asset appears: the image is served bigger, not
// re-created.
func (c *Client) UpscaleImage(ctx context.Context, req ImageUpscaleRequest) ([]GeneratedMedia, error) {
	if req.ProjectID == "" {
		return nil, fmt.Errorf("batchexecute: a project id is required")
	}
	if req.ContentID == "" {
		return nil, fmt.Errorf("batchexecute: a content id is required")
	}
	if req.CaptchaToken == "" {
		return nil, fmt.Errorf("batchexecute: a reCAPTCHA token is required")
	}

	resolution := req.Resolution
	if resolution == 0 {
		resolution = 1
	}

	contextBlock := []any{
		nil,
		toolContextID,
		nil, nil, nil,
		req.ProjectID,
		nil, nil, nil, nil,
		[]any{req.CaptchaToken, 1},
	}

	sourcePath := req.SourcePath
	if sourcePath == "" {
		sourcePath = "/project/" + req.ProjectID
		if req.MediaID != "" {
			sourcePath += "/edit/" + req.MediaID
		}
	}

	frames, err := c.CallWith(ctx, RPCIDImageUpscale,
		[]any{req.ContentID, resolution, contextBlock},
		CallOptions{SourcePath: sourcePath, BuildLabel: req.BuildLabel, SessionID: req.SessionID})
	if err != nil {
		return nil, err
	}

	var out []GeneratedMedia
	for _, frame := range frames {
		out = append(out, ParseGeneratedMedia(frame.Payload)...)
	}
	return out, nil
}

// GeneratedMedia is one asset returned by a generation call.
type GeneratedMedia struct {
	// MediaID identifies the asset within the project.
	MediaID string `json:"media_id"`
	// ContentID is the asset's storage id, used to build download URLs. It is the
	// uuid in the URL path, and for a video it differs from the media id the
	// generation returned.
	ContentID string `json:"content_id,omitempty"`
	// URL is the signed CDN link. It expires, so it should be fetched and stored
	// rather than kept.
	URL string `json:"url"`
	// Kind is "image" or "video".
	Kind string `json:"kind,omitempty"`
}

// mediaURLRe matches a signed asset URL and captures its kind and content id.
//
// Matching the URL directly is more robust than walking the payload by index:
// the asset is nested several levels deep and that nesting has moved before,
// whereas the URL shape has not. It matches both kinds on purpose, because a
// video response carries its poster (an /image/ URL) alongside the asset itself
// (a /video/ URL) — the caller picks by kind.
//
// The id is a uuid plus an optional suffix: a derived asset such as an upscale is
// named "<content-id>_upsampled". Requiring a bare 36-character uuid here made
// every upscaled URL unmatchable, which surfaced as a poll that never completed
// rather than as a parse error.
var mediaURLRe = regexp.MustCompile(
	`https://flow-content\.google/(image|video)/([0-9a-fA-F-]{36}(?:_[a-z]+)?)\?[^"\\\s]*`)

// unicodeEscapeRe matches a \uXXXX escape in a JSON string.
var unicodeEscapeRe = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)

// unescapeJSON resolves the escapes that appear inside the response payload.
//
// This matters more than it looks. The signed media URL is returned with its
// separators escaped — `\u003d` for `=` and `\u0026` for `&` — so a URL pulled
// out before unescaping is truncated at the first escape and its signature is
// cut off. The download then fails with a 403 that looks like an auth problem
// and is really a mangled URL.
func unescapeJSON(text string) string {
	text = strings.ReplaceAll(text, `\/`, "/")
	return unicodeEscapeRe.ReplaceAllStringFunc(text, func(match string) string {
		code, err := strconv.ParseInt(match[2:], 16, 32)
		if err != nil {
			return match
		}
		return string(rune(code))
	})
}

// ParseGeneratedMedia extracts every asset from a generation response.
func ParseGeneratedMedia(payload json.RawMessage) []GeneratedMedia {
	return parseMedia(payload, "")
}

// ParseMediaOfKind extracts only the assets of one kind: "image" or "video".
func ParseMediaOfKind(payload json.RawMessage, kind string) []GeneratedMedia {
	return parseMedia(payload, kind)
}

func parseMedia(payload json.RawMessage, wantKind string) []GeneratedMedia {
	if len(payload) == 0 {
		return nil
	}

	text := unescapeJSON(string(payload))

	seen := make(map[string]bool)
	var out []GeneratedMedia
	for _, match := range mediaURLRe.FindAllStringSubmatch(text, -1) {
		kind, contentID, assetURL := match[1], match[2], match[0]
		if wantKind != "" && kind != wantKind {
			continue
		}
		key := kind + ":" + contentID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, GeneratedMedia{
			MediaID:   contentID,
			ContentID: contentID,
			URL:       assetURL,
			Kind:      kind,
		})
	}
	return out
}

// MediaDetail fetches the signed URLs for one asset.
//
// This is the step that turns a submitted video into something downloadable:
// video is asynchronous, so the generation call returns only an id and the URL
// appears here once the render is ready. The response carries the poster and, for
// a video, the asset itself.
//
// It takes two ids because it needs two, and they are not interchangeable:
// sourceMediaID builds the source-path, and contentID is the argument. Both come
// from the same listing row but are different fields of it. Passing the wrong one
// is not rejected — the call answers 200 with a `null` payload, which is
// indistinguishable from a render that has not finished.
func (c *Client) MediaDetail(ctx context.Context, projectID, sourceMediaID, contentID string) ([]GeneratedMedia, error) {
	if projectID == "" || sourceMediaID == "" || contentID == "" {
		return nil, fmt.Errorf("batchexecute: project, source and content ids are all required")
	}

	frames, err := c.CallWith(ctx, RPCIDMediaDetail, []any{contentID}, CallOptions{
		// The /edit/<media-id> suffix is required; the plain project path is
		// rejected.
		SourcePath: "/project/" + projectID + "/edit/" + sourceMediaID,
	})
	if err != nil {
		return nil, err
	}

	var out []GeneratedMedia
	for _, frame := range frames {
		out = append(out, ParseGeneratedMedia(frame.Payload)...)
	}
	return out, nil
}

// isNonEmptyString reports whether a decoded value is a usable string.
func isNonEmptyString(value any) bool {
	text, ok := value.(string)
	return ok && text != ""
}

// AssetTypeOriginal marks a row that is the asset as it was generated. Other
// codes ("CAI", "CAM") mark derived assets — an upscale, a variant — which share
// the same media id and are not interchangeable for the upscale RPC.
//
// The meaning is inferred from behaviour rather than documentation: for the same
// media id, SPrCad returns an image for the CAE row and returns nothing at all
// for the CAI one.
const AssetTypeOriginal = "CAE"

// ProjectAsset is one entry from a project listing.
type ProjectAsset struct {
	// MediaID is the id the generation returned.
	MediaID string
	// ContentID is the storage id needed to build a download URL. It differs from
	// MediaID for videos, which is why both are kept.
	ContentID string
	// AssetID is the listing's own id for the row.
	//
	// It is a third id, and for some assets it is the one a submission hands back
	// — so a lookup that matches on MediaID alone reports "not in the project
	// listing" for an asset that is sitting right there. Both are accepted
	// wherever an id from outside is matched against the listing.
	AssetID string
	// TypeCode is the listing's own classification: AssetTypeOriginal for the
	// asset as generated, something else for a derived asset.
	TypeCode string
	Title    string
}

// matchesID reports whether a caller-supplied id names this asset.
//
// Any of the three can be the one a submit returned, and the cost of being
// wrong is a wait that spins for its whole timeout before giving up.
func (a ProjectAsset) matchesID(id string) bool {
	return id != "" && (a.MediaID == id || a.AssetID == id || a.ContentID == id)
}

// ParseProjectAssets extracts the entries from a project-listing response.
//
// Each row is
//
//	[content-id, project-id, media-id, type-code, null, detail, ...]
//
// where the type code is "CAE" for an original generation and "CAI" for a derived
// asset such as an upscale. The content id and the media id are different values
// and both are needed: the media id addresses the asset in the UI, and the
// content id is what the media-detail and upscale RPCs take.
//
// Note that several rows can share one media id — an original and its upscales do
// — so a caller looking for "the asset" has to say which it wants.
func ParseProjectAssets(payload json.RawMessage) []ProjectAsset {
	var doc []any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil
	}
	entries := findEntryList(doc, 0)
	if entries == nil {
		return nil
	}

	var out []ProjectAsset
	for _, entry := range entries {
		row, ok := entry.([]any)
		if !ok || !isAssetRow(row) {
			continue
		}

		// Which field is which depends on the shape, and getting it backwards is
		// silent: the ids are all uuids, so a swapped pair still looks like a
		// valid asset and only fails later, at the RPC that wanted the other one.
		//
		//	flat   [content-id, project-id, media-id, type-code, null, detail, …]
		//	nested [media-id, null, null, [title, ts, …, content-id, ?, ts], project]
		//
		// The nested pair was read backwards here — detail[4] was called the media
		// id and detail[5] the content id. detail[4] is the content id, which the
		// upload response proves directly: it reports `media_id` and `content_id`
		// separately, and the content id is the one that lands at detail[4].
		var mediaID, contentID, title, typeCode string

		if detail, ok := row[3].([]any); ok {
			mediaID = stringAt(row, 0)
			contentID = stringAt(detail, 4)
			title = stringAt(detail, 0)
		} else if s, ok := row[3].(string); ok {
			contentID = stringAt(row, 0)
			mediaID = stringAt(row, 2)
			typeCode = s
		}

		if mediaID == "" {
			// Without a media id the row cannot be addressed, and substituting
			// the content id would silently conflate two different identifiers.
			continue
		}
		if contentID == "" {
			contentID = mediaID
		}

		asset := ProjectAsset{
			MediaID:   mediaID,
			ContentID: contentID,
			AssetID:   stringAt(row, 0),
			TypeCode:  typeCode,
		}
		if title != "" {
			asset.Title = title
		} else if detail, ok := row[5].([]any); ok && len(detail) > 1 {
			if t, ok := detail[1].(string); ok {
				asset.Title = t
			}
		}
		out = append(out, asset)
	}
	return out
}

// findEntryList walks a nested payload looking for the first array whose elements
// look like project entries.
//
// Rows are matched on shape — four leading non-empty strings — rather than on a
// path. An earlier version additionally required row[3] to be an array, which no
// row is, so every listing parsed to nothing and the callers that waited on it
// timed out with no error to explain why.
func findEntryList(value any, depth int) []any {
	if depth > 4 {
		return nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	for _, item := range list {
		row, ok := item.([]any)
		if !ok {
			continue
		}
		if isAssetRow(row) {
			return list
		}
	}
	for _, item := range list {
		if found := findEntryList(item, depth+1); found != nil {
			return found
		}
	}
	return nil
}

// isAssetRow reports whether an array is a project-listing row.
//
// Two shapes reach here. The flat one leads with four non-empty strings:
//
//	[content-id, project-id, media-id, type-code, null, detail, ...]
//
// The one the app serves today leads with a single id and nests the pair inside
// the detail array, leaving row[1] and row[2] null:
//
//	[media-id, null, null, [title, ts, null, null, content-id, ?, ts], project-id]
//
// Matching only the first shape found no rows at all in the second — so every
// media id looked absent from its own project, and an upscale could not resolve
// a content id for an asset that was sitting right there in the listing.
//
// **Only detail[4] is required**, not detail[5]. An uploaded image carries a
// content id at detail[4] and nothing at detail[5], because it has no derived
// variant — so demanding both rejected every uploaded asset. That is why
// conditioning on a freshly uploaded image reported it as "not in the project
// listing" while it was in the listing, and why the engine could not do
// image-to-video from an upload at all.
func isAssetRow(row []any) bool {
	if len(row) < 4 || !isNonEmptyString(row[0]) {
		return false
	}
	if isNonEmptyString(row[1]) && isNonEmptyString(row[2]) && isNonEmptyString(row[3]) {
		return true
	}
	detail, ok := row[3].([]any)
	return ok && len(detail) > 4 && isNonEmptyString(detail[4])
}

// stringAt reads a string field, returning "" when it is absent or not a string.
func stringAt(row []any, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	s, _ := row[i].(string)
	return s
}

// ErrAssetNotListed marks the first of the two resolve failures that are worth
// waiting on: the render has not appeared in the project listing yet.
//
// A render in flight produces it, and that is expected. Everything else — a
// media-detail call that answers nothing, a listing that moved, an id that never
// landed — is permanent, and a caller that cannot tell them apart polls a
// ten-second failure for its whole timeout. Use RetryableResolveError rather
// than testing this directly, so the other half of the wait is not forgotten.
var ErrAssetNotListed = errors.New("asset is not in the project listing yet")

// ErrAssetNotReady marks the second half of the same wait: the asset is in the
// listing, but the media-detail call has no downloadable URL for it yet.
//
// Both are what a render in flight looks like — the asset appears before its
// file exists — and both are worth retrying. Only the first used to be marked as
// such, so a render that had been accepted, charged for and listed was abandoned
// the moment its URL lagged, with a log line asking "still rendering?" while
// treating the answer as permanent.
var ErrAssetNotReady = errors.New("asset is listed but has no download URL yet")

// RetryableResolveError reports whether a resolve failure is one that a render
// in flight produces, and is therefore worth waiting on.
//
// There are exactly two, and they arrive in sequence: the asset is missing from
// the listing entirely, then it appears while its URL is still being produced.
// Every other failure — a media-detail call that answers nothing, a listing that
// moved, an id that never landed — is permanent, and polling one of those for the
// full timeout turns a ten-second failure into a seven-minute one.
func RetryableResolveError(err error) bool {
	return errors.Is(err, ErrAssetNotListed) || errors.Is(err, ErrAssetNotReady)
}

// ProjectAssets reads the project listing.
func (c *Client) ProjectAssets(ctx context.Context, projectID string) ([]ProjectAsset, error) {
	frames, err := c.CallWith(ctx, RPCIDProjectContent,
		[]any{"projects/" + projectID, nil, nil, nil, []any{1}},
		CallOptions{SourcePath: "/project/" + projectID})
	if err != nil {
		return nil, err
	}

	var out []ProjectAsset
	for _, frame := range frames {
		out = append(out, ParseProjectAssets(frame.Payload)...)
	}
	return out, nil
}

// ResolveVideoURL returns the signed download URL for a submitted video.
//
// Video is asynchronous, so this is a two-step lookup: find the asset in the
// project listing, then ask the media-detail RPC for the signed URL. It returns
// an error when the render is not ready yet, which the caller is expected to
// retry.
func (c *Client) ResolveVideoURL(ctx context.Context, projectID, mediaID string) (string, error) {
	assets, err := c.ProjectAssets(ctx, projectID)
	if err != nil {
		return "", err
	}

	var found *ProjectAsset
	for i := range assets {
		if assets[i].matchesID(mediaID) {
			found = &assets[i]
			break
		}
	}
	if found == nil {
		return "", fmt.Errorf("batchexecute: %s: %w", mediaID, ErrAssetNotListed)
	}

	// The media-detail RPC is addressed by the asset's **content id**, which is
	// what the parser now puts in ContentID for both listing shapes.
	//
	// This is the defect that cost the most. The nested listing's id pair was read
	// backwards, so ContentID held the wrong uuid — and the call does not reject a
	// wrong uuid, it answers 200 with a `null` payload. That is indistinguishable
	// from a render that has not finished, so a completed video looked like one
	// that never completed and the poll waited out its whole timeout.
	media, err := c.MediaDetail(ctx, projectID, found.MediaID, found.ContentID)
	if err != nil {
		return "", err
	}
	for _, item := range media {
		if item.Kind == "video" && item.URL != "" {
			return item.URL, nil
		}
	}
	return "", fmt.Errorf("batchexecute: no video URL for %s yet (still rendering?): %w",
		mediaID, ErrAssetNotReady)
}

// ParseFrames walks a batchexecute response.
//
// The body is a length-prefixed stream: a byte count on its own line, then that
// many bytes of JSON, repeated. Lines that are not numbers are skipped, and any
// line beginning with [[ that fails to parse is ignored rather than aborting —
// the stream carries a preamble and an epilogue that are not frames.
func ParseFrames(body string) ([]Frame, error) {
	var frames []Frame

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}

		var wrapper [][]any
		if err := json.Unmarshal([]byte(line), &wrapper); err != nil {
			continue
		}

		for _, item := range wrapper {
			if len(item) < 3 {
				continue
			}
			id, ok := item[0].(string)
			if !ok || id != "wrb.fr" {
				continue
			}

			frame := Frame{}
			if rpc, ok := item[1].(string); ok {
				frame.RPCID = rpc
			}
			if payload, ok := item[2].(string); ok && payload != "" {
				frame.Payload = json.RawMessage(payload)
			}
			frames = append(frames, frame)
		}
	}

	if len(frames) == 0 {
		return nil, fmt.Errorf("batchexecute: the response contained no wrb.fr frames")
	}
	return frames, nil
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}

// UpscaleImageRaw is UpscaleImage but returns the raw frames.
//
// Exposed for diagnostics: the parsed view only surfaces URLs, and when a
// response carries none the raw shape is what shows where the value actually is.
func (c *Client) UpscaleImageRaw(ctx context.Context, req ImageUpscaleRequest) ([]json.RawMessage, error) {
	resolution := req.Resolution
	if resolution == 0 {
		resolution = 1
	}
	contextBlock := []any{
		nil, toolContextID, nil, nil, nil, req.ProjectID, nil, nil, nil, nil,
		[]any{req.CaptchaToken, 1},
	}
	sourcePath := req.SourcePath
	if sourcePath == "" {
		sourcePath = "/project/" + req.ProjectID
		if req.MediaID != "" {
			sourcePath += "/edit/" + req.MediaID
		}
	}

	frames, err := c.CallWith(ctx, RPCIDImageUpscale,
		[]any{req.ContentID, resolution, contextBlock},
		CallOptions{SourcePath: sourcePath, BuildLabel: req.BuildLabel, SessionID: req.SessionID})
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Payload)
	}
	return out, nil
}

// UpscaleImageRawBody returns the verbatim response body for the image-upscale
// RPC. Diagnostics only; see CallRaw for why the parsed view is not enough.
func (c *Client) UpscaleImageRawBody(ctx context.Context, req ImageUpscaleRequest) (string, error) {
	resolution := req.Resolution
	if resolution == 0 {
		resolution = 1
	}
	contextBlock := []any{
		nil, toolContextID, nil, nil, nil, req.ProjectID, nil, nil, nil, nil,
		[]any{req.CaptchaToken, 1},
	}
	sourcePath := req.SourcePath
	if sourcePath == "" {
		sourcePath = "/project/" + req.ProjectID
		if req.MediaID != "" {
			sourcePath += "/edit/" + req.MediaID
		}
	}
	return c.CallRaw(ctx, RPCIDImageUpscale,
		[]any{req.ContentID, resolution, contextBlock},
		CallOptions{SourcePath: sourcePath, BuildLabel: req.BuildLabel, SessionID: req.SessionID})
}
