package batchexecute

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

/*
 * Pacing the calls that spend credits.
 *
 * A refusal has no status to react to — it is HTTP 200 with a reason in the
 * frame — so there is nothing to back off *from* after the fact. The only place
 * to act is before the call goes out, which is what these pin.
 *
 * The other half matters as much: reads must not be paced. Polling depends on
 * them being immediate, and a poll spaced two seconds apart turns a seven-minute
 * wait into a much longer one.
 */

// pacedServer answers every request with a well-formed frame and records when
// each one arrived.
func pacedServer(t *testing.T) (*httptest.Server, *[]time.Time) {
	t.Helper()

	var at []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		at = append(at, time.Now())
		fmt.Fprint(w, ")]}'\n\n[[\"wrb.fr\",\"UpteDb\",\"[]\",null,null,null,\"generic\"]]\n")
	}))
	t.Cleanup(server.Close)
	return server, &at
}

func fixedPayload(string) (any, error) { return []any{"x"}, nil }

// TestSubmissionsAreSpacedApart is the property the limiter exists for: a burst
// of submissions goes out at a minimum interval rather than all at once.
func TestSubmissionsAreSpacedApart(t *testing.T) {
	const (
		gap = 150 * time.Millisecond
		// The arrivals are measured at the server, so each one carries the round
		// trip and whatever the scheduler did with it. A tolerance is part of the
		// assertion rather than a loosening of it: the property being pinned is
		// that the calls are spaced, not that the clock is exact.
		//
		// It is well under the gap on purpose. An unspaced client sends the three
		// in about a millisecond — see TestAnUnlimitedClientIsUnpaced — so the
		// threshold still separates the two cases by two orders of magnitude.
		tolerance = 30 * time.Millisecond
	)

	server, at := pacedServer(t)
	pointAt(t, server)

	client := testClient(t)
	client.SetSubmissionLimits(0, gap)

	for i := 0; i < 3; i++ {
		if _, err := client.call(context.Background(), RPCIDGenerate, CallOptions{},
			fixedPayload); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	if len(*at) != 3 {
		t.Fatalf("the server saw %d requests, want 3", len(*at))
	}
	// The first goes out immediately; each later one must be at least a gap
	// after the one before it. The gap is reserved rather than merely read, so
	// this holds even when the calls overlap.
	for i := 1; i < len(*at); i++ {
		if d := (*at)[i].Sub((*at)[i-1]); d < gap-tolerance {
			t.Errorf("requests %d and %d were %s apart, want at least %s (allowing %s of "+
				"measurement jitter)", i-1, i, d, gap, tolerance)
		}
	}
}

// TestReadsAreNotSpaced is the half that would be easy to lose. A read costs
// nothing and a refused one can simply be repeated, so pacing reads would buy no
// protection and would slow down the polling that every video wait depends on.
func TestReadsAreNotSpaced(t *testing.T) {
	const gap = 200 * time.Millisecond

	server, at := pacedServer(t)
	pointAt(t, server)

	client := testClient(t)
	client.SetSubmissionLimits(0, gap)

	for i := 0; i < 3; i++ {
		if _, err := client.call(context.Background(), RPCIDMediaDetail, CallOptions{},
			fixedPayload); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	if len(*at) != 3 {
		t.Fatalf("the server saw %d requests, want 3", len(*at))
	}
	// Three reads against a local server. If they were being paced this would be
	// at least twice the gap.
	if elapsed := (*at)[2].Sub((*at)[0]); elapsed >= gap {
		t.Errorf("three reads took %s, so they are being paced; the limiter must apply "+
			"to submissions only", elapsed)
	}
}

// TestAnUnlimitedClientIsUnpaced records the default. The limits are a policy
// the engine opts into, not a property of the transport — so a client built
// without them behaves exactly as it did before the limiter existed, and the
// existing tests that drive several calls are not silently slowed down.
func TestAnUnlimitedClientIsUnpaced(t *testing.T) {
	const gap = 200 * time.Millisecond

	server, at := pacedServer(t)
	pointAt(t, server)

	client := testClient(t) // no SetSubmissionLimits call

	for i := 0; i < 3; i++ {
		if _, err := client.call(context.Background(), RPCIDGenerate, CallOptions{},
			fixedPayload); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	if elapsed := (*at)[2].Sub((*at)[0]); elapsed >= gap {
		t.Errorf("three submissions took %s on a client with no limits set", elapsed)
	}
}

// TestTheInFlightCapSerialisesSubmissions covers the other half of the limiter.
// The spacing stops a burst from being *sent* together; the cap stops a burst
// from being *in flight* together, which is the state that matters if the
// account is being judged on how many requests it has open.
func TestTheInFlightCapSerialisesSubmissions(t *testing.T) {
	hold := make(chan struct{})
	entered := make(chan struct{}, 4)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-hold
		fmt.Fprint(w, ")]}'\n\n[[\"wrb.fr\",\"ogiZ0b\",\"[]\",null,null,null,\"generic\"]]\n")
	}))
	t.Cleanup(server.Close)
	pointAt(t, server)

	client := testClient(t)
	client.SetSubmissionLimits(1, 0) // one at a time, no spacing

	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := client.call(context.Background(), RPCIDGenerate, CallOptions{},
				fixedPayload)
			done <- err
		}()
	}

	// The first submission is with the server. The second must not be: the only
	// slot is taken.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no submission reached the server at all")
	}
	select {
	case <-entered:
		t.Fatal("a second submission reached the server while the cap was 1")
	case <-time.After(150 * time.Millisecond):
	}

	close(hold) // let the first finish so the second can take the slot

	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("call: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a submission never completed; the slot was not released")
		}
	}
}

// A cancelled wait must give the slot back. Without that, a caller that gave up
// would leak capacity the client can never recover — and after enough
// cancellations the client would stop submitting entirely.
func TestACancelledSubmissionReleasesItsSlot(t *testing.T) {
	hold := make(chan struct{})
	entered := make(chan struct{}, 4)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-hold
		fmt.Fprint(w, ")]}'\n\n[[\"wrb.fr\",\"ogiZ0b\",\"[]\",null,null,null,\"generic\"]]\n")
	}))
	t.Cleanup(server.Close)
	pointAt(t, server)

	client := testClient(t)
	client.SetSubmissionLimits(1, 0)

	// The first holds the slot. The second waits, then its context is cancelled.
	first := make(chan error, 1)
	go func() {
		_, err := client.call(context.Background(), RPCIDGenerate, CallOptions{}, fixedPayload)
		first <- err
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() {
		_, err := client.call(ctx, RPCIDGenerate, CallOptions{}, fixedPayload)
		second <- err
	}()

	// Give the second time to reach the wait, then cancel it.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-second:
		if err == nil {
			t.Fatal("a cancelled wait should report the cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled submission never returned")
	}

	close(hold)
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("the first call should have completed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first submission never completed")
	}
}
