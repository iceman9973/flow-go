package engine

import (
	"sync"
	"testing"
	"time"
)

func TestPacingStartsAtTheBaseInterval(t *testing.T) {
	p := newSubmissionPacing(3*time.Second, maxSubmissionGap)
	if got := p.gap(); got != 3*time.Second {
		t.Errorf("gap = %v, want the base 3s", got)
	}
}

// The doubling sequence, strike by strike. Each refusal has to cost more than the
// last one: retrying at the rate that earned the refusal is what keeps earning
// it, which is the whole reason this exists.
func TestEachRefusalDoublesTheGap(t *testing.T) {
	p := newSubmissionPacing(3*time.Second, maxSubmissionGap)

	cases := []struct {
		strike int
		want   time.Duration
	}{
		{1, 6 * time.Second},
		{2, 12 * time.Second},
		{3, 24 * time.Second},
		{4, 48 * time.Second},
	}
	for _, tc := range cases {
		got, widened := p.widen()
		if !widened {
			t.Fatalf("strike %d did not widen the gap", tc.strike)
		}
		if got != tc.want {
			t.Errorf("after strike %d the gap is %v, want %v", tc.strike, got, tc.want)
		}
		if p.gap() != tc.want {
			t.Errorf("gap() disagrees with widen(): %v vs %v", p.gap(), tc.want)
		}
	}
}

// Past the cap there is nothing left to gain, and a cooldown that keeps growing
// stops being a rate correction and becomes a stall. The report is what keeps the
// log honest: a line saying the pacing moved, when it did not, is worse than no
// line at all.
func TestTheGapIsCappedAndStopsReportingChanges(t *testing.T) {
	p := newSubmissionPacing(3*time.Second, maxSubmissionGap)

	// 3 -> 6 -> 12 -> 24 -> 48 -> (96, capped to 60).
	for i := 0; i < 5; i++ {
		p.widen()
	}
	if got := p.gap(); got != maxSubmissionGap {
		t.Fatalf("gap = %v, want the cap %v", got, maxSubmissionGap)
	}

	got, widened := p.widen()
	if widened {
		t.Error("a refusal at the cap reported a change that did not happen")
	}
	if got != maxSubmissionGap {
		t.Errorf("gap = %v after a refusal at the cap, want %v", got, maxSubmissionGap)
	}
}

// A submission that is accepted puts the gap back to the base — the auto-recovery
// half of the behaviour. It reports true exactly once, so a successful run is
// quiet after the first line.
func TestAnAcceptedSubmissionRestoresTheBase(t *testing.T) {
	p := newSubmissionPacing(3*time.Second, maxSubmissionGap)

	p.widen()
	p.widen()
	if p.gap() != 12*time.Second {
		t.Fatalf("setup: gap = %v, want 12s", p.gap())
	}

	got, restored := p.reset()
	if !restored {
		t.Error("reset reported no change after two strikes")
	}
	if got != 3*time.Second {
		t.Errorf("gap = %v after reset, want the base 3s", got)
	}
	if p.gap() != 3*time.Second {
		t.Errorf("gap() = %v after reset, want the base", p.gap())
	}

	// Already at the base: nothing was restored, so nothing is logged.
	if _, restored := p.reset(); restored {
		t.Error("reset reported a change when the pacing was already at the base")
	}
}

// A non-positive base means the limiter is off, and the pacing must stay out of
// the way rather than widening from zero into a cooldown nobody configured.
func TestADisabledBaseDisablesThePacing(t *testing.T) {
	p := newSubmissionPacing(0, maxSubmissionGap)

	if got := p.gap(); got != 0 {
		t.Errorf("gap = %v, want 0", got)
	}
	if _, widened := p.widen(); widened {
		t.Error("widen reported a change with the pacing disabled")
	}
	if p.gap() != 0 {
		t.Errorf("gap = %v after widen, want 0", p.gap())
	}
}

// The engine reads the pacing through a nil-safe method, because an Engine built
// as a struct literal in a test has no pacing at all.
func TestANilPacingIsSafe(t *testing.T) {
	var p *submissionPacing

	if got := p.gap(); got != 0 {
		t.Errorf("gap = %v on a nil pacing, want 0", got)
	}
	if _, widened := p.widen(); widened {
		t.Error("widen reported a change on a nil pacing")
	}
	if _, restored := p.reset(); restored {
		t.Error("reset reported a change on a nil pacing")
	}
}

// The engine reads the gap from several goroutines — one per account, and the
// refusal callback arrives from inside an upstream call — so the arithmetic is
// behind a mutex. This is meaningful under -race.
func TestPacingIsSafeUnderConcurrentUse(t *testing.T) {
	p := newSubmissionPacing(3*time.Second, maxSubmissionGap)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				p.widen()
				_ = p.gap()
				p.reset()
			}
		}()
	}
	wg.Wait()

	// Whatever the interleaving, the gap is always one of the legal values.
	switch got := p.gap(); got {
	case 3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second, 48 * time.Second, maxSubmissionGap:
	default:
		t.Errorf("gap = %v, which is not a value the doubling can produce", got)
	}
}

// Which job statuses count as an accepted submission, and which deliberately do
// not. "empty" is the one that matters: it is what a refusal is renamed to before
// it reaches finishJob, and relaxing the cooldown on it would undo the backoff
// exactly when it is needed.
// The per-run cooldown grows with the attempt, and it has to keep growing.
//
// It was a two-case constant: 25s, then 35s, then 25s again. Nothing reached the
// third case at three attempts, so it looked right — but raising the attempt
// count would have made the fourth attempt wait exactly as long as the second,
// which is the same retry a third time.
func TestUnusualActivityCooldownGrowsWithTheAttempt(t *testing.T) {
	if got := unusualActivityCooldown(1); got != 25*time.Second {
		t.Errorf("attempt 1 cooldown = %v, want 25s", got)
	}
	if got := unusualActivityCooldown(2); got != 35*time.Second {
		t.Errorf("attempt 2 cooldown = %v, want 35s", got)
	}

	previous := time.Duration(0)
	for attempt := 1; attempt <= 6; attempt++ {
		got := unusualActivityCooldown(attempt)
		if got <= previous {
			t.Errorf("cooldown did not grow from attempt %d to %d: %v then %v",
				attempt-1, attempt, previous, got)
		}
		previous = got
	}
}

func TestSubmissionAccepted(t *testing.T) {
	accepted := []string{"submitted", "ready", "succeeded", "timeout"}
	for _, status := range accepted {
		if !submissionAccepted(status) {
			t.Errorf("submissionAccepted(%q) = false, want true", status)
		}
	}

	refused := []string{"empty", "failed", ""}
	for _, status := range refused {
		if submissionAccepted(status) {
			t.Errorf("submissionAccepted(%q) = true, want false", status)
		}
	}
}

func TestAutoRetryEnabled(t *testing.T) {
	t.Setenv("FLOW_AUTO_RETRY", "")
	if !autoRetryEnabled() {
		t.Error("autoRetryEnabled() should be true by default")
	}

	t.Setenv("FLOW_AUTO_RETRY", "0")
	if autoRetryEnabled() {
		t.Error("autoRetryEnabled() should be false when FLOW_AUTO_RETRY=0")
	}

	t.Setenv("FLOW_AUTO_RETRY", "off")
	if autoRetryEnabled() {
		t.Error("autoRetryEnabled() should be false when FLOW_AUTO_RETRY=off")
	}

	t.Setenv("FLOW_AUTO_RETRY", "1")
	if !autoRetryEnabled() {
		t.Error("autoRetryEnabled() should be true when FLOW_AUTO_RETRY=1")
	}
}
