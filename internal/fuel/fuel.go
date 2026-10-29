// Package fuel is the fuel model (paddock#92): fuel used per lap, measured at
// each line crossing, averaged over the last few laps, and the laps that leaves;
// and fuel used per stint and in total, every drop added up. It is the arithmetic
// of v1's FuelRule and FuelStints, from src/those_who_run/rules/fuel.py and
// src/those_who_run/stints.py at v1.0.0 (those-who-run/those-who-run), ported as
// it is; its comments are v1's, where each records what a recording showed.
//
// v1 worked fuel out three times, in the fuel rule, the race summary and the
// store, each handling a refuel its own way (those-who-run/those-who-run#186).
// v2 has this one model: the fuel rule (package rules) owns one per session and
// says what it means to the driver; the gauge and the live KV read its laps
// remaining.
//
// Consumption is measured at each line crossing (fuel at lap start minus fuel at
// lap end) and averaged over the last RollingLaps laps. The in-lap and the
// out-lap of a stop are not measured: both include the pit lane at the limiter,
// so they read light (the out-lap at Willow Springs used 13.3 against 14.1 to
// 15.9 for the laps before it, 2026-10-17) or, with the refuel inside the lap,
// negative. A completed stop starts a new stint, and the estimate is redone from
// the new level at once. A rise in fuel level over a lap still counts as a stop,
// for a stop the detector did not see.
//
// A crossing that falls inside a telemetry gap (no frames for more than
// MaxGapSecs, not a pause) is not measured, and neither is the lap after it: the
// first frame back is seconds past the line, so the lap would read heavy and the
// next one light (the pit call slipped a lap when only the first was skipped).
// The level still moves the estimate on.
//
// The package has no clock, no network and no files.
package fuel

// Config is the model's settings: v1's fuel rule's keys of the same names.
type Config struct {
	// RollingLaps is how many laps the average is over (rolling_laps, 3).
	RollingLaps int `yaml:"rolling_laps"`
	// RefuelEpsilon is how far the level may rise over a lap before it is a
	// refuel the pit detector missed (refuel_epsilon, 0.5).
	RefuelEpsilon float64 `yaml:"refuel_epsilon"`
	// MaxGapSecs is the longest time between two frames that is not a telemetry
	// gap (max_gap_secs, 1 s).
	MaxGapSecs float64 `yaml:"max_gap_secs"`
}

// DefaultConfig is v1's: config.DEFAULT_RULES and FuelRule.reset.
func DefaultConfig() Config {
	return Config{RollingLaps: 3, RefuelEpsilon: 0.5, MaxGapSecs: 1.0}
}

// MinStintFuel is the fuel a stint has to burn to be a figure worth giving (v1's
// MIN_STINT_FUEL): a stop straight after another, or a stint cut short by the
// flag, is left out of Used, so the results card never reads "zero percent, then
// seventy-four percent". Total keeps every drop.
const MinStintFuel = 1.0

// Crossing is a line crossing, as the model measures it.
type Crossing struct {
	// Level is the fuel level on the frame the lap counter moved on.
	Level float64
	// Secs is the time from the frame compared against to this one.
	Secs float64
	// Bridged says a pause or a loading screen came between the two frames: the
	// time between them is not lost telemetry.
	Bridged bool
	// PitLap says the lap that closed spent time in the pit lane: an in-lap or an
	// out-lap.
	PitLap bool
}

// Verdict is what the model made of a crossing's lap.
type Verdict int

// The verdicts, in the order v1 tries them.
const (
	// Measured is a lap whose use is in the average.
	Measured Verdict = iota
	// Joined is the first lap of a session joined part way: not a whole lap
	// (those-who-run/those-who-run#265).
	Joined
	// InGap is a lap crossed inside a telemetry gap.
	InGap
	// AfterGap is the lap after a crossing inside a telemetry gap.
	AfterGap
	// Refuelled is a lap the level rose over: a stop the detector missed. The
	// caller starts a new stint of its calls.
	Refuelled
	// PitLap is an in-lap or out-lap.
	PitLap
	// Unused is a lap that used no fuel (fuel off, or a car that does not use
	// it).
	Unused
)

// Model is one session's fuel model. The zero value is not ready: use New. A
// Model is not safe for concurrent use.
type Model struct {
	cfg Config

	started     bool
	lapStart    float64 // the level the lap in progress started on
	partial     bool    // the lap in progress was joined part way (#265)
	skipNext    bool    // the lap in progress started inside a telemetry gap
	consumption []float64
	remaining   float64
	known       bool // remaining is set
	stint       int32

	stints []float64 // fuel used in each stint
	last   float64
	seen   bool // last is set
}

// New is a model that has seen nothing, with cfg's settings; a RollingLaps under
// 1 is 1, as v1's.
func New(cfg Config) *Model {
	cfg.RollingLaps = max(cfg.RollingLaps, 1)
	m := &Model{cfg: cfg}
	m.Reset()
	return m
}

// Reset forgets the session: call on every session start.
func (m *Model) Reset() {
	*m = Model{cfg: m.cfg, stint: 1, stints: []float64{0}}
}

// Started says the first lap's start level is known.
func (m *Model) Started() bool { return m.started }

// Start takes the level the first measured lap starts on: v1 takes the first
// racing frame on lap 1 or later. joined says the session was joined part way,
// so the lap in progress is not a whole one.
func (m *Model) Start(level float64, joined bool) {
	m.started = true
	m.lapStart = level
	m.partial = joined
}

// Cross takes a line crossing: the lap that closed is measured, or left out for
// the reason the verdict gives, and the next lap starts on c.Level. Laps
// remaining is redone from c.Level once any lap has been measured. Cross before
// Start does nothing and returns Unused.
func (m *Model) Cross(c Crossing) Verdict {
	if !m.started {
		return Unused
	}
	used := m.lapStart - c.Level
	gap := !c.Bridged && c.Secs > m.cfg.MaxGapSecs
	skip := m.skipNext
	m.skipNext = gap
	partial := m.partial
	m.partial = false
	risen := used < -m.cfg.RefuelEpsilon
	var v Verdict
	switch {
	case partial && !risen:
		v = Joined
	case gap && !risen:
		v = InGap
	case skip && !risen:
		v = AfterGap
	case risen:
		v = Refuelled
	case c.PitLap:
		v = PitLap
	case used > 0:
		v = Measured
		m.consumption = append(m.consumption, used)
		if len(m.consumption) > m.cfg.RollingLaps {
			m.consumption = m.consumption[1:]
		}
	default:
		v = Unused
	}
	m.lapStart = c.Level
	if avg, ok := m.average(); ok {
		m.remaining, m.known = c.Level/avg, true
	}
	return v
}

// Stop takes the pit detector's stint (pit.Status.Stint) and the level after
// the last stop. A stint the model has not seen yet means a stop was completed:
// laps remaining is redone at once from fuelAfter, and Stop returns true, so the
// caller starts a new stint of its calls.
func (m *Model) Stop(stint int32, fuelAfter float64) bool {
	for len(m.stints) < int(stint) {
		m.stints = append(m.stints, 0)
	}
	if stint == m.stint {
		return false
	}
	m.stint = stint
	if avg, ok := m.average(); ok {
		m.remaining, m.known = fuelAfter/avg, true
	}
	return true
}

// average is the rolling average of the measured laps, summed in order as v1's
// sum() did; ok is false before the first.
func (m *Model) average() (avg float64, ok bool) {
	if len(m.consumption) == 0 {
		return 0, false
	}
	var sum float64
	for _, x := range m.consumption {
		sum += x
	}
	avg = sum / float64(len(m.consumption))
	return avg, avg > 0
}

// LapsRemaining is the laps the fuel lasts at the rolling average: the gauge
// (gt7_fuel_laps_remaining) and the live KV read it. ok is false until a lap
// has been measured.
func (m *Model) LapsRemaining() (laps float64, ok bool) { return m.remaining, m.known }

// Burn takes one racing frame's fuel level, as v1's store fed FuelStints: every
// drop since the last frame is added to the current stint, so a refuel never
// cancels what was burnt before it. A stint ends where the pit detector says a
// stop ended (Stop), not where the level goes up: the level also jumps on a
// restart that is not seen as one (the qualifying recording of 2026-10-14
// refills 75 -> 100 on the restart frame, #157), settles up a fraction at the
// start, and creeps up in an electric car (Xiaomi VGT, 2026-10-16). It returns
// the drop, 0 when the level did not fall.
func (m *Model) Burn(level float64) float64 {
	last, seen := m.last, m.seen
	m.last, m.seen = level, true
	if !seen || level >= last {
		return 0
	}
	m.stints[len(m.stints)-1] += last - level
	return last - level
}

// Total is the fuel burnt in the session, every drop added up (v1's
// fuel_burned).
func (m *Model) Total() float64 {
	var sum float64
	for _, x := range m.stints {
		sum += x
	}
	return sum
}

// Stints is the fuel burnt in each stint, every one (v1's fuel_stints).
func (m *Model) Stints() []float64 { return append([]float64(nil), m.stints...) }

// Used is the fuel burnt in each stint, leaving out those under MinStintFuel
// (v1's fuel_burned_by_stint).
func (m *Model) Used() []float64 {
	var out []float64
	for _, x := range m.stints {
		if x >= MinStintFuel {
			out = append(out, x)
		}
	}
	return out
}

// Consumption is the measured laps in the average, oldest first.
func (m *Model) Consumption() []float64 { return append([]float64(nil), m.consumption...) }
