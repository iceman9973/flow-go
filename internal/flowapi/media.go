package flowapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kodelyx/flow-go/internal/config"
	"github.com/kodelyx/flow-go/internal/httpx"
)

/* ------------------------------------------------------------------ *
 * Status polling
 * ------------------------------------------------------------------ */

// PollOnce asks Flow for the current state of a generation.
func (c *Client) PollOnce(ctx context.Context, mediaID string) (MediaStatus, error) {
	session, err := c.auth.Session(ctx)
	if err != nil {
		return MediaStatus{}, err
	}
	projectID := session.ResolveProjectID(c.opts.ProjectID)

	body := map[string]any{
		"media": []map[string]any{
			{"name": mediaID, "projectId": projectID},
		},
	}

	result, err := c.Call(ctx, config.Endpoints["poll_status"], body, "")
	if err != nil {
		return MediaStatus{}, err
	}

	var parsed GenerateResponse
	if err := result.JSON(&parsed); err != nil {
		return MediaStatus{}, fmt.Errorf("flow: decode status response: %w", err)
	}
	if len(parsed.Media) == 0 {
		// Not an error: Flow returns an empty list while a job is still being
		// registered, which is normal for the first few polls.
		return MediaStatus{}, nil
	}
	return parsed.Media[0].MediaMetadata.MediaStatus, nil
}

// WaitForMedia polls until the generation reaches a terminal state or the
// timeout expires. Returns the final status.
func (c *Client) WaitForMedia(ctx context.Context, mediaID string) (MediaStatus, error) {
	timeout := time.Duration(config.PollTimeout) * time.Second
	interval := time.Duration(config.PollInterval) * time.Second

	deadline := time.Now().Add(timeout)
	start := time.Now()

	for {
		status, err := c.PollOnce(ctx, mediaID)
		if err != nil {
			// A transient poll failure is not fatal: keep polling until the
			// deadline, but surface it if we run out of time.
			log.Printf("flow[%s]: poll error for %s: %v", c.opts.AccountID, truncate(mediaID, 12), err)
		} else if status.Terminal() {
			elapsed := int(time.Since(start).Seconds())
			if status.Succeeded() {
				log.Printf("flow[%s]: %s ready in %ds", c.opts.AccountID, truncate(mediaID, 12), elapsed)
			} else {
				log.Printf("flow[%s]: %s finished as %s after %ds",
					c.opts.AccountID, truncate(mediaID, 12), status.MediaGenerationStatus, elapsed)
			}
			return status, nil
		}

		if time.Now().After(deadline) {
			return MediaStatus{}, fmt.Errorf(
				"flow: timed out after %ds waiting for %s", config.PollTimeout, truncate(mediaID, 12))
		}

		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return MediaStatus{}, ctx.Err()
		}
	}
}

/* ------------------------------------------------------------------ *
 * Media URL resolution
 * ------------------------------------------------------------------ */

// mediaRedirectPaths are the Labs endpoints that hand back a signed media URL.
// They are part of an undocumented internal API, so more than one shape is tried
// and a failure falls through to the legacy inline endpoint rather than aborting.
var mediaRedirectPaths = []string{
	"/fx/api/trpc/media.getMediaUrlRedirect",
	"/fx/api/media/redirect",
}

// MediaURL resolves a downloadable URL for a finished generation.
//
// preferUpsampled asks for the 1080p variant first, matching the Flow UI's
// high-resolution download. Flow stores it under the same ID with an
// "_upsampled" suffix, so a caller that generated at 720p and wants 1080p does
// not need a second generation pass.
func (c *Client) MediaURL(ctx context.Context, mediaID string, preferUpsampled bool) (string, error) {
	candidates := []string{mediaID}
	if preferUpsampled {
		candidates = append([]string{mediaID + "_upsampled"}, candidates...)
	}

	var lastErr error
	for _, candidate := range candidates {
		for _, path := range mediaRedirectPaths {
			signedURL, err := c.resolveRedirect(ctx, path, candidate)
			if err == nil && signedURL != "" {
				log.Printf("flow[%s]: resolved media URL for %s", c.opts.AccountID, truncate(candidate, 16))
				return signedURL, nil
			}
			if err != nil {
				lastErr = err
			}
		}
	}

	if lastErr == nil {
		lastErr = errors.New("no redirect endpoint returned a URL")
	}
	return "", fmt.Errorf("flow: could not resolve a media URL for %s: %w", truncate(mediaID, 12), lastErr)
}

func (c *Client) resolveRedirect(ctx context.Context, path, mediaID string) (string, error) {
	u := config.LabsBase + path + "?" + url.Values{
		"name": {mediaID},
		"key":  {config.APIKey()},
	}.Encode()

	resp, err := c.hc.Do(ctx, &httpx.Request{
		Method: "GET",
		URL:    u,
		Headers: map[string]string{
			"Accept":  "*/*",
			"Referer": config.FlowUIBase,
			"Origin":  config.LabsBase,
		},
		Cookies:     c.auth.Jar().HeaderForDomain(config.LabsBase),
		DisableQUIC: true,
	})
	if err != nil {
		return "", err
	}

	// A redirect is the expected success shape. The uTLS client does not follow
	// redirects, so the Location header is read directly.
	if loc := resp.Header.Get("Location"); loc != "" {
		return loc, nil
	}

	if resp.Status == 200 {
		// Some responses inline the URL instead of redirecting.
		body := strings.TrimSpace(resp.Text())
		if strings.HasPrefix(body, "http") && !strings.ContainsAny(body, " \n\t\"") {
			return body, nil
		}
		// tRPC wraps its payload in {"result":{"data":{...}}}.
		var wrapper struct {
			Result struct {
				Data any `json:"data"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(body), &wrapper); err == nil && wrapper.Result.Data != nil {
			if s, ok := wrapper.Result.Data.(string); ok && strings.HasPrefix(s, "http") {
				return s, nil
			}
			if m, ok := wrapper.Result.Data.(map[string]any); ok {
				for _, key := range []string{"url", "redirectUrl", "signedUrl"} {
					if s, ok := m[key].(string); ok && strings.HasPrefix(s, "http") {
						return s, nil
					}
				}
			}
		}
	}

	return "", fmt.Errorf("no redirect (status %d)", resp.Status)
}

/* ------------------------------------------------------------------ *
 * Download
 * ------------------------------------------------------------------ */

// Download streams a media URL to disk. Returns the number of bytes written.
//
// Cookies are attached to every hop. Google's signed URLs usually redirect
// through a CDN, and a request that loses its credentials on the second hop gets
// a 403 — which is the single most common download failure in this kind of
// client.
func (c *Client) Download(ctx context.Context, mediaURL, targetPath string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return 0, err
	}

	cookieHeader := c.auth.Jar().HeaderForDomain(config.LabsBase)
	currentURL := mediaURL

	for hop := 0; hop < 10; hop++ {
		resp, err := c.hc.Do(ctx, &httpx.Request{
			Method: "GET",
			URL:    currentURL,
			Headers: map[string]string{
				"Accept":  "*/*",
				"Referer": config.FlowUIBase,
			},
			Cookies:     cookieHeader,
			DisableQUIC: true,
		})
		if err != nil {
			return 0, fmt.Errorf("flow: download request failed: %w", err)
		}

		if resp.Status >= 300 && resp.Status < 400 {
			location := resp.Header.Get("Location")
			if location == "" {
				return 0, fmt.Errorf("flow: redirect with no Location (status %d)", resp.Status)
			}
			currentURL = location
			continue
		}

		if resp.Status != 200 {
			return 0, fmt.Errorf("flow: download failed with status %d", resp.Status)
		}

		tmp := targetPath + ".part"
		written, err := writeFile(tmp, resp.Body)
		if err != nil {
			_ = os.Remove(tmp)
			return 0, err
		}
		if written < 1000 {
			_ = os.Remove(tmp)
			return 0, fmt.Errorf("flow: downloaded only %d bytes; treating as a failed download", written)
		}
		if err := os.Rename(tmp, targetPath); err != nil {
			_ = os.Remove(tmp)
			return 0, err
		}

		log.Printf("flow[%s]: saved %s (%.1f MB)", c.opts.AccountID, filepath.Base(targetPath), float64(written)/(1024*1024))
		return written, nil
	}

	return 0, fmt.Errorf("flow: too many redirects downloading %s", truncate(mediaURL, 80))
}

func writeFile(path string, data []byte) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(f, strings.NewReader(string(data)))
	if err != nil {
		return n, err
	}
	return n, nil
}

// LegacyMediaBase64 fetches a media item from the older inline endpoint, which
// returns the file base64-encoded inside a JSON document.
func (c *Client) LegacyMediaBase64(ctx context.Context, mediaID string) ([]byte, error) {
	endpoint := strings.ReplaceAll(config.Endpoints["get_media"], "{media_id}", mediaID)

	result, err := c.Get(ctx, endpoint, "")
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Video struct {
			EncodedVideo string `json:"encodedVideo"`
		} `json:"video"`
	}
	if err := result.JSON(&parsed); err != nil {
		return nil, fmt.Errorf("flow: decode media response: %w", err)
	}
	if parsed.Video.EncodedVideo == "" {
		return nil, fmt.Errorf("flow: media response carried no inline video")
	}

	data, err := base64.StdEncoding.DecodeString(parsed.Video.EncodedVideo)
	if err != nil {
		return nil, fmt.Errorf("flow: media payload is not valid base64: %w", err)
	}
	return data, nil
}
