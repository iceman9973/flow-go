package cookiejar

import "testing"

// The key names an account's file in the cookie directory, so two properties
// carry all the weight: the same signed-in account must always land on the same
// file, and two accounts must never share one.

func keyJar(cookies ...Cookie) *Jar {
	return FromCookies(cookies, "test")
}

func keyCookie(name, value string) Cookie {
	return Cookie{Domain: ".google.com", Path: "/", Name: name, Value: value}
}

func TestTheSameAccountAlwaysGetsTheSameKey(t *testing.T) {
	first := AccountKey(keyJar(keyCookie("SAPISID", "sapisid-value"), keyCookie("SID", "sid-value")))
	second := AccountKey(keyJar(keyCookie("SAPISID", "sapisid-value"), keyCookie("SID", "sid-value")))

	if first == "" {
		t.Fatal("a jar holding identity cookies must have a key")
	}
	if first != second {
		t.Errorf("the same account got two keys: %q and %q", first, second)
	}
}

func TestTwoAccountsGetDifferentKeys(t *testing.T) {
	first := AccountKey(keyJar(keyCookie("SAPISID", "first-account")))
	second := AccountKey(keyJar(keyCookie("SAPISID", "second-account")))

	if first == second {
		t.Errorf("two accounts share the key %q", first)
	}
}

// TestRotatingCookiesDoNotChangeTheKey is the one that matters. Derived from
// `__Secure-1PSIDTS` or its siblings the key would move every few hours, and
// every rotation would allocate a fresh account file instead of updating the one
// the account already has — which is exactly the property the file name exists
// to provide.
func TestRotatingCookiesDoNotChangeTheKey(t *testing.T) {
	stable := []Cookie{keyCookie("SAPISID", "sapisid-value"), keyCookie("SID", "sid-value")}
	before := AccountKey(keyJar(stable...))

	rotated := append(append([]Cookie{}, stable...),
		keyCookie("__Secure-1PSIDTS", "rotated-one"),
		keyCookie("__Secure-3PSIDTS", "rotated-two"),
		keyCookie("SIDCC", "rotated-three"),
	)
	after := AccountKey(keyJar(rotated...))

	if before != after {
		t.Errorf("the key moved when only rotating cookies changed: %q -> %q", before, after)
	}
}

func TestAJarWithNoIdentityCookiesHasNoKey(t *testing.T) {
	// A rotating cookie is a real cookie and not an identity one: on its own it
	// must not be enough to attribute a jar to an account.
	jar := keyJar(keyCookie("__Secure-1PSIDTS", "rotating"), keyCookie("NID", "analytics"))

	if key := AccountKey(jar); key != "" {
		t.Errorf("key = %q, want empty for a jar with no identity cookies", key)
	}
}

func TestANilJarHasNoKey(t *testing.T) {
	if key := AccountKey(nil); key != "" {
		t.Errorf("key = %q, want empty", key)
	}
}

func TestTheKeyDoesNotDependOnCookieOrder(t *testing.T) {
	first := AccountKey(keyJar(
		keyCookie("SAPISID", "sapisid-value"),
		keyCookie("SID", "sid-value"),
		keyCookie("EMAIL", "person@example.test"),
	))
	second := AccountKey(keyJar(
		keyCookie("EMAIL", "person@example.test"),
		keyCookie("SID", "sid-value"),
		keyCookie("SAPISID", "sapisid-value"),
	))

	if first != second {
		t.Errorf("the key depends on the order the cookies arrived in: %q vs %q", first, second)
	}
}

func TestTheKeyIsShortEnoughToBeAFilename(t *testing.T) {
	key := AccountKey(keyJar(keyCookie("SAPISID", "sapisid-value")))
	if len(key) != 12 {
		t.Errorf("key %q is %d chars, want 12", key, len(key))
	}
}
