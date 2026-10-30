package rules

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"

	"go.yaml.in/yaml/v3"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// The rules file (R5): v1's rules YAML, the shape of rules.example.yaml, so v1's
// file loads unchanged. Of it ecu reads queue.max_age_seconds and, under rules,
// each rule's enabled, priority, max_age_seconds, hold_secs and pit_lane, and the
// rule's own settings (its thresholds), which the rule reads. The templates, the
// script and the voice are radio's: ecu ignores them. Every value not given takes
// v1's default (config.DEFAULT_RULES at v1.0.0).

// QueueMaxAgeSecs is v1's queue.max_age_seconds: a callout's life when its rule
// sets none.
const QueueMaxAgeSecs = 8.0

// common are v1's defaults for the settings every rule takes (DEFAULT_RULES and
// base.py: priority from the rule's default_priority, pit_lane hold, no hold, no
// max age of its own).
type common struct {
	priority int32
	pitLane  eventsv1.PitLane
	holdSecs float64
	maxAge   float64
}

var defaults = map[string]common{
	"lap_time":     {priority: 20, pitLane: eventsv1.PitLane_PIT_LANE_HOLD},
	"fuel":         {priority: 15, pitLane: eventsv1.PitLane_PIT_LANE_SPEAK},
	"final_lap":    {priority: 10, pitLane: eventsv1.PitLane_PIT_LANE_HOLD},
	"position":     {priority: 12, pitLane: eventsv1.PitLane_PIT_LANE_DROP, holdSecs: 1},
	"start_brief":  {priority: 8, pitLane: eventsv1.PitLane_PIT_LANE_HOLD},
	"race_summary": {priority: 25, pitLane: eventsv1.PitLane_PIT_LANE_SPEAK, maxAge: 30},
	"temps":        {priority: 14, pitLane: eventsv1.PitLane_PIT_LANE_HOLD},
	"telemetry":    {priority: 5, pitLane: eventsv1.PitLane_PIT_LANE_SPEAK}, // v1's watchdog skipped the lane's quiet
}

var pitLanes = map[string]eventsv1.PitLane{
	"speak": eventsv1.PitLane_PIT_LANE_SPEAK,
	"hold":  eventsv1.PitLane_PIT_LANE_HOLD,
	"drop":  eventsv1.PitLane_PIT_LANE_DROP,
}

// Config is the rules file as ecu reads it.
type Config struct {
	// QueueMaxAgeSecs is a callout's life when its rule sets none.
	QueueMaxAgeSecs float64
	// Rules are each known rule's settings, by name: every rule of Order and
	// telemetry, given in the file or not.
	Rules map[string]*Settings
	// Unknown are the rules the file names that ecu doesn't know, sorted: a
	// misspelling, most likely. They are ignored, as v1 ignored them, and ecu
	// logs a warning naming them.
	Unknown []string
}

// Settings are one rule's settings: those every rule takes, and its own, which it
// reads with the getters.
type Settings struct {
	Name    string
	Enabled bool
	// Priority is v1's, from 1: lower is more urgent.
	Priority int32
	// MaxAgeSecs is the rule's own max_age_seconds, 0 for the queue's.
	MaxAgeSecs float64
	// HoldSecs is how long radio waits before speaking it (v1's hold_secs).
	HoldSecs float64
	PitLane  eventsv1.PitLane

	raw  map[string]any
	errs []error
}

// DefaultConfig is v1's defaults, with no file.
func DefaultConfig() Config {
	cfg, _ := ParseConfig(nil) // nothing to refuse in no file
	return cfg
}

// ParseConfig reads a rules file (YAML). An empty one is v1's defaults. A setting
// every rule takes with a value of the wrong type, a priority under 1, a negative
// time, or a pit_lane other than speak, hold or drop is an error. Rules the file
// names that ecu does not know are ignored, as v1 ignored them.
func ParseConfig(data []byte) (Config, error) {
	var file struct {
		Queue map[string]any            `yaml:"queue"`
		Rules map[string]map[string]any `yaml:"rules"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return Config{}, fmt.Errorf("rules file: %w", err)
	}
	cfg := Config{QueueMaxAgeSecs: QueueMaxAgeSecs, Rules: map[string]*Settings{}}
	var errs []error
	if v, ok := file.Queue["max_age_seconds"]; ok && v != nil {
		f, ok := number(v)
		if !ok || f <= 0 {
			errs = append(errs, fmt.Errorf("queue.max_age_seconds: want a number above 0, got %v", v))
		} else {
			cfg.QueueMaxAgeSecs = f
		}
	}
	for _, name := range slices.Sorted(maps.Keys(defaults)) {
		s, err := settings(name, file.Rules[name])
		errs = append(errs, err)
		cfg.Rules[name] = s
	}
	for _, name := range slices.Sorted(maps.Keys(file.Rules)) {
		if _, ok := defaults[name]; !ok {
			cfg.Unknown = append(cfg.Unknown, name)
		}
	}
	return cfg, errors.Join(errs...)
}

func settings(name string, raw map[string]any) (*Settings, error) {
	d := defaults[name]
	s := &Settings{Name: name, raw: raw}
	s.Enabled = s.Bool("enabled", true)
	s.Priority = int32(s.Int("priority", int(d.priority))) //nolint:gosec // checked below
	s.MaxAgeSecs = s.Float("max_age_seconds", d.maxAge)
	s.HoldSecs = s.Float("hold_secs", d.holdSecs)
	s.PitLane = d.pitLane
	if v, ok := raw["pit_lane"]; ok && v != nil {
		str, _ := v.(string)
		lane, ok := pitLanes[str]
		if !ok {
			s.fail("pit_lane: want speak, hold or drop, got %v", v)
		}
		s.PitLane = lane
	}
	if s.Priority < 1 || s.Priority > math.MaxInt16 {
		s.fail("priority: want 1 or more, got %d", s.Priority)
	}
	if s.MaxAgeSecs < 0 || s.HoldSecs < 0 {
		s.fail("max_age_seconds and hold_secs: want 0 or more")
	}
	return s, s.Err()
}

// Err is every error the getters met, each naming the rule and the key.
func (s *Settings) Err() error { return errors.Join(s.errs...) }

func (s *Settings) fail(format string, args ...any) {
	s.errs = append(s.errs, fmt.Errorf("rules.%s.%s", s.Name, fmt.Sprintf(format, args...)))
}

// get is the value of key, and false when the file has none (or null, which v1
// read as not set).
func (s *Settings) get(key string) (any, bool) {
	v, ok := s.raw[key]
	return v, ok && v != nil
}

// Null says the file sets key to null. The getters read null as not set, as v1
// read most settings; a rule whose v1 read null as off (temps' thresholds:
// `float(raw) if raw else None`) asks this.
func (s *Settings) Null(key string) bool {
	v, ok := s.raw[key]
	return ok && v == nil
}

// Float is the setting key as a number, or def when it is not set.
func (s *Settings) Float(key string, def float64) float64 {
	v, ok := s.get(key)
	if !ok {
		return def
	}
	f, ok := number(v)
	if !ok {
		s.fail("%s: want a number, got %v", key, v)
		return def
	}
	return f
}

// Int is the setting key as a whole number (v1's int(), which drops a fraction),
// or def when it is not set.
func (s *Settings) Int(key string, def int) int {
	v, ok := s.get(key)
	if !ok {
		return def
	}
	f, ok := number(v)
	if !ok || math.Abs(f) > math.MaxInt32 {
		s.fail("%s: want a whole number, got %v", key, v)
		return def
	}
	return int(f)
}

// Bool is the setting key as true or false, or def when it is not set.
func (s *Settings) Bool(key string, def bool) bool {
	v, ok := s.get(key)
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		s.fail("%s: want true or false, got %v", key, v)
		return def
	}
	return b
}

// String is the setting key as text, or def when it is not set.
func (s *Settings) String(key string, def string) string {
	v, ok := s.get(key)
	if !ok {
		return def
	}
	str, ok := v.(string)
	if !ok {
		s.fail("%s: want text, got %v", key, v)
		return def
	}
	return str
}

// Floats is the setting key as a list of numbers, or def when it is not set.
func (s *Settings) Floats(key string, def []float64) []float64 {
	v, ok := s.get(key)
	if !ok {
		return def
	}
	list, ok := v.([]any)
	out := make([]float64, 0, len(list))
	for _, x := range list {
		f, isNum := number(x)
		ok = ok && isNum
		out = append(out, f)
	}
	if !ok {
		s.fail("%s: want a list of numbers, got %v", key, v)
		return def
	}
	return out
}

// Strings is the setting key as a list of text, or def when it is not set.
func (s *Settings) Strings(key string, def []string) []string {
	v, ok := s.get(key)
	if !ok {
		return def
	}
	list, ok := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		str, isStr := x.(string)
		ok = ok && isStr
		out = append(out, str)
	}
	if !ok {
		s.fail("%s: want a list of text, got %v", key, v)
		return def
	}
	return out
}

// Decode reads the rule's settings into v, a struct whose fields carry yaml tags
// named for v1's keys (inline structs too), over the defaults v already holds.
// Keys v has no field for, such as the templates, are left alone.
func (s *Settings) Decode(v any) {
	set := map[string]any{}
	for k, x := range s.raw {
		if x != nil { // null is not set, as v1 read it
			set[k] = x
		}
	}
	data, err := yaml.Marshal(set)
	if err == nil {
		err = yaml.Unmarshal(data, v)
	}
	if err != nil {
		s.errs = append(s.errs, fmt.Errorf("rules.%s: %w", s.Name, err))
	}
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	default:
		return 0, false
	}
}
