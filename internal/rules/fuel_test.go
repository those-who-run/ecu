package rules

import (
	"slices"
	"testing"

	"github.com/those-who-run/ecu/internal/pit"
)

// fr is one race frame for the fuel rule: its time, lap and level.
type fr struct {
	t     float64
	lap   int32
	fuel  float64
	total int32
}

// feed is v1's run_rule: each frame against the one before, every one in race.
func feed(r *Fuel, frames []fr, pits pit.Status) []FuelCall {
	var out []FuelCall
	var prev *fr
	for i := range frames {
		out = append(out, r.Evaluate(frameOf(prev, frames[i], pits))...)
		prev = &frames[i]
	}
	return out
}

func frameOf(prev *fr, f fr, pits pit.Status) FuelFrame {
	ff := FuelFrame{InRace: true, Lap: f.lap, TotalLaps: f.total, Fuel: f.fuel, Pits: pits}
	if prev != nil {
		ff.LapCompleted = f.lap == prev.lap+1 && prev.lap >= 1
		ff.PrevLap, ff.Secs = prev.lap, f.t-prev.t
	}
	return ff
}

// laps is one frame a lap from lap 1, each burning burn, in a race of total laps.
func laps(n int, fuel, burn float64, total int32) []fr {
	var out []fr
	for i := range n {
		out = append(out, fr{t: float64(i), lap: int32(i) + 1, fuel: fuel - burn*float64(i), total: total}) //nolint:gosec // small
	}
	return out
}

func kinds(calls []FuelCall) []string {
	var out []string
	for _, c := range calls {
		k := c.Kind
		if c.Laps > 0 {
			k += ":" + string(rune('0'+c.Laps))
		}
		out = append(out, k)
	}
	return out
}

func check(t *testing.T, got []FuelCall, want ...string) {
	t.Helper()
	if !slices.Equal(kinds(got), want) {
		t.Errorf("got %v, want %v", kinds(got), want)
	}
}

// R5, R7: v1's test_fuel_thresholds: 100 units, 10 a lap, 20 laps. The pit
// calls come ahead of the number, at priority 5; the thresholds at 15.
func TestFuelThresholdsAndPitCalls(t *testing.T) {
	got := feed(NewFuel(DefaultFuelConfig()), laps(11, 100, 10, 20), pit.Status{})
	check(t, got, "template:9", "template:5", "template:3", "template_pit_next", "template:2",
		"template_pit", "template:1", "template_critical")
	for _, c := range got {
		want := int32(15)
		if c.Kind == FuelPit || c.Kind == FuelPitNext || c.Kind == FuelCritical {
			want = 5
		}
		if c.Priority != want {
			t.Errorf("%s at priority %d, want %d", c.Kind, c.Priority, want)
		}
	}
	cfg := DefaultFuelConfig()
	cfg.PitWindow = false
	check(t, feed(NewFuel(cfg), laps(11, 100, 10, 20), pit.Status{}),
		"template:9", "template:5", "template:3", "template:2", "template:1", "template_critical")
}

// R4: a timed race has no laps left to cover, so the pit call is given.
func TestFuelPitCallInATimedRace(t *testing.T) {
	check(t, feed(NewFuel(DefaultFuelConfig()), laps(2, 20, 10, 0), pit.Status{}), "template_pit", "template:1")
}

// R4: silent when the fuel covers the race; silent with fuel off.
func TestFuelSilentWhenEnoughOrOff(t *testing.T) {
	check(t, feed(NewFuel(DefaultFuelConfig()), laps(5, 100, 10, 5), pit.Status{}))
	check(t, feed(NewFuel(DefaultFuelConfig()), laps(5, 100, 0, 20), pit.Status{}))
}

// R3: 2.99 laps is "just under three", and the pit calls go by the same count
// (v1's test_fuel_count_is_rounded_up_within_a_quarter_lap).
func TestFuelCountIsRoundedUpWithinAQuarterLap(t *testing.T) {
	frames := []fr{{0, 1, 100, 20}, {1, 2, 74.75, 20}}
	check(t, feed(NewFuel(DefaultFuelConfig()), frames, pit.Status{}), "template_short:3")
	cfg := DefaultFuelConfig()
	cfg.RoundUpLaps = 0
	check(t, feed(NewFuel(cfg), frames, pit.Status{}), "template_pit_next", "template:2")
	frames = append(frames, fr{2, 3, 47.3, 20})
	check(t, feed(NewFuel(DefaultFuelConfig()), frames, pit.Status{}),
		"template_short:3", "template_pit_next", "template_short:2")
}

// R4: tight to the flag once a stint; quiet with room, or in a timed race.
func TestFuelTightToTheFlagOnceAStint(t *testing.T) {
	r := NewFuel(DefaultFuelConfig())
	check(t, feed(r, []fr{{0, 1, 100, 3}, {1, 2, 66, 3}, {2, 3, 33, 3}}, pit.Status{}), "template_tight")
	check(t, feed(NewFuel(DefaultFuelConfig()), []fr{{0, 1, 100, 3}, {1, 2, 80, 3}}, pit.Status{}))
	check(t, feed(NewFuel(DefaultFuelConfig()), []fr{{0, 1, 100, 0}, {1, 2, 66, 0}}, pit.Status{}), "template_pit_next", "template_short:2")
}

// R4: "Fuel is fine to the end." on the final lap, only after a warning this
// stint (v1's test_fuel_reassures_on_the_final_lap_only_after_a_warning).
func TestFuelReassuresOnTheFinalLapOnlyAfterAWarning(t *testing.T) {
	cfg := DefaultFuelConfig()
	cfg.PitWindow = false
	worried := []fr{{0, 1, 100, 4}, {1, 2, 70, 4}, {2, 3, 55, 4}, {3, 4, 45, 4}}
	check(t, feed(NewFuel(cfg), worried, pit.Status{}), "template:2", "template_tight", "template_last_lap_enough")
	check(t, feed(NewFuel(cfg), laps(3, 100, 10, 4), pit.Status{}))
}

// R6: nothing on a crossing seen through a loading screen (the pit cut); a
// pause says it as usual (v1's test_fuel_says_nothing_on_the_crossing_into_the_pits).
func TestFuelSaysNothingOnACrossingThroughALoadingScreen(t *testing.T) {
	frames := []fr{{0, 1, 96.7, 6}, {1, 2, 65.6, 6}, {2, 3, 37.9, 6}}
	last := fr{3, 4, 22.9, 6}
	for _, loading := range []bool{true, false} {
		r := NewFuel(DefaultFuelConfig())
		feed(r, frames, pit.Status{})
		f := frameOf(&frames[2], last, pit.Status{})
		f.Bridged, f.Loading = true, loading
		got := r.Evaluate(f)
		if loading {
			check(t, got)
		} else {
			check(t, got, "template_critical")
		}
	}
}

func stop(in, out int32, fuelAfter float64) pit.Stop {
	return pit.Stop{Number: 1, InLap: in, OutLap: out, FuelAfter: fuelAfter, Exited: out > 0}
}

// R6: nothing on a crossing in the pit lane (Circuit Gilles Villeneuve), and
// nothing past the flag.
func TestFuelSaysNothingInThePitLaneOrPastTheFlag(t *testing.T) {
	frames := []fr{{0, 1, 50, 12}, {1, 2, 35, 12}, {2, 3, 20, 12}}
	r := NewFuel(DefaultFuelConfig())
	feed(r, frames, pit.Status{})
	open := pit.Status{InPitLane: true, Stops: []pit.Stop{stop(3, 0, 0)}}
	check(t, r.Evaluate(frameOf(&frames[2], fr{3, 4, 5, 12}, open)))

	r = NewFuel(DefaultFuelConfig())
	frames = []fr{{0, 1, 30, 2}, {1, 2, 20, 2}}
	feed(r, frames, pit.Status{})
	check(t, r.Evaluate(frameOf(&frames[1], fr{2, 3, 10, 2}, pit.Status{})))
}

// R2: a stop starts a new stint even without fuel: the calls are armed again and
// the estimate is redone at once (v1's test_fuel_a_stop_starts_a_new_stint_even_without_fuel).
func TestFuelAStopStartsANewStint(t *testing.T) {
	r := NewFuel(DefaultFuelConfig())
	frames := []fr{{0, 1, 30, 20}, {1, 2, 20, 20}, {2, 3, 10, 20}}
	check(t, feed(r, frames, pit.Status{}), "template_pit_next", "template:2", "template_pit", "template:1")
	done := pit.Status{Stops: []pit.Stop{stop(3, 4, 10)}}
	got := r.Evaluate(frameOf(&frames[2], fr{3, 3, 9, 20}, done)) // the exit, no crossing
	check(t, got)
	if laps, ok := r.Model().LapsRemaining(); !ok || laps != 1 {
		t.Errorf("laps remaining %v, want 1 at once", laps)
	}
	check(t, r.Evaluate(frameOf(&fr{4, 4, 8, 20}, fr{5, 5, 0.5, 20}, done)), "template_pit", "template_critical")
}

// R1: a level that rose over a lap is a stop the detector missed: the calls are
// armed again (v1's test_fuel_refuel_resets_announcements).
func TestFuelARefuelArmsTheCallsAgain(t *testing.T) {
	frames := laps(3, 40, 10, 30)
	frames = append(frames, fr{3, 4, 40, 30}, fr{4, 5, 30, 30})
	check(t, feed(NewFuel(DefaultFuelConfig()), frames, pit.Status{}),
		"template:3", "template_pit_next", "template:2", "template:4", "template:3")
}

// R1: the lap a session was joined on is not measured.
func TestFuelTheJoinedLapIsNotMeasured(t *testing.T) {
	r := NewFuel(DefaultFuelConfig())
	f := frameOf(nil, fr{0, 7, 50, 20}, pit.Status{})
	f.Joined = true
	r.Evaluate(f)
	r.Evaluate(frameOf(&fr{0, 7, 50, 20}, fr{1, 8, 45, 20}, pit.Status{}))
	if _, ok := r.Model().LapsRemaining(); ok {
		t.Error("the joined lap was measured")
	}
	r.Reset()
	if _, ok := r.Model().LapsRemaining(); ok {
		t.Error("Reset kept the estimate")
	}
}
