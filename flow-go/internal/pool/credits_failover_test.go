package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/flowapi"
)

// The pool used to hand out a worker it knew could not pay. Three separate
// things allowed that, and these pin each one: a fallback pass in `pick` that
// took any idle worker regardless of balance, a drained balance never being
// written down as zero, and an out-of-credits error being terminal instead of a
// reason to try another account.

// outOfCredits is the shape upstream returns when an account has nothing left.
func outOfCredits() error {
	return &flowapi.APIError{
		Status:  402,
		Reason:  "PUBLIC_ERROR_INSUFFICIENT_CREDITS",
		Message: "not enough credits",
	}
}

func workerByID(t *testing.T, p *Pool, id string) *Worker {
	t.Helper()
	for _, w := range p.Workers() {
		if w.ID == id {
			return w
		}
	}
	t.Fatalf("no worker %q is registered", id)
	return nil
}

// TestADrainedWorkerIsNotHandedOut is the regression test for the fallback pass.
// A single drained worker is exactly the case that pass existed for: it was the
// only idle one, so it was handed a job it could only fail.
func TestADrainedWorkerIsNotHandedOut(t *testing.T) {
	p := New()
	only := newTestWorker("drained")
	only.SetCredits(0, "G1_TIER1")
	p.Register(only)

	started := time.Now()
	got, err := p.Acquire(context.Background(), 10)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatalf("acquired %q, which is known to have no credits", got.ID)
	}

	// Structural, so it must be reported rather than waited out: a balance does
	// not recover on a timer, and sitting through the full acquire window to say
	// so is what turned a clear failure into a slow one.
	if elapsed > 2*time.Second {
		t.Errorf("Acquire took %s to report an unaffordable pool; it should not wait", elapsed)
	}

	var noWorker *NoWorkerError
	if !errors.As(err, &noWorker) {
		t.Fatalf("error is %T, want *NoWorkerError: %v", err, err)
	}
	if !strings.Contains(noWorker.Reason, "out of credits") {
		t.Errorf("reason %q should name the cause", noWorker.Reason)
	}
}

// TestUnknownCreditsAreStillHandedOut is the other half of the same rule, and
// the reason the fallback could not simply be deleted without care: an unread
// balance is not a drained one.
func TestUnknownCreditsAreStillHandedOut(t *testing.T) {
	p := New()
	p.Register(newTestWorker("unread"))

	got, err := p.Acquire(context.Background(), 10)
	if err != nil {
		t.Fatalf("a worker whose balance was never read should still be usable: %v", err)
	}
	if got.ID != "unread" {
		t.Errorf("acquired %q, want unread", got.ID)
	}
}

// TestOutOfCreditsMarksTheWorkerDrained covers the discarded-zero half: the
// balance has to actually be written down, or the worker keeps its stale
// positive figure and stays affordable forever.
func TestOutOfCreditsMarksTheWorkerDrained(t *testing.T) {
	p := New()
	w := newTestWorker("spent")
	w.SetCredits(500, "G1_TIER1")
	p.Register(w)

	p.Release(w, 0, outOfCredits())

	credits, known := w.Credits()
	if !known || credits != 0 {
		t.Errorf("credits = %d (known=%v), want a known 0", credits, known)
	}
	if w.State() != StateCircuitOpen {
		t.Errorf("state = %v, want the worker parked", w.State())
	}
	if w.Affordable(10) {
		t.Error("a drained worker must not report itself affordable")
	}
}

// TestASuccessfulJobDoesNotZeroTheBalance guards the reason the zero above is
// written explicitly rather than by relaxing recordSuccess. `Execute` releases
// with 0 on success because it did not measure a balance; reading that as
// "drained" would empty the pool on the first job.
func TestASuccessfulJobDoesNotZeroTheBalance(t *testing.T) {
	p := New()
	w := newTestWorker("healthy")
	w.SetCredits(500, "G1_TIER1")
	p.Register(w)

	p.Release(w, 0, nil)

	credits, known := w.Credits()
	if !known || credits != 500 {
		t.Errorf("credits = %d (known=%v), want the recorded 500 left alone", credits, known)
	}
	if !w.Affordable(10) {
		t.Error("a worker that just succeeded must stay affordable")
	}
}

// TestExecuteFailsOverWhenAnAccountIsOutOfCredits is the end-to-end behaviour:
// a drained account is a reason to try a different account, not a reason to fail
// the job. Out-of-credits is not `Retryable` — the same request to the same
// account would fail identically — so this also pins that `Execute` admits it as
// a failover trigger.
func TestExecuteFailsOverWhenAnAccountIsOutOfCredits(t *testing.T) {
	p := New()
	p.Register(newTestWorker("first"))
	p.Register(newTestWorker("second"))

	var tried []string
	err := p.Execute(context.Background(), 10, func(_ context.Context, w *Worker) error {
		tried = append(tried, w.ID)
		if len(tried) == 1 {
			// Whichever account is tried first is the one that cannot pay.
			return outOfCredits()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Execute should have failed over to a solvent account, got: %v", err)
	}
	if len(tried) != 2 {
		t.Fatalf("tried %v, want both accounts attempted", tried)
	}

	// The account that refused is out of rotation, so a later job does not come
	// back to it.
	drained := workerByID(t, p, tried[0])
	if credits, known := drained.Credits(); !known || credits != 0 {
		t.Errorf("the refusing worker has credits = %d (known=%v), want a known 0", credits, known)
	}
	if drained.State() != StateCircuitOpen {
		t.Errorf("the refusing worker is %v, want it parked", drained.State())
	}
}

// TestExecuteReportsEveryAccountDrained checks the all-exhausted message is
// actionable rather than a generic timeout.
func TestExecuteReportsEveryAccountDrained(t *testing.T) {
	p := New()
	a := newTestWorker("a")
	a.SetCredits(0, "G1_TIER1")
	b := newTestWorker("b")
	b.SetCredits(0, "G1_FREEMIUM")
	p.Register(a)
	p.Register(b)

	err := p.Execute(context.Background(), 10, func(context.Context, *Worker) error {
		t.Error("no op should run when no account can pay")
		return nil
	})
	if err == nil {
		t.Fatal("Execute should fail when every account is drained")
	}
	if !strings.Contains(err.Error(), "out of credits") {
		t.Errorf("error %q should say the accounts are out of credits", err)
	}
}
