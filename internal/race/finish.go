package race

import (
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// The timed finish (v1's engine.py at v1.0.0, paddock#85). A timed race has no
// FINISHED phase (no lap total to exceed). Its end is the first line crossing after
// the clock runs out, and GT7 shows it the way it shows the flag in a lap race: it
// takes the car over, throttle and brake at 0 while it coasts (Suzuka East timed
// race, 2026-10-16: 15 s of it, then the results screen). Drivers never coast
// hands-off that long at racing speed: 1.8 s at most in every race recorded with a
// field.
const (
	finishGraceSecs = 0.5 // inputs right at the line still count as the driver's (FINISH_GRACE_SECS)
	finishCoastSecs = 3.0 // FINISH_COAST_SECS
	finishCoastKph  = 20.0
)

// finish is the race's finish, and what the timed finish watches for it: v1's
// Engine.finished, _crossing and _coast_since. The zero value is a race not
// finished, as a start resets it.
type finish struct {
	kind       eventsv1.FinishKind // how the race finished; 0 before it (v1's finished false)
	crossing   *state              // the last line crossing of a timed race
	coasting   bool                // the car has coasted hands-off since coastSince
	coastSince float64
}

// timedLap is a complete lap with a time, as the best lap and the total time read
// it.
type timedLap struct {
	lap int32
	ms  int32
}

// observeFinish is where v1's Engine._step finds the finish: _timed_finish, then
// the first FINISHED frame. prev is the frame the rules compare against (seen),
// and kind the session's kind before this frame. The first finish in an open
// session publishes race.finished; there is at most one until the next start.
func (m *Machine) observeFinish(prev, s *state, t time.Time, kind kindVerdict) {
	var at *state
	switch {
	case m.timedFinish(prev, s, kind):
		m.finish.kind, at = eventsv1.FinishKind_FINISH_KIND_TIMED, m.finish.crossing
	case s.phase == phaseFinished && m.finish.kind == eventsv1.FinishKind_FINISH_KIND_UNSPECIFIED:
		m.finish.kind, at = eventsv1.FinishKind_FINISH_KIND_FLAG, s
	default:
		return
	}
	if m.session != nil {
		m.publishFinish(at, t)
	}
}

// timedFinish is v1's Engine._timed_finish: it says the timed race with a field
// (one GT7 flags with cars on track) finished on s, with the crossing it finished
// at in m.finish.crossing. After a lap time is posted, throttle and brake both 0
// at over 20 km/h for 3 s is GT7 driving the car after the line; inputs within
// 0.5 s of the line are still the driver's, and after that the driver is driving
// another lap. A paused car is not coasting.
func (m *Machine) timedFinish(prev, s *state, kind kindVerdict) bool {
	f := &m.finish
	if s.phase == phasePaused {
		f.coasting = false
	}
	if f.kind != eventsv1.FinishKind_FINISH_KIND_UNSPECIFIED ||
		prev == nil ||
		s.phase != phaseInRace ||
		kind.kind != eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD ||
		!kind.carsOnTrack {
		return false
	}
	if lapTimePosted(prev, s) {
		crossing := *s
		f.crossing, f.coasting = &crossing, false
		return false
	}
	if f.crossing == nil {
		return false
	}
	if s.throttle != 0 || s.brake != 0 || s.speedKph <= finishCoastKph {
		if s.t-f.crossing.t > finishGraceSecs {
			f.crossing = nil // the driver is driving: just another lap
		}
		f.coasting = false
		return false
	}
	if !f.coasting {
		f.coastSince, f.coasting = s.t, true
	}
	return s.t-f.coastSince >= finishCoastSecs
}

// publishFinish publishes race.finished as of at, the flag frame or the timed
// finish's crossing: its lap, the lap completed at the line, and its position
// (none outside a field, as for a lap), with the session's total time and best lap
// so far. The final lap of a lap race closes on the flag frame, before this.
func (m *Machine) publishFinish(at *state, t time.Time) {
	sess := m.session
	kind := m.kinds.kind()
	ev := &eventsv1.RaceFinished{
		SessionId: sess.id,
		Kind:      m.finish.kind,
		Lap:       at.lap - 1, // the lap the crossing closed, as v1 logs it
		At:        at.seq,
	}
	if p := at.racePosition(); p != 0 && !solo(kind) {
		ev.Position = &p
	}
	if total, ok := sess.totalTimeMs(); ok {
		ev.TotalTimeMs = &total
	}
	if best, ok := sess.bestLap(kind == eventsv1.SessionType_SESSION_TYPE_LAP_RACE); ok {
		ev.BestLap, ev.BestLapTimeMs = &best.lap, &best.ms
	}
	m.out = append(m.out, Output{
		Event:    m.event(sess.id+":race.finished:1", t),
		Session:  sess.id,
		Finished: ev,
	})
}

// totalTimeMs is the race's time, the sum of its complete laps' times (v1's
// summary total_time_ms); false when no lap was timed.
func (sess *session) totalTimeMs() (int32, bool) {
	var total int32
	for _, l := range sess.timedLaps {
		total += l.ms
	}
	return total, len(sess.timedLaps) > 0
}

// bestLap is v1's results.best_lap, the session's best lap as v1's summary, card
// and spoken summary give it: GT7's own best when the session has one (it leaves
// out a start lap from a grid ahead of the line), else the quickest complete lap,
// leaving out lap 1 of a lap race when there are others.
//
// TODO(paddock#83): the lap model owns the best lap, with lap.best; it must keep
// RaceFinished's best lap equal to the last lap.best of the session.
func (sess *session) bestLap(skipLap1 bool) (timedLap, bool) {
	if sess.gt7BestMs > 0 {
		for _, l := range sess.timedLaps {
			if l.ms == sess.gt7BestMs {
				return l, true
			}
		}
	}
	laps := sess.timedLaps
	if skipLap1 {
		var rest []timedLap
		for _, l := range laps {
			if l.lap != 1 {
				rest = append(rest, l)
			}
		}
		if len(rest) > 0 {
			laps = rest
		}
	}
	var best timedLap
	for _, l := range laps {
		if best.ms == 0 || l.ms < best.ms {
			best = l
		}
	}
	return best, best.ms != 0
}
