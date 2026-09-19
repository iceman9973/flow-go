package pool

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodelyx/flow-go/internal/flowapi"
)

// newTestWorker builds a worker with no client. The pool never dereferences
// Client — only the op callback does — so this is enough to exercise all of the
// scheduling logic without a network or a credential.
func newTestWorker(id string) *Worker {
	return NewWorker(id, nil)
}

func TestAcquireAndRelease(t *testing.T) {
	p := New()
	w := newTestWorker("w1")
	p.Register(w)

	got, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if got.ID != "w1" {
		t.Errorf("acquired %q, want w1", got.ID)
	}

	// The worker is at capacity only once its limit is reached.
	for i := 1; i < MaxInFlight; i++ {
		if _, err := p.Acquire(context.Background(), 0); err != nil {
			t.Fatalf("Acquire %d failed: %v", i, err)
		}
	}

	// Now it should block, since nothing has been released.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, 0); err == nil {
		t.Error("Acquire should block once the worker is at capacity")
	}

	for i := 0; i < MaxInFlight; i++ {
		p.Release(w, 0, nil)
	}

	if _, err := p.Acquire(context.Background(), 0); err != nil {
		t.Errorf("Acquire after Release should succeed: %v", err)
	}
}

func TestNoWorkers(t *testing.T) {
	p := New()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	if _, err := p.Acquire(ctx, 0); err == nil {
		t.Error("Acquire with an empty pool should fail")
	}
}

// TestFreeTierIsSpentFirst checks the routing rule the UI claimed: free daily
// credits are consumed before paid ones.
func TestFreeTierIsSpentFirst(t *testing.T) {
	p := New()
	paid := newTestWorker("paid")
	paid.SetCredits(1000, "G1_TIER1")
	free := newTestWorker("free")
	free.SetCredits(50, "G1_FREEMIUM")

	// Register the paid worker first so a naive first-match would pick it.
	p.Register(paid)
	p.Register(free)

	for i := 0; i < 5; i++ {
		w, err := p.Acquire(context.Background(), 7)
		if err != nil {
			t.Fatalf("Acquire %d failed: %v", i, err)
		}
		if w.ID != "free" {
			t.Fatalf("acquire %d chose %q, expected the free worker to be spent first", i, w.ID)
		}
		p.Release(w, 0, nil)
	}
}

func TestAffordabilitySkipsBrokeWorker(t *testing.T) {
	p := New()
	broke := newTestWorker("broke")
	broke.SetCredits(3, "G1_FREEMIUM")
	rich := newTestWorker("rich")
	rich.SetCredits(500, "G1_TIER1")
	p.Register(broke)
	p.Register(rich)

	w, err := p.Acquire(context.Background(), 15)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if w.ID != "rich" {
		t.Errorf("acquired %q, but only 'rich' can afford a 15-credit job", w.ID)
	}
}

// TestUnknownCreditsAreAffordable guards against stranding capacity: a worker
// whose balance has simply never been checked must still be usable.
func TestUnknownCreditsAreAffordable(t *testing.T) {
	p := New()
	unknown := newTestWorker("unknown")
	p.Register(unknown)

	if _, err := p.Acquire(context.Background(), 15); err != nil {
		t.Errorf("a worker with unknown credits should still be acquirable: %v", err)
	}
}

// TestCircuitBreakerParksFailingWorker is the regression test for the "circuit
// breaker" that the Python version advertised but never implemented.
func TestCircuitBreakerParksFailingWorker(t *testing.T) {
	p := New()
	w := newTestWorker("flaky")
	p.Register(w)

	for i := 0; i < FailureThreshold; i++ {
		acquired, err := p.Acquire(context.Background(), 0)
		if err != nil {
			t.Fatalf("Acquire %d failed: %v", i, err)
		}
		p.Release(acquired, 0, errors.New("upstream said no"))
	}

	if got := w.State(); got != StateCircuitOpen {
		t.Fatalf("after %d failures the worker should be parked, state = %s", FailureThreshold, got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, 0); err == nil {
		t.Error("a parked worker must not be handed out")
	}

	stats := p.Stats()
	if stats.CircuitOpen != 1 {
		t.Errorf("Stats().CircuitOpen = %d, want 1", stats.CircuitOpen)
	}
	if stats.Failed != FailureThreshold {
		t.Errorf("Stats().Failed = %d, want %d", stats.Failed, FailureThreshold)
	}
}

func TestSuccessResetsFailureStreak(t *testing.T) {
	p := New()
	w := newTestWorker("w1")
	p.Register(w)

	// Two failures, then a success: the streak resets so one more failure does
	// not park the worker.
	for i := 0; i < FailureThreshold-1; i++ {
		acquired, _ := p.Acquire(context.Background(), 0)
		p.Release(acquired, 0, errors.New("transient"))
	}
	acquired, _ := p.Acquire(context.Background(), 0)
	p.Release(acquired, 0, nil)

	acquired, _ = p.Acquire(context.Background(), 0)
	p.Release(acquired, 0, errors.New("transient"))

	if got := w.State(); got == StateCircuitOpen {
		t.Error("a success should reset the consecutive-failure streak")
	}
}

// TestExecuteFailover checks that a retryable failure moves the job to another
// worker instead of failing the request.
//
// The op fails on its first call and succeeds on its second, so the test is
// independent of which worker the scheduler happens to pick first.
func TestExecuteFailover(t *testing.T) {
	p := New()
	p.Register(newTestWorker("first"))
	p.Register(newTestWorker("second"))

	var seen []string

	err := p.Execute(context.Background(), 0, func(_ context.Context, w *Worker) error {
		seen = append(seen, w.ID)
		if len(seen) == 1 {
			return &flowapi.APIError{Status: 503, Message: "upstream unavailable"}
		}
		return nil
	})

	if err != nil {
		t.Fatalf("Execute should have failed over and succeeded, got: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 attempts, got %d (%v)", len(seen), seen)
	}
	if seen[0] == seen[1] {
		t.Errorf("failover reused the same worker: %v", seen)
	}
}

// TestExecuteDoesNotFailoverOnPermanentError checks that a non-retryable error
// stops immediately rather than burning every account.
func TestExecuteDoesNotFailoverOnPermanentError(t *testing.T) {
	p := New()
	for i := 0; i < 3; i++ {
		p.Register(newTestWorker(fmt.Sprintf("w%d", i)))
	}

	var attempts int32
	err := p.Execute(context.Background(), 0, func(context.Context, *Worker) error {
		atomic.AddInt32(&attempts, 1)
		return &flowapi.APIError{Status: 400, Message: "Invalid prompt"}
	})

	if err == nil {
		t.Fatal("expected the error to propagate")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("a permanent error should stop after 1 attempt, got %d", got)
	}
}

func TestExecuteReportsAllWorkersFailed(t *testing.T) {
	p := New()
	p.Register(newTestWorker("a"))
	p.Register(newTestWorker("b"))

	err := p.Execute(context.Background(), 0, func(context.Context, *Worker) error {
		return &flowapi.APIError{Status: 503, Message: "down"}
	})

	if err == nil {
		t.Fatal("expected an error when every worker fails")
	}
}

func TestStatsReflectRealState(t *testing.T) {
	p := New()
	stats := p.Stats()
	if stats.TotalWorkers != 0 || stats.CreditsAvailable != 0 || stats.CreditsKnown {
		t.Error("an empty pool should report zeros and no known credits")
	}

	a := newTestWorker("a")
	a.SetCredits(100, "G1_TIER1")
	b := newTestWorker("b")
	b.SetCredits(40, "G1_FREEMIUM")
	p.Register(a)
	p.Register(b)

	stats = p.Stats()
	if stats.TotalWorkers != 2 {
		t.Errorf("TotalWorkers = %d, want 2", stats.TotalWorkers)
	}
	if stats.CreditsAvailable != 140 {
		t.Errorf("CreditsAvailable = %d, want 140", stats.CreditsAvailable)
	}
	if !stats.CreditsKnown {
		t.Error("CreditsKnown should be true once a balance has been observed")
	}
	if stats.IdleWorkers != 2 {
		t.Errorf("IdleWorkers = %d, want 2", stats.IdleWorkers)
	}
}

// TestStatsDoNotInventCredits is the regression test for the reporting defect:
// the Python /stats endpoint presented 1600 credits of unit-test fixtures as
// live account analytics. An unpolled worker must report unknown, not a number.
func TestStatsDoNotInventCredits(t *testing.T) {
	p := New()
	p.Register(newTestWorker("never-checked"))

	stats := p.Stats()
	if stats.CreditsKnown {
		t.Error("CreditsKnown must be false before any balance has been observed")
	}
	if stats.CreditsAvailable != 0 {
		t.Errorf("CreditsAvailable = %d, want 0 for an unpolled pool", stats.CreditsAvailable)
	}
	if stats.Workers[0].CreditsKnown {
		t.Error("a worker's CreditsKnown must be false until a real check runs")
	}
}

func TestRoundRobinSpreadsLoad(t *testing.T) {
	p := New()
	for i := 0; i < 3; i++ {
		p.Register(newTestWorker(fmt.Sprintf("w%d", i)))
	}

	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		w, err := p.Acquire(context.Background(), 0)
		if err != nil {
			t.Fatalf("Acquire %d failed: %v", i, err)
		}
		seen[w.ID]++
		p.Release(w, 0, nil)
	}

	if len(seen) != 3 {
		t.Errorf("expected all 3 workers to be used, saw %v", seen)
	}
}

func TestRemove(t *testing.T) {
	p := New()
	p.Register(newTestWorker("a"))
	p.Register(newTestWorker("b"))

	if p.Size() != 2 {
		t.Fatalf("Size = %d, want 2", p.Size())
	}

	p.Remove("a")
	if p.Size() != 1 {
		t.Errorf("Size after Remove = %d, want 1", p.Size())
	}
	if p.Workers()[0].ID != "b" {
		t.Errorf("wrong worker survived: %s", p.Workers()[0].ID)
	}
}

func TestRegisterReplaces(t *testing.T) {
	p := New()
	p.Register(newTestWorker("same"))
	p.Register(newTestWorker("same"))

	if p.Size() != 1 {
		t.Errorf("re-registering an ID should replace, Size = %d", p.Size())
	}
}
