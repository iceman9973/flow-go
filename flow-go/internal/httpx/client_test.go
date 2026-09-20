package httpx

import "testing"

// TestWithProfileSelectsAProfile covers the selector added to establish that the
// TLS fingerprint is not what rejects an upscaled-image request: six profiles,
// including a Firefox one, all produce the identical rejection, so the profile
// cannot be the discriminator. The option has to actually change the profile for
// that conclusion to mean anything.
func TestWithProfileSelectsAProfile(t *testing.T) {
	for _, name := range []string{"chrome_152", "chrome_133", "firefox_135"} {
		client, err := New(WithTimeout(5), WithProfile(name))
		if err != nil {
			t.Errorf("New(WithProfile(%q)): %v", name, err)
			continue
		}
		if client == nil || client.hc == nil {
			t.Errorf("New(WithProfile(%q)) returned no client", name)
		}
	}
}

// TestWithProfileRejectsUnknownName guards against a typo silently falling back
// to the default, which would make a fingerprint comparison meaningless.
func TestWithProfileRejectsUnknownName(t *testing.T) {
	if _, err := New(WithProfile("chrome_999")); err == nil {
		t.Error("an unknown profile name should be an error, not a silent default")
	}
}

// TestProtocolRacingIsOffByDefault keeps the default transport unchanged: racing
// is a diagnostic and an opt-in, and enabling it for every request would move
// every generation onto a different protocol path.
func TestProtocolRacingIsOffByDefault(t *testing.T) {
	client, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if client.protocolRacing {
		t.Error("protocol racing must not be on by default")
	}
}

// TestProtocolRacingSuppressesTheStdlibQUICPath is the property that matters: with
// racing on, the TLS library owns HTTP/3 and does it with the profile's own QUIC
// settings. This package's stdlib transport presents Go's fingerprint, so running
// both would race two different clients and the wrong one could win.
func TestProtocolRacingSuppressesTheStdlibQUICPath(t *testing.T) {
	racing, err := New(WithProtocolRacing())
	if err != nil {
		t.Fatal(err)
	}
	if !racing.protocolRacing {
		t.Fatal("racing was requested but not recorded")
	}
	if racing.quic != nil {
		t.Error("the stdlib QUIC transport must not be built when racing is on")
	}

	plain, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if plain.quic == nil {
		t.Error("without racing the stdlib QUIC transport should still exist")
	}
}
