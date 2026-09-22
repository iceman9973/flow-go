// Image handling for the upload paths.
//
// This is the one place that knows Flow refuses an image whose longest edge is
// past its limit. It lives beside the HTTP client rather than in a protocol
// package because both transports need it — `flowapi` for the legacy upload and
// `batchexecute` for the batch one — and a second copy would be the copy that
// drifts.
package httpx

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log"
	"math"
	"strings"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WebP decoder with image.Decode
)

// MaxImageEdge is the longest edge Flow accepts on an uploaded image.
//
// Past it the upload is refused, and refused in a way that never mentions size —
// so an oversized image reads as a malformed request rather than as one that was
// simply too big. Bounding it locally turns that into a named outcome.
const MaxImageEdge = 2048

// jpegQuality is high enough that the downscale does not visibly add to
// whatever the original had already lost.
const jpegQuality = 92

// NormalizeImage bounds an image so Flow will accept it.
//
// Returns the bytes to upload and their content type. An image already within
// the limit comes back untouched — the common path costs one header parse and no
// re-encode — so this is safe to call on every upload.
//
// Bytes that are not a decodable image are an error, never a pass-through.
// Uploading something Flow cannot read produces a rejection that says nothing
// about the file, and the caller is the last party still able to name it.
func NormalizeImage(data []byte, mimeType string) ([]byte, string, error) {
	if len(data) == 0 {
		return nil, "", fmt.Errorf("the image is empty")
	}

	// DecodeConfig reads only the header, so a correctly sized image — the usual
	// case — never pays for its pixels.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("not a decodable image (declared as %q): %w",
			strings.TrimSpace(mimeType), err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", fmt.Errorf("the image reports a %dx%d size", cfg.Width, cfg.Height)
	}
	if cfg.Width <= MaxImageEdge && cfg.Height <= MaxImageEdge {
		return data, mimeType, nil
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("the image reports %dx%d but its pixels could not be read: %w",
			cfg.Width, cfg.Height, err)
	}

	scaled := fitWithin(src, MaxImageEdge)
	out, outType, err := encodeScaled(scaled, format)
	if err != nil {
		return nil, "", err
	}

	log.Printf("httpx: downscaled an image from %dx%d to %dx%d (%s, %d -> %d bytes)",
		cfg.Width, cfg.Height, scaled.Bounds().Dx(), scaled.Bounds().Dy(),
		outType, len(data), len(out))
	return out, outType, nil
}

// fitWithin scales an image so neither edge is longer than maxEdge, keeping the
// aspect ratio.
func fitWithin(src image.Image, maxEdge int) image.Image {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	scale := float64(maxEdge) / float64(max(w, h))
	nw := max(1, int(math.Round(float64(w)*scale)))
	nh := max(1, int(math.Round(float64(h)*scale)))

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	// CatmullRom is the bicubic kernel. ApproxBiLinear is cheaper but visibly
	// softer, and this runs only for uploads that were too big in the first
	// place, so the cost is not on the common path.
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, xdraw.Src, nil)
	return dst
}

// encodeScaled re-encodes a scaled image into a type Flow reads.
//
// PNG stays PNG, and so does anything carrying transparency: JPEG has no alpha
// channel, so re-encoding a cut-out as JPEG would fill its background with
// black. WebP is the case that matters in practice — x/image decodes it but
// cannot encode it — so it has to become one of these two.
func encodeScaled(img image.Image, format string) ([]byte, string, error) {
	var buf bytes.Buffer

	if format == "png" || hasAlpha(img) {
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", fmt.Errorf("could not re-encode the image as PNG: %w", err)
		}
		return buf.Bytes(), "image/png", nil
	}

	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", fmt.Errorf("could not re-encode the image as JPEG: %w", err)
	}
	return buf.Bytes(), "image/jpeg", nil
}

// hasAlpha reports whether an image has transparency worth preserving.
//
// An implementation that cannot answer is treated as transparent: guessing
// "opaque" is the guess that destroys data, and the cost of being wrong the
// other way is only a larger file.
func hasAlpha(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return !o.Opaque()
	}
	return true
}
