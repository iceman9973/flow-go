package cli

import (
	"io"
	"os"
	"strings"
	"testing"
)

// contains reports whether list holds want. Local to the package rather than
// shared with the server tests: they are separate packages, and a two-line
// helper is not worth a common test package.
func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// captureStreams runs fn with both standard streams redirected, and returns what
// each of them received.
//
// Both, because the property under test is a *split*: a warning that lands on
// stdout is not a warning, it is a corrupt result. The JSON these commands print
// is piped into jq, and one stray line makes that fail with a parse error that
// says nothing about the flag.
func captureStreams(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating the stdout pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating the stderr pipe: %v", err)
	}

	originalOut, originalErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	fn()

	os.Stdout, os.Stderr = originalOut, originalErr
	_ = outW.Close()
	_ = errW.Close()

	outBytes, _ := io.ReadAll(outR)
	errBytes, _ := io.ReadAll(errR)
	return string(outBytes), string(errBytes)
}

func TestIgnoredGenerateFlagsNamesWhatWasPassed(t *testing.T) {
	got := ignoredGenerateFlags("9:16", "4k", 7)

	for _, want := range []string{"--aspect", "--resolution", "--seed"} {
		if !contains(got, want) {
			t.Errorf("ignoredGenerateFlags missed %q: %v", want, got)
		}
	}
}

// --reference left this list when the CLI started routing it to the
// reference-to-video submission. It used to be reported as ignored even though
// the engine had a working path for it all along, and reporting it as ignored
// now would be a lie about what the run actually did.
func TestReferenceIsNoLongerReportedAsIgnored(t *testing.T) {
	if got := ignoredGenerateFlags("", "", 0); contains(got, "--reference") {
		t.Errorf("--reference is reported as ignored, but it is applied now: %v", got)
	}
}

func TestIgnoredGenerateFlagsIsEmptyWhenNothingWasPassed(t *testing.T) {
	// The common case. A warning printed for a run that passed no unsupported
	// flag is noise, and noise on stderr is what teaches a caller to ignore it.
	if got := ignoredGenerateFlags("", "", 0); len(got) != 0 {
		t.Errorf("nothing was passed but %v was reported", got)
	}
}

func TestIgnoredGenerateFlagsTreatsAZeroSeedAsUnset(t *testing.T) {
	// --seed defaults to 0, and 0 is a legal seed, so this is a boundary rather
	// than a bug: the flag cannot distinguish "not passed" from "passed 0", and
	// warning on every defaulted run would be worse than missing one explicit
	// zero.
	if got := ignoredGenerateFlags("", "", 0); len(got) != 0 {
		t.Errorf("a defaulted seed was reported: %v", got)
	}
	if got := ignoredGenerateFlags("", "", 1); !contains(got, "--seed") {
		t.Errorf("an explicit non-zero seed was not reported: %v", got)
	}
}

func TestWarnIgnoredFlagsWritesToStderrNotStdout(t *testing.T) {
	stdout, stderr := captureStreams(t, func() {
		warnIgnoredFlags([]string{"--aspect", "--seed"})
	})

	if stdout != "" {
		t.Errorf("the warning polluted stdout, which callers pipe into jq: %q", stdout)
	}
	for _, want := range []string{"--aspect", "--seed", "ignoring"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr is missing %q: %q", want, stderr)
		}
	}
}

func TestWarnIgnoredFlagsIsSilentWithNothingToReport(t *testing.T) {
	stdout, stderr := captureStreams(t, func() {
		warnIgnoredFlags(nil)
	})

	if stdout != "" || stderr != "" {
		t.Errorf("warnIgnoredFlags printed with nothing to report: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestUsageDocumentsTheWarnRatherThanFailBehaviour(t *testing.T) {
	// The help text is where a caller learns what a flag will do to their run.
	// It said "an error naming it" until the behaviour changed, and a help text
	// that describes the previous behaviour is worse than a vague one.
	text := usage()

	if strings.Contains(text, "Asking for one is an error") {
		t.Error("the help text still describes the removed hard failure")
	}
	for _, want := range []string{"--aspect", "doctor"} {
		if !strings.Contains(text, want) {
			t.Errorf("the help text does not mention %q", want)
		}
	}
}
