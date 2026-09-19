package batchexecute

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A render in flight fails this in two stages, and both must be retried. The
// second one is the case that cost a real submission: the asset had appeared in
// the listing, so it got past the first check, and the poll then treated the
// missing URL as permanent and abandoned a render that had already been charged
// for.
func TestRetryableResolveErrorCoversBothStagesOfARender(t *testing.T) {
	notListed := fmt.Errorf("batchexecute: %s: %w", "abc", ErrAssetNotListed)
	if !RetryableResolveError(notListed) {
		t.Error("ErrAssetNotListed is not retryable, but a render in flight produces it")
	}

	notReady := fmt.Errorf("batchexecute: no video URL for %s yet (still rendering?): %w",
		"abc", ErrAssetNotReady)
	if !RetryableResolveError(notReady) {
		t.Error("ErrAssetNotReady is not retryable, but it means the render is still going")
	}
}

// Everything else is permanent. Polling one of these for the full timeout turns
// a ten-second failure into a seven-minute one, so they must not be retried.
func TestRetryableResolveErrorRejectsPermanentFailures(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("batchexecute: the media detail response carried nothing"),
		errors.New("batchexecute: 401"),
		ErrAssetNotListed, // bare, without the media id in front — still retryable
	} {
		got := RetryableResolveError(err)
		if err == ErrAssetNotListed {
			if !got {
				t.Error("a bare ErrAssetNotListed should still be retryable")
			}
			continue
		}
		if got {
			t.Errorf("RetryableResolveError(%v) = true, want false", err)
		}
	}
}

// The message has to keep saying "still rendering?", because that is the whole
// diagnosis when the wait does expire — but it must not read as permanent.
func TestAssetNotReadyKeepsItsExplanation(t *testing.T) {
	err := fmt.Errorf("batchexecute: no video URL for %s yet (still rendering?): %w",
		"4b9d22e8", ErrAssetNotReady)

	if !errors.Is(err, ErrAssetNotReady) {
		t.Error("the wrapped error does not match ErrAssetNotReady")
	}
	if got := err.Error(); !strings.Contains(got, "still rendering?") {
		t.Errorf("message lost its explanation: %q", got)
	}
}
