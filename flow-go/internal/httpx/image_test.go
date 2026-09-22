package httpx

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// The upload guard. Flow refuses an image whose longest edge is past its limit
// and never says so, which is why the check is here rather than in each caller.

func testImage(w, h int, opaque bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	a := uint8(255)
	if !opaque {
		a = 128
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: a})
		}
	}
	return img
}

func encodeAsPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func encodeAsJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// dimensionsOf reads back what NormalizeImage produced.
func dimensionsOf(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the normalised bytes are not a decodable image: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestAnImageWithinTheLimitIsPassedThroughUntouched(t *testing.T) {
	src := encodeAsPNG(t, testImage(640, 480, true))

	out, mimeType, err := NormalizeImage(src, "image/png")
	if err != nil {
		t.Fatalf("NormalizeImage: %v", err)
	}
	// Untouched means byte-identical, not merely equivalent: the common path must
	// not re-encode, or every upload would pay for a decode it did not need.
	if !bytes.Equal(out, src) {
		t.Error("an image inside the limit was modified")
	}
	if mimeType != "image/png" {
		t.Errorf("content type = %q, want the declared image/png", mimeType)
	}
}

func TestAnOversizedImageIsDownscaledToTheLimit(t *testing.T) {
	src := encodeAsJPEG(t, testImage(2500, 1500, true))

	out, mimeType, err := NormalizeImage(src, "image/jpeg")
	if err != nil {
		t.Fatalf("NormalizeImage: %v", err)
	}

	w, h := dimensionsOf(t, out)
	if w != MaxImageEdge || h != 1229 {
		t.Errorf("downscaled to %dx%d, want %dx1229", w, h, MaxImageEdge)
	}
	if mimeType != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", mimeType)
	}
}

// TestDownscalingKeepsTheAspectRatioOfATallImage is the case a max-width-only
// check gets wrong: the long edge here is the height.
func TestDownscalingKeepsTheAspectRatioOfATallImage(t *testing.T) {
	src := encodeAsPNG(t, testImage(1200, 3000, true))

	out, _, err := NormalizeImage(src, "image/png")
	if err != nil {
		t.Fatalf("NormalizeImage: %v", err)
	}

	w, h := dimensionsOf(t, out)
	if h != MaxImageEdge || w != 819 {
		t.Errorf("downscaled to %dx%d, want 819x%d", w, h, MaxImageEdge)
	}
}

// TestAPNGWithTransparencyStaysPNG guards the re-encode choice: JPEG has no
// alpha channel, so flattening a cut-out would fill its background with black.
func TestAPNGWithTransparencyStaysPNG(t *testing.T) {
	src := encodeAsPNG(t, testImage(2500, 1500, false))

	out, mimeType, err := NormalizeImage(src, "image/png")
	if err != nil {
		t.Fatalf("NormalizeImage: %v", err)
	}
	if mimeType != "image/png" {
		t.Errorf("content type = %q, want image/png — transparency must survive", mimeType)
	}
	if _, _, err := image.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("the re-encoded PNG does not decode: %v", err)
	}
}

func TestBytesThatAreNotAnImageAreRejected(t *testing.T) {
	_, _, err := NormalizeImage([]byte("this is not an image"), "image/png")
	if err == nil {
		t.Fatal("undecodable bytes were accepted")
	}
	// The message has to say what the bytes are not, and name what they claimed
	// to be — uploading them would produce a rejection that says nothing.
	if !strings.Contains(err.Error(), "not a decodable image") {
		t.Errorf("error %q should say the bytes are not an image", err)
	}
	if !strings.Contains(err.Error(), "image/png") {
		t.Errorf("error %q should name the declared content type", err)
	}
}

func TestAnEmptyImageIsRejected(t *testing.T) {
	_, _, err := NormalizeImage(nil, "image/png")
	if err == nil {
		t.Fatal("an empty image was accepted")
	}
	// Named specifically. The generic path would report empty bytes as
	// "undecodable", which sends the reader off to look at the file's format
	// when the real problem is that there is no file.
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q should say the image is empty", err)
	}
}

// TestTheWebPDecoderIsRegistered pins the blank import. Without it
// image.DecodeConfig reports "unknown format" and every WebP is rejected as
// undecodable — a regression nothing else here would notice.
//
// The fixture is a WebP container whose pixel payload is truncated, which is the
// point: the failure has to come from the WebP decoder, not from the dispatcher
// failing to recognise the format at all.
func TestTheWebPDecoderIsRegistered(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(4+8+4))
	buf.WriteString("WEBP")
	buf.WriteString("VP8L")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(4))
	buf.Write([]byte{0x2f, 0x00, 0x00, 0x00})

	// The declared type deliberately does not mention WebP. It is echoed into the
	// error message, so declaring it as "image/webp" would make the assertion
	// below pass whether or not a decoder ever saw the bytes — which is exactly
	// the false negative this test exists to avoid.
	_, _, err := NormalizeImage(buf.Bytes(), "application/octet-stream")
	if err == nil {
		t.Fatal("a truncated WebP must not normalise cleanly")
	}

	msg := strings.ToLower(err.Error())
	// "unknown format" is what the dispatcher says when no decoder claims the
	// magic bytes, so seeing it means the WebP decoder is not registered at all.
	// When it is registered the failure comes from the decoder instead — parsing
	// the truncated payload — and is a different error with no "webp" in it, so
	// the absence of this one is the signal to assert on.
	if strings.Contains(msg, "unknown format") {
		t.Errorf("image.DecodeConfig did not recognise the WebP container (%v) — "+
			"the golang.org/x/image/webp import is missing", err)
	}
}
