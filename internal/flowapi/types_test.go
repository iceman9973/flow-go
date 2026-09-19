package flowapi

import (
	"errors"
	"fmt"
	"testing"
)

// TestUnauthenticatedCoversNoFlowKey is the regression test for the defect that
// deadlocked the Python engine.
//
// The old _is_unauthenticated() only matched a bare 401. Once the extension
// self-healed a 401 it cleared its own token, so every later call came back as a
// generic 503 NO_FLOW_KEY — which the check did not recognise, so the
// force-refresh path never ran and generation stayed broken until a restart.
//
// Every shape an expired credential can take must classify as unauthenticated.
func TestUnauthenticatedCoversNoFlowKey(t *testing.T) {
	cases := []struct {
		name string
		err  *APIError
		want bool
	}{
		{
			name: "bare 401",
			err:  &APIError{Status: 401, Message: "Unauthorized"},
			want: true,
		},
		{
			name: "503 with NO_FLOW_KEY message",
			err:  &APIError{Status: 503, Message: "NO_FLOW_KEY"},
			want: true,
		},
		{
			name: "503 with lowercase no_flow_key",
			err:  &APIError{Status: 503, Message: "no_flow_key"},
			want: true,
		},
		{
			name: "503 UNAUTHENTICATED",
			err:  &APIError{Status: 503, Message: "UNAUTHENTICATED"},
			want: true,
		},
		{
			name: "403 with UNAUTHENTICATED status code",
			err:  &APIError{Status: 403, Message: "nope", Code: "UNAUTHENTICATED"},
			want: true,
		},
		{
			name: "403 with AUTH_ERROR reason",
			err:  &APIError{Status: 403, Message: "nope", Reason: "AUTH_ERROR"},
			want: true,
		},
		{
			name: "403 with expired-token wording",
			err:  &APIError{Status: 403, Message: "Token has been expired or revoked."},
			want: true,
		},
		{
			name: "407 proxy auth",
			err:  &APIError{Status: 407, Message: "Proxy Authentication Required"},
			want: true,
		},
		{
			name: "generic 503 is NOT an auth failure",
			err:  &APIError{Status: 503, Message: "Service temporarily unavailable"},
			want: false,
		},
		{
			name: "403 quota is not an auth failure",
			err:  &APIError{Status: 403, Message: "Quota exceeded", Reason: "QUOTA_EXCEEDED"},
			want: false,
		},
		{
			name: "400 bad request is not an auth failure",
			err:  &APIError{Status: 400, Message: "Invalid value for resolution"},
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Unauthenticated(); got != tc.want {
				t.Errorf("Unauthenticated() = %v, want %v (err: %v)", got, tc.want, tc.err)
			}
		})
	}
}

// TestIsUnauthenticatedThroughWrapping checks the package-level helper unwraps
// the error chain, since callers see a wrapped error rather than the concrete
// type.
func TestIsUnauthenticatedThroughWrapping(t *testing.T) {
	inner := &APIError{Status: 503, Message: "NO_FLOW_KEY"}
	wrapped := fmt.Errorf("flow: submit failed: %w", inner)

	if !IsUnauthenticated(wrapped) {
		t.Error("IsUnauthenticated should unwrap and detect the auth failure")
	}

	plain := errors.New("connection reset")
	if IsUnauthenticated(plain) {
		t.Error("IsUnauthenticated should not fire on a transport error")
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		err  *APIError
		want bool
	}{
		{&APIError{Status: 429}, true},
		{&APIError{Status: 500}, true},
		{&APIError{Status: 502}, true},
		{&APIError{Status: 503, Message: "upstream down"}, true},
		{&APIError{Status: 504}, true},
		{&APIError{Status: 403, Reason: "UNUSUAL_ACTIVITY"}, true},
		{&APIError{Status: 400, Message: "bad prompt"}, false},
		{&APIError{Status: 403, Reason: "FORBIDDEN"}, false},
	}

	for _, tc := range cases {
		if got := tc.err.Retryable(); got != tc.want {
			t.Errorf("Retryable() for %v = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestOutOfCredits(t *testing.T) {
	cases := []struct {
		err  *APIError
		want bool
	}{
		{&APIError{Status: 402, Message: "Not enough credits: 0 left, but a 4s video costs 7"}, true},
		{&APIError{Status: 403, Reason: "NO_CREDITS"}, true},
		{&APIError{Status: 400, Message: "Invalid aspect ratio"}, false},
	}

	for _, tc := range cases {
		if got := tc.err.OutOfCredits(); got != tc.want {
			t.Errorf("OutOfCredits() for %v = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestMediaIDs covers both response shapes: the modern `media` array and the
// legacy `operations` array the upsampler still returns.
func TestMediaIDs(t *testing.T) {
	resp := &GenerateResponse{
		Media:      []MediaItem{{Name: "media-a"}, {Name: "media-b"}, {Name: ""}},
		Operations: []MediaItem{{Name: "media-b"}, {Name: "media-c"}},
	}

	ids := resp.MediaIDs()
	want := []string{"media-a", "media-b", "media-c"}

	if len(ids) != len(want) {
		t.Fatalf("MediaIDs() = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("MediaIDs()[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestTerminalStatuses(t *testing.T) {
	cases := []struct {
		status MediaStatus
		term   bool
		ok     bool
	}{
		{MediaStatus{StatusSuccessful}, true, true},
		{MediaStatus{"MEDIA_GENERATION_STATUS_FAILED"}, true, false},
		{MediaStatus{"MEDIA_GENERATION_STATUS_BLOCKED"}, true, false},
		{MediaStatus{"MEDIA_GENERATION_STATUS_PENDING"}, false, false},
		{MediaStatus{""}, false, false},
	}

	for _, tc := range cases {
		if got := tc.status.Terminal(); got != tc.term {
			t.Errorf("Terminal() for %q = %v, want %v", tc.status.MediaGenerationStatus, got, tc.term)
		}
		if got := tc.status.Succeeded(); got != tc.ok {
			t.Errorf("Succeeded() for %q = %v, want %v", tc.status.MediaGenerationStatus, got, tc.ok)
		}
	}
}

// TestNormalizeResolution checks the upscale tier mapping, including the native
// 720p no-op case.
func TestNormalizeResolution(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"720p", "", false},
		{"native", "", false},
		{"1080p", "1080p", false},
		{"1080", "1080p", false},
		{"FHD", "1080p", false},
		{"4k", "4k", false},
		{"2160p", "4k", false},
		{"UHD", "4k", false},
		{"1440p", "", true},
	}

	for _, tc := range cases {
		got, err := NormalizeResolution(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeResolution(%q) expected an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeResolution(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeResolution(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveAspect(t *testing.T) {
	if got, err := resolveAspect("landscape"); err != nil || got != "VIDEO_ASPECT_RATIO_LANDSCAPE" {
		t.Errorf("resolveAspect(landscape) = %q, %v", got, err)
	}
	if got, err := resolveAspect(""); err != nil || got != "VIDEO_ASPECT_RATIO_LANDSCAPE" {
		t.Errorf("resolveAspect(empty) should default to landscape, got %q, %v", got, err)
	}
	if got, err := resolveAspect("VIDEO_ASPECT_RATIO_PORTRAIT"); err != nil || got != "VIDEO_ASPECT_RATIO_PORTRAIT" {
		t.Errorf("resolveAspect should pass enum values through, got %q, %v", got, err)
	}
	if _, err := resolveAspect("diagonal"); err == nil {
		t.Error("resolveAspect should reject an unknown aspect")
	}
}

func TestResolveVideoModel(t *testing.T) {
	for duration, want := range map[int]string{
		4: "abra_t2v_4s", 6: "abra_t2v_6s", 8: "abra_t2v_8s", 10: "abra_t2v_10s",
	} {
		got, err := resolveVideoModel(VideoRequest{Duration: duration})
		if err != nil {
			t.Fatalf("duration %d: unexpected error: %v", duration, err)
		}
		if got != want {
			t.Errorf("duration %d model = %q, want %q", duration, got, want)
		}
	}

	if _, err := resolveVideoModel(VideoRequest{Duration: 7}); err == nil {
		t.Error("an unsupported duration should error")
	}

	if got, err := resolveVideoModel(VideoRequest{Duration: 4, ModelKey: "custom_model"}); err != nil || got != "custom_model" {
		t.Errorf("an explicit ModelKey should win, got %q, %v", got, err)
	}
}

// TestResolveSeedReproducible checks that an explicit seed is stable and that
// batch members are offset from one another.
func TestResolveSeedReproducible(t *testing.T) {
	seed := int64(12345)

	a := resolveSeed(&seed, 0)
	b := resolveSeed(&seed, 0)
	if a != b {
		t.Errorf("the same seed and index should be reproducible: %d != %d", a, b)
	}

	c := resolveSeed(&seed, 1)
	if c == a {
		t.Error("different indices in a batch should produce different seeds")
	}

	// A nil seed must vary.
	x := resolveSeed(nil, 0)
	y := resolveSeed(nil, 0)
	if x == y {
		t.Log("note: two random seeds collided, which is possible but unlikely")
	}
	if x < 1 || x > 9999 {
		t.Errorf("random seed %d is outside the expected 1..9999 range", x)
	}
}

func TestErrorBodyReason(t *testing.T) {
	body := &ErrorBody{
		Message: "Request blocked",
		Details: []ErrorDetail{{Type: "type.googleapis.com/ErrorInfo"}, {Reason: "UNUSUAL_ACTIVITY"}},
	}
	if got := body.Reason(); got != "UNUSUAL_ACTIVITY" {
		t.Errorf("Reason() = %q, want UNUSUAL_ACTIVITY", got)
	}

	var nilBody *ErrorBody
	if got := nilBody.Reason(); got != "" {
		t.Errorf("Reason() on nil should be empty, got %q", got)
	}
}
