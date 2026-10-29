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
// reset on each start, as v1's is; its entries and exits are the session's, and
// each is a pit.entered or a pit.exited.

// pitCounts are the session's pit counts, which the events' ids are built from
// (contracts v0.2.2): every entry, withdrawn or not, for pit.entered's <entry>,
// and the stops completed, for pit.exited's <stop>. They are the session's, not
// the detector's, so a session continued after an ecu restart can carry them on
// (TODO(paddock#76): start them from the stops before the restart).
type pitCounts struct {
	entries int32 // every entry, the open one included
	stops   int32 // the stops completed
	// The open entry's number and its PitEntered's times, for its PitExited.
	entry     int32
	enteredT  float64
	enteredAt time.Time
}

// observePits feeds the detector the frame f, received at t, and publishes an
// entry or an exit while a session is open. A frame changes the pits at most
// once, so it gives at most one pit event.
func (m *Machine) observePits(f *telemetryv1.Frame, t time.Time) {
	change := m.pits.Observe(f)
	sess := m.session
	if change == pit.Unchanged || sess == nil {
		return
	}
	status := m.pits.Status()
	pits := &sess.pits
	switch change {
	case pit.Entered:
		stop := status.Stops[len(status.Stops)-1]
		pits.entries++
		pits.entry, pits.enteredT, pits.enteredAt = pits.entries, stop.EntryT, t
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
		// TODO(paddock#79): publish pit.teleport, which withdraws the entry. The
		// entry keeps its number and the stop number goes back, as v1's does.
	}
}

// pitStop is the stop as the contracts' PitStop, as v1's PitStop.record() writes
// it: fuel to 3 decimals, times in seconds to 2 decimals (here in ms), the
// limiter's speed to 1. A stop still open has no out-lap, lane time or position
// after (contracts v0.2.2), where v1 recorded its last position.
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
