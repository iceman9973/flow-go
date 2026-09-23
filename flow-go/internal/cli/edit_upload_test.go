package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/*
 * Video upload, and the positional source.
 *
 * Only the argument handling is reachable here: past it the command needs a
 * session, and the upload's own protocol is pinned in the engine package where
 * the two steps can be driven against a test server. What matters at this layer
 * is which argument becomes what — the part a user actually types.
 */

// A positional source is taken from the first argument only when that argument is
// a file that exists. That is what keeps a one-word prompt unambiguous, and it is
// the whole reason `flow-go edit clip.mp4 "make it neon"` can work.
func TestEditTakesAPositionalSourceWhenItIsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, []byte("pretend this is a video"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var code int
	_, stderr := captureStreams(t, func() {
		code = Run([]string{"edit", path})
	})

	// It got past the source check — the file was taken as the source — and
	// stopped at the prompt, which is as far as this can go without a session.
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--prompt") {
		t.Errorf("the positional file should have been taken as the source, leaving the "+
			"prompt missing: %q", stderr)
	}
	if strings.Contains(stderr, "--source") {
		t.Errorf("the file was not taken as the source: %q", stderr)
	}
}

// A path that is not on disk is reported as a missing file rather than passed
// through as an id.
//
// It used to be read as the prompt, which meant the run reported a missing
// *source* and sent the reader looking at their media ids. Two arguments with
// nothing named are source-then-prompt, so the first is a source — and once it
// is, a separator in it is decisive: an id has none, so this names the file.
func TestEditReportsAMissingPathAsAMissingFile(t *testing.T) {
	var code int
	_, stderr := captureStreams(t, func() {
		code = Run([]string{"edit", "./not-a-real-file.mp4", "make it neon"})
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "is not a file") {
		t.Errorf("want the missing file named: %q", stderr)
	}
	if !strings.Contains(stderr, "not-a-real-file.mp4") {
		t.Errorf("want the offending path quoted: %q", stderr)
	}
}

// A directory is not a source: the upload would refuse it, and saying so here
// keeps the failure about the argument rather than about the upload.
func TestEditDoesNotAcceptADirectoryAsASource(t *testing.T) {
	dir := t.TempDir()

	var code int
	_, stderr := captureStreams(t, func() {
		code = Run([]string{"edit", dir, "make it neon"})
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "is not a file") {
		t.Errorf("a directory should not be taken as a source: %q", stderr)
	}
}

// The rule itself, exercised directly rather than through the command.
//
// Going through `Run` reaches the bridge, which waits five seconds to become its
// host before failing — five seconds per case, for a decision that touches no
// network. This covers every combination, including the ones the command cannot
// reach without a session.
func TestResolveEditArgsDecidesTheSourceAndThePrompt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(file, []byte("pretend this is a video"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	const id = "1370d7c6-6e2f-4ebb-b2fa-b40da7204012"

	cases := []struct {
		name       string
		source     string
		prompt     string
		positional []string
		wantSource string
		wantPrompt string
	}{
		{
			name:       "a file first is the source, the rest is the prompt",
			positional: []string{file, "make", "it", "neon"},
			wantSource: file,
			wantPrompt: "make it neon",
		},
		{
			name:       "a file alone leaves the prompt missing",
			positional: []string{file},
			wantSource: file,
			wantPrompt: "",
		},
		{
			name:       "an id first is the source when there are two arguments",
			positional: []string{id, "make it neon"},
			wantSource: id,
			wantPrompt: "make it neon",
		},
		{
			name:       "one argument that is not a file is the prompt",
			positional: []string{"make it neon"},
			wantSource: "",
			wantPrompt: "make it neon",
		},
		{
			// The cost of letting the flag decide: with --source named, every
			// positional is a prompt word — including one that happens to be a
			// path. Stated here so it is a known behaviour.
			name:       "--source takes precedence over both signals",
			source:     "an-id",
			positional: []string{file, "make it neon"},
			wantSource: "an-id",
			wantPrompt: file + " make it neon",
		},
		{
			name:       "--source leaves a multi-word prompt intact",
			source:     "an-id",
			positional: []string{"make it", "neon"},
			wantSource: "an-id",
			wantPrompt: "make it neon",
		},
		{
			name:       "--prompt wins over the positional words",
			prompt:     "from the flag",
			positional: []string{file, "ignored"},
			wantSource: file,
			wantPrompt: "from the flag",
		},
		{
			name:       "no arguments at all",
			wantSource: "",
			wantPrompt: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSource, gotPrompt := resolveEditArgs(tc.source, tc.prompt, tc.positional)
			if gotSource != tc.wantSource {
				t.Errorf("source = %q, want %q", gotSource, tc.wantSource)
			}
			if gotPrompt != tc.wantPrompt {
				t.Errorf("prompt = %q, want %q", gotPrompt, tc.wantPrompt)
			}
		})
	}
}

// A URL is not something the edit RPC can address — it takes an asset id, and
// resolving one from a URL would mean downloading it. Said plainly rather than
// passed through to fail as "not in the project listing", which names the
// symptom and points at the project rather than at the argument.
func TestEditRefusesAURLSource(t *testing.T) {
	var code int
	_, stderr := captureStreams(t, func() {
		code = Run([]string{"edit", "https://example.com/clip.mp4", "make it neon"})
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "is a URL") {
		t.Errorf("want the URL called out: %q", stderr)
	}
	if !strings.Contains(stderr, "download it first") {
		t.Errorf("want the next step named: %q", stderr)
	}
}

// A single argument that is not a file is the prompt, and the source is then
// genuinely missing — the case the rule above must not swallow.
func TestEditWithOneArgumentAndNoSource(t *testing.T) {
	var code int
	_, stderr := captureStreams(t, func() {
		code = Run([]string{"edit", "make it neon"})
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--source is required") {
		t.Errorf("want the source reported missing: %q", stderr)
	}
}

// `upload` is the short spelling of `upload-video`.
func TestUploadIsAnAliasForUploadVideo(t *testing.T) {
	var code int
	_, stderr := captureStreams(t, func() {
		code = Run([]string{"upload"})
	})

	if code == 2 {
		t.Fatalf("upload was treated as an unknown command: %s", stderr)
	}
	if !strings.Contains(stderr, "video file is required") {
		t.Errorf("the alias should reach the same command: %q", stderr)
	}
}

func TestUploadVideoIsARecognisedCommand(t *testing.T) {
	var code int
	_, stderr := captureStreams(t, func() {
		code = Run([]string{"upload-video"})
	})

	if code == 2 {
		t.Fatalf("upload-video was treated as an unknown command: %s", stderr)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "video file is required") {
		t.Errorf("want the missing file named: %q", stderr)
	}
}

func TestUploadVideoTakesExactlyOneFile(t *testing.T) {
	var code int
	_, stderr := captureStreams(t, func() {
		code = Run([]string{"upload-video", "a.mp4", "b.mp4"})
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "takes one file") {
		t.Errorf("want the extra argument reported: %q", stderr)
	}
}

func TestUsageDocumentsVideoUpload(t *testing.T) {
	text := usage()

	for _, want := range []string{
		"upload-video",
		"a media ID, a content ID, or a path",
		"resumable",
		"upload-video clip.mp4",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the help text does not mention %q", want)
		}
	}

	// The old wording said the source had to be an id, which is no longer true.
	if strings.Contains(text, "The asset to edit, as a media ID or a content ID") {
		t.Error("the help text still says the source must be an id")
	}
}
