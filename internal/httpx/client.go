// Package httpx is a Chrome-impersonating HTTP transport.
//
// Ported from the approach in free-gemini-api/gemini/client.go: a uTLS-based
// client pinned to a real Chrome TLS/JA4 profile, with the exact header order
// Chrome sends, plus an optional HTTP/3 (QUIC) fast path that falls back to
// HTTP/2 when the upstream does not speak QUIC.
//
// Flow's aisandbox backend is behind Google's bot heuristics, so the TLS
// fingerprint matters as much as the headers do. A stock net/http client gets
// challenged; this one does not.
package httpx

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	stdhttp "net/http"
	"runtime"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// ChromeHeaderOrder is the header ordering Chrome uses for XHR/fetch calls.
// Google's edge inspects this alongside the TLS fingerprint.
var ChromeHeaderOrder = []string{
	"host",
	"sec-ch-ua",
	"sec-ch-ua-mobile",
	"sec-ch-ua-platform",
	"sec-ch-ua-arch",
	"sec-ch-ua-bitness",
	"sec-ch-ua-full-version",
	"user-agent",
	"accept",
	"accept-encoding",
	"accept-language",
	"authorization",
	"content-type",
	"origin",
	"referer",
	"cookie",
	"x-goog-api-key",
	"priority",
}

// ChromePseudoHeaderOrder is the HTTP/2 pseudo-header ordering Chrome uses.
var ChromePseudoHeaderOrder = []string{
	":method",
	":authority",
	":scheme",
	":path",
}

// platformDetails returns the UA / sec-ch-ua values matching the host OS, so the
// client looks like the browser it claims to be.
func platformDetails() (ua, platform, secChUa string) {
	switch runtime.GOOS {
	case "windows":
		return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
			`"Windows"`,
			`"Not(A:Brand";v="99", "Google Chrome";v="152", "Chromium";v="152"`
	case "linux":
		return "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
			`"Linux"`,
			`"Not(A:Brand";v="99", "Google Chrome";v="152", "Chromium";v="152"`
	default:
		return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
			`"macOS"`,
			`"Not(A:Brand";v="99", "Google Chrome";v="152", "Chromium";v="152"`
	}
}

// Platform values for the current host, exposed so callers can build matching
// headers for their own requests.
var (
	ChromeUA       = ""
	ChromePlatform = ""
	ChromeSecChUA  = ""
)

func init() {
	ChromeUA, ChromePlatform, ChromeSecChUA = platformDetails()
}

// Request describes one outbound call.
type Request struct {
	Method      string
	URL         string
	Headers     map[string]string
	HeaderOrder []string
	Body        []byte
	// Cookies is a pre-rendered Cookie header value.
	Cookies string
	// DisableQUIC forces the HTTP/2 path for this request.
	DisableQUIC bool
	// AllowQUIC permits the HTTP/3 path on a non-idempotent method. The default
	// is to keep POSTs on HTTP/2, because a request retried over a second
	// protocol risks being submitted twice. Set this only for calls that are
	// safe to repeat.
	AllowQUIC bool
}

// Response is a fully buffered upstream response.
type Response struct {
	Status int
	Header stdhttp.Header
	Body   []byte
}

// Text returns the body as a string.
func (r *Response) Text() string { return string(r.Body) }

// OK reports a 2xx status.
func (r *Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Client wraps a uTLS transport and an optional HTTP/3 transport.
type Client struct {
	hc      tls_client.HttpClient
	quic    *stdhttp.Client
	useQUIC bool
	timeout time.Duration
	// protocolRacing means the TLS library is racing HTTP/3 against HTTP/2
	// internally, so this package must not second-guess the protocol.
	protocolRacing bool
}

// Option configures the client.
type Option func(*clientOptions)

type clientOptions struct {
	timeout        time.Duration
	enableQUIC     bool
	proxyURL       string
	profileName    string
	protocolRacing bool
}

// WithTimeout sets the per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(o *clientOptions) { o.timeout = d }
}

// WithProtocolRacing lets the TLS library race HTTP/3 against HTTP/2 and keep
// whichever wins, the way Chrome does.
//
// This is not the same as this package's own QUIC path. That one hands the
// request to Go's stdlib HTTP/3 transport, which presents Go's TLS fingerprint —
// so it looks less like a browser than HTTP/2 does, and a request that is judged
// on its fingerprint will fail harder, not pass. Protocol racing uses the
// selected profile's own HTTP/3 settings and QUIC handshake, which is the point.
func WithProtocolRacing() Option {
	return func(o *clientOptions) { o.protocolRacing = true }
}

// WithProfile selects the browser TLS/HTTP2 profile by name, e.g. "chrome_152"
// or "firefox_133". Empty means the newest Chrome profile the library ships.
//
// The name must be a key of profiles.MappedTLSClients. This exists so the profile
// can be varied at runtime: a rejection that does not change when the fingerprint
// changes is not a fingerprint problem, and without this there is no way to tell.
func WithProfile(name string) Option {
	return func(o *clientOptions) { o.profileName = name }
}

// WithQUIC enables the HTTP/3 fast path (default on, matching free-gemini-api).
func WithQUIC(enabled bool) Option {
	return func(o *clientOptions) { o.enableQUIC = enabled }
}

// WithProxy routes traffic through an HTTP or SOCKS5 proxy. Keeping every call
// for one account on one exit IP materially improves Flow's bot scoring.
func WithProxy(proxyURL string) Option {
	return func(o *clientOptions) { o.proxyURL = proxyURL }
}

// New builds a client pinned to a Chrome TLS profile.
func New(opts ...Option) (*Client, error) {
	o := clientOptions{timeout: 90 * time.Second, enableQUIC: true}
	for _, opt := range opts {
		opt(&o)
	}

	profile := profiles.Chrome_152
	if o.profileName != "" {
		selected, ok := profiles.MappedTLSClients[o.profileName]
		if !ok {
			return nil, fmt.Errorf("httpx: unknown TLS profile %q", o.profileName)
		}
		profile = selected
	}

	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(int(o.timeout.Seconds())),
		tls_client.WithClientProfile(profile),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithNotFollowRedirects(),
	}
	if o.protocolRacing {
		options = append(options, tls_client.WithProtocolRacing())
	}
	if o.proxyURL != "" {
		options = append(options, tls_client.WithProxyUrl(o.proxyURL))
	}

	hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("httpx: build tls client: %w", err)
	}

	c := &Client{
		hc:             hc,
		useQUIC:        o.enableQUIC,
		timeout:        o.timeout,
		protocolRacing: o.protocolRacing,
	}

	// The stdlib QUIC transport is only built when protocol racing is off. With
	// racing on, the TLS library owns the HTTP/3 path and does it with the right
	// fingerprint; running both would race two different clients.
	if o.enableQUIC && !o.protocolRacing {
		transport := &http3.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS13,
				ClientSessionCache: tls.NewLRUClientSessionCache(32),
			},
			QUICConfig: &quic.Config{
				MaxIdleTimeout:  30 * time.Second,
				KeepAlivePeriod: 10 * time.Second,
			},
		}
		c.quic = &stdhttp.Client{Timeout: o.timeout, Transport: transport}
	}

	return c, nil
}

// Do executes the request, preferring HTTP/3 and falling back to HTTP/2 once.
func (c *Client) Do(ctx context.Context, req *Request) (*Response, error) {
	// With protocol racing the TLS library picks the protocol, HTTP/3 included.
	if c.protocolRacing {
		return c.doHTTP2(ctx, req)
	}
	if c.quic != nil && c.useQUIC && !req.DisableQUIC &&
		(req.Method == stdhttp.MethodGet || req.AllowQUIC) {
		// HTTP/3 is only worth attempting on idempotent calls; a POST retried
		// over a second protocol risks double-submitting a generation. Callers
		// that know a POST is safe to repeat opt in with AllowQUIC.
		if resp, err := c.doQUIC(ctx, req); err == nil {
			return resp, nil
		} else {
			c.useQUIC = false
		}
	}
	return c.doHTTP2(ctx, req)
}

func (c *Client) doHTTP2(ctx context.Context, req *Request) (*Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, strings.NewReader(string(req.Body)))
	if err != nil {
		return nil, err
	}

	hreq.Header[http.HeaderOrderKey] = pickOrder(req.HeaderOrder)
	hreq.Header[http.PHeaderOrderKey] = ChromePseudoHeaderOrder
	applyHeaders(hreq, req)
	if req.Cookies != "" {
		hreq.Header.Set("Cookie", req.Cookies)
	}

	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &Response{Status: resp.StatusCode, Header: stdhttp.Header(resp.Header), Body: body}, nil
}

func (c *Client) doQUIC(ctx context.Context, req *Request) (*Response, error) {
	hreq, err := stdhttp.NewRequestWithContext(ctx, req.Method, req.URL, strings.NewReader(string(req.Body)))
	if err != nil {
		return nil, err
	}
	for k, v := range defaultHeaders() {
		hreq.Header.Set(k, v)
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	if req.Cookies != "" {
		hreq.Header.Set("Cookie", req.Cookies)
	}

	resp, err := c.quic.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

func applyHeaders(hreq *http.Request, req *Request) {
	for k, v := range defaultHeaders() {
		hreq.Header.Set(k, v)
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
}

func defaultHeaders() map[string]string {
	return map[string]string{
		"User-Agent":         ChromeUA,
		"Accept":             "*/*",
		"Accept-Language":    "en-US,en;q=0.9",
		"sec-ch-ua":          ChromeSecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": ChromePlatform,
	}
}

func pickOrder(order []string) []string {
	if len(order) > 0 {
		return order
	}
	return ChromeHeaderOrder
}
