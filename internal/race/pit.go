package race

import (
	"math"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/pit"
)

// The session's pit stops (paddock#78): v1's Engine._apply_pits, from engine.py at
// v1.0.0, fed by internal/pit's detector. The detector sees every frame and is
// reset on each start, as v1's is; its entries, exits and withdrawn entries are the
// session's, and each is a pit.entered, a pit.exited or a pit.teleport.

// pitCounts are the session's pit counts, which the events' ids are built from
// (contracts v0.2.3): every entry, withdrawn or not, for pit.entered's and
// pit.teleport's <entry>, and the stops completed, for pit.exited's <stop>.
// They are the session's, not the detector's, so a session continued after an
// ecu restart can carry them on (TODO(paddock#76): start them from the stops
// before the restart). They only go up: when the detector resets inside a
// session (the lap counter goes back without a restart the machine sees), the
// ids carry on, but the summary's stops come from the detector and number from
// 1 again, so PitExited's stop.stop and the summary's can disagree. v1's
// summary does the same; paddock#76 settles it with the restore.
type pitCounts struct {
	entries int32 // every entry, the open one included
	stops   int32 // the stops completed
	// The open entry's number and its PitEntered's times and seq, for its
	// PitExited or PitTeleport.
	entry      int32
	enteredT   float64
	enteredAt  time.Time
	enteredSeq uint32
}

// observePits feeds the detector the frame f, received at t, and publishes an
// entry, an exit or an entry withdrawn while a session is open. A frame changes
// the pits at most once, so it gives at most one pit event.
func (m *Machine) observePits(f *telemetryv1.Frame, t time.Time) {
	change := m.pits.Observe(f)
	sess := m.session
	if change == pit.Unchanged || sess == nil {
		return
	}
	status := m.pits.Status()
	m.ctx.PitChange = change
	// The laps' roles come from the status as applied here (v1's set_pits).
	sess.laps.pits = status
	m.summaryPits(status)
	pits := &sess.pits
	switch change {
	case pit.Entered:
		stop := status.Stops[len(status.Stops)-1]
		pits.entries++
		pits.entry, pits.enteredT, pits.enteredAt, pits.enteredSeq = pits.entries, stop.EntryT, t, f.GetSeq()
		ev := &eventsv1.PitEntered{
			SessionId:  sess.id,
			Entry:      pits.entries,
			Stop:       pits.stops + 1,
			InLap:      stop.InLap,
			EnteredT:   stop.EntryT,
			EnteredAt:  timestamppb.New(t),
			FuelBefore: round3(stop.FuelBefore),
			Evidence:   stop.Evidence,
			At:         f.GetSeq(),
		}
		if p := stop.PositionBefore; p != 0 {
			ev.PositionBefore = &p
		}
		m.out = append(m.out, Output{
			Event:      m.event(sess.id+":pit.entered:"+strconv.Itoa(int(pits.entries)), t),
			Session:    sess.id,
			PitEntered: ev,
		})
	case pit.Exited:
		stop := status.Stops[len(status.Stops)-1]
		pits.stops++
		record := pitStop(stop)
		record.Stop = pits.stops
		ev := &eventsv1.PitExited{
			SessionId: sess.id,
			Stop:      record,
			Stint:     pits.stops + 1,
			Entry:     pits.entry,
			EnteredAt: timestamppb.New(pits.enteredAt),
			ExitedAt:  timestamppb.New(t),
			At:        f.GetSeq(),
			ExitedT:   stop.ExitT,
			EnteredT:  pits.enteredT,
		}
		m.out = append(m.out, Output{
			Event:     m.event(sess.id+":pit.exited:"+strconv.Itoa(int(pits.stops)), t),
			Session:   sess.id,
			PitExited: ev,
		})
	case pit.Withdrawn:
		// The entry was no stop (paddock#79): the detector's status is back to
		// what it was before it, so the stop count stays and the next entry
		// takes the stop number again, as v1's does. The entry keeps its number.
		jump := status.Teleports[len(status.Teleports)-1]
		// TODO(paddock#76): leave entry_at out when the entry was published by
		// an earlier ecu run, whose seqs this run's do not continue.
		at := pits.enteredSeq
		ev := &eventsv1.PitTeleport{
			SessionId:  sess.id,
			Entry:      pits.entry,
			EntryAt:    &at,
			Lap:        jump.Lap,
			Evidence:   jump.Evidence,
			At:         f.GetSeq(),
			WithdrawnT: jump.T,
		}
		m.out = append(m.out, Output{
			Event:       m.event(sess.id+":pit.teleport:"+strconv.Itoa(int(pits.entry)), t),
			Session:     sess.id,
			PitTeleport: ev,
		})
	}
}

// pitStop is the stop as the contracts' PitStop, as v1's PitStop.record() writes
// it: fuel to 3 decimals, times in seconds to 2 decimals (here in ms), the
// limiter's speed to 1. A stop still open has no out-lap, lane time or position
// after (contracts v0.2.3), where v1 recorded its last position.
func pitStop(s pit.Stop) *eventsv1.PitStop {
	kph := roundN(s.PitLaneKph, 1)
	out := &eventsv1.PitStop{
		Stop:         s.Number,
		InLap:        s.InLap,
		StationaryMs: secsToMs(s.StationarySecs),
		FuelBefore:   round3(s.FuelBefore),
		FuelAfter:    round3(s.FuelAfter),
		FuelAdded:    round3(s.FuelAdded()),
		TyresChanged: s.TyresChanged,
		PitLaneKph:   &kph,
		Evidence:     s.Evidence,
	}
	if p := s.PositionBefore; p != 0 {
		out.PositionBefore = &p
	}
	if secs, done := s.PitLaneSecs(); done {
		lane := secsToMs(secs)
		out.OutLap, out.PitLaneMs = &s.OutLap, &lane
		if p := s.PositionAfter; p != 0 {
			out.PositionAfter = &p
		}
	}
	return out
}

// pitSummary puts the session's stints and stops in its summary, a stop still open
// last, as v1's SessionStore.end_session stores them.
//
// It reads the detector's status on the frame that ends the session, whatever
// ended it. That is deliberate: v1 reads it so at a race end (set_pits before
// end_session on race_ended), and contracts' fuel_after is the level "now
// while it is still in the pit lane". When a start, a restart or the end of the
// frames closes the session with the car in the pit lane, v1 stored the open
// stop as it stood at its last change (fuel, time at rest and position from
// the entry frame); that is not a value to go back to.
//
// TODO(paddock#76): a session continued after an ecu restart counts on from the
// stops before it, as v1 does.
func pitSummary(summary *eventsv1.SessionSummary, status pit.Status) {
	summary.Stints = status.Stint()
	for _, s := range status.Stops {
		summary.PitStops = append(summary.PitStops, pitStop(s))
	}
}

// secsToMs is v1's seconds rounded to 2 decimals, in milliseconds.
func secsToMs(secs float64) int32 {
	return int32(math.Round(roundN(secs, 2) * 1000))
}

// roundN is Python's round(x, places), as round3 is.
func roundN(x float64, places int) float64 {
	// FormatFloat always gives a number ParseFloat reads.
	r, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', places, 64), 64)
	return r
}
