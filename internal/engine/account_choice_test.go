package engine

import "testing"

func credits(n int) *int { return &n }

// The case that started this: three signed-in accounts holding 1, 11 and 25
// credits, and a 7-credit render. The engine was sitting on the 1-credit account
// and refusing work the 25-credit one could do.
func TestBestAffordableAccountPicksTheRichestThatCanPay(t *testing.T) {
	rows := []AccountCredits{
		{Index: 0, SignedIn: true, Email: "a@example.com", Credits: credits(1)},
		{Index: 1, SignedIn: true, Email: "b@example.com", Credits: credits(11)},
		{Index: 2, SignedIn: true, Email: "c@example.com", Credits: credits(25)},
	}

	index, balance := bestAffordableAccount(rows, 7)
	if index != 2 || balance != 25 {
		t.Errorf("got index=%d balance=%d, want index=2 balance=25", index, balance)
	}
}

// A 0-credit account must never be chosen. This is the whole complaint: the
// engine was using one that could not render at all.
func TestBestAffordableAccountSkipsTheBrokeAccount(t *testing.T) {
	rows := []AccountCredits{
		{Index: 0, SignedIn: true, Credits: credits(0)},
		{Index: 1, SignedIn: true, Credits: credits(11)},
	}

	index, _ := bestAffordableAccount(rows, 7)
	if index != 1 {
		t.Errorf("got index=%d, want index=1 — the 0-credit account was chosen", index)
	}
}

// An exact balance qualifies. The test is "can afford", and an off-by-one here
// would strand an account that can pay for the job.
func TestBestAffordableAccountAcceptsAnExactBalance(t *testing.T) {
	rows := []AccountCredits{{Index: 0, SignedIn: true, Credits: credits(7)}}

	index, _ := bestAffordableAccount(rows, 7)
	if index != 0 {
		t.Error("an exactly-sufficient account was rejected")
	}
}

// Nothing can pay. The caller refuses, and must not be handed an account to move
// to — moving would be a pointless re-bootstrap that ends in the same refusal.
func TestBestAffordableAccountReturnsNothingWhenNobodyCanPay(t *testing.T) {
	rows := []AccountCredits{
		{Index: 0, SignedIn: true, Credits: credits(1)},
		{Index: 1, SignedIn: true, Credits: credits(3)},
	}

	if index, _ := bestAffordableAccount(rows, 7); index != -1 {
		t.Errorf("got index=%d, want -1 when no account can cover the cost", index)
	}
}

// A balance that could not be read is not a balance. `Credits` is nil exactly
// when the read failed, and choosing that account would move the engine onto one
// on no evidence at all — the failure mode this whole area keeps producing.
func TestBestAffordableAccountIgnoresUnreadBalances(t *testing.T) {
	rows := []AccountCredits{
		{Index: 0, SignedIn: true, Credits: nil, Error: "read failed"},
		{Index: 1, SignedIn: true, Credits: credits(11)},
	}

	index, balance := bestAffordableAccount(rows, 7)
	if index != 1 || balance != 11 {
		t.Errorf("got index=%d balance=%d, want index=1 balance=11", index, balance)
	}

	// And when the only candidate is unread, there is no candidate.
	only := []AccountCredits{{Index: 0, SignedIn: true, Credits: nil}}
	if index, _ := bestAffordableAccount(only, 7); index != -1 {
		t.Errorf("got index=%d, want -1 when the only balance is unread", index)
	}
}

// A row that is not signed in is not an account, whatever balance it reports.
func TestBestAffordableAccountIgnoresSignedOutRows(t *testing.T) {
	rows := []AccountCredits{
		{Index: 0, SignedIn: false, Credits: credits(500)},
		{Index: 1, SignedIn: true, Credits: credits(9)},
	}

	index, _ := bestAffordableAccount(rows, 7)
	if index != 1 {
		t.Errorf("got index=%d, want index=1 — a signed-out row was chosen", index)
	}
}

// Ties resolve to the first, so the choice is stable across runs rather than
// depending on map or scan ordering.
func TestBestAffordableAccountIsStableOnATie(t *testing.T) {
	rows := []AccountCredits{
		{Index: 0, SignedIn: true, Credits: credits(10)},
		{Index: 1, SignedIn: true, Credits: credits(10)},
	}

	index, _ := bestAffordableAccount(rows, 7)
	if index != 0 {
		t.Errorf("got index=%d, want the first on a tie", index)
	}
}

func TestDescribeBalancesRendersUnreadAsUnknown(t *testing.T) {
	rows := []AccountCredits{
		{Index: 0, Credits: credits(1)},
		{Index: 1, Credits: nil},
	}

	got := describeBalances(rows)
	if got != "accounts 0=1, 1=unknown" {
		t.Errorf("describeBalances = %q", got)
	}
	if got := describeBalances(nil); got != "nothing readable" {
		t.Errorf("describeBalances(nil) = %q, want \"nothing readable\"", got)
	}
}
