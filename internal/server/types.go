package server

// VideoGenerationRequest is the body of POST /v1/videos/generations.
type VideoGenerationRequest struct {
	Prompt string `json:"prompt"`
	// Model overrides the video key (e.g. "abra_t2v_8s"). Empty derives it from
	// Duration.
	Model  string `json:"model,omitempty"`
	Aspect string `json:"aspect"`
	// Quality is "360p" or "720p" (the default). It is not cosmetic: a 4s render
	// costs 4 credits at 360p against 7 at 720p, so on a thin balance the cheap
	// one is the difference between a render and a refusal.
	Quality string `json:"quality,omitempty"`
	// Duration is seconds; it selects the model when Model is empty.
	Duration int    `json:"duration"`
	Count    int    `json:"count"`
	Seed     *int64 `json:"seed,omitempty"`

	// Image inputs. Each may be a local file path or an existing media ID.
	StartImage      string   `json:"start_image,omitempty"`
	EndImage        string   `json:"end_image,omitempty"`
	ReferenceImages []string `json:"reference_images,omitempty"`
	// StartFrame and EndFrame are the crop values the app sends next to a
	// condition image, as three normalised numbers. The app always sends them and
	// a submission without them comes back empty, so they are required whenever
	// the matching image is set — the meaning of the numbers is not documented.
	StartFrame []float64 `json:"start_frame,omitempty"`
	EndFrame   []float64 `json:"end_frame,omitempty"`

	// Resolution: "", "720p", "1080p", or "4k". Anything above 720p runs a
	// second upsampler pass over the finished video.
	Resolution string `json:"resolution,omitempty"`

	// Download writes the finished media to output/.
	Download *bool `json:"download,omitempty"`

	// Wait blocks until the generation finishes. When false the request returns
	// as soon as Flow accepts the job.
	Wait *bool `json:"wait,omitempty"`

	AudioPreference string `json:"audio_preference,omitempty"`
}

// ImageGenerationRequest is the body of POST /v1/images/generations.
type ImageGenerationRequest struct {
	Prompt       string   `json:"prompt"`
	Aspect       string   `json:"aspect"`
	Count        int      `json:"count"`
	Seed         *int64   `json:"seed,omitempty"`
	Model        string   `json:"model,omitempty"`
	ReferenceIDs []string `json:"reference_images,omitempty"`
	Download     *bool    `json:"download,omitempty"`
}

// VideoEditRequest is the body of POST /v1/videos/edit.
//
// Source is the asset to edit — a media id or a content id; a media id is
// resolved first. Model defaults to abra_edit, which is the video-to-video model
// the app reaches from its Ingredients composer mode.
type VideoEditRequest struct {
	Source    string `json:"source"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Wait      *bool  `json:"wait,omitempty"`
	Download  *bool  `json:"download,omitempty"`
}

// VideoReferenceRequest is the body of POST /v1/videos/reference.
//
// References are the assets to condition on — media ids or content ids; media ids
// are resolved first. Model defaults to the abra_r2v_* key for Duration.
type VideoReferenceRequest struct {
	References []string `json:"references"`
	Prompt     string   `json:"prompt"`
	Model      string   `json:"model,omitempty"`
	Duration   int      `json:"duration,omitempty"`
	ProjectID  string   `json:"project_id,omitempty"`
	Wait       *bool    `json:"wait,omitempty"`
	Download   *bool    `json:"download,omitempty"`
}

// CookieSyncRequest is the body of POST /api/sync-cookies, the fallback used
// when an operator prefers to push a cookie dump directly rather than run the
// extension.
type CookieSyncRequest struct {
	Cookies []CookiePayload `json:"cookies"`
}

// CookiePayload mirrors a chrome.cookies object.
type CookiePayload struct {
	Domain         string  `json:"domain"`
	ExpirationDate float64 `json:"expirationDate,omitempty"`
	HostOnly       bool    `json:"hostOnly,omitempty"`
	HTTPOnly       bool    `json:"httpOnly,omitempty"`
	Name           string  `json:"name"`
	Path           string  `json:"path"`
	SameSite       string  `json:"sameSite,omitempty"`
	Secure         bool    `json:"secure,omitempty"`
	Session        bool    `json:"session,omitempty"`
	StoreID        string  `json:"storeId,omitempty"`
	Value          string  `json:"value"`
}
