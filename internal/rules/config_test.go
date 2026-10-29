package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// AC3 and R5: v1's rules.example.yaml (at v1.0.0, copied to testdata) loads
// unchanged, and gives v1's defaults: the same as no file.
func TestV1sExampleLoadsWithTheDefaults(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "rules.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueueMaxAgeSecs != 8 {
		t.Errorf("queue max age %v, want 8", cfg.QueueMaxAgeSecs)
	}
	want := map[string]struct {
		priority     int32
		lane         eventsv1.PitLane
		hold, maxAge float64
	}{
		"lap_time":     {20, eventsv1.PitLane_PIT_LANE_HOLD, 0, 0},
		"fuel":         {15, eventsv1.PitLane_PIT_LANE_SPEAK, 0, 0},
		"final_lap":    {10, eventsv1.PitLane_PIT_LANE_HOLD, 0, 0},
		"position":     {12, eventsv1.PitLane_PIT_LANE_DROP, 1, 0},
		"start_brief":  {8, eventsv1.PitLane_PIT_LANE_HOLD, 0, 0},
		"race_summary": {25, eventsv1.PitLane_PIT_LANE_SPEAK, 0, 30},
		"temps":        {14, eventsv1.PitLane_PIT_LANE_HOLD, 0, 0},
		"telemetry":    {5, eventsv1.PitLane_PIT_LANE_HOLD, 0, 0},
	}
	defaults := DefaultConfig()
	for name, w := range want {
		for _, c := range []Config{cfg, defaults} {
			s := c.Rules[name]
			if s == nil || !s.Enabled || s.Priority != w.priority || s.PitLane != w.lane || s.HoldSecs != w.hold || s.MaxAgeSecs != w.maxAge {
				t.Errorf("%s: %+v, want %+v", name, s, w)
			}
		}
	}
	if got := cfg.Rules["fuel"].Floats("thresholds_laps", nil); len(got) != 5 || got[0] != 10 {
		t.Errorf("fuel's thresholds %v", got)
	}
}

// AC3 and R5: a pit_lane v1 doesn't know, and a setting of the wrong type, are
// refused at start-up, naming the rule and the key.
func TestABadSettingIsRefused(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"rules:\n  position:\n    pit_lane: whisper\n", "rules.position.pit_lane"},
		{"rules:\n  fuel:\n    priority: high\n", "rules.fuel.priority"},
		{"rules:\n  fuel:\n    priority: 0\n", "rules.fuel.priority"},
		{"rules:\n  temps:\n    enabled: maybe\n", "rules.temps.enabled"},
		{"rules:\n  temps:\n    hold_secs: -1\n", "rules.temps.max_age_seconds and hold_secs"},
		{"queue:\n  max_age_seconds: never\n", "queue.max_age_seconds"},
		{"rules: [1, 2]\n", "rules file"},
	} {
		_, err := ParseConfig([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want an error naming %s", tc.yaml, err, tc.want)
		}
	}
}

// The file's own values win; null is not set, as in v1; a disabled rule is not
// run; a rule ecu doesn't know is ignored.
func TestTheFileOverridesTheDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(`queue: {max_age_seconds: 12}
rules:
  final_lap: {enabled: false}
  position: {priority: 3, pit_lane: speak, hold_secs: null, settle_secs: 2.5, kinds: [a, b]}
  race_summary: {max_age_seconds: 40, quit_min_laps: 2, flag: true, name: x}
  nonsense: {pit_lane: whisper}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Rules["position"]
	if cfg.QueueMaxAgeSecs != 12 || p.Priority != 3 || p.PitLane != eventsv1.PitLane_PIT_LANE_SPEAK || p.HoldSecs != 1 {
		t.Errorf("queue %v, position %+v", cfg.QueueMaxAgeSecs, p)
	}
	if p.Float("settle_secs", 3) != 2.5 || p.Float("absent", 7) != 7 || strings.Join(p.Strings("kinds", nil), ",") != "a,b" {
		t.Error("position's own settings")
	}
	rs := cfg.Rules["race_summary"]
	if rs.MaxAgeSecs != 40 || rs.Int("quit_min_laps", 1) != 2 || !rs.Bool("flag", false) || rs.String("name", "") != "x" {
		t.Error("race_summary's settings")
	}
	var own struct {
		QuitMinLaps int     `yaml:"quit_min_laps"`
		MaxAge      float64 `yaml:"max_age_seconds"`
		Kept        string  `yaml:"kept"`
	}
	own.Kept = "default"
	rs.Decode(&own)
	if rs.Err() != nil || own.QuitMinLaps != 2 || own.MaxAge != 40 || own.Kept != "default" {
		t.Errorf("Decode: %+v, %v", own, rs.Err())
	}
	if cfg.Rules["final_lap"].Enabled {
		t.Error("final_lap enabled")
	}
	if strings.Join(cfg.Unknown, ",") != "nonsense" {
		t.Errorf("unknown rules %v, want nonsense", cfg.Unknown)
	}
	e, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range e.Rules() {
		if name == "final_lap" {
			t.Error("a disabled rule runs")
		}
	}
}

// A rule's own setting of the wrong type stops the engine at start-up.
func TestARulesOwnBadSettingStopsTheEngine(t *testing.T) {
	cfg, err := ParseConfig([]byte("rules:\n  final_lap: {extra: [1, x], flag: 3, n: x, s: 1, list: 4}\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Rules["final_lap"]
	s.Floats("extra", nil)
	s.Bool("flag", false)
	s.Int("n", 0)
	s.String("s", "")
	s.Strings("list", nil)
	s.Float("n", 0)
	if err := s.Err(); err == nil || strings.Count(err.Error(), "rules.final_lap.") != 6 {
		t.Errorf("errors: %v", err)
	}
	real, had := builders["final_lap"]
	builders["final_lap"] = func(s *Settings) Rule { s.Float("n", 0); return &stub{} }
	t.Cleanup(func() {
		delete(builders, "final_lap")
		if had {
			builders["final_lap"] = real
		}
	})
	if _, err := New(cfg, nil); err == nil {
		t.Error("the engine started with a bad setting")
	}
}

// Register refuses a name v1 never had, and a name twice.
func TestRegisterRefusesUnknownAndTwice(t *testing.T) {
	real, had := builders["temps"]
	delete(builders, "temps")
	defer func() {
		delete(builders, "temps")
		if had {
			builders["temps"] = real
		}
	}()
	for _, name := range []string{"whisper", "temps"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) did not panic", name)
				}
			}()
			Register(name, func(*Settings) Rule { return &stub{} })
			Register(name, func(*Settings) Rule { return &stub{} })
		}()
	}
}
