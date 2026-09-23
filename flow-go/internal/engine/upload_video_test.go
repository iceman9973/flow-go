package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

/*
 * The video upload.
 *
 * Worth testing carefully rather than smoke-testing, for two reasons. Both steps
 * are POSTs told apart by path, and the session URL arrives in a *header* rather
 * than a body — so the composition, which URL step two is sent to, is a real
 * thing to get wrong. And the headers are a captured set: a missing one fails
 * loudly, but an extra one that the capture does not have is exactly the kind of
 * thing a later "belt and braces" edit reintroduces, so the absence of a bearer
 * is asserted too.
 *
 * `flowUploadBase` is the seam that makes this drivable without touching Google.
 * Authentication is cookies, so an engine with a jar needs nothing else — there
 * is no token to stub.
 */

const (
	testProjectID = "proj-1"
	sessionPath   = "/upload-session"
)

// uploadCalls records what each step of the handshake carried.
type uploadCalls struct {
	startHeaders http.Header
	startMethod  string
	startCookies string
	startPath    string
	startBody    string

	postHeaders       http.Header
	postMethod        string
	postCookies       string
	postPath          string
	postBody          []byte
	postContentLength int64
}

// uploadServer answers both steps of the handshake.
//
// They are told apart by path: step one goes to the project's upload endpoint,
// step two to whatever `x-goog-upload-url` the first reply named.
func uploadServer(t *testing.T, sessionStatus, resultStatus int,
	resultBody string) (*httptest.Server, *uploadCalls) {

	t.Helper()

	calls := &uploadCalls{}
	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, uploadVideoPathPrefix):
			calls.startHeaders = r.Header.Clone()
			calls.startMethod = r.Method
			calls.startCookies = r.Header.Get("Cookie")
			calls.startPath = r.URL.Path
			body, _ := io.ReadAll(r.Body)
			calls.startBody = string(body)

			// The session URL is a response header, not a body. Only set on a
			// success: a refused session should not hand one out.
			if sessionStatus < 300 {
				w.Header().Set("x-goog-upload-url", server.URL+sessionPath)
			}
			w.WriteHeader(sessionStatus)

		case r.URL.Path == sessionPath:
			calls.postHeaders = r.Header.Clone()
			calls.postMethod = r.Method
			calls.postCookies = r.Header.Get("Cookie")
			calls.postPath = r.URL.Path
			calls.postContentLength = r.ContentLength
			calls.postBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(resultStatus)
			fmt.Fprint(w, resultBody)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, calls
}

// pointUploadAt redirects the upload's first call at a test server.
func pointUploadAt(t *testing.T, server *httptest.Server) {
	t.Helper()
	original := flowUploadBase
	flowUploadBase = server.URL
	t.Cleanup(func() { flowUploadBase = original })
}

// uploadEngine is an engine that can run the whole handshake without a session.
//
// Cookies are the entire authentication, so a jar is all it needs — which is why
// there is no token seam on the Engine for these tests to set.
func uploadEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{
		ready:     true,
		projectID: testProjectID,
		// Cookies are matched by host, so the fixture has to name the test
		// server or nothing is attached and the auth assertions pass vacuously.
		primaryJar: cookiejar.FromCookies([]cookiejar.Cookie{
			{Name: "SAPISID", Value: "sapisid-value", Domain: "127.0.0.1", Path: "/"},
		}, "test"),
	}
}

// uploadStore is a database of its own, so the upload cache in one test cannot
// be a hit in another.
func uploadStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// uploadEngineWithStore is uploadEngine plus a database, which is what the cache
// needs. Without one the cache is inert — which is what the rest of these tests
// rely on.
func uploadEngineWithStore(t *testing.T, st *store.Store) *Engine {
	t.Helper()
	e := uploadEngine(t)
	e.store = st
	return e
}

// videoFixture writes a file of a known size and returns its path and length.
func videoFixture(t *testing.T, name, content string) (string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path, int64(len(content))
}

func TestUploadVideoRunsBothStepsAndReturnsTheMediaID(t *testing.T) {
	const content = "pretend this is a video"
	path, size := videoFixture(t, "clip.mp4", content)

	server, calls := uploadServer(t, 200, 200, `{"mediaId":"mock-video-media-123"}`)
	pointUploadAt(t, server)

	got, err := uploadEngine(t).UploadVideo(context.Background(), path, UploadOptions{})
	if err != nil {
		t.Fatalf("UploadVideo: %v", err)
	}

	if got.MediaID != "mock-video-media-123" {
		t.Errorf("media id = %q, want the id from step two", got.MediaID)
	}
	if got.Path != path {
		t.Errorf("path = %q, want the path that was uploaded (%q)", got.Path, path)
	}
	if got.Bytes != size {
		t.Errorf("bytes = %d, want %d", got.Bytes, size)
	}
	if got.ProjectID != testProjectID {
		t.Errorf("project id = %q, want %q", got.ProjectID, testProjectID)
	}

	// The project is in the *path*, not a header — the part of this endpoint that
	// is easiest to get wrong by analogy with the one it replaced.
	if calls.startPath != uploadVideoPathPrefix+testProjectID {
		t.Errorf("step one went to %q, want %q", calls.startPath,
			uploadVideoPathPrefix+testProjectID)
	}

	// Both steps are POSTs. Asserted rather than left implicit because the
	// resumable-upload protocol this borrows from uses PUT for the data step, so
	// "PUT" is the version a reader would write from memory — and a mock that
	// matched on path alone would not notice.
	if calls.startMethod != http.MethodPost {
		t.Errorf("step one was a %s, want POST", calls.startMethod)
	}
	if calls.postMethod != http.MethodPost {
		t.Errorf("step two was a %s, want POST", calls.postMethod)
	}

	// The captured header set. The length and the type are declared before any of
	// the file is sent, which is what makes them a contract rather than a hint.
	wantHeaders := map[string]string{
		"X-Goog-Upload-Protocol":              "resumable",
		"X-Goog-Upload-Command":               "start",
		"X-Goog-Upload-Header-Content-Length": strconv.FormatInt(size, 10),
		"X-Goog-Upload-Header-Content-Type":   "video/mp4",
		"X-Goog-Upload-File-Name":             "clip.mp4",
		"Origin":                              flowUploadBase,
		"Referer":                             flowUploadBase + "/",
	}
	for name, want := range wantHeaders {
		if got := calls.startHeaders.Get(name); got != want {
			t.Errorf("step one %s = %q, want %q", name, got, want)
		}
	}

	// Step two goes to the URL step one named, as a POST. This is the composition
	// the two halves cannot check on their own.
	if calls.postPath != sessionPath {
		t.Errorf("the upload went to %q, want the session URL step one returned", calls.postPath)
	}
	if v := calls.postHeaders.Get("X-Goog-Upload-Command"); v != "upload, finalize" {
		t.Errorf("X-Goog-Upload-Command = %q", v)
	}
	if v := calls.postHeaders.Get("X-Goog-Upload-Offset"); v != "0" {
		t.Errorf("X-Goog-Upload-Offset = %q", v)
	}
	if string(calls.postBody) != content {
		t.Errorf("the uploaded body was %q, want the file's contents", calls.postBody)
	}

	// The length must be declared, not chunked. Go only infers a length from
	// three concrete reader types and an *os.File is not one of them, so without
	// the explicit ContentLength this arrives as -1.
	if calls.postContentLength != size {
		t.Errorf("the upload arrived with Content-Length %d, want %d — a chunked body is what "+
			"a resumable session refuses", calls.postContentLength, size)
	}
}

// Authentication is cookies alone, and the absence of a bearer is asserted as
// well as the presence of the cookie.
//
// The capture shows neither call carrying an Authorization header, which is
// consistent with everything else in this engine that talks to flow.google.com.
// Asserting only the cookie would let a later "belt and braces" edit add a header
// the verified protocol does not have, and nobody would notice until a live run.
func TestUploadVideoAuthenticatesWithCookiesAlone(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "data")

	server, calls := uploadServer(t, 200, 200, `{"mediaId":"m"}`)
	pointUploadAt(t, server)

	if _, err := uploadEngine(t).UploadVideo(context.Background(), path, UploadOptions{}); err != nil {
		t.Fatalf("UploadVideo: %v", err)
	}

	if !strings.Contains(calls.startCookies, "SAPISID=sapisid-value") {
		t.Errorf("the session call carried no account cookie: %q", calls.startCookies)
	}
	if auth := calls.startHeaders.Get("Authorization"); auth != "" {
		t.Errorf("the session call carried an Authorization header, which the captured "+
			"protocol does not: %q", auth)
	}
}

func TestUploadVideoRefusesARefusedSession(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest,
		http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			path, _ := videoFixture(t, "clip.mp4", "data")

			server, calls := uploadServer(t, status, 200, `{}`)
			pointUploadAt(t, server)

			_, err := uploadEngine(t).UploadVideo(context.Background(), path, UploadOptions{})
			if err == nil {
				t.Fatalf("HTTP %d should fail the upload", status)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(status)) {
				t.Errorf("the error should carry the status: %v", err)
			}
			if calls.postPath != "" {
				t.Error("step two must not run when step one was refused")
			}
		})
	}
}

func TestUploadVideoReportsARefusedUpload(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "data")

	server, _ := uploadServer(t, 200, http.StatusForbidden, `{"error":"nope"}`)
	pointUploadAt(t, server)

	_, err := uploadEngine(t).UploadVideo(context.Background(), path, UploadOptions{})
	if err == nil {
		t.Fatal("a refused upload should fail")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("the error should carry the status: %v", err)
	}
}

// An upload that succeeded but answered with nothing addressable must not be
// reported as success — and it must say the upload itself probably worked, which
// is worth knowing before re-sending several hundred megabytes.
func TestUploadVideoReportsAnUnreadableResult(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "data")

	server, _ := uploadServer(t, 200, 200, ``)
	pointUploadAt(t, server)

	_, err := uploadEngine(t).UploadVideo(context.Background(), path, UploadOptions{})
	if err == nil {
		t.Fatal("an empty result should fail the upload")
	}
	if !strings.Contains(err.Error(), "no media id") {
		t.Errorf("the error should name what was missing: %v", err)
	}
	if !strings.Contains(err.Error(), "may have succeeded") {
		t.Errorf("the error should say the upload itself may have worked: %v", err)
	}
}

func TestUploadVideoReportsASessionReplyWithNoURL(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "data")

	// A 200 with no session header: the endpoint answered and said nothing
	// usable, which is the failure a shape change produces.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	pointUploadAt(t, server)

	_, err := uploadEngine(t).UploadVideo(context.Background(), path, UploadOptions{})
	if err == nil {
		t.Fatal("a session reply with no URL should fail")
	}
	if !strings.Contains(err.Error(), "no session URL") {
		t.Errorf("the error should name what was missing: %v", err)
	}
}

/* ------------------------------------------------------------------ *
 * The reply shapes
 * ------------------------------------------------------------------ */

func TestSessionURLIsFoundUnderEveryHeaderName(t *testing.T) {
	const url = "https://example.test/session"

	for _, name := range []string{"x-goog-upload-url", "Location", "x-goog-upload-control-url"} {
		t.Run(name, func(t *testing.T) {
			// Built with Set, not as a literal map, and that matters: Header.Get
			// canonicalises the name it looks up, and a real response is
			// canonicalised on the way in. A literal {"x-goog-upload-url": ...}
			// is a shape no response ever has, and looking one up finds nothing —
			// which is a failing test that says the code is wrong when the
			// fixture is.
			header := http.Header{}
			header.Set(name, url)

			if got := sessionURLFrom(header); got != url {
				t.Errorf("sessionURLFrom = %q, want %q", got, url)
			}
		})
	}

	if got := sessionURLFrom(http.Header{}); got != "" {
		t.Errorf("sessionURLFrom with no session header = %q, want empty", got)
	}
}

func TestMediaIDIsFoundInTheCapturedShape(t *testing.T) {
	const id = "7f67de20-cf93-4724-9977-e331070c1c05"

	// The captured reply, then the two documented fallbacks.
	captured := `{"mediaId":"7f67de20-cf93-4724-9977-e331070c1c05",` +
		`"media":{"name":"7f67de20-cf93-4724-9977-e331070c1c05","projectId":"proj-1"},` +
		`"workflow":{"id":"wf-1"}}`
	for _, body := range []string{
		captured,
		`{"mediaId":"7f67de20-cf93-4724-9977-e331070c1c05"}`,
		`{"media":{"name":"7f67de20-cf93-4724-9977-e331070c1c05"}}`,
		`{"media":{"mediaId":"7f67de20-cf93-4724-9977-e331070c1c05"}}`,
	} {
		got, err := mediaIDFrom([]byte(body))
		if err != nil {
			t.Errorf("mediaIDFrom(%s): %v", body, err)
			continue
		}
		if got != id {
			t.Errorf("mediaIDFrom(%s) = %q, want %q", body, got, id)
		}
	}

	// A reply that is not JSON at all is a different failure from one that parses
	// and carries nothing, and the message has to say which.
	if _, err := mediaIDFrom([]byte("<html>gateway</html>")); err == nil ||
		!strings.Contains(err.Error(), "not JSON") {
		t.Errorf("a non-JSON reply should say so: %v", err)
	}
}

// The narrowed fallback set is deliberate, and this is the case that shows why.
//
// The captured reply carries a `workflow` object beside `media`, so a loose
// top-level `name` fallback would be willing to pick an id out of whichever
// object happened to carry one. An upload that succeeded and reported the wrong
// id is worse than one that says it could not read the answer.
func TestMediaIDIsNotTakenFromAnUnrelatedName(t *testing.T) {
	_, err := mediaIDFrom([]byte(`{"name":"not-a-media-id","workflow":{"name":"also-not"}}`))
	if err == nil {
		t.Fatal("a reply with no media id should not be read as one")
	}
	if !strings.Contains(err.Error(), "no media id") {
		t.Errorf("want the missing id reported: %v", err)
	}
}

/* ------------------------------------------------------------------ *
 * Validation, which happens before anything is sent
 * ------------------------------------------------------------------ */

// Each of these is refused before a socket is opened, so they run against an
// engine with no jar and no client — if any of them reached the network the
// failure would be about the connection rather than about the file.
func TestUploadVideoRefusesBadInputBeforeSending(t *testing.T) {
	dir := t.TempDir()
	empty, _ := videoFixture(t, "empty.mp4", "")
	text, _ := videoFixture(t, "notes.txt", "hello")

	cases := []struct {
		name string
		path string
		want string
	}{
		{"missing file", filepath.Join(dir, "nope.mp4"), "read"},
		{"a directory", dir, "is a directory"},
		{"an empty file", empty, "is empty"},
		{"not a video", text, "does not look like a video"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{ready: true, projectID: testProjectID}
			_, err := e.UploadVideo(context.Background(), tc.path, UploadOptions{})
			if err == nil {
				t.Fatal("this should have been refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestUploadVideoNeedsAProject(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "data")

	e := &Engine{ready: true}
	if _, err := e.UploadVideo(context.Background(), path, UploadOptions{}); err == nil ||
		!strings.Contains(err.Error(), "no project id") {
		t.Errorf("an unresolved project should be refused: %v", err)
	}
}

// The size guard is a guard against pointing the command at the wrong file, and
// it has to fire before anything is sent.
func TestUploadVideoRefusesAnOversizedFile(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "more than nothing at all")

	original := config.MaxUploadMB
	config.MaxUploadMB = 0 // any non-empty file is now over the limit
	t.Cleanup(func() { config.MaxUploadMB = original })

	e := &Engine{ready: true, projectID: testProjectID}

	_, err := e.UploadVideo(context.Background(), path, UploadOptions{})
	if err == nil {
		t.Fatal("a file over the limit should be refused")
	}
	if !strings.Contains(err.Error(), "MAX_UPLOAD_MB") {
		t.Errorf("the error should name the knob that would allow it: %v", err)
	}
}

func TestVideoMIMEForCoversTheExtensionsFlowTakes(t *testing.T) {
	cases := map[string]string{
		"clip.mp4":     "video/mp4",
		"CLIP.MP4":     "video/mp4",
		"clip.m4v":     "video/mp4",
		"clip.mov":     "video/quicktime",
		"clip.webm":    "video/webm",
		"clip.mkv":     "video/x-matroska",
		"notes.txt":    "",
		"photo.png":    "",
		"no-extension": "",
		"clip.mp4.bak": "",
	}
	for name, want := range cases {
		if got := videoMIMEFor(name); got != want {
			t.Errorf("videoMIMEFor(%q) = %q, want %q", name, got, want)
		}
	}
}

/* ------------------------------------------------------------------ *
 * The upload cache
 *
 * Editing one video repeatedly is the normal way to use it — the same file goes
 * up once and is then edited with different prompts — so the second upload of the
 * same bytes must not happen at all.
 * ------------------------------------------------------------------ */

// The strongest form of "no network call": the server is closed, so a call that
// tried to happen would fail rather than merely go uncounted.
func TestUploadVideoReusesARecordedUpload(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "the same bytes as before")

	server, _ := uploadServer(t, 200, 200, `{"mediaId":"first-media-id"}`)
	pointUploadAt(t, server)

	e := uploadEngineWithStore(t, uploadStore(t))

	first, err := e.UploadVideo(context.Background(), path, UploadOptions{})
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if first.Cached {
		t.Error("the first upload cannot be a cache hit — nothing has been recorded yet")
	}
	if first.MediaID != "first-media-id" {
		t.Fatalf("first media id = %q", first.MediaID)
	}

	// Now make the network unusable. A second call that reaches it fails.
	server.Close()

	second, err := e.UploadVideo(context.Background(), path, UploadOptions{})
	if err != nil {
		t.Fatalf("the second upload reached the network, which is the thing the cache "+
			"exists to prevent: %v", err)
	}
	if !second.Cached {
		t.Error("the second upload should be reported as cached")
	}
	if second.MediaID != first.MediaID {
		t.Errorf("media id = %q, want the recorded %q", second.MediaID, first.MediaID)
	}
	if second.Bytes != first.Bytes {
		t.Errorf("bytes = %d, want %d", second.Bytes, first.Bytes)
	}
}

func TestForceUploadSkipsTheCache(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "the same bytes")

	server, calls := uploadServer(t, 200, 200, `{"mediaId":"media-1"}`)
	pointUploadAt(t, server)

	e := uploadEngineWithStore(t, uploadStore(t))

	if _, err := e.UploadVideo(context.Background(), path, UploadOptions{}); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if calls.startPath == "" {
		t.Fatal("the fixture never recorded a session call")
	}

	// Cleared, so its being set again is proof the network was used.
	calls.startPath = ""

	forced, err := e.UploadVideo(context.Background(), path, UploadOptions{Force: true})
	if err != nil {
		t.Fatalf("forced upload: %v", err)
	}
	if forced.Cached {
		t.Error("a forced upload must not be reported as cached")
	}
	if calls.startPath != uploadVideoPathPrefix+testProjectID {
		t.Error("--force-upload should have gone to the network again")
	}
}

// A media id is only addressable inside the project it was created in, so a hit
// in another project would be worse than a miss.
func TestTheUploadCacheIsScopedToTheProject(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "the same bytes")

	server, calls := uploadServer(t, 200, 200, `{"mediaId":"media-1"}`)
	pointUploadAt(t, server)

	e := uploadEngineWithStore(t, uploadStore(t))

	if _, err := e.UploadVideo(context.Background(), path, UploadOptions{}); err != nil {
		t.Fatalf("first upload: %v", err)
	}

	e.projectID = "another-project"
	calls.startPath = ""

	other, err := e.UploadVideo(context.Background(), path, UploadOptions{})
	if err != nil {
		t.Fatalf("upload into the second project: %v", err)
	}
	if other.Cached {
		t.Error("an upload recorded for one project must not satisfy another")
	}
	if calls.startPath != uploadVideoPathPrefix+"another-project" {
		t.Errorf("the second upload went to %q, want the second project's endpoint",
			calls.startPath)
	}
}

// The hash identifies the upload, not the path: editing the file in place and
// uploading again must send the new bytes rather than reuse the old media id.
func TestTheUploadCacheMissesWhenTheFileChanges(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "the first cut")

	server, calls := uploadServer(t, 200, 200, `{"mediaId":"media-1"}`)
	pointUploadAt(t, server)

	e := uploadEngineWithStore(t, uploadStore(t))

	if _, err := e.UploadVideo(context.Background(), path, UploadOptions{}); err != nil {
		t.Fatalf("first upload: %v", err)
	}

	if err := os.WriteFile(path, []byte("the second cut, different bytes"), 0o644); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}
	calls.startPath = ""

	changed, err := e.UploadVideo(context.Background(), path, UploadOptions{})
	if err != nil {
		t.Fatalf("upload after the file changed: %v", err)
	}
	if changed.Cached {
		t.Error("a changed file must not reuse the previous upload's media id")
	}
	if calls.startPath == "" {
		t.Error("a changed file should have gone to the network")
	}
}

// A store that cannot be read is not a reason to refuse the upload: the cache is
// an optimisation, and treating a failure to read it as a failure to upload would
// turn a cold database into a broken feature.
func TestAnUnreadableCacheStillUploads(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "bytes")

	server, calls := uploadServer(t, 200, 200, `{"mediaId":"media-1"}`)
	pointUploadAt(t, server)

	e := uploadEngine(t)
	// A closed store: every query on it fails.
	st := uploadStore(t)
	_ = st.Close()
	e.store = st

	result, err := e.UploadVideo(context.Background(), path, UploadOptions{})
	if err != nil {
		t.Fatalf("an unreadable cache should not fail the upload: %v", err)
	}
	if result.MediaID != "media-1" {
		t.Errorf("media id = %q, want the uploaded one", result.MediaID)
	}
	if calls.startPath == "" {
		t.Error("the upload should still have gone to the network")
	}
}

/* ------------------------------------------------------------------ *
 * How step two can fail
 * ------------------------------------------------------------------ */

// A timeout has to name the knob, because the transport's bound is fixed when
// the client is built and the error would otherwise read as a network fault.
type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }

func TestUploadTimeoutHintNamesTheKnob(t *testing.T) {
	if got := uploadTimeoutHint(timeoutError{}); !strings.Contains(got, "UPLOAD_TIMEOUT") {
		t.Errorf("a timeout should name UPLOAD_TIMEOUT: %q", got)
	}
	if got := uploadTimeoutHint(context.DeadlineExceeded); !strings.Contains(got, "UPLOAD_TIMEOUT") {
		t.Errorf("a deadline should name UPLOAD_TIMEOUT: %q", got)
	}
	if got := uploadTimeoutHint(fmt.Errorf("connection reset")); got != "" {
		t.Errorf("an ordinary error should not mention a timeout: %q", got)
	}
}

// A hanging step two must fail, and the error must name the knob that bounds it.
func TestUploadVideoReportsAHangingUpload(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "data")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, uploadVideoPathPrefix) {
			w.Header().Set("x-goog-upload-url", "http://"+r.Host+sessionPath)
			w.WriteHeader(http.StatusOK)
			return
		}
		// Never answers — but not forever either. Waiting on the request context
		// alone is not enough: a client that gives up on a request does not
		// necessarily close the connection, so this handler can outlive the test
		// and hang the server's shutdown, which the suite then reports as a
		// timeout with no failing test in it.
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	t.Cleanup(server.Close)
	pointUploadAt(t, server)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	_, err := uploadEngine(t).UploadVideo(ctx, path, UploadOptions{})
	if err == nil {
		t.Fatal("a hanging upload should fail")
	}
	if !strings.Contains(err.Error(), "UPLOAD_TIMEOUT") {
		t.Errorf("the error should name the knob that bounds it: %v", err)
	}
}

// A connection that dies mid-upload has no status to report, so the transport's
// own error is the whole of the diagnosis.
func TestUploadVideoReportsABrokenStream(t *testing.T) {
	path, _ := videoFixture(t, "clip.mp4", "data")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, uploadVideoPathPrefix) {
			w.Header().Set("x-goog-upload-url", "http://"+r.Host+sessionPath)
			w.WriteHeader(http.StatusOK)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server cannot hijack, so this proves nothing")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close() // the far end goes away without answering
	}))
	t.Cleanup(server.Close)
	pointUploadAt(t, server)

	_, err := uploadEngine(t).UploadVideo(context.Background(), path, UploadOptions{})
	if err == nil {
		t.Fatal("a broken stream should fail the upload")
	}
	if !strings.Contains(err.Error(), "the upload failed") {
		t.Errorf("want the transport failure reported: %v", err)
	}
}
