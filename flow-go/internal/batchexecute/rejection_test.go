package batchexecute

import (
	"strings"
	"testing"
)

// liveRefusal is a response body captured from the live endpoint on 2026-09-23,
// answering a NARWHAL image submission that the engine reported as "the
// transport returned no image (it answered with no frames at all)".
//
// It is the whole reason Frame.Error exists. The data slot really is null, so a
// parser that reads only that slot sees nothing — but the frame also carries the
// server's own diagnosis, in a sibling of the data slot, and the engine was
// discarding it and reporting the model enum and the project instead.
const liveRefusal = ")]}'\n\n" +
	"193\n" +
	`[["wrb.fr","ogiZ0b",null,null,null,[7,null,[["type.googleapis.com/google.rpc.ErrorInfo",["PUBLIC_ERROR_UNUSUAL_ACTIVITY"]]]],"generic"],["di",429],["af.httprm",429,"-2357547485598675784",26]]` + "\n" +
	"25\n" +
	`[["e",4,null,null,229]]` + "\n"

func TestParseFramesReadsTheRefusalReason(t *testing.T) {
	frames, err := ParseFrames(liveRefusal)
	if err != nil {
		t.Fatalf("ParseFrames failed: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 wrb.fr frame, got %d", len(frames))
	}

	frame := frames[0]
	if frame.RPCID != "ogiZ0b" {
		t.Errorf("RPCID = %q, want ogiZ0b", frame.RPCID)
	}
	// The data slot is genuinely empty. That is not a parsing failure; it is
	// what a refusal looks like on the wire.
	if len(frame.Payload) != 0 {
		t.Errorf("Payload = %q, want none", frame.Payload)
	}
	if frame.Error != "PUBLIC_ERROR_UNUSUAL_ACTIVITY" {
		t.Errorf("Error = %q, want PUBLIC_ERROR_UNUSUAL_ACTIVITY", frame.Error)
	}
}

// The refusal block's distance from the data slot is not a contract. The
// captured body has three leading nulls; a build that emitted two, or four,
// must still yield the reason rather than silently reading as a no-op.
func TestParseFramesFindsTheReasonAtAnyOffset(t *testing.T) {
	for _, lead := range []string{"null,", "null,null,", "null,null,null,null,"} {
		body := ")]}'\n\n193\n" +
			`[["wrb.fr","ogiZ0b",null,` + lead +
			`[7,null,[["type.googleapis.com/google.rpc.ErrorInfo",["PUBLIC_ERROR_UNUSUAL_ACTIVITY"]]]],"generic"]]` + "\n"
		frames, err := ParseFrames(body)
		if err != nil {
			t.Fatalf("leading %s: %v", lead, err)
		}
		if got := UpstreamError(frames); got != "PUBLIC_ERROR_UNUSUAL_ACTIVITY" {
			t.Errorf("leading %s: reason = %q, want it found anyway", lead, got)
		}
	}
}

// A successful response must not look like a refusal. The payload is a JSON
// string rather than nested arrays, which is what makes scanning the whole item
// safe — this pins that.
func TestParseFramesLeavesAnAnswerUnerrored(t *testing.T) {
	body := ")]}'\n\n199\n" +
		`[["wrb.fr","ogiZ0b","[[null,4,[[\"809194d7-bd63-4fc7-9554-65f0824a5a09\",\"https://flow-content.google/image/809194d7-bd63-4fc7-9554-65f0824a5a09?Expires=1790164270\u0026KeyName=labs-flow-prod-cdn-key\"]]]]",null,null,null,"generic"],["di",14]]` + "\n" +
		"25\n" +
		`[["e",4,null,null,235]]` + "\n"

	frames, err := ParseFrames(body)
	if err != nil {
		t.Fatalf("ParseFrames failed: %v", err)
	}
	if got := UpstreamError(frames); got != "" {
		t.Errorf("a successful response reported the refusal %q", got)
	}
	if len(parseMediaFrames(frames)) != 1 {
		t.Errorf("the asset was not parsed out of the response")
	}
}

func TestUpstreamErrorIsEmptyWithoutAFrame(t *testing.T) {
	if got := UpstreamError(nil); got != "" {
		t.Errorf("UpstreamError(nil) = %q, want empty", got)
	}
	if got := UpstreamError([]Frame{{RPCID: "ogiZ0b", Payload: []byte("[]")}}); got != "" {
		t.Errorf("UpstreamError of a plain frame = %q, want empty", got)
	}
}

// The message is the whole point of the type: it has to name the reason, so a
// caller that only prints the error still learns what happened.
func TestRejectedErrorNamesTheReason(t *testing.T) {
	got := (&RejectedError{Reason: "PUBLIC_ERROR_UNUSUAL_ACTIVITY", Frames: 1}).Error()
	if !strings.Contains(got, "PUBLIC_ERROR_UNUSUAL_ACTIVITY") {
		t.Errorf("Error() = %q, want it to name the reason", got)
	}
	if !strings.Contains(got, "refused") {
		t.Errorf("Error() = %q, want it to read as a refusal rather than a silence", got)
	}
}

// errorInfoReason is the shape match. A pair that merely looks similar — an
// ErrorInfo with no reason, or a reason that is not a string — must not be
// mistaken for one.
func TestErrorInfoReasonIgnoresNearMisses(t *testing.T) {
	cases := map[string]any{
		"no reason list":   []any{[]any{"type.googleapis.com/google.rpc.ErrorInfo", nil}},
		"reason not a str": []any{[]any{"type.googleapis.com/google.rpc.ErrorInfo", []any{7}}},
		"empty reason":     []any{[]any{"type.googleapis.com/google.rpc.ErrorInfo", []any{""}}},
		"unrelated pair":   []any{[]any{"type.googleapis.com/google.rpc.BadRequest", []any{"nope"}}},
		"a bare string":    "type.googleapis.com/google.rpc.ErrorInfo",
	}
	for name, value := range cases {
		if got := errorInfoReason(value); got != "" {
			t.Errorf("%s: errorInfoReason = %q, want empty", name, got)
		}
	}
}
