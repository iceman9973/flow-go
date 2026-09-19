package server

import "testing"

// TestImageResolution covers the menu-label mapping.
//
// "1K | Original size" is deliberately an error: it is the asset's existing
// size, and answering it with an upscale would hand back a 2K file for a request
// that asked for 1K.
func TestImageResolution(t *testing.T) {
	ok := []struct {
		label string
		want  int
		name  string
	}{
		{"", 1, "2K"},
		{"2K", 1, "2K"},
		{"2k", 1, "2K"},
		{" 2K ", 1, "2K"},
		{"4K", 2, "4K"},
		{"4k", 2, "4K"},
	}
	for _, tc := range ok {
		got, name, err := imageResolution(tc.label)
		if err != nil {
			t.Errorf("imageResolution(%q) returned an error: %v", tc.label, err)
			continue
		}
		if got != tc.want || name != tc.name {
			t.Errorf("imageResolution(%q) = (%d, %q), want (%d, %q)", tc.label, got, name, tc.want, tc.name)
		}
	}

	for _, label := range []string{"1K", "1", "original", "8K", "huge"} {
		if _, _, err := imageResolution(label); err == nil {
			t.Errorf("imageResolution(%q) should have been rejected", label)
		}
	}
}

// TestShortID keeps filenames from carrying a whole media id.
func TestShortID(t *testing.T) {
	if got := shortID("8e637e6c-3ddc-4976-8b2d-f512527b4419"); got != "8e637e6c" {
		t.Errorf("shortID = %q, want %q", got, "8e637e6c")
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("shortID of a short id = %q, want %q", got, "abc")
	}
}

// TestUnsupportedImageOptions covers the fields the HTTP request type accepts but
// the batchexecute transport does not implement.
//
// This is a contract test, not a formality: dropping these silently returns a 200
// with a different result than the caller asked for — count 4 yields one image —
// which is the failure mode that is hardest to notice from the outside.
func TestUnsupportedImageOptions(t *testing.T) {
	seed := int64(7)
	cases := []struct {
		name string
		req  ImageGenerationRequest
		want []string
	}{
		{"plain", ImageGenerationRequest{Prompt: "p"}, nil},
		{"count 1 is the default", ImageGenerationRequest{Prompt: "p", Count: 1}, nil},
		{"count", ImageGenerationRequest{Prompt: "p", Count: 4}, []string{"count"}},
		{"aspect", ImageGenerationRequest{Prompt: "p", Aspect: "16:9"}, []string{"aspect"}},
		{"seed", ImageGenerationRequest{Prompt: "p", Seed: &seed}, []string{"seed"}},
		{"references", ImageGenerationRequest{Prompt: "p", ReferenceIDs: []string{"a"}}, []string{"reference_images"}},
		{"several", ImageGenerationRequest{Prompt: "p", Count: 2, Aspect: "9:16"},
			[]string{"count", "aspect"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unsupportedImageOptions(tc.req)
			if len(got) != len(tc.want) {
				t.Fatalf("unsupportedImageOptions = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("option %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestUnsupportedVideoOptions pins which fields are refused.
//
// `model`, `count`, `start_image` and `end_image` are deliberately absent: the
// engine accepts all of them, so they are forwarded rather than rejected. `model`
// was previously accepted by the request type and dropped by the handler, which
// made it silently inert; the condition images were refused until their payload
// layout was captured and implemented.
func TestUnsupportedVideoOptions(t *testing.T) {
	seed := int64(7)
	cases := []struct {
		name string
		req  VideoGenerationRequest
		want int
	}{
		{"plain", VideoGenerationRequest{Prompt: "p", Duration: 4}, 0},
		{"model is supported", VideoGenerationRequest{Prompt: "p", Model: "abra_t2v_8s"}, 0},
		{"count is supported", VideoGenerationRequest{Prompt: "p", Count: 3}, 0},
		{"aspect", VideoGenerationRequest{Prompt: "p", Aspect: "9:16"}, 1},
		{"resolution", VideoGenerationRequest{Prompt: "p", Resolution: "1080p"}, 1},
		{"seed", VideoGenerationRequest{Prompt: "p", Seed: &seed}, 1},
		// Condition images are supported: the payload layout for them was captured
		// from the app and implemented, so they are forwarded, not refused.
		{"start image", VideoGenerationRequest{Prompt: "p", StartImage: "m"}, 0},
		{"end image", VideoGenerationRequest{Prompt: "p", EndImage: "m"}, 0},
		{"both images", VideoGenerationRequest{Prompt: "p", StartImage: "m", EndImage: "n"}, 0},
		{"references", VideoGenerationRequest{Prompt: "p", ReferenceImages: []string{"m"}}, 1},
		{"audio", VideoGenerationRequest{Prompt: "p", AudioPreference: "music"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unsupportedVideoOptions(tc.req); len(got) != tc.want {
				t.Errorf("unsupportedVideoOptions = %v, want %d option(s)", got, tc.want)
			}
		})
	}
}
