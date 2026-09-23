package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
)

// `--cookies` runs one command as one named account. These pin the two things
// that makes true: the named file is the one that gets loaded, and nothing else
// gets a chance to write over it on the way past.

func namedCookieFile(t *testing.T, cookies ...cookiejar.Cookie) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account_test.json")
	if err := cookiejar.FromCookies(cookies, "test").Save(path); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	return path
}

func sessionCookie(name, value string) cookiejar.Cookie {
	return cookiejar.Cookie{Domain: ".google.com", Path: "/", Name: name, Value: value}
}

func TestLoadJarUsesTheNamedCookieFile(t *testing.T) {
	path := namedCookieFile(t, sessionCookie("SAPISID", "named-account"))

	// No bridge at all: the named file has to be enough on its own, which is what
	// makes the flag usable in a process that is not the bridge host.
	e := &Engine{opts: Options{CookieFile: path}}

	jar, source, err := e.loadJar(context.Background())
	if err != nil {
		t.Fatalf("loadJar: %v", err)
	}
	if jar == nil || jar.Count() == 0 {
		t.Fatal("the named file loaded no cookies")
	}
	if source != path {
		t.Errorf("source = %q, want the named path %q", source, path)
	}
}

func TestLoadJarRejectsAMissingNamedFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there.json")

	e := &Engine{opts: Options{CookieFile: missing}}

	_, _, err := e.loadJar(context.Background())
	if err == nil {
		t.Fatal("a missing --cookies file must be an error")
	}
	// Named, so the reader is not left wondering which path was meant.
	if !strings.Contains(err.Error(), "not-there.json") {
		t.Errorf("error %q should name the file", err)
	}
}

// TestLoadJarRejectsANamedFileWithNoCookies pins the property that matters —
// an unusable --cookies file is an error, not a silent empty session — without
// insisting on which layer reports it. `LoadFile` rejects an empty jar's shape
// before `loadJar` can see it, so the count guard inside loadJar is defensive
// rather than the thing this test exercises.
func TestLoadJarRejectsANamedFileWithNoCookies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	if err := cookiejar.FromCookies(nil, "test").Save(path); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}

	e := &Engine{opts: Options{CookieFile: path}}

	_, _, err := e.loadJar(context.Background())
	if err == nil {
		t.Fatal("an unusable --cookies file must be an error, not a silent empty session")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should name the file", err)
	}
}
