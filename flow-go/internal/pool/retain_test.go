package pool

import "testing"

// Retain collapses the pool to one account.
//
// Bootstrap no longer calls it — registration is additive now, so that each
// discovered account keeps its worker and routing can pick between them — but the
// operation is still the supported way for a caller that genuinely acts as a
// single account, and these cover it.
func TestRetainDropsThePreviousAccount(t *testing.T) {
	p := New()
	p.Register(newTestWorker("acct-index0"))
	p.Register(newTestWorker("acct-index2"))

	p.Retain("acct-index2")

	if p.Size() != 1 {
		t.Fatalf("Size = %d, want 1 after Retain", p.Size())
	}
	if got := p.Workers()[0].ID; got != "acct-index2" {
		t.Errorf("retained worker = %q, want acct-index2", got)
	}
}

// Retain must leave the named worker alone, because Register is called straight
// after it and relies on the existing entry being there to replace.
func TestRetainKeepsTheNamedWorker(t *testing.T) {
	p := New()
	kept := newTestWorker("acct-current")
	p.Register(kept)

	p.Retain("acct-current")

	if p.Size() != 1 {
		t.Fatalf("Size = %d, want 1", p.Size())
	}
	if p.Workers()[0] != kept {
		t.Error("Retain replaced the named worker instead of keeping it")
	}
}

// A name that matches nothing empties the pool, which is the honest outcome: the
// engine is about to register the worker it actually wants, and holding onto
// stale ones is the bug.
func TestRetainWithNoMatchEmptiesThePool(t *testing.T) {
	p := New()
	p.Register(newTestWorker("acct-stale"))

	p.Retain("acct-absent")

	if p.Size() != 0 {
		t.Errorf("Size = %d, want 0 when nothing matches", p.Size())
	}
}
