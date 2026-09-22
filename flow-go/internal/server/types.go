package server

// The request types below are split into two groups, and the split is the point
// of them: the fields the batchexecute transport implements, and the fields it
// does not.
//
// The second group is declared rather than deleted on purpose. A field that is
// absent from the type is dropped by the JSON decoder without a word, so a
// caller migrating from the legacy aisandbox surface would get a 200, a result
// that quietly ignored half of what they asked for, and nothing to explain it.
// Declaring the field is what makes it possible to name it back to them in the
// response — which is what the `ignored` list is for.
//
// These fields used to be refused with a 400 instead. That failed a request with
// nothing else wrong with it, and the caller's next move — drop the field and
// retry — was one the server could take for them. What the 400 was protecting
// against was a *silent* substitution, and naming the field in the response
// answers that without the failure.

// ignoredOption is a request field the transport does not implement, and what to
// do instead when there is a real alternative.
type ignoredOption struct {
	Field string `json:"field"`
	// Hint names the endpoint or flag that does implement this, when one exists.
	// Empty when the field simply has no equivalent — the difference matters,
	// because "use POST /v1/videos/reference" is actionable and "not supported"
	// is only informative.
	Hint string `json:"hint,omitempty"`
}

// VideoGenerationRequest is the body of POST /v1/videos/generations.
//
// Implemented: Prompt, Model, Quality, Duration, Count, StartImage, EndImage,
// StartFrame, EndFrame, Download, Wait.
type VideoGenerationRequest struct {
	Prompt string `json:"prompt"`
	// Model overrides the video key (e.g. "abra_t2v_8s"). Empty derives it from
	// Duration.
	Model string `json:"model,omitempty"`
	// Quality is "360p" or "720p" (the default). It is not cosmetic: a 4s render
	// costs 4 credits at 360p against 7 at 720p, so on a thin balance the cheap
	// one is the difference between a render and a refusal.
	Quality string `json:"quality,omitempty"`
	// Duration is seconds; it selects the model when Model is empty.
	Duration int `json:"duration"`
	Count    int `json:"count"`

	// Image inputs. Each may be a local file path or an existing media ID.
	StartImage string `json:"start_image,omitempty"`
	EndImage   string `json:"end_image,omitempty"`
	// StartFrame and EndFrame are the crop values the app sends next to a
	// condition image, as three normalised numbers. The app always sends them and
	// a submission without them comes back empty, so they are required whenever
	// the matching image is set — the meaning of the numbers is not documented.
	StartFrame []float64 `json:"start_frame,omitempty"`
	EndFrame   []float64 `json:"end_frame,omitempty"`

	// Download writes the finished media to output/.
	Download *bool `json:"download,omitempty"`

	// Wait blocks until the generation finishes. When false the request returns
	// as soon as Flow accepts the job.
	Wait *bool `json:"wait,omitempty"`

	/* Accepted and reported as ignored — see ignoredVideoOptions. */

	// Aspect has no equivalent: the model key decides the frame, and the
	// image-conditioned models take theirs from the condition image.
	Aspect string `json:"aspect,omitempty"`
	// Seed is not carried by the batchexecute submission.
	Seed *int64 `json:"seed,omitempty"`
	// ReferenceImages is implemented, but at POST /v1/videos/reference rather
	// than here — a different RPC with a different payload arrangement. The hint
	// in the response says so.
	ReferenceImages []string `json:"reference_images,omitempty"`
	// Resolution is not a knob on this transport. 720p is what Flow renders, and
	// the model key encodes it; there is no second pass to ask for.
	Resolution string `json:"resolution,omitempty"`
	// AudioPreference is a legacy-surface field with no batchexecute equivalent.
	AudioPreference string `json:"audio_preference,omitempty"`
}

// ImageGenerationRequest is the body of POST /v1/images/generations.
//
// Implemented: Prompt, Model, Download.
type ImageGenerationRequest struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model,omitempty"`
	// Download writes the finished media to output/.
	Download *bool `json:"download,omitempty"`

	/* Accepted and reported as ignored — see ignoredImageOptions. */

	// Count is accepted but only 1 is honoured: the image RPC takes one prompt
	// and returns one asset. The hint in the response says to submit twice.
	Count int `json:"count"`
	// Aspect has no equivalent on the image RPC.
	Aspect string `json:"aspect,omitempty"`
	// Seed is not carried by the batchexecute submission.
	Seed *int64 `json:"seed,omitempty"`
	// ReferenceIDs has no equivalent for images.
	ReferenceIDs []string `json:"reference_images,omitempty"`
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
