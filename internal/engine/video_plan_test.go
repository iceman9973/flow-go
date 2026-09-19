package engine

import (
	"strings"
	"testing"
)

// The case this whole check exists for: a 4s 720p render costs 7 credits and the
// account has 1. The server would have accepted the submission and answered with
// no media, which presents as a broken request rather than an empty wallet.
func TestDecideVideoPlanRefusesWhenNothingFits(t *testing.T) {
	plan := decideVideoPlan(4, 1, "abra_t2v_4s", "720p", 1)

	if plan.Affordable() {
		t.Fatal("a 1-credit account was allowed to submit a 7-credit render")
	}
	for _, want := range []string{"insufficient credits", "7", "1"} {
		if !strings.Contains(plan.Reason, want) {
			t.Errorf("reason %q does not mention %q", plan.Reason, want)
		}
	}
	if plan.Cost != 7 {
		t.Errorf("Cost = %d, want 7 (4s at 720p)", plan.Cost)
	}
	if plan.Balance != 1 {
		t.Errorf("Balance = %d, want 1", plan.Balance)
	}
	if !plan.Known {
		t.Error("Known = false, but the balance was read")
	}
}

// 360p is the same render for roughly half, so an account that cannot afford
// 720p should still be served rather than refused.
func TestDecideVideoPlanDowngradesWhenTheCheaperQualityFits(t *testing.T) {
	plan := decideVideoPlan(4, 1, "abra_t2v_4s", "720p", 4)

	if !plan.Affordable() {
		t.Fatalf("a 4-credit account was refused a 360p render it can pay for: %s", plan.Reason)
	}
	if !plan.Downgraded {
		t.Error("Downgraded = false, but the request was cut from 720p to 360p")
	}
	if plan.Quality != "360p" {
		t.Errorf("Quality = %q, want 360p", plan.Quality)
	}
	if plan.Model != "abra_t2v_4s_360p" {
		t.Errorf("Model = %q, want abra_t2v_4s_360p", plan.Model)
	}
	if plan.Cost != 4 {
		t.Errorf("Cost = %d, want 4 (4s at 360p)", plan.Cost)
	}
}

// An exact balance must be accepted. The comparison is "can afford", not "has
// more than", and an off-by-one here refuses a render the account can pay for.
func TestDecideVideoPlanAcceptsAnExactBalance(t *testing.T) {
	plan := decideVideoPlan(10, 1, "abra_t2v_10s", "720p", 15)

	if !plan.Affordable() {
		t.Fatalf("an exactly-sufficient balance was refused: %s", plan.Reason)
	}
	if plan.Downgraded {
		t.Error("Downgraded = true, but the balance covered the requested quality")
	}
	if plan.Cost != 15 {
		t.Errorf("Cost = %d, want 15 (10s at 720p)", plan.Cost)
	}
}

// The count multiplies. Four renders at 7 credits is 28, not 7 — reading only
// the per-render figure would let a request through at a quarter of its cost.
func TestDecideVideoPlanMultipliesByCount(t *testing.T) {
	plan := decideVideoPlan(4, 4, "abra_t2v_4s", "720p", 10)

	if plan.Affordable() {
		t.Fatal("10 credits was allowed to cover 4 renders at 7 each")
	}
	if plan.Cost != 28 {
		t.Errorf("Cost = %d, want 28 (4 × 7)", plan.Cost)
	}
}

// A duration the table does not record must not become a refusal. The table
// covers the durations the app offers, and a model named outright can sit
// outside it — the cost table must not be the limit on what can be generated.
func TestDecideVideoPlanAllowsAnUnrecordedCost(t *testing.T) {
	plan := decideVideoPlan(7, 1, "veo_3_1_t2v_something", "720p", 0)

	if !plan.Affordable() {
		t.Fatalf("an unrecorded cost was treated as a refusal: %s", plan.Reason)
	}
	if plan.Cost != 0 {
		t.Errorf("Cost = %d, want 0 for an unrecorded pair", plan.Cost)
	}
}

// A 360p request on a broke account has nowhere cheaper to go, so it is refused
// rather than downgraded to the quality it already asked for.
func TestDecideVideoPlanDoesNotDowngradeBelow360p(t *testing.T) {
	plan := decideVideoPlan(4, 1, "abra_t2v_4s_360p", "360p", 3)

	if plan.Affordable() {
		t.Fatal("a 3-credit account was allowed to submit a 4-credit 360p render")
	}
	if plan.Downgraded {
		t.Error("Downgraded = true, but 360p is already the cheapest quality")
	}
	if plan.Quality != "360p" {
		t.Errorf("Quality = %q, want 360p unchanged", plan.Quality)
	}
}
