package server

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/engine"
)

// TestStatusForAnEmptyResultIsNotASuccess is the #1 regression test.
//
// The transport accepted the submission and produced nothing, and the handler
// returned 200 with `status: "empty"` and `media: null`. A caller reading the
// status code saw a generation that worked. Seven of the fourteen jobs in the
// live database were recorded that way. The status code is the contract most
// callers read, so this is the assertion that matters.
func TestStatusForAnEmptyResultIsNotASuccess(t *testing.T) {
	err := &engine.EmptyResultError{
		Kind:    "video",
		Model:   "abra_t2v_8s",
		JobID:   "job-1",
		Account: "acct-1",
		Frames:  1,
		Hint:    "the token was spent",
	}

	got := statusFor(err)
	if got == 200 {
		t.Fatal("an empty generation still reports 200 — this is the defect")
	}
	if got != 502 {
		t.Errorf("statusFor(EmptyResultError) = %d, want 502 — the upstream answered "+
			"badly, which is not the caller's fault", got)
	}
}

// TestStatusForAnUnavailableEngineIs503 checks the typed path.
//
// 503 rather than 502 because the caller's next move differs: the engine cannot
// serve until something outside the process is fixed, and retrying in the
// meantime achieves nothing.
func TestStatusForAnUnavailableEngineIs503(t *testing.T) {
	err := &engine.UnavailableError{
		Reason: "the Google session expired and no browser extension is attached to refresh it",
		Hint:   "attach the browser extension, then call POST /v1/bridge/refresh and retry",
	}

	if got := statusFor(err); got != 503 {
		t.Errorf("statusFor(UnavailableError) = %d, want 503", got)
	}
}

// TestStatusForRecognisesWrappedTypedErrors matters because the pool wraps its
// own errors, and errors.As has to see through that for the mapping to hold.
func TestStatusForRecognisesWrappedTypedErrors(t *testing.T) {
	inner := &engine.UnavailableError{Reason: "the session expired"}
	wrapped := fmt.Errorf("engine: generation failed: %w", inner)

	if got := statusFor(wrapped); got != 503 {
		t.Errorf("statusFor(wrapped UnavailableError) = %d, want 503 — the typed check "+
			"must survive wrapping", got)
	}

	empty := fmt.Errorf("engine: submission failed: %w", &engine.EmptyResultError{Kind: "image"})
	if got := statusFor(empty); got != 502 {
		t.Errorf("statusFor(wrapped EmptyResultError) = %d, want 502", got)
	}
}

// TestStatusForKeepsTheStringFallbacks pins the behaviour the typed checks were
// layered on top of, so adding them did not quietly change anything else.
func TestStatusForKeepsTheStringFallbacks(t *testing.T) {
	cases := []struct {
		message string
		want    int
	}{
		{"engine: insufficient credits: 8s at 720p costs 7 and the account has 3", 402},
		{"engine: not ready — call Bootstrap first", 503},
		{"engine: no cookies available", 503},
		{"pool: no worker available — no account is registered", 503},
		{"pool: all workers failed, last error: upstream 503", 503},
		{"engine: a prompt is required", 400},
		{"batchexecute: jIps6 returned 500", 502},
	}

	for _, tc := range cases {
		got := statusFor(errors.New(tc.message))
		if got != tc.want {
			t.Errorf("statusFor(%q) = %d, want %d", tc.message, got, tc.want)
		}
	}
}

func TestStatusForNilIs200(t *testing.T) {
	if got := statusFor(nil); got != 200 {
		t.Errorf("statusFor(nil) = %d, want 200", got)
	}
}
