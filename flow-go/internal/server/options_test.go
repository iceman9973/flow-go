package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/kodelyx/Browser-cdp/cdp-control/bridge"
	"github.com/kodelyx/flow-go/flow-go/internal/engine"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

/* ------------------------------------------------------------------ *
 * Fixtures
 * ------------------------------------------------------------------ */

// testApp builds a real router over a real store, with an engine that has no
// session.
//
// No session is the point rather than a limitation: an unready engine fails the
// generation at preflight, which is *after* the handler has decided what to do
// with the request's fields. So a request carrying an unsupported field reaches
// the same code path it would in production, and the status it comes back with
// says whether that field was refused or merely noted.
func testApp(t *testing.T) *fiber.App {
	t.Helper()
	app, _ := testAppWithStore(t)
	return app
}

// testAppWithStore is testApp plus the store, for a test that has to seed a row
// before asking the API about it.
func testAppWithStore(t *testing.T) (*fiber.App, *store.Store) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	br := bridge.NewBridge([]string{"https://flow.google.com"}, []string{"google.com"}, t.TempDir())
	eng, err := engine.New(st, br, engine.Options{})
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}

	app := fiber.New()
	RegisterRoutes(app, eng, br)
	return app, st
}

func postJSON(t *testing.T, app *fiber.App, path, body string) (int, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("the response is not JSON (%s): %v", raw, err)
	}
	return resp.StatusCode, decoded
}

// ignoredFields reads the `ignored` list out of a response body.
func ignoredFields(t *testing.T, body map[string]any) []string {
	t.Helper()

	raw, ok := body["ignored"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("`ignored` is %T, want an array", raw)
	}

	var out []string
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("an entry in `ignored` is %T, want an object", item)
		}
		field, _ := entry["field"].(string)
		out = append(out, field)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

/* ------------------------------------------------------------------ *
 * The handler no longer refuses the request
 * ------------------------------------------------------------------ */

func TestVideoGenerationDoesNotRefuseAnUnsupportedField(t *testing.T) {
	// The behaviour this replaces: `{"prompt":"x","aspect":"9:16"}` came back
	// 400 with "unsupported option(s): aspect". A 400 for a field that has no
	// bearing on whether the generation can run is a failure the caller has to
	// clear by hand before the server will do the thing it could already do.
	app := testApp(t)

	status, body := postJSON(t, app, "/v1/videos/generations",
		`{"prompt":"a paper boat","aspect":"9:16","seed":7,"resolution":"4k"}`)

	if status == http.StatusBadRequest {
		t.Fatalf("an unsupported field still produced a 400: %v", body)
	}
	// 503 because the test engine has no session — which is the point: the
	// request got far enough to be judged on its merits.
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 from the unready engine: %v", status, body)
	}
}

func TestVideoGenerationNamesEveryFieldItIgnored(t *testing.T) {
	app := testApp(t)

	_, body := postJSON(t, app, "/v1/videos/generations",
		`{"prompt":"a paper boat","aspect":"9:16","seed":7,"resolution":"4k","audio_preference":"silent"}`)

	got := ignoredFields(t, body)
	for _, want := range []string{"aspect", "seed", "resolution", "audio_preference"} {
		if !contains(got, want) {
			t.Errorf("`ignored` does not name %q: %v", want, got)
		}
	}
}

func TestVideoGenerationReportsTheIgnoredFieldsOnAFailureToo(t *testing.T) {
	// A caller who sent `aspect` and hit a failure has two candidate
	// explanations. Saying which fields were dropped is what stops them chasing
	// the wrong one.
	app := testApp(t)

	status, body := postJSON(t, app, "/v1/videos/generations",
		`{"prompt":"a paper boat","aspect":"9:16"}`)
	if status < 400 {
		t.Fatalf("expected a failure from the unready engine, got %d", status)
	}
	if !contains(ignoredFields(t, body), "aspect") {
		t.Errorf("the failure response dropped the ignored note: %v", body)
	}
}

func TestVideoGenerationStillRefusesAnEmptyPrompt(t *testing.T) {
	// Dropping the unsupported-field 400 must not drop the 400s that are about
	// the request actually being unusable.
	app := testApp(t)

	status, body := postJSON(t, app, "/v1/videos/generations", `{"aspect":"9:16"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a missing prompt: %v", status, body)
	}
	if !strings.Contains(body["error"].(string), "prompt is required") {
		t.Errorf("unexpected error: %v", body["error"])
	}
}

func TestVideoGenerationIgnoresNothingForASupportedRequest(t *testing.T) {
	// The common case must stay clean: no `ignored` key at all, so a caller who
	// sends only supported fields sees exactly the response they saw before this
	// field existed.
	app := testApp(t)

	_, body := postJSON(t, app, "/v1/videos/generations",
		`{"prompt":"a paper boat","duration":8,"quality":"360p","count":2}`)

	if got := ignoredFields(t, body); len(got) != 0 {
		t.Errorf("a fully supported request reported ignored fields: %v", got)
	}
}

func TestImageGenerationDoesNotRefuseCountAboveOne(t *testing.T) {
	app := testApp(t)

	status, body := postJSON(t, app, "/v1/images/generations", `{"prompt":"a red boat","count":4}`)

	if status == http.StatusBadRequest {
		t.Fatalf("count > 1 still produced a 400: %v", body)
	}
	if !contains(ignoredFields(t, body), "count") {
		t.Errorf("`ignored` does not name count: %v", body)
	}
}

func TestImageGenerationDoesNotRefuseCountOfOne(t *testing.T) {
	// The original report was that `{"count": 1}` was refused. It was not — the
	// threshold was `> 1` — but a count of one must stay silent either way, and
	// the boundary is the thing worth pinning.
	app := testApp(t)

	_, body := postJSON(t, app, "/v1/images/generations", `{"prompt":"a red boat","count":1}`)

	if got := ignoredFields(t, body); len(got) != 0 {
		t.Errorf("count of 1 reported an ignored field: %v", got)
	}
}

/* ------------------------------------------------------------------ *
 * The option lists
 * ------------------------------------------------------------------ */

func TestIgnoredVideoOptionsIsEmptyForASupportedRequest(t *testing.T) {
	req := VideoGenerationRequest{
		Prompt: "a paper boat", Duration: 8, Quality: "360p", Count: 2,
		StartImage: "media-1", StartFrame: []float64{0, 0, 1},
	}

	if got := ignoredVideoOptions(req); len(got) != 0 {
		t.Errorf("a supported request was reported as ignored: %+v", got)
	}
}

func TestIgnoredVideoOptionsNamesTheUnappliedFields(t *testing.T) {
	seed := int64(7)
	req := VideoGenerationRequest{
		Prompt:          "a paper boat",
		Aspect:          "9:16",
		Resolution:      "4k",
		Seed:            &seed,
		AudioPreference: "silent",
	}

	got := ignoredVideoOptions(req)
	var fields []string
	for _, option := range got {
		fields = append(fields, option.Field)
	}
	for _, want := range []string{"aspect", "resolution", "seed", "audio_preference"} {
		if !contains(fields, want) {
			t.Errorf("ignoredVideoOptions missed %q: %v", want, fields)
		}
	}
}

func TestIgnoredVideoOptionsPointsReferencesAtTheirOwnEndpoint(t *testing.T) {
	// Reference images are implemented — at a different path, on a different
	// RPC. A bare "reference_images" would leave the caller to conclude the
	// capability does not exist.
	req := VideoGenerationRequest{Prompt: "x", ReferenceImages: []string{"media-1"}}

	got := ignoredVideoOptions(req)
	if len(got) != 1 {
		t.Fatalf("expected one ignored option, got %+v", got)
	}
	if got[0].Field != "reference_images" {
		t.Errorf("field = %q, want reference_images", got[0].Field)
	}
	if !strings.Contains(got[0].Hint, "/v1/videos/reference") {
		t.Errorf("the hint does not name the endpoint that implements it: %q", got[0].Hint)
	}
}

func TestIgnoredImageOptionsCountOnlyTripsAboveOne(t *testing.T) {
	cases := []struct {
		count int
		want  bool
	}{
		{0, false},
		{1, false},
		{2, true},
		{4, true},
	}
	for _, tc := range cases {
		got := ignoredImageOptions(ImageGenerationRequest{Prompt: "x", Count: tc.count})
		if named := contains(fieldNames(got), "count"); named != tc.want {
			t.Errorf("count=%d: count reported as ignored = %v, want %v", tc.count, named, tc.want)
		}
	}
}

func TestIgnoredImageOptionsHintsAtTheWorkaround(t *testing.T) {
	got := ignoredImageOptions(ImageGenerationRequest{Prompt: "x", Count: 4})
	if len(got) != 1 {
		t.Fatalf("expected one ignored option, got %+v", got)
	}
	if !strings.Contains(got[0].Hint, "submit twice") {
		t.Errorf("the hint does not say what to do instead: %q", got[0].Hint)
	}
}

func fieldNames(options []ignoredOption) []string {
	var out []string
	for _, option := range options {
		out = append(out, option.Field)
	}
	return out
}

/* ------------------------------------------------------------------ *
 * The response shape
 * ------------------------------------------------------------------ */

func TestGenerationResponseKeepsTheOutcomeAtTheTopLevel(t *testing.T) {
	// The whole reason for the embedded struct. Nesting the outcome under an
	// "outcome" key would break every existing caller, and the note is not worth
	// a breaking change.
	outcome := &engine.BatchVideoOutcome{
		JobID: "job-1", Status: "ready", MediaIDs: []string{"m1"},
		ProjectID: "proj-1", Credits: 1290,
	}

	data, err := json.Marshal(videoGenerationResponse{outcome, []ignoredOption{{Field: "aspect"}}})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	for key, want := range map[string]any{
		"job_id":            "job-1",
		"status":            "ready",
		"project_id":        "proj-1",
		"credits_remaining": float64(1290),
	} {
		if body[key] != want {
			t.Errorf("%s = %v, want %v — the outcome is not being promoted", key, body[key], want)
		}
	}
	if _, nested := body["outcome"]; nested {
		t.Error("the outcome was nested under an `outcome` key, which changes the response shape")
	}
	if _, ok := body["ignored"]; !ok {
		t.Error("`ignored` is missing from the response")
	}
}

func TestBatchGenerationResponseKeepsTheOutcomeAtTheTopLevel(t *testing.T) {
	// The image path returns the other outcome type, and it is the one the
	// `count` note rides on.
	outcome := &engine.BatchOutcome{JobID: "job-2", Status: "ready", Model: "NARWHAL"}

	data, err := json.Marshal(batchGenerationResponse{outcome, []ignoredOption{{Field: "count"}}})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if body["job_id"] != "job-2" || body["model"] != "NARWHAL" {
		t.Errorf("the outcome is not being promoted: %v", body)
	}
	if _, ok := body["ignored"]; !ok {
		t.Error("`ignored` is missing from the response")
	}
}

func TestGenerationResponseOmitsIgnoredWhenThereIsNothingToSay(t *testing.T) {
	outcome := &engine.BatchVideoOutcome{JobID: "job-1", Status: "ready"}

	data, err := json.Marshal(videoGenerationResponse{outcome, nil})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if _, ok := body["ignored"]; ok {
		t.Errorf("`ignored` is present with nothing to report: %s", data)
	}
}

func TestIgnoredOptionOmitsAnEmptyHint(t *testing.T) {
	data, err := json.Marshal(ignoredOption{Field: "seed"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if string(data) != `{"field":"seed"}` {
		t.Errorf("marshalled as %s, want no hint key", data)
	}
}
