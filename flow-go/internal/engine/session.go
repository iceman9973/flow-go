package engine

import (
	"fmt"
	"log"
	"time"
)

// Session liveness — why the engine can fail fast instead of failing slowly.
//
// The engine's readiness gate is a one-way latch. Bootstrap sets ready=true and
// nothing ever sets it back, which is right for what `ready` means: an account
// and an anti-CSRF token were loaded. But it left the engine with no way to say
// "I can no longer serve this", so the gate could never fire a second time.
//
// After the extension disconnects and the cookies behind the session go stale,
// every request still passed that gate and ran the whole path — a balance read,
// a reCAPTCHA mint, two priming attempts, then a 401 — before failing. That is
// the 25-30 second hang. It is not one slow call; it is a sequence of them, each
// paying a round trip to rediscover a fact the previous one had already learned.
//
// The state recorded here is that missing signal, and it is deliberately NOT
// "the bridge is disconnected". The engine is designed to run with no browser at
// all: the cookies persisted under cookies/ are a supported source, and a
// browserless caller supplies AtToken, Fsid and Fingerprint for exactly that
// case. Treating "no extension attached" as "not ready" would break a mode that
// works, and would be the same mistake in the other direction.
//
// The honest signal is narrower. The upstream rejected our credentials, and
// there is no browser attached that could refresh them. Nothing inside the
// process can change either half of that, so it is worth remembering and worth
// refusing on — with a message that says what to do about it.

// sessionDeath records the point at which the session became unusable, and why.
type sessionDeath struct {
	reason string
	at     time.Time
}

// sessionHint is what the caller can actually do about a dead session.
const sessionHint = "attach the browser extension, then call POST /v1/bridge/refresh and retry"

// UnavailableError reports that the engine cannot serve the request, and that no
// amount of retrying by the caller will change that until something outside the
// process is fixed.
//
// It exists so the HTTP layer does not have to infer the status from the
// wording. statusFor matched on substrings, which works until a message is
// reworded — and a reworded message silently becoming a 502 instead of a 503 is
// exactly the kind of change nobody notices.
type UnavailableError struct {
	// Reason states what is wrong, in the operator's terms.
	Reason string
	// Hint is the action that would fix it. Optional.
	Hint string
}

func (e *UnavailableError) Error() string {
	if e.Hint == "" {
		return e.Reason
	}
	return e.Reason + " — " + e.Hint
}

// markSessionDead records that the session can no longer be recovered.
//
// Called at the moment the engine learns it, which is the 401 path: the upstream
// rejected the credentials and there is no browser to refresh them. The first
// caller pays for that discovery; every caller after it should not.
//
// The pool is drained at the same time. Its worker wraps the legacy REST client,
// which is authenticated by the same dead session, so leaving it registered
// would keep a worker in rotation that cannot serve — and the pool has no other
// way to find out, since nothing else ever told it the session had gone.
func (e *Engine) markSessionDead(reason string) {
	e.sessionMu.Lock()
	if e.sessionDead.reason != "" {
		// Already known. Do not overwrite the first reason: it is the one that
		// describes the actual cause, and later callers are only seeing it
		// second-hand.
		e.sessionMu.Unlock()
		return
	}
	e.sessionDead = sessionDeath{reason: reason, at: time.Now()}
	e.sessionMu.Unlock()

	log.Printf("engine: session is no longer usable — %s; %s", reason, sessionHint)

	// Outside the lock: Remove takes the pool's lock, and holding one while
	// taking the other is how two independent subsystems become one deadlock.
	if id := e.AccountID(); id != "" {
		e.pool.Remove(id)
	}
}

// noteSessionRestored clears the death record after a bootstrap that worked.
//
// A bootstrap proves the session is usable again, which is the only evidence
// that should clear this. Deliberately not cleared by "the extension
// reconnected": an attached browser with expired cookies is still a session that
// cannot serve, and clearing on connection would put the engine straight back
// into the slow failure it just stopped having.
func (e *Engine) noteSessionRestored() {
	e.sessionMu.Lock()
	previous := e.sessionDead.reason
	e.sessionDead = sessionDeath{}
	e.sessionMu.Unlock()

	if previous != "" {
		log.Printf("engine: the session is usable again (was: %s)", previous)
	}
}

// sessionFailure returns why the session is unusable, or nil when it is fine.
func (e *Engine) sessionFailure() error {
	e.sessionMu.Lock()
	defer e.sessionMu.Unlock()
	if e.sessionDead.reason == "" {
		return nil
	}
	return &UnavailableError{Reason: e.sessionDead.reason, Hint: sessionHint}
}

// SessionFailure reports why the engine cannot serve, or "" when it can.
//
// Exposed for /health and /status so the state is visible where an operator
// looks first, rather than only in the error of the request that tripped over
// it.
func (e *Engine) SessionFailure() string {
	e.sessionMu.Lock()
	defer e.sessionMu.Unlock()
	return e.sessionDead.reason
}

// preflight is the gate every generation path runs first.
//
// It is cheap on purpose: no network, no upstream call, nothing that can itself
// be slow. Everything it rejects would otherwise be discovered 25-30 seconds
// later, after the balance read, the reCAPTCHA mint and the priming retries had
// each paid a round trip for the same answer.
//
// It checks only what is knowable up front. It does not ask whether the cookies
// are still valid — that costs a round trip and would defeat the point — so a
// session that has quietly expired still fails on its first upstream call. What
// it does guarantee is that the failure is paid for once, not once per request.
func (e *Engine) preflight() error {
	if !e.Ready() {
		// The same wording the existing gate used, so the 503 mapping and any
		// caller matching on it keep working.
		return &UnavailableError{
			Reason: "engine: not ready — call Bootstrap first",
			Hint:   "no account has been loaded yet; " + sessionHint,
		}
	}
	if err := e.sessionFailure(); err != nil {
		return err
	}
	return nil
}

// deadSessionReason is the wording the 401 path uses, kept in one place because
// it is asserted on by tests and read by operators.
func deadSessionReason() string {
	return "the Google session expired and no browser extension is attached to refresh it"
}

// describeSessionState is a one-line summary for logs.
func (e *Engine) describeSessionState() string {
	if reason := e.SessionFailure(); reason != "" {
		return fmt.Sprintf("unusable (%s)", reason)
	}
	return "usable"
}
