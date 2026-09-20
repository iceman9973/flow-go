package recaptcha

import (
	"encoding/base64"
	"testing"
)

// The user agent is the whole reason a browserless run works, and it fails
// silently when it is wrong: Flow answers 200 with an empty frame and charges
// nothing, so there is no error to catch and no status to check. A regression
// here would look exactly like a wrong model key, which is where the diagnostic
// used to send the reader. These tests are the cheap guard.

func TestNewHTTPStartsWithThePinnedUserAgent(t *testing.T) {
	p := NewHTTP(nil, "", "")
	if p.userAgent != recaptchaUA {
		t.Fatalf("userAgent = %q, want the pinned default", p.userAgent)
	}
}

func TestWithUserAgentReplacesThePinnedDefault(t *testing.T) {
	const real = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/153.0.0.0"

	p := NewHTTP(nil, "", "").WithUserAgent(real)
	if p.userAgent != real {
		t.Fatalf("userAgent = %q, want the caller's", p.userAgent)
	}
}

// An empty value means "no browser to read from", and must leave the pinned
// default rather than blanking the header — an empty User-Agent is its own
// fingerprint and a worse one than a stale build.
func TestWithUserAgentIgnoresAnEmptyValue(t *testing.T) {
	p := NewHTTP(nil, "", "").WithUserAgent("")
	if p.userAgent != recaptchaUA {
		t.Fatalf("userAgent = %q, want the pinned default kept", p.userAgent)
	}

	p.WithUserAgent("   ")
	if p.userAgent != recaptchaUA {
		t.Fatalf("whitespace blanked the user agent: %q", p.userAgent)
	}
}

// Build has to carry the value through to the provider it puts in the chain,
// including in auto mode where the HTTP provider is the last real one.
func TestBuildCarriesTheUserAgentIntoTheHTTPProvider(t *testing.T) {
	const real = "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/153.0.0.0"

	for _, mode := range []string{"http", "", "auto", "broker"} {
		built := Build(mode, nil, nil, nil, nil, real)
		chain, ok := built.(*Chain)
		if !ok {
			t.Fatalf("mode %q: Build did not return a chain", mode)
		}

		var found bool
		for _, p := range chain.providers {
			http, ok := p.(*HTTPProvider)
			if !ok {
				continue
			}
			found = true
			if http.userAgent != real {
				t.Errorf("mode %q: the HTTP provider has userAgent %q, want the caller's",
					mode, http.userAgent)
			}
		}
		if !found {
			t.Errorf("mode %q: no HTTP provider in the chain", mode)
		}
	}
}

// recaptchaCO is derived from recaptchaOrigin so the two cannot drift apart.
// It was written out by hand once, and drifted from labs.google to
// flow.google.com without anything noticing.
func TestOriginParameterMatchesTheOrigin(t *testing.T) {
	want := base64.RawURLEncoding.EncodeToString([]byte(recaptchaOrigin+":443")) + "."
	if recaptchaCO != want {
		t.Fatalf("recaptchaCO = %q, want %q derived from %q", recaptchaCO, want, recaptchaOrigin)
	}
}
