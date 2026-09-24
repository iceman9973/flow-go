package config

// Aspect ratios on the batchexecute transport.
//
// These are NOT the same values as Aspects / ImageAspects above. Those two hold
// the *legacy aisandbox* enum strings ("VIDEO_ASPECT_RATIO_PORTRAIT") and are
// read only by internal/flowapi, which is reachable only through
// Engine.SubmitVideo — a path with no callers. The tables here hold the
// *integers* the batchexecute payloads carry, which is a different wire format
// entirely. Do not merge the two: the strings are not translatable into these
// numbers, and a submission built from the wrong one is accepted by the server
// and renders nothing.
//
// The integers come from the Flow frontend bundle, where the video and image
// protobuf classes each expose a setter that writes one field, and the two
// enums are numbered differently — video calls landscape 2, image calls it 3.
// That difference is why there are two tables rather than one.

// Video aspect values, as written at the video request's aspect slot.
//
// The video request array is [prompt-block, model, mode, <aspect>, ...], so this
// is index 3. The code's own decoded layout for YhhmEf records that slot as a
// bare null on every capture — every capture was taken at the app's default,
// which is landscape, and landscape is also what the server reads a null as.
const (
	// VideoAspectSquare is 1:1. Documented by the bundle's mapping, but not
	// offered on the CLI: the video composer does not expose a square option.
	VideoAspectSquare = 0
	// VideoAspectPortrait is 9:16.
	VideoAspectPortrait = 1
	// VideoAspectLandscape is 16:9, and is the default.
	VideoAspectLandscape = 2
)

// VideoAspectRatios maps every spelling a caller may use to its wire value.
//
// The ratio aliases are accepted because they are what the Flow UI itself
// labels these options with, and a caller reading them off the screen should not
// have to translate.
var VideoAspectRatios = map[string]int{
	"portrait":  VideoAspectPortrait,
	"9:16":      VideoAspectPortrait,
	"landscape": VideoAspectLandscape,
	"16:9":      VideoAspectLandscape,
	"square":    VideoAspectSquare,
	"1:1":       VideoAspectSquare,
}

// VideoAspectNames lists the aspects the video CLI accepts, in the order they
// should be offered. Square is deliberately absent — see VideoAspectSquare.
var VideoAspectNames = []string{"landscape", "16:9", "portrait", "9:16"}

// Image aspect values, from the image protobuf class's own mapping.
//
// The image enum is numbered independently of the video one and disagrees with
// it: landscape is 3 here and 2 there. A single shared table would be wrong for
// one of the two.
const (
	ImageAspectSquare    = 1
	ImageAspectPortrait  = 2
	ImageAspectLandscape = 3
	ImageAspectThreeFour = 4
	ImageAspectFourThree = 5
)

// ImageAspectRatios maps every spelling a caller may use to its wire value.
var ImageAspectRatios = map[string]int{
	"square":    ImageAspectSquare,
	"1:1":       ImageAspectSquare,
	"portrait":  ImageAspectPortrait,
	"9:16":      ImageAspectPortrait,
	"landscape": ImageAspectLandscape,
	"16:9":      ImageAspectLandscape,
	"3:4":       ImageAspectThreeFour,
	"4:3":       ImageAspectFourThree,
}

// ImageAspectNames lists the aspects the image CLI accepts.
var ImageAspectNames = []string{"landscape", "16:9", "portrait", "9:16", "square", "1:1", "4:3", "3:4"}

// VideoAspectValue resolves a video --aspect value to its wire integer.
//
// The second result is false when the name is not a known aspect, so a caller
// can refuse rather than fall back to a default the user did not ask for. An
// empty name is not a name and is not looked up here — the caller decides what
// an absent flag means, because on this transport "absent" and "landscape" are
// different payloads even though the server renders both as landscape.
func VideoAspectValue(name string) (int, bool) {
	value, ok := VideoAspectRatios[normalizeAspectName(name)]
	return value, ok
}

// ImageAspectValue resolves an image --aspect value to its wire integer.
//
// Note that the image aspect slot itself is not yet placed in the payload; see
// the note in internal/batchexecute. This function is the mapping, which is
// known, kept separate from the slot, which is not.
func ImageAspectValue(name string) (int, bool) {
	value, ok := ImageAspectRatios[normalizeAspectName(name)]
	return value, ok
}

// normalizeAspectName lowercases and trims, so " 9:16 " and "9:16" are the same
// request. A caller pasting a value out of the UI should not have to know that
// the shell kept a space.
func normalizeAspectName(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			continue
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
