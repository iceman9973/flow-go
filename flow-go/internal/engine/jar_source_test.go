package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/batchexecute"
	"github.com/kodelyx/flow-go/flow-go/internal/bridge"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/httpx"
)

/*
 * Which account a request acts as.
 *
 * The bridge holds one jar and SyncCookies overwrites it once per attached
 * profile, so after a boot that registered several, `bridge.Jar()` answers with
 * the last one's cookies. Four call sites used to read it that way, including
 * two that are on the generation path — and the failure that produces is the
 * silent kind: a submission sent under another account's session while naming
 * this account's project is accepted and answered with an empty result.
 *
 * The test above in bundle_gate_test.go pins `Engine.Jar()`'s own preference.
 * These pin the call sites, because a correct accessor that nobody calls is
 * worth nothing — and that is exactly how the four sites drifted apart in the
 * first place.
 */

// testServerJar is a jar whose cookies are actually addressable at an httptest
// server.
//
// The domain is the whole point. identityJar's cookies carry Domain
// ".google.com", and Jar.ForDomain matches on host — so against a server on
// 127.0.0.1 such a jar attaches *nothing*, and the Cookie header is empty
// whichever jar was used. An assertion built on that would pass no matter what
// the engine did. The mechanism under test is which jar is consulted, so the
// fixture has to make the two distinguishable, not identical.
func testServerJar(sapisid string) *cookiejar.Jar {
	return cookiejar.FromCookies([]cookiejar.Cookie{
		{Name: "SAPISID", Value: sapisid, Domain: "127.0.0.1", Path: "/"},
		{Name: "SID", Value: "sid-" + sapisid, Domain: "127.0.0.1", Path: "/"},
	}, "test")
}

// listingServer answers the project-content RPC with a single asset row and
// records the Cookie header of every request, which is what tells the test
// which account's jar the engine acted as.
//
// The row is the flat listing shape — [content-id, project-id, media-id,
// type-code, ...] — with the original type code, so findAsset takes it without
// needing a derived variant.
func listingServer(t *testing.T, contentID, projectID, mediaID string) (*httptest.Server, *[]string) {
	t.Helper()

	var cookies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		cookies = append(cookies, r.Header.Get("Cookie"))

		listing := fmt.Sprintf(`[[%q,%q,%q,%q,null]]`,
			contentID, projectID, mediaID, batchexecute.AssetTypeOriginal)
		encoded, err := json.Marshal(listing)
		if err != nil {
			t.Errorf("could not encode the listing: %v", err)
			return
		}
		fmt.Fprintf(w, ")]}'\n\n[[\"wrb.fr\",%q,%s,null,null,null,\"generic\"]]\n",
			batchexecute.RPCIDProjectContent, encoded)
	}))
	t.Cleanup(server.Close)
	return server, &cookies
}

// bridgeHolding persists one account file and returns a bridge that will serve
// it as its own jar — the stand-in for "some other profile synced last".
func bridgeHolding(t *testing.T, dir, accountID, sapisid string) *bridge.Bridge {
	t.Helper()

	bundle := &cookiejar.Bundle{
		AccountID: accountID,
		Cookies:   testServerJar(sapisid).Cookies(),
	}
	if err := bundle.Save(filepath.Join(dir, "account_"+accountID+".json")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	br := bridge.NewBridge(nil, nil, dir)
	if br.Jar() == nil {
		t.Fatal("the fixture is wrong: the bridge must hold a jar, or this proves nothing")
	}
	return br
}

// TestAssetResolutionUsesTheEnginesOwnJar is the regression test for the cookie
// source on the generation path.
//
// Both helpers below are reached from a live generation — conditionImageID from
// GenerateVideoViaBatch for its --start-image / --end-image, ResolveContentID
// from the same resolution — and both used to build their client from the
// bridge's jar. The two jars are made distinguishable by their SAPISID, so the
// Cookie header the server saw says which account the request acted as.
func TestAssetResolutionUsesTheEnginesOwnJar(t *testing.T) {
	const (
		contentID = "11111111-1111-4111-8111-111111111111"
		projectID = "33333333-3333-4333-8333-333333333333"
		mediaID   = "22222222-2222-4222-8222-222222222222"
	)

	cases := []struct {
		name string
		call func(*Engine) (string, error)
	}{
		{
			// The media id is what a fresh upload returns, so this is the path
			// an uploaded frame actually takes.
			name: "conditionImageID",
			call: func(e *Engine) (string, error) {
				return e.conditionImageID(context.Background(), mediaID)
			},
		},
		{
			name: "ResolveContentID",
			call: func(e *Engine) (string, error) {
				return e.ResolveContentID(context.Background(), mediaID)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("FLOW_COOKIE_DIR", dir)
			br := bridgeHolding(t, dir, "acct-other", "other-sapisid")

			server, cookies := listingServer(t, contentID, projectID, mediaID)
			pointAtServer(t, server)

			hc, err := httpx.New()
			if err != nil {
				t.Fatalf("httpx.New: %v", err)
			}
			e := &Engine{
				hc:         hc,
				bridge:     br,
				primaryJar: testServerJar("mine-sapisid"),
				ready:      true,
				projectID:  projectID,
			}

			got, err := tc.call(e)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got != contentID {
				t.Fatalf("%s resolved %q, want the content id %q", tc.name, got, contentID)
			}

			if len(*cookies) == 0 {
				t.Fatal("no request reached the listing RPC")
			}
			for i, header := range *cookies {
				if strings.Contains(header, "other-sapisid") {
					t.Errorf("request %d carried the bridge's cookies — the engine acted as "+
						"another account: Cookie=%q", i, header)
				}
				if !strings.Contains(header, "mine-sapisid") {
					t.Errorf("request %d did not carry the engine's own cookies: Cookie=%q",
						i, header)
				}
			}
		})
	}
}

// TestAssetResolutionStillFallsBackToTheBridge pins the half that must not
// change. Before Bootstrap has captured a jar there is nothing of the engine's
// own, and the bridge is what is left — so the fix must not have turned a
// browserless first run into "no cookies loaded".
func TestAssetResolutionStillFallsBackToTheBridge(t *testing.T) {
	const (
		contentID = "44444444-4444-4444-8444-444444444444"
		projectID = "55555555-5555-4555-8555-555555555555"
		mediaID   = "66666666-6666-4666-8666-666666666666"
	)

	dir := t.TempDir()
	t.Setenv("FLOW_COOKIE_DIR", dir)
	br := bridgeHolding(t, dir, "acct-only", "only-sapisid")

	server, cookies := listingServer(t, contentID, projectID, mediaID)
	pointAtServer(t, server)

	hc, err := httpx.New()
	if err != nil {
		t.Fatalf("httpx.New: %v", err)
	}
	// No primaryJar: this is a process that has not bootstrapped yet.
	e := &Engine{hc: hc, bridge: br, ready: true, projectID: projectID}

	got, err := e.conditionImageID(context.Background(), mediaID)
	if err != nil {
		t.Fatalf("conditionImageID: %v", err)
	}
	if got != contentID {
		t.Fatalf("resolved %q, want %q", got, contentID)
	}

	if len(*cookies) == 0 {
		t.Fatal("no request reached the listing RPC")
	}
	if !strings.Contains((*cookies)[0], "only-sapisid") {
		t.Errorf("the bridge's jar should have been used before a boot: Cookie=%q", (*cookies)[0])
	}
}
