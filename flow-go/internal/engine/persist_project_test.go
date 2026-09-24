package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
)

// seedBundle writes a minimal account file and returns its path.
//
// Written as JSON rather than through the struct so the test does not have to
// restate every field name, and so the shape it exercises is the one on disk.
func seedBundle(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "account_x.json")
	seed := `{"at":"at-value","fsid":"fsid-value",` +
		`"cookies":[{"name":"SID","value":"v","domain":".google.com"}]}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("writing the seed bundle: %v", err)
	}
	return path
}

// persistProject is what stops every run listing the projects again.
//
// The file is the only place a project is remembered between processes, and the
// bridge only ever fills an empty one — so without this the resolution is thrown
// away at exit and paid for on the next run.
func TestPersistProjectRecordsIt(t *testing.T) {
	path := seedBundle(t)

	persistProject(path, "proj-1")

	got, err := cookiejar.LoadBundleFile(path)
	if err != nil {
		t.Fatalf("re-reading the bundle: %v", err)
	}
	if got.ProjectID != "proj-1" {
		t.Errorf("project_id = %q, want proj-1", got.ProjectID)
	}

	// The rest of the file is the account's credential and has to survive the
	// rewrite — losing the page tokens here would cost a priming round trip on
	// every later run, which is the opposite of the point.
	if got.At != "at-value" || got.Fsid != "fsid-value" {
		t.Errorf("the page tokens did not survive: at=%q fsid=%q", got.At, got.Fsid)
	}
	if len(got.Cookies) != 1 {
		t.Errorf("cookies = %d, want 1", len(got.Cookies))
	}
}

// A file that already names a project is left alone.
//
// That value was chosen for that account, and a project belongs to exactly one
// signed-in account — replacing it with a fresher one would be the mismatch the
// page-token work exists to avoid.
func TestPersistProjectNeverOverwrites(t *testing.T) {
	path := seedBundle(t)

	persistProject(path, "proj-1")
	persistProject(path, "proj-2")

	got, err := cookiejar.LoadBundleFile(path)
	if err != nil {
		t.Fatalf("re-reading the bundle: %v", err)
	}
	if got.ProjectID != "proj-1" {
		t.Errorf("project_id = %q, want the first value to stand", got.ProjectID)
	}
}

// Nothing to write to and nothing to write: a browser-sourced boot has no file,
// and a boot that resolved no project has nothing to record. Neither is an error.
func TestPersistProjectDoesNothingWithoutAPathOrAProject(t *testing.T) {
	persistProject("", "proj-1")

	path := filepath.Join(t.TempDir(), "account_y.json")
	persistProject(path, "")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a call with no project created a file")
	}

	// A path that is not there is reported rather than fatal: the cost of the
	// failure is one round trip next run, which is what happens today anyway.
	persistProject(filepath.Join(t.TempDir(), "missing.json"), "proj-1")
}
