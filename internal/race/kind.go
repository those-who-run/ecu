package race

import eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"

// kinds is v1's SessionDetector (src/those_who_run/sessionkind.py at v1.0.0), cut
// to the session's kind, which is all SessionType carries (paddock#34). GT7 sends no
// mode, so the kind comes from the lap total and the field of cars:
//
//   - a lap race: a lap total and at least two cars (a race against one AI car
//     reports two);
//   - solo laps: a lap total and one car;
//   - a timed session with a field: no lap total and three or more cars, or a
//     driver who has been anything but first (qualifying and a timed race look
//     exactly alike);
//   - solo timed: no lap total and one car, a time trial or practice. A second car
//     can appear part way through (the ghost), so two cars only make a field once
//     the driver has been anything but first.
//
// total_cars is -1 in the frames GT7 replays around the results screen and for the
// whole of a time trial, so -1 says nothing about the field. GT7 can also post the
// lap total a moment after the start, so the verdict is kept up to date: every frame
// is fed, and the verdict read when it is needed. The detector's fuel use and start
// type, which v1's rules read, are not here: no event carries them yet.
type kinds struct {
	valid bool // a racing frame has been seen
	laps  int32
	cars  int32
	field bool
}

// observe feeds one frame (SessionDetector.observe). Only racing frames count.
func (k *kinds) observe(s *state) {
	if !s.inRace() {
		return
	}
	k.valid = true
	if s.totalCars >= 0 {
		k.cars = s.totalCars
		if s.totalLaps > 0 {
			k.laps = s.totalLaps
		}
		if k.cars >= 3 || s.racePosition() > 1 {
			k.field = true
		}
	}
}

// kind is the verdict so far (SessionDetector._decide), as the contracts'
// SessionType: one value for each of v1's kinds.
func (k *kinds) kind() eventsv1.SessionType {
	switch {
	case !k.valid:
		return eventsv1.SessionType_SESSION_TYPE_UNKNOWN
	case k.laps > 0 && (k.cars >= 2 || k.field):
		return eventsv1.SessionType_SESSION_TYPE_LAP_RACE
	case k.laps > 0:
		return eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS
	case k.field:
		return eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD
	default:
		return eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED
	}
}

// solo says a session of type t has no field, so a position means nothing: GT7
// reports 1 there. LapCompleted.position and SessionSummary.final_position are
// absent for it (contracts v0.1.4), a deliberate difference from v1, which stored
// the 1.
func solo(t eventsv1.SessionType) bool {
	return t == eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS || t == eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED
}
