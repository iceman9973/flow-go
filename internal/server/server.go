package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/google/uuid"
	"github.com/kodelyx/cdp-control/bridge"
	"github.com/kodelyx/cdp-control/cdp"
	"github.com/kodelyx/cdp-control/cookiejar"
	"github.com/kodelyx/flow-go/internal/batchexecute"
	"github.com/kodelyx/flow-go/internal/config"
	"github.com/kodelyx/flow-go/internal/engine"
	"github.com/kodelyx/flow-go/internal/httpx"
)

// RegisterRoutes wires every HTTP route onto the app.
func RegisterRoutes(app *fiber.App, eng *engine.Engine, br *bridge.Bridge) {
	app.Use(cors.New())

	// pickClient returns the extension a diagnostic should act on: the one named
	// by `?addr=`, or the current one when no name is given.
	//
	// More than one can be attached at once — the generic bridge and the Flow one
	// — and only the most recent becomes current. Driving a page over raw CDP
	// needs the generic bridge specifically, because the Flow bridge deliberately
	// has no raw surface; without a way to name it, that route would have to wait
	// for the other extension to disconnect.
	pickClient := func(c fiber.Ctx) *cdp.Client {
		if addr := strings.TrimSpace(c.Query("addr")); addr != "" {
			return br.Clients()[addr]
		}
		return br.Current()
	}
	app.Use(logger.New())
	app.Use(recoverer)

	app.Get("/", help)
	app.Get("/help", help)

	app.Get("/health", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status": healthStatus(eng, br),
			"engine": "flow-go (pure Go, no browser on the generation path)",
			"ready":  eng.Ready(),
			// Which upstream path has a credential. The engine boots without a
			// Labs session on purpose — the cookie-authenticated path is the one
			// that works against flow.google.com — so this says which of the two
			// is usable rather than leaving it to be discovered from a failing
			// call.
			"bearer_path_available": eng.BearerPathAvailable(),
			"bridge":                br.Status(),
			"error":                 eng.LastError(),
		})
	})

	app.Get("/status", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"ready":                 eng.Ready(),
			"bearer_path_available": eng.BearerPathAvailable(),
			"account_id":            eng.AccountID(),
			"bridge":                br.Status(),
			"pool":                  eng.Pool().Stats(),
			"error":                 eng.LastError(),
		})
	})

	app.Get("/stats", func(c fiber.Ctx) error {
		stats, err := eng.Store().Stats()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{
			"database": stats,
			"pool":     eng.Pool().Stats(),
			"bridge":   br.Status(),
		})
	})

	app.Get("/v1/workers", func(c fiber.Ctx) error {
		return c.JSON(eng.Pool().Stats())
	})

	// Ask the browser to bring the target page up so the site renews its own
	// session, then re-sync cookies and re-bootstrap the engine.
	//
	// This exists because Labs invalidates its session server-side and starts
	// returning ACCESS_TOKEN_REFRESH_NEEDED; only the site itself can renew it.
	// It is the single recovery action when the engine reports auth problems.
	app.Post("/v1/bridge/refresh", func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		jar, err := br.RefreshSession(ctx)
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": err.Error()})
		}

		if err := eng.Bootstrap(ctx); err != nil {
			return c.Status(503).JSON(fiber.Map{
				"error":       err.Error(),
				"cookies":     jar.Count(),
				"credentials": jar.HasAuthCookies(),
				"jar_changed": true,
			})
		}

		return c.JSON(fiber.Map{
			"status":      "ok",
			"cookies":     jar.Count(),
			"credentials": jar.HasAuthCookies(),
			"ready":       eng.Ready(),
		})
	})

	// Hand the browser-derived session to a process that has no browser.
	//
	// The bridge port admits one host, and everything the engine takes from the
	// browser is short-lived — the cookies rotate, and the page tokens exist only
	// in a loaded page. So the process that has the browser serves this to the one
	// that does not, on demand, rather than through a file snapshot that goes
	// stale on Google's rotation schedule instead of on a timer anyone here
	// controls. The CLI is the caller: it takes a snapshot at start-up and seeds
	// itself from it, which leaves no window in which the copy is older than the
	// run using it.
	//
	// Guarded by the bridge token. These are the account's credentials, and an
	// unauthenticated localhost endpoint would hand them to any process on the
	// machine — the exact threat the bridge token was added to close.
	app.Get("/v1/session", func(c fiber.Ctx) error {
		if !bridgeTokenMatches(c) {
			return c.Status(401).JSON(fiber.Map{
				"error": "the bridge token is required, as X-Bridge-Token or ?token=",
			})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		snapshot, err := eng.SessionSnapshot(ctx)
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(snapshot)
	})

	// The account's real credit balance, read over batchexecute.
	//
	// This deliberately does not use the legacy aisandbox endpoint: it reports a
	// different number, and its token can belong to a different signed-in
	// account, so it is not a figure to present as the balance.
	app.Get("/v1/credits", func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), 60*time.Second)
		defer cancel()

		credits, err := eng.Credits(ctx)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{
			"credits":    credits,
			"account_id": eng.AccountID(),
			"project_id": eng.ProjectID(),
			"source":     "batchexecute nzlxg",
		})
	})

	// Evaluate an expression in the attached tab. A debugging surface: when the
	// reCAPTCHA broker or a selector stops working, this is how you find out what
	// the page actually looks like without adding logging to the engine.
	app.Post("/v1/bridge/eval", func(c fiber.Ctx) error {
		var req struct {
			Expression string `json:"expression"`
			TabID      int    `json:"tab_id,omitempty"`
		}
		if err := c.Bind().JSON(&req); err != nil || req.Expression == "" {
			return c.Status(400).JSON(fiber.Map{"error": "expression is required"})
		}

		// pickClient, not Current(): `cdp.evaluate` is the generic extension's
		// operation and the Flow one does not implement it. Current() is the Flow
		// extension whenever both are attached — which is the normal setup — so
		// this route answered "Unknown operation: cdp.evaluate" for the one
		// extension it exists to drive, and ignored `?addr=`. The generic
		// extension is the monitoring and debugging surface and the narrow one is
		// for the engine's own work; neither can stand in for the other.
		client := pickClient(c)
		if client == nil || !client.Connected() {
			return c.Status(503).JSON(fiber.Map{"error": "no extension connected"})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		if _, err := client.Attach(ctx, req.TabID); err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}

		raw, err := client.Evaluate(ctx, req.Expression)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"result": json.RawMessage(raw)})
	})

	// Read the CDP events the extension has buffered for the attached tab.
	//
	// The Network domain is enabled on attach, so this holds every request the
	// page has made — including the ones the app issues to its own API, with the
	// full URL and its query parameters. That is how the app's real API key and
	// endpoint shapes can be read off the browser rather than guessed.
	app.Post("/v1/bridge/events", func(c fiber.Ctx) error {
		var req struct {
			Limit  int    `json:"limit"`
			Filter string `json:"filter"`
		}
		_ = c.Bind().JSON(&req)
		if req.Limit <= 0 || req.Limit > 1000 {
			req.Limit = 500
		}

		// pickClient, for the same reason as /v1/bridge/eval: CDP events come
		// from the generic extension, and Current() is the Flow one whenever both
		// are attached. Reading events off the narrow extension returns its own
		// (deliberately empty) buffer, so this looked like "the page made no
		// requests" rather than "the wrong extension was asked".
		client := pickClient(c)
		if client == nil || !client.Connected() {
			return c.Status(503).JSON(fiber.Map{"error": "no extension connected"})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		events, err := client.ReadEvents(ctx, req.Limit)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}

		type requestRow struct {
			Method   string            `json:"method"`
			URL      string            `json:"url"`
			PostData string            `json:"post_data,omitempty"`
			Headers  map[string]string `json:"headers,omitempty"`
		}
		var requests []requestRow
		methods := map[string]int{}

		for _, ev := range events {
			methods[ev.Method]++
			if ev.Method != "Network.requestWillBeSent" {
				continue
			}
			var params struct {
				Request struct {
					URL      string            `json:"url"`
					Method   string            `json:"method"`
					PostData string            `json:"postData"`
					Headers  map[string]string `json:"headers"`
				} `json:"request"`
			}
			if err := json.Unmarshal(ev.Params, &params); err != nil {
				continue
			}
			if req.Filter != "" && !strings.Contains(params.Request.URL, req.Filter) {
				continue
			}
			requests = append(requests, requestRow{
				Method:   params.Request.Method,
				URL:      params.Request.URL,
				PostData: params.Request.PostData,
				Headers:  params.Request.Headers,
			})
		}

		return c.JSON(fiber.Map{
			"events_read": len(events),
			"by_type":     methods,
			"requests":    requests,
		})
	})

	// Issue an arbitrary CDP command against the attached tab.
	//
	// This is the escape hatch the other diagnostics cannot provide: commands like
	// Page.addScriptToEvaluateOnNewDocument have to run *before* the page loads,
	// so a hook installed via cdp.evaluate is always too late.
	app.Post("/v1/bridge/cdp", func(c fiber.Ctx) error {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
			TabID  int            `json:"tab_id,omitempty"`
		}
		if err := c.Bind().JSON(&req); err != nil || req.Method == "" {
			return c.Status(400).JSON(fiber.Map{"error": "method is required"})
		}

		client := pickClient(c)
		if client == nil || !client.Connected() {
			return c.Status(503).JSON(fiber.Map{"error": "no extension connected"})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		if _, err := client.Attach(ctx, req.TabID); err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}

		var raw json.RawMessage
		if err := client.CallCDP(ctx, req.Method, req.Params, &raw); err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"result": raw})
	})

	// Submit a generation over the batchexecute transport, using a reCAPTCHA
	// token from the configured provider.
	//
	// This is the corrected path. The aisandbox REST surface it replaces is the
	// legacy one and cannot work: the app moved to flow.google.com and its key
	// blocks that referrer.
	app.Post("/v1/debug/batchexecute-generate", func(c fiber.Ctx) error {
		var req struct {
			Prompt    string `json:"prompt"`
			Model     string `json:"model"`
			ProjectID string `json:"project_id"`
			Action    string `json:"action"`
			Kind      string `json:"kind"`
			Count     int    `json:"count"`
			// DropAuthorization removes the SAPISIDHASH header for a comparison
			// run. The page's own batchexecute call sends cookies, `at` and
			// X-Same-Domain and no Authorization header at all, so this is the
			// one structural difference left to vary.
			DropAuthorization bool `json:"drop_authorization"`
		}
		if err := c.Bind().JSON(&req); err != nil || req.Prompt == "" {
			return c.Status(400).JSON(fiber.Map{"error": "prompt is required"})
		}
		if req.Model == "" {
			req.Model = "NARWHAL"
		}
		if req.ProjectID == "" {
			req.ProjectID = eng.ProjectID()
		}
		if req.Action == "" {
			req.Action = "IMAGE_GENERATION"
		}
		if req.ProjectID == "" {
			return c.Status(503).JSON(fiber.Map{"error": "no project id resolved"})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		captcha, err := eng.CaptchaToken(ctx, req.Action)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "captcha: " + err.Error()})
		}

		jar := eng.Bridge().Jar()
		if jar == nil {
			return c.Status(503).JSON(fiber.Map{"error": "no cookies loaded"})
		}
		hc, err := httpx.New(
			httpx.WithTimeout(time.Duration(config.RequestTimeout) * time.Second),
		)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		client := eng.NewBatchexecuteClient(jar, hc)
		if req.DropAuthorization {
			client.SetHeaderOverrides(map[string]string{"Authorization": ""})
		}
		callOpts := batchexecute.CallOptions{
			SourcePath: "/project/" + req.ProjectID,
			BuildLabel: config.BuildLabel(),
		}

		var frames []batchexecute.Frame
		if strings.EqualFold(req.Kind, "video") {
			frames, err = client.GenerateVideo(ctx, batchexecute.GenerateVideoRequest{
				ProjectID:    req.ProjectID,
				Model:        req.Model,
				Prompt:       req.Prompt,
				Count:        req.Count,
				CaptchaToken: captcha,
			}, callOpts)
		} else {
			frames, err = client.Generate(ctx, batchexecute.GenerateRequest{
				ProjectID:    req.ProjectID,
				Model:        req.Model,
				Prompt:       req.Prompt,
				CaptchaToken: captcha,
			}, callOpts)
		}
		if err != nil {
			return c.Status(502).JSON(fiber.Map{
				"error":       err.Error(),
				"captcha_len": len(captcha),
				"project_id":  req.ProjectID,
				"model":       req.Model,
			})
		}

		out := make([]json.RawMessage, 0, len(frames))
		for _, frame := range frames {
			out = append(out, frame.Payload)
		}
		return c.JSON(fiber.Map{
			"status":      "submitted",
			"captcha_len": len(captcha),
			"project_id":  req.ProjectID,
			"model":       req.Model,
			"frames":      out,
		})
	})

	// Ask the extension for the cookies matching one scope and report exactly
	// what came back, before the backend's own name filter runs.
	//
	// Cookie retrieval is the one call where every failure mode looks identical
	// from the outside: a scope check that refuses, a domain filter that matches
	// nothing, and a name filter that drops everything all present as "the
	// browser has no cookies for that host". This separates them.
	app.Post("/v1/debug/cookies", func(c fiber.Ctx) error {
		var req struct {
			Domain string `json:"domain"`
			URL    string `json:"url"`
			// Names asks for the unfiltered view: every cookie name the extension
			// can see for the scope, before the backend's own name filter runs.
			Names bool `json:"names"`
			// All asks every attached extension rather than only the current one.
			// Two can be connected at once — the generic bridge and the Flow one —
			// and only the most recent becomes current, so without this a
			// comparison needs one of them disconnected, which is precisely when
			// it cannot be asked anything.
			All bool `json:"all"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}
		if req.Domain == "" && req.URL == "" {
			return c.Status(400).JSON(fiber.Map{"error": "domain or url is required"})
		}

		details := map[string]any{}
		if req.Domain != "" {
			details["domain"] = req.Domain
		}
		if req.URL != "" {
			details["url"] = req.URL
		}

		ask := func(ctx context.Context, client *cdp.Client) fiber.Map {
			row := fiber.Map{"addr": client.RemoteAddr}

			// Which surface this one offers, so the answer says who gave it.
			surface := client.ProbeSurface()
			row["flow_operations"] = surface.FlowOperations
			row["ops"] = len(surface.Advertised)

			if req.Names {
				var out json.RawMessage
				if err := client.Call(ctx, "cookies.names", map[string]any{"details": details}, &out); err != nil {
					row["error"] = err.Error()
					return row
				}
				row["names"] = out
				return row
			}

			cookies, err := client.ListCookies(ctx, details)
			if err != nil {
				row["error"] = err.Error()
				return row
			}
			// Host and name only: this is a diagnostic, and the values are the
			// credentials.
			names := make([]string, 0, len(cookies))
			for _, ck := range cookies {
				names = append(names, ck.Domain+" | "+ck.Name)
			}
			row["count"] = len(cookies)
			row["cookies"] = names
			return row
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if req.All {
			clients := br.Clients()
			rows := make([]fiber.Map, 0, len(clients))
			for _, client := range clients {
				rows = append(rows, ask(ctx, client))
			}
			return c.JSON(fiber.Map{"clients": rows, "sent": details})
		}

		client := br.Current()
		if client == nil || !client.Connected() {
			return c.Status(503).JSON(fiber.Map{"error": "no extension connected"})
		}
		return c.JSON(ask(ctx, client))
	})

	// Dump the raw upstream credits response.
	//
	// The parsed balance alone is not enough to trust: the field names are
	// undocumented and the value may be an allocation rather than a balance, so
	// the whole payload is exposed to be read directly.
	app.Get("/v1/debug/credits-raw", func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		raw, err := eng.RawCredits(ctx)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"raw": json.RawMessage(raw)})
	})

	// The raw batchexecute credits response for one signed-in account.
	//
	// Distinct from /v1/debug/credits-raw, which calls the legacy aisandbox
	// endpoint and reports whatever account that credential belongs to. This one
	// uses the transport and the `authuser` a real balance read uses, so it can be
	// pointed at a specific account — which is the only way to find out whether the
	// response carries a tier, given the account tier is currently read from an
	// endpoint that ignores `authuser`.
	app.Get("/v1/debug/credits-rpc", func(c fiber.Ctx) error {
		authUser := 0
		if raw := strings.TrimSpace(c.Query("authuser")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return c.Status(400).JSON(fiber.Map{
					"error": "authuser must be a non-negative integer",
				})
			}
			authUser = n
		}

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		frames, err := eng.RawCreditsRPC(ctx, authUser)
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"authuser": authUser, "frames": frames})
	})

	// Resolve an image at a larger size over the batchexecute transport.
	//
	// The download menu's "2K / Upscaled" choice runs this before fetching the
	// file, which is why no new project asset appears.
	app.Post("/v1/debug/image-upscale", func(c fiber.Ctx) error {
		var req struct {
			ContentID  string `json:"content_id"`
			MediaID    string `json:"media_id"`
			ProjectID  string `json:"project_id"`
			Resolution int    `json:"resolution"`
			// HeaderOverrides is diagnostic: it varies the request headers so
			// the one that matters can be found by elimination. A value of ""
			// removes the header.
			HeaderOverrides map[string]string `json:"header_overrides"`
			// TLSProfile selects the TLS/HTTP2 fingerprint by name, e.g.
			// "chrome_152" or "firefox_133". Empty means the default.
			TLSProfile string `json:"tls_profile"`
			// ProtocolRacing lets the TLS library race HTTP/3 against HTTP/2,
			// the way the browser does.
			ProtocolRacing bool `json:"protocol_racing"`
			// HeaderOrder replaces the header ordering, so the browser's exact
			// order can be replayed rather than approximated.
			HeaderOrder []string `json:"header_order"`
			// DropCredentials strips the session cookies for this call, which is
			// how a real 401 is produced on demand. Without it the 401 recovery
			// path cannot be exercised except by waiting for a session to expire.
			DropCredentials bool `json:"drop_credentials"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}
		if req.ContentID == "" && req.MediaID == "" {
			return c.Status(400).JSON(fiber.Map{"error": "media_id or content_id is required"})
		}
		if req.ProjectID == "" {
			req.ProjectID = eng.ProjectID()
		}
		// A caller normally holds the media id — it is what a generation returns
		// and what the editor URL carries — while SPrCad takes the content id.
		// Resolve it rather than making every caller know both.
		if req.ContentID == "" {
			contentID, err := eng.ResolveContentID(c.Context(), req.MediaID)
			if err != nil {
				return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
			}
			req.ContentID = contentID
		}
		if req.Resolution == 0 {
			req.Resolution = 1
		}

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		captcha, err := eng.CaptchaToken(ctx, "IMAGE_GENERATION")
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "captcha: " + err.Error()})
		}

		jar := eng.Bridge().Jar()
		if jar == nil {
			return c.Status(503).JSON(fiber.Map{"error": "no cookies loaded"})
		}
		httpOpts := []httpx.Option{
			httpx.WithTimeout(time.Duration(config.RequestTimeout) * time.Second),
			httpx.WithProfile(req.TLSProfile),
		}
		if req.ProtocolRacing {
			httpOpts = append(httpOpts, httpx.WithProtocolRacing())
		}
		hc, err := httpx.New(httpOpts...)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		if req.DropCredentials {
			jar = withoutAuthCookies(jar)
		}

		client := eng.NewBatchexecuteClient(jar, hc)
		client.SetHeaderOverrides(req.HeaderOverrides)
		client.SetHeaderOrder(req.HeaderOrder)
		media, err := client.UpscaleImage(ctx, batchexecute.ImageUpscaleRequest{
			ProjectID:    req.ProjectID,
			MediaID:      req.MediaID,
			ContentID:    req.ContentID,
			Resolution:   req.Resolution,
			CaptchaToken: captcha,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{
				"error":       err.Error(),
				"captcha_len": len(captcha),
			})
		}

		// Include the raw frames: the parsed view hides the shape, and the shape
		// is what tells us where the resolved URL actually lives.
		raw, _ := client.UpscaleImageRaw(ctx, batchexecute.ImageUpscaleRequest{
			ProjectID:    req.ProjectID,
			MediaID:      req.MediaID,
			ContentID:    req.ContentID,
			Resolution:   req.Resolution,
			CaptchaToken: captcha,
		})

		// The verbatim body: when the parsed view is nil this is the only place
		// the server's actual answer is still legible.
		body, _ := client.UpscaleImageRawBody(ctx, batchexecute.ImageUpscaleRequest{
			ProjectID:    req.ProjectID,
			MediaID:      req.MediaID,
			ContentID:    req.ContentID,
			Resolution:   req.Resolution,
			CaptchaToken: captcha,
		})
		if len(body) > 3000 {
			body = body[:3000]
		}

		return c.JSON(fiber.Map{
			"status":      "ok",
			"captcha_len": len(captcha),
			"resolution":  req.Resolution,
			"media":       media,
			"raw":         raw,
			"raw_body":    body,
			"fingerprint": eng.Fingerprint(),
		})
	})

	// Dump raw CDP events whose payload contains a substring.
	//
	// Diagnostics only. requestWillBeSentExtraInfo carries the headers the
	// network stack actually sent, Cookie included, which requestWillBeSent
	// does not — and that is the only place the real header set is visible.
	app.Post("/v1/debug/events-raw", func(c fiber.Ctx) error {
		var req struct {
			Filter string `json:"filter"`
			Type   string `json:"type"`
			Limit  int    `json:"limit"`
		}
		_ = c.Bind().JSON(&req)
		if req.Limit <= 0 {
			req.Limit = 2000
		}

		client := pickClient(c)
		if client == nil || !client.Connected() {
			return c.Status(503).JSON(fiber.Map{"error": "no extension connected"})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		events, err := client.ReadEvents(ctx, req.Limit)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}

		out := []fiber.Map{}
		for _, ev := range events {
			if req.Type != "" && ev.Method != req.Type {
				continue
			}
			if req.Filter != "" && !strings.Contains(string(ev.Params), req.Filter) {
				continue
			}
			out = append(out, fiber.Map{"type": ev.Method, "params": json.RawMessage(ev.Params)})
		}
		return c.JSON(fiber.Map{"count": len(out), "events": out})
	})

	// Hand back a Labs access token, so the legacy aisandbox REST surface can be
	// probed from outside the process.
	//
	// Diagnostics only. The legacy endpoints are what the previous Python engine
	// used; whether they still answer decides whether image-to-video and friends
	// can be added from that reference or have to be re-derived for batchexecute.
	app.Post("/v1/debug/labs-token", func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		token, err := eng.LabsAccessToken(ctx)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{
			"token":   token,
			"length":  len(token),
			"api_key": config.LabsAPIKey(),
			"base":    config.LabsAPIBase(),
		})
	})

	// Report the exact headers a batchexecute call would go out with.
	//
	// Diagnostics only. A rejected call says nothing about which header was
	// wrong, so the resolved set has to be inspectable to compare it against the
	// browser's own request.
	app.Post("/v1/debug/headers", func(c fiber.Ctx) error {
		var req struct {
			HeaderOverrides map[string]string `json:"header_overrides"`
		}
		_ = c.Bind().JSON(&req)

		jar := eng.Bridge().Jar()
		if jar == nil {
			return c.Status(503).JSON(fiber.Map{"error": "no cookies loaded"})
		}
		hc, err := httpx.New(httpx.WithTimeout(time.Duration(config.RequestTimeout) * time.Second))
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		client := eng.NewBatchexecuteClient(jar, hc)
		client.SetHeaderOverrides(req.HeaderOverrides)

		headers, cookies := client.ResolvedHeaders()
		cookieHead := cookies
		if len(cookieHead) > 160 {
			cookieHead = cookieHead[:160] + "..."
		}
		return c.JSON(fiber.Map{
			"headers":     headers,
			"cookie_len":  len(cookies),
			"cookie_head": cookieHead,
			"fingerprint": eng.Fingerprint(),
		})
	})

	// Mint a reCAPTCHA token through the configured chain and hand it back.
	//
	// Diagnostics only: it exists so a token produced by the Go-side provider can
	// be replayed from the browser, which separates "the token is wrong" from
	// "the request carrying it is wrong".
	app.Post("/v1/debug/captcha", func(c fiber.Ctx) error {
		var req struct {
			Action string `json:"action"`
		}
		_ = c.Bind().JSON(&req)
		if req.Action == "" {
			req.Action = "IMAGE_GENERATION"
		}

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		token, err := eng.CaptchaToken(ctx, req.Action)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{
			"action": req.Action,
			"length": len(token),
			"token":  token,
		})
	})

	// Submit a video upscale over the Go transport and report the new asset id.
	//
	// Diagnostics only: it exists to establish whether this RPC works from Go at
	// all, which the image upscale (SPrCad) does not.
	app.Post("/v1/debug/video-upscale", func(c fiber.Ctx) error {
		var req struct {
			ContentID string `json:"content_id"`
			MediaID   string `json:"media_id"`
			ProjectID string `json:"project_id"`
			Model     string `json:"model"`
		}
		if err := c.Bind().JSON(&req); err != nil || req.ContentID == "" || req.MediaID == "" {
			return c.Status(400).JSON(fiber.Map{"error": "content_id and media_id are required"})
		}
		if req.ProjectID == "" {
			req.ProjectID = eng.ProjectID()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		captcha, err := eng.CaptchaToken(ctx, "VIDEO_GENERATION")
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "captcha: " + err.Error()})
		}

		jar := eng.Bridge().Jar()
		if jar == nil {
			return c.Status(503).JSON(fiber.Map{"error": "no cookies loaded"})
		}
		hc, err := httpx.New(httpx.WithTimeout(time.Duration(config.RequestTimeout) * time.Second))
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		client := eng.NewBatchexecuteClient(jar, hc)
		frames, err := client.Upscale(ctx, batchexecute.UpscaleRequest{
			ProjectID:    req.ProjectID,
			ContentID:    req.ContentID,
			MediaID:      req.MediaID,
			Model:        req.Model,
			CaptchaToken: captcha,
		}, batchexecute.CallOptions{
			SourcePath: "/project/" + req.ProjectID + "/edit/" + req.MediaID,
			BuildLabel: config.BuildLabel(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{
				"error":       err.Error(),
				"captcha_len": len(captcha),
			})
		}

		assetID, parseErr := batchexecute.ParseUpscaledAssetID(frames)
		raw := make([]json.RawMessage, 0, len(frames))
		for _, f := range frames {
			raw = append(raw, f.Payload)
		}

		out := fiber.Map{
			"status":      "ok",
			"captcha_len": len(captcha),
			"frames":      raw,
		}
		if parseErr != nil {
			out["parse_error"] = parseErr.Error()
		} else {
			out["upscaled_asset_id"] = assetID
		}
		return c.JSON(out)
	})

	// Upload an image and return its media id, for use as a condition image.
	//
	// Accepts either a local path (the CLI's case) or base64 bytes (the HTTP
	// case). The id this returns is the one a generation conditions on.
	app.Post("/v1/media/upload", func(c fiber.Ctx) error {
		var req struct {
			Path       string `json:"path"`
			FileName   string `json:"file_name"`
			MimeType   string `json:"mime_type"`
			DataBase64 string `json:"data_base64"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}

		var data []byte
		switch {
		case req.Path != "":
			raw, err := os.ReadFile(req.Path)
			if err != nil {
				return c.Status(400).JSON(fiber.Map{"error": err.Error()})
			}
			data = raw
			if req.FileName == "" {
				req.FileName = filepath.Base(req.Path)
			}
		case req.DataBase64 != "":
			raw, err := base64.StdEncoding.DecodeString(req.DataBase64)
			if err != nil {
				return c.Status(400).JSON(fiber.Map{"error": "data_base64 is not valid base64: " + err.Error()})
			}
			data = raw
		default:
			return c.Status(400).JSON(fiber.Map{"error": "path or data_base64 is required"})
		}

		if req.FileName == "" {
			req.FileName = "upload"
		}
		// The app stores the name with spaces replaced by underscores.
		req.FileName = strings.ReplaceAll(req.FileName, " ", "_")
		if req.MimeType == "" {
			req.MimeType = http.DetectContentType(data)
		}
		if !strings.HasPrefix(req.MimeType, "image/") {
			return c.Status(400).JSON(fiber.Map{
				"error": "only images can be uploaded, got " + req.MimeType,
			})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		mediaID, contentID, err := eng.UploadImageViaBatch(ctx, data, req.MimeType, req.FileName)
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{
			"status": "ok",
			// Both ids are returned because they are not interchangeable: a
			// generation's start_image/end_image take the content id, while the
			// media id is what addresses the asset elsewhere.
			"media_id":   mediaID,
			"content_id": contentID,
			"file_name":  req.FileName,
			"mime_type":  req.MimeType,
			"bytes":      len(data),
		})
	})

	// Submit a video generation with the RPC and model chosen explicitly, and
	// return the raw frames.
	//
	// Diagnostics only. Which RPC a submission goes to depends on whether images
	// are attached, and that mapping is the thing under investigation for the
	// models that do not work yet — so it has to be overridable.
	app.Post("/v1/debug/video-submit", func(c fiber.Ctx) error {
		var req struct {
			RPC       string `json:"rpc"`
			Model     string `json:"model"`
			Prompt    string `json:"prompt"`
			Mode      int    `json:"mode"`
			StartID   string `json:"start_image"`
			EndID     string `json:"end_image"`
			ProjectID string `json:"project_id"`
		}
		if err := c.Bind().JSON(&req); err != nil || req.Model == "" {
			return c.Status(400).JSON(fiber.Map{"error": "model is required"})
		}
		if req.Prompt == "" {
			req.Prompt = "a test prompt"
		}
		if req.ProjectID == "" {
			req.ProjectID = eng.ProjectID()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		captcha, err := eng.CaptchaToken(ctx, "VIDEO_GENERATION")
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "captcha: " + err.Error()})
		}
		jar := eng.Bridge().Jar()
		if jar == nil {
			return c.Status(503).JSON(fiber.Map{"error": "no cookies loaded"})
		}
		hc, err := httpx.New(httpx.WithTimeout(time.Duration(config.RequestTimeout) * time.Second))
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		client := eng.NewBatchexecuteClient(jar, hc)

		genReq := batchexecute.GenerateVideoRequest{
			ProjectID:    req.ProjectID,
			Model:        req.Model,
			Prompt:       req.Prompt,
			CaptchaToken: captcha,
			StartImage:   req.StartID,
			EndImage:     req.EndID,
		}
		if req.Mode > 0 {
			mode := req.Mode
			genReq.ModeOverride = &mode
		}
		frames, err := client.GenerateVideo(ctx, genReq, batchexecute.CallOptions{
			SourcePath: "/project/" + req.ProjectID,
			BuildLabel: config.BuildLabel(),
			RPCID:      req.RPC,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error(), "captcha_len": len(captcha)})
		}
		raw := make([]json.RawMessage, 0, len(frames))
		for _, f := range frames {
			raw = append(raw, f.Payload)
		}
		// Report the id that was actually used, not the one that was requested:
		// leaving rpc empty means "let the conditioning decide", and that is the
		// interesting value when a submission comes back empty.
		effectiveRPC := req.RPC
		if effectiveRPC == "" {
			effectiveRPC = batchexecute.VideoRPCID(genReq)
		}
		return c.JSON(fiber.Map{"status": "ok", "rpc": effectiveRPC, "model": req.Model, "frames": raw})
	})

	// Send an argument captured off the wire to the RPC it was captured from.
	//
	// Every other diagnostic builds its argument from a Go struct, which is no
	// help when the question is whether a payload seen in the app's own traffic
	// works when replayed. This one takes the argument verbatim. The literal
	// string "__CAPTCHA__" anywhere inside it is replaced with a fresh reCAPTCHA
	// token, since a captured one is single-use and long expired.
	app.Post("/v1/debug/raw-rpc", func(c fiber.Ctx) error {
		var req struct {
			RPC       string          `json:"rpc"`
			Arg       json.RawMessage `json:"arg"`
			ProjectID string          `json:"project_id"`
			Action    string          `json:"action"`
			// SourcePath overrides the path the call claims to come from. The
			// editor route is required by some RPCs and rejected by others, and
			// which is which has moved at least once — so being able to vary it is
			// the difference between testing that and guessing at it.
			SourcePath string `json:"source_path"`
			// AuthUser selects which signed-in Google account the call acts as.
			// Several RPCs answer differently per account, and without this the
			// route could only ever exercise the default one — which is exactly
			// how a per-account value gets mistaken for a global one.
			AuthUser int `json:"authuser"`
		}
		if err := c.Bind().JSON(&req); err != nil || req.RPC == "" || len(req.Arg) == 0 {
			return c.Status(400).JSON(fiber.Map{"error": "rpc and arg are required"})
		}
		if req.ProjectID == "" {
			req.ProjectID = eng.ProjectID()
		}
		if req.Action == "" {
			req.Action = "VIDEO_GENERATION"
		}

		var arg any
		if err := json.Unmarshal(req.Arg, &arg); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "arg must be JSON: " + err.Error()})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		// Mint a token only when the argument actually carries the placeholder.
		//
		// Most RPCs here take no reCAPTCHA at all, and demanding one for them
		// made every such probe fail with "no reCAPTCHA provider configured"
		// before the request under test was ever sent — which reads like a
		// verdict on the RPC rather than on the token.
		captcha := ""
		if bytes.Contains(req.Arg, []byte(captchaPlaceholder)) {
			token, err := eng.CaptchaToken(ctx, req.Action)
			if err != nil {
				return c.Status(502).JSON(fiber.Map{"error": "captcha: " + err.Error()})
			}
			captcha = token
		}
		jar := eng.Bridge().Jar()
		if jar == nil {
			return c.Status(503).JSON(fiber.Map{"error": "no cookies loaded"})
		}
		hc, err := httpx.New(httpx.WithTimeout(time.Duration(config.RequestTimeout) * time.Second))
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		replaced := replacePlaceholder(arg, captchaPlaceholder, captcha)
		client := eng.NewBatchexecuteClient(jar, hc)
		client.SetAuthUser(req.AuthUser)
		// Drop the seeded anti-CSRF token when acting as another account.
		//
		// The seed belongs to whichever account the page is showing, and sending
		// it alongside a different `authuser` is answered with 400 — so without
		// this the route can only ever exercise the default account, which is the
		// exact blind spot it exists to remove.
		if req.AuthUser != 0 {
			client.SeedToken("")
		}
		sourcePath := req.SourcePath
		if sourcePath == "" {
			sourcePath = "/project/" + req.ProjectID
		}
		frames, err := client.CallWith(ctx, req.RPC, replaced, batchexecute.CallOptions{
			SourcePath: sourcePath,
			BuildLabel: config.BuildLabel(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error(), "captcha_len": len(captcha)})
		}
		raw := make([]json.RawMessage, 0, len(frames))
		for _, f := range frames {
			raw = append(raw, f.Payload)
		}
		return c.JSON(fiber.Map{"status": "ok", "rpc": req.RPC, "frames": raw})
	})

	app.Post("/v1/videos/generations", handleVideoGeneration(eng))
	app.Post("/v1/images/generations", handleImageGeneration(eng))
	// Submit a generation through the attached browser tab, using this engine's
	// own credentials and a browser-minted reCAPTCHA token.
	//
	// Diagnostic. It answers the question that decides where a
	// PUBLIC_ERROR_UNUSUAL_ACTIVITY rejection comes from: is the account refused,
	// or is this process's HTTP client refused? If the page can submit with the
	// engine's token, the account is fine and the transport is the difference.
	app.Post("/v1/debug/browser-submit", func(c fiber.Ctx) error {
		var req struct {
			Prompt   string `json:"prompt"`
			Duration int    `json:"duration"`
			Aspect   string `json:"aspect"`
		}
		if err := c.Bind().JSON(&req); err != nil || req.Prompt == "" {
			return c.Status(400).JSON(fiber.Map{"error": "prompt is required"})
		}
		if req.Duration == 0 {
			req.Duration = 4
		}
		if req.Aspect == "" {
			req.Aspect = "landscape"
		}

		extension := br.Current()
		if extension == nil || !extension.Connected() {
			return c.Status(503).JSON(fiber.Map{"error": "no extension connected"})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		token, err := eng.AccessToken(ctx)
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": err.Error()})
		}
		captcha, err := eng.CaptchaToken(ctx, "VIDEO_GENERATION")
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "captcha: " + err.Error()})
		}

		aspectEnum := "VIDEO_ASPECT_RATIO_LANDSCAPE"
		if strings.EqualFold(req.Aspect, "portrait") {
			aspectEnum = "VIDEO_ASPECT_RATIO_PORTRAIT"
		}

		modelKey := config.VideoModels[req.Duration]
		if modelKey == "" {
			return c.Status(400).JSON(fiber.Map{"error": "unsupported duration"})
		}

		payload := map[string]any{
			"mediaGenerationContext": map[string]any{"batchId": uuid.NewString()},
			"clientContext": map[string]any{
				"recaptchaContext": map[string]any{
					"token":           captcha,
					"applicationType": "RECAPTCHA_APPLICATION_TYPE_WEB",
				},
				"sessionId": fmt.Sprintf(";%d", time.Now().UnixMilli()),
				"tool":      config.ClientCtx.Tool,
				"projectId": eng.ProjectID(),
			},
			"requests": []map[string]any{{
				"aspectRatio": aspectEnum,
				"textInput": map[string]any{
					"structuredPrompt": map[string]any{
						"parts": []map[string]any{{"text": req.Prompt}},
					},
				},
				"videoModelKey": modelKey,
				"seed":          12345,
				"metadata":      map[string]any{},
			}},
			"useV2ModelConfig": true,
		}

		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		endpoint := config.APIBase + config.Endpoints["generate_t2v"] +
			"?key=" + config.APIKey()

		// The request is issued by the page itself, so it carries the page's own
		// origin, TLS session, cookies and network path.
		expression := fmt.Sprintf(`(async () => {
  try {
    const r = await fetch(%q, {
      method: "POST",
      headers: {Authorization: "Bearer " + %q, "Content-Type": "text/plain;charset=UTF-8"},
      body: %q,
    });
    return {status: r.status, body: (await r.text()).slice(0, 600)};
  } catch (e) { return {error: String(e)}; }
})()`, endpoint, token, string(payloadJSON))

		if _, err := extension.Attach(ctx, 0); err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		raw, err := extension.Evaluate(ctx, expression)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}

		var outcome map[string]any
		_ = json.Unmarshal(raw, &outcome)
		return c.JSON(fiber.Map{
			"note":        "submitted from the page, using the engine's own token",
			"captcha_len": len(captcha),
			"project_id":  eng.ProjectID(),
			"outcome":     outcome,
		})
	})
	app.Post("/v1/videos/upscale", handleUpscale(eng))
	app.Post("/v1/videos/edit", handleVideoEdit(eng))
	app.Post("/v1/videos/reference", handleVideoReference(eng))
	app.Post("/v1/images/upscale", handleImageUpscale(eng))

	app.Get("/v1/jobs", func(c fiber.Ctx) error {
		limit, _ := strconv.Atoi(c.Query("limit", "50"))
		jobs, err := eng.Store().RecentGenerations(limit)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"jobs": jobs})
	})

	app.Get("/v1/jobs/:id", func(c fiber.Ctx) error {
		job, err := eng.Store().GenerationByJob(c.Params("id"))
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "no such job"})
		}
		return c.JSON(job)
	})

	app.Get("/v1/media", func(c fiber.Ctx) error {
		limit, _ := strconv.Atoi(c.Query("limit", "50"))
		media, err := eng.Store().RecentMedia(limit)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"media": media})
	})

	app.Get("/v1/accounts", func(c fiber.Ctx) error {
		accounts, err := eng.Store().ListAccounts()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"accounts": accounts})
	})

	// The account's projects, over the transport. This is the browser-free route
	// to a project id: it needs cookies and nothing else.
	app.Get("/v1/projects", func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		projects, err := eng.ListProjects(ctx)
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{
			"projects": projects,
			"count":    len(projects),
			// The listing is most-recently-modified first, so this is the
			// project a run would pick when it has not been told one.
			"most_recent": firstProjectID(projects),
		})
	})

	// Create a project. Over the transport, so no browser and no click.
	app.Post("/v1/projects", func(c fiber.Ctx) error {
		var req struct {
			Label string `json:"label"`
		}
		_ = c.Bind().JSON(&req)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		project, err := eng.CreateProject(ctx, req.Label)
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.Status(201).JSON(fiber.Map{"project": project})
	})

	// Switch which signed-in Google account the engine acts as.
	//
	// A browser can hold several accounts at once and they share one cookie jar,
	// so the account is selected by the `authuser` index rather than by the
	// cookies. Switching re-bootstraps, because the session, the account row and
	// the project all have to move together — otherwise a project belonging to
	// one account gets submitted under another's session and comes back empty.
	app.Post("/v1/accounts/switch", func(c fiber.Ctx) error {
		var req struct {
			Index int `json:"index"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()

		if err := eng.SetAccountIndex(ctx, req.Index); err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{
			"status":  "ok",
			"index":   eng.AccountIndex(),
			"account": eng.AccountID(),
			"project": eng.ProjectID(),
			"ready":   eng.Ready(),
		})
	})

	// Every signed-in account's balance, one session mint per account.
	app.Get("/v1/accounts/credits", func(c fiber.Ctx) error {
		max := 4
		if raw := strings.TrimSpace(c.Query("max")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				max = n
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		rows, err := eng.AccountsCredits(ctx, max)
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}

		// The total counts only the balances that were actually read. A failed
		// read used to be reported as 0, which made a wrong total look like a
		// fact — and the count of what went into it is returned beside it so the
		// difference is visible.
		total, counted := engine.TotalCredits(rows)

		// One row for a request that asked for more is "one account visible", not
		// "one account signed in": the session endpoint answers for the default
		// account whatever `authuser` says, so any further signed-in accounts are
		// unreachable from here. Presenting a total without that caveat would
		// claim a completeness this call cannot establish.
		note := ""
		if len(rows) <= 1 && max > 1 {
			note = "only one account is visible. The session endpoint answers for the " +
				"default account regardless of `authuser`, so any other signed-in accounts " +
				"cannot be read here and this total covers one of them."
		}

		return c.JSON(fiber.Map{
			"active_index":     eng.AccountIndex(),
			"accounts":         rows,
			"total_credits":    total,
			"accounts_counted": counted,
			"accounts_listed":  len(rows),
			"note":             note,
		})
	})

	// Which signed-in account would pay for a job of a given cost.
	//
	// Read-only: it reports the decision without moving to the account or
	// submitting anything, so the choice can be inspected and checked against the
	// balances it was made from. `?cost=` is what the render is expected to cost,
	// i.e. config.VideoCost(duration, quality) × count.
	app.Get("/v1/accounts/affordable", func(c fiber.Ctx) error {
		cost := 0
		if raw := strings.TrimSpace(c.Query("cost")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return c.Status(400).JSON(fiber.Map{
					"error": "cost must be a non-negative integer",
				})
			}
			cost = n
		}

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		choice, err := eng.ChooseAccountFor(ctx, cost)
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(choice)
	})

	// Fallback for operators who would rather push a cookie dump than run the
	// extension. Same scope rules apply: only domains the bridge is configured
	// for are accepted.
	app.Post("/api/sync-cookies", func(c fiber.Ctx) error {
		var req CookieSyncRequest
		if err := c.Bind().JSON(&req); err != nil || len(req.Cookies) == 0 {
			return c.Status(400).JSON(fiber.Map{"error": "expected a non-empty cookies array"})
		}

		allowed := map[string]bool{}
		for _, domain := range br.CookieDomains {
			allowed[domain] = true
			allowed["."+domain] = true
		}

		converted := make([]cookiejar.Cookie, 0, len(req.Cookies))
		skipped := 0
		for _, ck := range req.Cookies {
			if !domainAllowedFor(ck.Domain, br.CookieDomains) {
				skipped++
				continue
			}
			converted = append(converted, cookiejar.Cookie{
				Domain:         ck.Domain,
				ExpirationDate: ck.ExpirationDate,
				HostOnly:       ck.HostOnly,
				HTTPOnly:       ck.HTTPOnly,
				Name:           ck.Name,
				Path:           ck.Path,
				SameSite:       ck.SameSite,
				Secure:         ck.Secure,
				Session:        ck.Session,
				StoreID:        ck.StoreID,
				Value:          ck.Value,
			})
		}

		if len(converted) == 0 {
			return c.Status(400).JSON(fiber.Map{
				"error":   "no cookies matched the configured scope",
				"allowed": br.CookieDomains,
			})
		}

		jar := cookiejar.FromCookies(converted, "api/sync-cookies")
		path := filepath.Join("cookies", "cookies.json")
		if err := jar.Save(path); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		// Reload the engine so the new cookies take effect immediately.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := eng.Bootstrap(ctx); err != nil {
				log.Printf("server: reload after cookie sync failed: %v", err)
			}
		}()

		return c.JSON(fiber.Map{
			"status":          "ok",
			"accepted":        len(converted),
			"skipped":         skipped,
			"has_credentials": jar.HasAuthCookies(),
			"path":            path,
		})
	})

	app.Get("/output/*", func(c fiber.Ctx) error {
		return c.SendFile(filepath.Join("output", c.Params("*")))
	})
}

func recoverer(c fiber.Ctx) error {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("server: recovered from panic: %v", r)
			_ = c.Status(500).JSON(fiber.Map{"error": "internal error"})
		}
	}()
	return c.Next()
}

func healthStatus(eng *engine.Engine, br *bridge.Bridge) string {
	switch {
	case eng.Ready():
		return "ok"
	case br.Connected():
		return "syncing"
	default:
		return "waiting_for_browser"
	}
}

func domainAllowedFor(domain string, allowed []string) bool {
	for _, candidate := range allowed {
		bare := candidate
		if len(bare) > 0 && bare[0] == '.' {
			bare = bare[1:]
		}
		if domain == candidate || domain == bare || domain == "."+bare {
			return true
		}
		if len(domain) > len(bare)+1 && domain[len(domain)-len(bare)-1:] == "."+bare {
			return true
		}
	}
	return false
}

/* ------------------------------------------------------------------ *
 * Handlers
 * ------------------------------------------------------------------ */

func handleVideoGeneration(eng *engine.Engine) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req VideoGenerationRequest
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}
		if req.Prompt == "" {
			return c.Status(400).JSON(fiber.Map{"error": "prompt is required"})
		}

		if unsupported := unsupportedVideoOptions(req); len(unsupported) > 0 {
			return c.Status(400).JSON(fiber.Map{
				"error": "unsupported option(s): " + strings.Join(unsupported, ", ") +
					" — not implemented on the batchexecute transport",
			})
		}

		// Video goes over the batchexecute transport, the same one the app uses.
		// The aisandbox REST path this endpoint used to call is legacy and cannot
		// work: its key blocks the app's own origin.
		outcome, err := eng.GenerateVideoViaBatch(c.Context(), engine.BatchVideoRequest{
			Prompt:     req.Prompt,
			Model:      req.Model,
			Quality:    req.Quality,
			Duration:   req.Duration,
			Count:      req.Count,
			ProjectID:  "",
			Wait:       boolOr(req.Wait, false),
			Download:   boolOr(req.Download, true),
			StartImage: req.StartImage,
			EndImage:   req.EndImage,
			StartFrame: req.StartFrame,
			EndFrame:   req.EndFrame,
		})
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(outcome)
	}
}

// unsupportedVideoOptions names the video-generation fields the transport does not
// implement. Note that `model` is *not* here: the engine accepts it and the
// handler used to drop it, which made the field silently ineffective.
func unsupportedVideoOptions(req VideoGenerationRequest) []string {
	var out []string
	if strings.TrimSpace(req.Aspect) != "" {
		out = append(out, "aspect")
	}
	if strings.TrimSpace(req.Resolution) != "" {
		out = append(out, "resolution")
	}
	if req.Seed != nil {
		out = append(out, "seed")
	}
	if len(req.ReferenceImages) > 0 {
		out = append(out, "reference_images")
	}
	if strings.TrimSpace(req.AudioPreference) != "" {
		out = append(out, "audio_preference")
	}
	return out
}
func handleImageGeneration(eng *engine.Engine) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req ImageGenerationRequest
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}
		if req.Prompt == "" {
			return c.Status(400).JSON(fiber.Map{"error": "prompt is required"})
		}

		// The request type accepts more than the batchexecute path implements.
		// Ignoring those fields returns a wrong result with a 200 — ask for four
		// images and get one, with nothing to say why. Refuse them instead.
		if unsupported := unsupportedImageOptions(req); len(unsupported) > 0 {
			return c.Status(400).JSON(fiber.Map{
				"error": "unsupported option(s): " + strings.Join(unsupported, ", ") +
					" — not implemented on the batchexecute transport",
			})
		}

		// Images go over the batchexecute transport. The aisandbox REST path in
		// internal/flowapi cannot work: the app moved to flow.google.com and the
		// API key that path depends on blocks that origin.
		outcome, err := eng.GenerateImageViaBatch(c.Context(), engine.BatchImageRequest{
			Prompt:   req.Prompt,
			Model:    req.Model,
			Download: boolOr(req.Download, true),
		})
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(outcome)
	}
}

// unsupportedImageOptions names the image-generation fields the transport does
// not implement, so the caller is told rather than quietly given something else.
func unsupportedImageOptions(req ImageGenerationRequest) []string {
	var out []string
	if req.Count > 1 {
		out = append(out, "count")
	}
	if strings.TrimSpace(req.Aspect) != "" {
		out = append(out, "aspect")
	}
	if req.Seed != nil {
		out = append(out, "seed")
	}
	if len(req.ReferenceIDs) > 0 {
		out = append(out, "reference_images")
	}
	return out
}
func handleUpscale(eng *engine.Engine) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req UpscaleRequest
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}
		if req.MediaID == "" {
			return c.Status(400).JSON(fiber.Map{"error": "media_id is required"})
		}
		if req.Resolution == "" {
			req.Resolution = "1080p"
		}

		// This is the same pass the Flow download menu offers as "1080p /
		// Upscaled". The legacy REST upsampler this endpoint used to call is dead.
		outcome, err := eng.UpscaleViaBatch(c.Context(), engine.BatchUpscaleRequest{
			MediaID:    req.MediaID,
			Resolution: req.Resolution,
			Wait:       true,
			Download:   boolOr(req.Download, true),
		})
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(outcome)
	}
}

// handleVideoReference submits a generation conditioned on reference images.
//
// This is the abra_r2v_* family. It goes to MZZa6b with a payload that puts the
// prompt at index 0 and the reference list at index 1 — neither the i2v shape nor
// the edit one, which is why it took a live capture to find.
func handleVideoReference(eng *engine.Engine) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req VideoReferenceRequest
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}
		if strings.TrimSpace(req.Prompt) == "" {
			return c.Status(400).JSON(fiber.Map{"error": "prompt is required"})
		}
		if len(req.References) == 0 {
			return c.Status(400).JSON(fiber.Map{"error": "references is required"})
		}

		outcome, err := eng.GenerateVideoFromReferencesViaBatch(c.Context(), engine.BatchReferenceRequest{
			References: req.References,
			Prompt:     req.Prompt,
			Model:      req.Model,
			Duration:   req.Duration,
			ProjectID:  req.ProjectID,
			Wait:       boolOr(req.Wait, true),
			Download:   boolOr(req.Download, false),
		})
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(outcome)
	}
}

func boolOr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

// handleVideoEdit submits an edit of an existing asset.
//
// This is abra_edit — the video-to-video model, reached in the app from the
// Ingredients composer mode once an asset is attached. It goes to jIps6, which
// is both a different RPC and a different payload shape from a generation.
func handleVideoEdit(eng *engine.Engine) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req VideoEditRequest
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}
		if strings.TrimSpace(req.Source) == "" {
			return c.Status(400).JSON(fiber.Map{"error": "source is required"})
		}
		if strings.TrimSpace(req.Prompt) == "" {
			return c.Status(400).JSON(fiber.Map{"error": "prompt is required"})
		}

		outcome, err := eng.EditVideoViaBatch(c.Context(), engine.BatchEditRequest{
			Source:    req.Source,
			Prompt:    req.Prompt,
			Model:     req.Model,
			ProjectID: req.ProjectID,
			Wait:      boolOr(req.Wait, true),
			Download:  boolOr(req.Download, false),
		})
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(outcome)
	}
}

// handleImageUpscale resolves an image at a higher resolution.
//
// This runs the app's own SPrCad request inside the attached tab. The Go
// transport cannot make this call — see engine.UpscaleImage for the elimination
// that established it — so the browser does the request and hands back the bytes.
func handleImageUpscale(eng *engine.Engine) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req ImageUpscaleRequest
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}
		if req.ContentID == "" && req.MediaID == "" {
			return c.Status(400).JSON(fiber.Map{"error": "media_id or content_id is required"})
		}
		if req.ProjectID == "" {
			req.ProjectID = eng.ProjectID()
		}
		// A caller normally holds the media id — it is what a generation returns
		// and what the editor URL carries — while SPrCad takes the content id.
		// Resolve it rather than making every caller know both.
		if req.ContentID == "" {
			contentID, err := eng.ResolveContentID(c.Context(), req.MediaID)
			if err != nil {
				return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
			}
			req.ContentID = contentID
		}

		resolution, label, err := imageResolution(req.Resolution)
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": err.Error()})
		}

		start := time.Now()
		result, err := eng.UpscaleImage(c.Context(), req.ProjectID, req.MediaID, req.ContentID, resolution)
		if err != nil {
			return c.Status(statusFor(err)).JSON(fiber.Map{"error": err.Error()})
		}

		response := fiber.Map{
			"status":          "succeeded",
			"media_id":        req.MediaID,
			"content_id":      req.ContentID,
			"resolution":      label,
			"media_type":      result.MediaType,
			"bytes":           result.Bytes,
			"elapsed_seconds": time.Since(start).Seconds(),
		}

		if boolOr(req.Download, true) {
			ext := ".bin"
			switch result.MediaType {
			case "image/jpeg":
				ext = ".jpg"
			case "image/png":
				ext = ".png"
			}
			name := fmt.Sprintf("upscale-%s-%s%s", shortID(req.MediaID), strings.ToLower(label), ext)
			if err := os.MkdirAll("output", 0o755); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
			if err := os.WriteFile(filepath.Join("output", name), result.Data, 0o644); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
			response["file"] = "output/" + name
			response["url"] = "/output/" + name
		}

		return c.JSON(response)
	}
}

// imageResolution maps the download menu's label onto SPrCad's selector.
//
// "1K | Original size" is not an upscale: it is the size the asset already has,
// which its media URL serves directly, so asking for it is an error rather than a
// silent no-op.
func imageResolution(label string) (int, string, error) {
	switch strings.ToUpper(strings.TrimSpace(label)) {
	case "", "2K", "2":
		return engine.Resolution2K, "2K", nil
	case "4K", "4":
		return engine.Resolution4K, "4K", nil
	case "1K", "1", "ORIGINAL":
		return 0, "", fmt.Errorf(
			"1K is the asset's original size and needs no upscale; ask for 2K or 4K")
	default:
		return 0, "", fmt.Errorf("resolution must be 2K or 4K, not %q", label)
	}
}

// withoutAuthCookies rebuilds a jar with the session cookies removed.
//
// Diagnostics only. It exists so a 401 can be produced on demand: the recovery
// path that handles one is otherwise unreachable until a session happens to
// expire, which is exactly when it must work.
func withoutAuthCookies(jar *cookiejar.Jar) *cookiejar.Jar {
	if jar == nil {
		return nil
	}
	auth := make(map[string]bool, len(cookiejar.AuthCookieNames))
	for _, name := range cookiejar.AuthCookieNames {
		auth[name] = true
	}
	kept := make([]cookiejar.Cookie, 0, len(jar.Cookies()))
	for _, ck := range jar.Cookies() {
		if auth[ck.Name] {
			continue
		}
		kept = append(kept, ck)
	}
	return cookiejar.FromCookies(kept, "diagnostic-no-credentials")
}

// shortID trims an id to something usable in a filename.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// captchaPlaceholder is the literal a captured payload carries where a
// reCAPTCHA token used to be. The token is single-use and long expired by the
// time a capture is replayed, so the capture stores this instead and the
// placeholder is swapped for a fresh token at call time.
const captchaPlaceholder = "__CAPTCHA__"

// bridgeTokenMatches reports whether the request carries the bridge token.
//
// The token is read from disk rather than from the bridge because the bridge does
// not expose it. The file is where it lives, and both processes read the same one,
// so a comparison against the file is a comparison against the same secret the
// extension had to prove it held.
//
// The comparison is constant-time. This is a shared secret standing in front of the
// account's cookies, and a byte-at-a-time compare leaks its prefix.
func bridgeTokenMatches(c fiber.Ctx) bool {
	raw, err := os.ReadFile(filepath.Join(config.DataDir(), "bridge-token"))
	if err != nil {
		return false
	}
	want := strings.TrimSpace(string(raw))
	if want == "" {
		return false
	}

	supplied := strings.TrimSpace(c.Get("X-Bridge-Token"))
	if supplied == "" {
		// Also accepted as a query parameter, so a shell can use it without
		// quoting a header.
		supplied = strings.TrimSpace(c.Query("token"))
	}
	if supplied == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(supplied)) == 1
}

// replacePlaceholder returns value with every string equal to placeholder
// replaced by with, recursing through arrays and objects.
//
// It exists so a payload captured off the wire can be replayed verbatim: the
// reCAPTCHA token inside a capture is single-use and long expired, so it is
// stored as a literal placeholder and swapped for a fresh one at call time.
// The value is returned unchanged when the placeholder is absent.
func replacePlaceholder(value any, placeholder, with string) any {
	switch v := value.(type) {
	case string:
		if v == placeholder {
			return with
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = replacePlaceholder(item, placeholder, with)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = replacePlaceholder(item, placeholder, with)
		}
		return out
	default:
		return value
	}
}

// statusFor maps an engine error onto an HTTP status. A not-ready engine is a
// 503 rather than a 500: the caller should retry once the browser has synced.
// firstProjectID names the project a run would pick from a listing, or "" when
// the account has none.
func firstProjectID(projects []batchexecute.Project) string {
	if len(projects) == 0 {
		return ""
	}
	return projects[0].ID
}

func statusFor(err error) int {
	if err == nil {
		return 200
	}
	message := err.Error()
	switch {
	// A refusal to spend money the account does not have is not an upstream
	// failure and not a bad argument. It is the one outcome the caller can fix by
	// topping up or asking for less, so it gets its own status rather than being
	// reported as 502 alongside transport errors.
	case strings.Contains(message, "insufficient credits"):
		return 402
	case strings.Contains(message, "not ready"),
		strings.Contains(message, "no cookies available"),
		strings.Contains(message, "no worker available"),
		strings.Contains(message, "all workers failed"):
		return 503
	case strings.Contains(message, "required"):
		return 400
	default:
		return 502
	}
}

/* ------------------------------------------------------------------ *
 * Help
 * ------------------------------------------------------------------ */

func help(c fiber.Ctx) error {
	if c.Query("format") == "json" {
		data, _ := json.MarshalIndent(helpSections(), "", "  ")
		return c.Type("json").Send(data)
	}
	c.Type("text")
	return c.SendString(helpText())
}

func helpSections() []map[string]string {
	return []map[string]string{
		{"method": "GET", "path": "/health", "description": "Liveness plus bridge and readiness state"},
		{"method": "GET", "path": "/status", "description": "Account, pool, and bridge snapshot"},
		{"method": "GET", "path": "/stats", "description": "Database aggregates and pool statistics"},
		{"method": "GET", "path": "/v1/workers", "description": "Worker pool detail"},
		{"method": "GET", "path": "/v1/credits", "description": "Refresh and return account balances"},
		{"method": "POST", "path": "/v1/videos/generations", "description": "Submit a video generation"},
		{"method": "POST", "path": "/v1/images/generations", "description": "Submit an image generation"},
		{"method": "POST", "path": "/v1/videos/upscale", "description": "Upscale a finished video to 1080p or 4k"},
		{"method": "POST", "path": "/v1/videos/edit", "description": "Edit an existing asset with abra_edit"},
		{"method": "POST", "path": "/v1/videos/reference", "description": "Generate from reference images with abra_r2v_*"},
		{"method": "POST", "path": "/v1/images/upscale", "description": "Resolve an image at 2K or 4K (needs the browser)"},
		{"method": "GET", "path": "/v1/jobs", "description": "Recent jobs"},
		{"method": "GET", "path": "/v1/jobs/:id", "description": "One job"},
		{"method": "GET", "path": "/v1/media", "description": "Recent generated media"},
		{"method": "GET", "path": "/v1/accounts", "description": "Tracked accounts"},
		{"method": "POST", "path": "/api/sync-cookies", "description": "Push a cookie dump directly (extension fallback)"},
		{"method": "GET", "path": "/output/*", "description": "Generated files"},
	}
}

func helpText() string {
	return `flow-go — Google Flow generation engine

The browser supplies cookies and base information. Everything else — access
tokens, project resolution, generation, polling, upscaling, downloads, storage —
happens in this process. No generation request travels through a browser.

ENDPOINTS
  GET  /health                     liveness, bridge state, readiness
  GET  /status                     account, pool, and bridge snapshot
  GET  /stats                      database aggregates and pool statistics
  GET  /v1/workers                 worker pool detail
  GET  /v1/credits                 refresh and return account balances
  POST /v1/videos/generations      submit a video generation
  POST /v1/images/generations      submit an image generation
  POST /v1/videos/upscale          upscale a finished video to 1080p or 4k
  POST /v1/images/upscale          resolve an image at 2K or 4K (needs the browser)
  GET  /v1/jobs                    recent jobs
  GET  /v1/jobs/:id                one job
  GET  /v1/media                   recent generated media
  GET  /v1/accounts                tracked accounts
  POST /api/sync-cookies           push a cookie dump directly
  GET  /output/*                   generated files

VIDEO REQUEST
  {
    "prompt": "a paper boat on a river",
    "aspect": "landscape",            // or portrait
    "duration": 8,                    // 4, 6, 8, or 10
    "count": 1,                       // 1..4
    "resolution": "1080p",            // "", 720p, 1080p, 4k
    "start_image": "/path/in.png",    // local path or existing media ID
    "reference_images": ["/a.png"],
    "wait": false,                    // false returns a job_id to poll
    "download": true
  }

IMAGE REQUEST
  {
    "prompt": "a single red paper boat",
    "aspect": "square",               // landscape, 4x3, square, 3x4, portrait
    "count": 1,
    "model": "narwhal",               // harbor_seal, narwhal, gem_pix_2
    "download": true
  }

CLI
  flow-go serve                    start the API and the extension bridge
  flow-go generate --prompt ...    generate from the command line
  flow-go stats                    print database statistics
  flow-go export stats.json        export statistics
  flow-go cookies                  show cookie and credential status
`
}

// EnsureOutputDir makes sure the static file route has somewhere to serve from.
func EnsureOutputDir() error {
	return os.MkdirAll("output", 0o755)
}
