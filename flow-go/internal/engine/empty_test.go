package engine

import (
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/batchexecute"
)

// TestVideoEmptyErrorReadsTheReasonFromTheFrames pins the contract the video
// paths depend on.
//
// They build their user-facing message from the refusal's frames, which is where
// the reason and its hint come from. That only works while the transport hands
// the frames back alongside the refusal — it used to return nil with the error,
// which turned a named refusal into a bare "nothing came back", and this is the
// test that says so.
func TestVideoEmptyErrorReadsTheReasonFromTheFrames(t *testing.T) {
	e := identityEngine(t, Options{})

	refused := e.videoEmptyError("video", "abra_t2v_8s", "job-1", []batchexecute.Frame{{
		RPCID: "YhhmEf",
		Error: batchexecute.ReasonUnusualActivity,
	}})

	if refused.Rejected != batchexecute.ReasonUnusualActivity {
		t.Errorf("Rejected = %q, want %q", refused.Rejected, batchexecute.ReasonUnusualActivity)
	}
	if refused.Frames != 1 {
		t.Errorf("Frames = %d, want 1 — a refusal is a frame that came back, not silence", refused.Frames)
	}
	if !strings.Contains(refused.Error(), batchexecute.ReasonUnusualActivity) {
		t.Errorf("Error() = %q, want it to name the reason", refused.Error())
	}

	// The reason buys a different hint, which is most of what the reason is for.
	silent := e.videoEmptyError("video", "abra_t2v_8s", "job-1", nil)
	if refused.Hint == silent.Hint {
		t.Error("a refused video got the same hint as a silent one; the reason-specific " +
			"hint is the point of carrying the reason at all")
	}
}

// TestEmptyResultErrorCarriesTheDiagnosis checks the message is actually usable.
//
// The old behaviour was a 200 with `status: "empty"` and `media: null`, which
// told the caller nothing at all: no model, no job id, no account, no reason.
// Seven of the fourteen jobs in the live database were recorded that way, and
// from the outside they were indistinguishable from a success still in flight.
func TestEmptyResultErrorCarriesTheDiagnosis(t *testing.T) {
	err := &EmptyResultError{
		Kind:    "video",
		Model:   "abra_t2v_8s",
		JobID:   "8f14e45fceea167a5a36dedd4bea2543",
		Account: "acct-8c4008fc1143",
		Frames:  1,
		Hint:    "the reCAPTCHA token was single-use",
	}

	got := err.Error()

	for _, want := range []string{
		"video",             // what was asked for
		"abra_t2v_8s",       // which model
		"8f14e45f",          // which job, so it can be found in generations
		"acct-8c4008fc1143", // which account it ran on
		"no media id",       // what came back
		"single-use",        // why it probably happened
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, should mention %q", got, want)
		}
	}
}

// TestEmptyResultErrorDistinguishesNoFramesFromNoMediaID covers the distinction
// the Frames field exists for.
//
// "The transport answered with nothing" and "the transport answered with
// something we could not read" have different causes, and collapsing them into
// one message sends the reader after the wrong one.
func TestEmptyResultErrorDistinguishesNoFramesFromNoMediaID(t *testing.T) {
	silent := (&EmptyResultError{Kind: "image", Frames: 0}).Error()
	unreadable := (&EmptyResultError{Kind: "image", Frames: 3}).Error()

	if !strings.Contains(silent, "no frames at all") {
		t.Errorf("Error() = %q, want it to say the transport was silent", silent)
	}
	if !strings.Contains(unreadable, "3 frame(s)") {
		t.Errorf("Error() = %q, want it to report how many frames came back", unreadable)
	}
	if silent == unreadable {
		t.Error("the two cases produced identical messages; the distinction is lost")
	}
}

// TestEmptyResultErrorToleratesMissingContext checks the message degrades
// cleanly, since not every call site knows every field.
func TestEmptyResultErrorToleratesMissingContext(t *testing.T) {
	got := (&EmptyResultError{Kind: "image"}).Error()

	if !strings.Contains(got, "image") {
		t.Errorf("Error() = %q, want it to name the kind", got)
	}
	// No stray separators from the fields that were not supplied.
	if strings.Contains(got, ", ,") || strings.HasSuffix(got, ", ") {
		t.Errorf("Error() = %q has a dangling separator", got)
	}
	if strings.Contains(got, ". ") {
		t.Errorf("Error() = %q has a dangling hint separator", got)
	}
}

// TestImageHintIsNotTheVideoHint guards against the hints being copy-pasted.
//
// They name different causes on purpose: an image call returns the asset inline,
// so an empty frame means the request was understood and declined, whereas a
// video submission can come back empty for reasons specific to the submission.
// Handing the reader the wrong one is what the video hint's own comment records
// happening twice already.
func TestImageHintIsNotTheVideoHint(t *testing.T) {
	e := identityEngine(t, Options{})

	image := e.emptyImageHint()
	video := e.emptySubmissionHint()

	if image == video {
		t.Fatal("the image and video hints are identical; one of them is wrong")
	}
	if !strings.Contains(image, "inline") {
		t.Errorf("the image hint should say the asset comes back inline, got %q", image)
	}
	if !strings.Contains(video, "reCAPTCHA") {
		t.Errorf("the video hint should name the token, got %q", video)
	}
}
