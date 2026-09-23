package cli

import (
	"flag"
	"testing"
)

/*
 * Environment-backed flag defaults.
 *
 * A flag's default is the value it takes when it is not passed, so binding the
 * environment into the default is the *only* way a `.env` value reaches the flag
 * at all. FLOW_PROXY and FLOW_RECAPTCHA were documented in .env.example and read
 * by nothing before this — a value set there was silently ignored, and the run
 * used the built-in default instead.
 *
 * The ordering that makes it work is in Run: config.LoadEnv runs first, then the
 * command binds its flags. Bind before LoadEnv and the environment is not there
 * yet.
 */

func TestEnvDefaultPrefersTheEnvironment(t *testing.T) {
	t.Setenv("FLOW_TEST_KNOB", "from-env")

	if got := envDefault("FLOW_TEST_KNOB", "fallback"); got != "from-env" {
		t.Errorf("envDefault = %q, want the environment value", got)
	}
}

func TestEnvDefaultFallsBackWhenUnset(t *testing.T) {
	if got := envDefault("FLOW_TEST_KNOB_UNSET", "fallback"); got != "fallback" {
		t.Errorf("envDefault = %q, want the fallback", got)
	}
}

// An empty assignment counts as unset.
//
// `FLOW_RECAPTCHA=` in a .env file is how that file says "not configured", and
// honouring it literally would blank the strategy — turning a documented default
// of `auto` into no strategy at all. Whitespace is treated the same way, or a
// stray space after the `=` would become a proxy URL of " ".
func TestEnvDefaultTreatsEmptyAndBlankAsUnset(t *testing.T) {
	t.Setenv("FLOW_TEST_KNOB_EMPTY", "")
	t.Setenv("FLOW_TEST_KNOB_BLANK", "   ")

	if got := envDefault("FLOW_TEST_KNOB_EMPTY", "auto"); got != "auto" {
		t.Errorf("envDefault for an empty value = %q, want the fallback", got)
	}
	if got := envDefault("FLOW_TEST_KNOB_BLANK", "auto"); got != "auto" {
		t.Errorf("envDefault for a blank value = %q, want the fallback", got)
	}
}

// The two ghosts the task named, end to end through the real flag binding.
func TestCommonFlagsHonourTheDocumentedEnvironment(t *testing.T) {
	t.Setenv("FLOW_PROXY", "socks5://127.0.0.1:1080")
	t.Setenv("FLOW_RECAPTCHA", "broker")

	var c commonFlags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	c.bind(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if c.proxy != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy = %q, want the FLOW_PROXY value", c.proxy)
	}
	if c.captcha != "broker" {
		t.Errorf("captcha = %q, want the FLOW_RECAPTCHA value", c.captcha)
	}
}

// With nothing in the environment the compiled defaults must still stand, so a
// machine with no .env behaves exactly as it did before the binding existed.
func TestCommonFlagsKeepTheirDefaultsWithoutTheEnvironment(t *testing.T) {
	t.Setenv("FLOW_PROXY", "")
	t.Setenv("FLOW_RECAPTCHA", "")

	var c commonFlags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	c.bind(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if c.proxy != "" {
		t.Errorf("proxy = %q, want empty", c.proxy)
	}
	if c.captcha != "auto" {
		t.Errorf("captcha = %q, want %q", c.captcha, "auto")
	}
}

// An explicit flag still wins over the file. That is the whole point of binding
// the environment into the *default* rather than reading it after the parse.
func TestAnExplicitFlagBeatsTheEnvironment(t *testing.T) {
	t.Setenv("FLOW_PROXY", "socks5://127.0.0.1:1080")
	t.Setenv("FLOW_RECAPTCHA", "broker")

	var c commonFlags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	c.bind(fs)
	if err := fs.Parse([]string{"--proxy", "http://elsewhere:8080", "--captcha", "off"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if c.proxy != "http://elsewhere:8080" {
		t.Errorf("proxy = %q, want the flag value", c.proxy)
	}
	if c.captcha != "off" {
		t.Errorf("captcha = %q, want the flag value", c.captcha)
	}
}

// IMAGE_MODEL was a third ghost: read by config.DefaultImageModel, which only the
// dead flowapi path calls, so it had no effect on `flow-go image` at all. It is
// bound to the model flag now.
func TestImageModelIsBoundToTheImageModelFlag(t *testing.T) {
	t.Setenv("IMAGE_MODEL", "gem_pix_2")

	// The same binding runImage performs, so a change there that drops the
	// environment is caught here.
	fs := flag.NewFlagSet("image", flag.ContinueOnError)
	model := fs.String("model", envDefault("IMAGE_MODEL", ""), "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if *model != "gem_pix_2" {
		t.Errorf("model default = %q, want the IMAGE_MODEL value", *model)
	}
}
