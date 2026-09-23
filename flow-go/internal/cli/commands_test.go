package cli

import (
	"strings"
	"testing"
)

/*
 * The two CLI additions that are reachable without a session.
 *
 * `edit` and `--reference` both exposed engine paths that had no caller at all,
 * so what is worth pinning here is that the commands exist, that they fail for
 * the right reason rather than as unknown commands, and that the help text
 * describes what they do. Anything beyond that needs a live account.
 */

// TestPromptFromArgsPrefersTheFlag: an explicit flag is a decision, and the
// trailing words must not quietly override it.
func TestPromptFromArgsPrefersTheFlag(t *testing.T) {
	got := promptFromArgs("from the flag", []string{"from", "the", "args"})
	if got != "from the flag" {
		t.Errorf("promptFromArgs = %q, want the flag value", got)
	}
}

// TestPromptFromArgsJoinsThePositionalWords: the shell has already split the
// prompt into words, so they are rejoined into the one string the server wants.
func TestPromptFromArgsJoinsThePositionalWords(t *testing.T) {
	got := promptFromArgs("", []string{"a", "paper", "boat"})
	if got != "a paper boat" {
		t.Errorf("promptFromArgs = %q, want %q", got, "a paper boat")
	}
}

func TestPromptFromArgsIsEmptyWithNothingToUse(t *testing.T) {
	if got := promptFromArgs("", nil); got != "" {
		t.Errorf("promptFromArgs with no flag and no args = %q, want empty", got)
	}
}

// TestEditIsARecognisedCommand is the point of S8: before this, `flow-go edit`
// was an unknown command and the engine path behind it had no caller at all.
//
// It is invoked with no --source, so it fails — but it must fail as a usage
// error from the command itself, not as an unknown command from the dispatcher,
// which is exit code 2.
func TestEditIsARecognisedCommand(t *testing.T) {
	var code int
	stdout, stderr := captureStreams(t, func() {
		code = Run([]string{"edit"})
	})

	if code == 2 {
		t.Fatalf("edit was treated as an unknown command: %s", stderr)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1 — a missing required flag is a usage failure", code)
	}
	if !strings.Contains(stderr, "--source") {
		t.Errorf("the failure should name the missing flag: %q", stderr)
	}
	if stdout != "" {
		t.Errorf("a failure must not write to stdout, which callers pipe into jq: %q", stdout)
	}
}

// An edit whose source is given but whose prompt is not must name the prompt.
func TestEditWithoutAPromptNamesThePrompt(t *testing.T) {
	var code int
	_, stderr := captureStreams(t, func() {
		code = Run([]string{"edit", "--source", "media-1"})
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--prompt") {
		t.Errorf("the failure should name the missing prompt: %q", stderr)
	}
}

// The help text is where a caller learns a command exists. It named neither of
// these before S8, so neither was discoverable.
func TestUsageDocumentsTheNewSurface(t *testing.T) {
	text := usage()

	for _, want := range []string{"edit", "--source", "--reference", "EDIT FLAGS"} {
		if !strings.Contains(text, want) {
			t.Errorf("the help text does not mention %q", want)
		}
	}
	// --reference is applied now, so it must not still be listed among the flags
	// that are ignored.
	if strings.Contains(text, "reference-to-video has no batchexecute transport") {
		t.Error("the help text still says reference-to-video is unsupported")
	}
}
