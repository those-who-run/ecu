package race

import (
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/track"
)

// The live view (paddock#88): what ecu's status, in the `live` KV bucket and on
// /healthz, shows of the race state, as v1's status route read it off its engine
// between frames (v1's status.py, live_session and activity). Live copies it out
// as plain values, with no allocation, so the step loop can hand it to the status
// writer after every step without holding up the frames; the writer, on its own
// goroutine, turns it into the contracts' messages and writes them.

// Live is the race state as of the last step or tick.
type Live struct {
	// Run is the machine's run id.
	Run string
	// Packets is the packets the machine has taken (v1's /healthz `packets`).
	Packets uint32
	// Phase is the last frame's phase, a frozen frame's paused (pause.go), and
	// PHASE_UNSPECIFIED before the first frame.
	Phase telemetryv1.Phase
	// Ignoring is the replay or the demo whose frames are ignored (start.go,
	// v1's Engine.ignoring), DISCARD_REASON_UNSPECIFIED when none is.
	Ignoring eventsv1.DiscardReason
	// Session is the open session; its ID is "" while none is open.
	Session LiveSession
}

// LiveSession is the open session as of its last frame (v1's status.py
// LiveSession, PitStatus and the fuel on the frame). Zero values are none.
type LiveSession struct {
	// ID is the session's id, as in its SessionStarted.
	ID string
	// Type is the kind verdict's (v1's engine.kind), SESSION_TYPE_UNKNOWN until
	// there is one, and PositionShown whether its field and cars_on_track are
	// both set (v1's results.position_shown).
	Type          eventsv1.SessionType
	PositionShown bool
	// TrackName is the session's track, the layout's or the circuit's name, ""
	// before the detector placed one; TrackID is the layout's id, 0 when only
	// the circuit is known; TrackSure is a probable or confirmed verdict.
	TrackID   int32
	TrackName string
	TrackSure bool
	// CarCode is GT7's code for the car on that frame (HasFrame), the one
	// SessionStarted names, and CarName its name from the car table (cars.go,
	// paddock#86): "car <code>" for a car missing from it, "" for none.
	CarCode int32
	CarName string
	// HasFrame says the session has a frame to read Lap and the rest from: the
	// last driving frame, or the last frame before one (v1's
	// `engine.last_driving or engine.prev`).
	HasFrame bool
	// Lap and TotalLaps are GT7's on that frame, TotalLaps 0 in a timed session;
	// Position and TotalCars are too, 0 where GT7 sent none (0 or -1).
	Lap, TotalLaps, Position, TotalCars int32
	// InPitLane, Stint and Stops are the session's pit state: in the pit lane
	// from its pit.entered to its pit.exited or pit.teleport, the stint from 1,
	// and the stops completed.
	InPitLane    bool
	Stint, Stops int32
	FuelLevel    float64 // on the last frame, in litres
	FuelCapacity float64 // on the last frame, in litres: 0 for a car with no tank
}

// Live is the race state as the status shows it, as of the last step or tick.
func (m *Machine) Live() Live {
	l := Live{Run: m.run, Packets: m.seq, Ignoring: m.check.ignoring}
	if m.prev != nil {
		l.Phase = m.prev.phase
	}
	sess := m.session
	if sess == nil {
		return l
	}
	kind := m.kinds.verdict()
	ls := LiveSession{
		ID:            sess.id,
		Type:          kind.kind,
		PositionShown: kind.field && kind.carsOnTrack,
		InPitLane:     sess.laps.pits.InPitLane,
		Stint:         sess.laps.pits.Stint(),
		Stops:         sess.pits.stops,
	}
	if t := sess.track; t != nil && t.Name != "" {
		ls.TrackID, ls.TrackName = t.TrackID, t.Name
		ls.TrackSure = t.Confidence == track.Probable || t.Confidence == track.Confirmed
	}
	frame := m.lastDriving
	if frame == nil {
		frame = m.prev
	}
	if frame != nil {
		ls.HasFrame = true
		ls.Lap, ls.TotalLaps = frame.lap, max(frame.totalLaps, 0)
		ls.Position, ls.TotalCars = frame.racePosition(), max(frame.totalCars, 0)
		ls.CarCode, ls.CarName = frame.carCode, carName(frame.carCode)
	}
	if m.prev != nil {
		ls.FuelLevel, ls.FuelCapacity = m.prev.fuel, m.prev.fuelCapacity
	}
	l.Session = ls
	return l
}
