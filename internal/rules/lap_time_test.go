package rules

import (
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// lapCallShape is the call's kind, priority and slots, flattened for comparing; delta
// is -1 when delta_ms is absent.
type lapCallShape struct {
	kind     string
	priority int32
	lap, ms  int32
	delta    int32
}

func shape(c LapCall) lapCallShape {
	d := int32(-1)
	if c.Slots.DeltaMs != nil {
		d = c.Slots.GetDeltaMs()
	}
	return lapCallShape{c.Kind, c.Priority, c.Slots.GetLap(), c.Slots.GetLapTimeMs(), d}
}

// closed is a lap the lap model closed.
func closed(n, ms int32, counts bool, role eventsv1.PitRole) *eventsv1.LapCompleted {
	return &eventsv1.LapCompleted{Lap: n, LapTimeMs: ms, Complete: true, CountsForBest: counts, PitRole: role}
}

type step struct {
	lap  *eventsv1.LapCompleted
	f    LapTimeFrame
	want *lapCallShape // nil: no call
}

func runSteps(t *testing.T, r *LapTime, steps []step) {
	t.Helper()
	for i, s := range steps {
		got, ok := r.Lap(s.lap, s.f)
		switch {
		case s.want == nil && ok:
			t.Errorf("step %d: got %+v, want no call", i, shape(got))
		case s.want != nil && !ok:
			t.Errorf("step %d: got no call, want %+v", i, *s.want)
		case s.want != nil && shape(got) != *s.want:
			t.Errorf("step %d: got %+v, want %+v", i, shape(got), *s.want)
		}
	}
}

const none = eventsv1.PitRole_PIT_ROLE_NONE

// AC1 (R2, R3, R5, R7), as the lap model gives real-nuc-suzuka-pit-stop-2026-10-15's
// laps: lap 1 from the grid, left out of the best, has no delta; so has lap 2,
// measured against nothing; the in and out laps have their kinds and no delta;
// lap 5 is a new best by 0.997 s, against GT7's best of lap 2.
func TestTheSuzukaPitStopRaceIsCalledAsV1Did(t *testing.T) {
	race := LapTimeFrame{HasLapCount: true}
	gt7 := func(ms int32) LapTimeFrame { return LapTimeFrame{HasLapCount: true, PrevGameBestMs: ms} }
	runSteps(t, NewLapTime(DefaultLapTimeConfig()), []step{
		{closed(1, 57109, false, none), race, &lapCallShape{KindDeltaFirst, 20, 1, 57109, -1}},
		{closed(2, 60538, true, none), race, &lapCallShape{KindDeltaFirst, 20, 2, 60538, -1}},
		{closed(3, 82544, false, eventsv1.PitRole_PIT_ROLE_IN_LAP), gt7(60538),
			&lapCallShape{KindInLap, 20, 3, 82544, -1}},
		{closed(4, 69892, false, eventsv1.PitRole_PIT_ROLE_OUT_LAP), gt7(60538),
			&lapCallShape{KindOutLap, 20, 4, 69892, -1}},
		{closed(5, 59541, true, none), gt7(60538), &lapCallShape{KindNewBest, 11, 5, 59541, -997}},
		{closed(6, 60310, true, none), gt7(59541), &lapCallShape{KindDeltaSlower, 20, 6, 60310, 769}},
	})
}

// AC3 (R3): in a lap race, lap 2 measured only against lap 1 has no delta, even
// when lap 1 counted (a rolling start); lap 3 has one.
func TestLap2OfALapRaceAgainstLap1OnlyHasNoDelta(t *testing.T) {
	race := LapTimeFrame{HasLapCount: true}
	runSteps(t, NewLapTime(DefaultLapTimeConfig()), []step{
		{closed(1, 51000, true, none), race, &lapCallShape{KindDeltaFirst, 20, 1, 51000, -1}},
		{closed(2, 53100, true, none), LapTimeFrame{HasLapCount: true, PrevGameBestMs: 51000},
			&lapCallShape{KindDeltaFirst, 20, 2, 53100, -1}},
		{closed(3, 52000, true, none), LapTimeFrame{HasLapCount: true, PrevGameBestMs: 51000},
			&lapCallShape{KindDeltaSlower, 20, 3, 52000, 1000}},
	})
	// In a timed session lap 2 against a counted lap 1 has its delta.
	runSteps(t, NewLapTime(DefaultLapTimeConfig()), []step{
		{closed(1, 51000, true, none), LapTimeFrame{}, &lapCallShape{KindDeltaFirst, 20, 1, 51000, -1}},
		{closed(2, 52500, true, none), LapTimeFrame{PrevGameBestMs: 51000},
			&lapCallShape{KindDeltaSlower, 20, 2, 52500, 1500}},
	})
}

// AC5 (R4): a lap 0.04 s slower in the raw times but the same in tenths is
// delta_equal, with the exact difference in delta_ms; one 0.04 s faster is no
// new best either.
func TestALapEqualInTenthsIsEqualToBest(t *testing.T) {
	gt7 := LapTimeFrame{PrevGameBestMs: 63100}
	runSteps(t, NewLapTime(DefaultLapTimeConfig()), []step{
		{closed(2, 63140, true, none), gt7, &lapCallShape{KindDeltaEqual, 20, 2, 63140, 40}},
		{closed(3, 63060, true, none), gt7, &lapCallShape{KindDeltaEqual, 20, 3, 63060, -40}},
	})
}

// R5, R6: the situational kinds, chosen on the spoken difference.
func TestTheSituationalKinds(t *testing.T) {
	cases := []struct {
		ms   int32
		want lapCallShape
	}{
		{59900, lapCallShape{KindNewBestSmall, 11, 2, 59900, -100}},
		{59800, lapCallShape{KindNewBest, 11, 2, 59800, -200}},
		{60200, lapCallShape{KindDeltaSlowerSmall, 20, 2, 60200, 200}},
		{60300, lapCallShape{KindDeltaSlower, 20, 2, 60300, 300}},
		{62000, lapCallShape{KindDeltaSlower, 20, 2, 62000, 2000}},
		{62100, lapCallShape{KindDeltaSlowerLarge, 20, 2, 62100, 2100}},
	}
	for _, c := range cases {
		r := NewLapTime(DefaultLapTimeConfig())
		runSteps(t, r, []step{{closed(2, c.ms, true, none), LapTimeFrame{PrevGameBestMs: 60000}, &c.want}})
	}
}

// R2: without GT7's best the rule measures against its own, the quickest lap
// that counts: a pit lap or a start lap left out is never a best.
func TestWithoutGT7sBestTheBestIsTheQuickestCountingLap(t *testing.T) {
	runSteps(t, NewLapTime(DefaultLapTimeConfig()), []step{
		{closed(1, 50000, false, none), LapTimeFrame{}, &lapCallShape{KindDeltaFirst, 20, 1, 50000, -1}},
		{closed(2, 61000, false, eventsv1.PitRole_PIT_ROLE_IN_AND_OUT), LapTimeFrame{},
			&lapCallShape{KindPitLap, 20, 2, 61000, -1}},
		{closed(3, 60000, true, none), LapTimeFrame{}, &lapCallShape{KindDeltaFirst, 20, 3, 60000, -1}},
		{closed(4, 60500, true, none), LapTimeFrame{}, &lapCallShape{KindDeltaSlower, 20, 4, 60500, 500}},
		{closed(5, 59000, true, none), LapTimeFrame{}, &lapCallShape{KindNewBest, 11, 5, 59000, -1000}},
	})
}

// GT7's best on the frame before wins, even one carried over from an earlier
// session and on no lap of this one (Circuit Gilles Villeneuve, 2026-10-17).
func TestGT7sCarriedBestIsTheReference(t *testing.T) {
	runSteps(t, NewLapTime(DefaultLapTimeConfig()), []step{
		{closed(1, 98000, true, none), LapTimeFrame{PrevGameBestMs: 97524},
			&lapCallShape{KindDeltaSlower, 20, 1, 98000, 476}},
	})
}

// R1: a lap with no time, or a lap already called, gets no call; Reset forgets
// the session.
func TestOneCallPerLap(t *testing.T) {
	r := NewLapTime(DefaultLapTimeConfig())
	runSteps(t, r, []step{
		{closed(1, 60000, true, none), LapTimeFrame{}, &lapCallShape{KindDeltaFirst, 20, 1, 60000, -1}},
		{closed(1, 60000, true, none), LapTimeFrame{}, nil},
		{&eventsv1.LapCompleted{Lap: 2}, LapTimeFrame{}, nil},
	})
	r.Reset()
	runSteps(t, r, []step{
		{closed(1, 61000, true, none), LapTimeFrame{}, &lapCallShape{KindDeltaFirst, 20, 1, 61000, -1}},
	})
}
