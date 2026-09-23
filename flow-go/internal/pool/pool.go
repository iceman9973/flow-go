// Package pool schedules generation work across accounts.
//
// Every generation in this engine goes through the pool. That is deliberate: in
// the Python version the pool existed but nothing called it — register_worker,
// acquire_worker, and execute_with_failover had no production callers, so the
// headline "multi-account failover" feature was display-only and /stats reported
// a pool that routed nothing.
//
// The routing rules here are the ones the UI claimed:
//
//	least-busy    — prefer the worker with the fewest in-flight jobs
//	sticky        — a worker that just served an account is preferred for it
//	circuit break — a worker that keeps failing is parked for a cooldown
//	throttle      — a worker the server throttled is parked at once, briefly
//	failover      — a retryable failure moves the job to another worker
//	affordability — a worker that cannot pay for the job is skipped
package pool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/flowapi"
	"github.com/kodelyx/flow-go/flow-go/internal/recaptcha"
)

// State is a worker's availability.
type State string

const (
	// StateIdle means the worker can take a job.
	StateIdle State = "idle"
	// StateBusy means the worker is at its concurrency limit.
	StateBusy State = "busy"
	// StateCircuitOpen means the worker is parked after repeated failures.
	StateCircuitOpen State = "circuit_open"
)

// Defaults for the circuit breaker.
const (
	// FailureThreshold is how many consecutive failures park a worker.
	FailureThreshold = 3
	// CircuitCooldown is how long a parked worker stays out of rotation.
	CircuitCooldown = 90 * time.Second
	// ThrottleCooldown is the shorter park applied when the server throttled the
	// account rather than the request being wrong.
	//
	// Shorter than CircuitCooldown on purpose: a throttle clears on its own, and
	// keeping the account out of rotation for longer than the throttle lasts
	// only wastes capacity.
	ThrottleCooldown = 45 * time.Second
	// CreditsCooldown is how long an account that ran out of credits stays out of
	// rotation.
	//
	// Far longer than the others, because this is not a transient condition: a
	// balance does not recover on a timer, so re-admitting the worker quickly
	// would only hand it another job it cannot pay for. The cooldown exists to
	// name the reason in the pool's "nothing available" message; the balance
	// itself is what actually keeps the worker out.
	CreditsCooldown = 15 * time.Minute
	// MaxInFlight is the per-worker concurrency ceiling.
	MaxInFlight = 2
)

// Balance is one authoritative credit reading for an account.
type Balance struct {
	// Credits is the account's spendable balance.
	Credits int
	// SKU is the subscription tier, when the reader knows it.
	//
	// Empty leaves the stored tier alone. The balance RPC does not carry one —
	// it comes from the Labs session, which is the legacy bearer path and may
	// itself be unavailable — so a reader that has no tier must not be read as
	// claiming the account has none.
	SKU string
}

// CreditsReader reads one account's authoritative balance.
//
// Supplied by the caller rather than taken from Worker.Client, and that is the
// whole point. Client is the legacy aisandbox REST surface, so the balance it
// reports belongs to whichever account that credential happens to be for —
// Engine.Credits records that it "reports a different number, and the token used
// for it can belong to a different signed-in account entirely, so it is not a
// source to trust." Reading it here would record one account's credits against
// another. The authoritative read is batchexecute `nzlxg`.
type CreditsReader func(ctx context.Context) (Balance, error)

// Worker is one account's client plus its scheduling state.
type Worker struct {
	ID     string
	Client *flowapi.Client

	// Concurrency is this worker's in-flight ceiling. Zero means MaxInFlight.
	Concurrency int

	mu               sync.Mutex
	reader           CreditsReader
	inFlight         int
	credits          int
	creditsKnown     bool
	sku              string
	consecutiveFails int
	circuitOpenUntil time.Time
	lastUsed         time.Time
	served           int64
	failed           int64

	// projectID is the Flow project this account generates into.
	//
	// Per worker rather than per engine, because it is a property of the signed-in
	// account and not of the process: two accounts signed into the same browser
	// have different project lists, and a project id resolved under one is not
	// necessarily addressable by the other. Held behind the mutex because the
	// engine sets it once at registration while generation reads it per job.
	projectID string

	// jar is this account's cookies, and captcha mints its reCAPTCHA tokens.
	//
	// Both are per worker for the same reason projectID is: they belong to the
	// signed-in account rather than to the process. A generation routed to this
	// worker has to open with this account's cookies and a token minted in the
	// browser profile that is signed into it — a token minted under one profile
	// and spent under another is rejected with no error at all, just an empty
	// result, which is the failure mode this whole struct exists to avoid.
	//
	// Behind the mutex for the same reason as projectID: written once at
	// registration, read per job.
	jar     *cookiejar.Jar
	captcha recaptcha.Provider
}

// SetJar records the cookies this account generates with.
//
// Call it before the worker is registered, alongside SetCreditsReader and
// SetProjectID, so no job can be routed to a worker whose cookies are missing.
func (w *Worker) SetJar(jar *cookiejar.Jar) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.jar = jar
}

// Jar returns this account's cookies, or nil when none were recorded.
func (w *Worker) Jar() *cookiejar.Jar {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.jar
}

// SetCaptcha records the provider that mints this account's reCAPTCHA tokens.
func (w *Worker) SetCaptcha(provider recaptcha.Provider) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.captcha = provider
}

// Captcha returns this account's token provider, or nil when none was recorded.
//
// A nil provider is not an error: it means this account has no way to mint one,
// and the caller falls back the same way it does when the provider itself fails.
func (w *Worker) Captcha() recaptcha.Provider {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.captcha
}

// NewWorker wraps a client.
func NewWorker(id string, client *flowapi.Client) *Worker {
	return &Worker{ID: id, Client: client, Concurrency: MaxInFlight}
}

// SetCreditsReader installs the authoritative balance reader for this worker.
//
// Call it before the worker is registered. A worker with no reader keeps its
// balance unknown, and the pool leaves it that way rather than substituting a
// figure from the untrusted source.
func (w *Worker) SetCreditsReader(fn CreditsReader) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reader = fn
}

// creditsReader returns the installed reader, or nil when there is none.
func (w *Worker) creditsReader() CreditsReader {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reader
}

// SetCredits records a freshly observed balance.
func (w *Worker) SetCredits(credits int, sku string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.credits = credits
	w.creditsKnown = true
	if sku != "" {
		w.sku = sku
	}
}

// Credits returns the last observed balance and whether it is known.
func (w *Worker) Credits() (int, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.credits, w.creditsKnown
}

// SKU returns the account's subscription tier.
func (w *Worker) SKU() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sku
}

// SetProjectID records the Flow project this account generates into.
//
// Call it before the worker is registered, alongside SetCreditsReader, so no job
// can be routed to a worker whose project is still unknown.
func (w *Worker) SetProjectID(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.projectID = id
}

// ProjectID returns the project this account generates into.
//
// Empty means the project was never resolved, which is a state the caller has to
// handle rather than treat as a value: the engine's project resolution can come
// back empty when neither the listing nor the browser yielded one, and an empty
// project id sent upstream is refused as an unusual request rather than as a
// missing field.
//
// The worker's own client already carries this project — flowapi.New bakes it
// into Client.opts at construction — so this is not what makes a routed call
// generate into the right place. It exists so the *result* can name the project
// the job actually ran in, which the engine-level project cannot once more than
// one account is registered.
func (w *Worker) ProjectID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.projectID
}

// State reports the worker's current availability.
func (w *Worker) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	if time.Now().Before(w.circuitOpenUntil) {
		return StateCircuitOpen
	}
	if w.inFlight >= w.limit() {
		return StateBusy
	}
	return StateIdle
}

func (w *Worker) limit() int {
	if w.Concurrency > 0 {
		return w.Concurrency
	}
	return MaxInFlight
}

// Affordable reports whether the worker can pay for a job of the given cost.
// An unknown balance is treated as affordable: refusing to schedule a worker
// whose credits have simply not been checked yet would strand capacity.
func (w *Worker) Affordable(cost int) bool {
	if cost <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.creditsKnown {
		return true
	}
	return w.credits >= cost
}

// IsFree reports whether the account is on the free tier. Free credits renew
// daily, so they are spent before paid ones.
func (w *Worker) IsFree() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sku != "" && w.sku != "G1_TIER1"
}

func (w *Worker) acquire() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if time.Now().Before(w.circuitOpenUntil) {
		return false
	}
	if w.inFlight >= w.limit() {
		return false
	}
	w.inFlight++
	w.lastUsed = time.Now()
	return true
}

func (w *Worker) release() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight > 0 {
		w.inFlight--
	}
}

// recordSuccess clears the failure streak and, when the caller measured one,
// records the new balance.
//
// The `credits > 0` guard is load-bearing, and is not the discarded zero it
// looks like. `Release` is called with 0 by every caller that did not read a
// balance — `Execute` does so on both its success and failure paths — so a bare
// 0 means "not measured", not "empty". Treating it as a balance would mark every
// worker drained on its first job and empty the pool. A real zero arrives
// through SetCredits, or through recordFailure when the server says the account
// cannot pay.
func (w *Worker) recordSuccess(credits int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.served++
	w.consecutiveFails = 0
	w.circuitOpenUntil = time.Time{}
	if credits > 0 {
		w.credits = credits
		w.creditsKnown = true
	}
}

// recordFailure parks the worker when the failure warrants it.
//
// A throttle parks immediately, on the first one. The server is asking this
// account to slow down, and a retry is what turns a soft throttle into a hard
// one — so waiting for a streak would mean deliberately causing the damage
// before reacting to it.
//
// Anything else has to repeat FailureThreshold times first, because one
// transient error should not cost a worker its capacity.
func (w *Worker) recordFailure(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failed++
	w.consecutiveFails++

	// A drained account is the one failure that is a definite statement about the
	// balance rather than about the request. Record it as a known zero — which is
	// what actually keeps the worker out of `pick`, because `Affordable` answers
	// false for a known balance below the cost — and park it so the pool can name
	// the reason instead of letting the caller sit out the whole acquire deadline
	// and then report a bare timeout.
	//
	// This is done here rather than by loosening `recordSuccess`'s guard on
	// purpose. The zero that `Execute` passes to `Release` means "this job did
	// not measure a balance", not "the balance is zero"; recording it would take
	// every worker out of rotation on its first successful job. The balance is
	// only written from a source that actually knows it — this, or SetCredits.
	if flowapi.IsOutOfCredits(err) {
		w.credits = 0
		w.creditsKnown = true
		w.circuitOpenUntil = time.Now().Add(CreditsCooldown)
		log.Printf("pool: worker %s is out of credits — parked for %s", w.ID, CreditsCooldown)
		return
	}

	var apiErr *flowapi.APIError
	if errors.As(err, &apiErr) && apiErr.Throttled() {
		w.circuitOpenUntil = time.Now().Add(ThrottleCooldown)
		log.Printf("pool: worker %s throttled (%d %s) — parked for %s",
			w.ID, apiErr.Status, apiErr.Reason, ThrottleCooldown)
		return
	}

	if w.consecutiveFails >= FailureThreshold {
		w.circuitOpenUntil = time.Now().Add(CircuitCooldown)
		log.Printf("pool: worker %s parked for %s after %d consecutive failures",
			w.ID, CircuitCooldown, w.consecutiveFails)
	}
}

// WorkerStats is a snapshot for reporting.
type WorkerStats struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"project_id"`
	State        State      `json:"state"`
	InFlight     int        `json:"in_flight"`
	Credits      int        `json:"credits"`
	CreditsKnown bool       `json:"credits_known"`
	SKU          string     `json:"sku,omitempty"`
	Free         bool       `json:"free"`
	Served       int64      `json:"served"`
	Failed       int64      `json:"failed"`
	LastUsed     *time.Time `json:"last_used,omitempty"`
	CircuitUntil *time.Time `json:"circuit_until,omitempty"`
}

// Stats is a pool-wide snapshot.
type Stats struct {
	TotalWorkers     int           `json:"total_workers"`
	ActiveWorkers    int           `json:"active_workers"`
	IdleWorkers      int           `json:"idle_workers"`
	CircuitOpen      int           `json:"circuit_open_workers"`
	CreditsAvailable int           `json:"pool_credits_available"`
	CreditsKnown     bool          `json:"credits_known"`
	Served           int64         `json:"total_served"`
	Failed           int64         `json:"total_failed"`
	Workers          []WorkerStats `json:"workers"`
}

// Pool routes work across workers.
type Pool struct {
	mu      sync.RWMutex
	workers []*Worker
	rr      int
}

// New builds an empty pool.
func New() *Pool { return &Pool{} }

// Register adds a worker. Re-registering an ID replaces it.
func (p *Pool) Register(w *Worker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, existing := range p.workers {
		if existing.ID == w.ID {
			p.workers[i] = w
			return
		}
	}
	p.workers = append(p.workers, w)
}

// Remove drops a worker.
func (p *Pool) Remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, w := range p.workers {
		if w.ID == id {
			p.workers = append(p.workers[:i], p.workers[i+1:]...)
			return
		}
	}
}

// Retain drops every worker except the named one.
//
// This used to be how the engine handled an account switch: it acted as a single
// signed-in account at a time, so each switch had to drop the previous account's
// worker or the pool would hold an account it could no longer route to — and
// report that account's balance as available.
//
// The engine no longer calls it. Registration is now additive on purpose: each
// account discovered in the browser gets its own worker, with its own project and
// its own balance, and routing picks between them. Retain is the operation that
// would undo that, so calling it from Bootstrap would leave the pool holding
// whichever account bootstrapped last.
//
// Kept because it is still the supported way to collapse a pool to one account —
// a caller that genuinely acts as a single account wants exactly this, and it is
// cheaper than rebuilding the pool. It is covered by retain_test.go.
func (p *Pool) Retain(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.workers[:0]
	for _, w := range p.workers {
		if w.ID == id {
			kept = append(kept, w)
		}
	}
	p.workers = kept
}

// Workers returns a snapshot of the registered workers.
func (p *Pool) Workers() []*Worker {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Worker, len(p.workers))
	copy(out, p.workers)
	return out
}

// Size reports how many workers are registered.
func (p *Pool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.workers)
}

// Acquire picks the best worker for a job costing `cost` credits and reserves a
// slot on it. The caller must call Release when done.
//
// Selection order:
//  1. free-tier workers that can afford it, least busy first — spend the daily
//     renewable credits before paid ones
//  2. paid workers that can afford it, least busy first
//
// There is deliberately no "anything idle at all" step after those. There used
// to be, and it is what handed jobs to an account known to have no credits —
// work it could only fail, and a wasted round trip against an account the server
// was already refusing. A worker whose balance is *unknown* is still selected,
// because `Affordable` treats an unread balance as payable on purpose, so no
// capacity that could pay is left idle by dropping it.
func (p *Pool) Acquire(ctx context.Context, cost int) (*Worker, error) {
	return p.acquireExcluding(ctx, cost, nil)
}

// AcquireTimeout bounds how long Acquire waits for a worker to become free.
//
// It is a wait for the *transient* case — every worker is busy and one will
// finish — and it is deliberately not applied to the structural ones. A pool
// with nothing registered, or with everything parked on a cooldown that outlasts
// this window, cannot be fixed by waiting, and spending the full 30 seconds to
// say so is what turned a clear failure into a slow one.
const AcquireTimeout = 30 * time.Second

// acquirePollInterval is how often a waiting Acquire re-checks for a free worker.
const acquirePollInterval = 200 * time.Millisecond

// NoWorkerError reports that no worker can take the job, and why.
//
// The wording leads with "no worker available" because that is the phrase a
// caller or a log grep matches on, and the reason is appended rather than
// replacing it — the message used to be exactly "no worker available
// (registered=0)", which says what happened but nothing about what to do.
type NoWorkerError struct {
	// Reason states the cause in the operator's terms.
	Reason string
	// RetryAfter is when a parked worker would come back, when that is known.
	RetryAfter time.Time
}

func (e *NoWorkerError) Error() string {
	msg := "pool: no worker available — " + e.Reason
	if !e.RetryAfter.IsZero() {
		msg += fmt.Sprintf("; the next one frees in %s", time.Until(e.RetryAfter).Round(time.Second))
	}
	return msg
}

// acquireExcluding is Acquire with a set of worker IDs to skip. Failover uses it
// so a worker that already failed this job is not handed back again.
func (p *Pool) acquireExcluding(ctx context.Context, cost int, exclude map[string]bool) (*Worker, error) {
	deadline := time.Now().Add(AcquireTimeout)

	for {
		if w := p.pick(cost, exclude); w != nil {
			return w, nil
		}

		// Waiting only helps if something could free up. Ask before paying for
		// the wait rather than after it.
		if err := p.whyNothingAvailable(exclude, cost, deadline); err != nil {
			return nil, err
		}

		if time.Now().After(deadline) {
			return nil, &NoWorkerError{
				Reason: fmt.Sprintf("all %d registered worker(s) stayed busy for the full %s",
					p.Size(), AcquireTimeout),
			}
		}

		select {
		case <-time.After(acquirePollInterval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// whyNothingAvailable explains why no worker can serve this request, and returns
// nil when waiting could still change that.
//
// The distinction is the whole point: "busy" is worth waiting for, "absent" is
// not. A worker parked by the circuit breaker is the interesting case — the
// cooldown is 90 seconds against a 30-second wait, so a parked worker cannot
// possibly come back before the deadline, and waiting for it is a guaranteed
// 30-second failure. Naming when it frees is more useful than making the caller
// sit through the wait to find out.
func (p *Pool) whyNothingAvailable(exclude map[string]bool, cost int, deadline time.Time) error {
	workers := p.Workers()

	var candidates, parked, unaffordable int
	var earliestFree time.Time
	for _, w := range workers {
		if exclude != nil && exclude[w.ID] {
			continue
		}
		candidates++

		// A worker that cannot pay is structural, like a parked one: waiting does
		// not add credits. Counting it separately is what lets an exhausted pool
		// say so immediately instead of making the caller sit out the whole
		// acquire window and then report a bare timeout.
		if !w.Affordable(cost) {
			unaffordable++
		}

		until := w.parkedUntil()
		if until.IsZero() {
			continue
		}
		parked++
		if earliestFree.IsZero() || until.Before(earliestFree) {
			earliestFree = until
		}
	}

	if candidates == 0 {
		if len(workers) == 0 {
			return &NoWorkerError{
				Reason: "no account is registered — the engine has not bootstrapped, " +
					"or its session was lost and its worker was dropped",
			}
		}
		return &NoWorkerError{
			Reason: fmt.Sprintf("every registered worker (%d) has already been tried for this job",
				len(workers)),
		}
	}

	// Nothing to wait for: a balance does not recover on a timer. Reported ahead
	// of the parked case because it is the more actionable of the two — an
	// operator can add credits or register another account, whereas a cooldown
	// only needs time.
	if unaffordable == candidates {
		return &NoWorkerError{
			Reason: fmt.Sprintf("every registered worker (%d) is out of credits for a %d-credit "+
				"job; add credits, or register another account", candidates, cost),
		}
	}

	// Every candidate is parked, so nothing can free up until a cooldown
	// expires. If even the earliest of those expires after the deadline, the
	// wait is guaranteed to fail.
	if parked == candidates && !earliestFree.Before(deadline) {
		return &NoWorkerError{
			Reason: fmt.Sprintf("all %d worker(s) are parked after repeated failures and the "+
				"cooldown outlasts this wait", candidates),
			RetryAfter: earliestFree,
		}
	}

	// At least one worker is merely busy, and will free up. Worth waiting for.
	return nil
}

// parkedUntil returns when this worker's circuit closes, or the zero time when
// it is not parked.
func (w *Worker) parkedUntil() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if time.Now().Before(w.circuitOpenUntil) {
		return w.circuitOpenUntil
	}
	return time.Time{}
}

func (p *Pool) pick(cost int, exclude map[string]bool) *Worker {
	p.mu.Lock()
	candidates := make([]*Worker, 0, len(p.workers))
	for _, w := range p.workers {
		if exclude != nil && exclude[w.ID] {
			continue
		}
		candidates = append(candidates, w)
	}
	p.rr++
	offset := p.rr
	p.mu.Unlock()

	if len(candidates) == 0 {
		return nil
	}

	// Least-busy first, with a round-robin tiebreak so a steady-state pool does
	// not pin every job to one worker.
	sort.SliceStable(candidates, func(i, j int) bool {
		return inFlightOf(candidates[i]) < inFlightOf(candidates[j])
	})
	candidates = rotate(candidates, offset)

	for _, w := range candidates {
		if w.IsFree() && w.Affordable(cost) && w.acquire() {
			return w
		}
	}
	for _, w := range candidates {
		if w.Affordable(cost) && w.acquire() {
			return w
		}
	}

	// Deliberately no third pass.
	//
	// There used to be one that took any idle worker at all, and it is what let a
	// drained account keep receiving jobs: `acquire` checks the circuit and the
	// in-flight count but never the balance, so a worker known to be unable to
	// pay was still handed work it could only fail. The two passes above already
	// cover everything else — `Affordable` answers true for a worker whose
	// balance has simply not been read yet, precisely so unchecked capacity is
	// not stranded — so the only thing a third pass could ever add is a worker we
	// already know cannot pay.
	return nil
}

func inFlightOf(w *Worker) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inFlight
}

func rotate(workers []*Worker, offset int) []*Worker {
	if len(workers) < 2 || offset == 0 {
		return workers
	}
	n := offset % len(workers)
	return append(append([]*Worker{}, workers[n:]...), workers[:n]...)
}

// Release returns a worker to the pool, recording the outcome.
func (p *Pool) Release(w *Worker, credits int, err error) {
	if w == nil {
		return
	}
	if err != nil {
		w.recordFailure(err)
	} else {
		w.recordSuccess(credits)
	}
	w.release()
}

// Execute acquires a worker, runs op, and fails over to another worker when the
// error is retryable. This is the single entry point generation code should use.
func (p *Pool) Execute(ctx context.Context, cost int, op func(context.Context, *Worker) error) error {
	attempted := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAttempts(p.Size()); attempt++ {
		// Stop as soon as every registered worker has been tried. Without this
		// the next Acquire would block for its full 30-second deadline waiting
		// for a worker that can never be handed out.
		if len(attempted) >= p.Size() && p.Size() > 0 {
			break
		}

		worker, err := p.acquireExcluding(ctx, cost, attempted)
		if err != nil {
			if lastErr != nil {
				return fmt.Errorf("%w (last error: %v)", err, lastErr)
			}
			return err
		}

		attempted[worker.ID] = true

		opErr := op(ctx, worker)
		if opErr == nil {
			p.Release(worker, 0, nil)
			return nil
		}

		lastErr = opErr
		// Release records the outcome. For an out-of-credits error that is what
		// marks the worker drained and parks it — see recordFailure.
		p.Release(worker, 0, opErr)

		// An account that cannot pay is a reason to try a *different* account, not
		// a reason to fail the job. It is not `Retryable` — the same request to the
		// same account would fail identically — but it is precisely what failover
		// is for, so it is admitted here rather than by widening IsRetryable,
		// which other callers rely on meaning "retrying the same thing is worth
		// it". The next iteration excludes this worker by balance, so the job
		// moves on instead of coming back to it.
		if !flowapi.IsRetryable(opErr) && !flowapi.IsOutOfCredits(opErr) {
			return opErr
		}

		log.Printf("pool: worker %s failed (%v), failing over", worker.ID, opErr)
	}

	if lastErr != nil {
		return fmt.Errorf("pool: all workers failed, last error: %w", lastErr)
	}
	// Reachable only when the loop exited without ever calling op, which the
	// guard above prevents whenever a worker exists — so this is the empty pool.
	return &NoWorkerError{
		Reason: "no account is registered — the engine has not bootstrapped, " +
			"or its session was lost and its worker was dropped",
	}
}

func maxAttempts(size int) int {
	if size < 1 {
		return 1
	}
	return size + 1
}

// RefreshCredits asks every worker for its authoritative balance.
//
// The reader comes from the worker, not from Worker.Client. Client is the legacy
// aisandbox REST surface, so its balance belongs to whichever account that
// credential is for — reading it here would record one account's credits against
// another, which is the failure the Python version had when it reported test
// fixtures as live balances. That is why this sat uncalled: the wiring existed
// but read the wrong source. With an authoritative reader installed — batchexecute
// `nzlxg`, via Engine.creditsReader — these are the same numbers /v1/credits serves.
//
// A worker with no reader is skipped and stays unknown rather than being given a
// figure from the untrusted source. Unknown is not zero: Worker.Affordable
// answers true for an unknown balance, so skipping costs no capacity.
func (p *Pool) RefreshCredits(ctx context.Context) {
	for _, w := range p.Workers() {
		reader := w.creditsReader()
		if reader == nil {
			log.Printf("pool: worker %s has no authoritative credits reader; "+
				"leaving its balance unknown", w.ID)
			continue
		}
		balance, err := reader(ctx)
		if err != nil {
			log.Printf("pool: credit check failed for %s: %v", w.ID, err)
			continue
		}
		w.SetCredits(balance.Credits, balance.SKU)
	}
}

// Stats reports a snapshot of the pool. Every number here is derived from live
// worker state; nothing is fabricated.
func (p *Pool) Stats() Stats {
	workers := p.Workers()
	stats := Stats{TotalWorkers: len(workers), Workers: make([]WorkerStats, 0, len(workers))}

	for _, w := range workers {
		w.mu.Lock()
		ws := WorkerStats{
			ID:           w.ID,
			ProjectID:    w.projectID,
			InFlight:     w.inFlight,
			Credits:      w.credits,
			CreditsKnown: w.creditsKnown,
			SKU:          w.sku,
			Free:         w.sku != "" && w.sku != "G1_TIER1",
			Served:       w.served,
			Failed:       w.failed,
		}
		if !w.lastUsed.IsZero() {
			t := w.lastUsed
			ws.LastUsed = &t
		}
		if time.Now().Before(w.circuitOpenUntil) {
			t := w.circuitOpenUntil
			ws.CircuitUntil = &t
			ws.State = StateCircuitOpen
		} else if w.inFlight >= w.limit() {
			ws.State = StateBusy
		} else {
			ws.State = StateIdle
		}
		if w.creditsKnown {
			stats.CreditsAvailable += w.credits
			stats.CreditsKnown = true
		}
		stats.Served += w.served
		stats.Failed += w.failed
		w.mu.Unlock()

		switch ws.State {
		case StateIdle:
			stats.IdleWorkers++
		case StateBusy:
			stats.ActiveWorkers++
		case StateCircuitOpen:
			stats.CircuitOpen++
		}
		stats.Workers = append(stats.Workers, ws)
	}

	return stats
}
