package cli

import (
	"flag"
	"reflect"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/config"
)

// videoFlagSet mirrors the generate command's flag set, defaults included.
//
// The defaults are the point: --duration is 10 and --quality is "720p", so a
// merge that compared values alone would treat every trailing `4s 360p` as a
// contradiction with flags nobody passed.
func videoFlagSet(duration *int, quality *string, count *int, aspect *string) *flag.FlagSet {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.IntVar(duration, "duration", config.DefaultDuration, "")
	fs.StringVar(quality, "quality", "720p", "")
	fs.IntVar(count, "count", 1, "")
	fs.StringVar(aspect, "aspect", "", "")
	return fs
}

// videoResult is the whole outcome of one command line.
type videoResult struct {
	rest     []string
	duration int
	quality  string
	count    int
	aspect   string
}

// runVideoShorthand walks a command line through the same three steps the real
// command does — parse, extract, merge — in that order, because flagWasSet reads
// what the parse recorded and would answer differently if it ran first.
func runVideoShorthand(t *testing.T, args []string) (videoResult, error) {
	t.Helper()

	var out videoResult
	var duration int
	var quality string
	var count int
	var aspect string

	fs := videoFlagSet(&duration, &quality, &count, &aspect)
	positional := parseInterspersed(fs, args)

	opts, rest, err := extractTrailingOptions("", positional, videoShorthand)
	if err != nil {
		return out, err
	}
	if err := mergeShorthand(fs, opts, &duration, &quality, &count, &aspect,
		config.VideoAspectValue); err != nil {
		return out, err
	}

	out.rest = rest
	out.duration = duration
	out.quality = quality
	out.count = count
	out.aspect = aspect
	return out, nil
}

// The headline case, and the one the feature exists for.
func TestTheWholeShorthandLineResolves(t *testing.T) {
	got, err := runVideoShorthand(t, []string{"cyberpunk car in neon rain", "9:16", "8s", "720p", "x2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want := []string{"cyberpunk car in neon rain"}; !reflect.DeepEqual(got.rest, want) {
		t.Errorf("prompt = %q, want %q", got.rest, want)
	}
	if got.aspect != "9:16" {
		t.Errorf("aspect = %q, want 9:16", got.aspect)
	}
	if got.duration != 8 {
		t.Errorf("duration = %d, want 8", got.duration)
	}
	if got.quality != "720p" {
		t.Errorf("quality = %q, want 720p", got.quality)
	}
	if got.count != 2 {
		t.Errorf("count = %d, want 2", got.count)
	}
}

// The tokens are independent, so their order is the caller's business.
func TestTokenOrderDoesNotMatter(t *testing.T) {
	orders := [][]string{
		{"a golden sunset", "4s", "360p", "x2"},
		{"a golden sunset", "360p", "x2", "4s"},
		{"a golden sunset", "x2", "4s", "360p"},
	}
	for _, args := range orders {
		got, err := runVideoShorthand(t, args)
		if err != nil {
			t.Fatalf("%v: unexpected error: %v", args, err)
		}
		if want := []string{"a golden sunset"}; !reflect.DeepEqual(got.rest, want) {
			t.Errorf("%v: prompt = %q, want %q", args, got.rest, want)
		}
		if got.duration != 4 || got.quality != "360p" || got.count != 2 {
			t.Errorf("%v: got %ds %s x%d, want 4s 360p x2",
				args, got.duration, got.quality, got.count)
		}
	}
}

// Only the tail is peeled. A non-token at the end ends the scan, so the tokens
// are not hunted for further back — which is what stops a prompt from being
// picked apart from the middle.
func TestANonTokenAtTheEndStopsTheScan(t *testing.T) {
	got, err := runVideoShorthand(t, []string{"a paper boat", "8s", "tomorrow"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want := []string{"a paper boat", "8s", "tomorrow"}; !reflect.DeepEqual(got.rest, want) {
		t.Errorf("prompt = %q, want %q", got.rest, want)
	}
	if got.duration != config.DefaultDuration {
		t.Errorf("duration = %d, want the default %d", got.duration, config.DefaultDuration)
	}
}

/* ------------------------------------------------------------------ *
 * the token classes
 * ------------------------------------------------------------------ */

func TestDurationTokens(t *testing.T) {
	cases := []struct {
		token string
		want  int
		ok    bool
	}{
		{"4s", 4, true},
		{"6s", 6, true},
		{"8s", 8, true},
		{"10s", 10, true},
		{"8S", 8, true},
		{" 8s ", 8, true},
		// Not durations: a length the catalogue has no key for is a word the
		// prompt may legitimately end in, not an unsupported request.
		{"12s", 0, false},
		{"2s", 0, false},
		{"s", 0, false},
		{"8", 0, false},
		{"8sec", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseDurationToken(tc.token)
		if ok != tc.ok {
			t.Errorf("parseDurationToken(%q) ok = %v, want %v", tc.token, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseDurationToken(%q) = %d, want %d", tc.token, got, tc.want)
		}
	}
}

func TestQualityTokens(t *testing.T) {
	cases := []struct {
		token string
		want  string
		ok    bool
	}{
		{"360p", "360p", true},
		{"720p", "720p", true},
		{"360P", "360p", true},
		{" 720p ", "720p", true},
		// 1080p is not offered by the model catalogue, so it is a word.
		{"1080p", "", false},
		{"p", "", false},
		{"720", "", false},
	}
	for _, tc := range cases {
		got, ok := parseQualityToken(tc.token)
		if ok != tc.ok {
			t.Errorf("parseQualityToken(%q) ok = %v, want %v", tc.token, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseQualityToken(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}
}

func TestCountTokens(t *testing.T) {
	cases := []struct {
		token string
		want  int
		ok    bool
	}{
		{"x1", 1, true},
		{"x2", 2, true},
		{"x4", 4, true},
		{"1x", 1, true},
		{"2x", 2, true},
		{"4x", 4, true},
		{"X2", 2, true},
		{" 2X ", 2, true},
		// Outside 1..4, or not a count at all.
		{"x5", 0, false},
		{"5x", 0, false},
		{"x0", 0, false},
		{"x", 0, false},
		{"x2x", 0, false},
		{"2", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseCountToken(tc.token)
		if ok != tc.ok {
			t.Errorf("parseCountToken(%q) ok = %v, want %v", tc.token, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseCountToken(%q) = %d, want %d", tc.token, got, tc.want)
		}
	}
}

// The quality list is a literal because the parser has to match exactly, and
// NormalizeVideoQuality cannot — it answers "720p" for anything it does not know.
// This is what stops the literal and the cost table drifting apart.
func TestShorthandQualitiesMatchTheCostTable(t *testing.T) {
	byQuality, ok := config.VideoCosts[config.DefaultDuration]
	if !ok {
		t.Fatalf("config.VideoCosts has no entry for the default duration %d", config.DefaultDuration)
	}

	known := make(map[string]bool, len(byQuality))
	for quality := range byQuality {
		known[quality] = true
	}
	for _, quality := range videoQualities {
		if !known[quality] {
			t.Errorf("videoQualities offers %q, which config.VideoCosts cannot price", quality)
		}
	}
	if len(videoQualities) != len(known) {
		t.Errorf("videoQualities has %d entries and the cost table %d; they should agree",
			len(videoQualities), len(known))
	}
}

/* ------------------------------------------------------------------ *
 * what must not be mistaken for a token
 * ------------------------------------------------------------------ */

// A prompt is normally one quoted string, so a prompt that merely ends in
// something ratio-shaped is a single argument that matches nothing. A substring
// or suffix match would silently eat the last two characters and render a
// different picture.
func TestAPromptThatEndsInARatioIsNotEaten(t *testing.T) {
	opts, rest, err := extractTrailingOptions("", []string{"a study in 4:3"}, imageShorthand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Aspect != "" {
		t.Errorf("aspect = %q, want none: the ratio is inside the prompt", opts.Aspect)
	}
	if want := []string{"a study in 4:3"}; !reflect.DeepEqual(rest, want) {
		t.Errorf("prompt = %q, want %q", rest, want)
	}
}

// The boundary, stated rather than left to be discovered: an *unquoted* prompt
// whose last word is a token is genuinely ambiguous, and this is what the rule
// does with it. Quoting is what disambiguates, which is why the help text shows
// the prompt quoted.
func TestAnUnquotedPromptEndingInATokenIsTakenAsTheToken(t *testing.T) {
	opts, rest, err := extractTrailingOptions("", []string{"a", "study", "in", "4:3"}, imageShorthand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Aspect != "4:3" {
		t.Errorf("aspect = %q, want 4:3 — this is the documented ambiguity", opts.Aspect)
	}
	if want := []string{"a", "study", "in"}; !reflect.DeepEqual(rest, want) {
		t.Errorf("prompt = %q, want %q", rest, want)
	}
}

// A token on its own is the prompt, because taking it would leave nothing to
// generate and the caller would get a complaint about a prompt they believe they
// supplied.
func TestTokensAloneStayThePrompt(t *testing.T) {
	for _, token := range []string{"9:16", "8s", "720p", "x2"} {
		opts, rest, err := extractTrailingOptions("", []string{token}, videoShorthand)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", token, err)
		}
		if opts.Aspect != "" || opts.Duration != 0 || opts.Quality != "" || opts.Count != 0 {
			t.Errorf("%s: was read as an option with nothing left to generate", token)
		}
		if want := []string{token}; !reflect.DeepEqual(rest, want) {
			t.Errorf("%s: prompt = %q, want %q", token, rest, want)
		}
	}
}

// Unless --prompt already names the prompt, in which case the lone positional is
// spare and is the option.
func TestTokensAloneAreTakenWhenPromptIsFlagged(t *testing.T) {
	opts, rest, err := extractTrailingOptions("a paper boat", []string{"9:16"}, videoShorthand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Aspect != "9:16" {
		t.Errorf("aspect = %q, want 9:16", opts.Aspect)
	}
	if len(rest) != 0 {
		t.Errorf("prompt = %q, want none", rest)
	}
}

// The image command does not take duration or quality tokens. "720p" is a
// plausible ending for an image prompt, and reading it as an option would cut
// the prompt and change the request.
func TestTheImageCommandDoesNotTakeDurationOrQualityTokens(t *testing.T) {
	opts, rest, err := extractTrailingOptions("", []string{"a timelapse", "720p", "8s"}, imageShorthand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Quality != "" || opts.Duration != 0 {
		t.Errorf("the image command took a video token: %+v", opts)
	}
	if want := []string{"a timelapse", "720p", "8s"}; !reflect.DeepEqual(rest, want) {
		t.Errorf("prompt = %q, want %q", rest, want)
	}
}

// The two commands read different aspect tables, so `3:4` is an image aspect and
// a prompt word on the video path.
func TestTheTwoCommandsUseTheirOwnAspectTables(t *testing.T) {
	if _, ok := config.VideoAspectValue("3:4"); ok {
		t.Error("3:4 is offered on the image path only")
	}

	_, rest, err := extractTrailingOptions("", []string{"a paper boat", "3:4"}, videoShorthand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"a paper boat", "3:4"}; !reflect.DeepEqual(rest, want) {
		t.Errorf("prompt = %q, want %q", rest, want)
	}
}

/* ------------------------------------------------------------------ *
 * conflicts
 * ------------------------------------------------------------------ */

func TestContradictoryTrailingTokensAreRefused(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string // substrings the message must name
	}{
		{"two durations", []string{"a boat", "8s", "4s"}, []string{"8s", "4s"}},
		{"two qualities", []string{"a boat", "360p", "720p"}, []string{"360p", "720p"}},
		{"two counts", []string{"a boat", "x2", "x3"}, []string{"x2", "x3"}},
		{"two ratios", []string{"a boat", "9:16", "16:9"}, []string{"9:16", "16:9"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runVideoShorthand(t, tc.args)
			if err == nil {
				t.Fatal("two contradictory tokens were accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

// Saying the same thing twice is not a contradiction, and refusing it would
// punish a caller for being explicit.
func TestRepeatingTheSameTokenIsFine(t *testing.T) {
	got, err := runVideoShorthand(t, []string{"a boat", "8s", "8s"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.duration != 8 {
		t.Errorf("duration = %d, want 8", got.duration)
	}
}

// A flag the caller set explicitly and a token that agrees are not a conflict.
func TestAFlagAndATokenThatAgreeAreFine(t *testing.T) {
	got, err := runVideoShorthand(t, []string{"a boat", "--duration", "8", "8s"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.duration != 8 {
		t.Errorf("duration = %d, want 8", got.duration)
	}
}

// A flag the caller set explicitly and a token that disagrees is a conflict, and
// the refusal names both sides so the caller can see which pair was objected to.
func TestAFlagAndATokenThatDisagreeAreRefused(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"duration", []string{"a boat", "--duration", "4", "8s"}, []string{"--duration 4", "8s"}},
		{"quality", []string{"a boat", "--quality", "360p", "720p"}, []string{"--quality 360p", "720p"}},
		{"count", []string{"a boat", "--count", "3", "x2"}, []string{"--count 3", "x2"}},
		{"aspect", []string{"a boat", "--aspect", "16:9", "9:16"}, []string{"--aspect 16:9", "9:16"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runVideoShorthand(t, tc.args)
			if err == nil {
				t.Fatal("a flag and a contradictory token were accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

// The flag defaults must not count as instructions. This is the case that makes
// flagWasSet necessary rather than comparing values: --duration is 10 and
// --quality is "720p" without anyone asking, so a value comparison would refuse
// every shorthand duration and every 360p.
func TestFlagDefaultsDoNotConflictWithShorthand(t *testing.T) {
	got, err := runVideoShorthand(t, []string{"a boat", "4s", "360p"})
	if err != nil {
		t.Fatalf("the defaults were treated as explicit flags: %v", err)
	}
	if got.duration != 4 {
		t.Errorf("duration = %d, want 4", got.duration)
	}
	if got.quality != "360p" {
		t.Errorf("quality = %q, want 360p", got.quality)
	}
}

// Nothing passed leaves every default alone.
func TestNoShorthandLeavesTheDefaults(t *testing.T) {
	got, err := runVideoShorthand(t, []string{"a paper boat"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.duration != config.DefaultDuration {
		t.Errorf("duration = %d, want %d", got.duration, config.DefaultDuration)
	}
	if got.quality != "720p" {
		t.Errorf("quality = %q, want 720p", got.quality)
	}
	if got.count != 1 {
		t.Errorf("count = %d, want 1", got.count)
	}
	if got.aspect != "" {
		t.Errorf("aspect = %q, want empty so the payload keeps its default", got.aspect)
	}
	if want := []string{"a paper boat"}; !reflect.DeepEqual(got.rest, want) {
		t.Errorf("prompt = %q, want %q", got.rest, want)
	}
}

// A nil target is a flag the command does not have. The image command passes nil
// for duration and quality, and its token set never fills them — but if that
// ever changed, this must not panic on the way to finding out.
func TestMergeSkipsTargetsTheCommandDoesNotHave(t *testing.T) {
	var count int
	var aspect string
	fs := flag.NewFlagSet("image", flag.ContinueOnError)
	fs.IntVar(&count, "count", 1, "")
	fs.StringVar(&aspect, "aspect", "", "")

	err := mergeShorthand(fs, trailingOptions{Duration: 8, Quality: "720p", Count: 2, Aspect: "1:1"},
		nil, nil, &count, &aspect, config.ImageAspectValue)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 || aspect != "1:1" {
		t.Errorf("count = %d, aspect = %q; want 2 and 1:1", count, aspect)
	}
}

/* ------------------------------------------------------------------ *
 * aspects
 * ------------------------------------------------------------------ */

func TestAspectsAgreeComparesByRatioNotBySpelling(t *testing.T) {
	if !aspectsAgree("9:16", "portrait", config.VideoAspectValue) {
		t.Error("9:16 and portrait are the same ratio and should agree")
	}
	if !aspectsAgree("16:9", "landscape", config.VideoAspectValue) {
		t.Error("16:9 and landscape are the same ratio and should agree")
	}
	if aspectsAgree("9:16", "16:9", config.VideoAspectValue) {
		t.Error("9:16 and 16:9 are different ratios and should not agree")
	}
	if aspectsAgree("nonsense", "9:16", config.VideoAspectValue) {
		t.Error("an unknown name should never agree with a known one")
	}
}

// --aspect 9:16 beside a trailing portrait is one request written twice, and must
// not be refused as a contradiction.
func TestAnAspectFlagAndItsAliasAgree(t *testing.T) {
	got, err := runVideoShorthand(t, []string{"a boat", "--aspect", "9:16", "portrait"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.aspect != "9:16" {
		t.Errorf("aspect = %q, want the flag's spelling 9:16", got.aspect)
	}
}

// flagWasSet is what tells a named flag from a defaulted one.
func TestFlagWasSet(t *testing.T) {
	var duration int
	var aspect string
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.IntVar(&duration, "duration", config.DefaultDuration, "")
	fs.StringVar(&aspect, "aspect", "", "")

	_ = parseInterspersed(fs, []string{"--duration", "8", "a boat"})

	if !flagWasSet(fs, "duration") {
		t.Error("--duration was passed and flagWasSet says otherwise")
	}
	if flagWasSet(fs, "aspect") {
		t.Error("--aspect was not passed and flagWasSet says it was")
	}
}
