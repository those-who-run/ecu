// Package rules is the built-in rules (paddock#89). Until the engine lands, it
// holds the fuel rule alone; this comment moves to the engine's own file then.
package rules

import (
	"math"
	"slices"

	"github.com/those-who-run/ecu/internal/fuel"
	"github.com/those-who-run/ecu/internal/pit"
)

// The fuel rule (paddock#92) warns of laps of fuel remaining at its thresholds,
// and calls the stop. It is v1's FuelRule, from src/those_who_run/rules/fuel.py
// at v1.0.0 (those-who-run/those-who-run), with its arithmetic moved into
// package fuel's Model; its comments are v1's, where each records what a
// recording showed.
//
// Warnings are only given when the estimate is shorter than the laps left in a
// lap-count race, or covers them only just.
//
// The estimate is spoken as whole laps, rounded up when it is within
// RoundUpLaps of the next one (2.99 is "just under three laps", not "two"), and
// the thresholds and the pit calls go by that count too. The rolling average
// reads heavy after a stop, from the last stint's laps (SUGO, 2026-10-18: 2.99
// laps called "two" with three in the tank, #247).
//
// The pit calls are the instruction that goes with the numbers, ahead of the
// threshold phrase for the same crossing, each once per stint: "Box next lap."
// when the count is PitLaps, and "Pit this lap." once it is under it (the lap
// after this one cannot be completed). The second comes even when the first was
// given, so the lap the driver has to box is always called.
//
// When the count covers the race, nothing is said, except once per stint when
// the margin is under TightMarginLaps: "Fuel to the flag, just." When fuel was a
// worry this stint (a threshold, pit or critical call) and the estimate covers
// the final lap, template_last_lap_enough is spoken as it starts.
//
// A crossing in the pits says nothing: the tank is about to be filled, or has
// just been. GT7 can put the line in any part of a stop: in the entry cut-scene
// (Suzuka East), in the exit cut-scene (Willow Springs) or in the pit lane
// itself (Circuit Gilles Villeneuve). A crossing seen through a loading screen
// is one of the first two.

// The fuel rule's kinds: the keys of its phrases in v1's script.yaml (contracts
// v0.2.3, Callout).
const (
	FuelThreshold     = "template"       // laps
	FuelShort         = "template_short" // laps: the count was rounded up
	FuelCritical      = "template_critical"
	FuelPitNext       = "template_pit_next"
	FuelPit           = "template_pit"
	FuelTight         = "template_tight"
	FuelLastLapEnough = "template_last_lap_enough"
)

// FuelConfig is the fuel rule's settings, v1's keys of the same names. The
// templates are radio's (paddock#90), not read here.
type FuelConfig struct {
	fuel.Config `yaml:",inline"`

	Priority         int32   `yaml:"priority"`
	CriticalPriority int32   `yaml:"critical_priority"`
	ThresholdsLaps   []int   `yaml:"thresholds_laps"`
	PitWindow        bool    `yaml:"pit_window"`
	PitLaps          float64 `yaml:"pit_laps"`
	PitPriority      int32   `yaml:"pit_priority"`
	RoundUpLaps      float64 `yaml:"round_up_laps"`
	TightMarginLaps  float64 `yaml:"tight_margin_laps"`
	// PitLane is what happens to its callouts in the pit lane: v1's fuel rule
	// speaks them, since its calls are about the stop.
	PitLane string `yaml:"pit_lane"`
}

// DefaultFuelConfig is v1's: config.DEFAULT_RULES and FuelRule.reset.
func DefaultFuelConfig() FuelConfig {
	return FuelConfig{
		Config:           fuel.DefaultConfig(),
		Priority:         15,
		CriticalPriority: 5,
		ThresholdsLaps:   []int{10, 5, 3, 2, 1},
		PitWindow:        true,
		PitLaps:          2,
		PitPriority:      5,
		RoundUpLaps:      0.25,
		TightMarginLaps:  0.5,
		PitLane:          "speak",
	}
}

// FuelFrame is what the fuel rule reads on a frame: the engine calls it on the
// frames v1 evaluated rules on (a race frame, or the frame that ends one), and
// compares against the last driving frame across a pause or a loading screen.
type FuelFrame struct {
	// InRace is the frame's phase is IN_RACE.
	InRace bool
	// Lap and TotalLaps are GT7's lap counter and lap total (0: timed).
	Lap, TotalLaps int32
	// Fuel is the fuel level.
	Fuel float64
	// LapCompleted says the lap counter moved on by one from the frame compared
	// against (v1's lap_completed(prev, state)); PrevLap is that frame's lap and
	// Secs the time between the two.
	LapCompleted bool
	PrevLap      int32
	Secs         float64
	// Bridged says a pause or loading screen came between the frame compared
	// against and this one; Loading says it was a loading screen (a pit cut).
	Bridged, Loading bool
	// Joined says the session was joined part way.
	Joined bool
	// Pits is the pit detector's status as of its last change.
	Pits pit.Status
}

// FuelCall is a callout the fuel rule raises: its kind, the laps slot (0: not
// set; set for FuelThreshold and FuelShort) and the priority.
type FuelCall struct {
	Kind     string
	Laps     int32
	Priority int32
}

// Fuel is the fuel rule for one session. Use NewFuel; Reset on every session
// start.
type Fuel struct {
	cfg        FuelConfig
	thresholds []int // descending, no repeats
	model      *fuel.Model

	announced                     map[int]bool
	critical, pitNext, pit, tight bool
}

// NewFuel is the fuel rule with cfg's settings.
func NewFuel(cfg FuelConfig) *Fuel {
	t := slices.Clone(cfg.ThresholdsLaps)
	slices.Sort(t)
	t = slices.Compact(t)
	slices.Reverse(t)
	r := &Fuel{cfg: cfg, thresholds: t, model: fuel.New(cfg.Config)}
	r.Reset()
	return r
}

// Reset forgets the session.
func (r *Fuel) Reset() {
	r.model.Reset()
	r.newStint()
}

// Model is the session's fuel model: laps remaining for the gauge and the live
// KV, and the fuel used per stint for the summary.
func (r *Fuel) Model() *fuel.Model { return r.model }

// newStint arms the thresholds and the pit calls again.
func (r *Fuel) newStint() {
	r.announced = map[int]bool{}
	r.critical, r.pitNext, r.pit, r.tight = false, false, false, false
}

// Evaluate is the calls for one frame, in the order they are to be spoken.
func (r *Fuel) Evaluate(f FuelFrame) []FuelCall {
	if stops := f.Pits.Stops; len(stops) > 0 {
		if r.model.Stop(f.Pits.Stint(), stops[len(stops)-1].FuelAfter) {
			r.newStint()
		}
	}
	r.model.Burn(f.Fuel)
	if !r.model.Started() && f.InRace && f.Lap >= 1 {
		r.model.Start(f.Fuel, f.Joined)
	}
	if !f.LapCompleted || !r.model.Started() {
		return nil
	}
	v := r.model.Cross(fuel.Crossing{
		Level: f.Fuel, Secs: f.Secs, Bridged: f.Bridged, PitLap: f.Pits.IsPitLap(f.PrevLap),
	})
	if v == fuel.Refuelled {
		r.newStint()
	}
	remaining, ok := r.model.LapsRemaining()
	if !ok {
		return nil
	}
	if (f.Bridged && f.Loading) || f.Pits.InPitLane || (f.TotalLaps > 0 && f.Lap > f.TotalLaps) {
		return nil
	}
	whole := int(math.Floor(remaining + r.cfg.RoundUpLaps))
	if f.TotalLaps > 0 {
		left := int(max(f.TotalLaps-f.Lap+1, 0))
		if whole >= left {
			return r.covered(remaining, left)
		}
	}

	var out []FuelCall
	if r.cfg.PitWindow && !r.pit {
		switch {
		case float64(whole) < r.cfg.PitLaps:
			r.pit, r.pitNext = true, true
			out = append(out, FuelCall{Kind: FuelPit, Priority: r.cfg.PitPriority})
		case !r.pitNext && float64(whole) < r.cfg.PitLaps+1:
			r.pitNext = true
			out = append(out, FuelCall{Kind: FuelPitNext, Priority: r.cfg.PitPriority})
		}
	}

	if remaining < 1 {
		if !r.critical {
			r.critical = true
			for _, t := range r.thresholds {
				r.announced[t] = true
			}
			out = append(out, FuelCall{Kind: FuelCritical, Priority: r.cfg.CriticalPriority})
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
		kind := FuelThreshold
		if float64(whole) > remaining {
			kind = FuelShort
		}
		out = append(out, FuelCall{Kind: kind, Laps: int32(whole), Priority: r.cfg.Priority}) //nolint:gosec // a lap count
	}
	return out
}

// covered is the call when the estimate covers the left laps of a lap race.
func (r *Fuel) covered(remaining float64, left int) []FuelCall {
	if !r.tight && remaining-float64(left) < r.cfg.TightMarginLaps {
		r.tight = true
		return []FuelCall{{Kind: FuelTight, Priority: r.cfg.Priority}}
	}
	warned := len(r.announced) > 0 || r.pit || r.critical
	if warned && left == 1 && remaining >= float64(left) {
		return []FuelCall{{Kind: FuelLastLapEnough, Priority: r.cfg.Priority}}
	}
	return nil
}
