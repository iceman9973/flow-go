package engine

import (
	"os"
	"strings"
	"sync"
	"time"
)

// autoRetryEnabled reports whether the engine should wait out transient
// PUBLIC_ERROR_UNUSUAL_ACTIVITY cooldowns and auto-retry submissions.
func autoRetryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLOW_AUTO_RETRY"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// unusualActivityCooldown is how long a single run waits before retrying a
// submission the assessment refused, by attempt number.
//
// It grows by a fixed step rather than being a two-case constant. Two cases are
// all that is reachable at three attempts, and they silently oscillate the
// moment the attempt count is raised: 25s, 35s, 25s, 35s. A formula cannot, and
// the growth is the whole point — the second wait has to be longer than the
// first or the retry is just the same attempt again.
//
// The step is deliberately gentler than submissionPacing's doubling, and the two
// are not the same clock. That one governs how closely two *different*
// generations may follow each other, and it is read when a client is built.
// This one is a single run waiting out a refusal before trying the same
// generation again — and the observed recovery is tens of seconds, so doubling
// here would spend a minute and a half asleep on the third attempt.
func unusualActivityCooldown(attempt int) time.Duration {
	return time.Duration(15+10*attempt) * time.Second
}

// maxSubmissionGap caps the adaptive cooldown.
//
// A ceiling rather than unbounded doubling: past a minute the wait stops being a
// rate correction and starts being a stall, and a run that has been flagged that
// hard is better off ending with a clear error than sitting silent for minutes
// between attempts that are still going to be refused.
const maxSubmissionGap = 60 * time.Second

// submissionPacing is the engine's adaptive cooldown on submission rate.
//
// It exists because PUBLIC_ERROR_UNUSUAL_ACTIVITY is the one signal that says
// the *rate* is the problem, and it is also the only signal available: the
// refusal arrives as HTTP 200 with the reason buried in the response frame, so
// there is no status code to react to and nothing to retry against. The only
// place to act is before the next call goes out — which is why this widens the
// gap the submission limiter spaces calls by, rather than delaying one retry.
//
// Each refusal doubles the gap and an accepted submission puts it back to the
// base. The doubling is what makes a flagged run recover instead of hammering:
// retrying at the rate that earned the refusal keeps earning it.
//
// The state lives on the Engine, not on a client, because a client is built per
// generation (see newBatchexecuteClient) — a gap remembered on one would be
// discarded by the next.
type submissionPacing struct {
	mu      sync.Mutex
	base    time.Duration
	max     time.Duration
	strikes int
}

// newSubmissionPacing builds a pacing that starts at base. A non-positive base
// means the limiter is off, and this stays out of the way entirely.
func newSubmissionPacing(base, max time.Duration) *submissionPacing {
	return &submissionPacing{base: base, max: max}
}

// gap is the interval submissions should be spaced by right now.
func (p *submissionPacing) gap() time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gapLocked()
}

// gapLocked is base doubled once per strike, clamped to max.
//
// The loop is bounded by the clamp rather than by an arbitrary strike count, and
// strikes can only grow while the gap is below the cap, so the arithmetic cannot
// overflow no matter how many refusals arrive.
func (p *submissionPacing) gapLocked() time.Duration {
	if p.base <= 0 {
		return 0
	}
	gap := p.base
	for i := 0; i < p.strikes; i++ {
		gap *= 2
		if gap >= p.max {
			return p.max
		}
	}
	return gap
}

// widen doubles the gap after a refusal and reports the interval now in force,
// plus whether it changed.
//
// A refusal that arrives when the gap is already at the cap reports false: there
// is nothing left to widen, and logging a change that did not happen would make
// the cooldown look like it was still responding.
func (p *submissionPacing) widen() (time.Duration, bool) {
	if p == nil {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	current := p.gapLocked()
	if p.base <= 0 || current >= p.max {
		return current, false
	}
	p.strikes++
	return p.gapLocked(), true
}

// reset returns the gap to the base interval and reports whether it had been
// widened.
//
// The report is what keeps the log honest: a line on every successful generation
// would be noise, and noise is what teaches a reader to stop reading.
func (p *submissionPacing) reset() (time.Duration, bool) {
	if p == nil {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.strikes == 0 {
		return p.gapLocked(), false
	}
	p.strikes = 0
	return p.gapLocked(), true
}

// submissionAccepted reports whether a finished job's status means the
// submission itself was accepted, which is what the pacing governs.
//
// The statuses are read off the four generation paths, not invented:
//
//   - "submitted" is what a video carries when the media ids came back and
//     nothing waited for the render, which is every run without --wait. The
//     nothing-came-back case is renamed to "empty" before it reaches here, so
//     "submitted" cannot mean "accepted nothing".
//   - "ready" is a video that was also polled and downloaded.
//   - "succeeded" is an image.
//   - "timeout" means the render outran the poll, not that the submission was
//     refused — the rate was fine.
//
// "empty" and "failed" are deliberately absent. Neither proves the rate is
// acceptable, and relaxing the cooldown on a run that is still being refused
// would undo the backoff exactly when it is needed.
func submissionAccepted(status string) bool {
	switch status {
	case "submitted", "ready", "succeeded", "timeout":
		return true
	}
	return false
}
