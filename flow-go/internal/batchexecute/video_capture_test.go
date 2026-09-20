package batchexecute

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// i2vCapture is the app's own image-to-video submission, captured live by driving
// the composer with a first and last frame set. Generated uuids and the reCAPTCHA
// token are masked so the fixture is stable.
const i2vCapture = `[[[[null, null, [[["make videos"]]]], "omni_flash_i2v_8s_first_last_360p", 1, null, [null, "<UUID>", null, null, null, [null, 0.34176829268292686, 1, 0.6582317073170731]], [null, "<UUID>", null, null, null, [null, 0.341796875, 1, 0.658203125]], [null, null, null, null, "<UUID>", "<UUID>"]]], [null, 22, null, null, null, "<UUID>", null, null, null, null, ["<CAPTCHA>", 1]], ["<UUID>", 2]]`

// TestBuildVideoArgumentMatchesCapture is the strongest check available for this
// payload: it rebuilds the argument from the same inputs and requires the two to
// be identical once generated uuids are masked.
//
// It is worth having because the layout *looks* right when it is wrong. An
// image-conditioned submission whose layout is correct but whose RPC id is the
// text one is accepted with a 200 and returns an empty result, so nothing short
// of a byte comparison catches a regression here.
func TestBuildVideoArgumentMatchesCapture(t *testing.T) {
	var captured []any
	if err := json.Unmarshal([]byte(i2vCapture), &captured); err != nil {
		t.Fatalf("the fixture is not valid JSON: %v", err)
	}
	captcha := captured[1].([]any)[10].([]any)[0].(string)

	ours := buildVideoArgument(GenerateVideoRequest{
		ProjectID:    "d86bc0b4-30dc-4a52-9f3d-cb90be0079ea",
		Model:        "omni_flash_i2v_8s_first_last_360p",
		Prompt:       "make videos",
		StartImage:   "a8f10d63-9ec8-4344-ae0b-174b9c3672ef",
		EndImage:     "1edd7a7d-1f1d-46a6-ab58-767a0db9f104",
		StartFrame:   []float64{0.34176829268292686, 1, 0.6582317073170731},
		EndFrame:     []float64{0.341796875, 1, 0.658203125},
		CaptchaToken: captcha,
	})

	// Mask generated uuids and undo Go's HTML escaping of the placeholder, so the
	// comparison is about structure rather than about which encoder ran last.
	mask := func(v any) string {
		b, _ := json.Marshal(v)
		re := regexp.MustCompile(`(?i)"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"`)
		out := re.ReplaceAllString(string(b), `"<UUID>"`)
		out = strings.ReplaceAll(out, `\u003c`, "<")
		return strings.ReplaceAll(out, `\u003e`, ">")
	}

	got, want := mask(ours), mask(captured)
	if got == want {
		return
	}
	t.Errorf("the payload does not match the captured submission\n ours: %s\n app : %s",
		strings.TrimSpace(got), strings.TrimSpace(want))
}

// startOnlyCapture is the app's own first-frame-only submission, captured live by
// filling only the Start chip of the composer. Generated uuids and the reCAPTCHA
// token are masked.
//
// Note the differences from i2vCapture: the model is abra_i2v_8s rather than an
// omni_flash_*_first_last_* key, there is one image slot rather than two, and the
// frame is encoded as a full-frame crop [null, null, 1, 1].
const startOnlyCapture = `[[[[null, null, [[["a wooden boat with the word zqxjkv painted on its side"]]]], "abra_i2v_8s", 2, null, [null, "<UUID>", null, null, null, [null, null, 1, 1]], [null, null, null, null, "<UUID>", "<UUID>"]]], [null, 22, null, null, null, "<UUID>", null, null, null, null, ["<CAPTCHA>", 1]], ["<UUID>", 2]]`

// TestBuildVideoArgumentMatchesStartOnlyCapture rebuilds the first-frame-only
// submission from the same inputs and requires it to equal the capture.
//
// The mode is pinned to the captured 2 here rather than left to the default,
// because the two captures disagree: i2vCapture carries 1 and this one carries 2
// for the same "conditioned on images" meaning. Both are accepted in practice, so
// the default is left alone and only the part that is unambiguous — the layout and
// the frame encoding — is asserted by comparison.
func TestBuildVideoArgumentMatchesStartOnlyCapture(t *testing.T) {
	var captured []any
	if err := json.Unmarshal([]byte(startOnlyCapture), &captured); err != nil {
		t.Fatalf("the fixture is not valid JSON: %v", err)
	}
	captcha := captured[1].([]any)[10].([]any)[0].(string)
	mode := captured[0].([]any)[0].([]any)[2].(float64)

	ours := buildVideoArgument(GenerateVideoRequest{
		ProjectID:    "d86bc0b4-30dc-4a52-9f3d-cb90be0079ea",
		Model:        "abra_i2v_8s",
		Prompt:       "a wooden boat with the word zqxjkv painted on its side",
		StartImage:   "4f926d5d-24c6-4fe6-bd28-09a8391048dd",
		CaptchaToken: captcha,
		ModeOverride: func() *int { m := int(mode); return &m }(),
	})

	mask := func(v any) string {
		b, _ := json.Marshal(v)
		re := regexp.MustCompile(`(?i)"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"`)
		out := re.ReplaceAllString(string(b), `"<UUID>"`)
		out = strings.ReplaceAll(out, `\u003c`, "<")
		return strings.ReplaceAll(out, `\u003e`, ">")
	}

	got, want := mask(ours), mask(captured)
	if got == want {
		return
	}
	t.Errorf("the payload does not match the captured submission\n ours: %s\n app : %s",
		strings.TrimSpace(got), strings.TrimSpace(want))
}

// TestVideoRPCID pins the id each conditioning shape goes to.
//
// This is the regression that cost the most time: a first-frame-only submission
// sent to nprQif is accepted with a 200 and returns an empty result, so every
// payload-level check passed while the feature was dead.
func TestVideoRPCID(t *testing.T) {
	cases := []struct {
		name string
		req  GenerateVideoRequest
		want string
	}{
		{"text only", GenerateVideoRequest{}, RPCIDGenerateVideo},
		{"start frame only", GenerateVideoRequest{StartImage: "a"}, RPCIDGenerateVideoImageStart},
		{"both frames", GenerateVideoRequest{StartImage: "a", EndImage: "b"}, RPCIDGenerateVideoImage},
		{"end frame only", GenerateVideoRequest{EndImage: "b"}, RPCIDGenerateVideoImage},
	}
	for _, tc := range cases {
		if got := VideoRPCID(tc.req); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}

	if RPCIDGenerateVideoImageStart == RPCIDGenerateVideoImage ||
		RPCIDGenerateVideoImageStart == RPCIDGenerateVideo {
		t.Fatal("the three conditioning shapes must not share an RPC id")
	}
}

// TestVideoImageBlockFullFrame pins the uncropped encoding. A missing crop array
// and a full-frame crop are different payloads, and only the second is what the
// app sends.
func TestVideoImageBlockFullFrame(t *testing.T) {
	got, _ := json.Marshal(videoImageBlock("m", nil))
	if want := `[null,"m",null,null,null,[null,null,1,1]]`; string(got) != want {
		t.Errorf("uncropped block: got %s, want %s", got, want)
	}

	got, _ = json.Marshal(videoImageBlock("m", []float64{0.25, 1, 0.75}))
	if want := `[null,"m",null,null,null,[null,0.25,1,0.75]]`; string(got) != want {
		t.Errorf("cropped block: got %s, want %s", got, want)
	}
}

// editCapture is the app's own video edit submission, captured live by attaching
// an existing asset in the Ingredients composer mode and generating. The source,
// b8864807-…, is a *video* in the project, which is what makes this the
// video-to-video path rather than an image-conditioned one. Generated uuids and
// the reCAPTCHA token are masked.
const editCapture = `[[[[null, "b8864807-d1ec-476c-ac5b-eb2b919e526e", 0, 192], [null, null, [[["a wooden boat with the word zqxjkv painted on its side"]]]], "abra_edit", 2, [null, null, null, null, "<UUID>", "<UUID>"]]], [null, 22, null, null, null, "<UUID>", null, null, null, null, ["<CAPTCHA>", 1]], ["<UUID>", 2]]`

// TestBuildEditArgumentMatchesCapture rebuilds the edit submission and requires it
// to equal the capture.
//
// Worth pinning because this payload is shaped differently from a generation: the
// source block sits at index 0, ahead of the prompt, and the request has five
// elements rather than the generation's longer list. A generation layout sent to
// jIps6 is accepted and returns an empty result, so nothing short of a comparison
// catches a mix-up between the two builders.
func TestBuildEditArgumentMatchesCapture(t *testing.T) {
	var captured []any
	if err := json.Unmarshal([]byte(editCapture), &captured); err != nil {
		t.Fatalf("the fixture is not valid JSON: %v", err)
	}
	captcha := captured[1].([]any)[10].([]any)[0].(string)

	ours := buildEditArgument(EditVideoRequest{
		ProjectID:    "d86bc0b4-30dc-4a52-9f3d-cb90be0079ea",
		SourceID:     "b8864807-d1ec-476c-ac5b-eb2b919e526e",
		Prompt:       "a wooden boat with the word zqxjkv painted on its side",
		Model:        "abra_edit",
		TrimStart:    0,
		TrimEnd:      192,
		CaptchaToken: captcha,
	})

	mask := func(v any) string {
		b, _ := json.Marshal(v)
		re := regexp.MustCompile(`(?i)"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"`)
		out := re.ReplaceAllString(string(b), `"<UUID>"`)
		out = strings.ReplaceAll(out, `\u003c`, "<")
		return strings.ReplaceAll(out, `\u003e`, ">")
	}

	got, want := mask(ours), mask(captured)
	if got == want {
		return
	}
	t.Errorf("the payload does not match the captured submission\n ours: %s\n app : %s",
		strings.TrimSpace(got), strings.TrimSpace(want))
}

// referenceCapture is the app's own reference-image submission, captured live from
// the browser while generating from four project images. Generated uuids and the
// reCAPTCHA token are masked; the zero-tailed constant at the end is kept verbatim,
// because its value is the point of keeping it.
const referenceCapture = `[[[[null, null, [[["make video "]]]], [[null, "<UUID>"], [null, "<UUID>"], [null, "<UUID>"], [null, "<UUID>"]], "abra_r2v_4s", 2, null, [null, null, null, null, "<UUID>", "<UUID>"], null, null, null, null, [["d351dd3c-0a12-1522-0000-000000000000"]]]], [null, 22, null, null, null, "<UUID>", null, null, null, null, ["<CAPTCHA>", 1]], ["<UUID>", 2]]`

// TestBuildReferenceArgumentMatchesCapture rebuilds the reference-image submission
// and requires it to equal the capture.
//
// This one matters more than the others, because a wrong answer here does not look
// wrong: an `abra_r2v_*` model sent to the single-image RPC is accepted and returns
// a media id. Only a byte comparison against the capture separates the real path
// from one that merely answers.
func TestBuildReferenceArgumentMatchesCapture(t *testing.T) {
	var captured []any
	if err := json.Unmarshal([]byte(referenceCapture), &captured); err != nil {
		t.Fatalf("the fixture is not valid JSON: %v", err)
	}
	captcha := captured[1].([]any)[10].([]any)[0].(string)

	ours := buildReferenceArgument(ReferenceVideoRequest{
		ProjectID: "96bdb77f-8ba7-44e3-939c-c6f0a7ed2d7e",
		Model:     "abra_r2v_4s",
		Prompt:    "make video ",
		References: []string{
			"181b4689-9f6a-484a-9c84-8d55badf4996",
			"fca8f6d4-00ff-4d62-a0a0-09744179d5ed",
			"f164b0e3-1f9e-4cba-a0ef-518c7ce76398",
			"d7075b03-9b3f-48fe-99ab-b8ae9e2b7d04",
		},
		CaptchaToken: captcha,
	})

	// Mask generated uuids but leave the zero-tailed constant alone, so this test
	// also pins its value.
	uuidRe := regexp.MustCompile(`(?i)"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"`)
	mask := func(v any) string {
		b, _ := json.Marshal(v)
		out := uuidRe.ReplaceAllStringFunc(string(b), func(m string) string {
			if strings.HasSuffix(m, `-0000-000000000000"`) {
				return m
			}
			return `"<UUID>"`
		})
		out = strings.ReplaceAll(out, `\u003c`, "<")
		return strings.ReplaceAll(out, `\u003e`, ">")
	}

	got, want := mask(ours), mask(captured)
	if got == want {
		return
	}
	t.Errorf("the payload does not match the captured submission\n ours: %s\n app : %s",
		strings.TrimSpace(got), strings.TrimSpace(want))
}

// TestReferenceShapeIsNotTheImageShape pins the difference that made r2v look
// solved when it was not.
//
// A reference submission puts the prompt at index 0 and the image list at index 1;
// the single-image shape puts the prompt at index 2 and each image in a trailing
// slot. The server accepts either, so the distinction has to be asserted rather
// than observed.
func TestReferenceShapeIsNotTheImageShape(t *testing.T) {
	ref := buildReferenceArgument(ReferenceVideoRequest{
		ProjectID: "p", Model: "abra_r2v_4s", Prompt: "x",
		References: []string{"a"}, CaptchaToken: "t",
	})
	img := buildVideoArgument(GenerateVideoRequest{
		ProjectID: "p", Model: "abra_i2v_4s", Prompt: "x",
		StartImage: "a", CaptchaToken: "t",
	})

	refReq := ref[0].([]any)[0].([]any)
	imgReq := img[0].([]any)[0].([]any)

	if len(refReq) != 11 {
		t.Errorf("a reference request should carry 11 slots, got %d", len(refReq))
	}
	if _, ok := refReq[1].([]any); !ok {
		t.Fatalf("the reference list should sit at index 1, got %#v", refReq[1])
	}
	if model, ok := refReq[2].(string); !ok || model != "abra_r2v_4s" {
		t.Errorf("the model should sit at index 2, got %#v", refReq[2])
	}
	if len(refReq) == len(imgReq) {
		t.Errorf("the two shapes should differ in length, both are %d", len(refReq))
	}
}
