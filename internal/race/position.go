package race

import (
	"strconv"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// The final lap and the settled position (paddock#84): the decisions of v1's
// FinalLapRule and PositionRule (rules/final_lap.py and rules/position.py at
// v1.0.0), as facts. The position is v1's rule with only its settle left: the
// start grace (15 s) and the time between calls (10 s) are the callout's
// (paddock#94), so race.position gives every settled change and each v1 call has
// one at or before it.

// settleSecs is how long a new position must hold, and how long the grid slot is
// followed quietly after the race's first driving frame (v1's settle_secs).
const settleSecs = 3.0

// places is v1's PositionRule, with start_grace_secs and min_interval_secs 0. The
// zero value is the rule reset, as a start resets it.
type places struct {
	announced    int32 // the last position published, or the grid slot; 0 for none
	started      bool
	startedAt    float64 // the first driving frame with a field and a position
	pending      bool    // a new position is settling since pendingSince
	pendingSince float64
}

// observe is PositionRule.evaluate: it says the position settled on s, and the one
// before it. field is the rule's has_field, and inPitLane the pit detector's state
// after s. In the pit lane the position is followed but not taken, and settles
// again from the exit, so the first change after it is the net change.
func (p *places) observe(s *state, field, inPitLane bool) (previous int32, settled bool) {
	if !s.inRace() || !field {
		return 0, false
	}
	pos := s.racePosition()
	if pos == 0 {
		return 0, false
	}
	if !p.started {
		p.startedAt, p.started = s.t, true
	}
	if p.announced == 0 || s.t-p.startedAt < settleSecs {
		// The grid slot takes a moment to settle: qualifying reported P20 on the
		// first driving frame and P1 on the next (2026-10-14).
		p.announced = pos
		return 0, false
	}
	if inPitLane {
		p.pending = false
		return 0, false
	}
	if pos == p.announced {
		p.pending = false
		return 0, false
	}
	if !p.pending {
		p.pendingSince, p.pending = s.t, true
	}
	if s.t-p.pendingSince < settleSecs {
		return 0, false
	}
	previous, p.announced, p.pending = p.announced, pos, false
	return previous, true
}

// hasField is v1's Rule.has_field: other cars are racing for position, by the
// session kind, never GT7's cars-on-track flag (paddock#146): the kind's field
// once it is known, and before it, on a start's first frame, the field the kind
// reads from that frame alone. kind is the kind before s, as v1's rules read it.
func hasField(kind kindVerdict, s *state) bool {
	if kind.kind != eventsv1.SessionType_SESSION_TYPE_UNKNOWN {
		return kind.field
	}
	return frameField(s.totalLaps, s.totalCars, s.racePosition())
}

// observeRace is where v1's Engine._step runs its rules: after the pits, before
// the timed finish, the track, the kind and the lap store, on a driving frame or
// the one that continues a race, against prev, the frame the rules compare
// against (seen). kind is the session's kind before s. The rules' state is kept
// with or without a session, as v1's is; the events go into an open session only.
func (m *Machine) observeRace(prev, s *state, t time.Time, kind kindVerdict) {
	if !s.inRace() && !racingFrame(prev, s) {
		return
	}
	// FinalLapRule: the lap counter reaching the lap total, once a race. A timed
	// race has no lap total.
	if !m.finalLap && s.totalLaps > 0 && lapCompleted(prev, s) && s.lap == s.totalLaps {
		m.finalLap, m.ctx.FinalLap = true, true
		if m.session != nil {
			m.publishFinalLap(s, t)
		}
	}
	if previous, ok := m.places.observe(s, hasField(kind, s), m.pits.InPitLane()); ok && m.session != nil {
		m.publishPosition(previous, s, t)
		m.ctx.Settled = m.out[len(m.out)-1].Position
	}
}

// publishFinalLap publishes race.final_lap on s, the frame the lap counter reached
// the lap total.
func (m *Machine) publishFinalLap(s *state, t time.Time) {
	sess := m.session
	m.out = append(m.out, Output{
		Event:    m.event(sess.id+":race.final_lap:1", t),
		Session:  sess.id,
		FinalLap: &eventsv1.RaceFinalLapStarted{SessionId: sess.id, Lap: s.lap, At: s.seq},
	})
}

// publishPosition publishes race.position on s, the frame the position settled
// on, with the one before it.
func (m *Machine) publishPosition(previous int32, s *state, t time.Time) {
	sess := m.session
	m.published = s.racePosition()
	seq := strconv.FormatUint(uint64(s.seq), 10)
	m.out = append(m.out, Output{
		Event:   m.event(sess.id+":race.position:"+m.run+":"+seq, t),
		Session: sess.id,
		Position: &eventsv1.RacePositionChanged{
			SessionId:        sess.id,
			PreviousPosition: &previous,
			Position:         s.racePosition(),
			At:               s.seq,
			RunId:            m.run,
		},
	})
}
