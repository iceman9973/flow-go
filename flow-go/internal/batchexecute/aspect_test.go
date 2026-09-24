package batchexecute

import (
	"context"
	"strings"
	"testing"
)

// videoAspectSlotAt digs the aspect slot out of a built payload.
//
// The layout is [[<variation>, ...], <context-block>, [<uuid>, <mode>]], and the
// variation is [prompt-block, model, mode, <aspect>, ...] — so the slot is
// variation[3]. Reading it through the builder rather than constructing the
// array here is deliberate: a test that rebuilds the payload tests its own copy
// of the layout, which is how a positional bug survives.
func videoAspectSlotAt(t *testing.T, req GenerateVideoRequest) any {
	t.Helper()

	arg := buildVideoArgument(req)
	requests, ok := arg[0].([]any)
	if !ok || len(requests) == 0 {
		t.Fatalf("the payload carries no requests array: %#v", arg[0])
	}
	variation, ok := requests[0].([]any)
	if !ok {
		t.Fatalf("the first variation is not an array: %#v", requests[0])
	}
	if len(variation) < 4 {
		t.Fatalf("the variation is only %d long, so it has no index 3: %#v",
			len(variation), variation)
	}
	return variation[3]
}

// Unset must leave the slot null. That null is not a placeholder — it is what
// every live capture carries, and the captured-payload fixtures compare
// byte-for-byte, so writing anything here by default would break them and
// change what an unmodified caller sends.
func TestVideoAspectSlotIsNullWhenUnset(t *testing.T) {
	got := videoAspectSlotAt(t, GenerateVideoRequest{Model: "abra_t2v_8s", Prompt: "p"})
	if got != nil {
		t.Errorf("an unset aspect wrote %#v into the slot, want nil", got)
	}
}

func TestVideoAspectSlotCarriesTheWireInteger(t *testing.T) {
	cases := []struct {
		aspect string
		want   int
	}{
		{"portrait", 1},
		{"9:16", 1},
		{"landscape", 2},
		{"16:9", 2},
		{"square", 0},
		{"1:1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.aspect, func(t *testing.T) {
			got := videoAspectSlotAt(t, GenerateVideoRequest{
				Model: "abra_t2v_8s", Prompt: "p", AspectRatio: tc.aspect,
			})
			value, ok := got.(int)
			if !ok {
				t.Fatalf("the slot holds %#v, want an int", got)
			}
			if value != tc.want {
				t.Errorf("aspect %q wrote %d, want %d", tc.aspect, value, tc.want)
			}
		})
	}
}

// An explicit landscape and an absent flag are the same render but not the same
// bytes, and the difference is the whole reason the default is nil rather than
// the landscape integer. If these ever collapse into one value, the fixtures
// stop matching and the "unset" case silently starts sending a value.
func TestExplicitLandscapeIsNotTheSameAsUnset(t *testing.T) {
	unset := videoAspectSlotAt(t, GenerateVideoRequest{Model: "abra_t2v_8s", Prompt: "p"})
	explicit := videoAspectSlotAt(t, GenerateVideoRequest{
		Model: "abra_t2v_8s", Prompt: "p", AspectRatio: "landscape",
	})

	if unset != nil {
		t.Errorf("unset wrote %#v, want nil", unset)
	}
	if explicit != 2 {
		t.Errorf("explicit landscape wrote %#v, want 2", explicit)
	}
}

// The builder is total so it cannot fail halfway through constructing a
// payload; refusing an unknown name is GenerateVideo's job. This pins that
// division, so a later change that makes the builder return a default instead
// of nil is caught here rather than in an empty render.
func TestVideoAspectSlotFallsBackToNullOnAnUnknownName(t *testing.T) {
	got := videoAspectSlotAt(t, GenerateVideoRequest{
		Model: "abra_t2v_8s", Prompt: "p", AspectRatio: "widescreen",
	})
	if got != nil {
		t.Errorf("an unknown aspect wrote %#v, want nil", got)
	}
}

// The aspect is the only thing that changed: everything before it in the
// variation, and everything after it, must still be where it was.
func TestWiringTheAspectLeavesTheRestOfTheVariationAlone(t *testing.T) {
	req := GenerateVideoRequest{
		Model: "abra_t2v_8s", Prompt: "a paper boat", AspectRatio: "portrait",
	}
	variation := buildVideoArgument(req)[0].([]any)[0].([]any)

	if model, _ := variation[1].(string); model != "abra_t2v_8s" {
		t.Errorf("the model moved: %#v", variation[1])
	}
	if mode, _ := variation[2].(int); mode != videoModeText {
		t.Errorf("the mode moved: %#v, want %d", variation[2], videoModeText)
	}
	// The uuid pair is the last element, after the image slots that are absent
	// here. Its presence is what says the aspect did not push it out.
	if _, ok := variation[len(variation)-1].([]any); !ok {
		t.Errorf("the uuid block is not the last element: %#v", variation[len(variation)-1])
	}
}

// An unknown aspect is refused before the submission is built, which is what
// keeps a typo from costing a captcha token and a round trip. The error names
// both the offending value and the valid set, because a refusal that does not
// say what to type instead just moves the problem.
func TestGenerateVideoRefusesAnUnknownAspect(t *testing.T) {
	client := New(nil, nil)

	_, err := client.GenerateVideo(context.Background(), GenerateVideoRequest{
		ProjectID:    "p",
		Model:        "abra_t2v_8s",
		Prompt:       "p",
		AspectRatio:  "widescreen",
		CaptchaToken: "t",
	}, CallOptions{})

	if err == nil {
		t.Fatal("an unknown aspect was accepted")
	}
	if !strings.Contains(err.Error(), "widescreen") {
		t.Errorf("the refusal does not name the value: %v", err)
	}
	if !strings.Contains(err.Error(), "portrait") {
		t.Errorf("the refusal does not list what is valid: %v", err)
	}
}

// A valid aspect must not be refused as unknown. It will still fail — there is
// no transport behind this client — and that failure is the point: it proves
// the validation let it through rather than the test passing for the wrong
// reason.
func TestGenerateVideoDoesNotRefuseAKnownAspect(t *testing.T) {
	client := New(nil, nil)

	_, err := client.GenerateVideo(context.Background(), GenerateVideoRequest{
		ProjectID:    "p",
		Model:        "abra_t2v_8s",
		Prompt:       "p",
		AspectRatio:  "portrait",
		CaptchaToken: "t",
	}, CallOptions{})

	if err != nil && strings.Contains(err.Error(), "unknown video aspect") {
		t.Errorf("a valid aspect was refused as unknown: %v", err)
	}
}

/* ------------------------------------------------------------------ *
 * the image payload
 * ------------------------------------------------------------------ */

// imageAspectSlotAt digs the aspect slot out of a built image payload.
//
// The shape is [null, [<request>], 1, <context-block>, [<uuid>]], and the
// request is [null, null, null, <seed>, <aspect>, "<model>", ...] — so the slot
// is request[4].
func imageAspectSlotAt(t *testing.T, req GenerateRequest, seed int64) any {
	t.Helper()

	arg := buildGenerateArgument(req, seed)
	outer, ok := arg[1].([]any)
	if !ok || len(outer) == 0 {
		t.Fatalf("the payload carries no request array: %#v", arg[1])
	}
	request, ok := outer[0].([]any)
	if !ok {
		t.Fatalf("the request is not an array: %#v", outer[0])
	}
	if len(request) < 5 {
		t.Fatalf("the request is only %d long, so it has no index 4: %#v", len(request), request)
	}
	return request[4]
}

// Unset must keep the exact value a working submission already carries. This is
// the byte-level promise that adding --aspect changed nothing for a caller who
// never mentions it — the same promise the video path keeps by leaving its slot
// null, kept here by leaving this one at its default.
func TestImageAspectSlotKeepsTheDefaultWhenUnset(t *testing.T) {
	got := imageAspectSlotAt(t, GenerateRequest{Model: "NARWHAL", Prompt: "p"}, 1)
	if got != imageAspectDefault {
		t.Errorf("an unset aspect wrote %#v, want the default %d", got, imageAspectDefault)
	}
}

func TestImageAspectSlotCarriesTheWireInteger(t *testing.T) {
	cases := []struct {
		aspect string
		want   int
	}{
		{"square", 1},
		{"1:1", 1},
		{"portrait", 2},
		{"9:16", 2},
		{"landscape", 3},
		{"16:9", 3},
		{"3:4", 4},
		{"4:3", 5},
	}
	for _, tc := range cases {
		t.Run(tc.aspect, func(t *testing.T) {
			got := imageAspectSlotAt(t, GenerateRequest{
				Model: "NARWHAL", Prompt: "p", AspectRatio: tc.aspect,
			}, 1)
			value, ok := got.(int)
			if !ok {
				t.Fatalf("the slot holds %#v, want an int", got)
			}
			if value != tc.want {
				t.Errorf("aspect %q wrote %d, want %d", tc.aspect, value, tc.want)
			}
		})
	}
}

// Index 3 is the seed and index 4 is the aspect. Reading one for the other is
// the mistake this whole slot's history invites — the video path's aspect index
// is 3, and applying it here would reseed every image instead of re-aspecting
// it, silently and with a plausible-looking render as the only feedback.
func TestTheImageSeedIsNotTheAspectSlot(t *testing.T) {
	arg := buildGenerateArgument(GenerateRequest{
		Model: "NARWHAL", Prompt: "p", AspectRatio: "square",
	}, 99)
	request := arg[1].([]any)[0].([]any)

	if request[3] != int64(99) {
		t.Errorf("seed at [3] = %v, want 99", request[3])
	}
	if request[4] != 1 {
		t.Errorf("aspect at [4] = %v, want 1 (square)", request[4])
	}
}

// The two enums are independent, so the same word must land on different
// integers on the two paths. If this ever sees them equal, one table has been
// substituted for the other.
func TestLandscapeLandsOnDifferentIntegersOnTheTwoPaths(t *testing.T) {
	video := videoAspectSlotAt(t, GenerateVideoRequest{
		Model: "abra_t2v_8s", Prompt: "p", AspectRatio: "landscape",
	})
	image := imageAspectSlotAt(t, GenerateRequest{
		Model: "NARWHAL", Prompt: "p", AspectRatio: "landscape",
	}, 1)

	if video != 2 {
		t.Errorf("landscape on the video path = %v, want 2", video)
	}
	if image != 3 {
		t.Errorf("landscape on the image path = %v, want 3", image)
	}
	if video == image {
		t.Errorf("landscape is %v on both paths; the enums are independent and "+
			"this table pair should not be merged", video)
	}
}

// An unrecognised name falls back to the default rather than to null: null is
// not a value this slot has been observed to accept, and the builder has to be
// total. Generate refuses the name long before this is reached.
func TestImageAspectSlotFallsBackToTheDefaultOnAnUnknownName(t *testing.T) {
	got := imageAspectSlotAt(t, GenerateRequest{
		Model: "NARWHAL", Prompt: "p", AspectRatio: "widescreen",
	}, 1)
	if got != imageAspectDefault {
		t.Errorf("an unknown aspect wrote %#v, want the default %d", got, imageAspectDefault)
	}
}

func TestGenerateRefusesAnUnknownImageAspect(t *testing.T) {
	client := New(nil, nil)

	_, err := client.Generate(context.Background(), GenerateRequest{
		ProjectID:    "p",
		Model:        "NARWHAL",
		Prompt:       "p",
		AspectRatio:  "widescreen",
		CaptchaToken: "t",
	}, CallOptions{})

	if err == nil {
		t.Fatal("an unknown image aspect was accepted")
	}
	if !strings.Contains(err.Error(), "widescreen") {
		t.Errorf("the refusal does not name the value: %v", err)
	}
	if !strings.Contains(err.Error(), "square") {
		t.Errorf("the refusal does not list what is valid: %v", err)
	}
}
