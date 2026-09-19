package batchexecute

import (
	"strings"
	"testing"
)

// TestEndpointURLOmitsTheDefaultAccount pins that account 0 adds nothing.
//
// Every call before this parameter existed sent no `authuser` at all, and the two
// forms are not equivalent to every Google endpoint — so the default has to stay
// exactly as it was rather than becoming an explicit zero.
func TestEndpointURLOmitsTheDefaultAccount(t *testing.T) {
	c := New(nil, nil)

	got := c.endpointURL("rpcids=X&source-path=%2Fproject%2Fp")
	if strings.Contains(got, "authuser") {
		t.Errorf("account 0 should add no parameter, got %s", got)
	}
	if !strings.HasSuffix(got, "rpcids=X&source-path=%2Fproject%2Fp") {
		t.Errorf("the query was altered: %s", got)
	}
}

// TestEndpointURLAppendsTheAccount covers the switch. It must be appended, not
// substituted, or the rpcid and source-path that scope the call would be lost.
func TestEndpointURLAppendsTheAccount(t *testing.T) {
	c := New(nil, nil)
	c.SetAuthUser(2)

	got := c.endpointURL("rpcids=X&source-path=%2Fproject%2Fp")
	if !strings.HasSuffix(got, "&authuser=2") {
		t.Errorf("expected authuser appended, got %s", got)
	}
	if !strings.Contains(got, "rpcids=X") || !strings.Contains(got, "source-path=") {
		t.Errorf("the existing query was lost: %s", got)
	}
	if strings.Contains(got, "?authuser") {
		t.Errorf("authuser must not become the first parameter: %s", got)
	}
}

// TestAuthUserRoundTrips covers the accessor pair.
func TestAuthUserRoundTrips(t *testing.T) {
	c := New(nil, nil)
	if got := c.AuthUser(); got != 0 {
		t.Errorf("a fresh client should act as account 0, got %d", got)
	}
	for _, want := range []int{1, 3, 0} {
		c.SetAuthUser(want)
		if got := c.AuthUser(); got != want {
			t.Errorf("SetAuthUser(%d) then AuthUser() = %d", want, got)
		}
	}
}
