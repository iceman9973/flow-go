package pool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/flowapi"
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

// TestAcquireWithNoWorkersFailsImmediately is the fast-fail regression test.
//
// An empty pool cannot become non-empty by waiting: a worker only appears when
// Bootstrap registers one, which is not something the wait can cause. The loop
// used to run regardless, so a request arriving after the engine dropped its
// worker — which is what happens when the session is lost — sat for the full
// 30-second deadline before reporting what was already known.
func TestAcquireWithNoWorkersFailsImmediately(t *testing.T) {
	p := New()

	start := time.Now()
	_, err := p.Acquire(context.Background(), 0)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Acquire with an empty pool should fail")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Acquire took %s to report an empty pool; it should not wait at all", elapsed)
	}

	var noWorker *NoWorkerError
	if !errors.As(err, &noWorker) {
		t.Fatalf("error is %T, want *NoWorkerError so the cause travels with it", err)
	}
	// `statusFor` used to map this phrase to a 503; it went with the HTTP API.
	// The phrase is still the one a caller or a log grep matches on, and the
	// reason is appended to it, so it has to survive the append.
	if !strings.Contains(err.Error(), "no worker available") {
		t.Errorf("error %q must keep the phrase callers match on", err)
	}
	if !strings.Contains(err.Error(), "not bootstrapped") {
		t.Errorf("error %q should name the likely cause, not just the symptom", err)
	}
}

// TestAcquireFailsFastWhenEveryWorkerIsParked covers the other structural case.
//
// The circuit cooldown is 90 seconds and the acquire deadline is 30, so a parked
// worker cannot possibly come back before the deadline — waiting for it is a
// guaranteed 30-second failure. The error names when it does come back instead,
// which is more useful than making the caller wait to find out.
func TestAcquireFailsFastWhenEveryWorkerIsParked(t *testing.T) {
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
	if w.State() != StateCircuitOpen {
		t.Fatalf("the fixture did not park the worker: state = %s", w.State())
	}

	start := time.Now()
	_, err := p.Acquire(context.Background(), 0)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a parked worker must not be handed out")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Acquire took %s on a worker parked for %s; the wait cannot succeed so it "+
			"should not be paid", elapsed, CircuitCooldown)
	}

	var noWorker *NoWorkerError
	if !errors.As(err, &noWorker) {
		t.Fatalf("error is %T, want *NoWorkerError", err)
	}
	if noWorker.RetryAfter.IsZero() {
		t.Error("a parked worker has a known return time; the error should carry it")
	}
	if !strings.Contains(err.Error(), "parked") {
		t.Errorf("error %q should say the worker is parked rather than just absent", err)
	}
}

// TestAcquireStillWaitsForABusyWorker guards the case the wait exists for.
//
// Fail-fast on the structural cases is only correct if it does not also break
// the transient one: every worker busy is a wait worth paying, because one of
// them is going to finish. Narrowing this too far would turn ordinary contention
// into a spurious failure.
func TestAcquireStillWaitsForABusyWorker(t *testing.T) {
	p := New()
	w := newTestWorker("w1")
	p.Register(w)

	// Fill the worker to its limit.
	for i := 0; i < MaxInFlight; i++ {
		if _, err := p.Acquire(context.Background(), 0); err != nil {
			t.Fatalf("Acquire %d failed: %v", i, err)
		}
	}

	// Free a slot shortly after the caller starts waiting.
	go func() {
		time.Sleep(150 * time.Millisecond)
		p.Release(w, 0, nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := p.Acquire(ctx, 0)
	if err != nil {
		t.Fatalf("Acquire should have waited for the busy worker and succeeded: %v", err)
	}
	if got.ID != "w1" {
		t.Errorf("acquired %q, want w1", got.ID)
	}
}

// TestExecuteOnAnEmptyPoolFailsImmediately covers the same defect one layer up,
// on the entry point generation code is meant to use.
func TestExecuteOnAnEmptyPoolFailsImmediately(t *testing.T) {
	p := New()

	start := time.Now()
	err := p.Execute(context.Background(), 0, func(context.Context, *Worker) error {
		t.Error("the op must not run when there is no worker")
		return nil
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Execute on an empty pool should fail")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Execute took %s to report an empty pool", elapsed)
	}
	if !strings.Contains(err.Error(), "no worker available") {
		t.Errorf("error %q must keep the phrase callers match on", err)
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

// TestRefreshCreditsUsesTheAuthoritativeReader covers the wiring that left the
// pool's affordability layer inert.
//
// The workers here carry a nil Client, and that is the assertion: reading a
// balance from the worker's own client would dereference nil and panic. Only a
// read through the installed reader can make these tests pass.
func TestRefreshCreditsUsesTheAuthoritativeReader(t *testing.T) {
	p := New()
	w := newTestWorker("w1")

	calls := 0
	w.SetCreditsReader(func(context.Context) (Balance, error) {
		calls++
		return Balance{Credits: 1050, SKU: "G1_TIER1"}, nil
	})
	p.Register(w)

	p.RefreshCredits(context.Background())

	if calls != 1 {
		t.Errorf("reader called %d times, want 1", calls)
	}
	credits, known := w.Credits()
	if !known {
		t.Fatal("creditsKnown is false after a successful read — the balance reads as unknown")
	}
	if credits != 1050 {
		t.Errorf("credits = %d, want 1050", credits)
	}
	if w.SKU() != "G1_TIER1" {
		t.Errorf("SKU = %q, want G1_TIER1", w.SKU())
	}
}

// TestRefreshCreditsInventsNothing covers both ways a balance stays unknown.
// Neither may produce a number, because a fabricated balance is worse than an
// absent one: the scheduler would route on it.
func TestRefreshCreditsInventsNothing(t *testing.T) {
	p := New()

	noReader := newTestWorker("no-reader")
	p.Register(noReader)

	failing := newTestWorker("failing")
	failing.SetCreditsReader(func(context.Context) (Balance, error) {
		return Balance{Credits: 999}, errors.New("nzlxg answered 401")
	})
	p.Register(failing)

	p.RefreshCredits(context.Background())

	for _, w := range []*Worker{noReader, failing} {
		if credits, known := w.Credits(); known {
			t.Errorf("%s: creditsKnown is true with credits %d — a balance was invented", w.ID, credits)
		}
	}
}

// TestRefreshCreditsKeepsATierItWasNotToldAbout: the balance RPC carries no
// subscription tier, so a reader with none to offer must not be read as claiming
// the account has none. Clearing it would silently switch off the free-tier
// routing preference.
func TestRefreshCreditsKeepsATierItWasNotToldAbout(t *testing.T) {
	p := New()
	w := newTestWorker("w1")

	offerSKU := true
	w.SetCreditsReader(func(context.Context) (Balance, error) {
		if offerSKU {
			return Balance{Credits: 1050, SKU: "G1_TIER1"}, nil
		}
		return Balance{Credits: 900}, nil
	})
	p.Register(w)

	p.RefreshCredits(context.Background())
	if w.SKU() != "G1_TIER1" {
		t.Fatalf("SKU = %q, want G1_TIER1 after the first read", w.SKU())
	}

	offerSKU = false
	p.RefreshCredits(context.Background())

	if w.SKU() != "G1_TIER1" {
		t.Errorf("SKU = %q, want it preserved — an absent tier is not a claim of no tier", w.SKU())
	}
	if credits, _ := w.Credits(); credits != 900 {
		t.Errorf("credits = %d, want 900 — the balance should still update", credits)
	}
}

// TestRefreshCreditsMakesAffordabilityReal is the point of the whole change.
// Before it nothing set creditsKnown, so Affordable answered true for every
// worker and a job the account could not pay for was scheduled to it anyway.
func TestRefreshCreditsMakesAffordabilityReal(t *testing.T) {
	p := New()
	w := newTestWorker("w1")

	// Unknown is affordable on purpose: refusing would strand capacity.
	if !w.Affordable(50) {
		t.Fatal("an unknown balance should be treated as affordable")
	}

	w.SetCreditsReader(func(context.Context) (Balance, error) {
		return Balance{Credits: 12}, nil
	})
	p.Register(w)
	p.RefreshCredits(context.Background())

	if w.Affordable(50) {
		t.Error("a 12-credit balance should not be affordable for a 50-credit job")
	}
	if !w.Affordable(12) {
		t.Error("a 12-credit balance should be affordable for a 12-credit job")
	}
}

// TestExecuteKeepsABalanceSetDuringTheOp covers the contract the generation path
// depends on.
//
// The video closure records the authoritative post-submit balance on its worker
// from inside the op. Execute then finishes by calling Release(worker, 0, nil),
// and that zero must not undo the write. If Release ever started recording its
// argument unconditionally, every successful render would reset the pool to a
// zero balance and pick() would stop routing to a perfectly healthy account —
// the same class of failure as the inert gate this layer was fixed for.
func TestExecuteKeepsABalanceSetDuringTheOp(t *testing.T) {
	p := New()
	w := newTestWorker("w1")
	w.SetCredits(1050, "G1_TIER1")
	p.Register(w)

	err := p.Execute(context.Background(), 0, func(_ context.Context, worker *Worker) error {
		worker.SetCredits(1040, "")
		return nil
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	credits, known := w.Credits()
	if !known {
		t.Fatal("creditsKnown is false after the op recorded a balance")
	}
	if credits != 1040 {
		t.Errorf("credits = %d, want 1040 — Release(0) overwrote what the op recorded", credits)
	}
	if w.SKU() != "G1_TIER1" {
		t.Errorf("SKU = %q, want it preserved — a balance carries no tier", w.SKU())
	}
	if got := p.Stats().CreditsAvailable; got != 1040 {
		t.Errorf("CreditsAvailable = %d, want 1040 — the gate is reading a stale figure", got)
	}
}
