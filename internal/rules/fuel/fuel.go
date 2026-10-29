// Package fuel is v1's fuel rule (rules/fuel.py at v1.0.0, paddock#92): laps of
// fuel remaining at its thresholds, and the stop called. The arithmetic is
// package internal/fuel's Model, which the rule feeds and which the gauge reads;
// the rule decides what is said. Its comments are v1's, where each records what
// a recording showed.
//
// Warnings are only given when the estimate is shorter than the laps left in a
// lap-count race, or covers them only just.
//
// The estimate is spoken as whole laps, rounded up when it is within
// round_up_laps of the next one (2.99 is "just under three laps", not "two"),
// and the thresholds and the pit calls go by that count too. The rolling average
// reads heavy after a stop, from the last stint's laps (SUGO, 2026-10-18: 2.99
// laps called "two" with three in the tank, #247).
//
// The pit calls are the instruction that goes with the numbers, ahead of the
// threshold phrase for the same crossing, each once per stint: "Box next lap."
// when the count is pit_laps, and "Pit this lap." once it is under it (the lap
// after this one cannot be completed). The second comes even when the first was
// given, so the lap the driver has to box is always called.
//
// When the count covers the race, nothing is said, except once per stint when
// the margin is under tight_margin_laps: "Fuel to the flag, just." When fuel was
// a worry this stint (a threshold, pit or critical call) and the estimate covers
// the final lap, template_last_lap_enough is spoken as it starts.
//
// A crossing in the pits says nothing: the tank is about to be filled, or has
// just been. GT7 can put the line in any part of a stop: in the entry cut-scene
// (Suzuka East), in the exit cut-scene (Willow Springs) or in the pit lane
// itself (Circuit Gilles Villeneuve). A crossing seen through a loading screen
// is one of the first two.
//
// Its callouts are spoken in the pit lane (pit_lane speak, the engine's default
// for fuel): they are about the stop.
package fuel

import (
	"math"
	"slices"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	fuelmodel "github.com/those-who-run/ecu/internal/fuel"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
)

// Name is the rule's name, its key in the rules file.
const Name = "fuel"

// GaugeName is the gauge the rule shows between callouts: the model's laps
// remaining (R8; paddock#100's gt7_fuel_laps_remaining).
const GaugeName = "gt7_fuel_laps_remaining"

// The rule's kinds: the keys of its phrases in v1's script.yaml (contracts
// v0.2.3, Callout).
const (
	KindThreshold     = "template"       // laps
	KindShort         = "template_short" // laps: the count was rounded up
	KindCritical      = "template_critical"
	KindPitNext       = "template_pit_next"
	KindPit           = "template_pit"
	KindTight         = "template_tight"
	KindLastLapEnough = "template_last_lap_enough"
)

var _ = rules.Register(Name, func(s *rules.Settings) rules.Rule {
	cfg := DefaultConfig()
	s.Decode(&cfg)
	return New(cfg)
})

// Config is the rule's own settings, v1's keys of the same names; priority and
// pit_lane are the engine's. The templates are radio's (paddock#90).
type Config struct {
	fuelmodel.Config `yaml:",inline"`

	CriticalPriority int32   `yaml:"critical_priority"`
	ThresholdsLaps   []int   `yaml:"thresholds_laps"`
	PitWindow        bool    `yaml:"pit_window"`
	PitLaps          float64 `yaml:"pit_laps"`
	PitPriority      int32   `yaml:"pit_priority"`
	RoundUpLaps      float64 `yaml:"round_up_laps"`
	TightMarginLaps  float64 `yaml:"tight_margin_laps"`
}

// DefaultConfig is v1's: config.DEFAULT_RULES and FuelRule.reset.
func DefaultConfig() Config {
	return Config{
		Config:           fuelmodel.DefaultConfig(),
		CriticalPriority: 5,
		ThresholdsLaps:   []int{10, 5, 3, 2, 1},
		PitWindow:        true,
		PitLaps:          2,
		PitPriority:      5,
		RoundUpLaps:      0.25,
		TightMarginLaps:  0.5,
	}
}

// Rule is FuelRule for one session at a time.
type Rule struct {
	cfg        Config
	thresholds []int // descending, no repeats
	model      *fuelmodel.Model

	announced                     map[int]bool
	critical, pitNext, pit, tight bool
}

// New is the rule with cfg's settings.
func New(cfg Config) *Rule {
	t := slices.Clone(cfg.ThresholdsLaps)
	slices.Sort(t)
	t = slices.Compact(t)
	slices.Reverse(t)
	r := &Rule{cfg: cfg, thresholds: t, model: fuelmodel.New(cfg.Config)}
	r.Reset()
	return r
}

// Reset forgets the session.
func (r *Rule) Reset() {
	r.model.Reset()
	r.newStint()
}

// Model is the session's fuel model: laps remaining for the gauge and the live
// KV, and the fuel used per stint for the summary.
func (r *Rule) Model() *fuelmodel.Model { return r.model }

// Gauge is the model's laps remaining (rules.Gauge).
func (r *Rule) Gauge() (name string, value float64, ok bool) {
	laps, ok := r.model.LapsRemaining()
	return GaugeName, laps, ok
}

// newStint arms the thresholds and the pit calls again.
func (r *Rule) newStint() {
	r.announced = map[int]bool{}
	r.critical, r.pitNext, r.pit, r.tight = false, false, false, false
}

// Evaluate is the calls for one frame, in the order they are to be spoken.
func (r *Rule) Evaluate(ctx *race.Context) []rules.Callout {
	// v1's on_pit, on any frame: a completed stop starts a new stint.
	if stops := ctx.Pits.Stops; len(stops) > 0 && r.model.Stop(ctx.Pits.Stint(), stops[len(stops)-1].FuelAfter) {
		r.newStint()
	}
	if !ctx.Racing {
		return nil
	}
	cur := &ctx.Cur
	if ctx.Session != "" {
		r.model.Burn(cur.Fuel) // v1's store, fed the frames its rules saw
	}
	if !r.model.Started() && cur.InRace() && cur.Lap >= 1 {
		r.model.Start(cur.Fuel, ctx.Joined)
	}
	if !race.LapCompleted(ctx.Prev, cur) || !r.model.Started() {
		return nil
	}
	bridged := ctx.Bridged != telemetryv1.Phase_PHASE_UNSPECIFIED
	v := r.model.Cross(fuelmodel.Crossing{
		Level: cur.Fuel, Secs: cur.T - ctx.Prev.T, Bridged: bridged, PitLap: ctx.Pits.IsPitLap(ctx.Prev.Lap),
	})
	if v == fuelmodel.Refuelled {
		r.newStint()
	}
	remaining, ok := r.model.LapsRemaining()
	if !ok {
		return nil
	}
	pitCut := ctx.Bridged == telemetryv1.Phase_PHASE_LOADING
	if pitCut || ctx.Pits.InPitLane || (cur.HasLapCount() && cur.Lap > cur.TotalLaps) {
		return nil
	}
	whole := int(math.Floor(remaining + r.cfg.RoundUpLaps))
	if left, ok := cur.LapsLeft(); ok && whole >= int(left) {
		return r.covered(remaining, int(left))
	}

	var out []rules.Callout
	if r.cfg.PitWindow && !r.pit {
		switch {
		case float64(whole) < r.cfg.PitLaps:
			r.pit, r.pitNext = true, true
			out = append(out, rules.Callout{Kind: KindPit, Priority: r.cfg.PitPriority})
		case !r.pitNext && float64(whole) < r.cfg.PitLaps+1:
			r.pitNext = true
			out = append(out, rules.Callout{Kind: KindPitNext, Priority: r.cfg.PitPriority})
		}
	}

	if remaining < 1 {
		if !r.critical {
			r.critical = true
			for _, t := range r.thresholds {
				r.announced[t] = true
			}
			out = append(out, rules.Callout{Kind: KindCritical, Priority: r.cfg.CriticalPriority})
		}
		return out
	}

	crossed := false
	for _, t := range r.thresholds {
		if whole <= t && !r.announced[t] {
			r.announced[t], crossed = true, true
		}
	}
	if crossed {
		kind := KindThreshold
		if float64(whole) > remaining {
			kind = KindShort
		}
		laps := int32(whole) //nolint:gosec // a lap count
		out = append(out, rules.Callout{Kind: kind, Slots: &eventsv1.CalloutSlots{Laps: &laps}})
	}
	return out
}

// covered is the call when the estimate covers the laps left of a lap race.
func (r *Rule) covered(remaining float64, left int) []rules.Callout {
	if !r.tight && remaining-float64(left) < r.cfg.TightMarginLaps {
		r.tight = true
		return []rules.Callout{{Kind: KindTight}}
	}
	warned := len(r.announced) > 0 || r.pit || r.critical
	if warned && left == 1 && remaining >= float64(left) {
		return []rules.Callout{{Kind: KindLastLapEnough}}
	}
	return nil
}
