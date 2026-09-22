package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/batchexecute"
	"github.com/kodelyx/flow-go/flow-go/internal/httpx"
)

// The project self-heal. An account with no projects cannot generate at all —
// every generation call carries a project id, and one that names nothing is
// refused upstream — so the engine creates one rather than falling through to a
// boot that looks healthy and requests that fail much later with "no project id
// resolved".

// projectCreateServer answers the create RPC, and records what each request
// carried so the test can prove the create call was the one made.
func projectCreateServer(t *testing.T, payload any) (*httptest.Server, *[]string) {
	t.Helper()

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		requests = append(requests, r.PostForm.Get("f.req"))

		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("could not encode the payload: %v", err)
			return
		}
		fmt.Fprintf(w, ")]}'\n\n[[\"wrb.fr\",%q,%s,null,null,null,\"generic\"]]\n",
			batchexecute.RPCIDProjectCreate, encoded)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// pointAtServer redirects the batchexecute client at a test server for the
// duration of a test.
func pointAtServer(t *testing.T, server *httptest.Server) {
	t.Helper()
	original := batchexecute.Origin
	batchexecute.Origin = server.URL
	t.Cleanup(func() { batchexecute.Origin = original })
}

func createClient(t *testing.T) *batchexecute.Client {
	t.Helper()
	hc, err := httpx.New()
	if err != nil {
		t.Fatalf("httpx.New: %v", err)
	}
	return batchexecute.New(identityJar("sapisid-value", "rotating-value"), hc)
}

func TestProvisionProjectCreatesOneAndReturnsItsID(t *testing.T) {
	const id = "8f2c1d4e-0000-4000-8000-000000000001"
	// The frame carries its payload as JSON-encoded *text*, so this is the inner
	// document: CreateProject reads row[0] as the id and row[1] as the detail
	// array once that text is parsed.
	server, requests := projectCreateServer(t, fmt.Sprintf(`[%q,["Sep 22 - 22:00"]]`, id))
	pointAtServer(t, server)

	e := &Engine{}
	got := e.provisionProject(context.Background(), createClient(t), 0)
	if got != id {
		t.Fatalf("provisionProject returned %q, want %q", got, id)
	}

	if len(*requests) == 0 {
		t.Fatal("no request reached the create RPC")
	}
	if !strings.Contains((*requests)[0], batchexecute.RPCIDProjectCreate) {
		t.Errorf("the request did not name the create RPC: %s", (*requests)[0])
	}
}

// TestProvisionProjectReportsFailureRatherThanInventingAnID is the important
// half: a create that does not answer with an id must leave the engine with no
// project, so the boot reports the degraded state instead of generating against
// a made-up id.
func TestProvisionProjectReportsFailureRatherThanInventingAnID(t *testing.T) {
	server, _ := projectCreateServer(t, `[]`)
	pointAtServer(t, server)

	e := &Engine{}
	if got := e.provisionProject(context.Background(), createClient(t), 0); got != "" {
		t.Fatalf("provisionProject returned %q, want an empty id", got)
	}
}

func TestProvisionProjectWithoutAClient(t *testing.T) {
	e := &Engine{}
	if got := e.provisionProject(context.Background(), nil, 0); got != "" {
		t.Fatalf("provisionProject returned %q with no client, want an empty id", got)
	}
}

// projectServer answers both RPCs projectFromRPC consults: an empty project
// listing, and a create that yields an id. Both frames go out on every request
// because CallWith selects the one it asked for by rpc id.
func projectServer(t *testing.T, listPayload, createPayload string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()

		list, err := json.Marshal(listPayload)
		if err != nil {
			t.Errorf("could not encode the listing: %v", err)
			return
		}
		create, err := json.Marshal(createPayload)
		if err != nil {
			t.Errorf("could not encode the create: %v", err)
			return
		}

		fmt.Fprintf(w, ")]}'\n\n[[\"wrb.fr\",%q,%s,null,null,null,\"generic\"],"+
			"[\"wrb.fr\",%q,%s,null,null,null,\"generic\"]]\n",
			batchexecute.RPCIDProjectList, list,
			batchexecute.RPCIDProjectCreate, create)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestProjectFromRPCProvisionsWhenTheListingIsEmpty is the wiring test. The
// self-heal is worth nothing unless an empty listing actually reaches it, and
// nothing else in the suite would notice if that branch stopped being taken.
func TestProjectFromRPCProvisionsWhenTheListingIsEmpty(t *testing.T) {
	const id = "8f2c1d4e-0000-4000-8000-000000000002"
	server := projectServer(t, `[]`, fmt.Sprintf(`[%q,["Sep 22 - 22:00"]]`, id))
	pointAtServer(t, server)

	hc, err := httpx.New()
	if err != nil {
		t.Fatalf("httpx.New: %v", err)
	}
	e := &Engine{hc: hc}

	got := e.projectFromRPC(context.Background(), identityJar("sapisid-value", "rotating-value"), 0)
	if got != id {
		t.Fatalf("projectFromRPC returned %q, want the auto-provisioned %q", got, id)
	}
}
