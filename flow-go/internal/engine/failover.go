package engine

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/config"
)

// describeAccount names an account for a log line.
//
// The address is what distinguishes two accounts signed in to one browser, and
// the index is what the switch mechanism addresses it by — so both, because
// either alone leaves the reader guessing which account a rotation landed on.
func describeAccount(row *AccountCredits) string {
	if row == nil {
		return "another account"
	}
	if row.Email != "" {
		return fmt.Sprintf("%d (%s)", row.Index, row.Email)
	}
	if row.Name != "" {
		return fmt.Sprintf("%d (%s)", row.Index, row.Name)
	}
	return strconv.Itoa(row.Index)
}

// accountCooldowns records which signed-in accounts are cooling down after a rate
// refusal, and until when.
//
// Keyed by the `authuser` index, because that is how the switch mechanism
// addresses an account: it is the key the scan returns and the key
// SetAccountIndex consumes.
//
// Engine-level rather than per-client, for the same reason the submission pacing
// is: a client is built per generation, so a cooldown remembered on one would be
// discarded by the next — which is exactly the sequence a refused run produces.
type accountCooldowns struct {
	mu    sync.Mutex
	until map[int]time.Time
}

// mark starts or extends an account's cooldown.
//
// It never shortens one that is already running. A second refusal inside the
// window is evidence the window is still needed, not evidence it has passed, and
// shortening it would make the next attempt land in the same refused moment.
func (c *accountCooldowns) mark(index int, d time.Duration) {
	if c == nil || d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	// Drop what has already expired, so the map stays the size of the accounts
	// actually cooling rather than of every account ever refused.
	for other, end := range c.until {
		if !now.Before(end) {
			delete(c.until, other)
		}
	}

	if c.until == nil {
		c.until = make(map[int]time.Time)
	}
	if end := now.Add(d); end.After(c.until[index]) {
		c.until[index] = end
	}
}

// cooling reports whether an account is inside its cooldown.
func (c *accountCooldowns) cooling(index int, now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	end, ok := c.until[index]
	return ok && now.Before(end)
}

// clear ends an account's cooldown, because a submission on it was accepted.
func (c *accountCooldowns) clear(index int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.until, index)
}

// soonest returns the shortest remaining cooldown among the accounts still
// cooling, and whether there is one.
//
// The shortest rather than the longest: when every account is being refused the
// client as a whole has to wait, and the first account to come back is the first
// chance of a successful submission. Waiting for the last would add delay that
// buys nothing.
func (c *accountCooldowns) soonest(now time.Time) (time.Duration, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var best time.Duration
	found := false
	for _, end := range c.until {
		if !now.Before(end) {
			continue
		}
		if remaining := end.Sub(now); !found || remaining < best {
			best, found = remaining, true
		}
	}
	return best, found
}

// bestUncooledAccount picks the signed-in account a refused generation should run
// as, or -1 when there is none.
//
// Pure, and separate from the switch for the same reason bestAffordableAccount is:
// this decides which account a rate-limited generation lands on, and it is worth
// being able to test without a network or a Bootstrap.
//
// Preference order: an uncooled account whose balance is known to cover the cost,
// then any uncooled one. The preference matters because a rotation spends a
// submission against an account that has not been refused yet, and spending it on
// one that cannot pay wastes exactly the request the rotation exists to protect.
// Falling back to an unknown balance is deliberate: an unread balance is not
// evidence of an empty wallet, which is the rule ensureAffordable already applies.
//
// The current account is excluded even when it is not cooling, because rotating to
// where we already are is not a rotation.
func bestUncooledAccount(rows []AccountCredits, current int, cooling map[int]bool, cost int) int {
	fallback := -1
	for _, row := range rows {
		if !row.SignedIn || row.Index == current || cooling[row.Index] {
			continue
		}
		if row.Credits != nil && *row.Credits >= cost {
			return row.Index
		}
		if fallback == -1 {
			fallback = row.Index
		}
	}
	return fallback
}

// rotateAwayFromCooling moves the engine onto a signed-in account that is not
// cooling down, and reports the account it moved to.
//
// It reuses the switch mechanism rather than inventing a second one.
// SetAccountIndex is a full Bootstrap because the session, the account row and the
// project have to move together — a project belonging to one account submitted
// under another's session comes back empty — so there is nothing new to build
// here, only a different reason to move.
//
// Note what "0-wait" does and does not mean: it skips the cooldown sleep, not the
// cost of the switch. The Bootstrap is seconds of work, and it is still far less
// than waiting out a refusal on an account that has already answered.
func (e *Engine) rotateAwayFromCooling(ctx context.Context, cost int) (*AccountCredits, bool) {
	rows, err := e.AccountsCredits(ctx, config.AccountScanLimit)
	if err != nil {
		log.Printf("engine: could not scan the signed-in accounts for one that is not cooling "+
			"down (%v); waiting instead", err)
		return nil, false
	}

	now := time.Now()
	cooling := make(map[int]bool, len(rows))
	for _, row := range rows {
		if e.cooldowns.cooling(row.Index, now) {
			cooling[row.Index] = true
		}
	}

	index := bestUncooledAccount(rows, e.AccountIndex(), cooling, cost)
	if index == -1 {
		return nil, false
	}
	if err := e.SetAccountIndex(ctx, index); err != nil {
		log.Printf("engine: could not rotate to account %d (%v); waiting instead", index, err)
		return nil, false
	}

	for i := range rows {
		if rows[i].Index == index {
			return &rows[i], true
		}
	}
	return nil, true
}
