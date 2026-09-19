package auth

import (
	"testing"
	"time"

	"github.com/kodelyx/cdp-control/cookiejar"
	"github.com/kodelyx/flow-go/internal/httpx"
)

// TestAccountIndexDefaultsToZero pins the default. Zero is the first signed-in
// account and is the value every existing caller already behaves as, so a change
// here would silently move every deployment to a different account.
func TestAccountIndexDefaultsToZero(t *testing.T) {
	p := NewProvider(cookiejar.FromRaw("SID=x", ".google.com", "test"), nil)
	if got := p.AccountIndex(); got != 0 {
		t.Errorf("a fresh provider should act as account 0, got %d", got)
	}
}

// TestSetAccountIndexRoundTrips covers the switch itself.
func TestSetAccountIndexRoundTrips(t *testing.T) {
	p := NewProvider(cookiejar.FromRaw("SID=x", ".google.com", "test"), nil)
	for _, want := range []int{0, 1, 2, 5} {
		p.SetAccountIndex(want)
		if got := p.AccountIndex(); got != want {
			t.Errorf("SetAccountIndex(%d) then AccountIndex() = %d", want, got)
		}
	}
}

// TestSetAccountIndexClampsNegative keeps a nonsensical index from reaching the
// query string as `authuser=-1`, which would be sent rather than omitted and
// would fail the mint for a reason that looks nothing like the cause.
func TestSetAccountIndexClampsNegative(t *testing.T) {
	p := NewProvider(cookiejar.FromRaw("SID=x", ".google.com", "test"), nil)
	p.SetAccountIndex(-3)
	if got := p.AccountIndex(); got != 0 {
		t.Errorf("a negative index should clamp to 0, got %d", got)
	}
}

// TestSetAccountIndexDropsTheCachedToken is the one that matters most.
//
// Accounts share a cookie jar, so the cookie hash does not change when the
// account does — which means the existing cache key would happily serve the old
// account's token to the new account's calls. The switch has to discard it.
func TestSetAccountIndexDropsTheCachedToken(t *testing.T) {
	p := NewProvider(cookiejar.FromRaw("SID=x", ".google.com", "test"), httpxMust(t))

	p.mu.Lock()
	p.cached = &Session{AccessToken: "token-for-account-0", MintedAt: time.Now()}
	p.mu.Unlock()

	p.SetAccountIndex(1)

	p.mu.Lock()
	cached := p.cached
	p.mu.Unlock()

	if cached != nil {
		t.Fatalf("the cached token survived an account switch: %+v", cached)
	}
}

func httpxMust(t *testing.T) *httpx.Client {
	t.Helper()
	hc, err := httpx.New()
	if err != nil {
		t.Fatalf("httpx.New: %v", err)
	}
	return hc
}
