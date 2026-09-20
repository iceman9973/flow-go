package flowapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kodelyx/flow-go/flow-go/internal/config"
)

/* ------------------------------------------------------------------ *
 * Request context blocks
 * ------------------------------------------------------------------ */

// RecaptchaContext is the captcha block nested inside clientContext.
type RecaptchaContext struct {
	ApplicationType string `json:"applicationType,omitempty"`
	Token           string `json:"token"`
}

// ClientContext is the per-request client block every Flow call carries.
type ClientContext struct {
	ProjectID        string            `json:"projectId,omitempty"`
	Tool             string            `json:"tool"`
	UserPaygateTier  string            `json:"userPaygateTier,omitempty"`
	SessionID        string            `json:"sessionId,omitempty"`
	RecaptchaContext *RecaptchaContext `json:"recaptchaContext,omitempty"`
}

// GenerationContext carries the batch identity for a generation.
type GenerationContext struct {
	BatchID                string `json:"batchId"`
	AudioFailurePreference string `json:"audioFailurePreference,omitempty"`
}

// buildClientContext mirrors generators/common.py:build_client_context.
func buildClientContext(projectID, tier, captchaToken string) *ClientContext {
	return &ClientContext{
		ProjectID:       projectID,
		Tool:            config.ClientCtx.Tool,
		UserPaygateTier: tier,
		SessionID:       fmt.Sprintf(";%d", time.Now().UnixMilli()),
		RecaptchaContext: &RecaptchaContext{
			ApplicationType: config.ClientCtx.RecaptchaAppType,
			Token:           captchaToken,
		},
	}
}

// buildGenerationContext mirrors generators/common.py:build_generation_context.
func buildGenerationContext(audioPref string) *GenerationContext {
	return &GenerationContext{
		BatchID:                uuid.NewString(),
		AudioFailurePreference: audioPref,
	}
}

// resolveSeed mirrors generators/common.py:resolve_seed.
//
// A nil seed yields a fresh random value per request, which is the Python
// behaviour. An explicit seed is offset by the variation index so the takes in a
// batch differ from each other while remaining reproducible as a set.
func resolveSeed(seed *int64, index int) int64 {
	if seed == nil {
		return int64(randUint32()%9999) + 1
	}
	return (*seed + int64(index)) % 4294967296
}

func randUint32() uint32 {
	return uint32(time.Now().UnixNano() ^ int64(uuid.New().ID()))
}

/* ------------------------------------------------------------------ *
 * Responses
 * ------------------------------------------------------------------ */

// MediaItem is one entry of the `media` array in a Flow response. Only the fields
// the engine reads are modelled; the rest is preserved in RawItem for callers
// that need to dig further.
type MediaItem struct {
	Name          string          `json:"name"`
	MediaMetadata MediaMetadata   `json:"mediaMetadata"`
	Image         ImagePayload    `json:"image"`
	Video         VideoPayload    `json:"video"`
	Raw           json.RawMessage `json:"-"`
}

// MediaMetadata wraps the nested status block.
type MediaMetadata struct {
	MediaStatus MediaStatus `json:"mediaStatus"`
}

// MediaStatus reports where a generation has got to.
type MediaStatus struct {
	MediaGenerationStatus string `json:"mediaGenerationStatus"`
}

// ImagePayload carries the generated image reference.
type ImagePayload struct {
	GeneratedImage GeneratedImage `json:"generatedImage"`
}

// GeneratedImage holds the URLs a generated image can be fetched from.
type GeneratedImage struct {
	FifeURL  string `json:"fifeUrl"`
	ImageURI string `json:"imageUri"`
}

// VideoPayload carries the generated video reference, when the API inlines it.
type VideoPayload struct {
	EncodedVideo string `json:"encodedVideo"`
}

// Generation status values returned in MediaStatus.
const (
	StatusSuccessful = "MEDIA_GENERATION_STATUS_SUCCESSFUL"
	StatusFailed     = "MEDIA_GENERATION_STATUS_FAILED"
	StatusBlocked    = "MEDIA_GENERATION_STATUS_BLOCKED"
)

// Terminal reports whether a status means the poll can stop.
func (s MediaStatus) Terminal() bool {
	v := s.MediaGenerationStatus
	return v == StatusSuccessful || strings.Contains(v, "FAILED") || strings.Contains(v, "BLOCKED")
}

// Succeeded reports a successful terminal status.
func (s MediaStatus) Succeeded() bool { return s.MediaGenerationStatus == StatusSuccessful }

// GenerateResponse is the shared shape of every generation endpoint.
type GenerateResponse struct {
	Media            []MediaItem `json:"media"`
	Operations       []MediaItem `json:"operations"`
	RemainingCredits json.Number `json:"remainingCredits"`
	Error            *ErrorBody  `json:"error"`
}

// CreditsResponse is the shape of the credits endpoint.
type CreditsResponse struct {
	Credits          json.Number `json:"credits"`
	RemainingCredits json.Number `json:"remainingCredits"`
	Sku              string      `json:"sku"`
	Error            *ErrorBody  `json:"error"`
}

// ErrorBody is the upstream error envelope.
type ErrorBody struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Status  string        `json:"status"`
	Details []ErrorDetail `json:"details"`
}

// ErrorDetail carries the machine-readable reason for a rejection.
type ErrorDetail struct {
	Reason string `json:"reason"`
	Type   string `json:"@type"`
}

// Reason returns the first detail reason, which is the useful part of a Flow
// rejection ("NO_CREDITS", "UNUSUAL_ACTIVITY", ...).
func (e *ErrorBody) Reason() string {
	if e == nil {
		return ""
	}
	for _, d := range e.Details {
		if d.Reason != "" {
			return d.Reason
		}
	}
	return ""
}

// MediaIDs extracts the media IDs from a response, preferring `media` and
// falling back to `operations` for the legacy shape the upsampler returns.
func (r *GenerateResponse) MediaIDs() []string {
	seen := make(map[string]struct{})
	var ids []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		ids = append(ids, name)
	}
	for _, m := range r.Media {
		add(m.Name)
	}
	for _, op := range r.Operations {
		add(op.Name)
	}
	return ids
}

/* ------------------------------------------------------------------ *
 * Errors
 * ------------------------------------------------------------------ */

// APIError is a non-2xx response from Flow, classified.
type APIError struct {
	Status  int
	Message string
	Reason  string
	Code    string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "request failed"
	}
	if e.Reason != "" {
		return fmt.Sprintf("flow: %s (%d %s)", msg, e.Status, e.Reason)
	}
	return fmt.Sprintf("flow: %s (%d)", msg, e.Status)
}

// Unauthenticated reports whether the failure was an auth problem, which means
// the caller should mint a new token and retry rather than give up.
//
// This is the single most important classification in the engine. The Python
// version only treated a bare 401 as unauthenticated, so once the extension had
// cleared its own token every later call came back as a generic 503 and the
// self-heal path never ran — the pipeline deadlocked until someone restarted it.
// Here every shape an expired credential can take is recognised.
func (e *APIError) Unauthenticated() bool {
	if e == nil {
		return false
	}
	switch e.Status {
	case 401, 407:
		return true
	case 403:
		// 403 is overloaded: it covers both "token expired" and "account not
		// allowed". Only the auth-shaped ones count.
		switch strings.ToUpper(e.Code) {
		case "UNAUTHENTICATED", "PERMISSION_DENIED_AUTH", "CREDENTIALS_MISSING":
			return true
		}
		switch strings.ToUpper(e.Reason) {
		case "AUTH_ERROR", "UNAUTHENTICATED", "CREDENTIALS_MISSING", "INVALID_CREDENTIALS":
			return true
		}
		lower := strings.ToLower(e.Message)
		return strings.Contains(lower, "invalid credentials") ||
			strings.Contains(lower, "authentication") ||
			strings.Contains(lower, "token has been expired") ||
			strings.Contains(lower, "login required")
	}
	// A 503 whose body says the credential is missing is an auth failure, not a
	// transient outage. This is exactly the case the Python engine missed.
	if e.Status == 503 {
		upper := strings.ToUpper(e.Message)
		if strings.Contains(upper, "NO_FLOW_KEY") || strings.Contains(upper, "NO_TOKEN") ||
			strings.Contains(upper, "UNAUTHENTICATED") {
			return true
		}
	}
	return false
}

// Retryable reports whether a retry against another account is worth attempting.
func (e *APIError) Retryable() bool {
	if e == nil {
		return false
	}
	switch e.Status {
	case 429, 500, 502, 503, 504:
		return true
	case 403:
		// UNUSUAL_ACTIVITY is a soft throttle: a different account may succeed.
		return strings.Contains(strings.ToUpper(e.Reason), "UNUSUAL_ACTIVITY")
	}
	return false
}

// Throttled reports whether the failure was the server pushing back rather than
// the request being wrong.
//
// Two statuses mean the same thing here: a clean 429, and the 403
// UNUSUAL_ACTIVITY soft throttle. Retrying either against the same account makes
// it worse, which is why the pool parks a worker on the first one instead of
// waiting for a streak — a streak of throttled calls is exactly the damage.
func (e *APIError) Throttled() bool {
	if e == nil {
		return false
	}
	if e.Status == 429 {
		return true
	}
	return e.Status == 403 && strings.Contains(strings.ToUpper(e.Reason), "UNUSUAL_ACTIVITY")
}

// OutOfCredits reports the specific condition where the account cannot pay.
func (e *APIError) OutOfCredits() bool {
	if e == nil {
		return false
	}
	reason := strings.ToUpper(e.Reason)
	if strings.Contains(reason, "CREDIT") || strings.Contains(reason, "QUOTA") {
		return true
	}
	lower := strings.ToLower(e.Message)
	return strings.Contains(lower, "not enough credits") || strings.Contains(lower, "insufficient credits")
}
