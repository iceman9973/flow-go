package engine

import "testing"

// A request conditioned on an existing asset must not be moved to another account.
//
// Projects are per-account, so the asset id belongs to the project of the account
// that supplied it. Moving the engine to a richer account leaves the id pointing at
// a project the new account cannot see, and the render fails after a long wait
// looking for an asset that is in the listing — just not that one.
//
// Measured: an image generated on account 2 was used as the start frame of a
// request the engine moved to account 1 to afford, and resolution retried its full
// window before failing with "not in the project listing".
func TestCanMoveAccountsRefusesAConditionedRequest(t *testing.T) {
	for _, req := range []BatchVideoRequest{
		{StartImage: "18aa7415-2e39-4316-8e66-bf1d374ac2d9"},
		{EndImage: "18aa7415-2e39-4316-8e66-bf1d374ac2d9"},
		{StartImage: "a", EndImage: "b"},
	} {
		if canMoveAccounts(req) {
			t.Errorf("canMoveAccounts(%+v) = true; a conditioned request is project-bound", req)
		}
	}
}

// A request with nothing to resolve can be served by any account — that is the
// case account selection exists for.
func TestCanMoveAccountsAllowsAnUnconditionedRequest(t *testing.T) {
	for _, req := range []BatchVideoRequest{
		{Prompt: "a paper boat on a river"},
		{StartImage: "", EndImage: ""},
		{StartImage: "   ", EndImage: "\t"},
	} {
		if !canMoveAccounts(req) {
			t.Errorf("canMoveAccounts(%+v) = false; nothing binds this request to an account", req)
		}
	}
}
