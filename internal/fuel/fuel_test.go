package fuel

import (
	"math"
	"slices"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// lap is one crossing: the level on it, and the seconds since the frame before.
type lap struct {
	level float64
	secs  float64
	pit   bool
}

func cross(m *Model, laps ...lap) []Verdict {
	var out []Verdict
	for _, l := range laps {
		out = append(out, m.Cross(Crossing{Level: l.level, Secs: l.secs, PitLap: l.pit}))
	}
	return out
}

// R1: the use per lap is the drop over it, averaged over the last rolling_laps.
func TestTheAverageIsOverTheLastRollingLaps(t *testing.T) {
	m := New(DefaultConfig())
	if _, ok := m.LapsRemaining(); ok {
		t.Fatal("laps remaining before a lap was measured")
	}
	m.Start(100, false)
	cross(m, lap{level: 90}, lap{level: 70}, lap{level: 60}, lap{level: 50})
	if !slices.Equal(m.consumption, []float64{20, 10, 10}) {
		t.Errorf("consumption %v, want the last three laps", m.consumption)
	}
	if got, _ := m.LapsRemaining(); !near(got, 50/(40.0/3)) {
		t.Errorf("laps remaining %v", got)
	}
}

// R1: the in-lap and the out-lap of a stop are not measured (v1's
// test_fuel_in_lap_and_out_lap_are_not_measured).
func TestPitLapsAreNotMeasured(t *testing.T) {
	m := New(Config{RollingLaps: 5, RefuelEpsilon: 0.5, MaxGapSecs: 1})
	m.Start(100, false)
	got := cross(m, lap{level: 90}, lap{level: 80}, lap{level: 70, pit: true}, lap{level: 60, pit: true}, lap{level: 50})
	if !slices.Equal(got, []Verdict{Measured, Measured, PitLap, PitLap, Measured}) {
		t.Errorf("verdicts %v", got)
	}
	if !slices.Equal(m.consumption, []float64{10, 10, 10}) {
		t.Errorf("consumption %v", m.consumption)
	}
}

// R1: a lap crossed inside a telemetry gap, and the lap after it, are not
// measured; across a pause they are (v1's test_fuel_is_not_measured_across_a_
// telemetry_gap and test_fuel_is_measured_across_a_pause).
func TestALapInsideATelemetryGapIsNotMeasured(t *testing.T) {
	m := New(DefaultConfig())
	m.Start(100, false)
	got := cross(m, lap{level: 90, secs: 0.1}, lap{level: 79, secs: 10}, lap{level: 70, secs: 0.1}, lap{level: 60, secs: 0.1})
	if !slices.Equal(got, []Verdict{Measured, InGap, AfterGap, Measured}) {
		t.Errorf("verdicts %v", got)
	}
	if !slices.Equal(m.consumption, []float64{10, 10}) {
		t.Errorf("consumption %v", m.consumption)
	}

	paused := New(DefaultConfig())
	paused.Start(100, false)
	if v := paused.Cross(Crossing{Level: 90, Secs: 150, Bridged: true}); v != Measured {
		t.Errorf("across a pause: %v, want measured", v)
	}
}

// R1: the first lap of a session joined part way is not a whole lap; a level that
// rose is a stop the detector missed; a lap that used nothing is not measured.
func TestJoinedRefuelledAndUnusedLaps(t *testing.T) {
	m := New(DefaultConfig())
	m.Start(100, true)
	got := cross(m, lap{level: 95}, lap{level: 100}, lap{level: 100}, lap{level: 90})
	if !slices.Equal(got, []Verdict{Joined, Refuelled, Unused, Measured}) {
		t.Errorf("verdicts %v", got)
	}
	if New(DefaultConfig()).Cross(Crossing{Level: 1}) != Unused {
		t.Error("a crossing before the start was measured")
	}
}

// R2: a completed stop redoes the estimate at once from the level after it.
func TestAStopRedoesTheEstimateAtOnce(t *testing.T) {
	m := New(DefaultConfig())
	if m.Stop(1, 50) {
		t.Error("stint 1 is no stop")
	}
	m.Start(30, false)
	cross(m, lap{level: 20}, lap{level: 10})
	if !m.Stop(2, 45) {
		t.Fatal("stint 2 was not a new stint")
	}
	if got, _ := m.LapsRemaining(); !near(got, 4.5) {
		t.Errorf("laps remaining %v, want 4.5", got)
	}
	if m.Stop(2, 45) {
		t.Error("the same stint twice")
	}
}

// R1: fuel used per stint and in total, every drop added up (v1's
// test_fuel_stints_follow_the_pit_detector).
func TestFuelUsedFollowsThePitDetector(t *testing.T) {
	m := New(DefaultConfig())
	for _, level := range []float64{100, 90, 90.5, 89, 70, 100, 95} { // a rise alone splits nothing
		m.Burn(level)
	}
	m.Stop(2, 100)
	for _, level := range []float64{100, 80} {
		m.Burn(level)
	}
	if got := m.Used(); len(got) != 2 || !near(got[0], 35.5) || !near(got[1], 20) {
		t.Errorf("used %v, want [35.5 20]", got)
	}
	if !near(m.Total(), 55.5) || len(m.Stints()) != 2 {
		t.Errorf("total %v, stints %v", m.Total(), m.Stints())
	}

	short := New(DefaultConfig())
	short.Burn(100)
	short.Burn(99.6)
	short.Stop(2, 100)
	short.Burn(100)
	short.Burn(70)
	if got := short.Used(); len(got) != 1 || !near(got[0], 30) || !near(short.Total(), 30.4) {
		t.Errorf("used %v, total %v: the 0.4 before the stop is no stint to give", got, short.Total())
	}
}

func TestRollingLapsUnderOneIsOne(t *testing.T) {
	m := New(Config{RollingLaps: 0})
	m.Start(100, false)
	cross(m, lap{level: 90}, lap{level: 70})
	if !slices.Equal(m.consumption, []float64{20}) {
		t.Errorf("consumption %v", m.consumption)
	}
}
