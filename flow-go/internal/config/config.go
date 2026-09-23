// Package config holds every constant the Flow client needs.
//
// Ported from a Python engine that is no longer in this tree: the directory that
// held it, flow-agent/flow_engine, has been deleted, so the comments below that
// name it record where a value came from rather than pointing at something you
// can still go and read. That makes this file the only surviving copy of those
// values, which is the reason to keep the ones nothing calls — see the legacy
// Labs block near the bottom.
//
// Values that were environment overrides in Python remain environment overrides
// here.
package config

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

/* ------------------------------------------------------------------ *
 * Project / paths
 * ------------------------------------------------------------------ */

// DefaultProject is the Flow project the engine falls back to when the caller
// does not supply one and no project has been discovered from the browser.
const DefaultProject = "0143adf4-5864-4cb4-abb5-fe4254ad0dc7"

// DataDir returns the directory holding the SQLite DB, cookie cache, and output.
func DataDir() string {
	if v := os.Getenv("FLOW_DATA_DIR"); v != "" {
		return absExpand(v)
	}
	return absExpand("data")
}

// OutputDir is where generated media is written.
func OutputDir() string {
	if v := os.Getenv("FLOW_OUTPUT_DIR"); v != "" {
		return absExpand(v)
	}
	return absExpand(filepath.Join("output"))
}

// DBPath is the SQLite database file.
func DBPath() string {
	if v := os.Getenv("FLOW_DB_PATH"); v != "" {
		return absExpand(v)
	}
	return filepath.Join(DataDir(), "flow.db")
}

// CookieDir holds cookie files, one JSON array per account.
func CookieDir() string {
	if v := os.Getenv("FLOW_COOKIE_DIR"); v != "" {
		return absExpand(v)
	}
	return absExpand("cookies")
}

func absExpand(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

/* ------------------------------------------------------------------ *
 * Upstream API
 * ------------------------------------------------------------------ */

// APIBase is the Flow (Labs FX) generation backend.
const APIBase = "https://aisandbox-pa.googleapis.com"

// LabsBase is the Labs FX frontend, used for session and token exchange.
const LabsBase = "https://labs.google"

// FlowUIBase is the Flow tool UI, used as the Origin/Referer for generation calls.
const FlowUIBase = "https://labs.google/fx/tools/flow"

// LabsSessionPath returns the current Labs session as JSON. This is the endpoint
// that turns cookies into a usable access token without a browser.
const LabsSessionPath = "/fx/api/auth/session"

// APIKey is the public client key the Flow web UI embeds in every request.
// It is not a secret: it ships in the browser bundle. Overridable so a rotated
// key does not require a rebuild.
func APIKey() string {
	if v := os.Getenv("FLOW_API_KEY"); v != "" {
		return v
	}
	return "AIzaSyBtrm0o5ab1c-Ec8ZuLcGt3oJAA5VWt3pY"
}

// ClientContext is the fixed portion of every request's clientContext block.
type ClientContextValues struct {
	Tool             string
	Tier             string
	Origin           string
	RecaptchaAppType string
}

// ClientCtx is the value the Python engine carried as CLIENT_CTX. That file is
// gone, so this is now the only copy.
var ClientCtx = ClientContextValues{
	Tool:             "PINHOLE",
	Tier:             "PAYGATE_TIER_ONE",
	Origin:           "https://labs.google",
	RecaptchaAppType: "RECAPTCHA_APPLICATION_TYPE_WEB",
}

// Aspects maps friendly aspect names to the Flow enum values.
var Aspects = map[string]string{
	"portrait":  "VIDEO_ASPECT_RATIO_PORTRAIT",
	"landscape": "VIDEO_ASPECT_RATIO_LANDSCAPE",
}

// ImageAspects mirrors t2i.IMAGE_ASPECTS.
var ImageAspects = map[string]string{
	"landscape": "IMAGE_ASPECT_RATIO_LANDSCAPE",
	"4x3":       "IMAGE_ASPECT_RATIO_4_3",
	"square":    "IMAGE_ASPECT_RATIO_SQUARE",
	"3x4":       "IMAGE_ASPECT_RATIO_3_4",
	"portrait":  "IMAGE_ASPECT_RATIO_PORTRAIT",
}

/* ------------------------------------------------------------------ *
 * Endpoints
 * ------------------------------------------------------------------ */

// Endpoints is the map the Python engine carried as ENDPOINTS. That file is
// gone, so this is now the only copy. Paths containing "{}" are templates.
var Endpoints = map[string]string{
	"generate_t2v":    "/v1/video:batchAsyncGenerateVideoText",
	"generate_i2v":    "/v1/video:batchAsyncGenerateVideoStartImage",
	"generate_fl":     "/v1/video:batchAsyncGenerateVideoStartAndEndImage",
	"generate_r2v":    "/v1/video:batchAsyncGenerateVideoReferenceImages",
	"generate_edit":   "/v1/video:batchAsyncGenerateVideoEditVideo",
	"upload_image":    "/v1/flow/uploadImage",
	"upsample_video":  "/v1/video:batchAsyncGenerateVideoUpsampleVideo",
	"poll_status":     "/v1/video:batchCheckAsyncVideoGenerationStatus",
	"get_media":       "/v1/media/{media_id}",
	"get_credits":     "/v1/credits",
	"batch_images":    "/v1/projects/{project_id}/flowMedia:batchGenerateImages",
	"batch_check":     "/v1/projects/{project_id}/flowMedia:batchCheckAsync",
	"project_credits": "/v1/projects/{project_id}/credits",
}

/* ------------------------------------------------------------------ *
 * Models
 * ------------------------------------------------------------------ */

// VideoCosts is what a video costs in credits, by duration and quality.
//
// Measured against a live account rather than derived. The app charges per render,
// and the quality is the expensive axis: a 4s 360p costs 4 against 7 for the 720p
// of the same length. That gap is the difference between an account being able to
// submit and being refused, and the engine had no way to know it — it asked for
// the 720p model unconditionally and answered "submitted 0 videos" when the
// balance would not cover it, which reads like a broken request rather than an
// empty wallet.
var VideoCosts = map[int]map[string]int{
	4:  {"360p": 4, "720p": 7},
	6:  {"360p": 5, "720p": 10},
	8:  {"360p": 6, "720p": 12},
	10: {"360p": 7, "720p": 15},
}

// NormalizeVideoQuality returns the quality key the model suffix and the cost
// table both use. Anything unrecognised is the 720p default, which is what the
// app renders when no quality is chosen.
func NormalizeVideoQuality(quality string) string {
	if strings.EqualFold(strings.TrimSpace(quality), "360p") {
		return "360p"
	}
	return "720p"
}

// VideoCost returns what a render costs, and whether that pair is known.
func VideoCost(duration int, quality string) (int, bool) {
	if duration == 0 {
		duration = DefaultDuration
	}
	byQuality, ok := VideoCosts[duration]
	if !ok {
		return 0, false
	}
	cost, ok := byQuality[NormalizeVideoQuality(quality)]
	return cost, ok
}

// VideoModelQuality applies a quality to a model key.
//
// The catalog carries a `_360p` variant of every video model, and the suffix is
// the whole of the difference — duration and conditioning are unchanged. An
// unrecognised quality leaves the key alone rather than inventing a suffix the
// catalog does not have.
func VideoModelQuality(base, quality string) string {
	if NormalizeVideoQuality(quality) == "360p" {
		return base + "_360p"
	}
	return base
}

// VideoModels maps a duration in seconds to the Flow model key.
var VideoModels = map[int]string{
	4:  "abra_t2v_4s",
	6:  "abra_t2v_6s",
	8:  "abra_t2v_8s",
	10: "abra_t2v_10s",
}

// ImageToVideoModels maps a duration to the model used when only a start frame
// is supplied. These are the abra_i2v_* keys, and they go to a different RPC
// from either of the other two cases.
var ImageToVideoModels = map[int]string{
	4:  "abra_i2v_4s",
	6:  "abra_i2v_6s",
	8:  "abra_i2v_8s",
	10: "abra_i2v_10s",
}

// FirstLastVideoModels maps a duration to the model used when both a start and
// an end frame are supplied.
var FirstLastVideoModels = map[int]string{
	4:  "omni_flash_i2v_4s_first_last",
	6:  "omni_flash_i2v_6s_first_last",
	8:  "omni_flash_i2v_8s_first_last",
	10: "omni_flash_i2v_10s_first_last",
}

// ReferenceVideoModels maps a duration to the model used when several reference
// images are supplied. These go to a fourth RPC of their own, with a payload shape
// unlike any of the others.
var ReferenceVideoModels = map[int]string{
	4:  "abra_r2v_4s",
	6:  "abra_r2v_6s",
	8:  "abra_r2v_8s",
	10: "abra_r2v_10s",
}

// ReferenceVideoModelFor picks the reference-image model for a duration.
func ReferenceVideoModelFor(duration int) (string, bool) {
	if duration == 0 {
		duration = DefaultDuration
	}
	model, ok := ReferenceVideoModels[duration]
	return model, ok
}

// VideoModelFor picks the model key for a generation from its duration and the
// frames it is conditioned on.
//
// The three cases need three different models *and* three different RPCs, so a
// single per-duration default cannot serve them: conditioning a t2v model on an
// image is accepted by the server and generates nothing.
func VideoModelFor(duration int, hasStart, hasEnd bool) (string, bool) {
	if duration == 0 {
		duration = DefaultDuration
	}
	switch {
	case hasStart && hasEnd:
		model, ok := FirstLastVideoModels[duration]
		return model, ok
	case hasStart:
		model, ok := ImageToVideoModels[duration]
		return model, ok
	default:
		model, ok := VideoModels[duration]
		return model, ok
	}
}

// EditModel is the video-to-video edit model key.
const EditModel = "abra_edit"

// ImageModels maps friendly names to the Flow image model enum.
var ImageModels = map[string]string{
	"harbor_seal":   "HARBOR_SEAL",
	"lite":          "HARBOR_SEAL",
	"narwhal":       "NARWHAL",
	"nano_banana_2": "NARWHAL",
	"standard":      "NARWHAL",
	"gem_pix_2":     "GEM_PIX_2",
	"pro":           "GEM_PIX_2",
}

// DefaultImageModel resolves IMAGE_MODEL, accepting either a friendly key or an
// already-resolved enum value.
func DefaultImageModel() string {
	raw := os.Getenv("IMAGE_MODEL")
	if raw == "" {
		return "NARWHAL"
	}
	if resolved, ok := ImageModels[strings.ToLower(raw)]; ok {
		return resolved
	}
	for _, v := range ImageModels {
		if v == raw {
			return raw
		}
	}
	return "NARWHAL"
}

/* ------------------------------------------------------------------ *
 * Upsampling
 * ------------------------------------------------------------------ */

// NativeVideoResolution is what Flow generates before an upsampler pass.
const NativeVideoResolution = "720p"

// UpsampleModels maps a target resolution to the upsampler model key.
var UpsampleModels = map[string]string{
	"1080p": envOr("VIDEO_UPSAMPLER_1080P_MODEL", "veo_3_1_upsampler_1080p"),
	"4k":    envOr("VIDEO_UPSAMPLER_4K_MODEL", "veo_3_1_upsampler_4k"),
}

// UpsampleResolutions maps a target resolution to the Flow resolution enum.
var UpsampleResolutions = map[string]string{
	"1080p": envOr("VIDEO_UPSAMPLE_ENUM_1080P", "VIDEO_RESOLUTION_1080P"),
	"4k":    envOr("VIDEO_UPSAMPLE_ENUM_4K", "VIDEO_RESOLUTION_4K"),
}

// CreditsPerUpsample: 1080p is free, 4K is a paid higher-tier operation.
var CreditsPerUpsample = map[string]int{"1080p": 0, "4k": 50}

/* ------------------------------------------------------------------ *
 * Generation economics
 * ------------------------------------------------------------------ */

// Durations lists the supported video lengths.
var Durations = []int{4, 6, 8, 10}

// DefaultDuration is used when the caller does not specify one.
const DefaultDuration = 10

// MaxCount is the maximum variations per request.
const MaxCount = 4

// CreditsPerVideo is the cost of one video at each duration.
var CreditsPerVideo = map[int]int{4: 7, 6: 10, 8: 12, 10: 15}

// CreditsPerVideoEdit is what one video edit costs.
//
// Set to the standard 10s video price. The app does not expose a separate edit
// price, and an edit is a full render of the same length, so quoting the 10s
// figure is the closest honest answer available. It is a named constant rather
// than a literal at the call site so the number is visible and revisable in one
// place — the edit path had no price at all before this, which meant it had no
// affordability check either.
var CreditsPerVideoEdit = 10

// CreditsPerReferenceVideo is what one reference-conditioned video costs.
//
// Same reasoning as CreditsPerVideoEdit: a `abra_r2v_*` render is the same
// length and the same shape of work as the text-conditioned one, so it is priced
// at the 10s figure. Before this the path had no price, so a reference render on
// a drained account was submitted and answered with nothing rather than refused.
var CreditsPerReferenceVideo = 10

// SegmentDuration and FPS describe the produced media.
const (
	SegmentDuration = 10
	FPS             = 24
)

/* ------------------------------------------------------------------ *
 * Runtime
 * ------------------------------------------------------------------ */

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

// WSPort is the port the extension bridge listens on. There is no HTTP port:
// flow-go has no HTTP API, so `flow-go bridge` is a WebSocket listener and the
// generation paths are CLI runs. HTTP_PORT is deliberately not read any more —
// setting it has no effect.
var WSPort = envInt("WS_PORT", 9222)

// AccountIndex seeds which signed-in Google account the engine acts as, as an
// `authuser` index. It is only a seed: once an account has been chosen and
// recorded, the stored index is what the engine starts on, so a deliberate choice
// is not undone by the next restart.
var AccountIndex = envInt("ACCOUNT_INDEX", 0)

// ProjectID is a Flow project to generate into, for runs that should not have to
// discover one from a browser.
//
// The engine can learn a project id without a browser — it lists the account's
// projects over the transport, and creates one when the account has none — but
// naming one here skips both, so a run that must not make either call can say
// where it wants to work.
//
// Empty by default, and deliberately ranked below a live browser — see
// engine.Options.DefaultProjectID for why.
var ProjectID = envOr("FLOW_PROJECT_ID", "")

// AccountScanLimit bounds how many signed-in account indices are examined when
// looking for one that can pay for a job.
//
// Chrome permits ten, but the scan costs a session, a profile and a balance read
// per index and it runs on the request path — so the default covers the common
// case and the knob covers the rest. Scanning past the real number of signed-in
// accounts is harmless rather than wrong: an index beyond the last falls back to
// the default account, and those repeats are recognised and dropped.
var AccountScanLimit = envInt("ACCOUNT_SCAN_LIMIT", 6)

var (
	// PollInterval is seconds between generation status polls.
	PollInterval = envInt("POLL_INTERVAL", 10)
	// PollTimeout is the total seconds to wait for a generation to finish.
	PollTimeout = envInt("POLL_TIMEOUT", 420)
	// RequestTimeout bounds a single upstream HTTP call.
	RequestTimeout = envInt("REQUEST_TIMEOUT", 90)
	// UploadTimeout bounds one video upload, in seconds.
	//
	// Its own knob rather than sharing RequestTimeout, because the two measure
	// different things. RequestTimeout bounds a JSON RPC of a few kilobytes; a
	// video is orders of magnitude larger, so one number for both means either a
	// needlessly long RPC timeout or an upload that fails on a slow link for a
	// reason the operator cannot see. The transport's timeout is fixed when the
	// client is built, so the upload needs its own client either way.
	UploadTimeout = envInt("UPLOAD_TIMEOUT", 300)
	// MaxUploadMB is the largest video the upload will send, in megabytes.
	//
	// A guard against pointing the command at the wrong file rather than a limit
	// Flow is known to enforce — no such limit has been observed, and this port
	// has never uploaded against a live account. So it is deliberately generous
	// and deliberately overridable: refusing a legitimate 150 MB clip would be a
	// worse failure than allowing a large one, and the whole cost of the guard is
	// that a mistake is caught before several hundred megabytes leave the machine.
	MaxUploadMB = envInt("MAX_UPLOAD_MB", 100)
	// SessionTTL bounds how long a cached access token is trusted, in seconds.
	// Google's bearer tokens live ~60 minutes; we refresh well before that.
	SessionTTL = envInt("SESSION_TTL", 2400)
)

// Rate limiting guards against Google's UNUSUAL_ACTIVITY throttle.
var (
	// MaxConcurrentRequests is the in-flight generation cap per worker.
	MaxConcurrentRequests = envInt("MAX_CONCURRENT_REQUESTS", 4)
	// RequestMinInterval is the minimum spacing between generation starts.
	RequestMinInterval = envFloat("REQUEST_MIN_INTERVAL", 2.0)
)

// UserAgents rotated across requests.
var UserAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
}

// The legacy Labs REST surface.
//
// These are the values the previous Python engine hardcoded. That engine is gone
// from this tree, so these constants are the only surviving record of the
// surface — which is a better reason to keep them than the one they were kept
// for.
//
// They are kept so the surface can be probed: if it still answers, the
// image-to-video, reference and edit generators can be ported from the recorded
// shape instead of re-derived for batchexecute. Nothing depends on them at
// runtime, and nothing here is reachable from the HTTP API or the CLI.
const (
	labsAPIBase = "https://aisandbox-pa.googleapis.com"
	// labsAPIKey is the public client key the previous engine shipped. It is not
	// a secret, but it is referrer-restricted, which is why that engine could
	// never be pointed at flow.google.com.
	labsAPIKey = "AIzaSyBtrm0o5ab1c-Ec8ZuLcGt3oJAA5VWt3pY"
)

// LabsAPIBase is the legacy REST host.
func LabsAPIBase() string { return labsAPIBase }

// LabsAPIKey is the legacy public client key.
func LabsAPIKey() string { return labsAPIKey }

// BuildLabel returns the app's `bl` parameter, sent with every batchexecute call.
//
// It identifies the frontend build the client claims to be. The default is the
// value observed in the live app's own requests; override with FLOW_BUILD_LABEL
// when the app ships a new build.
//
// Re-read it from a live request rather than trusting this, and the reason is in
// this comment's own history: the app moved from `.00_p0` to `.09_p0` and then
// on again, and nothing here noticed either time, because a stale label produces
// no error — the calls simply go out claiming to be an older frontend than the
// one they are talking to.
//
// The current value was read back off https://flow.google.com/ on 2026-09-23
// rather than assumed, which is the only way this stays true:
//
//	curl -sS -A "<a Chrome UA>" https://flow.google.com/ \
//	  | grep -oE 'boq_labs-ai-sandbox-frontend_[0-9]+\.[0-9]+_p[0-9]'
func BuildLabel() string {
	return envOr("FLOW_BUILD_LABEL", "boq_labs-ai-sandbox-frontend_20260922.00_p0")
}

// LoadEnv reads .env and config.env if present. Real environment variables win,
// matching the setdefault behaviour of the Python loader.
func LoadEnv() {
	for _, name := range []string{".env", "config.env"} {
		if _, err := os.Stat(name); err != nil {
			continue
		}
		if err := loadWithoutOverride(name); err != nil {
			log.Printf("config: could not read %s: %v", name, err)
		}
	}
}

// loadWithoutOverride parses a dotenv file but only fills variables that are
// not already set in the process environment.
func loadWithoutOverride(path string) error {
	values, err := godotenv.Read(path)
	if err != nil {
		return err
	}
	for key, value := range values {
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}

// EnsureDirs creates the directories the engine writes to.
func EnsureDirs() error {
	for _, dir := range []string{DataDir(), OutputDir(), CookieDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}
