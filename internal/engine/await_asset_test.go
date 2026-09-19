package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// shrinkResolveWindow makes the retry window small enough to test, and restores it
// afterwards. The production values are thirty seconds and three, which is correct
// for a real upload and useless in a unit test.
func shrinkResolveWindow(t *testing.T, window time.Duration) {
	t.Helper()
	oldWindow, oldInterval := assetResolveWindow, assetResolveInterval
	assetResolveWindow, assetResolveInterval = window, time.Millisecond
	t.Cleanup(func() { assetResolveWindow, assetResolveInterval = oldWindow, oldInterval })
}

// The ordinary case: the asset is already there and nothing waits.
func TestAwaitAssetReturnsAtOnceWhenFound(t *testing.T) {
	shrinkResolveWindow(t, time.Second)

	calls := 0
	found, err := awaitAsset(context.Background(), func() (bool, error) {
		calls++
		return true, nil
	})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v, want found", found, err)
	}
	if calls != 1 {
		t.Errorf("resolve called %d times, want 1", calls)
	}
}

// The case this exists for: an upload returns its ids before the listing carries
// the row, so the first few attempts find nothing.
func TestAwaitAssetRetriesUntilTheListingCatchesUp(t *testing.T) {
	shrinkResolveWindow(t, time.Second)

	calls := 0
	found, err := awaitAsset(context.Background(), func() (bool, error) {
		calls++
		return calls >= 3, nil
	})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v, want found on the third attempt", found, err)
	}
	if calls != 3 {
		t.Errorf("resolve called %d times, want 3", calls)
	}
}

// A genuinely absent id is still a client error. It must be reported, not waited
// on forever, and it must not be reported as found.
func TestAwaitAssetGivesUpWhenTheAssetNeverArrives(t *testing.T) {
	shrinkResolveWindow(t, 50*time.Millisecond)

	calls := 0
	found, err := awaitAsset(context.Background(), func() (bool, error) {
		calls++
		return false, nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil — an absent asset is not an error here", err)
	}
	if found {
		t.Error("found = true for an asset that never appeared")
	}
	if calls < 2 {
		t.Errorf("resolve called %d times; it should have retried before giving up", calls)
	}
}

// A failed listing call is not something waiting fixes, so it is returned at once
// rather than retried.
func TestAwaitAssetReturnsAnErrorImmediately(t *testing.T) {
	shrinkResolveWindow(t, time.Second)

	sentinel := errors.New("listing call failed")
	calls := 0
	found, err := awaitAsset(context.Background(), func() (bool, error) {
		calls++
		return false, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the sentinel", err)
	}
	if found {
		t.Error("found = true alongside an error")
	}
	if calls != 1 {
		t.Errorf("resolve called %d times, want 1 — an error must not be retried", calls)
	}
}

// A cancelled context stops the wait rather than running the window out.
func TestAwaitAssetStopsOnContextCancel(t *testing.T) {
	shrinkResolveWindow(t, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	found, err := awaitAsset(ctx, func() (bool, error) { return false, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if found {
		t.Error("found = true after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v; the cancellation should have ended it", elapsed)
	}
}
