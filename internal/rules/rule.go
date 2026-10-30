// Package rules is ecu's rule engine (paddock#89): v1's built-in rules, run in
// v1's order on every frame against one read-only race.Context, and what they
// raise turned into the contracts' Callout (contracts v0.2.3), ready to publish on
// twr.v1.<src>.callout.p0, .p1 or .p2.
//
// The engine decides everything about a callout but its words: which rule raised
// it, its kind and slots, its priority, when it is created, held and stale, and
// what happens to it in the pit lane. radio phrases and speaks it.
//
// A rule is a package of its own under internal/rules (laptime, fuel, finallap,
// ...), which registers itself with Register; internal/rules/all imports every
// one. README.md says how to add one. Like internal/race, the package and the
// rules do no I/O: no NATS, no network, no files, no clock (a lint rule and
// imports_test.go hold it). Reading the rules file is cmd/ecu's.
package rules

import (
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
)

// Rule is one of v1's rules (rules/base.py's Rule): state in, callouts out.
//
// Evaluate is called on every frame, in a race or not, with the frame as v1's
// rules saw it; a rule never keeps ctx past the call. ctx.Racing is where v1
// called the rule's evaluate, and the context's other fields stand for v1's
// hooks (README.md has the table). Reset is called on every start, before the
// start's frame is evaluated (v1's reset).
//
// A rule that panics is recovered, logged with its name and counted, and the
// frame and the other rules carry on (R4).
type Rule interface {
	Reset()
	Evaluate(ctx *race.Context) []Callout
}

// Callout is what a rule raises: the meaning, which radio phrases. The engine
// stamps the rest (README.md, "What the engine stamps").
type Callout struct {
	// Kind is the script key the rule chose (contracts' Callout.kind), such as
	// template or template_new_best.
	Kind string
	// Slots are the values that fill the phrase; nil for none.
	Slots *eventsv1.CalloutSlots
	// Priority overrides the rule's own (a new best's 11, critical fuel's 5); 0
	// takes the rule's.
	Priority int32
	// AsOf is the frame the callout is created as of, when it is not the frame it
	// is raised on: a timed finish's line crossing (v1 created the race summary
	// at the crossing's time). Its Time is created_at. nil is the frame raised on.
	// at is always the frame raised on.
	AsOf *race.View
	// Alarm is the car alarm the same firing raises (contracts' CarAlarmRaised,
	// paddock#97: the temps rule), nil for none. The rule sets what fired; the
	// engine stamps session_id, at and run_id and publishes it as an event on the
	// frame raised on, whatever the pit lane does with the callout. It is not
	// published with no session open.
	Alarm *eventsv1.CarAlarmRaised
}

// Gauge is a rule that has a value to show between callouts (R8), such as the
// fuel rule's laps remaining. ok is false while it has none.
type Gauge interface {
	Gauge() (name string, value float64, ok bool)
}

// Builder makes a rule from its settings, once at start-up. It reads its own
// settings with s's getters, whose errors the engine reports for it.
type Builder func(s *Settings) Rule

// Order is v1's rule registry order (rules/__init__.py's REGISTRY), the order the
// rules run in on each frame. A rule's name is its key in the rules file. The
// telemetry rule (paddock#98) is not a frame rule and is not here.
var Order = []string{"lap_time", "fuel", "final_lap", "position", "start_brief", "race_summary", "temps"}

var builders = map[string]Builder{}

// Register makes the rule name, one of Order, available to the engine. A rule's
// package calls it from a package-level var, so importing the package registers
// it: `var _ = rules.Register("final_lap", build)`. It panics on a name not in
// Order or registered twice, which is a programming error.
func Register(name string, b Builder) bool {
	if !known(name) {
		panic("rules: " + name + " is not in rules.Order")
	}
	if _, dup := builders[name]; dup {
		panic("rules: " + name + " registered twice")
	}
	builders[name] = b
	return true
}

func known(name string) bool {
	for _, n := range Order {
		if n == name {
			return true
		}
	}
	return false
}
