package batchexecute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

/*
 * Escalating an assessment refusal.
 *
 * A refusal by the assessment is the one refusal a caller can do something about
 * beyond explaining it: the *token* was rejected, and a token minted in a real
 * page scores higher than one minted over the transport. Every other reason — a
 * wrong model, a project belonging to another account — is answered by changing
 * the request, and minting again would only spend a round trip to learn the same
 * thing.
 *
 * The escalation is deliberately the third attempt overall and not the second.
 * Both attempts before it minted over the transport, so both presented a token of
 * the same quality; a page-minted token is the genuinely different answer, and it
 * is worth exactly one try.
 *
 * A note on the attempt counting below. `retryIfEmpty` is entered *after* the
 * first attempt has already come back empty, and the empty first response is
 * passed in as `frames`. So the first call it makes to `submit` is the **retry**,
 * and the second is the escalated attempt. Getting that off by one makes a test
 * assert the opposite of what it reads as — which is how these were written the
 * first time.
 */

// carriesNothing is the "this call produced nothing" test, which in production
// each RPC supplies for its own response shape.
func carriesNothing(frames []Frame) bool {
	for _, frame := range frames {
		if len(frame.Payload) > 0 {
			return false
		}
	}
	return true
}

// refusal is a frame the server answered with an explicit reason and no data.
func refusal(reason string) []Frame {
	return []Frame{{RPCID: RPCIDGenerate, Error: reason}}
}

// answer is a frame carrying data.
func answer() []Frame {
	return []Frame{{RPCID: RPCIDGenerate, Payload: json.RawMessage(`["asset"]`)}}
}

// escalateOpts is a call carrying both a transport refresher and a page-minting
// escalator, which is the configuration the engine wires up.
func escalateOpts(escalate func(context.Context) (string, error)) CallOptions {
	return CallOptions{
		RefreshCaptcha:  func(context.Context) (string, error) { return "transport-2", nil },
		EscalateCaptcha: escalate,
	}
}

// recorder collects the tokens each attempt was submitted with, so a test can
// assert which one went out where.
func recorder(responses ...[]Frame) (*[]string, func(string) ([]Frame, error)) {
	var tokens []string
	submit := func(token string) ([]Frame, error) {
		tokens = append(tokens, token)
		if len(tokens) > len(responses) {
			return nil, fmt.Errorf("attempt %d went out, but only %d responses were staged",
				len(tokens), len(responses))
		}
		return responses[len(tokens)-1], nil
	}
	return &tokens, submit
}

func TestAnAssessmentRefusalIsEscalatedToAPageToken(t *testing.T) {
	// The retry is refused; the escalated attempt is accepted.
	tokens, submit := recorder(refusal(ReasonUnusualActivity), answer())

	frames, err := retryIfEmpty(context.Background(), escalateOpts(
		func(context.Context) (string, error) { return "page-token", nil }),
		nil, carriesNothing, submit)

	if err != nil {
		t.Fatalf("the escalated attempt should have succeeded: %v", err)
	}
	if carriesNothing(frames) {
		t.Fatal("the escalated attempt returned nothing, yet was reported as an answer")
	}
	if len(*tokens) != 2 {
		t.Fatalf("the server saw %d attempts after the first, want 2 (retry, escalated)",
			len(*tokens))
	}
	if (*tokens)[0] != "transport-2" {
		t.Errorf("the retry carried %q, want the freshly minted transport token", (*tokens)[0])
	}
	if (*tokens)[1] != "page-token" {
		t.Errorf("the escalated attempt carried %q, want the page-minted token", (*tokens)[1])
	}
}

// Without an escalator the refusal is final, and no further attempt goes out.
// This is the browserless case: there is no page to ask, so the refusal stands
// rather than being followed by an attempt that could only fail.
func TestARefusalWithoutAnEscalatorIsFinal(t *testing.T) {
	tokens, submit := recorder(refusal(ReasonUnusualActivity))

	opts := CallOptions{RefreshCaptcha: func(context.Context) (string, error) {
		return "transport-2", nil
	}}

	_, err := retryIfEmpty(context.Background(), opts, nil, carriesNothing, submit)

	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("want a RejectedError, got %v", err)
	}
	if rejected.Reason != ReasonUnusualActivity {
		t.Errorf("reason = %q, want %q", rejected.Reason, ReasonUnusualActivity)
	}
	if len(*tokens) != 1 {
		t.Errorf("the server saw %d attempts after the first, want 1 — nothing is configured "+
			"to escalate to", len(*tokens))
	}
}

// A page-minted token that is refused too means the assessment is refusing the
// client rather than the token, and the refusal is what the caller gets.
func TestAnEscalatedRefusalIsStillARefusal(t *testing.T) {
	tokens, submit := recorder(refusal(ReasonUnusualActivity), refusal(ReasonUnusualActivity))

	_, err := retryIfEmpty(context.Background(), escalateOpts(
		func(context.Context) (string, error) { return "page-token", nil }),
		nil, carriesNothing, submit)

	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("want a RejectedError, got %v", err)
	}
	if rejected.Reason != ReasonUnusualActivity {
		t.Errorf("reason = %q", rejected.Reason)
	}
	if len(*tokens) != 2 {
		t.Errorf("the server saw %d attempts after the first, want 2 — the escalation should "+
			"have been tried exactly once", len(*tokens))
	}
}

// A mint that fails leaves the refusal standing. Reporting the mint failure
// instead would replace a stated reason with a vaguer one, and there is nothing
// to resubmit with.
func TestAFailedEscalationLeavesTheRefusalStanding(t *testing.T) {
	tokens, submit := recorder(refusal(ReasonUnusualActivity))

	_, err := retryIfEmpty(context.Background(), escalateOpts(
		func(context.Context) (string, error) { return "", fmt.Errorf("no extension attached") }),
		nil, carriesNothing, submit)

	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("want a RejectedError, got %v", err)
	}
	if rejected.Reason != ReasonUnusualActivity {
		t.Errorf("reason = %q, want the refusal rather than the mint failure", rejected.Reason)
	}
	if len(*tokens) != 1 {
		t.Errorf("the server saw %d attempts after the first, want 1 — a failed mint must not "+
			"be resubmitted", len(*tokens))
	}
}

// An empty token is a failure, not a token. submit("") means "keep the token you
// already have", so passing one on would resend the token that was just refused
// — the exact bug the refresher exists to prevent, arriving by a new route.
func TestAnEmptyEscalatedTokenIsNotResubmitted(t *testing.T) {
	tokens, submit := recorder(refusal(ReasonUnusualActivity))

	_, err := retryIfEmpty(context.Background(), escalateOpts(
		func(context.Context) (string, error) { return "", nil }),
		nil, carriesNothing, submit)

	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("want a RejectedError, got %v", err)
	}
	if len(*tokens) != 1 {
		t.Errorf("the server saw %d attempts after the first, want 1 — an empty token would "+
			"have resent the spent one", len(*tokens))
	}
}

// Any other refusal is final. Minting again cannot fix a wrong model or a project
// belonging to another account, and doing it anyway would spend a browser round
// trip on every such failure.
func TestOnlyTheAssessmentRefusalIsEscalated(t *testing.T) {
	escalated := 0
	tokens, submit := recorder(refusal("PUBLIC_ERROR_SOMETHING_ELSE"))

	_, err := retryIfEmpty(context.Background(), escalateOpts(
		func(context.Context) (string, error) {
			escalated++
			return "page-token", nil
		}),
		nil, carriesNothing, submit)

	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("want a RejectedError, got %v", err)
	}
	if escalated != 0 {
		t.Errorf("a non-assessment refusal was escalated %d times", escalated)
	}
	if rejected.Reason != "PUBLIC_ERROR_SOMETHING_ELSE" {
		t.Errorf("reason = %q, want the reason the server actually gave", rejected.Reason)
	}
	if len(*tokens) != 1 {
		t.Errorf("the server saw %d attempts after the first, want 1", len(*tokens))
	}
}

// The reason the escalation is worth a round trip at all: the retry's reason is
// kept when the escalated attempt carries none of its own, so a caller that gets
// nothing back still learns why.
func TestAnEscalationThatAnswersNothingKeepsTheRefusalReason(t *testing.T) {
	tokens, submit := recorder(refusal(ReasonUnusualActivity), nil)

	_, err := retryIfEmpty(context.Background(), escalateOpts(
		func(context.Context) (string, error) { return "page-token", nil }),
		nil, carriesNothing, submit)

	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("want a RejectedError, got %v", err)
	}
	if rejected.Reason != ReasonUnusualActivity {
		t.Errorf("reason = %q, want the refusal that was already known", rejected.Reason)
	}
	if len(*tokens) != 2 {
		t.Errorf("attempts = %d, want 2", len(*tokens))
	}
}
