// Package telemetry is v1's telemetry rule (rules/telemetry.py's TelemetryVoice
// at v1.0.0, paddock#98): "Lost telemetry." once when packets stop while the last
// frame was a race or a pause (silence in a menu is normal), and "Telemetry
// back." when they resume after such a loss, so a quiet headset is never
// ambiguous.
//
// Frames are what is missing, so the rule is a rules.Watcher: the watchdog's tick
// (paddock#87) brings the loss, ctx.Lost with the last frame's phase, and the
// first frame after it brings the restore, ctx.Restored. It follows the
// detection, whatever its source: a replay never ticks, so it never speaks there.
//
// v1's defaults that the engine applies (internal/rules/config.go): priority 5,
// on .p0; pit_lane speak, since v1's watchdog pushed its callouts past the
// engine's pit-lane quiet, and a loss held in the lane would wait for an exit
// that may never come.
package telemetry

import (
	"strings"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/ecu/race"
)

// Name is the rule's name, its key in the rules file.
const Name = "telemetry"

// defaultPhases is v1's DEFAULT_PHASES: the last frame's phases a loss is spoken
// in.
var defaultPhases = []string{"in_race", "paused"}

var _ = rules.Register(Name, func(s *rules.Settings) rules.Rule {
	r := &rule{phases: map[string]bool{}}
	for _, p := range s.Strings("phases", defaultPhases) {
		r.phases[p] = true
	}
	return r
})

// rule is TelemetryVoice. announced is v1's lost_announced: a loss was spoken,
// so its end is too.
type rule struct {
	phases    map[string]bool
	announced bool
}

// WatchesFeed makes the rule run on the watchdog's tick.
func (*rule) WatchesFeed() {}

// Reset keeps announced, as v1 never reset TelemetryVoice: the first frame after
// a loss can be a start, which resets the rules before it is evaluated, and its
// "Telemetry back." must still be said.
func (*rule) Reset() {}

// Evaluate raises template_lost on a loss in one of the phases, and
// template_back on the restore after it. Neither has slots.
func (r *rule) Evaluate(ctx *race.Context) []rules.Callout {
	switch {
	case ctx.Lost != nil:
		if !r.phases[phaseName(ctx.Lost.GetPhase())] {
			return nil
		}
		r.announced = true
		return []rules.Callout{{Kind: "template_lost"}}
	case ctx.Restored != nil && r.announced:
		r.announced = false
		return []rules.Callout{{Kind: "template_back"}}
	}
	return nil
}

// phaseName is the phase as v1 named it in the rules file: PHASE_IN_RACE is
// in_race.
func phaseName(p telemetryv1.Phase) string {
	return strings.ToLower(strings.TrimPrefix(p.String(), "PHASE_"))
}
