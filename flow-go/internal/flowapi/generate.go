package flowapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/kodelyx/flow-go/flow-go/internal/config"
)

/* ------------------------------------------------------------------ *
 * Video generation
 * ------------------------------------------------------------------ */

// VideoRequest describes one text-to-video submission.
type VideoRequest struct {
	Prompt   string
	Aspect   string // "portrait" or "landscape"; enum values pass through
	Duration int    // one of config.Durations
	Count    int    // 1..config.MaxCount
	Seed     *int64
	// ModelKey overrides the duration-derived model. Used by callers that need
	// a model the duration table does not cover.
	ModelKey string
	// AudioPreference is forwarded as audioFailurePreference.
	AudioPreference string
}

// VideoResult is the outcome of a submission.
type VideoResult struct {
	MediaIDs         []string
	RemainingCredits int
}

// GenerateVideo submits a text-to-video job. Returns the media IDs to poll.
func (c *Client) GenerateVideo(ctx context.Context, req VideoRequest) (*VideoResult, error) {
	body, err := c.buildVideoBody(req, "")
	if err != nil {
		return nil, err
	}
	return c.submitVideo(ctx, config.Endpoints["generate_t2v"], body, req)
}

// GenerateVideoFromImage submits an image-to-video job.
func (c *Client) GenerateVideoFromImage(ctx context.Context, req VideoRequest, imageMediaID string) (*VideoResult, error) {
	body, err := c.buildVideoBody(req, imageMediaID)
	if err != nil {
		return nil, err
	}
	return c.submitVideo(ctx, config.Endpoints["generate_i2v"], body, req)
}

// GenerateVideoFirstLast submits a job interpolating between two frames.
func (c *Client) GenerateVideoFirstLast(ctx context.Context, req VideoRequest, startMediaID, endMediaID string) (*VideoResult, error) {
	modelKey, err := resolveVideoModel(req)
	if err != nil {
		return nil, err
	}
	aspect, err := resolveAspect(req.Aspect)
	if err != nil {
		return nil, err
	}

	requests := make([]map[string]any, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		requests = append(requests, map[string]any{
			"aspectRatio":   aspect,
			"textInput":     structuredPrompt(req.Prompt),
			"videoModelKey": modelKey,
			"seed":          resolveSeed(req.Seed, i),
			"metadata":      map[string]any{},
			"startImage":    map[string]any{"mediaId": startMediaID},
			"endImage":      map[string]any{"mediaId": endMediaID},
		})
	}

	body := map[string]any{
		"mediaGenerationContext": buildGenerationContext(req.AudioPreference),
		"requests":               requests,
		"useV2ModelConfig":       true,
	}
	return c.submitVideo(ctx, config.Endpoints["generate_fl"], body, req)
}

// GenerateVideoFromReferences submits a job using reference images for
// character or style consistency.
func (c *Client) GenerateVideoFromReferences(ctx context.Context, req VideoRequest, refMediaIDs []string) (*VideoResult, error) {
	if len(refMediaIDs) == 0 {
		return nil, fmt.Errorf("flow: reference generation needs at least one reference image")
	}
	modelKey, err := resolveVideoModel(req)
	if err != nil {
		return nil, err
	}
	aspect, err := resolveAspect(req.Aspect)
	if err != nil {
		return nil, err
	}

	refs := make([]map[string]any, 0, len(refMediaIDs))
	for _, id := range refMediaIDs {
		refs = append(refs, map[string]any{
			"mediaId":        id,
			"imageUsageType": "IMAGE_USAGE_TYPE_ASSET",
		})
	}

	requests := make([]map[string]any, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		requests = append(requests, map[string]any{
			"aspectRatio":     aspect,
			"textInput":       structuredPrompt(req.Prompt),
			"videoModelKey":   modelKey,
			"seed":            resolveSeed(req.Seed, i),
			"metadata":        map[string]any{},
			"referenceImages": refs,
		})
	}

	body := map[string]any{
		"mediaGenerationContext": buildGenerationContext(req.AudioPreference),
		"requests":               requests,
		"useV2ModelConfig":       true,
	}
	return c.submitVideo(ctx, config.Endpoints["generate_r2v"], body, req)
}

func (c *Client) buildVideoBody(req VideoRequest, startImageID string) (map[string]any, error) {
	modelKey, err := resolveVideoModel(req)
	if err != nil {
		return nil, err
	}
	aspect, err := resolveAspect(req.Aspect)
	if err != nil {
		return nil, err
	}

	requests := make([]map[string]any, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		item := map[string]any{
			"aspectRatio":   aspect,
			"textInput":     structuredPrompt(req.Prompt),
			"videoModelKey": modelKey,
			"seed":          resolveSeed(req.Seed, i),
			"metadata":      map[string]any{},
		}
		if startImageID != "" {
			item["startImage"] = map[string]any{"mediaId": startImageID}
		}
		requests = append(requests, item)
	}

	return map[string]any{
		"mediaGenerationContext": buildGenerationContext(req.AudioPreference),
		"requests":               requests,
		"useV2ModelConfig":       true,
	}, nil
}

func (c *Client) submitVideo(ctx context.Context, endpoint string, body map[string]any, req VideoRequest) (*VideoResult, error) {
	log.Printf("flow[%s]: generating %d video(s) at %ds [%s]",
		c.opts.AccountID, req.Count, req.Duration, truncate(req.Prompt, 60))

	result, err := c.Call(ctx, endpoint, body, recaptchaActionVideo)
	if err != nil {
		return nil, err
	}

	var parsed GenerateResponse
	if err := result.JSON(&parsed); err != nil {
		return nil, fmt.Errorf("flow: decode generation response: %w", err)
	}

	ids := parsed.MediaIDs()
	if len(ids) == 0 {
		return nil, fmt.Errorf("flow: generation accepted but returned no media")
	}

	out := &VideoResult{MediaIDs: ids}
	if n, err := parsed.RemainingCredits.Int64(); err == nil {
		out.RemainingCredits = int(n)
	}

	log.Printf("flow[%s]: submitted %d media (credits left: %d)",
		c.opts.AccountID, len(ids), out.RemainingCredits)
	return out, nil
}

/* ------------------------------------------------------------------ *
 * Image generation
 * ------------------------------------------------------------------ */

// ImageRequest describes one text-to-image submission.
type ImageRequest struct {
	Prompt       string
	Aspect       string // landscape, 4x3, square, 3x4, portrait
	Count        int
	Seed         *int64
	Model        string   // friendly name or enum; empty uses the configured default
	ReferenceIDs []string // media IDs to use as reference images
}

// ImageResult is one generated image.
type ImageResult struct {
	MediaID  string
	ImageURL string
	Model    string
}

// GenerateImage submits a text-to-image job and returns the resulting images.
//
// Unlike video, this endpoint is synchronous: the URLs come back in the response,
// so no polling is needed.
func (c *Client) GenerateImage(ctx context.Context, req ImageRequest) ([]ImageResult, error) {
	if req.Count < 1 {
		req.Count = 1
	}
	if req.Count > config.MaxCount {
		req.Count = config.MaxCount
	}

	aspect := config.ImageAspects[req.Aspect]
	if aspect == "" {
		aspect = req.Aspect
	}

	model := config.ImageModels[strings.ToLower(req.Model)]
	if model == "" {
		model = req.Model
	}
	if model == "" {
		model = config.DefaultImageModel()
	}

	requests := make([]map[string]any, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		item := map[string]any{
			"seed": resolveSeed(req.Seed, i),
			// Images take the prompt parts directly. Video wraps them in a
			// textInput block; images do not, and using the video helper here
			// produces a doubly nested structuredPrompt that the API rejects with
			// `Unknown name "structuredPrompt"`.
			"structuredPrompt": promptParts(req.Prompt),
			"imageAspectRatio": aspect,
			"imageModelName":   model,
			"imageInputs":      []any{},
		}
		if len(req.ReferenceIDs) > 0 {
			inputs := make([]map[string]any, 0, len(req.ReferenceIDs))
			for _, id := range req.ReferenceIDs {
				inputs = append(inputs, map[string]any{
					"name":           id,
					"imageInputType": "IMAGE_INPUT_TYPE_REFERENCE",
				})
			}
			item["imageInputs"] = inputs
		}
		requests = append(requests, item)
	}

	// The image endpoint expects these at the top level unconditionally, unlike
	// the video path where they only appear for reference-conditioned requests.
	body := map[string]any{
		"requests":               requests,
		"mediaGenerationContext": buildGenerationContext(""),
		"useNewMedia":            true,
	}

	endpoint := config.Endpoints["batch_images"]
	result, err := c.Call(ctx, endpoint, body, recaptchaActionImage)
	if err != nil {
		return nil, err
	}

	var parsed GenerateResponse
	if err := result.JSON(&parsed); err != nil {
		return nil, fmt.Errorf("flow: decode image response: %w", err)
	}

	images := parseImages(parsed.Media, model)
	if len(images) == 0 {
		return nil, fmt.Errorf("flow: image generation returned no images")
	}

	log.Printf("flow[%s]: generated %d image(s)", c.opts.AccountID, len(images))
	return images, nil
}

var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func parseImages(items []MediaItem, model string) []ImageResult {
	out := make([]ImageResult, 0, len(items))
	for _, item := range items {
		img := ImageResult{Model: model}
		if uuidRe.MatchString(item.Name) {
			img.MediaID = item.Name
		}
		url := item.Image.GeneratedImage.FifeURL
		if url == "" {
			url = item.Image.GeneratedImage.ImageURI
		}
		img.ImageURL = url
		if img.MediaID == "" && url != "" {
			img.MediaID = uuidRe.FindString(url)
		}
		out = append(out, img)
	}
	return out
}

/* ------------------------------------------------------------------ *
 * Upload
 * ------------------------------------------------------------------ */

// UploadImage sends image bytes to Flow and returns the resulting media ID.
func (c *Client) UploadImage(ctx context.Context, data []byte, mimeType string) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("flow: refusing to upload an empty file")
	}

	body := map[string]any{
		"imageBytes": base64.StdEncoding.EncodeToString(data),
	}

	result, err := c.Call(ctx, config.Endpoints["upload_image"], body, "")
	if err != nil {
		return "", err
	}

	var parsed struct {
		MediaID string `json:"mediaId"`
		Name    string `json:"name"`
		Media   struct {
			Name string `json:"name"`
		} `json:"media"`
	}
	if err := result.JSON(&parsed); err != nil {
		return "", fmt.Errorf("flow: decode upload response: %w", err)
	}

	mediaID := parsed.MediaID
	if mediaID == "" {
		mediaID = parsed.Name
	}
	if mediaID == "" {
		mediaID = parsed.Media.Name
	}
	if mediaID == "" {
		return "", fmt.Errorf("flow: upload succeeded but returned no media ID")
	}
	return mediaID, nil
}

/* ------------------------------------------------------------------ *
 * Upsampling
 * ------------------------------------------------------------------ */

// upsampleResolutionCandidates mirrors the ladder in generators/upsample.py.
// The resolution enum is undocumented, so a rejected spelling is retried and
// finally omitted entirely — the model key already encodes the target. A
// rejected request never starts a generation, so the ladder cannot double-charge.
var upsampleResolutionCandidates = map[string][]string{
	"1080p": {"VIDEO_RESOLUTION_1080P", "VIDEO_RESOLUTION_1080p", ""},
	"4k":    {"VIDEO_RESOLUTION_4K", "VIDEO_RESOLUTION_4k", ""},
}

var schemaRejectionHints = []string{
	"resolution", "invalid value", "invalid_argument", "unknown name", "cannot find field",
}

// NormalizeResolution maps a user-facing resolution to an upsample tier, or ""
// for the native 720p output which needs no second pass.
func NormalizeResolution(resolution string) (string, error) {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(resolution), " ", ""))
	switch key {
	case "", "720p", "720", "native", "source", "original":
		return "", nil
	case "1080", "1080p", "fhd", "hd", "full_hd", "fullhd":
		return "1080p", nil
	case "4k", "2160", "2160p", "uhd":
		return "4k", nil
	}
	return "", fmt.Errorf("unsupported resolution %q; use 720p, 1080p, or 4k", resolution)
}

// UpsampleVideo submits a second pass that renders a finished video at a higher
// resolution. Returns the media IDs of the upsampled output.
func (c *Client) UpsampleVideo(ctx context.Context, mediaID, aspect, resolution string, seed *int64, sceneID string) ([]string, error) {
	tier, err := NormalizeResolution(resolution)
	if err != nil {
		return nil, err
	}
	if tier == "" {
		return nil, fmt.Errorf("flow: upsampling to 720p is a no-op; Flow already generates at 720p")
	}

	modelKey := config.UpsampleModels[tier]
	if modelKey == "" {
		return nil, fmt.Errorf("flow: no upsampler configured for %s", tier)
	}

	aspectEnum, err := resolveAspect(aspect)
	if err != nil {
		return nil, err
	}

	candidates := resolutionCandidates(tier)
	var lastErr error

	for i, resolutionEnum := range candidates {
		item := map[string]any{
			"aspectRatio":   aspectEnum,
			"videoModelKey": modelKey,
			"seed":          resolveSeed(seed, 0),
			"metadata":      map[string]any{},
			"videoInput":    map[string]any{"mediaId": mediaID},
		}
		if sceneID != "" {
			item["metadata"] = map[string]any{"sceneId": sceneID}
		}
		if resolutionEnum != "" {
			item["resolution"] = resolutionEnum
		}

		body := map[string]any{
			"mediaGenerationContext": buildGenerationContext(""),
			"requests":               []map[string]any{item},
		}

		log.Printf("flow[%s]: upsampling %s to %s [%s]", c.opts.AccountID, truncate(mediaID, 12), tier, modelKey)

		result, err := c.Call(ctx, config.Endpoints["upsample_video"], body, recaptchaActionVideo)
		if err == nil {
			var parsed GenerateResponse
			if decodeErr := result.JSON(&parsed); decodeErr != nil {
				return nil, fmt.Errorf("flow: decode upsample response: %w", decodeErr)
			}
			ids := parsed.MediaIDs()
			if len(ids) == 0 {
				return nil, fmt.Errorf("flow: upsample accepted but returned no media")
			}
			log.Printf("flow[%s]: upsample submitted, %d media", c.opts.AccountID, len(ids))
			return ids, nil
		}

		lastErr = err
		if looksLikeSchemaRejection(err) && i+1 < len(candidates) {
			log.Printf("flow[%s]: upsample rejected the request shape (%v) — trying the next spelling", c.opts.AccountID, err)
			continue
		}
		return nil, err
	}

	return nil, lastErr
}

func resolutionCandidates(tier string) []string {
	configured := config.UpsampleResolutions[tier]
	ordered := make([]string, 0, 4)
	seen := map[string]bool{}
	add := func(v string) {
		if seen[v] {
			return
		}
		seen[v] = true
		ordered = append(ordered, v)
	}
	if configured != "" {
		add(configured)
	}
	for _, candidate := range upsampleResolutionCandidates[tier] {
		add(candidate)
	}
	return ordered
}

func looksLikeSchemaRejection(err error) bool {
	apiErr, ok := err.(*APIError)
	if !ok {
		return false
	}
	if apiErr.Status != 400 && apiErr.Status != 404 {
		return false
	}
	lower := strings.ToLower(apiErr.Message)
	for _, hint := range schemaRejectionHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

/* ------------------------------------------------------------------ *
 * Credits
 * ------------------------------------------------------------------ */

// Credits queries the account's remaining credit balance and subscription SKU.
//
// Only /v1/credits is used. A project-scoped variant
// (/v1/projects/{id}/credits) appears in some third-party notes but does not
// exist: it returns Google's generic HTML 404, so probing it just adds a wasted
// round trip and a misleading error.
func (c *Client) Credits(ctx context.Context) (credits int, sku string, err error) {
	result, err := c.Get(ctx, config.Endpoints["get_credits"], "")
	if err != nil {
		return 0, "", err
	}

	var parsed CreditsResponse
	if err := result.JSON(&parsed); err != nil {
		return 0, "", fmt.Errorf("flow: decode credits response: %w", err)
	}

	if n, convErr := parsed.Credits.Int64(); convErr == nil {
		credits = int(n)
	} else if n, convErr := parsed.RemainingCredits.Int64(); convErr == nil {
		credits = int(n)
	}

	c.mu.Lock()
	if parsed.Sku != "" {
		c.lastSku = parsed.Sku
	}
	sku = c.lastSku
	c.mu.Unlock()

	return credits, sku, nil
}

/* ------------------------------------------------------------------ *
 * Shared helpers
 * ------------------------------------------------------------------ */

// Captcha actions, re-exported so callers do not need the recaptcha package.
const (
	recaptchaActionVideo = "VIDEO_GENERATION"
	recaptchaActionImage = "IMAGE_GENERATION"
)

// promptParts is the inner prompt shape: {"parts":[{"text": "..."}]}.
func promptParts(prompt string) map[string]any {
	return map[string]any{
		"parts": []map[string]any{{"text": prompt}},
	}
}

// structuredPrompt is the video shape, which nests the parts one level deeper
// under a textInput block.
func structuredPrompt(prompt string) map[string]any {
	return map[string]any{
		"structuredPrompt": promptParts(prompt),
	}
}

func resolveAspect(aspect string) (string, error) {
	if aspect == "" {
		return config.Aspects["landscape"], nil
	}
	if enum, ok := config.Aspects[strings.ToLower(aspect)]; ok {
		return enum, nil
	}
	if strings.HasPrefix(aspect, "VIDEO_ASPECT_RATIO_") {
		return aspect, nil
	}
	return "", fmt.Errorf("unknown aspect %q; use portrait or landscape", aspect)
}

func resolveVideoModel(req VideoRequest) (string, error) {
	if req.ModelKey != "" {
		return req.ModelKey, nil
	}
	duration := req.Duration
	if duration == 0 {
		duration = config.DefaultDuration
	}
	model, ok := config.VideoModels[duration]
	if !ok {
		return "", fmt.Errorf("unsupported duration %ds; use one of %v", duration, config.Durations)
	}
	return model, nil
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}
