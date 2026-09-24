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
	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/httpx"
)

// EndpointPath is the Flow app's batchexecute path.
const EndpointPath = "/_/AiSandboxAngularFrontend/data/batchexecute"

// Origin is the host the RPC is served from and bound to.
// Origin is a var rather than a const so a test can point the client at a local
// server. Without that, exercising the priming handshake means talking to
// Google — which is neither hermetic nor free, and the handshake is exactly
// where a retry can resend something it should not.
var Origin = "https://flow.google.com"

// RPCIDProjectList returns the account's projects.
//
// This was recorded as `WuwhI` for a long time, and that was wrong: `WuwhI` is
// the generation-status RPC and answers `null` or `[]` to everything else,
// including this RPC's own payload. The mistake cost the feature — with the id
// believed known, the absence of a project listing read as "the API cannot list
// projects" and the browser tab stayed the only way to learn a project id.
//
// The real id answers with one row per project:
//
//	[["<project-id>",
//	  ["12 Jul, 18:11", null, [<unix-sec>, <nsec>], "<thumbnail-url>", "<asset-id>"]],
//	 ["<project-id>", ["11 Jul, 11:38", null, [<unix-sec>, <nsec>]]]]
//
// A project with no assets carries only the first three detail slots, which is
// why the thumbnail and the trailing id are optional in Project.
//
// Note the argument's `projects/*` — the listing is expressed as a resource
// pattern, not as a call about a project. That is also why it needs no project
// context and works before one is known.
//
// The same id was also recorded here as `RPCIDAssetPage`, "pages through a
// project's assets", with this identical payload. It was never called under that
// name, so the label went unverified — the second time this one id was
// mislabelled, after `WuwhI`. The third argument is a page cursor, null on the
// first page; nothing here has needed a second page yet.
//
// An empty list is a real answer, not an error, and it is the answer whenever
// the account has no projects. Checked against the app itself: with the listing
// empty, flow.google.com/u/0/ shows only "New project" — the two agree, so a
// caller must treat empty as "none" and not as "the call failed".
const RPCIDProjectList = "UpteDb"

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

	// RPCIDOperation polls a long-running operation by id.
	//
	// Payload [null, null, [[<operation-id>]]]. Used to follow an upscale or an
	// edit to completion.
	RPCIDOperation = "jwpduf"

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

	// RPCIDProjectCreate creates a project and returns its id.
	//
	// The app never calls this, which is why it went unfound for so long: clicking
	// New project mints a uuid in the page and navigates, with no request beside the
	// click. The id it mints is accepted for generation, so the app gets away with
	// never registering anything — but such a project is invisible to the listing.
	//
	// This call is what makes a project *visible*. Captured by instrumenting the
	// page's fetch and XHR rather than polling the extension's event buffer, which
	// rotates within seconds and lost the request every time.
	//
	// The argument, decoded from that capture:
	//
	//	["projects/*", [null, ["<label>"]], [null, 22]]
	//
	// `22` is the same constant the context block of every generation carries, and
	// is likely a tool or resource type. The label is a display name — the app uses
	// the local date and time it was created, and the listing shows it verbatim.
	//
	// The response is `[<project-id>, [<label>]]` — so the caller is told the id it
	// has just been given, and does not have to re-list to find it.
	RPCIDProjectCreate = "jHPbke"

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
	{RPCIDProjectList, "the account's projects", `["projects/*",21,null,null,null,null,[1]]`},
	{RPCIDGenerate, "SUBMIT A GENERATION (needs a session-context blob)", `<see RPCIDGenerate doc comment>`},
	{RPCIDGenerationStatus, "generation status / telemetry", `[[["MEDIA_GENERATION",["<uuid>",[<sec>,<ns>],null,<env flags>]]]]`},
	{RPCIDModelCatalog, "model catalog", `[]`},
	{RPCIDAvailableModels, "available models", `[]`},
	{RPCIDProjectContent, "project media + generation history", `["projects/<project-id>",null,null,null,[1]]`},
	{RPCIDToolProject, "tool-scoped project metadata", `["tools/PINHOLE/projects/<project-id>"]`},
	{RPCIDMediaCatalog, "project asset catalog", `[1]`},
	{RPCIDProjectMeta, "project metadata", `["<project-id>"]`},
	{RPCIDAssets, "project assets", `[]`},
	{RPCIDFlags, "feature flags", `[14]`},
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

	// subSlots caps how many submission calls may be in flight at once, and
	// subGap spaces them apart. Both zero mean no limit, which is the default:
	// the limits are opted into by the engine (see SetSubmissionLimits) rather
	// than imposed on every caller, so a test or a diagnostic client is not
	// silently slowed down or serialised.
	//
	// They exist because this is the only local defence against the pattern
	// that plausibly triggers PUBLIC_ERROR_UNUSUAL_ACTIVITY. A refusal has no
	// status to react to — it is HTTP 200 with a reason in the frame — so there
	// is nothing to back off *from* after the fact. The only place to act is
	// before the call goes out.
	subSlots chan struct{}
	subGap   time.Duration
	// subMu guards subNext, the earliest instant the next submission may go out.
	// Reserved rather than merely read, so concurrent callers space out instead
	// of all measuring the same gap and firing together.
	subMu   sync.Mutex
	subNext time.Time
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
	// Error is the reason the server gave for refusing the call, when it gave
	// one.
	//
	// It is a sibling of the data element rather than part of it, which is why
	// it went unread for so long. A refusal is still HTTP 200 and still a
	// `wrb.fr` frame, but its data slot is null and the reason sits two levels
	// down in the slot after it:
	//
	//	["wrb.fr","ogiZ0b",null,null,null,
	//	 [7,null,[["type.googleapis.com/google.rpc.ErrorInfo",
	//	           ["PUBLIC_ERROR_UNUSUAL_ACTIVITY"]]]],"generic"]
	//
	// A parser that reads only the data element therefore sees a frame carrying
	// nothing, and reports a refusal as a silent no-op. Those are different
	// problems — one needs the assessment fixed, the other needs a model or a
	// project fixed — so they must not arrive as the same message.
	Error string
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
	// RefreshCaptcha re-mints the captcha token a payload carries.
	//
	// **A reCAPTCHA token is single-use**, so a retry that resends the previous
	// payload presents a spent one — and the server answers an empty frame
	// rather than an error, which is why this was invisible for so long. When
	// this is set, every attempt after the first asks for a fresh token and
	// rebuilds the payload with it.
	//
	// It matters on two paths, not one. The priming handshake resends when the
	// server answers with an anti-CSRF token, and a session refresh resends
	// after a 401. Both went out with the token the first attempt had already
	// spent. The second is rare; the first happens on every boot, because the
	// seeded `at` is stale by then — so the first generation after a restart
	// failed, every time.
	//
	// Nil means "this call carries no captcha", and a resend is safe.
	RefreshCaptcha func(ctx context.Context) (string, error)
	// EscalateCaptcha mints a token from a higher-scoring strategy than the one
	// that produced a refused one.
	//
	// It is asked for only after a call has been refused with
	// ReasonUnusualActivity, and only once — the point is to answer "the
	// assessment rejected this token" with a better token rather than with an
	// explanation, and a second rejection means the assessment is refusing the
	// client rather than the token.
	//
	// Nil is the default and means the refusal is final, which is the behaviour
	// every caller had before this existed. It is also what a run with no
	// browser attached must get: there is no page to ask, so the refusal stands
	// and `auto` stays browser-free.
	EscalateCaptcha func(ctx context.Context) (string, error)
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
// CallWith performs a call whose payload does not change between attempts.
//
// A call that carries a captcha token must use the builder form instead — see
// call — because a reCAPTCHA token is single-use and a resend presents a spent
// one.
func (c *Client) CallWith(ctx context.Context, rpcID string, payload any, opts CallOptions) ([]Frame, error) {
	return c.call(ctx, rpcID, opts, func(string) (any, error) { return payload, nil })
}

// call performs one batchexecute call, building the payload per attempt.
//
// build receives the captcha token to use: empty means "the one you already
// have", and a non-empty value is a freshly minted replacement. That is what
// makes a retry safe. The priming handshake resends when the server answers with
// an anti-CSRF token, and a session refresh resends after a 401 — and both used
// to go out with the token the first attempt had already spent, which the server
// answers with an empty frame rather than an error.
func (c *Client) call(ctx context.Context, rpcID string, opts CallOptions,
	build func(token string) (any, error)) ([]Frame, error) {

	// Submissions are paced, reads are not. See enterSubmission for why that
	// distinction is the point rather than a convenience.
	release, err := c.enterSubmission(ctx, rpcID)
	if err != nil {
		return nil, err
	}
	defer release()

	auth, err := c.Authorization()
	if err != nil {
		return nil, err
	}

	// The envelope is [[[rpcid, "<args>", null, "generic"]]]. The arguments are a
	// string, not an object — that nesting is part of the protocol.
	envelopeFor := func(ctx context.Context, retry bool) (string, error) {
		token := ""
		if retry && opts.RefreshCaptcha != nil {
			fresh, err := opts.RefreshCaptcha(ctx)
			if err != nil {
				return "", fmt.Errorf(
					"batchexecute: %s is retrying and could not obtain a fresh captcha token: %w",
					rpcID, err)
			}
			token = fresh
		}

		payload, err := build(token)
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
		out, err := json.Marshal(envelope)
		if err != nil {
			return "", fmt.Errorf("batchexecute: could not encode the envelope: %w", err)
		}
		return string(out), nil
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

	resp, err := c.post(ctx, rpcID, envelopeFor, query.Encode(), auth)
	if err != nil {
		return nil, err
	}
	return ParseFrames(resp.Text())
}

/* ------------------------------------------------------------------ *
 * Pacing the calls that spend credits
 * ------------------------------------------------------------------ */

// submissionRPCs are the calls that cost the account something.
//
// They are the ones Flow throttles, and the ones worth pacing: a refused read
// costs nothing and can simply be repeated, whereas a refused submission has
// either been charged or has spent the account's goodwill. So the limiter
// applies here and nowhere else — reads must stay cheap and immediate, because
// polling depends on them and a poll that is spaced two seconds apart turns a
// seven-minute wait into a much longer one.
//
// maseQ (the upload) is deliberately absent. It is a write, but it spends no
// credits and it happens once before a submission, so pacing it would only add
// latency to the one step that is already a browser round trip.
var submissionRPCs = map[string]bool{
	RPCIDGenerate:                true, // ogiZ0b — image generation
	RPCIDGenerateVideo:           true, // YhhmEf — text to video
	RPCIDGenerateVideoImage:      true, // nprQif — first and last frame
	RPCIDGenerateVideoImageStart: true, // eb1hJf — start frame only
	RPCIDGenerateVideoReferences: true, // MZZa6b — reference images
	RPCIDVideoEdit:               true, // jIps6 — video edit
}

// SetSubmissionLimits caps how many submission calls may be in flight and the
// minimum gap between them.
//
// A zero or negative value for either means no limit, and the zero value of a
// Client means no limit for both — so a client built without this call behaves
// exactly as it did before the limiter existed. That is deliberate: the limits
// are a policy the engine opts into, not a property of the transport, and a
// test or a diagnostic client should not be serialised behind them.
func (c *Client) SetSubmissionLimits(maxInFlight int, gap time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if maxInFlight > 0 {
		c.subSlots = make(chan struct{}, maxInFlight)
	} else {
		c.subSlots = nil
	}
	c.subGap = gap
	c.subNext = time.Time{}
}

// enterSubmission waits for a submission slot and for the spacing to elapse,
// and returns the release function the caller must defer.
//
// Reads — and any rpc that is not in submissionRPCs — get a no-op release
// immediately, so this costs them nothing.
//
// The slot is taken before the gap is waited out rather than after. That makes
// the cap a real cap: with the wait outside the slot, a burst of callers would
// all hold nothing and all be released at once.
//
// Both waits honour ctx, and a cancelled wait gives the slot back — otherwise a
// client whose caller gave up would leak capacity it will never release.
func (c *Client) enterSubmission(ctx context.Context, rpcID string) (func(), error) {
	noop := func() {}

	if !submissionRPCs[rpcID] {
		return noop, nil
	}

	c.mu.Lock()
	slots := c.subSlots
	gap := c.subGap
	c.mu.Unlock()

	if slots == nil && gap <= 0 {
		return noop, nil
	}

	if slots != nil {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil, fmt.Errorf("batchexecute: waiting to submit %s: %w", rpcID, ctx.Err())
		}
	}
	release := func() {
		if slots != nil {
			<-slots
		}
	}

	if gap <= 0 {
		return release, nil
	}

	// Reserve this call's slot in the schedule rather than reading the last
	// send time. Two callers arriving together would otherwise both measure the
	// same gap and go out together, which is the burst the spacing exists to
	// prevent.
	c.subMu.Lock()
	now := time.Now()
	next := now
	if c.subNext.After(now) {
		next = c.subNext
	}
	c.subNext = next.Add(gap)
	c.subMu.Unlock()

	if wait := time.Until(next); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			release()
			return nil, fmt.Errorf("batchexecute: waiting to space out %s: %w", rpcID, ctx.Err())
		}
	}
	return release, nil
}

// UnauthorizedHandler refreshes a session the server has rejected.
//
// It returns the fresh cookie jar so the client can swap it in, or nil to keep
// the current one. An error means the refresh itself failed and the call should
// not be retried.
type UnauthorizedHandler func(ctx context.Context) (*cookiejar.Jar, error)

// SetUnauthorizedHandler installs the session refresher.
//
// Without one, a stale session is terminal for the client: a credit read starts
// answering 401 and keeps answering 401 until an operator re-syncs from the
// browser with `flow-go bridge`.
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
func (c *Client) post(ctx context.Context, rpcID string,
	envelope func(ctx context.Context, retry bool) (string, error),
	rawQuery, auth string) (*httpx.Response, error) {
	fullURL := c.endpointURL(rawQuery)
	headers := c.requestHeaders(auth)

	// sent counts every request this call has made, across both retry paths, so
	// the builder knows whether it is building the first payload or rebuilding
	// one whose single-use token has already been spent.
	sent := 0

	// send runs the priming sequence: the first attempt usually comes back 400
	// carrying the anti-CSRF token, which the second echoes back.
	send := func() (*httpx.Response, error) {
		for attempt := 1; attempt <= 2; attempt++ {
			envelopeJSON, err := envelope(ctx, sent > 0)
			if err != nil {
				return nil, err
			}
			sent++

			body := url.Values{}
			body.Set("f.req", envelopeJSON)
			if token := c.Token(); token != "" {
				body.Set("at", token)
			}

			// QUIC is off and the header order is Chrome's, always. Both used to
			// be settable for the image-upscale diagnosis — the app's own
			// batchexecute calls go out over h3, and a faithful replay has to
			// supply the header order as well as the headers — and neither made
			// any difference to that rejection. The setters went with the
			// upscale code; the values are what they always resolved to.
			resp, err := c.hc.Do(ctx, &httpx.Request{
				Method:      "POST",
				URL:         fullURL,
				Body:        []byte(body.Encode()),
				Headers:     headers,
				HeaderOrder: httpx.ChromeHeaderOrder,
				Cookies:     c.jar.HeaderForDomain(Origin + "/"),
				DisableQUIC: true,
				AllowQUIC:   false,
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
					// Worth a line: this resend is the path that used to
					// present a spent captcha token, and the symptom it
					// produced was a generation that quietly did nothing. The
					// retry rebuilds its payload, so a call that carries a
					// captcha is minting a fresh one here.
					log.Printf("batchexecute: %s primed; retrying", rpcID)
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
	// AspectRatio is the friendly name or ratio alias of the output aspect:
	// "square"/"1:1", "portrait"/"9:16", "landscape"/"16:9", "3:4" or "4:3". It
	// is resolved via config.ImageAspectValue and written at request[4].
	//
	// Empty does not mean "send nothing" here, the way it does on the video
	// path, because this slot carries a value in every working submission
	// already. Empty means "the default", which is what that value encodes.
	AspectRatio string
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

// imageAspectDefault is what position [4] of an image request carries when the
// caller names no aspect.
//
// That slot was a constant called generateMode for a long time, documented as
// "observed as 3 on every captured call; its meaning is unknown, so it is
// reproduced rather than guessed at". That was the honest reading of one value
// seen across a handful of submissions, and it is now understood to be the
// aspect ratio: 3 is IMAGE_ASPECT_RATIO_LANDSCAPE in the image protobuf enum
// (see config.ImageAspectRatios), and every capture was taken at the composer's
// default. That is what makes the old observation and the mapping agree rather
// than merely coexist — a mode flag that happened to equal the landscape value
// would be a coincidence; an aspect slot carrying the default aspect is not.
//
// The value stays 3 rather than becoming the mapping's square (1). Three is what
// a live submission is known to accept, and moving it would silently re-aspect
// every image rendered by a caller who never mentioned aspect at all — a change
// no one asked for, in the one direction (square crops) that cannot be undone
// after the fact.
const imageAspectDefault = 3

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
	// Refused here rather than defaulted in the builder, for the same reason the
	// video path refuses one: the server does not reject an aspect it does not
	// recognise, it accepts the submission and returns nothing, so a bad value
	// would be diagnosed from an empty result rather than from its cause. This
	// runs before the captcha token is spent.
	if req.AspectRatio != "" {
		if _, ok := config.ImageAspectValue(req.AspectRatio); !ok {
			return nil, fmt.Errorf("batchexecute: unknown image aspect %q; use one of %s",
				req.AspectRatio, strings.Join(config.ImageAspectNames, ", "))
		}
	}

	seed := req.Seed
	if seed == 0 {
		seed = time.Now().UnixNano() % 1000000000
	}

	if opts.SourcePath == "" {
		opts.SourcePath = "/project/" + req.ProjectID
	}

	return c.call(ctx, RPCIDGenerate, opts, func(token string) (any, error) {
		return buildGenerateArgument(req.withCaptcha(token), seed), nil
	})
}

// GenerateMedia submits a generation and returns the assets it produced.
//
// A successful call returns the asset inline, so unlike the legacy REST path
// there is no polling step for images.
func (c *Client) GenerateMedia(ctx context.Context, req GenerateRequest, opts CallOptions) ([]GeneratedMedia, error) {
	submit := func(token string) ([]Frame, error) {
		return c.Generate(ctx, req.withCaptcha(token), opts)
	}

	frames, err := submit("")
	if err != nil {
		return nil, err
	}
	if frames, err = retryIfEmpty(ctx, opts, frames, carriesNoMedia, submit); err != nil {
		return nil, err
	}
	return parseMediaFrames(frames), nil
}

// retryIfEmpty resubmits a generation that Flow accepted and answered with
// nothing.
//
// **The first generation after a boot comes back empty** — HTTP 200, no error,
// no asset, and about a second where a real one takes twenty-odd. The same
// submission sent again succeeds, and the captcha token is the only thing that
// differs between the two, so the retry mints a fresh one rather than resending
// the payload it already spent.
//
// empty reports whether a response carried nothing, and each RPC parses its own
// shape, so the caller supplies it. submit is given the token to use: an empty
// string means "the one already in the request", which is what the first attempt
// passes.
//
// Three things it deliberately does not do. It does not retry a call with no
// refresher — there is no token to re-mint, and such a call carries no captcha,
// so an empty answer is the caller's to interpret. It does not retry twice; one
// extra attempt is enough to cover the boot case and a second would only spend
// credits on a genuine failure. And it does not invent a cause for an empty
// answer that carried none — the empty frames are returned as they are, so the
// caller reports what actually happened.
//
// What it does now do is stop conflating two answers that only look alike. An
// empty frame with no reason is the silent no-op above; an empty frame carrying
// an ErrorInfo is the server stating why it refused, and that is returned as a
// RejectedError so the caller can report the reason instead of the symptom.
// Before, both arrived as "no frames", and a submission the assessment had
// thrown out was answered with a hint about the model enum and the project.
//
// A mint *failure* is a third thing, and is returned as itself. A provider that
// is deliberately off never reaches here: it answers with an empty token and no
// error, and such a call carries no refresher at all.
func retryIfEmpty(ctx context.Context, opts CallOptions, frames []Frame,
	empty func([]Frame) bool,
	submit func(token string) ([]Frame, error)) ([]Frame, error) {

	if opts.RefreshCaptcha == nil || !empty(frames) {
		return frames, nil
	}

	token, err := opts.RefreshCaptcha(ctx)
	if err != nil {
		// Fail rather than returning the empty frames as though they were the
		// answer. An empty response is what an upstream refusal looks like, and
		// handing it back without the reason that explains it is how "nothing
		// came back" became a symptom with no cause attached.
		return nil, fmt.Errorf("captcha mint failed: %w", err)
	}

	log.Printf("batchexecute: nothing came back; retrying once with a fresh captcha token")

	retried, err := submit(token)
	if err != nil {
		return nil, err
	}
	if !empty(retried) {
		return retried, nil
	}

	log.Printf("batchexecute: the retry came back empty too")

	// A refusal that survived the retry is not the silent no-op this function
	// exists to paper over. The retry is worth making either way — a spent or
	// stale token is the common cause and a fresh one clears it — but when the
	// second answer carries a reason as well, the reason is the answer, and
	// reporting "empty" would hide it for a third time.
	//
	// The retry's reason wins when there is one: it is the later and more
	// specific of the two, and a first attempt refused over a stale token says
	// nothing about why the second was refused.
	reason := UpstreamError(retried)
	if reason == "" {
		reason = UpstreamError(frames)
	}
	if reason == "" {
		return retried, nil
	}

	// A refusal by the assessment is the one case worth a third attempt.
	//
	// Both attempts so far minted over the transport, so both presented a token
	// of the same quality — and if that quality is what is being refused,
	// repeating it only spends another round trip to learn the same thing. A
	// page-minted token is a genuinely different answer, so it is asked for
	// once, and only here.
	//
	// Every other reason is final. A wrong model, or a project belonging to
	// another account, is not fixed by minting again.
	if opts.EscalateCaptcha != nil && reason == ReasonUnusualActivity {
		log.Printf("batchexecute: the assessment refused the token; asking a page for one instead")

		escalated, mintErr := opts.EscalateCaptcha(ctx)
		if mintErr != nil || escalated == "" {
			// No page to ask, or it could not mint. The refusal stands — it is
			// still the answer, and reporting the mint failure instead would
			// replace a stated reason with a vaguer one.
			//
			// An empty token is treated as a failure rather than passed to
			// submit: submit("") means "keep the token you already have", so
			// accepting one here would resend the spent token and spend another
			// attempt on the answer that was just refused.
			log.Printf("batchexecute: no higher-scoring token available (%v); the refusal stands",
				mintErr)
			return nil, &RejectedError{Reason: reason, Frames: len(retried)}
		}

		third, err := submit(escalated)
		if err != nil {
			return nil, err
		}
		if !empty(third) {
			log.Printf("batchexecute: the page-minted token was accepted")
			return third, nil
		}

		// Refused again, and with a better token this time. The assessment is
		// refusing the client rather than the token, and the second reason is
		// the more specific of the two.
		if escalatedReason := UpstreamError(third); escalatedReason != "" {
			reason = escalatedReason
		}
	}

	return nil, &RejectedError{Reason: reason, Frames: len(retried)}
}

// carriesNoMedia reports whether a response holds no asset URLs.
func carriesNoMedia(frames []Frame) bool {
	return len(parseMediaFrames(frames)) == 0
}

// carriesNoMediaIDs reports whether a response holds no asset ids.
//
// The video RPCs answer in a different shape from the image one — a listing row
// per asset rather than a signed URL — so they need their own test rather than a
// shared one. Using the URL test on a video response would report every
// submission as empty and retry all of them.
func carriesNoMediaIDs(frames []Frame) bool {
	for _, frame := range frames {
		if len(ParseGeneratedMediaIDs(frame.Payload)) > 0 {
			return false
		}
	}
	return true
}

// parseMediaFrames pulls every asset out of a response's frames.
func parseMediaFrames(frames []Frame) []GeneratedMedia {
	var out []GeneratedMedia
	for _, frame := range frames {
		out = append(out, ParseGeneratedMedia(frame.Payload)...)
	}
	return out
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
// videoAspectSlot resolves an aspect name into the value the video payload
// carries at request[3].
//
// Empty answers nil, which leaves the slot exactly as every capture has it, so
// the existing fixtures stay byte-identical and a caller who never mentions
// aspect is unaffected. An unrecognised name also answers nil.
//
// This function is total on purpose: the payload builder cannot fail
// mid-construction, and an unknown name is refused by GenerateVideo before any
// work is done, so the fallback here is unreachable from the CLI. It is nil
// rather than a default because a silently-substituted aspect is the failure
// this slot's whole history is about.
func videoAspectSlot(aspect string) any {
	if strings.TrimSpace(aspect) == "" {
		return nil
	}
	value, ok := config.VideoAspectValue(aspect)
	if !ok {
		return nil
	}
	return value
}

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
			// The aspect ratio. Every capture carries a bare null here, because
			// every capture was taken at the composer's default — and null is also
			// what the server reads as landscape, so an absent flag and an explicit
			// "landscape" are two different payloads that render the same way.
			//
			// This slot was a hardcoded nil until the bundle's own mapping showed
			// what belongs in it: the video protobuf class has a setter that writes
			// one field, and that field is this one. A wrong value here is not
			// rejected — the server accepts the submission and renders nothing —
			// which is why the value is resolved from a table rather than guessed.
			videoAspectSlot(req.AspectRatio),
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
// imageAspectSlot resolves an aspect name into the value an image request
// carries at index 4.
//
// Empty answers the default, not nil: this slot holds a value in every
// submission known to work, and null is not one of the things it has been
// observed to accept. That is the opposite of the video path, where the slot is
// null in every capture and an absent flag has to leave it that way.
//
// An unrecognised name also answers the default, for the same reason
// videoAspectSlot falls back to nil: the builder is total, and GenerateMedia
// refuses an unknown name before any work is done.
func imageAspectSlot(aspect string) any {
	if strings.TrimSpace(aspect) == "" {
		return imageAspectDefault
	}
	value, ok := config.ImageAspectValue(aspect)
	if !ok {
		return imageAspectDefault
	}
	return value
}

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
		// The aspect ratio, at index 4. Unset leaves this at the value a live
		// submission is known to accept, so a caller who never mentions aspect
		// sends exactly the bytes they sent before this slot was understood.
		imageAspectSlot(req.AspectRatio),
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

// CreditsFrames returns the raw credits response frames, unparsed.
//
// Diagnostics only. The parsed balance cannot answer whether the response carries
// anything else — a tier, an allocation, a reset time — and the field names are
// undocumented, so the only way to know is to look. It exists because the account
// tier is read from a different endpoint that is `authuser`-blind, and whether this
// one could replace it is a question about the payload, not about the balance.
func (c *Client) CreditsFrames(ctx context.Context, opts CallOptions) ([]json.RawMessage, error) {
	frames, err := c.CallWith(ctx, RPCIDCredits, nil, opts)
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(frames))
	for _, frame := range frames {
		out = append(out, frame.Payload)
	}
	return out, nil
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

// Project is one entry in the account's project list.
//
// Tagged snake_case because it is serialised straight out by the HTTP API, and
// the rest of that surface is snake_case.
type Project struct {
	// ID is the project id — the same value the app puts in an editor URL, and
	// the same one every generation call carries in its context block.
	ID string `json:"id"`
	// Label is the project's display name. The app sets it to the local date and
	// time of creation, and the project list shows it verbatim.
	Label string `json:"label,omitempty"`
	// Modified is when the listing says the project last changed. Zero when the
	// listing gave no timestamp, which it may not.
	Modified time.Time `json:"modified"`
	// Thumbnail is the project's poster. Empty for a project with no assets,
	// which is how a fresh project looks.
	Thumbnail string `json:"thumbnail,omitempty"`
	// LastAssetID is the id the listing carries after the poster. Empty
	// alongside an empty Thumbnail.
	LastAssetID string `json:"last_asset_id,omitempty"`
}

// projectListArg is the argument for RPCIDProjectList, measured from a live call.
//
// `projects/*` is the resource pattern that selects the listing. The `21` was
// constant across captures and is presumably a page size or a resource type; the
// trailing [1] is a flag whose meaning is not established, and is carried
// verbatim because dropping it was not tried and this call works.
var projectListArg = []any{"projects/*", 21, nil, nil, nil, nil, []any{1}}

// CreateProject creates a Flow project and returns it.
//
// This is the browser-free way to get a project that the app will also see. A
// minted uuid works for generation but never appears in the listing; a project
// made here does, because this is the call the listing is built from.
//
// The label is a display name and is shown as-is in the project list. An empty
// label gets the app's own convention, the local date and time, so a project
// created from a script is not conspicuous among ones created by hand.
func (c *Client) CreateProject(ctx context.Context, label string) (Project, error) {
	if strings.TrimSpace(label) == "" {
		label = time.Now().Format("Jan 2 - 15:04")
	}

	arg := []any{"projects/*", []any{nil, []any{label}}, []any{nil, projectToolCode}}

	frames, err := c.CallWith(ctx, RPCIDProjectCreate, arg, CallOptions{
		// Captured from the app's own call, which is made from the home page
		// rather than from inside a project — the project does not exist yet.
		SourcePath: "/u/0/",
	})
	if err != nil {
		return Project{}, err
	}

	for _, frame := range frames {
		var payload any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			continue
		}
		row, ok := payload.([]any)
		if !ok || len(row) == 0 {
			continue
		}
		id, ok := row[0].(string)
		if !ok || !looksLikeUUID(id) {
			continue
		}
		project := Project{ID: id, Modified: time.Now()}
		if detail, ok := row[1].([]any); ok {
			project.Label = stringAt(detail, 0)
		}
		return project, nil
	}
	return Project{}, fmt.Errorf("batchexecute: the create response carried no project id")
}

// projectToolCode is the trailing constant in the create argument, and the same
// `22` that every generation's context block carries at the same position.
const projectToolCode = 22

// ProjectList returns the projects belonging to the account this client acts as.
//
// This is how a project id is learned without a browser. Before it, the only
// route was to open an editor tab and read the id off the URL. The listing means
// a run no longer has to navigate anywhere to find out where it is, and
// CreateProject above covers the account that has nothing to list yet.
//
// Ordering is the server's, which is most-recently-modified first; the caller
// that wants "the project to use" can take the first entry rather than sorting.
func (c *Client) ProjectList(ctx context.Context, opts CallOptions) ([]Project, error) {
	frames, err := c.CallWith(ctx, RPCIDProjectList, projectListArg, opts)
	if err != nil {
		return nil, err
	}
	for _, frame := range frames {
		var payload any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			continue
		}
		if projects := parseProjectList(payload); len(projects) > 0 {
			return projects, nil
		}
	}
	return nil, nil
}

// parseProjectList pulls the rows out of the listing response.
//
// The response is a list of `[id, detail]` pairs nested one level deep, and an
// account with no projects answers with an empty list rather than an error — so
// a caller must treat "no rows" as a valid answer and not as a failure.
//
// Rows are selected by shape rather than by position: the id is a uuid and the
// detail is the array beside it. A row that does not look like that is skipped,
// which is what keeps a wrapper or a count row from being reported as a project.
func parseProjectList(payload any) []Project {
	var projects []Project
	for _, row := range asRows(payload) {
		id := stringAt(row, 0)
		if !looksLikeUUID(id) {
			continue
		}
		project := Project{ID: id}
		if detail, ok := row[1].([]any); ok {
			project.Label = stringAt(detail, 0)
			project.Modified = timestampAt(detail, 2)
			for _, entry := range detail {
				s, ok := entry.(string)
				if !ok {
					continue
				}
				switch {
				case project.Thumbnail == "" && strings.Contains(s, "googleusercontent.com"):
					project.Thumbnail = s
				case looksLikeUUID(s):
					project.LastAssetID = s
				}
			}
		}
		projects = append(projects, project)
	}
	return projects
}

// asRows flattens the one array level the listing wraps its rows in, tolerating
// the rows sitting directly at the top level as well.
func asRows(payload any) [][]any {
	top, ok := payload.([]any)
	if !ok {
		return nil
	}
	var rows [][]any
	for _, entry := range top {
		row, ok := entry.([]any)
		if !ok {
			continue
		}
		// A row is a pair whose first element is a string. Anything else is
		// another wrapper, so descend one level and take its rows. The length
		// check is not decoration: an account with no projects answers with an
		// empty inner array, and indexing row[0] on it panics.
		if len(row) > 0 {
			if _, isRow := row[0].(string); isRow {
				rows = append(rows, row)
				continue
			}
		}
		rows = append(rows, asRows(entry)...)
	}
	return rows
}

// timestampAt reads the `[<unix-seconds>, <nanoseconds>]` pair the listing uses
// for timestamps, returning the zero time when it is absent or malformed.
func timestampAt(row []any, i int) time.Time {
	if i < 0 || i >= len(row) {
		return time.Time{}
	}
	pair, ok := row[i].([]any)
	if !ok || len(pair) < 2 {
		return time.Time{}
	}
	seconds, okSec := pair[0].(float64)
	nanos, okNanos := pair[1].(float64)
	if !okSec || !okNanos || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), int64(nanos))
}

// looksLikeUUID reports whether a string has the 8-4-4-4-12 shape. It is used to
// pick ids out of a mixed array, so it is deliberately a shape test and not a
// parse: the ids here are not always well-formed and rejecting one would drop a
// project.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
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
func (c *Client) UploadMedia(ctx context.Context, req UploadMediaRequest, opts CallOptions) (mediaID, contentID string, err error) {
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

	// Bound the image before it is encoded, for the same reason the flowapi
	// upload does: Flow refuses an oversized image with a failure that never
	// mentions size. The content type has to come back out because this request
	// carries it, and a downscale can change it — a WebP has to be re-encoded as
	// PNG or JPEG, since x/image decodes WebP but cannot encode it.
	normalized, normalizedType, normErr := httpx.NormalizeImage(req.Data, req.MimeType)
	if normErr != nil {
		return "", "", fmt.Errorf("batchexecute: %w", normErr)
	}
	req.Data = normalized
	req.MimeType = normalizedType

	if opts.SourcePath == "" {
		opts.SourcePath = "/project/" + req.ProjectID
	}

	frames, err := c.call(ctx, RPCIDUploadMedia, opts, func(token string) (any, error) {
		return []any{
			[]any{
				nil, toolContextID, nil, nil, nil, req.ProjectID, nil, nil, nil, nil,
				[]any{req.withCaptcha(token).CaptchaToken, 1},
			},
			base64.StdEncoding.EncodeToString(req.Data),
			req.MimeType,
			1,
			nil, nil, nil, nil,
			req.FileName,
			nil,
			uuid.NewString(),
			uuid.NewString(),
		}, nil
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
	// Model is the video key: "abra_t2v_8s", or an image-conditioned key such as
	// "abra_i2v_8s" or "omni_flash_i2v_8s_first_last_360p".
	Model  string
	Prompt string
	// Count is how many variations to submit. Each becomes its own entry in the
	// requests array with its own uuid pair.
	Count int
	// AspectRatio is the friendly name or ratio alias of the output aspect
	// ("portrait", "9:16", "landscape", "16:9", "square", "1:1"). It is resolved
	// to the integer the payload carries at request[3] via config.VideoAspectValue.
	//
	// Empty is the default and is NOT the same as "landscape" on the wire: empty
	// leaves the slot null, which is what every capture carries and what the
	// server reads as landscape, while "landscape" writes the integer 2. Both
	// render landscape today, and keeping them distinct is what lets the existing
	// fixtures stay byte-identical.
	AspectRatio string
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
	// Refused here rather than defaulted in the builder: an aspect the server
	// does not recognise is not an error to it — it accepts the submission and
	// renders nothing — so a bad value would be diagnosed from an empty result
	// several minutes later. This runs before the captcha token is spent.
	if req.AspectRatio != "" {
		if _, ok := config.VideoAspectValue(req.AspectRatio); !ok {
			return nil, fmt.Errorf("batchexecute: unknown video aspect %q; use one of %s",
				req.AspectRatio, strings.Join(config.VideoAspectNames, ", "))
		}
	}

	count := req.Count
	if count < 1 {
		count = 1
	}

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
	submit := func(token string) ([]Frame, error) {
		return c.call(ctx, rpcID, opts, func(string) (any, error) {
			return buildVideoArgument(req.withCaptcha(token)), nil
		})
	}

	frames, err := submit("")
	if err != nil {
		return nil, err
	}
	return retryIfEmpty(ctx, opts, frames, carriesNoMediaIDs, submit)
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
	trimEnd := req.TrimEnd
	if trimEnd <= 0 {
		trimEnd = 192
	}
	request := []any{
		[]any{nil, req.SourceID, req.TrimStart, trimEnd},
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

	if opts.SourcePath == "" {
		opts.SourcePath = "/project/" + req.ProjectID
	}
	rpcID := RPCIDVideoEdit
	if opts.RPCID != "" {
		rpcID = opts.RPCID
	}
	submit := func(token string) ([]Frame, error) {
		return c.call(ctx, rpcID, opts, func(string) (any, error) {
			return buildEditArgument(req.withCaptcha(token)), nil
		})
	}

	frames, err := submit("")
	if err != nil {
		return nil, err
	}
	return retryIfEmpty(ctx, opts, frames, carriesNoMediaIDs, submit)
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

/* ------------------------------------------------------------------ *
 * Replacing a spent captcha token
 * ------------------------------------------------------------------ */

// Each request that carries a reCAPTCHA token gets a withCaptcha method rather
// than a shared generic: the method returns the caller's own type, which one
// interface cannot do, and it keeps the replacement beside the field it replaces.
//
// An empty token means "keep the one already there", so a caller with no
// refresher configured behaves exactly as it did before.

func (r GenerateRequest) withCaptcha(token string) GenerateRequest {
	if token != "" {
		r.CaptchaToken = token
	}
	return r
}

func (r UploadMediaRequest) withCaptcha(token string) UploadMediaRequest {
	if token != "" {
		r.CaptchaToken = token
	}
	return r
}

func (r GenerateVideoRequest) withCaptcha(token string) GenerateVideoRequest {
	if token != "" {
		r.CaptchaToken = token
	}
	return r
}

func (r EditVideoRequest) withCaptcha(token string) EditVideoRequest {
	if token != "" {
		r.CaptchaToken = token
	}
	return r
}

func (r ReferenceVideoRequest) withCaptcha(token string) ReferenceVideoRequest {
	if token != "" {
		r.CaptchaToken = token
	}
	return r
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

	if opts.SourcePath == "" {
		opts.SourcePath = "/project/" + req.ProjectID
	}
	rpcID := RPCIDGenerateVideoReferences
	if opts.RPCID != "" {
		rpcID = opts.RPCID
	}
	submit := func(token string) ([]Frame, error) {
		return c.call(ctx, rpcID, opts, func(string) (any, error) {
			return buildReferenceArgument(req.withCaptcha(token)), nil
		})
	}

	frames, err := submit("")
	if err != nil {
		return nil, err
	}
	return retryIfEmpty(ctx, opts, frames, carriesNoMediaIDs, submit)
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
const ()

// upscaleRequestLength is the size of the upscale request array. The payload is a
// fixed-length positional array with a long run of nulls, and the model key sits
// at the very end — so the length is part of the contract, not an accident.
//
// Measured from the app's own request: 32 elements, model at index 31. An earlier
// value of 35 (model at 34) was wrong and the server accepted the call without
// acting on it, which is the worst kind of failure — no error, no output.

// upscaleModelIndex is where the upsampler model key goes.

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
// codes ("CAI", "CAM") mark derived assets — a variant, or an upscale — which share
// the same media id and are not interchangeable.
//
// The meaning is inferred from behaviour rather than documentation: for one media
// id the CAE row answers with image data and the CAI row answers with nothing.
// That was established through the image-upscale RPC, since removed, but
// ResolveContentID and MediaDetail still depend on it.
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
		} else if len(row) > 5 {
			// The length is checked because this is the only index in this
			// function that is read directly. Every other field goes through
			// stringAt, which answers "" past the end, so a short row is
			// tolerated everywhere except here — and here it was a panic, not a
			// missing title.
			//
			// It is reachable: isAssetRow accepts the flat shape from four
			// elements up, so a row of exactly four or five arrives as a valid
			// asset and then indexes past its own end. An unrecovered panic in
			// the parser takes the whole process down, which is a severe
			// response to a listing that is merely shorter than expected.
			if detail, ok := row[5].([]any); ok && len(detail) > 1 {
				if t, ok := detail[1].(string); ok {
					asset.Title = t
				}
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
			// The whole item, not a fixed slot: the refusal block sits after the
			// data element but its distance from it is not a contract. Scanning
			// the item cannot reach the payload itself, which is a JSON string
			// rather than nested arrays, so a legitimate response can never look
			// like a refusal.
			frame.Error = errorInfoReason(item)
			frames = append(frames, frame)
		}
	}

	if len(frames) == 0 {
		return nil, fmt.Errorf("batchexecute: the response contained no wrb.fr frames")
	}
	return frames, nil
}

// errorInfoReason digs an ErrorInfo reason out of a frame.
//
// The refusal block is `["type.googleapis.com/google.rpc.ErrorInfo", ["<REASON>"]]`
// — the same envelope Google uses for its status details. It is matched by
// shape rather than by index because its position moved once already and
// nothing here would have noticed: a missed reason reads exactly like a
// response that carried none.
func errorInfoReason(value any) string {
	items, ok := value.([]any)
	if !ok {
		return ""
	}

	if len(items) == 2 {
		if kind, ok := items[0].(string); ok && strings.Contains(kind, "ErrorInfo") {
			if reasons, ok := items[1].([]any); ok {
				for _, reason := range reasons {
					if text, ok := reason.(string); ok && text != "" {
						return text
					}
				}
			}
		}
	}

	for _, item := range items {
		if reason := errorInfoReason(item); reason != "" {
			return reason
		}
	}
	return ""
}

// UpstreamError returns the reason the server refused a call, or "" when it
// refused nothing.
//
// Exported because the caller is the one that can act on it: the transport
// knows the server said `PUBLIC_ERROR_UNUSUAL_ACTIVITY`, and only the engine
// knows what that means for the account it is running as.
func UpstreamError(frames []Frame) string {
	for _, frame := range frames {
		if frame.Error != "" {
			return frame.Error
		}
	}
	return ""
}

// ReasonUnusualActivity is the reason Flow gives when it will not accept the
// reCAPTCHA token.
//
// It is exported because it is the one refusal a caller can do something about
// beyond explaining it: the assessment rejected the *token*, and a token minted
// in a real page scores higher than one minted over the transport. Every other
// reason — a wrong model, a project belonging to another account — is answered
// by changing the request, not by minting again.
const ReasonUnusualActivity = "PUBLIC_ERROR_UNUSUAL_ACTIVITY"

// RejectedError reports a call the server answered with an explicit refusal.
//
// It exists to keep a refusal from being reported as a silent no-op. Both
// arrive as HTTP 200 with a `wrb.fr` frame, and only one of them carries a
// reason — so before this, a submission the assessment had thrown out was
// reported as "the transport returned no frames", and the hint attached to that
// message sent the reader after the model enum and the project. Both were fine.
//
// PUBLIC_ERROR_UNUSUAL_ACTIVITY is what the assessment answers when it will not
// accept the reCAPTCHA token — see the recaptcha package, which documents the
// same reason for a token that is missing or stale. The remedy is a token the
// assessment accepts, not a different model.
type RejectedError struct {
	// Reason is the ErrorInfo reason the server gave, e.g.
	// "PUBLIC_ERROR_UNUSUAL_ACTIVITY".
	Reason string
	// Frames is how many frames the final response carried. Non-zero, because a
	// refusal is an answer: reporting it as zero is what made it read as
	// silence.
	Frames int
}

func (e *RejectedError) Error() string {
	return "batchexecute: the server refused the call: " + e.Reason
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}
