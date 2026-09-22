package batchexecute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// **The first generation after a boot comes back empty** — HTTP 200, no error,
// no asset, and about a second where a real one takes twenty-odd. The same
// submission sent again succeeds, and the captcha token is the only thing that
// differs, so the retry has to mint a fresh one rather than resend the payload
// it already spent.
//
// These tests pin that, and the four cases where it must *not* happen: a
// response that carried an asset, a call with no refresher, a retry that also
// comes back empty, and a video-shaped response read by the image test.

// assetFrame is a generation response carrying one asset, in the shape the image
// RPC answers with.
const assetID = "11111111-2222-3333-4444-555555555555"

func assetPayload() string {
	inner := fmt.Sprintf(
		`[["a",null,"b",null,null,null,null,null,null,`+
			`"https://flow-content.google/image/%s?Expires=1&KeyName=k&Signature=s"]]`, assetID)
	return inner
}

// emptyPayload is what Flow answers when it accepts a request and does nothing.
const emptyPayload = "null"

// generationServer answers each request with the payload for that turn, and
// records what every request carried. A payload of "" means "no more turns".
func generationServer(t *testing.T, rpcID string, payloads ...string) (*httptest.Server, *[]string) {
	t.Helper()

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		requests = append(requests, r.PostForm.Get("f.req"))

		n := len(requests) - 1
		if n >= len(payloads) {
			n = len(payloads) - 1
		}
		payload := payloads[n]

		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("could not encode the payload: %v", err)
			return
		}
		fmt.Fprintf(w, ")]}'\n\n[[\"wrb.fr\",%q,%s,null,null,null,\"generic\"]]\n", rpcID, encoded)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// imageRequest is a request that passes the client's own validation, so the
// tests exercise the retry rather than the argument checks.
func imageRequest(token string) GenerateRequest {
	return GenerateRequest{
		ProjectID:    "proj-1",
		Model:        "NARWHAL",
		Prompt:       "a red apple on a wooden table",
		CaptchaToken: token,
	}
}

// A fresh mint on every refresh, counting so a test can see how many happened.
func refresher(calls *int) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		*calls++
		return fmt.Sprintf("token-%d", *calls), nil
	}
}

func TestAnEmptyGenerationIsRetriedWithAFreshToken(t *testing.T) {
	server, requests := generationServer(t, RPCIDGenerate, emptyPayload, assetPayload())
	pointAt(t, server)

	minted := 0
	media, err := testClient(t).GenerateMedia(context.Background(), imageRequest("token-0"),
		CallOptions{RefreshCaptcha: refresher(&minted)})
	if err != nil {
		t.Fatalf("GenerateMedia: %v", err)
	}

	if len(*requests) != 2 {
		t.Fatalf("the server saw %d requests, want 2 — the first attempt and one retry", len(*requests))
	}
	if minted != 1 {
		t.Errorf("minted %d tokens, want exactly 1 — one extra attempt", minted)
	}
	if (*requests)[0] == (*requests)[1] {
		t.Fatal("the retry resent the payload it was given. The token is single-use, so " +
			"the retry has to carry a fresh one or it presents a spent token")
	}
	if !strings.Contains((*requests)[1], "token-1") {
		t.Errorf("the retry did not carry the freshly minted token:\n  %s", (*requests)[1])
	}

	if len(media) != 1 {
		t.Fatalf("parsed %d assets, want 1", len(media))
	}
	if media[0].MediaID != assetID {
		t.Errorf("MediaID = %q, want %q", media[0].MediaID, assetID)
	}
}

// The retry is for the boot case, not a habit. A response that carried an asset
// is returned as it stands, and nothing is minted.
func TestAGenerationThatWorkedIsNotRetried(t *testing.T) {
	server, requests := generationServer(t, RPCIDGenerate, assetPayload())
	pointAt(t, server)

	minted := 0
	media, err := testClient(t).GenerateMedia(context.Background(), imageRequest("token-0"),
		CallOptions{RefreshCaptcha: refresher(&minted)})
	if err != nil {
		t.Fatalf("GenerateMedia: %v", err)
	}

	if len(*requests) != 1 {
		t.Errorf("the server saw %d requests, want 1 — a success is not retried", len(*requests))
	}
	if minted != 0 {
		t.Errorf("minted %d tokens, want 0", minted)
	}
	if len(media) != 1 {
		t.Errorf("parsed %d assets, want 1", len(media))
	}
}

// A call with no refresher carries no captcha, so an empty answer is the
// caller's to interpret. Retrying would be inventing a token it never had.
func TestAnEmptyGenerationIsNotRetriedWithoutARefresher(t *testing.T) {
	server, requests := generationServer(t, RPCIDGenerate, emptyPayload, assetPayload())
	pointAt(t, server)

	media, err := testClient(t).GenerateMedia(context.Background(), imageRequest("token-0"), CallOptions{})
	if err != nil {
		t.Fatalf("GenerateMedia: %v", err)
	}

	if len(*requests) != 1 {
		t.Errorf("the server saw %d requests, want 1 — no refresher, no retry", len(*requests))
	}
	if len(media) != 0 {
		t.Errorf("parsed %d assets, want 0", len(media))
	}
}

// When the retry comes back empty too the empty result is reported as it is. An
// error here would be invented: Flow answered 200 both times, and the caller
// already has a way to say "nothing came back".
func TestAnEmptyRetryIsReportedRatherThanFailed(t *testing.T) {
	server, requests := generationServer(t, RPCIDGenerate, emptyPayload, emptyPayload)
	pointAt(t, server)

	minted := 0
	media, err := testClient(t).GenerateMedia(context.Background(), imageRequest("token-0"),
		CallOptions{RefreshCaptcha: refresher(&minted)})
	if err != nil {
		t.Fatalf("an empty retry should not be an error: %v", err)
	}

	if len(*requests) != 2 {
		t.Errorf("the server saw %d requests, want 2 — one attempt, one retry", len(*requests))
	}
	if len(media) != 0 {
		t.Errorf("parsed %d assets, want 0", len(media))
	}
}

// A refresher that fails leaves the empty response alone rather than sending a
// spent token: the first attempt's token is gone, and there is no second one.
func TestAnEmptyGenerationWithAFailingRefresherFailsAndIsNotRetried(t *testing.T) {
	server, requests := generationServer(t, RPCIDGenerate, emptyPayload, assetPayload())
	pointAt(t, server)

	opts := CallOptions{RefreshCaptcha: func(context.Context) (string, error) {
		return "", fmt.Errorf("the mint is down")
	}}
	media, err := testClient(t).GenerateMedia(context.Background(), imageRequest("token-0"), opts)

	// The mint failure is reported rather than swallowed. It used to come back as
	// an empty success, and an empty response is exactly what an upstream refusal
	// looks like — so the caller could not tell "nothing came back" from "the
	// captcha could not be minted", which is the cause it actually needed.
	if err == nil {
		t.Fatal("a failed mint must be reported, not returned as an empty success")
	}
	if !strings.Contains(err.Error(), "captcha mint failed") {
		t.Errorf("error %q should say the mint is what failed", err)
	}
	if !strings.Contains(err.Error(), "the mint is down") {
		t.Errorf("error %q should carry the underlying cause", err)
	}

	if len(*requests) != 1 {
		t.Errorf("the server saw %d requests, want 1 — a failed mint cannot produce a retry", len(*requests))
	}
	if len(media) != 0 {
		t.Errorf("parsed %d assets, want 0", len(media))
	}
}

// The video RPCs answer in a listing shape, not the image one. A shared
// emptiness test would read every video response as empty and retry all of them,
// which is the expensive way to be wrong.
//
// The payload is the shape ParseGeneratedMediaIDs documents and the one its own
// test uses: [null, <credits>, [[<media-id>, …, <project-id>], …]].
func TestTheVideoEmptinessTestDoesNotReadAnAssetAsEmpty(t *testing.T) {
	videoPayload := json.RawMessage(
		`[null,357,[["694b7630-703d-4520-8687-04bafaae747c",null,null,` +
			`["Paper boat",[1789727042,291993000]],"d86bc0b4-30dc-4a52-9f3d-cb90be0079ea"]]]`)

	frames := []Frame{{RPCID: RPCIDGenerateVideo, Payload: videoPayload}}

	if carriesNoMediaIDs(frames) {
		t.Error("carriesNoMediaIDs read a response holding an id as empty")
	}
	if !carriesNoMedia(frames) {
		t.Error("carriesNoMedia read the same response as holding an asset URL; " +
			"the two tests must not agree, or one of them is being used on the wrong shape")
	}
}
