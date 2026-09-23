package cookiejar

import (
	"encoding/json"
	"os"
)

// Fingerprint is the browser identity a bundle carries.
//
// Its own type rather than flowapi.BrowserFingerprint for two reasons: this
// package sits below that one — flowapi imports cookiejar, not the reverse — and
// the file's spelling is snake_case while that struct carries no json tags and
// so serialises as Go field names.
type Fingerprint struct {
	UserAgent string `json:"user_agent,omitempty"`
	SecChUa   string `json:"sec_ch_ua,omitempty"`
	Platform  string `json:"platform,omitempty"`
	Language  string `json:"language,omitempty"`
	Mobile    string `json:"mobile,omitempty"`
}

// Empty reports whether there is nothing here worth adopting.
func (f *Fingerprint) Empty() bool {
	return f == nil || f.UserAgent == ""
}

// Bundle is one account, self-contained: its cookies plus everything the engine
// would otherwise have to rediscover.
//
// The point is that a file is enough. The project was re-resolved on every boot,
// and the page tokens and the browser identity were rows in the database — so a
// cookie file could not be handed to another process and simply work. Here it
// can: load the bundle and you have the credential, where it generates, the
// tokens that let a browserless call open the way the page would, and the
// identity those tokens were minted for.
type Bundle struct {
	AccountID   string       `json:"account_id,omitempty"`
	ProjectID   string       `json:"project_id,omitempty"`
	At          string       `json:"at,omitempty"`
	Fsid        string       `json:"fsid,omitempty"`
	Fingerprint *Fingerprint `json:"fingerprint,omitempty"`
	Cookies     []Cookie     `json:"cookies"`
}

// LoadBundleFile reads a bundle, accepting a legacy cookie array as well.
//
// A bare array comes back as a bundle holding only its cookies, which is exactly
// what it is — an account with nothing recorded about it beyond the credential.
// Callers do not have to know which shape is on disk, and the ones that only
// want cookies never did.
func LoadBundleFile(path string) (*Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var bundle Bundle
	if err := json.Unmarshal(data, &bundle); err == nil && len(bundle.Cookies) > 0 {
		return &bundle, nil
	}

	jar, err := LoadFile(path)
	if err != nil {
		return nil, err
	}
	return &Bundle{Cookies: jar.Cookies()}, nil
}

// Save writes the bundle, creating parent directories.
func (b *Bundle) Save(path string) error {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Jar is the bundle's cookies as a jar.
func (b *Bundle) Jar() *Jar {
	if b == nil {
		return nil
	}
	return FromCookies(b.Cookies, "bundle")
}

// Complete reports whether this bundle is enough to run an account with.
//
// Everything it asks for is something a run cannot do without, and every one of
// them fails *silently* when it is missing — which is the whole reason the check
// is here rather than left to whatever notices first:
//
//   - cookies with a credential among them, or every call goes out unauthenticated;
//   - the page's `at` and `f.sid`, without which batchexecute has to prime for a
//     token — and that handshake never fires when the server answers 401 instead
//     of 400, so the token is never learned at all;
//   - the browser identity, because a captcha token is only valid for the client
//     it was minted under, and the mismatch comes back as an empty result with no
//     error anywhere to point at it.
//
// A bundle written by a browser sync carries all three only if a Flow tab was
// open at the time — `mergeBundle` preserves what an earlier sync learned, so a
// profile that has had one keeps them. That is exactly the distinction this
// draws: a profile that never has cannot generate, and registering it would put
// a worker in the pool whose every routed job fails.
func (b *Bundle) Complete() bool {
	if b == nil {
		return false
	}
	if b.At == "" || b.Fsid == "" {
		return false
	}
	if b.Fingerprint.Empty() {
		return false
	}
	jar := b.Jar()
	return jar != nil && jar.HasAuthCookies()
}
