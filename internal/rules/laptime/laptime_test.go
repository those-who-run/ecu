package laptime

import (
	"testing"

	"github.com/those-who-run/ecu/internal/pit"
	"github.com/those-who-run/ecu/internal/race"
)

// shape is a call's kind, priority and slots, flattened for comparing; delta is
// -1 when delta_ms is absent, and priority 0 is the rule's own (20).
type shape struct {
	kind     string
	priority int32
	lap, ms  int32
	delta    int32
}

// step is a lap closed, against the best before it, and the call it gets (nil for
// none).
type step struct {
	lap         race.ClosedLap
	hasLapCount bool
	prevBest    int32
	want        *shape
}

func closed(n, ms int32, counts bool, role pit.Role) race.ClosedLap {
	return race.ClosedLap{Lap: n, TimeMs: ms, Counts: counts, Role: role}
}

func runSteps(t *testing.T, r *rule, steps []step) {
	t.Helper()
	for i, s := range steps {
		c, ok := r.call(s.lap, s.hasLapCount, s.prevBest)
		var got shape
		if ok {
			got = shape{c.Kind, c.Priority, c.Slots.GetLap(), c.Slots.GetLapTimeMs(), -1}
			if c.Slots.DeltaMs != nil {
				got.delta = c.Slots.GetDeltaMs()
			}
		}
		switch {
		case s.want == nil && ok:
			t.Errorf("step %d: got %+v, want no call", i, got)
		case s.want != nil && !ok:
			t.Errorf("step %d: got no call, want %+v", i, *s.want)
		case s.want != nil && got != *s.want:
			t.Errorf("step %d: got %+v, want %+v", i, got, *s.want)
		}
	}
}

func newRule() *rule { return &rule{cfg: defaults()} }

// AC1 (R2, R3, R5, R7), as real-nuc-suzuka-pit-stop-2026-10-15's laps close: lap
// 1 from the grid has no delta, nor has lap 2, with nothing to measure against;
// the in and out laps have their kinds and no delta; lap 5 is a new best by
// 0.997 s against GT7's best of lap 2, at priority 11.
func TestTheSuzukaPitStopRaceIsCalledAsV1Did(t *testing.T) {
	runSteps(t, newRule(), []step{
		{closed(1, 57109, false, pit.RoleNone), true, 0, &shape{KindDeltaFirst, 0, 1, 57109, -1}},
		{closed(2, 60538, true, pit.RoleNone), true, 0, &shape{KindDeltaFirst, 0, 2, 60538, -1}},
		{closed(3, 82544, false, pit.RoleIn), true, 60538, &shape{KindInLap, 0, 3, 82544, -1}},
		{closed(4, 69892, false, pit.RoleOut), true, 60538, &shape{KindOutLap, 0, 4, 69892, -1}},
		{closed(5, 59541, true, pit.RoleNone), true, 60538, &shape{KindNewBest, 11, 5, 59541, -997}},
		{closed(6, 60310, true, pit.RoleNone), true, 59541, &shape{KindDeltaSlower, 0, 6, 60310, 769}},
	})
}

// AC3 (R3): in a lap race, lap 2 measured only against lap 1 has no delta, even
// when lap 1 counted (a rolling start); lap 3 has one. In a timed session lap 2
// against a counted lap 1 has its delta.
func TestLap2OfALapRaceAgainstLap1OnlyHasNoDelta(t *testing.T) {
	runSteps(t, newRule(), []step{
		{closed(1, 51000, true, pit.RoleNone), true, 0, &shape{KindDeltaFirst, 0, 1, 51000, -1}},
		{closed(2, 53100, true, pit.RoleNone), true, 51000, &shape{KindDeltaFirst, 0, 2, 53100, -1}},
		{closed(3, 52000, true, pit.RoleNone), true, 51000, &shape{KindDeltaSlower, 0, 3, 52000, 1000}},
	})
	runSteps(t, newRule(), []step{
		{closed(1, 51000, true, pit.RoleNone), false, 0, &shape{KindDeltaFirst, 0, 1, 51000, -1}},
		{closed(2, 52500, true, pit.RoleNone), false, 51000, &shape{KindDeltaSlower, 0, 2, 52500, 1500}},
	})
}

// AC5 (R4): a lap 0.04 s slower in the raw times but the same in tenths is
// delta_equal, with the exact difference in delta_ms; one 0.04 s faster is no new
// best either.
func TestALapEqualInTenthsIsEqualToBest(t *testing.T) {
	runSteps(t, newRule(), []step{
		{closed(2, 63140, true, pit.RoleNone), false, 63100, &shape{KindDeltaEqual, 0, 2, 63140, 40}},
		{closed(3, 63060, true, pit.RoleNone), false, 63100, &shape{KindDeltaEqual, 0, 3, 63060, -40}},
	})
}

// R5, R6: the situational kinds, chosen on the spoken difference with v1's
// thresholds.
func TestTheSituationalKinds(t *testing.T) {
	for _, c := range []shape{
		{KindNewBestSmall, 11, 2, 59900, -100},
		{KindNewBest, 11, 2, 59800, -200},
		{KindDeltaSlowerSmall, 0, 2, 60200, 200},
		{KindDeltaSlower, 0, 2, 60300, 300},
		{KindDeltaSlower, 0, 2, 62000, 2000},
		{KindDeltaSlowerLarge, 0, 2, 62100, 2100},
		{KindPitLap, 0, 2, 62100, -1},
	} {
		role := pit.RoleNone
		if c.kind == KindPitLap {
			role = pit.RoleInOut
		}
		runSteps(t, newRule(), []step{{closed(2, c.ms, true, role), false, 60000, &c}})
	}
}

// R1: a lap with no time, or a lap already called, gets no call; Reset forgets
// the session.
func TestOneCallPerLap(t *testing.T) {
	r := newRule()
	runSteps(t, r, []step{
		{closed(1, 60000, true, pit.RoleNone), false, 0, &shape{KindDeltaFirst, 0, 1, 60000, -1}},
		{closed(1, 60000, true, pit.RoleNone), false, 0, nil},
		{race.ClosedLap{Lap: 2}, false, 0, nil},
	})
	r.Reset()
	runSteps(t, r, []step{
		{closed(1, 61000, true, pit.RoleNone), false, 0, &shape{KindDeltaFirst, 0, 1, 61000, -1}},
	})
}
