package batchexecute

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodelyx/cdp-control/cookiejar"
)

func jarWith(cookies ...cookiejar.Cookie) *cookiejar.Jar {
	return cookiejar.FromCookies(cookies, "test")
}

func TestAuthorizationShape(t *testing.T) {
	jar := jarWith(cookiejar.Cookie{
		Domain: ".google.com", Path: "/", Name: "SAPISID", Value: "sapisid-value",
	})
	client := New(jar, nil)

	header, err := client.Authorization()
	if err != nil {
		t.Fatalf("Authorization failed: %v", err)
	}
	if !strings.HasPrefix(header, "SAPISIDHASH ") {
		t.Fatalf("header %q should start with SAPISIDHASH", header)
	}

	// SAPISIDHASH <seconds>_<sha1hex(seconds + " " + SAPISID + " " + origin)>
	parts := strings.SplitN(strings.TrimPrefix(header, "SAPISIDHASH "), "_", 2)
	if len(parts) != 2 {
		t.Fatalf("header %q should be <seconds>_<hash>", header)
	}

	seconds, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatalf("timestamp %q is not an integer: %v", parts[0], err)
	}
	if delta := time.Since(time.Unix(seconds, 0)); delta > time.Minute || delta < -time.Minute {
		t.Errorf("timestamp is %s away from now, which is not plausible", delta)
	}

	expected := sha1.Sum([]byte(parts[0] + " " + "sapisid-value" + " " + Origin))
	if parts[1] != hex.EncodeToString(expected[:]) {
		t.Errorf("hash does not match sha1(timestamp SAPISID origin)")
	}
}

// TestAuthorizationIsOriginBound is the property that matters for safety: the
// hash is scoped to the origin, so it cannot be replayed against another host.
func TestAuthorizationIsOriginBound(t *testing.T) {
	jar := jarWith(cookiejar.Cookie{
		Domain: ".google.com", Path: "/", Name: "SAPISID", Value: "v",
	})
	client := New(jar, nil)

	header, err := client.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	seconds := strings.SplitN(strings.TrimPrefix(header, "SAPISIDHASH "), "_", 2)[0]

	otherOrigin := sha1.Sum([]byte(seconds + " " + "v" + " " + "https://labs.google"))
	if strings.Contains(header, hex.EncodeToString(otherOrigin[:])) {
		t.Error("the hash must be bound to flow.google.com, not another origin")
	}
}

func TestAuthorizationWithoutSAPISID(t *testing.T) {
	jar := jarWith(cookiejar.Cookie{
		Domain: ".google.com", Path: "/", Name: "SID", Value: "only-sid",
	})
	client := New(jar, nil)

	if _, err := client.Authorization(); err == nil {
		t.Error("Authorization should fail without a SAPISID cookie")
	}
}

func TestSAPISIDPrefersSAPISID(t *testing.T) {
	jar := jarWith(
		cookiejar.Cookie{Domain: ".google.com", Path: "/", Name: "__Secure-3PAPISID", Value: "fallback"},
		cookiejar.Cookie{Domain: ".google.com", Path: "/", Name: "SAPISID", Value: "preferred"},
	)
	client := New(jar, nil)

	if got := client.SAPISID(); got != "preferred" {
		t.Errorf("SAPISID() = %q, want preferred", got)
	}
}

// TestExtractXSRF covers the first half of the handshake: the server answers an
// un-tokened call with a body carrying the anti-CSRF token, which must be read
// rather than surfaced as an error.
func TestExtractXSRF(t *testing.T) {
	body := `)]}'

199
[["er",null,null,null,null,400,null,null,null,3,[{"48448350":["xsrf","AIQ-s5gPQTOep6C4Wl2jUb_jIuJb:1789723276401",["100694712991212656733"]]}]],["di",14],["af.httprm",14,"5708749757671396501",148]]
25
[["e",4,null,null,235]]`

	got := extractXSRF(body)
	want := "AIQ-s5gPQTOep6C4Wl2jUb_jIuJb:1789723276401"
	if got != want {
		t.Errorf("extractXSRF = %q, want %q", got, want)
	}

	if extractXSRF("no token here") != "" {
		t.Error("extractXSRF should return empty when there is no xsrf token")
	}
}

// TestParseFrames uses a real response body captured from the live endpoint.
func TestParseFrames(t *testing.T) {
	body := ")]}'\n\n" +
		"199\n" +
		`[["wrb.fr","WuwhI","[]",null,null,null,"generic"],["di",14]]` + "\n" +
		"25\n" +
		`[["e",4,null,null,235]]` + "\n"

	frames, err := ParseFrames(body)
	if err != nil {
		t.Fatalf("ParseFrames failed: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 wrb.fr frame, got %d", len(frames))
	}
	if frames[0].RPCID != "WuwhI" {
		t.Errorf("RPCID = %q, want WuwhI", frames[0].RPCID)
	}
	if string(frames[0].Payload) != "[]" {
		t.Errorf("Payload = %q, want []", frames[0].Payload)
	}
}

func TestParseFramesIgnoresNoise(t *testing.T) {
	// A length prefix, a preamble that is not a frame, and an epilogue.
	body := ")]}'\n199\nnot json at all\n[][\"unrelated\"]\n" +
		`[["wrb.fr","Xyz","{\"a\":1}",null,null,null,"generic"]]` + "\n" +
		`[["e",4,null,null,235]]`

	frames, err := ParseFrames(body)
	if err != nil {
		t.Fatalf("ParseFrames failed: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if string(frames[0].Payload) != `{"a":1}` {
		t.Errorf("Payload = %q", frames[0].Payload)
	}
}

func TestParseFramesEmpty(t *testing.T) {
	if _, err := ParseFrames(")]}'\n\n0\n"); err == nil {
		t.Error("a body with no frames should be an error, not an empty result")
	}
}

func TestTokenCachingAndReset(t *testing.T) {
	jar := jarWith(cookiejar.Cookie{
		Domain: ".google.com", Path: "/", Name: "SAPISID", Value: "v",
	})
	client := New(jar, nil)

	if client.Token() != "" {
		t.Error("a fresh client should have no anti-CSRF token")
	}

	client.setToken("abc")
	if client.Token() != "abc" {
		t.Error("the token should be cached")
	}

	// A new session invalidates the token, since it is bound to the session.
	client.SetJar(jarWith(cookiejar.Cookie{
		Domain: ".google.com", Path: "/", Name: "SAPISID", Value: "v2",
	}))
	if client.Token() != "" {
		t.Error("swapping the jar should drop the cached token")
	}
}

func TestEndpointConstants(t *testing.T) {
	// These are the values observed in the live app's own traffic; changing them
	// without evidence would silently break the transport.
	if EndpointPath != "/_/AiSandboxAngularFrontend/data/batchexecute" {
		t.Errorf("EndpointPath changed: %s", EndpointPath)
	}
	if Origin != "https://flow.google.com" {
		t.Errorf("Origin changed: %s", Origin)
	}
	if RPCIDProjectList != "WuwhI" {
		t.Errorf("RPCIDProjectList changed: %s", RPCIDProjectList)
	}
}

// TestGenerateArgumentPromptNesting is the regression test for the defect that
// cost the most time.
//
// The prompt must sit at [1][0][8][0][0][0] as a *string*. With one extra level
// of nesting a list lands there instead, and the server answers HTTP 200 while
// generating nothing at all — a silent no-op that no status code reveals. The
// only cheap guard is asserting the shape directly.
func TestGenerateArgumentPromptNesting(t *testing.T) {
	req := GenerateRequest{
		ProjectID:    "proj-123",
		Model:        "NARWHAL",
		Prompt:       "a small wooden rowing boat",
		CaptchaToken: "0cAFcWeAtoken",
	}

	arg := buildGenerateArgument(req, 4242)

	// Walk to [1][0][8].
	outer, ok := arg[1].([]any)
	if !ok || len(outer) != 1 {
		t.Fatalf("arg[1] should hold exactly one request, got %#v", arg[1])
	}
	request, ok := outer[0].([]any)
	if !ok || len(request) != 14 {
		t.Fatalf("the request should have 14 fields, got %d", len(request))
	}

	promptBlock, ok := request[8].([]any)
	if !ok {
		t.Fatalf("request[8] should be the prompt block, got %T", request[8])
	}

	// [8][0][0][0] must be the string itself.
	got, ok := promptBlock[0].([]any)[0].([]any)[0].(string)
	if !ok {
		t.Fatal("the prompt must be a string at [1][0][8][0][0][0]; " +
			"a list there means one level of nesting too many, and the server " +
			"will accept the request while generating nothing")
	}
	if got != req.Prompt {
		t.Errorf("prompt = %q, want %q", got, req.Prompt)
	}
}

// TestGenerateArgumentFields checks the other positions the server reads.
func TestGenerateArgumentFields(t *testing.T) {
	req := GenerateRequest{
		ProjectID:    "proj-123",
		Model:        "abra_t2v_8s",
		Prompt:       "p",
		CaptchaToken: "0cAFcWeAtoken",
	}
	arg := buildGenerateArgument(req, 99)

	request := arg[1].([]any)[0].([]any)

	if request[3] != int64(99) {
		t.Errorf("seed at [3] = %v, want 99", request[3])
	}
	if request[4] != generateMode {
		t.Errorf("mode at [4] = %v, want %d", request[4], generateMode)
	}
	if request[5] != req.Model {
		t.Errorf("model at [5] = %v, want %q", request[5], req.Model)
	}

	ctx := request[7].([]any)
	if ctx[1] != toolContextID {
		t.Errorf("tool id at [7][1] = %v, want %d", ctx[1], toolContextID)
	}
	if ctx[5] != req.ProjectID {
		t.Errorf("project at [7][5] = %v, want %q", ctx[5], req.ProjectID)
	}
	tokenPair := ctx[10].([]any)
	if tokenPair[0] != req.CaptchaToken {
		t.Errorf("captcha at [7][10][0] = %v, want the token", tokenPair[0])
	}
	if tokenPair[1] != 1 {
		t.Errorf("captcha flag at [7][10][1] = %v, want 1", tokenPair[1])
	}

	// The context block is repeated at [3], identical to [1][0][7].
	repeated := arg[3].([]any)
	if repeated[5] != req.ProjectID {
		t.Errorf("project at [3][5] = %v, want %q", repeated[5], req.ProjectID)
	}
	if arg[2] != 1 {
		t.Errorf("arg[2] = %v, want 1", arg[2])
	}
	if len(arg) != 5 {
		t.Errorf("the argument should have 5 top-level fields, got %d", len(arg))
	}
}

func TestGenerateValidatesInput(t *testing.T) {
	client := New(nil, nil)
	ctx := context.Background()

	cases := []struct {
		name string
		req  GenerateRequest
	}{
		{"no project", GenerateRequest{Model: "NARWHAL", Prompt: "p", CaptchaToken: "t"}},
		{"no model", GenerateRequest{ProjectID: "p", Prompt: "p", CaptchaToken: "t"}},
		{"no prompt", GenerateRequest{ProjectID: "p", Model: "NARWHAL", CaptchaToken: "t"}},
		{"blank prompt", GenerateRequest{ProjectID: "p", Model: "NARWHAL", Prompt: "   ", CaptchaToken: "t"}},
		{"no captcha", GenerateRequest{ProjectID: "p", Model: "NARWHAL", Prompt: "p"}},
	}

	for _, tc := range cases {
		if _, err := client.Generate(ctx, tc.req, CallOptions{}); err == nil {
			t.Errorf("%s: expected a validation error", tc.name)
		}
	}
}

// TestParseGeneratedMediaUnescapesURL is the regression test for a 403 that
// looked like an auth failure.
//
// The signed URL comes back with `\u003d` for `=` and `\u0026` for `&`. Pulling
// the URL out before unescaping truncates it at the first escape, cutting off the
// signature, and the download fails with a 403 that has nothing to do with
// credentials.
func TestParseGeneratedMediaUnescapesURL(t *testing.T) {
	payload := []byte(`[[["a9f1fa5f-3f4d-442c-be35-3a4f368992cc",null,"265332c4-7e00-4a51-b042-633199690c09",null,null,null,[[null,533000,null,null,null,null,1,"a prompt",29,null,null,"265332c4-7e00-4a51-b042-633199690c09",null,"https://flow-content.google/image/a9f1fa5f-3f4d-442c-be35-3a4f368992cc?Expires\u003d1789747066\u0026KeyName\u003dlabs-flow-prod-cdn-key\u0026Signature\u003dK4bz43CtwJoH1N4Y4HKhdIS7dDA",3]]]]`)

	media := ParseGeneratedMedia(payload)
	if len(media) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(media))
	}

	got := media[0]
	if got.MediaID != "a9f1fa5f-3f4d-442c-be35-3a4f368992cc" {
		t.Errorf("MediaID = %q", got.MediaID)
	}

	// The whole query string has to survive, signature included.
	for _, want := range []string{"Expires=1789747066", "KeyName=labs-flow-prod-cdn-key", "Signature=K4bz43CtwJoH1N4Y4HKhdIS7dDA"} {
		if !strings.Contains(got.URL, want) {
			t.Errorf("URL is missing %q\n  got: %s", want, got.URL)
		}
	}
	if strings.Contains(got.URL, `\u`) {
		t.Errorf("URL still contains an escape sequence: %s", got.URL)
	}
}

func TestParseGeneratedMediaDeduplicates(t *testing.T) {
	// The same asset appears in several places in a real response.
	payload := []byte(`["https://flow-content.google/image/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee?Expires=1\u0026Signature=x",
	                    "https://flow-content.google/image/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee?Expires=1\u0026Signature=x"]`)

	media := ParseGeneratedMedia(payload)
	if len(media) != 1 {
		t.Errorf("expected the duplicate to collapse, got %d", len(media))
	}
}

func TestParseGeneratedMediaEmpty(t *testing.T) {
	if got := ParseGeneratedMedia(nil); got != nil {
		t.Error("a nil payload should yield no media")
	}
	if got := ParseGeneratedMedia([]byte(`null`)); got != nil {
		t.Error("a null payload should yield no media")
	}
}

// TestGenerateVideoArgumentPromptNesting is the regression test for the same
// off-by-one that bit the image payload.
//
// The video prompt block sits at [0] (not [8] as for images) and nests three
// levels, so the string lands at [0][0][2][0][0][0]. With an extra level the
// server returns 200 and generates nothing.
func TestGenerateVideoArgumentPromptNesting(t *testing.T) {
	arg := buildVideoArgument(GenerateVideoRequest{
		ProjectID:    "proj-1",
		Model:        "abra_t2v_8s",
		Prompt:       "a paper boat drifting down a calm river",
		Count:        2,
		CaptchaToken: "0cAFcWeAtoken",
	})

	requests, ok := arg[0].([]any)
	if !ok || len(requests) != 2 {
		t.Fatalf("arg[0] should hold one entry per variation, got %#v", arg[0])
	}

	first, ok := requests[0].([]any)
	if !ok || len(first) != 5 {
		t.Fatalf("a video request should have 5 fields, got %d", len(first))
	}

	block, ok := first[0].([]any)
	if !ok || len(block) != 3 {
		t.Fatalf("the prompt block should have 3 fields, got %#v", first[0])
	}

	// block is [null, null, [[[prompt]]]], so the string is at block[2][0][0][0].
	got, ok := block[2].([]any)[0].([]any)[0].([]any)[0].(string)
	if !ok {
		t.Fatal("the prompt must be a string at [0][0][2][0][0][0]; a list there " +
			"means one level of nesting too many and the server will accept the " +
			"request while generating nothing")
	}
	if got != "a paper boat drifting down a calm river" {
		t.Errorf("prompt = %q", got)
	}

	if first[1] != "abra_t2v_8s" {
		t.Errorf("model at [1] = %v", first[1])
	}
	if first[2] != videoModeText {
		t.Errorf("mode at [2] = %v, want %d", first[2], videoModeText)
	}

	// The context block repeats the project and the captcha token.
	ctx := arg[1].([]any)
	if ctx[5] != "proj-1" {
		t.Errorf("project at [1][5] = %v", ctx[5])
	}
	if ctx[10].([]any)[0] != "0cAFcWeAtoken" {
		t.Errorf("captcha at [1][10][0] = %v", ctx[10].([]any)[0])
	}
	if arg[2].([]any)[1] != videoModeText {
		t.Errorf("batch mode at [2][1] = %v, want %d", arg[2].([]any)[1], videoModeText)
	}
}

// TestVideoAndImagePromptsDiffer guards the thing that caused the most confusion:
// the two RPCs place the prompt at different paths.
func TestVideoAndImagePromptsDiffer(t *testing.T) {
	prompt := "same text"

	imgArg := buildGenerateArgument(GenerateRequest{
		ProjectID: "p", Model: "NARWHAL", Prompt: prompt, CaptchaToken: "t"}, 1)
	imgPrompt := imgArg[1].([]any)[0].([]any)[8].([]any)[0].([]any)[0].([]any)[0].(string)

	vidArg := buildVideoArgument(GenerateVideoRequest{
		ProjectID: "p", Model: "abra_t2v_4s", Prompt: prompt, Count: 1, CaptchaToken: "t"})
	vidPrompt := vidArg[0].([]any)[0].([]any)[0].([]any)[2].([]any)[0].([]any)[0].([]any)[0].(string)

	if imgPrompt != prompt || vidPrompt != prompt {
		t.Errorf("prompts should both round-trip: image=%q video=%q", imgPrompt, vidPrompt)
	}
}

// TestParseGeneratedMediaIDs covers the asynchronous video response shape, which
// carries ids to poll rather than a URL.
func TestParseGeneratedMediaIDs(t *testing.T) {
	payload := []byte(`[null,357,[["694b7630-703d-4520-8687-04bafaae747c",null,null,["Paper boat",[1789727042,291993000]],"d86bc0b4-30dc-4a52-9f3d-cb90be0079ea"],[["694b7630-703d-4520-8687-04bafaae747c","d86bc0b4-30dc-4a52-9f3d-cb90be0079ea"]]]]`)

	ids := ParseGeneratedMediaIDs(payload)
	if len(ids) != 1 {
		t.Fatalf("expected 1 media id, got %v", ids)
	}
	if ids[0] != "694b7630-703d-4520-8687-04bafaae747c" {
		t.Errorf("media id = %q", ids[0])
	}

	if got := ParseGeneratedMediaIDs([]byte(`null`)); got != nil {
		t.Error("a null payload should yield no ids")
	}
	if got := ParseGeneratedMediaIDs(nil); got != nil {
		t.Error("a nil payload should yield no ids")
	}
}

func TestGenerateVideoValidatesInput(t *testing.T) {
	client := New(nil, nil)
	ctx := context.Background()

	for _, req := range []GenerateVideoRequest{
		{Model: "abra_t2v_4s", Prompt: "p", CaptchaToken: "t"},
		{ProjectID: "p", Prompt: "p", CaptchaToken: "t"},
		{ProjectID: "p", Model: "abra_t2v_4s", CaptchaToken: "t"},
		{ProjectID: "p", Model: "abra_t2v_4s", Prompt: " ", CaptchaToken: "t"},
		{ProjectID: "p", Model: "abra_t2v_4s", Prompt: "p"},
	} {
		if _, err := client.GenerateVideo(ctx, req, CallOptions{}); err == nil {
			t.Errorf("expected a validation error for %+v", req)
		}
	}
}

// TestParseMediaOfKind covers a video response, which carries its poster (an
// /image/ URL) alongside the asset itself (a /video/ URL). Filtering by kind is
// what stops the poster being downloaded as if it were the video.
func TestParseMediaOfKind(t *testing.T) {
	payload := []byte(`["a255d24c-83aa-4e51-ae84-ab28d038b407",
	  "https://flow-content.google/image/a255d24c-83aa-4e51-ae84-ab28d038b407?Expires=1\u0026Signature=poster",
	  "https://flow-content.google/video/a255d24c-83aa-4e51-ae84-ab28d038b407?Expires=1\u0026Signature=asset"]`)

	all := ParseGeneratedMedia(payload)
	if len(all) != 2 {
		t.Fatalf("expected both poster and asset, got %d", len(all))
	}

	videos := ParseMediaOfKind(payload, "video")
	if len(videos) != 1 {
		t.Fatalf("expected 1 video, got %d", len(videos))
	}
	if videos[0].Kind != "video" {
		t.Errorf("kind = %q", videos[0].Kind)
	}
	if !strings.Contains(videos[0].URL, "Signature=asset") {
		t.Errorf("wrong URL chosen: %s", videos[0].URL)
	}

	images := ParseMediaOfKind(payload, "image")
	if len(images) != 1 || !strings.Contains(images[0].URL, "Signature=poster") {
		t.Errorf("expected the poster as the image: %v", images)
	}
}

// TestParseProjectAssets uses the real listing shape.
//
// A row is [content-id, project-id, media-id, type-code, null, detail, ...]. The
// content id and the media id are different values, and several rows can share a
// media id — an original and its upscales do — so a parser that conflates them
// cannot tell an upscale from its source.
func TestParseProjectAssets(t *testing.T) {
	const payload = `[null,null,[` +
		`["451f2cef-cdf8-47b9-9c20-db8ea17e1008","d86bc0b4-30dc-4a52-9f3d-cb90be0079ea",` +
		`"1370d7c6-6e2f-4ebb-b2fa-b40da7204012","CAE",null,` +
		`[[1789727860,674285000],"a paper boat drifting on a river",null,null,null,"https://lh3.example/a"]],` +
		`["451f2cef-cdf8-47b9-9c20-db8ea17e1008_upsampled","d86bc0b4-30dc-4a52-9f3d-cb90be0079ea",` +
		`"1370d7c6-6e2f-4ebb-b2fa-b40da7204012","CAI",null,` +
		`[[1789727860,674285000],null,null,null,null,"https://lh3.example/b"]]` +
		`]]`

	assets := ParseProjectAssets([]byte(payload))
	if len(assets) != 2 {
		t.Fatalf("parsed %d assets, want 2 — a parser that finds nothing makes every "+
			"caller wait until its timeout with no error", len(assets))
	}

	source, upscaled := assets[0], assets[1]

	if source.MediaID != "1370d7c6-6e2f-4ebb-b2fa-b40da7204012" {
		t.Errorf("MediaID = %q, want the value at row[2]", source.MediaID)
	}
	if source.ContentID != "451f2cef-cdf8-47b9-9c20-db8ea17e1008" {
		t.Errorf("ContentID = %q, want the value at row[0]", source.ContentID)
	}
	if source.MediaID == source.ContentID {
		t.Error("the content id must not be the media id; the upscale and media-detail " +
			"RPCs take the former")
	}
	if source.Title != "a paper boat drifting on a river" {
		t.Errorf("Title = %q", source.Title)
	}
	// The type code is what tells an original from a derived asset, and both share
	// a media id — so it is the only field that can disambiguate them.
	if source.TypeCode != AssetTypeOriginal {
		t.Errorf("TypeCode = %q, want %q for the generated asset", source.TypeCode, AssetTypeOriginal)
	}
	if upscaled.TypeCode == AssetTypeOriginal {
		t.Errorf("the upscaled row should not be marked %q", AssetTypeOriginal)
	}

	// The upscaled row shares the media id but has its own content id.
	if upscaled.MediaID != source.MediaID {
		t.Errorf("the upscaled row should share the source's media id, got %q", upscaled.MediaID)
	}
	if upscaled.ContentID != "451f2cef-cdf8-47b9-9c20-db8ea17e1008_upsampled" {
		t.Errorf("upscaled ContentID = %q", upscaled.ContentID)
	}
}

// TestParseProjectAssetsReadsTheNestedListingShape uses a row copied from a live
// Zzl0ze response.
//
// This is the shape the app actually serves today, and it is not the flat one:
// row[1] and row[2] are null, the id pair sits inside the detail array, and the
// project id takes row[3]'s place. A reader that required four leading non-empty
// strings matched no rows at all here — so an asset that was plainly in the
// listing read as absent, and an upscale could not resolve its content id.
func TestParseProjectAssetsReadsTheNestedListingShape(t *testing.T) {
	const payload = `[null,null,[` +
		`["f7af1f07-af43-49ab-9f6d-dd5a606a459e",null,null,` +
		`["Green paper leaf on paper",[1789799832,452366000],null,null,` +
		`"e95cdd98-63f7-4563-8f52-1f630632fd11",` +
		`"3b696c3c-fbea-4242-b612-83a9dba1c9f0",[1789799853,163938000]],` +
		`"96bdb77f-8ba7-44e3-939c-c6f0a7ed2d7e"]` +
		`]]`

	assets := ParseProjectAssets([]byte(payload))
	if len(assets) != 1 {
		t.Fatalf("parsed %d assets, want 1", len(assets))
	}

	got := assets[0]
	if got.MediaID != "e95cdd98-63f7-4563-8f52-1f630632fd11" {
		t.Errorf("MediaID = %q, want the value at detail[4]", got.MediaID)
	}
	if got.ContentID != "3b696c3c-fbea-4242-b612-83a9dba1c9f0" {
		t.Errorf("ContentID = %q, want the value at detail[5]", got.ContentID)
	}
	if got.MediaID == got.ContentID {
		t.Error("the media id and the content id must stay distinct — the upscale takes " +
			"the content id and the UI addresses the media id")
	}
	if got.Title != "Green paper leaf on paper" {
		t.Errorf("Title = %q, want the value at detail[0]", got.Title)
	}
}

// TestParseProjectAssetsStillReadsTheFlatShape keeps the older shape working.
//
// Both are in circulation, and the nested one was added alongside the flat one
// rather than in place of it.
func TestParseProjectAssetsStillReadsTheFlatShape(t *testing.T) {
	const payload = `[null,null,[` +
		`["451f2cef-cdf8-47b9-9c20-db8ea17e1008","d86bc0b4-30dc-4a52-9f3d-cb90be0079ea",` +
		`"1370d7c6-6e2f-4ebb-b2fa-b40da7204012","CAE",null,` +
		`[[1789727860,674285000],"a paper boat drifting on a river",null,null,null,"https://lh3.example/a"]]` +
		`]]`

	assets := ParseProjectAssets([]byte(payload))
	if len(assets) != 1 {
		t.Fatalf("parsed %d assets, want 1", len(assets))
	}
	if assets[0].MediaID != "1370d7c6-6e2f-4ebb-b2fa-b40da7204012" {
		t.Errorf("MediaID = %q", assets[0].MediaID)
	}
	if assets[0].ContentID != "451f2cef-cdf8-47b9-9c20-db8ea17e1008" {
		t.Errorf("ContentID = %q", assets[0].ContentID)
	}
	if assets[0].TypeCode != AssetTypeOriginal {
		t.Errorf("TypeCode = %q", assets[0].TypeCode)
	}
}

// TestParseProjectAssetsRejectsJunk guards against the shape test being loosened
// until arbitrary arrays parse as assets.
func TestParseProjectAssetsRejectsJunk(t *testing.T) {
	for name, payload := range map[string]string{
		"empty":        `[]`,
		"null":         `null`,
		"not json":     `{`,
		"numbers only": `[[1,2,3,4]]`,
		"nulls":        `[[null,null,null,null]]`,
		// A row shaped like the nested one but with no id pair inside the detail
		// array is not an asset and must not be counted as one.
		"nested without ids": `[["a",null,null,["title",[1],null,null,null,null,[1]],"proj"]]`,
	} {
		if got := ParseProjectAssets([]byte(payload)); len(got) != 0 {
			t.Errorf("%s: parsed %d assets, want 0", name, len(got))
		}
	}
}

func TestParseProjectAssetsEmpty(t *testing.T) {
	if got := ParseProjectAssets(nil); got != nil {
		t.Error("a nil payload should yield no assets")
	}
	if got := ParseProjectAssets([]byte(`null`)); got != nil {
		t.Error("a null payload should yield no assets")
	}
}

func TestMediaDetailValidatesInput(t *testing.T) {
	client := New(nil, nil)
	ctx := context.Background()

	for _, tc := range [][3]string{
		{"", "m", "c"}, {"p", "", "c"}, {"p", "m", ""},
	} {
		if _, err := client.MediaDetail(ctx, tc[0], tc[1], tc[2]); err == nil {
			t.Errorf("expected a validation error for %v", tc)
		}
	}
}

// TestBuildUpscaleArgument pins the shape of the upscale payload.
//
// Every value here is read off the app's own captured request. This payload is
// unforgiving in a specific way: get the shape right and the indices wrong and
// the server accepts the call, returns a well-formed empty result, and renders
// nothing. There is no error to notice, which is exactly why the positions are
// pinned rather than described.
func TestBuildUpscaleArgument(t *testing.T) {
	arg := buildUpscaleArgument(UpscaleRequest{
		ProjectID:    "proj-1",
		ContentID:    "content-1",
		MediaID:      "source-media",
		CallID:       "call-1",
		CaptchaToken: "0cAFcWeAtoken",
	})

	if len(arg) != 3 {
		t.Fatalf("the argument has %d elements, want 3 — [request, context, [call-id]]. "+
			"Dropping the trailing array is easy and makes the call a no-op", len(arg))
	}

	outer, ok := arg[0].([]any)
	if !ok || len(outer) != 1 {
		t.Fatalf("arg[0] should hold one request, got %#v", arg[0])
	}

	request, ok := outer[0].([]any)
	if !ok {
		t.Fatalf("the request should be an array, got %T", outer[0])
	}
	if len(request) != upscaleRequestLength {
		t.Fatalf("request length = %d, want %d — the payload is positional and the "+
			"length is part of the contract", len(request), upscaleRequestLength)
	}

	// The model key must be the last element.
	if got := request[upscaleModelIndex]; got != UpscaleModel1080p {
		t.Errorf("model at [%d] = %v, want %q (the 1080p default)",
			upscaleModelIndex, got, UpscaleModel1080p)
	}
	if upscaleModelIndex != len(request)-1 {
		t.Errorf("upscaleModelIndex = %d but the request has %d elements; the model "+
			"must be the last one", upscaleModelIndex, len(request))
	}

	// Everything between the known positions must be null.
	known := map[int]bool{0: true, 2: true, 4: true, 6: true, upscaleModelIndex: true}
	for i, v := range request {
		if known[i] {
			continue
		}
		if v != nil {
			t.Errorf("request[%d] = %v, want nil — only the recorded positions are used", i, v)
		}
	}

	// [0] carries the source asset's *content* id, not its media id.
	first, ok := request[0].([]any)
	if !ok || len(first) != 2 || first[1] != "content-1" {
		t.Errorf("request[0] should be [null, \"content-1\"], got %#v", request[0])
	}

	// [4] carries the source media id.
	src, ok := request[4].([]any)
	if !ok || len(src) != 5 || src[1] != "source-media" {
		t.Errorf("request[4] should carry the source media id, got %#v", request[4])
	}

	if request[2] != 2 {
		t.Errorf("request[2] = %v, want 2", request[2])
	}
	if request[6] != 2 {
		t.Errorf("request[6] = %v, want 2", request[6])
	}

	// The context block repeats the project and captcha token.
	ctx := arg[1].([]any)
	if ctx[5] != "proj-1" {
		t.Errorf("project at [1][5] = %v", ctx[5])
	}
	if ctx[10].([]any)[0] != "0cAFcWeAtoken" {
		t.Errorf("captcha at [1][10][0] = %v", ctx[10].([]any)[0])
	}

	// The trailing call id, supplied here so a capture can be replayed exactly.
	tail, ok := arg[2].([]any)
	if !ok || len(tail) != 1 || tail[0] != "call-1" {
		t.Errorf("arg[2] should be [\"call-1\"], got %#v", arg[2])
	}
}

func TestBuildUpscaleArgumentGeneratesMissingIDs(t *testing.T) {
	arg := buildUpscaleArgument(UpscaleRequest{
		ProjectID: "p", ContentID: "c", MediaID: "m", CaptchaToken: "t"})

	request := arg[0].([]any)[0].([]any)

	if request[0].([]any)[1] != "c" {
		t.Errorf("request[0][1] should be the content id, got %v", request[0].([]any)[1])
	}
	if request[4].([]any)[4] == nil {
		t.Error("a request uuid should be generated")
	}
	if tail := arg[2].([]any)[0]; tail == nil || tail == "" {
		t.Error("a call id should be generated when none is supplied")
	}
	if request[upscaleModelIndex] != UpscaleModel1080p {
		t.Errorf("the default model should be the 1080p upsampler, got %v", request[upscaleModelIndex])
	}
}

// TestParseUpscaledAssetID uses a real captured response.
//
// The value it returns is a submission acknowledgement, not a finished asset:
// the id names a render that does not exist yet, which is why the response has no
// URL and why treating it as "no output" was wrong.
func TestParseUpscaledAssetID(t *testing.T) {
	const captured = `[[[["451f2cef-cdf8-47b9-9c20-db8ea17e1008_upsampled"],"",null,null,1]],323,` +
		`[["1370d7c6-6e2f-4ebb-b2fa-b40da7204012",null,null,["Paper boat drifting on river",` +
		`[1789727595,919116000],null,null,"451f2cef-cdf8-47b9-9c20-db8ea17e1008",` +
		`"4f83f44a-e1e3-4ca1-acbe-671c3a180bc6",[1789727601,738295000]],` +
		`"d86bc0b4-30dc-4a52-9f3d-cb90be0079ea"]]]`

	frames := []Frame{{RPCID: "p0UkFb", Payload: json.RawMessage(captured)}}
	got, err := ParseUpscaledAssetID(frames)
	if err != nil {
		t.Fatalf("ParseUpscaledAssetID: %v", err)
	}
	if want := "451f2cef-cdf8-47b9-9c20-db8ea17e1008_upsampled"; got != want {
		t.Errorf("asset id = %q, want %q", got, want)
	}
}

func TestParseUpscaledAssetIDRejectsEmpty(t *testing.T) {
	// A response with the right shape but no id must be an error, not a silent "".
	for name, frames := range map[string][]Frame{
		"no frames":     {},
		"empty payload": {{RPCID: "p0UkFb"}},
		"null payload":  {{RPCID: "p0UkFb", Payload: json.RawMessage("null")}},
		"wrong shape":   {{RPCID: "p0UkFb", Payload: json.RawMessage(`[[]]`)}},
	} {
		if _, err := ParseUpscaledAssetID(frames); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestUpscaleValidatesInput(t *testing.T) {
	client := New(nil, nil)
	ctx := context.Background()

	for _, req := range []UpscaleRequest{
		{MediaID: "m", CaptchaToken: "t"},
		{ProjectID: "p", CaptchaToken: "t"},
		{ProjectID: "p", MediaID: "m"},
		{ProjectID: "p", MediaID: "m", CaptchaToken: "t"}, // no content id
	} {
		if _, err := client.Upscale(ctx, req, CallOptions{}); err == nil {
			t.Errorf("expected a validation error for %+v", req)
		}
	}
}

func TestUpscaleModelKeys(t *testing.T) {
	// These are the keys the app sends; changing them silently would make every
	// upscale a no-op.
	if UpscaleModel1080p != "veo_3_1_upsampler_1080p" {
		t.Errorf("1080p model key changed: %s", UpscaleModel1080p)
	}
	if UpscaleModel4K != "veo_3_1_upsampler_4k" {
		t.Errorf("4k model key changed: %s", UpscaleModel4K)
	}
}

// TestParseGeneratedMediaUpsampledURL is the regression test for a poll that
// never completed.
//
// A derived asset is named "<content-id>_upsampled", which is longer than a bare
// uuid. The URL regex used to require exactly 36 hex-or-dash characters before
// the "?", so upscaled URLs never matched and MediaDetail reported "still
// rendering" forever — with no error, just a seven-minute timeout.
func TestParseGeneratedMediaUpsampledURL(t *testing.T) {
	const payload = `["d86bc0b4-30dc-4a52-9f3d-cb90be0079ea",null,` +
		`"https://flow-content.google/video/451f2cef-cdf8-47b9-9c20-db8ea17e1008_upsampled` +
		`?Expires=1789755207\u0026KeyName=labs-flow-prod-cdn-key\u0026Signature=UBOrz8aC",` +
		`"https://flow-content.google/image/451f2cef-cdf8-47b9-9c20-db8ea17e1008_upsampled` +
		`?Expires=1789755207\u0026KeyName=labs-flow-prod-cdn-key\u0026Signature=abc"]`

	media := ParseMediaOfKind(json.RawMessage(payload), "video")
	if len(media) != 1 {
		t.Fatalf("parsed %d video entries, want 1 — an upscaled URL must match", len(media))
	}
	if media[0].ContentID != "451f2cef-cdf8-47b9-9c20-db8ea17e1008_upsampled" {
		t.Errorf("ContentID = %q; the suffix is part of the id", media[0].ContentID)
	}
	if media[0].Kind != "video" {
		t.Errorf("Kind = %q, want video", media[0].Kind)
	}
	// The signed query must survive intact: dropping it makes the URL a 403.
	if !strings.Contains(media[0].URL, "Signature=UBOrz8aC") {
		t.Errorf("URL lost its signature: %q", media[0].URL)
	}
	if strings.Contains(media[0].URL, `\u0026`) {
		t.Errorf("URL still carries an escaped ampersand: %q", media[0].URL)
	}
}

// TestParseGeneratedMediaPlainURL keeps the ordinary case working.
func TestParseGeneratedMediaPlainURL(t *testing.T) {
	const payload = `"https://flow-content.google/video/1edd7a7d-1f1d-46a6-ab58-767a0db9f104?Expires=1&Signature=x"`
	media := ParseMediaOfKind(json.RawMessage(payload), "video")
	if len(media) != 1 {
		t.Fatalf("parsed %d entries, want 1", len(media))
	}
	if media[0].ContentID != "1edd7a7d-1f1d-46a6-ab58-767a0db9f104" {
		t.Errorf("ContentID = %q", media[0].ContentID)
	}
}

// TestBuildVideoArgumentImageMode pins the image-conditioned layout.
//
// Every index here is read off the app's own captured request. The two traps:
// the mode flag is 1 for an image submission and 2 for a text one, and the uuid
// block moves to the end — so writing it at a fixed index puts it in the middle
// of the image slots and the request becomes a silent no-op.
func TestBuildVideoArgumentImageMode(t *testing.T) {
	arg := buildVideoArgument(GenerateVideoRequest{
		ProjectID:    "proj-1",
		Model:        "omni_flash_i2v_8s_first_last_360p",
		Prompt:       "make videos",
		StartImage:   "start-media",
		EndImage:     "end-media",
		CaptchaToken: "0cAFcWeAtoken",
	})

	request := arg[0].([]any)[0].([]any)

	if len(request) != 7 {
		t.Fatalf("request length = %d, want 7 for a two-image submission", len(request))
	}
	if request[1] != "omni_flash_i2v_8s_first_last_360p" {
		t.Errorf("model at [1] = %v", request[1])
	}
	if request[2] != videoModeImage {
		t.Errorf("mode at [2] = %v, want %d for an image-conditioned request", request[2], videoModeImage)
	}
	if request[3] != nil {
		t.Errorf("request[3] = %v, want nil", request[3])
	}

	// Each image slot is [null, mediaId, null, null, null, <crop>].
	for i, want := range map[int]string{4: "start-media", 5: "end-media"} {
		slot, ok := request[i].([]any)
		if !ok || len(slot) < 2 {
			t.Fatalf("request[%d] should be an image slot, got %#v", i, request[i])
		}
		if slot[1] != want {
			t.Errorf("request[%d][1] = %v, want %q", i, slot[1], want)
		}
	}

	// The uuid block must be last.
	uuids, ok := request[6].([]any)
	if !ok || len(uuids) != 6 || uuids[0] != nil {
		t.Errorf("request[6] should be the uuid block, got %#v", request[6])
	}

	// The trailing batch entry stays text-mode: only the per-request flag changes.
	tail := arg[2].([]any)
	if tail[1] != videoModeText {
		t.Errorf("the trailing batch entry = %v, want %d", tail[1], videoModeText)
	}
}

// TestBuildVideoArgumentImageSlotCrop covers the optional crop.
func TestBuildVideoArgumentImageSlotCrop(t *testing.T) {
	arg := buildVideoArgument(GenerateVideoRequest{
		ProjectID:    "p",
		Model:        "m",
		Prompt:       "x",
		StartImage:   "s",
		StartFrame:   []float64{0.3418, 1, 0.6582},
		CaptchaToken: "t",
	})
	request := arg[0].([]any)[0].([]any)

	slot := request[4].([]any)
	if len(slot) != 6 {
		t.Fatalf("a slot with a crop should carry a 6th element, got %#v", slot)
	}
	crop, ok := slot[5].([]any)
	if !ok || len(crop) != 4 {
		t.Fatalf("crop = %#v, want [null, a, b, c]", slot[5])
	}
	if crop[0] != nil || crop[1] != 0.3418 {
		t.Errorf("crop = %#v", crop)
	}

	// No crop is still a crop array, encoded as a full frame [null, null, 1, 1].
	// A missing array and a full-frame array are different payloads, and the app
	// sends the second — see videoImageBlock.
	bareArg := buildVideoArgument(GenerateVideoRequest{
		ProjectID: "p", Model: "m", Prompt: "x", StartImage: "s", CaptchaToken: "t",
	})
	bareSlot := bareArg[0].([]any)[0].([]any)[4].([]any)
	if len(bareSlot) != 6 {
		t.Fatalf("a slot without a crop should still carry a 6th element, got %d", len(bareSlot))
	}
	bareCrop, ok := bareSlot[5].([]any)
	if !ok || len(bareCrop) != 4 {
		t.Fatalf("full-frame crop = %#v, want [null, null, 1, 1]", bareSlot[5])
	}
	if bareCrop[0] != nil || bareCrop[1] != nil || bareCrop[2] != 1.0 || bareCrop[3] != 1.0 {
		t.Errorf("full-frame crop = %#v, want [null, null, 1, 1]", bareCrop)
	}
}

// TestBuildVideoArgumentTextModeUnchanged keeps the text path exactly as it was:
// this change must not move anything for a plain text submission.
func TestBuildVideoArgumentTextModeUnchanged(t *testing.T) {
	arg := buildVideoArgument(GenerateVideoRequest{
		ProjectID: "p", Model: "abra_t2v_8s", Prompt: "x", CaptchaToken: "t",
	})
	request := arg[0].([]any)[0].([]any)

	if len(request) != 5 {
		t.Fatalf("a text request should stay 5 long, got %d", len(request))
	}
	if request[2] != videoModeText {
		t.Errorf("mode = %v, want %d", request[2], videoModeText)
	}
	if _, ok := request[4].([]any); !ok {
		t.Errorf("request[4] should be the uuid block, got %#v", request[4])
	}
}
