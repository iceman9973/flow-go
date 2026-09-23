package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/batchexecute"
)

// A refused submission used to be reported as a silent no-op, with a hint that
// named the model enum and the project. Both were correct. This pins that the
// refusal now arrives with the server's own reason on it, and that the hint
// stops sending the reader after things that are not wrong.
func TestRefusedSubmissionNamesTheServerReason(t *testing.T) {
	err := &EmptyResultError{
		Kind:     "image",
		Model:    "NARWHAL",
		JobID:    "8f14e45fceea167a5a36dedd4bea2543",
		Account:  "acct-h5a40db955d8d",
		Frames:   1,
		Rejected: reasonUnusualActivity,
		Hint:     refusedHint(reasonUnusualActivity),
	}

	got := err.Error()

	for _, want := range []string{
		reasonUnusualActivity, // what the server actually said
		"NARWHAL",             // which model, so the job can be found
		"8f14e45f",
		"acct-h5a40db955d8d",
		"refused", // that it was a refusal rather than a silence
		"broker",  // and the way out
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, should mention %q", got, want)
		}
	}

	// The empty-result reading is the one that misled. It must not survive on
	// a refusal.
	if strings.Contains(got, "no frames at all") {
		t.Errorf("Error() = %q still reports a refusal as silence", got)
	}
	if strings.Contains(got, "Check that the model enum") {
		t.Errorf("Error() = %q still carries the model-and-project hint", got)
	}
}

// The two hints describe different problems and must not converge.
func TestRefusedHintIsNotTheEmptyHint(t *testing.T) {
	e := identityEngine(t, Options{})

	refused := refusedHint(reasonUnusualActivity)
	empty := e.emptyImageHint()

	if refused == empty {
		t.Fatal("the refusal and empty hints are identical; one of them is wrong")
	}
	if !strings.Contains(refused, "reCAPTCHA") {
		t.Errorf("the refusal hint should name the assessment, got %q", refused)
	}
	if strings.Contains(refused, "model enum is one this account can reach") {
		t.Errorf("the refusal hint repeats the empty hint's advice: %q", refused)
	}
}

// An unnamed reason still gets a usable hint — it just cannot be specific.
func TestHintForRejectionFallsBackWhenNoReasonWasGiven(t *testing.T) {
	fallback := "the usual empty-result hint"

	if got := hintForRejection("", fallback); got != fallback {
		t.Errorf("hintForRejection(\"\") = %q, want the fallback", got)
	}

	// A reason the engine does not know is still better than the fallback: it
	// is what the server said.
	unknown := hintForRejection("SOMETHING_NEW", fallback)
	if unknown == fallback {
		t.Error("a stated reason was replaced by the generic hint")
	}
	if !strings.Contains(unknown, "SOMETHING_NEW") {
		t.Errorf("the hint dropped the reason the server gave: %q", unknown)
	}
}

// The video paths read the reason off the frames they already hold, so all
// three of them report a refusal the same way.
func TestVideoEmptyErrorCarriesTheRefusal(t *testing.T) {
	e := identityEngine(t, Options{})

	refused := e.videoEmptyError("video", "abra_t2v_4s", "job-1", []batchexecute.Frame{
		{RPCID: "rpc", Error: reasonUnusualActivity},
	})
	if refused.Rejected != reasonUnusualActivity {
		t.Errorf("Rejected = %q, want the server's reason", refused.Rejected)
	}
	if refused.Frames != 1 {
		t.Errorf("Frames = %d, want 1 — a refusal is an answer, not a silence", refused.Frames)
	}
	if !strings.Contains(refused.Error(), reasonUnusualActivity) {
		t.Errorf("Error() = %q, want it to name the reason", refused.Error())
	}

	// No reason on the frames: the previous behaviour, unchanged.
	plain := e.videoEmptyError("video", "abra_t2v_4s", "job-2", []batchexecute.Frame{{RPCID: "rpc"}})
	if plain.Rejected != "" {
		t.Errorf("Rejected = %q, want empty for a silent answer", plain.Rejected)
	}
	if !strings.Contains(plain.Error(), "no media id") {
		t.Errorf("Error() = %q, want the silent-answer wording kept", plain.Error())
	}
	if plain.Hint != e.emptySubmissionHint() {
		t.Error("a silent answer should keep the empty-submission hint")
	}
}

// A transport failure is still a failure: it must not be swallowed into the
// empty-result path, which would report a broken call as a declined request.
func TestRejectedErrorIsRecognisable(t *testing.T) {
	var rejected *batchexecute.RejectedError
	err := error(&batchexecute.RejectedError{Reason: reasonUnusualActivity, Frames: 1})

	if !errors.As(err, &rejected) {
		t.Fatal("a RejectedError should be found with errors.As")
	}
	if rejected.Reason != reasonUnusualActivity {
		t.Errorf("Reason = %q", rejected.Reason)
	}
}
