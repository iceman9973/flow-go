package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	stdhttp "net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/config"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/httpx"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

/*
 * Video upload.
 *
 * Flow takes a video by a route that is neither batchexecute nor the media RPC:
 * a two-step resumable upload through the app's own host. The first call opens a
 * session and answers with the URL to stream at, in a response header; the
 * second posts the file there and answers with the media id.
 *
 * The image path has no equivalent — `maseQ` carries the bytes inside the
 * batchexecute payload — which is why this is a second transport rather than
 * another RPC id. It is also why an edit of a local file could not work before
 * this: the edit RPC takes a media id of something already in the project, and
 * nothing here could put a video there.
 *
 * **The wire shape is from a live capture of the app's own upload**, which is a
 * different standing from the first version of this file: that one reproduced
 * the Python engine this port replaces, against a Labs endpoint the app has
 * since retired, and was never exercised. What is verified is the *shape* — host,
 * path, headers, the two replies. What is not is this implementation of it: the
 * capture was taken in a browser, and nothing here has yet run it against a real
 * account. So a failure is still worth reading as "the shape has moved" before
 * assuming the video is at fault, and every failure path reports what it could
 * not read rather than a bare symptom.
 *
 * Authentication is cookies alone. The capture shows none of the calls carrying a
 * bearer, which is consistent with the rest of this engine: everything that talks
 * to flow.google.com authenticates with the account's cookies.
 */

// flowUploadBase is the host the upload starts against.
//
// A var rather than a const so a test can point it at a local server. Without
// that, exercising the two-step handshake means talking to Google — which is
// neither hermetic nor free — and both replies are exactly what a unit test
// should be pinning.
var flowUploadBase = "https://flow.google.com"

// uploadVideoPathPrefix opens a resumable upload session for one project. The
// project is in the *path*, not a header, which is the part of this endpoint
// that is easiest to get wrong by analogy with the old one.
const uploadVideoPathPrefix = "/upload/v1/flow/upload/video/"

// UploadOptions tunes one upload.
type UploadOptions struct {
	// Force uploads again even when this exact file is already in the project.
	//
	// It is the escape from the one thing the cache cannot know: a media id it
	// recorded may no longer resolve — the project may have been emptied, or the
	// asset removed — and a hit is never re-checked, because checking would cost
	// the round trip the cache exists to save.
	Force bool
}

// VideoUploadResult is what a completed video upload produces.
type VideoUploadResult struct {
	// MediaID addresses the uploaded video inside the project. It is what the
	// edit RPC takes.
	MediaID string `json:"media_id"`
	// Path is the local file, as it was given. Recorded because a media id is
	// opaque: without it, an operator comparing two uploads — or working out
	// which file a media id came from — has nothing to go on.
	Path string `json:"path"`
	// Bytes is how much was sent.
	Bytes int64 `json:"bytes"`
	// ProjectID is the project the video landed in.
	ProjectID string `json:"project_id"`
	// Cached reports that this came from the upload cache rather than the
	// network, so a caller — and an operator reading the JSON — can tell that
	// nothing was sent. Without it a cached result and a fresh one are the same
	// document, which is the sort of thing that makes a stale id hard to spot.
	Cached bool `json:"cached,omitempty"`
}

// UploadVideo puts a local video into the account's Flow project.
//
// It returns the media id the edit RPC needs, so a caller with a local file does
// not have to upload it by hand first.
func (e *Engine) UploadVideo(ctx context.Context, filePath string, opts UploadOptions) (*VideoUploadResult, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine: not ready — call Bootstrap first")
	}
	projectID := e.ProjectID()
	if projectID == "" {
		return nil, fmt.Errorf("engine: no project id resolved")
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("engine: read %s: %w", filePath, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("engine: %s is a directory, not a video", filePath)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("engine: %s is empty", filePath)
	}

	// A guard against pointing the command at the wrong file, not a limit Flow is
	// known to enforce. Checked before anything is sent, so a mistake costs
	// nothing — which is the whole point, since the alternative is several
	// hundred megabytes leaving the machine and being refused at the far end.
	if maxBytes := int64(config.MaxUploadMB) * 1024 * 1024; info.Size() > maxBytes {
		return nil, fmt.Errorf(
			"engine: %s is %.1f MB, over the %d MB upload limit. Raise MAX_UPLOAD_MB if that "+
				"is deliberate", filepath.Base(filePath),
			float64(info.Size())/(1024*1024), config.MaxUploadMB)
	}

	mimeType := videoMIMEFor(filePath)
	if mimeType == "" {
		return nil, fmt.Errorf(
			"engine: %s does not look like a video (%q); Flow takes mp4, m4v, mov, webm or mkv",
			filepath.Base(filePath), strings.ToLower(filepath.Ext(filePath)))
	}

	// Hashed before the transport is built, because a cache hit should cost
	// nothing else: building the client is a TLS client and a QUIC transport, and
	// the whole point of the cache is that the expensive path does not run.
	hash, err := hashFile(filePath)
	if err != nil {
		return nil, err
	}

	if !opts.Force {
		cached, cacheErr := e.findCachedUpload(hash, projectID)
		switch {
		case cacheErr != nil:
			// A cache that cannot be read is not a reason to refuse the upload —
			// it is a reason to say so and carry on, which is what a cold cache
			// would have done anyway.
			log.Printf("engine: could not read the upload cache (%v); uploading %s anyway",
				cacheErr, filepath.Base(filePath))
		case cached != nil:
			log.Printf("engine: reusing uploaded media %s for %s", cached.MediaID, filePath)
			return &VideoUploadResult{
				MediaID:   cached.MediaID,
				Path:      filePath,
				Bytes:     info.Size(),
				ProjectID: projectID,
				Cached:    true,
			}, nil
		}
	}

	// Its own transport, for the timeout. The client's is fixed when it is built,
	// and the shared one is bounded by RequestTimeout — a number that measures a
	// JSON RPC of a few kilobytes. A video is orders of magnitude larger, so
	// sharing it means either a needlessly long RPC timeout or an upload that
	// fails on a slow link for no visible reason.
	//
	// The proxy is carried over: the upload is upstream traffic like any other,
	// and Flow scores IP consistency, so sending it from a different exit than
	// the generation would be the wrong kind of faithful.
	hc, err := httpx.New(
		httpx.WithTimeout(time.Duration(config.UploadTimeout)*time.Second),
		httpx.WithProxy(e.opts.ProxyURL),
		httpx.WithQUIC(false),
	)
	if err != nil {
		return nil, fmt.Errorf("engine: could not build the upload transport: %w", err)
	}

	step := uploadStep{
		HC:        hc,
		Jar:       e.Jar(),
		ProjectID: projectID,
		FileName:  filepath.Base(filePath),
		MimeType:  mimeType,
		Size:      info.Size(),
	}

	sessionURL, err := openUploadSession(ctx, step)
	if err != nil {
		return nil, err
	}

	mediaID, err := postVideo(ctx, step, sessionURL, filePath)
	if err != nil {
		return nil, err
	}

	// Recorded after the upload succeeded and never before. A row is a claim that
	// this media id resolves, so writing one for an upload that failed would make
	// the cache hand out something that was never created — and the failure would
	// surface later, as an edit on nothing.
	//
	// A failure to record is logged and not returned: the video is in the project
	// and the caller has its id, so failing the call now would send the operator
	// looking for a problem with a file that uploaded perfectly well. The cost is
	// one more upload next time.
	if e.store != nil {
		if recErr := e.store.RecordUpload(store.UploadRecord{
			FilePath:  filePath,
			FileHash:  hash,
			MediaID:   mediaID,
			ProjectID: projectID,
			Bytes:     info.Size(),
		}); recErr != nil {
			log.Printf("engine: uploaded %s but could not record it (%v); it will be sent again "+
				"next time", filepath.Base(filePath), recErr)
		}
	}

	return &VideoUploadResult{
		MediaID:   mediaID,
		Path:      filePath,
		Bytes:     info.Size(),
		ProjectID: projectID,
	}, nil
}

// findCachedUpload returns a previously recorded upload of these exact bytes into
// this project, or nil.
//
// A nil store is not an error and not a miss worth reporting: it means the engine
// was built without a database, and the upload simply proceeds.
func (e *Engine) findCachedUpload(hash, projectID string) (*store.UploadRecord, error) {
	if e.store == nil {
		return nil, nil
	}
	return e.store.FindUpload(hash, projectID)
}

// hashFile returns the SHA-256 of a file's contents, read in a stream.
//
// Streamed rather than read whole, because this runs on *every* upload — including
// the ones the cache answers, and a large file is exactly the case the cache
// exists for. Reading it into memory to hash it would give back most of what the
// cache saves.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("engine: could not open %s: %w", path, err)
	}
	defer f.Close()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("engine: could not read %s: %w", path, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// uploadStep is what both halves of the handshake need.
//
// Gathered into one value rather than passed as six arguments to each: the two
// calls share all of it, and a parameter list that long is where a swap goes
// unnoticed.
type uploadStep struct {
	HC *httpx.Client
	// Jar is the account's cookies, and the whole of the authentication. The
	// capture shows neither call carrying a bearer, which is consistent with how
	// everything else in this engine talks to flow.google.com.
	//
	// HeaderForDomain filters by host, so this cannot leak a Google cookie to a
	// session URL on some other host.
	Jar       *cookiejar.Jar
	ProjectID string
	FileName  string
	MimeType  string
	Size      int64
}

// openUploadSession is step one: it opens a resumable session and returns the
// URL to stream the file at.
//
// The content type and the length are declared here, before any of the file is
// sent — which is what makes them a contract rather than a hint, and why the
// caller refuses an unrecognised extension instead of sending the bytes anyway.
func openUploadSession(ctx context.Context, step uploadStep) (string, error) {
	url := flowUploadBase + uploadVideoPathPrefix + step.ProjectID

	resp, err := step.HC.Do(ctx, &httpx.Request{
		Method: "POST",
		URL:    url,
		Headers: map[string]string{
			"X-Goog-Upload-Protocol":              "resumable",
			"X-Goog-Upload-Command":               "start",
			"X-Goog-Upload-Header-Content-Length": strconv.FormatInt(step.Size, 10),
			"X-Goog-Upload-Header-Content-Type":   step.MimeType,
			"X-Goog-Upload-File-Name":             step.FileName,
			"Origin":                              flowUploadBase,
			"Referer":                             flowUploadBase + "/",
		},
		Cookies:     step.Jar.HeaderForDomain(url),
		DisableQUIC: true,
	})
	if err != nil {
		return "", fmt.Errorf("engine: could not open the upload session: %w", err)
	}
	if !resp.OK() {
		return "", fmt.Errorf("engine: the upload session was refused: HTTP %d %s",
			resp.Status, truncate(resp.Text(), 300))
	}

	if url := sessionURLFrom(resp.Header); url != "" {
		return url, nil
	}
	return "", fmt.Errorf(
		"engine: the upload session reply carried no session URL — expected %q, %q or %q. "+
			"Body: %s",
		"x-goog-upload-url", "Location", "x-goog-upload-control-url", truncate(resp.Text(), 300))
}

// postVideo is step two: it streams the file at the session URL and returns the
// media id the reply carries.
//
// POST rather than PUT, which is worth stating because the resumable-upload
// protocol this borrows from uses PUT for the data step and a reader who knows
// that will expect it here. The capture says POST.
//
// The body is streamed rather than buffered. A video is the one thing this
// engine sends that can be hundreds of megabytes, and reading it into a []byte
// to hand to a request would put all of it in memory twice.
func postVideo(ctx context.Context, step uploadStep, sessionURL, filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("engine: could not open %s: %w", filePath, err)
	}
	// Closed by the transport as well, since the body is an io.ReadCloser; a
	// second close is an error this deliberately ignores.
	defer f.Close()

	resp, err := step.HC.Do(ctx, &httpx.Request{
		Method: "POST",
		URL:    sessionURL,
		Headers: map[string]string{
			"X-Goog-Upload-Command": "upload, finalize",
			"X-Goog-Upload-Offset":  "0",
			"Content-Type":          step.MimeType,
		},
		Cookies:     step.Jar.HeaderForDomain(sessionURL),
		BodyReader:  f,
		BodyLength:  step.Size,
		DisableQUIC: true,
	})
	if err != nil {
		return "", fmt.Errorf("engine: the upload failed%s: %w", uploadTimeoutHint(err), err)
	}
	if !resp.OK() {
		return "", fmt.Errorf("engine: the upload was refused: HTTP %d %s",
			resp.Status, truncate(resp.Text(), 300))
	}

	return mediaIDFrom(resp.Body)
}

// sessionURLFrom finds the resumable session URL in a step-one reply.
//
// It comes from a response *header*, not a body — which is the second thing
// about this endpoint that is easy to get wrong by analogy with the old one,
// where the URL was JSON.
//
// Three names are tried because the capture names one and the protocol this
// borrows from names the others: `x-goog-upload-url` is what the app's own call
// carries, and `Location` and `x-goog-upload-control-url` are the shapes a
// resumable-upload session is also described with. Trying all three costs
// nothing and each is a URL a session could legitimately be at.
func sessionURLFrom(header stdhttp.Header) string {
	for _, name := range []string{"x-goog-upload-url", "Location", "x-goog-upload-control-url"} {
		if url := strings.TrimSpace(header.Get(name)); url != "" {
			return url
		}
	}
	return ""
}

// mediaIDFrom pulls the media id out of a step-two reply.
//
// The captured shape is `{"mediaId": "...", "media": {"name": "...",
// "projectId": "..."}, "workflow": {...}}`, so `mediaId` is the answer and
// `media.name` the documented fallback.
//
// Deliberately *not* the five names the old implementation accepted. Two of them
// — a top-level `name` and `id` — were guesses at a shape from a different
// endpoint, and the capture shows a `workflow` object beside `media`. A loose
// `name` fallback would be willing to pick an id out of whichever object happened
// to carry one, and an upload that succeeded and reported the wrong id is worse
// than one that says it could not read the answer.
func mediaIDFrom(body []byte) (string, error) {
	var doc struct {
		MediaID string `json:"mediaId"`
		Media   struct {
			Name    string `json:"name"`
			MediaID string `json:"mediaId"`
		} `json:"media"`
	}

	// An empty body is absent rather than malformed, and the two mean different
	// things: a body that is not JSON usually means something in front of the
	// endpoint answered instead of it — a gateway, an error page — whereas an
	// upload that answered with nothing probably worked. So only a non-empty
	// body that will not parse is reported as "not JSON"; an empty one falls
	// through to the message that says the upload may have succeeded.
	if err := json.Unmarshal(body, &doc); err != nil && strings.TrimSpace(string(body)) != "" {
		return "", fmt.Errorf("engine: the upload reply was not JSON: %s",
			truncate(string(body), 300))
	}

	if id := firstNonEmpty(doc.MediaID, doc.Media.Name, doc.Media.MediaID); id != "" {
		return id, nil
	}
	return "", fmt.Errorf(
		"engine: the upload reply carried no media id. The upload may have succeeded, but "+
			"nothing here can address it. Body: %s", truncate(string(body), 300))
}

// uploadTimeoutHint explains a timeout in terms of the knob that causes it.
//
// The transport's timeout is fixed when the client is built, so a video larger
// than the link can move inside UploadTimeout fails as a transport error. That
// reads like a network fault; naming the knob makes it actionable.
func uploadTimeoutHint(err error) string {
	var timeout interface{ Timeout() bool }
	if (errors.As(err, &timeout) && timeout.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf(" — it exceeded UPLOAD_TIMEOUT (%ds); raise it for large videos",
			config.UploadTimeout)
	}
	return ""
}

// videoMIMEFor maps a file extension to the content type Flow expects, or "" when
// the file is not a video it knows about.
//
// Deliberately not `mimeFor`, which handles images and falls back to
// `application/octet-stream`. The contracts differ: an image upload normalises
// the bytes before sending, so a permissive fallback is harmless there, whereas
// this content type is declared to the session call *before* any of the file is
// sent — so an unknown extension has to be refused here rather than claimed and
// discovered at the far end of a long upload.
func videoMIMEFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	}
	return ""
}

// firstNonEmpty returns the first argument that is not the empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
