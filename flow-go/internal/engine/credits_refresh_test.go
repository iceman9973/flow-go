package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/pool"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

// newCreditsTestEngine builds just enough engine to exercise RefreshCredits:
// a pool and a store, with no bridge and no HTTP client. RefreshCredits touches
// nothing else, and a test that needed a browser would not be worth having.
func newCreditsTestEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return &Engine{pool: pool.New(), store: st}, st
}

// TestRefreshCreditsRecordsTheBalanceAndKeepsTheIdentity covers the store half of
// the Bootstrap refresh: the balance and its timestamp reach the account row.
//
// The cookie hash half is the one that matters. This write passes an empty
// CookieHash because a balance update has nothing to say about identity, and that
// column used to be assigned unconditionally — so wiring this refresh up would
// have erased the hash Bootstrap had just recorded, which is what the account id
// is derived from. It is only safe now because UpsertAccount guards it.
func TestRefreshCreditsRecordsTheBalanceAndKeepsTheIdentity(t *testing.T) {
	eng, st := newCreditsTestEngine(t)

	// What Bootstrap records before the refresh runs.
	if err := st.UpsertAccount(store.Account{
		AccountID:  "acct-1",
		CookieHash: "hash-from-bootstrap",
		Status:     "active",
	}); err != nil {
		t.Fatalf("seed UpsertAccount: %v", err)
	}

	w := pool.NewWorker("acct-1", nil)
	w.SetCreditsReader(func(context.Context) (pool.Balance, error) {
		return pool.Balance{Credits: 1050, SKU: "G1_TIER1"}, nil
	})
	eng.pool.Register(w)

	eng.RefreshCredits(context.Background())

	accounts, err := st.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}

	got := accounts[0]
	if got.Credits == nil || *got.Credits != 1050 {
		t.Errorf("Credits = %v, want 1050", got.Credits)
	}
	if got.CreditsCheckedAt == nil {
		t.Error("CreditsCheckedAt is nil — a successful read should stamp it")
	}
	if got.CookieHash != "hash-from-bootstrap" {
		t.Errorf("CookieHash = %q, want it preserved — the balance write must not clear it",
			got.CookieHash)
	}
	if got.SKU != "G1_TIER1" {
		t.Errorf("SKU = %q, want G1_TIER1", got.SKU)
	}
}

// TestRefreshCreditsStampsNothingWhenTheReadFails: an account whose balance could
// not be read must not acquire a timestamp, or it starts looking checked. The
// store has its own test for the NULL-credits case; this one is about the
// timestamp being written beside it.
func TestRefreshCreditsStampsNothingWhenTheReadFails(t *testing.T) {
	eng, st := newCreditsTestEngine(t)

	if err := st.UpsertAccount(store.Account{
		AccountID:  "acct-1",
		CookieHash: "hash-from-bootstrap",
		Status:     "active",
	}); err != nil {
		t.Fatalf("seed UpsertAccount: %v", err)
	}

	w := pool.NewWorker("acct-1", nil)
	w.SetCreditsReader(func(context.Context) (pool.Balance, error) {
		return pool.Balance{}, errors.New("nzlxg answered 401")
	})
	eng.pool.Register(w)

	eng.RefreshCredits(context.Background())

	accounts, _ := st.ListAccounts()
	if accounts[0].Credits != nil {
		t.Errorf("Credits = %v, want nil after a failed read", *accounts[0].Credits)
	}
	if accounts[0].CreditsCheckedAt != nil {
		t.Error("CreditsCheckedAt was stamped for a read that produced no balance")
	}
	if accounts[0].CookieHash != "hash-from-bootstrap" {
		t.Errorf("CookieHash = %q, want it preserved", accounts[0].CookieHash)
	}
}

// TestRefreshCreditsSurvivesAFailingWorker: one account that cannot be read must
// not stop the others being recorded. Bootstrap runs this on every switch, so a
// single dead account would otherwise keep every balance stale.
func TestRefreshCreditsSurvivesAFailingWorker(t *testing.T) {
	eng, st := newCreditsTestEngine(t)

	for _, id := range []string{"acct-bad", "acct-good"} {
		if err := st.UpsertAccount(store.Account{AccountID: id, CookieHash: "h-" + id, Status: "active"}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	bad := pool.NewWorker("acct-bad", nil)
	bad.SetCreditsReader(func(context.Context) (pool.Balance, error) {
		return pool.Balance{}, errors.New("nzlxg answered 401")
	})
	eng.pool.Register(bad)

	good := pool.NewWorker("acct-good", nil)
	good.SetCreditsReader(func(context.Context) (pool.Balance, error) {
		return pool.Balance{Credits: 1050}, nil
	})
	eng.pool.Register(good)

	eng.RefreshCredits(context.Background())

	accounts, _ := st.ListAccounts()
	byID := make(map[string]store.Account, len(accounts))
	for _, a := range accounts {
		byID[a.AccountID] = a
	}

	if got := byID["acct-good"]; got.Credits == nil || *got.Credits != 1050 {
		t.Errorf("acct-good Credits = %v, want 1050 — a failing neighbour stopped it being recorded",
			got.Credits)
	}
	if got := byID["acct-bad"]; got.Credits != nil {
		t.Errorf("acct-bad Credits = %v, want nil", *got.Credits)
	}
	if got := byID["acct-good"]; got.CookieHash != "h-acct-good" {
		t.Errorf("acct-good CookieHash = %q, want it preserved", got.CookieHash)
	}
}
