package config

import "testing"

// TestVideoModelFor pins the model each conditioning shape resolves to.
//
// The three cases need three different models *and* three different RPCs. A
// caller that supplies images and no model used to get the per-duration
// text-to-video key, which the server accepts and then generates nothing from —
// so this is a silent-failure guard, not a cosmetic one.
func TestVideoModelFor(t *testing.T) {
	cases := []struct {
		name             string
		duration         int
		hasStart, hasEnd bool
		want             string
	}{
		{"text, 4s", 4, false, false, "abra_t2v_4s"},
		{"text, 10s", 10, false, false, "abra_t2v_10s"},
		{"start only, 4s", 4, true, false, "abra_i2v_4s"},
		{"start only, 6s", 6, true, false, "abra_i2v_6s"},
		{"start only, 8s", 8, true, false, "abra_i2v_8s"},
		{"start only, 10s", 10, true, false, "abra_i2v_10s"},
		{"both frames, 4s", 4, true, true, "omni_flash_i2v_4s_first_last"},
		{"both frames, 8s", 8, true, true, "omni_flash_i2v_8s_first_last"},
		{"end frame only falls back to text", 8, false, false, "abra_t2v_8s"},
	}

	for _, tc := range cases {
		got, ok := VideoModelFor(tc.duration, tc.hasStart, tc.hasEnd)
		if !ok {
			t.Errorf("%s: no model for %ds", tc.name, tc.duration)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}

	// An unset duration means the default, not "unsupported".
	got, ok := VideoModelFor(0, true, false)
	if !ok {
		t.Fatalf("duration 0 should resolve to the default")
	}
	if want := ImageToVideoModels[DefaultDuration]; got != want {
		t.Errorf("duration 0: got %q, want %q", got, want)
	}

	// A duration outside the table is a real error and must be reported as one,
	// otherwise a typo in a request silently becomes a text-to-video submission.
	if _, ok := VideoModelFor(7, true, false); ok {
		t.Error("7s should not resolve to a model")
	}
}

// TestVideoModelForConditioningChangesTheModel is the point of the function: the
// same duration must not yield the same key for all three shapes.
func TestVideoModelForConditioningChangesTheModel(t *testing.T) {
	text, _ := VideoModelFor(8, false, false)
	start, _ := VideoModelFor(8, true, false)
	both, _ := VideoModelFor(8, true, true)

	if text == start || text == both || start == both {
		t.Fatalf("the three shapes must resolve to different models, got %q / %q / %q",
			text, start, both)
	}
}

// TestVideoModelTablesCoverTheSameDurations keeps the three tables in step. A
// duration present in one and missing from another would make VideoModelFor fail
// only for some conditioning shapes, which is exactly the kind of gap that is
// hard to notice.
func TestVideoModelTablesCoverTheSameDurations(t *testing.T) {
	for _, d := range Durations {
		if _, ok := VideoModels[d]; !ok {
			t.Errorf("VideoModels is missing %ds", d)
		}
		if _, ok := ImageToVideoModels[d]; !ok {
			t.Errorf("ImageToVideoModels is missing %ds", d)
		}
		if _, ok := FirstLastVideoModels[d]; !ok {
			t.Errorf("FirstLastVideoModels is missing %ds", d)
		}
		if _, ok := ReferenceVideoModels[d]; !ok {
			t.Errorf("ReferenceVideoModels is missing %ds", d)
		}
	}
}

// TestReferenceVideoModelFor covers the fourth conditioning shape, which has its
// own RPC and its own model table.
func TestReferenceVideoModelFor(t *testing.T) {
	for _, tc := range []struct {
		duration int
		want     string
	}{
		{4, "abra_r2v_4s"},
		{6, "abra_r2v_6s"},
		{8, "abra_r2v_8s"},
		{10, "abra_r2v_10s"},
	} {
		got, ok := ReferenceVideoModelFor(tc.duration)
		if !ok || got != tc.want {
			t.Errorf("%ds: got %q (ok=%v), want %q", tc.duration, got, ok, tc.want)
		}
	}

	if got, ok := ReferenceVideoModelFor(0); !ok || got != ReferenceVideoModels[DefaultDuration] {
		t.Errorf("duration 0: got %q (ok=%v), want the default", got, ok)
	}
	if _, ok := ReferenceVideoModelFor(7); ok {
		t.Error("7s should not resolve to a reference model")
	}

	// A reference model must not collide with any other shape's model: the model
	// key is what selects the behaviour, so a duplicate would silently switch
	// conditioning.
	ref, _ := ReferenceVideoModelFor(8)
	for _, other := range []string{VideoModels[8], ImageToVideoModels[8], FirstLastVideoModels[8]} {
		if ref == other {
			t.Errorf("the reference model for 8s collides with %q", other)
		}
	}
}

// TestVideoCostsMatchTheMeasuredTable pins the numbers to what the app charges.
//
// They came from the user rather than from the code, and they are the reason a
// submission can be accepted and produce nothing: the server does not refuse a
// render the balance cannot cover, it just returns no media. Without the table the
// engine cannot tell that apart from a wrong argument, and it reported both as
// "submitted 0 videos".
func TestVideoCostsMatchTheMeasuredTable(t *testing.T) {
	for _, tc := range []struct {
		duration int
		quality  string
		want     int
	}{
		{4, "360p", 4}, {4, "720p", 7},
		{6, "360p", 5}, {6, "720p", 10},
		{8, "360p", 6}, {8, "720p", 12},
		{10, "360p", 7}, {10, "720p", 15},
	} {
		got, ok := VideoCost(tc.duration, tc.quality)
		if !ok {
			t.Errorf("VideoCost(%d, %q) is not recorded", tc.duration, tc.quality)
			continue
		}
		if got != tc.want {
			t.Errorf("VideoCost(%d, %q) = %d, want %d", tc.duration, tc.quality, got, tc.want)
		}
	}

	// The 360p render has to be cheaper at every duration, or the option is not
	// worth offering to an account that cannot afford the default.
	for duration := range VideoCosts {
		cheap, _ := VideoCost(duration, "360p")
		dear, _ := VideoCost(duration, "720p")
		if cheap >= dear {
			t.Errorf("%ds: 360p costs %d and 720p costs %d — the cheap option is not cheaper",
				duration, cheap, dear)
		}
	}

	// An unknown pair is reported as unknown rather than as free.
	if _, ok := VideoCost(7, "720p"); ok {
		t.Error("a duration with no entry must report the cost as unknown")
	}
}

// TestVideoModelQualityAppliesTheSuffix pins the only thing that changes between
// the two qualities.
func TestVideoModelQualityAppliesTheSuffix(t *testing.T) {
	for _, tc := range []struct{ base, quality, want string }{
		{"abra_t2v_4s", "360p", "abra_t2v_4s_360p"},
		{"abra_t2v_4s", "720p", "abra_t2v_4s"},
		{"abra_t2v_4s", "", "abra_t2v_4s"},
		{"abra_t2v_4s", "anything-else", "abra_t2v_4s"},
		{"abra_i2v_8s", "360p", "abra_i2v_8s_360p"},
		// Case and padding should not decide whether the cheap render is used.
		{"abra_t2v_6s", " 360P ", "abra_t2v_6s_360p"},
	} {
		if got := VideoModelQuality(tc.base, tc.quality); got != tc.want {
			t.Errorf("VideoModelQuality(%q, %q) = %q, want %q", tc.base, tc.quality, got, tc.want)
		}
	}
}
