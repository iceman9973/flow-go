package config

import "testing"

// The video table, value by value. Pinned rather than spot-checked because a
// wrong integer here is not rejected by the server — it accepts the submission
// and renders nothing — so the only place the mistake can be caught cheaply is
// here.
func TestVideoAspectValues(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"portrait", VideoAspectPortrait},
		{"9:16", VideoAspectPortrait},
		{"landscape", VideoAspectLandscape},
		{"16:9", VideoAspectLandscape},
		{"square", VideoAspectSquare},
		{"1:1", VideoAspectSquare},
	}
	for _, tc := range cases {
		got, ok := VideoAspectValue(tc.name)
		if !ok {
			t.Errorf("VideoAspectValue(%q) did not recognise the name", tc.name)
			continue
		}
		if got != tc.want {
			t.Errorf("VideoAspectValue(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestImageAspectValues(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"square", ImageAspectSquare},
		{"1:1", ImageAspectSquare},
		{"portrait", ImageAspectPortrait},
		{"9:16", ImageAspectPortrait},
		{"landscape", ImageAspectLandscape},
		{"16:9", ImageAspectLandscape},
		{"3:4", ImageAspectThreeFour},
		{"4:3", ImageAspectFourThree},
	}
	for _, tc := range cases {
		got, ok := ImageAspectValue(tc.name)
		if !ok {
			t.Errorf("ImageAspectValue(%q) did not recognise the name", tc.name)
			continue
		}
		if got != tc.want {
			t.Errorf("ImageAspectValue(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The two enums disagree, and that disagreement is the reason there are two
// tables rather than one. A future reader who "simplifies" this into a single
// shared map will break one of the two transports, and this is the test that
// says so.
func TestTheTwoAspectEnumsDisagreeOnLandscape(t *testing.T) {
	video, _ := VideoAspectValue("landscape")
	image, _ := ImageAspectValue("landscape")
	if video == image {
		t.Errorf("landscape is %d for both video and image; the two enums are "+
			"independent and this table pair should not be merged", video)
	}
}

// The CLI takes whatever the caller typed, and the shell has no opinion about
// case or stray spaces. A value copied off the Flow UI's own label should work.
func TestAspectNamesAreNormalised(t *testing.T) {
	for _, name := range []string{"Portrait", "PORTRAIT", " portrait ", "\tportrait\n"} {
		got, ok := VideoAspectValue(name)
		if !ok {
			t.Errorf("VideoAspectValue(%q) did not recognise the name", name)
			continue
		}
		if got != VideoAspectPortrait {
			t.Errorf("VideoAspectValue(%q) = %d, want %d", name, got, VideoAspectPortrait)
		}
	}
}

// Empty is not a name, and it is deliberately not a synonym for landscape: the
// video payload distinguishes "no aspect" (null) from "landscape" (2), and the
// existing captured fixtures depend on that difference. The caller decides what
// an absent flag means; this lookup refuses to guess.
func TestAnEmptyAspectIsNotALookup(t *testing.T) {
	for _, name := range []string{"", "   "} {
		if got, ok := VideoAspectValue(name); ok {
			t.Errorf("VideoAspectValue(%q) answered (%d, true), want ok=false", name, got)
		}
		if got, ok := ImageAspectValue(name); ok {
			t.Errorf("ImageAspectValue(%q) answered (%d, true), want ok=false", name, got)
		}
	}
}

func TestUnknownAspectsAreRefused(t *testing.T) {
	for _, name := range []string{"widescreen", "cinema", "21:9", "potrait"} {
		if got, ok := VideoAspectValue(name); ok {
			t.Errorf("VideoAspectValue(%q) answered (%d, true), want ok=false", name, got)
		}
	}
}

// The help text and the validation must not drift: a value offered in the help
// that the lookup rejects is a documented option that fails, and the reverse is
// an option nobody can discover.
func TestEveryOfferedVideoAspectIsAccepted(t *testing.T) {
	for _, name := range VideoAspectNames {
		if _, ok := VideoAspectValue(name); !ok {
			t.Errorf("the help offers --aspect %q but the lookup rejects it", name)
		}
	}
}

func TestEveryOfferedImageAspectIsAccepted(t *testing.T) {
	for _, name := range ImageAspectNames {
		if _, ok := ImageAspectValue(name); !ok {
			t.Errorf("the help offers --aspect %q but the lookup rejects it", name)
		}
	}
}
