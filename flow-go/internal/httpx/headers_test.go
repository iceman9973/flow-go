package httpx

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// lower maps a header set to lowercase keys, because defaultHeaders is written
// the way Chrome spells the headers and a lookup has to agree with that.
func lower(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		out[strings.ToLower(name)] = value
	}
	return out
}

// TestEveryDeclaredIdentityHeaderIsActuallySent is the property ChromeHeaderOrder
// depends on.
//
// An order list is only meaningful for headers that are present, and a hint that
// is declared in the order but absent from the request is a
// declared-versus-present mismatch that a fingerprinting edge can read directly.
// This checks both directions, so it fails whether a header is dropped from the
// set or added without being declared.
func TestEveryDeclaredIdentityHeaderIsActuallySent(t *testing.T) {
	headers := lower(defaultHeaders())

	declared := make(map[string]bool, len(ChromeHeaderOrder))
	for _, name := range ChromeHeaderOrder {
		declared[strings.ToLower(name)] = true
	}

	// The identity headers: the ones describing the browser rather than the
	// call. The rest of ChromeHeaderOrder is per-request by nature — host,
	// cookie, origin, referer, content-type, authorization and x-goog-api-key are
	// set by whoever makes the call.
	identity := []string{
		"user-agent",
		"accept",
		"accept-language",
		"sec-ch-ua",
		"sec-ch-ua-mobile",
		"sec-ch-ua-platform",
		"sec-ch-ua-arch",
		"sec-ch-ua-bitness",
		"sec-ch-ua-full-version",
		"priority",
	}

	for _, name := range identity {
		if !declared[name] {
			t.Errorf("%s is sent but is not declared in ChromeHeaderOrder", name)
		}
		if _, ok := headers[name]; !ok {
			t.Errorf("%s is declared in ChromeHeaderOrder but defaultHeaders omits it", name)
		}
	}
}

// A client claiming one build in its User-Agent and another in
// sec-ch-ua-full-version is more distinctive than one that omits the hint — the
// mismatch is the tell. Both are derived from one constant, and this pins that
// they still agree.
func TestTheVersionAgreesEverywhereItAppears(t *testing.T) {
	if !strings.Contains(ChromeUA, "Chrome/"+chromeFullVersion) {
		t.Errorf("the User-Agent does not carry the full version %q:\n  %s",
			chromeFullVersion, ChromeUA)
	}
	if !strings.Contains(ChromeSecChUA, `v="`+chromeMajorVersion+`"`) {
		t.Errorf("sec-ch-ua does not carry the major version %q:\n  %s",
			chromeMajorVersion, ChromeSecChUA)
	}
	if want := strconv.Quote(chromeFullVersion); lower(defaultHeaders())["sec-ch-ua-full-version"] != want {
		t.Errorf("sec-ch-ua-full-version = %s, want %s", want, want)
	}
}

// Client hints are quoted strings. An unquoted value is not a value Chrome ever
// sends, so it would be a tell rather than a fix.
func TestClientHintsAreQuoted(t *testing.T) {
	headers := lower(defaultHeaders())
	for _, name := range []string{"sec-ch-ua-arch", "sec-ch-ua-bitness", "sec-ch-ua-full-version"} {
		value, ok := headers[name]
		if !ok {
			t.Errorf("%s is missing", name)
			continue
		}
		if !strings.HasPrefix(value, `"`) || !strings.HasSuffix(value, `"`) {
			t.Errorf("%s = %s, which is not a quoted string", name, value)
		}
	}
}

// The arch hint describes the family, not the chip: Chrome answers "x86" for
// every 64-bit x86 machine and "arm" for every 64-bit ARM one, so Go's GOARCH is
// narrower than what the hint carries.
func TestArchIsTheFamilyChromeReports(t *testing.T) {
	want := "x86"
	if runtime.GOARCH == "arm64" || runtime.GOARCH == "arm" {
		want = "arm"
	}
	if ChromeArch != want {
		t.Errorf("ChromeArch = %q on %s, want %q", ChromeArch, runtime.GOARCH, want)
	}
	if got := lower(defaultHeaders())["sec-ch-ua-arch"]; got != strconv.Quote(want) {
		t.Errorf("sec-ch-ua-arch = %s, want %s", got, strconv.Quote(want))
	}
}

func TestBitnessMatchesThePlatform(t *testing.T) {
	if ChromeBitness != strconv.Itoa(strconv.IntSize) {
		t.Errorf("ChromeBitness = %q, want %d", ChromeBitness, strconv.IntSize)
	}
}

// priority is a fetch-metadata hint, and Chrome's value for an ordinary XHR is
// fixed. Pinning the exact string keeps a plausible-looking edit from changing it
// into something Chrome never sends.
func TestPriorityIsTheChromeValue(t *testing.T) {
	if got := lower(defaultHeaders())["priority"]; got != "u=1, i" {
		t.Errorf("priority = %q, want %q", got, "u=1, i")
	}
}

// Accept-Encoding is deliberately absent, and this test is here so that removing
// it from the set is a decision rather than an accident: Go's transport
// negotiates it and transparently decodes the response, and setting it by hand
// turns that decoding off. A caller handed compressed bytes has a worse bug than
// a missing header.
func TestAcceptEncodingIsLeftToTheTransport(t *testing.T) {
	if value, ok := lower(defaultHeaders())["accept-encoding"]; ok {
		t.Errorf("Accept-Encoding is set to %q; Go's transport must negotiate and "+
			"decode it, or callers receive compressed bodies", value)
	}
}
