package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSniffImageType covers the two formats the RPC returns. The download is
// saved with an extension derived from this, so a wrong answer writes a JPEG
// with a .png name.
func TestSniffImageType(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}, "image/jpeg"},
		{"png", []byte("\x89PNG\r\n\x1a\nrest"), "image/png"},
		{"unknown", []byte("not an image"), "application/octet-stream"},
		{"empty", nil, "application/octet-stream"},
		{"truncated jpeg", []byte{0xFF, 0xD8}, "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sniffImageType(tc.data); got != tc.want {
				t.Errorf("sniffImageType(%v) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

// TestUpscaleExpressionEmbedsConfig guards the interpolation: the expression is
// built by concatenating JSON onto a script literal, so a value that is not
// JSON-encoded would break the page-side call rather than fail to compile here.
func TestUpscaleExpressionEmbedsConfig(t *testing.T) {
	expr := upscaleExpression(
		"proj-1", "media-1", "content-1", Resolution2K, "boq_labs-ai-sandbox-frontend_20260917.00_p0")

	// The config object must be the first thing the script reads.
	start := strings.Index(expr, "const cfg = ")
	if start < 0 {
		t.Fatal("the expression does not define cfg")
	}
	rest := expr[start+len("const cfg = "):]
	end := strings.Index(rest, ";")
	if end < 0 {
		t.Fatal("the cfg literal is not terminated")
	}

	var cfg struct {
		SiteKey    string `json:"siteKey"`
		Action     string `json:"action"`
		ContentID  string `json:"contentID"`
		ProjectID  string `json:"projectID"`
		MediaID    string `json:"mediaID"`
		Resolution int    `json:"resolution"`
		BuildLabel string `json:"buildLabel"`
	}
	if err := json.Unmarshal([]byte(rest[:end]), &cfg); err != nil {
		t.Fatalf("the cfg literal is not valid JSON: %v", err)
	}

	if cfg.ContentID != "content-1" || cfg.ProjectID != "proj-1" || cfg.MediaID != "media-1" {
		t.Errorf("the ids did not survive: %+v", cfg)
	}
	if cfg.Resolution != Resolution2K {
		t.Errorf("resolution = %d, want %d", cfg.Resolution, Resolution2K)
	}
	if cfg.Action != "IMAGE_GENERATION" {
		t.Errorf("action = %q; the app scopes this call to IMAGE_GENERATION", cfg.Action)
	}
	if cfg.SiteKey == "" {
		t.Error("the site key is empty")
	}
	if cfg.BuildLabel == "" {
		t.Error("the build label is empty")
	}

	// The RPC id and the captcha slot are the parts a refactor would silently drop.
	if !strings.Contains(expr, "'SPrCad'") {
		t.Error("the expression does not call SPrCad")
	}
	if !strings.Contains(expr, "[token, 1]") {
		t.Error("the expression does not place the captcha in the context block")
	}
}

// TestUpscaleExpressionMatchesTheRequestTheAppExpects pins the parts of the
// request the upstream is strict about, and that a refactor would quietly drop.
//
// The same request exists a second time in the browser, in
// flow-go-extension/background.js (`runUpscale`), because the page is the only
// client the upstream accepts for this RPC. Two implementations of one wire
// contract drift unless both are pinned, so
// `flow-go-extension/test/dispatch.test.mjs` asserts the identical invariants
// against the extension's actual request. Change one, and the other's test fails.
func TestUpscaleExpressionMatchesTheRequestTheAppExpects(t *testing.T) {
	expr := upscaleExpression("proj-1", "media-1", "content-1", Resolution2K, "bl-1")

	required := []struct {
		fragment string
		why      string
	}{
		{"/_/AiSandboxAngularFrontend/data/batchexecute",
			"the batchexecute path — relative, so the page makes the call and not the Go transport"},
		{"rpcids=SPrCad", "the RPC id"},
		{"'/project/' + cfg.projectID", "the source path names the project, and only the project"},
		{"&hl=", "the language parameter the app sends"},
		{"&rt=c", "the response-type parameter the app sends"},
		{"'X-Same-Domain': '1'", "batchexecute requires it"},
		{"credentials: 'include'", "the call is authenticated by the page's own cookies"},
		{"'generic'", "the frame's fourth slot"},
		// Argument 0 is the media id, not the content id. This is the opposite of
		// the generation calls, which take content ids in startImage/endImage.
		// Taken from a live capture of the app's own SPrCad request: it carried
		// the media id of the tile being upscaled. Sending the content id instead
		// returns a null payload rather than an error, which is how it presented.
		{"[cfg.mediaID, cfg.resolution", "argument 0 and 1: the media id and the resolution selector"},
		{"[null, 22, null, null, null, cfg.projectID", "argument 2: the context block, tool id 22 then the project"},
		{"[token, 1]", "the captcha pair that closes the context block"},
		{"PUBLIC_ERROR", "an upstream refusal is surfaced rather than parsed as an image"},
	}

	for _, r := range required {
		if !strings.Contains(expr, r.fragment) {
			t.Errorf("the expression no longer contains %q — %s", r.fragment, r.why)
		}
	}

	// The editor route must not appear: the app's own request names the project
	// and carries the media id in arg[0] instead.
	if strings.Contains(expr, "/edit/") {
		t.Error("the source path still names the editor route; the app's request does not")
	}
	if strings.Contains(expr, "[cfg.contentID, cfg.resolution") {
		t.Error("argument 0 is still the content id; the app sends the media id there")
	}
}

// TestResolutionSelectors pins the values that were measured. 1 is what the
// "2K | Upscaled" menu item sends; 2 is what "4K | Upscaled" sends.
func TestResolutionSelectors(t *testing.T) {
	if Resolution2K != 1 {
		t.Errorf("Resolution2K = %d, want 1 (captured from the 2K menu item)", Resolution2K)
	}
	if Resolution4K != 2 {
		t.Errorf("Resolution4K = %d, want 2 (captured from the 4K menu item)", Resolution4K)
	}
}
