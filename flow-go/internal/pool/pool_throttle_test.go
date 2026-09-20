package pool

import (
	"context"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/flowapi"
)

// TestThrottleParksOnTheFirstFailure pins the rule that separates a throttle from
// an ordinary failure.
//
// A 429 — and the 403 UNUSUAL_ACTIVITY soft throttle, which is the same condition
// wearing a different status — means the server is asking this account to slow
// down. Retrying is what turns that into a harder throttle, so the worker is
// parked immediately rather than after FailureThreshold strikes.
func TestThrottleParksOnTheFirstFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"429", &flowapi.APIError{Status: 429, Message: "rate limited"}},
		{"403 UNUSUAL_ACTIVITY", &flowapi.APIError{Status: 403, Reason: "PUBLIC_ERROR_UNUSUAL_ACTIVITY"}},
	} {
		w := newTestWorker("w1")
		w.recordFailure(tc.err)

		if w.State() != StateCircuitOpen {
			t.Errorf("%s: a throttle should park the worker on the first failure, got %s",
				tc.name, w.State())
		}
	}
}

// TestOrdinaryFailuresStillNeedAStreak is the other half of the rule: one
// transient error must not cost a worker its capacity.
func TestOrdinaryFailuresStillNeedAStreak(t *testing.T) {
	w := newTestWorker("w1")
	for i := 1; i < FailureThreshold; i++ {
		w.recordFailure(&flowapi.APIError{Status: 500, Message: "server error"})
		if w.State() == StateCircuitOpen {
			t.Fatalf("worker parked after %d failure(s); the threshold is %d",
				i, FailureThreshold)
		}
	}

	w.recordFailure(&flowapi.APIError{Status: 500, Message: "server error"})
	if w.State() != StateCircuitOpen {
		t.Errorf("worker should be parked after %d consecutive failures, got %s",
			FailureThreshold, w.State())
	}
}

// TestUnrelatedErrorsDoNotLookLikeThrottles guards the classifier. A 403 without
// the UNUSUAL_ACTIVITY reason is a real refusal, not back-pressure, and must not
// be given the throttle treatment.
func TestUnrelatedErrorsDoNotLookLikeThrottles(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"plain 403", &flowapi.APIError{Status: 403, Reason: "PERMISSION_DENIED"}},
		{"500", &flowapi.APIError{Status: 500}},
		{"a non-API error", context.DeadlineExceeded},
		{"nil", nil},
	} {
		w := newTestWorker("w1")
		w.recordFailure(tc.err)

		if w.State() == StateCircuitOpen {
			t.Errorf("%s: should not park a worker on the first failure", tc.name)
		}
	}
}

// TestThrottleCooldownIsShorterThanCircuitCooldown keeps the two durations apart.
// A throttle clears on its own, so holding the account out of rotation for longer
// than the throttle lasts only wastes capacity.
func TestThrottleCooldownIsShorterThanCircuitCooldown(t *testing.T) {
	if ThrottleCooldown >= CircuitCooldown {
		t.Errorf("a throttled worker should return sooner than a broken one: %s >= %s",
			ThrottleCooldown, CircuitCooldown)
	}
	if ThrottleCooldown <= 0 {
		t.Errorf("ThrottleCooldown must be positive, got %s", ThrottleCooldown)
	}
}

// TestThrottleParksThroughRelease exercises the path the engine actually uses,
// rather than only the internal helper.
func TestThrottleParksThroughRelease(t *testing.T) {
	p := New()
	w := newTestWorker("w1")
	p.Register(w)

	if _, err := p.Acquire(context.Background(), 0); err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	p.Release(w, 0, &flowapi.APIError{Status: 429, Message: "rate limited"})

	if w.State() != StateCircuitOpen {
		t.Errorf("Release with a 429 should park the worker, got %s", w.State())
	}
}
