package server

import (
	"encoding/json"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

/* ------------------------------------------------------------------ *
 * Fixtures
 * ------------------------------------------------------------------ */

// seedVideoJob records a completed video generation — the shape that carries every
// field a job can have.
func seedVideoJob(t *testing.T, st *store.Store) string {
	t.Helper()

	credits := 4
	var elapsed int64 = 37500
	if _, err := st.RecordGeneration(store.Generation{
		JobID:        "job-video-1",
		AccountID:    "acct-0c960ba36880",
		Kind:         "video",
		Prompt:       "a paper boat on a river",
		Model:        "veo-3.0-generate-preview",
		Duration:     8,
		Count:        1,
		MediaIDs:     []string{"media-v1"},
		Status:       "ready",
		CreditsSpent: &credits,
		ElapsedMS:    &elapsed,
	}); err != nil {
		t.Fatalf("recording the video job: %v", err)
	}
	return "job-video-1"
}

// seedImageJob records the shape almost every row in a real database has: an image with
// no duration, no credits recorded and no aspect. It is the row that proves the empty
// fields survive rather than being dropped.
func seedImageJob(t *testing.T, st *store.Store) string {
	t.Helper()

	if _, err := st.RecordGeneration(store.Generation{
		JobID:    "job-image-1",
		Kind:     "image",
		Prompt:   "a red boat",
		Model:    "NARWHAL",
		Status:   "succeeded",
		MediaIDs: []string{},
	}); err != nil {
		t.Fatalf("recording the image job: %v", err)
	}
	return "job-image-1"
}

func seedMedia(t *testing.T, st *store.Store, generationID *int64) string {
	t.Helper()

	if _, err := st.RecordMedia(store.Media{
		GenerationID: generationID,
		MediaID:      "media-v1",
		Kind:         "video",
		Prompt:       "a paper boat on a river",
		FileName:     "media-v1.mp4",
		FilePath:     "/output/media-v1.mp4",
		URL:          "https://example.invalid/media-v1.mp4",
		// Resolution is left empty on purpose: every row in the live database has
		// it empty, and it must still appear.
		Bytes: 1048576,
	}); err != nil {
		t.Fatalf("recording the media: %v", err)
	}
	return "media-v1"
}

// findRow pulls the object with the given key out of a JSON array of objects.
func findRow(t *testing.T, list any, key, want string) map[string]any {
	t.Helper()

	rows, ok := list.([]any)
	if !ok {
		t.Fatalf("the list is %T, want an array", list)
	}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("a row is %T, want an object", raw)
		}
		if row[key] == want {
			return row
		}
	}
	t.Fatalf("no row with %s = %q in %v", key, want, list)
	return nil
}

/* ------------------------------------------------------------------ *
 * The structs are fully tagged
 * ------------------------------------------------------------------ */

func TestGenerationIsFullyTagged(t *testing.T) {
	// The complete guarantee, and the one the endpoints cannot give on their own: a
	// fully populated struct must produce *no* Go field name at all. An endpoint test
	// only exercises the fields its seed happened to fill.
	credits := 4
	var elapsed int64 = 37500
	data, err := json.Marshal(store.Generation{
		JobID: "job-1", AccountID: "acct-1", Kind: "video", Prompt: "p", Model: "m",
		Duration: 8, Aspect: "landscape", Count: 2, MediaIDs: []string{"m1"},
		Status: "ready", CreditsSpent: &credits, ElapsedMS: &elapsed, Error: "boom",
	})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	if leaked := pascalCaseKeys(keysOf(row)); len(leaked) != 0 {
		t.Errorf("store.Generation leaked Go field names: %v", leaked)
	}
	for _, key := range []string{
		"job_id", "account_id", "kind", "prompt", "model", "duration", "aspect",
		"count", "media_ids", "status", "credits_spent", "elapsed_ms", "error",
	} {
		if _, present := row[key]; !present {
			t.Errorf("%s is absent from %s", key, data)
		}
	}
}

func TestMediaIsFullyTagged(t *testing.T) {
	genID := int64(7)
	data, err := json.Marshal(store.Media{
		GenerationID: &genID, MediaID: "m1", Kind: "video", Prompt: "p",
		FileName: "m1.mp4", FilePath: "/output/m1.mp4", URL: "https://x.invalid/m1.mp4",
		Resolution: "1080p", Bytes: 1024,
	})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	if leaked := pascalCaseKeys(keysOf(row)); len(leaked) != 0 {
		t.Errorf("store.Media leaked Go field names: %v", leaked)
	}
	for _, key := range []string{
		"generation_id", "media_id", "kind", "prompt", "file_name", "file_path",
		"url", "resolution", "bytes",
	} {
		if _, present := row[key]; !present {
			t.Errorf("%s is absent from %s", key, data)
		}
	}
}

/* ------------------------------------------------------------------ *
 * The endpoints
 * ------------------------------------------------------------------ */

func TestJobsEndpointEmitsSnakeCase(t *testing.T) {
	app, st := testAppWithStore(t)
	seedVideoJob(t, st)

	row := findRow(t, getJSON(t, app, "/v1/jobs")["jobs"], "job_id", "job-video-1")

	if leaked := pascalCaseKeys(keysOf(row)); len(leaked) != 0 {
		t.Errorf("the endpoint leaked Go field names: %v", leaked)
	}
	for key, want := range map[string]any{
		"job_id":        "job-video-1",
		"account_id":    "acct-0c960ba36880",
		"kind":          "video",
		"prompt":        "a paper boat on a river",
		"model":         "veo-3.0-generate-preview",
		"duration":      float64(8),
		"count":         float64(1),
		"status":        "ready",
		"credits_spent": float64(4),
		"elapsed_ms":    float64(37500),
	} {
		if row[key] != want {
			t.Errorf("%s = %v, want %v", key, row[key], want)
		}
	}
	if ids, ok := row["media_ids"].([]any); !ok || len(ids) != 1 || ids[0] != "media-v1" {
		t.Errorf("media_ids = %v, want [media-v1]", row["media_ids"])
	}
}

func TestJobByIDEmitsTheSameKeysAsTheList(t *testing.T) {
	// The two handlers marshal the same struct by different routes — the list wraps it
	// in a fiber.Map, the single-job handler hands it to c.JSON directly. A divergence
	// between them is the kind of thing only a comparison catches.
	app, st := testAppWithStore(t)
	id := seedVideoJob(t, st)

	listed := findRow(t, getJSON(t, app, "/v1/jobs")["jobs"], "job_id", id)
	single := getJSON(t, app, "/v1/jobs/"+id)

	listedKeys, singleKeys := keysOf(listed), keysOf(single)
	if len(listedKeys) != len(singleKeys) {
		t.Fatalf("the list row has %d keys, the single job has %d: %v vs %v",
			len(listedKeys), len(singleKeys), listedKeys, singleKeys)
	}
	for _, key := range listedKeys {
		if _, present := single[key]; !present {
			t.Errorf("the single-job response is missing %q", key)
		}
	}
}

func TestMediaEndpointEmitsSnakeCase(t *testing.T) {
	app, st := testAppWithStore(t)
	seedMedia(t, st, nil)

	row := findRow(t, getJSON(t, app, "/v1/media")["media"], "media_id", "media-v1")

	if leaked := pascalCaseKeys(keysOf(row)); len(leaked) != 0 {
		t.Errorf("the endpoint leaked Go field names: %v", leaked)
	}
	for key, want := range map[string]any{
		"media_id":   "media-v1",
		"kind":       "video",
		"prompt":     "a paper boat on a river",
		"file_name":  "media-v1.mp4",
		"file_path":  "/output/media-v1.mp4",
		"url":        "https://example.invalid/media-v1.mp4",
		"resolution": "",
		"bytes":      float64(1048576),
	} {
		if row[key] != want {
			t.Errorf("%s = %v, want %v", key, row[key], want)
		}
	}
}

/* ------------------------------------------------------------------ *
 * What the empty values mean
 * ------------------------------------------------------------------ */

func TestGenerationOmitsOnlyTheErrorNote(t *testing.T) {
	// `error` is the one field whose empty value is the absence of a note rather than a
	// value, so it is the one field that is dropped. A job with no error should not
	// carry `"error": ""`.
	blank, err := json.Marshal(store.Generation{JobID: "job-1"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var noError map[string]any
	if err := json.Unmarshal(blank, &noError); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if _, present := noError["error"]; present {
		t.Errorf("error is present on a job that has none: %s", blank)
	}

	withError, err := json.Marshal(store.Generation{JobID: "job-1", Error: "upstream 500"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var failed map[string]any
	if err := json.Unmarshal(withError, &failed); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if failed["error"] != "upstream 500" {
		t.Errorf("error = %v, want the message", failed["error"])
	}
}

func TestGenerationKeepsNullDistinctFromZero(t *testing.T) {
	// credits_spent and elapsed_ms are pointers so that a NULL in the database — which is
	// what every image row holds — reads as `null`, not as `0`. A zero is a real value:
	// a video that cost nothing, a call that took no measurable time.
	zeroCredits, zeroElapsed := 0, int64(0)
	cases := []struct {
		name        string
		credits     *int
		elapsed     *int64
		wantCredits any
		wantElapsed any
	}{
		{"unrecorded", nil, nil, nil, nil},
		{"recorded as zero", &zeroCredits, &zeroElapsed, float64(0), float64(0)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(store.Generation{
				JobID: "job-1", CreditsSpent: tc.credits, ElapsedMS: tc.elapsed,
			})
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}
			var row map[string]any
			if err := json.Unmarshal(data, &row); err != nil {
				t.Fatalf("unmarshalling: %v", err)
			}

			for key, want := range map[string]any{
				"credits_spent": tc.wantCredits,
				"elapsed_ms":    tc.wantElapsed,
			} {
				got, present := row[key]
				if !present {
					t.Errorf("%s is absent; a nil must be sent as null, not omitted", key)
					continue
				}
				if got != want {
					t.Errorf("%s = %v, want %v", key, got, want)
				}
			}
		})
	}
}

func TestGenerationKeepsFieldsThatAreEmptyOnEveryRow(t *testing.T) {
	// `aspect` is empty on all 18 rows of the live database and `duration` is NULL on
	// every image. Both still ship, because dropping them would make "not recorded"
	// indistinguishable from "this server predates the field".
	data, err := json.Marshal(store.Generation{JobID: "job-1", Kind: "image"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	for key, want := range map[string]any{
		"aspect":     "",
		"duration":   float64(0),
		"account_id": "",
		"model":      "",
	} {
		got, present := row[key]
		if !present {
			t.Errorf("%s was dropped; its emptiness is a fact about the row", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestGenerationMediaIDsIsAnArrayNotANull(t *testing.T) {
	// A job with no media is the common case for a failure. The store reads `'[]'` back
	// into a non-nil empty slice, so the wire form is `[]` — not `null`, which a client
	// would have to special-case before iterating.
	data, err := json.Marshal(store.Generation{JobID: "job-1", MediaIDs: []string{}})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	ids, ok := row["media_ids"].([]any)
	if !ok {
		t.Fatalf("media_ids = %v (%T), want an array", row["media_ids"], row["media_ids"])
	}
	if len(ids) != 0 {
		t.Errorf("media_ids = %v, want empty", ids)
	}
}

func TestMediaKeepsEveryFieldIncludingEmptyOnes(t *testing.T) {
	// No field on Media takes omitempty. `resolution` is empty on every row in the live
	// database and still appears, so a client can read it rather than infer its absence.
	data, err := json.Marshal(store.Media{MediaID: "m1", Kind: "image"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	for key, want := range map[string]any{
		"resolution": "",
		"file_name":  "",
		"file_path":  "",
		"url":        "",
		"prompt":     "",
		"bytes":      float64(0),
	} {
		got, present := row[key]
		if !present {
			t.Errorf("%s was dropped; its emptiness is a fact about the asset", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestMediaKeepsGenerationIDNullWhenUnlinked(t *testing.T) {
	// An asset not yet linked to a job must read as null, not as 0 — 0 is a real row ID.
	data, err := json.Marshal(store.Media{MediaID: "m1", Kind: "image"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	value, present := row["generation_id"]
	if !present {
		t.Fatal("generation_id is absent; a nil must be sent as null")
	}
	if value != nil {
		t.Errorf("generation_id = %v, want null", value)
	}
}

/* ------------------------------------------------------------------ *
 * The export file
 * ------------------------------------------------------------------ */

func TestExportPayloadUsesTheSameJobAndMediaKeysAsTheEndpoints(t *testing.T) {
	// `flow-go export` marshals the same two structs into export.json, so the file and
	// the API can diverge. Both go through store.Generation and store.Media, which is
	// the point of tagging them there rather than building response DTOs in the
	// handlers — this pins that they stay in step.
	app, st := testAppWithStore(t)
	genID, err := st.RecordGeneration(store.Generation{
		JobID: "job-video-1", Kind: "video", Prompt: "p", Status: "ready",
	})
	if err != nil {
		t.Fatalf("recording the job: %v", err)
	}
	seedMedia(t, st, &genID)

	jobEndpointRow := findRow(t, getJSON(t, app, "/v1/jobs")["jobs"], "job_id", "job-video-1")
	mediaEndpointRow := findRow(t, getJSON(t, app, "/v1/media")["media"], "media_id", "media-v1")

	exportedJob := marshalToMap(t, store.Generation{
		JobID: "job-video-1", Kind: "video", Prompt: "p", Status: "ready",
	})
	exportedMedia := marshalToMap(t, store.Media{
		GenerationID: &genID, MediaID: "media-v1", Kind: "video", Prompt: "p",
		FileName: "media-v1.mp4", FilePath: "/output/media-v1.mp4",
		URL: "https://example.invalid/media-v1.mp4", Bytes: 1048576,
	})

	for key := range exportedJob {
		if _, present := jobEndpointRow[key]; !present {
			t.Errorf("export has job key %q but the endpoint does not", key)
		}
	}
	for key := range exportedMedia {
		if _, present := mediaEndpointRow[key]; !present {
			t.Errorf("export has media key %q but the endpoint does not", key)
		}
	}
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	return out
}
