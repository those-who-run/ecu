package race

import (
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/salsa"
)

// racing is a racing frame's state with a lap total, a car count and a position.
func racing(laps, cars, position int32) *state {
	return &state{phase: phaseInRace, lap: 1, totalLaps: laps, totalCars: cars, position: position}
}

// v1's tests of the kind (tests/test_sessionkind.py at v1.0.0), ported: the same
// frames give the same verdicts.
func TestKindFromTheLapTotalAndTheCars(t *testing.T) {
	for _, c := range []struct {
		laps, cars int32
		want       eventsv1.SessionType
	}{
		{3, 16, eventsv1.SessionType_SESSION_TYPE_LAP_RACE},
		{6, 2, eventsv1.SessionType_SESSION_TYPE_LAP_RACE}, // one AI opponent, Suzuka East
		{4, 1, eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS},
		{0, 20, eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD},
		{0, 1, eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED},
	} {
		var k kinds
		k.observe(racing(c.laps, c.cars, 1))
		if got := k.kind(); got != c.want {
			t.Errorf("%d laps, %d cars: %v, want %v", c.laps, c.cars, got, c.want)
		}
	}
}

func TestAGhostIsNotAFieldButBeingPassedIs(t *testing.T) {
	var k kinds
	k.observe(racing(0, 1, 1))
	k.observe(racing(0, 2, 1)) // a time trial's ghost
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED {
		t.Errorf("with a ghost: %v, want solo timed", got)
	}
	k.observe(racing(0, 2, 2))
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD {
		t.Errorf("passed: %v, want a timed field", got)
	}
}

func TestMinusOneCarsSaysNothingAndALateLapTotalCounts(t *testing.T) {
	var k kinds
	k.observe(racing(3, 12, 0))
	k.observe(racing(0, -1, -1))
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_LAP_RACE || k.laps != 3 {
		t.Errorf("after -1 cars: %v with %d laps, want a lap race of 3", got, k.laps)
	}

	k = kinds{}
	k.observe(racing(0, -1, -1))
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED {
		t.Errorf("a time trial, -1 all the way: %v, want solo timed", got)
	}
	k.observe(racing(2, 20, 0))
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_LAP_RACE {
		t.Errorf("the lap total posted late: %v, want a lap race", got)
	}
}

func TestOnlyRacingFramesCount(t *testing.T) {
	var k kinds
	k.observe(&state{phase: phaseAtTrack, totalLaps: 3, totalCars: 16})
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_UNKNOWN {
		t.Errorf("no racing frame: %v, want unknown", got)
	}
}

// The start carries the first frame's verdict and the summary the final one, and
// a lap's position follows the verdict when the lap ends (contracts v0.1.4). A
// ghost, then being passed: solo timed at the start, a timed field by the end.
func TestTheStartHasTheFirstVerdictAndTheEndTheFinalOne(t *testing.T) {
	d := drive{timed: true, cars: 2}
	d.add(0, 0, 50, -1, 1)    // 1: the track screen
	d.add(1, 0, 50, -1, 1)    // 2: the start, first with a ghost
	d.add(2, 0, 49, 60000, 1) // 3: lap 1, still first
	d.add(2, 0, 49, 60000, 2) // 4: passed: a field
	d.add(3, 0, 48, 61000, 2) // 5: lap 2
	d.add(0, 0, 48, 61000, 2) // 6: back to the track screen
	evs := events(run(d.all))
	if len(evs) != 5 {
		t.Fatalf("%d events, want start, laps 1 and 2, lap 3 cut short, end", len(evs))
	}
	if got := evs[0].Started.GetSessionType(); got != eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED {
		t.Errorf("start: %v, want the first frame's verdict, solo timed", got)
	}
	if lap := evs[1].Lap; lap.Position != nil {
		t.Errorf("lap 1, ended solo: position %d, want none", lap.GetPosition())
	}
	if lap := evs[2].Lap; lap.Position == nil || lap.GetPosition() != 2 {
		t.Errorf("lap 2, ended in a field: position %v, want 2", lap.Position)
	}
	sum := evs[4].Ended.GetSummary()
	if sum.GetSessionType() != eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD || sum.GetFinalPosition() != 2 ||
		sum.FinishingPosition != nil {
		t.Errorf("end: %v, want a timed field, final position 2 and no finishing position", sum)
	}
}

// A lap total GT7 posts after the start makes a lap race of what began as a timed
// field, and a lap race at the flag has a finishing position by the final verdict.
func TestALapTotalPostedLateMakesALapRace(t *testing.T) {
	var d drive
	d.add(0, 0, 50, -1, 5) // 1: the track screen
	d.add(1, 0, 50, -1, 5) // 2: the start, before the lap total
	d.all[1].TotalLaps = 0
	d.add(1, 0, 49, -1, 4)
	for lap := int16(2); lap <= 6; lap++ { // laps 1 to 5, then the flag on "lap 6 of 5"
		d.add(lap, 0, 49, 60000, 3)
	}
	evs := events(run(d.all))
	if got := evs[0].Started.GetSessionType(); got != eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD {
		t.Errorf("start: %v, want a timed field", got)
	}
	end := evs[len(evs)-1]
	sum := end.Ended.GetSummary()
	if end.Why != WhyFinished || sum.GetSessionType() != eventsv1.SessionType_SESSION_TYPE_LAP_RACE ||
		sum.FinishingPosition == nil || sum.GetFinishingPosition() != 3 {
		t.Errorf("end: %v %v, want a lap race finished in P3", end.Why, sum)
	}
}

// Outside a field there is no position: GT7 reports 1 in solo laps, and v1 stored
// it for each lap and as the final position. Leaving it out is a deliberate
// difference from v1 (contracts v0.1.4, paddock#276), as is no finishing position
// for solo laps at the flag, where v1 stored 1.
func TestNoPositionOutsideAField(t *testing.T) {
	for _, c := range []struct {
		name  string
		timed bool
		want  eventsv1.SessionType
	}{
		{"solo laps", false, eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS},
		{"solo timed", true, eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED},
	} {
		d := drive{timed: c.timed, cars: 1}
		d.add(0, 0, 50, -1, 1)
		for lap := int16(1); lap <= 6; lap++ { // a five-lap run to the flag, or six timed laps
			d.add(lap, 0, 50, 60000, 1)
		}
		d.add(0, salsa.FlagLoading, 50, 60000, 1)
		evs := events(run(d.all))
		if got := evs[0].Started.GetSessionType(); got != c.want {
			t.Errorf("%s: start %v", c.name, got)
		}
		var laps int
		for _, o := range evs {
			if o.Lap != nil {
				laps++
				if o.Lap.Position != nil {
					t.Errorf("%s: lap %d has position %d, want none (v1 stored 1)", c.name, o.Lap.GetLap(), o.Lap.GetPosition())
				}
			}
		}
		end := evs[len(evs)-1]
		sum := end.Ended.GetSummary()
		if laps == 0 || sum.GetSessionType() != c.want || sum.FinalPosition != nil || sum.FinishingPosition != nil {
			t.Errorf("%s: %d laps, end %v %v, want no final or finishing position (v1 stored 1)", c.name, laps, end.Why, sum)
		}
		if !c.timed && end.Why != WhyFinished {
			t.Errorf("%s: ended %v, want at the flag", c.name, end.Why)
		}
	}
}
