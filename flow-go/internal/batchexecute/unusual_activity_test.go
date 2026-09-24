package batchexecute

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

/*
 * Reporting a refusal as a rate signal.
 *
 * NoteUnusualActivity exists because the engine has to be able to slow itself
 * down, and a refusal is the only signal it gets: the answer is HTTP 200 with the
 * reason buried in the frame, so there is no status to react to and nothing to
 * retry against.
 *
 * The reason it is not simply folded into EscalateCaptcha is the browserless run.
 * That one has no page to escalate to, so EscalateCaptcha is nil — and it is
 * exactly the run with no other way to learn the rate is the problem.
 */

// The hook has to fire when no escalator is configured. This is the browserless
// case, and the case the cooldown is for.
func TestTheRateHookFiresWithoutAnEscalator(t *testing.T) {
	_, submit := recorder(refusal(ReasonUnusualActivity))

	var fired int
	opts := CallOptions{
		RefreshCaptcha:      func(context.Context) (string, error) { return "transport-2", nil },
		NoteUnusualActivity: func() { fired++ },
	}

	if _, err := retryIfEmpty(context.Background(), opts, nil, carriesNothing, submit); err == nil {
		t.Fatal("the refusal should still be returned as an error")
	}
	if fired != 1 {
		t.Errorf("the hook fired %d times, want exactly 1", fired)
	}
}

// It fires alongside the escalator too, rather than instead of it: the escalation
// governs this attempt, and the cooldown governs the next generation. A run that
// only escalated would arrive at the next generation at the same rate.
func TestTheRateHookFiresAlongsideEscalation(t *testing.T) {
	_, submit := recorder(refusal(ReasonUnusualActivity), answer())

	var fired int
	opts := escalateOpts(func(context.Context) (string, error) { return "page-token", nil })
	opts.NoteUnusualActivity = func() { fired++ }

	if _, err := retryIfEmpty(context.Background(), opts, nil, carriesNothing, submit); err != nil {
		t.Fatalf("the escalated attempt should have succeeded: %v", err)
	}
	if fired != 1 {
		t.Errorf("the hook fired %d times, want exactly 1", fired)
	}
}

// Any other reason is not a rate signal. Widening the pacing for a wrong model or
// a project belonging to another account would slow a run down over something
// that waiting cannot fix.
func TestTheRateHookIgnoresOtherReasons(t *testing.T) {
	_, submit := recorder(refusal("PUBLIC_ERROR_MODEL_ACCESS_DENIED"))

	var fired int
	opts := CallOptions{
		RefreshCaptcha:      func(context.Context) (string, error) { return "transport-2", nil },
		NoteUnusualActivity: func() { fired++ },
	}

	if _, err := retryIfEmpty(context.Background(), opts, nil, carriesNothing, submit); err == nil {
		t.Fatal("the refusal should be returned as an error")
	}
	if fired != 0 {
		t.Errorf("the hook fired %d times for a non-rate refusal, want 0", fired)
	}
}

func TestTheRateHookDoesNotFireOnAnAcceptedCall(t *testing.T) {
	_, submit := recorder(answer())

	var fired int
	opts := CallOptions{NoteUnusualActivity: func() { fired++ }}

	if _, err := retryIfEmpty(context.Background(), opts, nil, carriesNothing, submit); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fired != 0 {
		t.Errorf("the hook fired %d times on an accepted call, want 0", fired)
	}
}

// Nil is the default, so a call that does not set the hook has to behave exactly
// as it did before the hook existed — the refusal is returned and nothing panics.
func TestTheRateHookIsOptional(t *testing.T) {
	_, submit := recorder(refusal(ReasonUnusualActivity))

	opts := CallOptions{RefreshCaptcha: func(context.Context) (string, error) { return "t", nil }}

	if _, err := retryIfEmpty(context.Background(), opts, nil, carriesNothing, submit); err == nil {
		t.Fatal("the refusal should be returned as an error")
	}
}

// captureLog runs fn with the standard logger redirected, and returns what it
// wrote.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	restore := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(restore)

	fn()
	return buf.String()
}

// An initial refusal by the assessment is not retried.
//
// A fresh token does not clear it: a live run showed every attempt made inside
// the same refused window refused too — including one minted from a real page —
// while the same prompt succeeded once the caller waited. So the retry bought
// nothing and cost a submission against an account that was already being
// refused. One submission per refusal is the point.
func TestAnInitialAssessmentRefusalIsNotRetried(t *testing.T) {
	tokens, submit := recorder(answer())

	var fired int
	opts := CallOptions{
		RefreshCaptcha:      func(context.Context) (string, error) { return "transport-2", nil },
		NoteUnusualActivity: func() { fired++ },
	}

	var err error
	got := captureLog(t, func() {
		_, err = retryIfEmpty(context.Background(), opts, refusal(ReasonUnusualActivity),
			carriesNothing, submit)
	})

	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("want a RejectedError, got %v", err)
	}
	if rejected.Reason != ReasonUnusualActivity {
		t.Errorf("reason = %q, want the reason the server gave", rejected.Reason)
	}
	if len(*tokens) != 0 {
		t.Errorf("the server saw %d attempts after the first; an assessment refusal must "+
			"not be retried", len(*tokens))
	}
	if fired != 1 {
		t.Errorf("the hook fired %d times, want exactly 1 — with the retry gone this is the "+
			"only place the engine can learn the rate is the problem", fired)
	}
	if strings.Contains(got, "retrying once") {
		t.Errorf("the log says it retried, and it did not:\n%s", got)
	}
}

// The escalation is not reached for an initial refusal either — it is the same
// wasted round trip by another route, and the cooldown is what recovers the run.
func TestAnInitialAssessmentRefusalIsNotEscalated(t *testing.T) {
	tokens, submit := recorder(answer())

	escalated := 0
	opts := escalateOpts(func(context.Context) (string, error) {
		escalated++
		return "page-token", nil
	})

	if _, err := retryIfEmpty(context.Background(), opts, refusal(ReasonUnusualActivity),
		carriesNothing, submit); err == nil {
		t.Fatal("the refusal should be returned as an error")
	}
	if escalated != 0 {
		t.Errorf("the escalator was asked for a token %d times; an initial refusal goes "+
			"straight back to the caller's cooldown", escalated)
	}
	if len(*tokens) != 0 {
		t.Errorf("the server saw %d attempts after the first, want 0", len(*tokens))
	}
}

// A first response that states some *other* reason is still retried — a spent
// token is the common cause of an empty answer and a fresh one clears it — and
// the log has to name what the first response said.
//
// A live run printed "nothing came back" for both attempts and only named the
// reason on the third line, which left an ordinary empty response and a stated
// refusal indistinguishable except by how fast they returned: about a second
// against the twenty-odd a real render takes.
func TestANonRateReasonIsStillRetriedAndReported(t *testing.T) {
	_, submit := recorder(answer())
	opts := CallOptions{RefreshCaptcha: func(context.Context) (string, error) { return "t", nil }}

	var err error
	got := captureLog(t, func() {
		_, err = retryIfEmpty(context.Background(), opts,
			refusal("PUBLIC_ERROR_MODEL_ACCESS_DENIED"), carriesNothing, submit)
	})
	if err != nil {
		t.Fatalf("the retry should have succeeded: %v", err)
	}
	if !strings.Contains(got, "PUBLIC_ERROR_MODEL_ACCESS_DENIED") {
		t.Errorf("the log does not name the first response's reason:\n%s", got)
	}
	if !strings.Contains(got, "retrying once") {
		t.Errorf("the log does not report the retry that was made:\n%s", got)
	}
}

// The refusal's frames come back alongside the error, not instead of it.
//
// The video paths build their user-facing message from those frames — that is
// where PUBLIC_ERROR_UNUSUAL_ACTIVITY and its hint come from — so returning nil
// with the error turns a named refusal into a bare "nothing came back". This is
// the contract that makes suppressing the retry safe to do at all.
func TestTheRefusalFramesComeBackWithTheError(t *testing.T) {
	_, submit := recorder(answer())
	opts := CallOptions{RefreshCaptcha: func(context.Context) (string, error) { return "t", nil }}

	frames, err := retryIfEmpty(context.Background(), opts, refusal(ReasonUnusualActivity),
		carriesNothing, submit)
	if err == nil {
		t.Fatal("the refusal should be returned as an error")
	}
	if len(frames) == 0 {
		t.Fatal("the frames were dropped with the error, and the caller reads its reason from them")
	}
	if got := UpstreamError(frames); got != ReasonUnusualActivity {
		t.Errorf("the frames carry %q, want %q", got, ReasonUnusualActivity)
	}
}

// The refusal has more than one spelling, and the family is the server's to
// extend.
//
// A live run showed both inside a single call — the first response
// PUBLIC_ERROR_UNUSUAL_ACTIVITY_TOO_MUCH_TRAFFIC, the retry the plain one. The
// suffixed form is the same refusal with the rate named, so treating it as an
// ordinary error disabled the retry suppression, the pacing hook, the escalation
// and the cooldown all at once.
func TestIsUnusualActivityMatchesTheFamily(t *testing.T) {
	accepted := []string{
		ReasonUnusualActivity,
		ReasonUnusualActivity + "_TOO_MUCH_TRAFFIC",
		ReasonUnusualActivity + "_SOMETHING_THE_SERVER_ADDS_LATER",
	}
	for _, reason := range accepted {
		if !IsUnusualActivity(reason) {
			t.Errorf("IsUnusualActivity(%q) = false, want true", reason)
		}
	}

	refused := []string{
		"",
		"PUBLIC_ERROR_MODEL_ACCESS_DENIED",
		// A truncation of the constant is not a member of the family, and
		// neither is the constant with anything prepended.
		"PUBLIC_ERROR_UNUSUAL",
		"X" + ReasonUnusualActivity,
	}
	for _, reason := range refused {
		if IsUnusualActivity(reason) {
			t.Errorf("IsUnusualActivity(%q) = true, want false", reason)
		}
	}
}

// The suffixed refusal is suppressed exactly like the plain one. This is the
// behaviour the family check exists for, and the one the live run was getting
// wrong: it made the blind retry, which is the submission the suppression was
// added to stop.
func TestTheSuffixedRefusalIsNotRetriedEither(t *testing.T) {
	tokens, submit := recorder(answer())

	var fired int
	opts := CallOptions{
		RefreshCaptcha:      func(context.Context) (string, error) { return "transport-2", nil },
		NoteUnusualActivity: func() { fired++ },
	}

	_, err := retryIfEmpty(context.Background(), opts,
		refusal(ReasonUnusualActivity+"_TOO_MUCH_TRAFFIC"), carriesNothing, submit)

	if err == nil {
		t.Fatal("the refusal should be returned as an error")
	}
	if len(*tokens) != 0 {
		t.Errorf("the server saw %d attempts after the first; the suffixed refusal must not "+
			"be retried either", len(*tokens))
	}
	if fired != 1 {
		t.Errorf("the hook fired %d times, want exactly 1 — the suffixed refusal is a rate "+
			"signal too", fired)
	}
}

// SetSubmissionGap must not disturb the in-flight cap.
//
// It is a separate setter for that reason: re-sending the limits to change the
// gap allocates a new semaphore channel, and a call already holding a slot on the
// old one would release into the new one — handing back capacity it never took.
func TestSetSubmissionGapLeavesTheCapAlone(t *testing.T) {
	client := New(nil, nil)
	client.SetSubmissionLimits(4, 3*time.Second)

	before := client.subSlots
	client.SetSubmissionGap(12 * time.Second)

	if client.subSlots != before {
		t.Error("SetSubmissionGap replaced the semaphore channel; a call holding a slot " +
			"would release into the new one")
	}
	if client.subGap != 12*time.Second {
		t.Errorf("subGap = %v, want 12s", client.subGap)
	}
}

// When the first response really was silent, the old line is still the right one.
// Inventing a reason would be worse than saying nothing.
func TestASilentFirstResponseStillReadsAsSilent(t *testing.T) {
	_, submit := recorder(answer())
	opts := CallOptions{RefreshCaptcha: func(context.Context) (string, error) { return "t", nil }}

	var err error
	got := captureLog(t, func() {
		_, err = retryIfEmpty(context.Background(), opts, []Frame{{RPCID: RPCIDGenerate}},
			carriesNothing, submit)
	})
	if err != nil {
		t.Fatalf("the retry should have succeeded: %v", err)
	}
	if !strings.Contains(got, "nothing came back") {
		t.Errorf("the log does not report a silent first response:\n%s", got)
	}
	if strings.Contains(got, "stated") {
		t.Errorf("the log claims a reason that was never stated:\n%s", got)
	}
}
