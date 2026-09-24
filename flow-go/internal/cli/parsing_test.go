package cli

import (
	"flag"
	"reflect"
	"testing"
)

// generateSet mirrors the real generate command's flag set closely enough to
// exercise the ordering: an int, a string, and a repeatable value.
func generateSet(duration *int, aspect *string, refs *stringList) *flag.FlagSet {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.IntVar(duration, "duration", 10, "")
	fs.StringVar(aspect, "aspect", "", "")
	fs.Var(refs, "reference", "")
	return fs
}

// The help text's EXAMPLES block is written prompt-first:
//
//	flow-go generate "a paper boat on a river" --duration 8
//
// The standard flag package stops at the first non-flag argument, so that line
// used to parse no flags at all and then hand the literal words "--duration 8"
// to the prompt — the render went out at the default duration with a corrupted
// prompt, and nothing said so. This is that example, run.
func TestTheDocumentedExampleActuallyParses(t *testing.T) {
	var duration int
	var aspect string
	var refs stringList

	positional := parseInterspersed(
		generateSet(&duration, &aspect, &refs),
		[]string{"a paper boat on a river", "--duration", "8"},
	)

	if duration != 8 {
		t.Errorf("--duration after the prompt was not parsed: got %d, want 8", duration)
	}
	if want := []string{"a paper boat on a river"}; !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %q, want %q", positional, want)
	}
}

func TestFlagsBeforeThePromptStillWork(t *testing.T) {
	var duration int
	var aspect string
	var refs stringList

	positional := parseInterspersed(
		generateSet(&duration, &aspect, &refs),
		[]string{"--aspect", "portrait", "a paper boat"},
	)

	if aspect != "portrait" {
		t.Errorf("aspect = %q, want portrait", aspect)
	}
	if want := []string{"a paper boat"}; !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %q, want %q", positional, want)
	}
}

// A positional between two flags, which is the shape a caller produces when
// they add a flag to a command they already typed.
func TestFlagsOnBothSidesOfAPositional(t *testing.T) {
	var duration int
	var aspect string
	var refs stringList

	positional := parseInterspersed(
		generateSet(&duration, &aspect, &refs),
		[]string{"--duration", "6", "a paper boat", "--aspect", "9:16"},
	)

	if duration != 6 {
		t.Errorf("duration = %d, want 6", duration)
	}
	if aspect != "9:16" {
		t.Errorf("aspect = %q, want 9:16", aspect)
	}
	if want := []string{"a paper boat"}; !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %q, want %q", positional, want)
	}
}

// --reference is repeatable and must accumulate across a positional, not just
// within one run of the parser. Splitting the argument list into segments is
// exactly the kind of change that loses the earlier occurrences.
func TestARepeatableFlagAccumulatesAcrossPositionals(t *testing.T) {
	var duration int
	var aspect string
	var refs stringList

	positional := parseInterspersed(
		generateSet(&duration, &aspect, &refs),
		[]string{"--reference", "a.png", "keep the style", "--reference", "b.png"},
	)

	if want := []string{"a.png", "b.png"}; !reflect.DeepEqual([]string(refs), want) {
		t.Errorf("references = %q, want %q", refs, want)
	}
	if want := []string{"keep the style"}; !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %q, want %q", positional, want)
	}
}

// The positional arguments keep their order. `edit` reads the first as the
// source and the second as the prompt, so a reordering here would silently
// swap what is being edited with what is being asked for.
func TestPositionalsKeepTheirOrder(t *testing.T) {
	var duration int
	var aspect string
	var refs stringList

	positional := parseInterspersed(
		generateSet(&duration, &aspect, &refs),
		[]string{"clip.mp4", "--aspect", "portrait", "make it neon"},
	)

	if want := []string{"clip.mp4", "make it neon"}; !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %q, want %q", positional, want)
	}
	if aspect != "portrait" {
		t.Errorf("aspect = %q, want portrait", aspect)
	}
}

// A bare "--" ends flag parsing, which is the one escape hatch a caller has for
// passing something that looks like a flag.
func TestDoubleDashEndsFlagParsing(t *testing.T) {
	var duration int
	var aspect string
	var refs stringList

	positional := parseInterspersed(
		generateSet(&duration, &aspect, &refs),
		[]string{"--aspect", "portrait", "--", "--duration", "99"},
	)

	if aspect != "portrait" {
		t.Errorf("aspect = %q, want portrait", aspect)
	}
	if duration != 10 {
		t.Errorf("duration = %d, want the default 10: the flag after -- must not be read", duration)
	}
	if want := []string{"--duration", "99"}; !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %q, want %q", positional, want)
	}
}

func TestNoFlagsAtAll(t *testing.T) {
	var duration int
	var aspect string
	var refs stringList

	positional := parseInterspersed(
		generateSet(&duration, &aspect, &refs),
		[]string{"a", "b", "c"},
	)

	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %q, want %q", positional, want)
	}
}

func TestNothingAtAll(t *testing.T) {
	var duration int
	var aspect string
	var refs stringList

	if positional := parseInterspersed(generateSet(&duration, &aspect, &refs), nil); len(positional) != 0 {
		t.Errorf("positional = %q, want none", positional)
	}
}
