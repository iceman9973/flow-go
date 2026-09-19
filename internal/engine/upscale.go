package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kodelyx/cdp-control/cdp"
	"github.com/kodelyx/flow-go/internal/config"
	"github.com/kodelyx/flow-go/internal/recaptcha"
)

// ImageUpscaleResult is one resolved image.
type ImageUpscaleResult struct {
	// Data is the decoded image.
	Data []byte
	// MediaType is "image/jpeg" or "image/png", sniffed from the magic bytes.
	MediaType string
	// Bytes is len(Data), kept for logging without touching the slice.
	Bytes int
}

// UpscaleImage resolves an image at a higher resolution.
//
// It runs the app's own SPrCad request inside the attached tab rather than over
// the Go transport, and that is deliberate.
//
// The Go transport cannot make this call. Sending the identical request from Go
// is rejected with PUBLIC_ERROR_UNUSUAL_ACTIVITY while the very same captcha
// token and payload succeed from the page. That was established by elimination:
// the payload is byte-identical, and each of the following was varied in turn
// and made no difference —
//
//   - the `bl` build label (present, absent, and the browser's own value)
//   - the `f.sid` session id
//   - the `at` anti-CSRF token (including the page's own SNlM0e)
//   - the `source-path` shape
//   - the header set, including a byte-for-byte replica of every header the
//     browser sends (accept-encoding, priority, sec-ch-ua-*, x-browser-*,
//     x-client-data), and with Authorization removed
//   - the cookie jar (the same jar the browser produced)
//   - HTTP/2 versus HTTP/3
//
// The captcha token itself is not the problem: a token minted by this engine's
// own provider was replayed from the page and returned the image. The one
// difference left is the TLS and HTTP/2 fingerprint — the transport presents a
// Chrome 152 profile and the browser is Chrome 153 — and tls-client cannot
// match that build. Notably the generation RPC over the same Go transport does
// succeed, so SPrCad applies a stricter client check than generation does.
//
// Running it in the page sidesteps the whole question, and the page is already
// required for the captcha broker.
func (e *Engine) UpscaleImage(ctx context.Context, projectID, mediaID, contentID string, resolution int) (*ImageUpscaleResult, error) {
	if projectID == "" || contentID == "" {
		return nil, fmt.Errorf("engine: a project id and a content id are required")
	}
	if resolution == 0 {
		resolution = Resolution2K
	}
	if e.bridge == nil || !e.bridge.Connected() {
		return nil, fmt.Errorf("engine: upscaling needs the browser — no extension connected")
	}

	client := e.bridge.Current()
	if client == nil || !client.Connected() {
		return nil, fmt.Errorf("engine: upscaling needs the browser — no tab attached")
	}

	callCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()

	out, err := client.FlowUpscale(callCtx, cdp.FlowUpscaleRequest{
		ProjectID:  projectID,
		MediaID:    mediaID,
		ContentID:  contentID,
		Resolution: resolution,
		BuildLabel: config.BuildLabel(),
		SiteKey:    recaptcha.DefaultSiteKey,
	}, upscaleExpression(projectID, mediaID, contentID, resolution, config.BuildLabel()))
	if err != nil {
		return nil, fmt.Errorf("engine: the in-page upscale call failed: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("engine: upscale rejected: %s", out.Error)
	}
	if out.Data == "" {
		return nil, fmt.Errorf("engine: the upscale response carried no image data")
	}

	data, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil {
		// The RPC emits standard base64, but tolerate the URL-safe alphabet
		// rather than failing on a stray - or _.
		data, err = base64.URLEncoding.DecodeString(strings.NewReplacer("-", "+", "_", "/").Replace(out.Data))
		if err != nil {
			return nil, fmt.Errorf("engine: the upscale image was not valid base64: %w", err)
		}
	}

	return &ImageUpscaleResult{
		Data:      data,
		MediaType: sniffImageType(data),
		Bytes:     len(data),
	}, nil
}

// Resolution selectors for SPrCad's second argument.
//
// The download menu offers 1K / 2K / 4K, but the first of those is labelled
// "Original size" and the other two "Upscaled" — and that is exactly how the
// selector behaves. Measured against an image generated at 1376x768:
//
//	selector 1 (what the "2K | Upscaled" item sends) -> 2752x1536, exactly 2x
//	selector 2 (what "4K | Upscaled" sends)          -> PUBLIC_ERROR_MODEL_ACCESS_DENIED
//	                                                     on this account; 4K is gated
//	selector 0 (never sent by the app)               -> the same 2752x1536 image as 1
//
// So the selector is 1-based over the *upscaled* options, and 1K needs no call at
// all: it is the asset's own size, which the media URL already serves. Only 2K
// has been produced end to end here; 4K is untested beyond confirming it is
// refused for want of entitlement.
const (
	Resolution2K = 1
	Resolution4K = 2
)

// upscaleExpression is the page-side SPrCad call.
//
// It mints its own reCAPTCHA token through the page's grecaptcha, exactly as the
// download menu does, then posts the request with the page's own cookies and
// anti-CSRF token. It returns the image as base64, because that is the form the
// RPC answers in: SPrCad does not hand back a URL, it hands back the bytes. That
// is why the download is a blob — the page decodes this and saves it.
func upscaleExpression(projectID, mediaID, contentID string, resolution int, buildLabel string) string {
	payload := map[string]any{
		"siteKey":    "6LdsFiUsAAAAAIjVDZcuLhaHiDn5nnHVXVRQGeMV",
		"action":     "IMAGE_GENERATION",
		"contentID":  contentID,
		"projectID":  projectID,
		"mediaID":    mediaID,
		"resolution": resolution,
		"buildLabel": buildLabel,
	}
	encoded, _ := json.Marshal(payload)

	return `(async () => {
  const cfg = ` + string(encoded) + `;
  try {
    if (!window.grecaptcha || !window.grecaptcha.enterprise) {
      return {error: 'grecaptcha is not loaded on this page'};
    }
    await new Promise(resolve => window.grecaptcha.enterprise.ready(resolve));
    const token = await window.grecaptcha.enterprise.execute(cfg.siteKey, {action: cfg.action});

    // Position [0] is the media id, not the content id. This is the opposite of
    // the generation calls, which take content ids in startImage/endImage, and
    // getting it backwards returns a null payload rather than an error — which
    // is exactly how it presented. Taken from the app's own SPrCad request.
    //
    // Position [1] is the resolution selector and [2] is the context block the
    // app always sends: tool id 22, the project, then the captcha pair.
    const arg = [cfg.mediaID, cfg.resolution,
                 [null, 22, null, null, null, cfg.projectID, null, null, null, null, [token, 1]]];

    const body = new URLSearchParams();
    body.set('f.req', JSON.stringify([[['SPrCad', JSON.stringify(arg), null, 'generic']]]));
    const at = (window.WIZ_global_data && window.WIZ_global_data.SNlM0e) || '';
    if (at) body.set('at', at);

    // The source path names the project, and only the project. An earlier
    // version appended the editor route with the media id in it; the app's own
    // request does not, and the media id travels in arg[0] instead.
    let url = '/_/AiSandboxAngularFrontend/data/batchexecute?rpcids=SPrCad&source-path=' +
      encodeURIComponent('/project/' + cfg.projectID) +
      '&hl=' + encodeURIComponent(navigator.language || 'en') + '&rt=c';
    if (cfg.buildLabel) url += '&bl=' + encodeURIComponent(cfg.buildLabel);

    const res = await fetch(url, {
      method: 'POST',
      headers: {'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8', 'X-Same-Domain': '1'},
      body: body.toString(),
      credentials: 'include'
    });
    const text = await res.text();

    if (text.indexOf('PUBLIC_ERROR') !== -1) {
      const m = text.match(/PUBLIC_ERROR_[A-Z_]+/);
      return {error: m ? m[0] : 'PUBLIC_ERROR'};
    }

    const line = text.split('\n').find(l => l.trim().indexOf('[["wrb.fr"') === 0);
    if (!line) return {error: 'the response carried no SPrCad frame'};

    const outer = JSON.parse(line);
    const inner = JSON.parse(outer[0][2]);

    // The image is the longest base64-looking string anywhere in the payload.
    // Walking beats a fixed path: the metadata around it has moved between
    // captures, and the image is unambiguous by size and alphabet.
    let best = '';
    const walk = (x) => {
      if (typeof x === 'string') {
        if (x.length > best.length && /^[A-Za-z0-9+/=\-_]{500,}$/.test(x)) best = x;
      } else if (Array.isArray(x)) {
        for (const v of x) walk(v);
      } else if (x && typeof x === 'object') {
        for (const v of Object.values(x)) walk(v);
      }
    };
    walk(inner);

    if (!best) return {error: 'the response carried no image data'};
    return {data: best};
  } catch (e) {
    return {error: String(e)};
  }
})()`
}

// sniffImageType names the format from the magic bytes, so the caller does not
// have to guess an extension.
func sniffImageType(data []byte) string {
	switch {
	case len(data) > 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) > 8 && string(data[0:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}
