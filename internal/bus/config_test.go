package bus

import (
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

// parse is the configuration from args and the environment, as ecu reads it.
func parse(t *testing.T, args []string, env map[string]string) Config {
	t.Helper()
	var c Config
	fs := flag.NewFlagSet("ecu", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c.Flags(fs, func(k string) string { return env[k] })
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return c
}

// AC5: a src of replay.1 is refused at start, with a message naming the rule; so is
// anything else that is not one subject token of the allowed forms (R7).
func TestASrcThatIsNotOneTokenIsRefused(t *testing.T) {
	for _, src := range []string{
		"replay.1", "replay-1.2", "live.x", "*", "replay-*", ">", "replay->", "live ", "replay-a b", "replay-\t1",
		"", "replay-", "replay-_1", "LIVE", "replay", "other",
		"replay-" + strings.Repeat("a", 65),
	} {
		err := parse(t, []string{"-src", src}, nil).Validate()
		if !errors.Is(err, ErrSrc) {
			t.Errorf("src %q: %v, want refused", src, err)
		}
	}
	err := parse(t, []string{"-src", "replay.1"}, nil).Validate()
	if err == nil {
		t.Fatal("replay.1 not refused")
	}
	for _, want := range []string{`"replay.1"`, "one subject token", `no ".", "*", ">" or white space`, `"replay-<run>"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not say %s", err, want)
		}
	}
}

func TestTheSrcsAllowed(t *testing.T) {
	for _, src := range []string{"live", "parity", "replay-1", "replay-01JB2Z3Y4X", "replay-a_b-c", "replay-" + strings.Repeat("a", 64)} {
		if err := parse(t, []string{"-src", src}, nil).Validate(); err != nil {
			t.Errorf("src %q refused: %v", src, err)
		}
	}
}

// The flags win over the environment, which wins over the defaults.
func TestFlagsThenTheEnvironmentThenTheDefaults(t *testing.T) {
	if c := parse(t, nil, nil); c.URL != DefaultURL || c.Src != DefaultSrc {
		t.Errorf("defaults: %+v", c)
	}
	env := map[string]string{"TWR_NATS_URL": "nats://nats.example:4222", "TWR_SRC": "parity"}
	if c := parse(t, nil, env); c.URL != "nats://nats.example:4222" || c.Src != "parity" {
		t.Errorf("from the environment: %+v", c)
	}
	if c := parse(t, []string{"-nats", "nats://192.0.2.1:4222", "-src", "replay-7"}, env); c.URL != "nats://192.0.2.1:4222" ||
		c.Src != "replay-7" {
		t.Errorf("from the flags: %+v", c)
	}
	if err := parse(t, []string{"-nats", " "}, nil).Validate(); err == nil {
		t.Error("an empty URL was taken")
	}
}

// The log never shows a user or password from the URL.
func TestRedacted(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // made-up users at documentation addresses, no credentials
		"nats://127.0.0.1:4222":                         "nats://127.0.0.1:4222",
		"nats://user:secret@192.0.2.1:4222":             "nats://192.0.2.1:4222",
		"nats://a:b@192.0.2.1:4222, nats://192.0.2.2:1": "nats://192.0.2.1:4222,nats://192.0.2.2:1",
		"user:p@ss@192.0.2.1:4222":                      "192.0.2.1:4222",
	} {
		if got := redacted(in); got != want {
			t.Errorf("redacted(%q) = %q, want %q", in, got, want)
		}
	}
}

// Connect refuses what Validate refuses, before it reaches for the network.
func TestConnectRefusesABadSrc(t *testing.T) {
	if _, err := Connect(Config{URL: DefaultURL, Src: "replay.1"}, (&logs{}).logger(), Options{}); !errors.Is(err, ErrSrc) {
		t.Errorf("connected with src replay.1: %v", err)
	}
}
