package engine

import (
	"testing"
	"time"
)

// creditsPtr builds the pointer the scan uses, so a test can say "known" without
// repeating the address-of dance.
func creditsPtr(n int) *int { return &n }

func TestBestUncooledAccountSkipsTheCurrentAndTheCooling(t *testing.T) {
	scan := []AccountCredits{
		{Index: 0, SignedIn: true, Credits: creditsPtr(100)},
		{Index: 1, SignedIn: true, Credits: creditsPtr(100)},
		{Index: 2, SignedIn: true, Credits: creditsPtr(100)},
	}

	// Account 0 is in use and 1 is cooling, so 2 is the first usable one.
	if got := bestUncooledAccount(scan, 0, map[int]bool{1: true}, 10); got != 2 {
		t.Errorf("got account %d, want 2", got)
	}
}

// Rotating to where we already are is not a rotation.
func TestBestUncooledAccountNeverReturnsTheCurrent(t *testing.T) {
	scan := []AccountCredits{{Index: 0, SignedIn: true, Credits: creditsPtr(100)}}

	if got := bestUncooledAccount(scan, 0, nil, 10); got != -1 {
		t.Errorf("got account %d, want -1: the only account is the one in use", got)
	}
}

func TestBestUncooledAccountSkipsAccountsThatAreNotSignedIn(t *testing.T) {
	scan := []AccountCredits{
		{Index: 0, SignedIn: true},
		{Index: 1, SignedIn: false, Credits: creditsPtr(999)},
		{Index: 2, SignedIn: true},
	}

	if got := bestUncooledAccount(scan, 0, nil, 10); got != 2 {
		t.Errorf("got account %d, want 2 — index 1 has no session", got)
	}
}

// A rotation spends a submission against an account that has not been refused
// yet, so one known to cover the cost is worth preferring over one whose balance
// nobody has read.
func TestBestUncooledAccountPrefersOneThatCanPay(t *testing.T) {
	scan := []AccountCredits{
		{Index: 0, SignedIn: true},
		{Index: 1, SignedIn: true, Credits: nil},
		{Index: 2, SignedIn: true, Credits: creditsPtr(50)},
	}

	if got := bestUncooledAccount(scan, 0, nil, 10); got != 2 {
		t.Errorf("got account %d, want 2 — it is the one known to cover the cost", got)
	}
}

// An unread balance is not evidence of an empty wallet, which is the rule
// ensureAffordable already applies — so a scan with nothing known-affordable
// still rotates rather than waiting.
func TestBestUncooledAccountFallsBackToAnUnknownBalance(t *testing.T) {
	scan := []AccountCredits{
		{Index: 0, SignedIn: true},
		{Index: 1, SignedIn: true, Credits: nil},
	}

	if got := bestUncooledAccount(scan, 0, nil, 10); got != 1 {
		t.Errorf("got account %d, want 1 — an unread balance is not a refusal", got)
	}
}

// And an account known not to cover the cost is not preferred, but is still
// better than waiting on one that has been refused.
func TestBestUncooledAccountFallsBackToAnAccountThatCannotPay(t *testing.T) {
	scan := []AccountCredits{
		{Index: 0, SignedIn: true},
		{Index: 1, SignedIn: true, Credits: creditsPtr(3)},
	}

	if got := bestUncooledAccount(scan, 0, nil, 10); got != 1 {
		t.Errorf("got account %d, want 1", got)
	}
}

// Every account cooling is the case the fallback wait exists for.
func TestBestUncooledAccountWhenEverythingIsCooling(t *testing.T) {
	scan := []AccountCredits{
		{Index: 0, SignedIn: true},
		{Index: 1, SignedIn: true},
	}

	if got := bestUncooledAccount(scan, 0, map[int]bool{0: true, 1: true}, 10); got != -1 {
		t.Errorf("got account %d, want -1", got)
	}
}

func TestAccountCooldownsMarkAndExpire(t *testing.T) {
	var c accountCooldowns
	now := time.Now()

	if c.cooling(3, now) {
		t.Error("an unmarked account is reported as cooling")
	}
	c.mark(3, time.Minute)
	if !c.cooling(3, now) {
		t.Error("a marked account is not reported as cooling")
	}
	if c.cooling(3, now.Add(2*time.Minute)) {
		t.Error("an account is still cooling after its window has passed")
	}
}

// A second refusal inside the window is evidence the window is still needed, not
// evidence it has passed — so a mark never shortens one that is running.
func TestAccountCooldownsNeverShortensARunningWindow(t *testing.T) {
	var c accountCooldowns
	now := time.Now()

	c.mark(1, time.Minute)
	c.mark(1, time.Second)

	if !c.cooling(1, now.Add(30*time.Second)) {
		t.Error("a shorter mark cut a running window short")
	}
}

// And it does extend one, so a run that keeps being refused keeps backing off.
func TestAccountCooldownsExtendsARunningWindow(t *testing.T) {
	var c accountCooldowns
	now := time.Now()

	c.mark(1, time.Minute)
	c.mark(1, 5*time.Minute)

	if !c.cooling(1, now.Add(2*time.Minute)) {
		t.Error("a longer mark did not extend the window")
	}
}

// An accepted submission is the only evidence that an account's window has passed.
func TestAccountCooldownsClearEndsIt(t *testing.T) {
	var c accountCooldowns

	c.mark(1, time.Minute)
	c.clear(1)

	if c.cooling(1, time.Now()) {
		t.Error("the account is still cooling after being cleared")
	}
}

// The wait is for the first account to come back, not the last: waiting for the
// last adds delay that buys nothing.
func TestAccountCooldownsSoonestIsTheShortestRemaining(t *testing.T) {
	var c accountCooldowns

	c.mark(1, 30*time.Second)
	c.mark(2, 5*time.Second)

	got, ok := c.soonest(time.Now())
	if !ok {
		t.Fatal("soonest found no cooldown although two are running")
	}
	if got <= 0 || got > 5*time.Second {
		t.Errorf("soonest = %v, want about 5s", got)
	}
}

func TestAccountCooldownsSoonestWithNothingCooling(t *testing.T) {
	var c accountCooldowns

	if _, ok := c.soonest(time.Now()); ok {
		t.Error("soonest reported a cooldown on an empty map")
	}
}

// Expired entries are dropped rather than kept for the life of the process, so
// the map stays the size of the accounts actually cooling.
func TestAccountCooldownsPrunesExpiredEntries(t *testing.T) {
	var c accountCooldowns

	c.mark(1, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	c.mark(2, time.Minute)

	c.mu.Lock()
	size := len(c.until)
	c.mu.Unlock()

	if size != 1 {
		t.Errorf("the map holds %d entries, want 1 — the expired one was not pruned", size)
	}
}

// The engine reads these through nil-safe methods, because an Engine built as a
// struct literal in a test has no cooldowns at all.
func TestAccountCooldownsIsNilSafe(t *testing.T) {
	var c *accountCooldowns

	c.mark(1, time.Minute)
	c.clear(1)

	if c.cooling(1, time.Now()) {
		t.Error("a nil map reported an account as cooling")
	}
	if _, ok := c.soonest(time.Now()); ok {
		t.Error("a nil map reported a cooldown")
	}
}
