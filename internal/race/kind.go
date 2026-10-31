package race

import (
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// kinds is v1's SessionDetector (src/those_who_run/sessionkind.py at v1.0.0),
// ported as it is (paddock#82). GT7 sends no mode, so the kind comes from the lap
// total and the field of cars:
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
// Fuel use is on once the level drops fuelDrop below the highest seen while
// racing (a refuel raises the mark), off after fuelSettle of driving at moving
// speed without a drop, or at once with no tank. The start is read from the
// fastest the car goes in the first startWindow packets of racing: under gridKph
// is a grid start, else rolling in a lap race and flying otherwise. It counts
// GT7's packets, not the clock, because packets arrive in bursts (a rolling start
// at the Nordschleife, 2026-10-15, read through one).
//
// total_cars is -1 in the frames GT7 replays around the results screen and for the
// whole of a time trial, so -1 says nothing about the field. GT7 can also post the
// lap total a moment after the start, so the verdict is kept up to date: every frame
// is fed, and each change of the decision is published (session.kind).
//
// The zero value is the detector reset (v1's SessionDetector.reset).
type kinds struct {
	valid       bool // a racing frame has been seen
	laps        int32
	cars        int32
	field       bool
	carsOnTrack bool

	fuelKnown, fuelOn bool
	topFuel           float64
	haveTop           bool
	lastT             float64
	haveLastT         bool // false: the next racing frame starts a new step
	drivingSecs       float64

	startPacket int32
	haveStart   bool
	startKph    float64
	startKnown  bool

	joined bool
	last   kindVerdict // the verdict so far (v1's SessionDetector._kind)
}

// v1's SessionDetector defaults (paddock#82, Interfaces).
const (
	fuelSettle  = 20.0 // seconds of driving without a drop: fuel use off
	fuelDrop    = 0.05 // litres below the highest level: fuel use on
	movingKph   = 20.0
	maxStepSecs = 1.0 // a pause or a gap is not driving time
	startWindow = 18  // packets, 0.3 s at 60 Hz
	gridKph     = 30.0
)

// frameField says one frame shows a field, as the kind reads it: three or more
// cars, a driver who is anything but first, or a lap total and a second car.
// Two cars alone with no lap total are a time trial's ghost.
func frameField(laps, cars, position int32) bool {
	return cars >= 3 || position > 1 || (laps > 0 && cars >= 2)
}

// kindVerdict is v1's SessionKind: the decision, and the evidence for it.
type kindVerdict struct {
	kind        eventsv1.SessionType // 0 before any verdict
	totalLaps   int32
	field       bool
	fuelUse     eventsv1.FuelUse
	carsOnTrack bool
	start       eventsv1.StartType
	evidence    string
}

// unknownKind is v1's UNKNOWN: no racing frame yet.
var unknownKind = kindVerdict{
	kind:     eventsv1.SessionType_SESSION_TYPE_UNKNOWN,
	fuelUse:  eventsv1.FuelUse_FUEL_USE_UNKNOWN,
	start:    eventsv1.StartType_START_TYPE_UNKNOWN,
	evidence: "no race frame yet",
}

// sameDecision says v and w are the same decision (v1's SessionKind.decision): the
// kind, fuel use, cars-on-track flag and start. The evidence's wording is not.
func (v kindVerdict) sameDecision(w kindVerdict) bool {
	return v.kind == w.kind && v.fuelUse == w.fuelUse && v.carsOnTrack == w.carsOnTrack && v.start == w.start
}

// message is the verdict as the contracts' SessionKind, as v1's SessionKind.record
// stores it. A timed session's lap total is absent, where v1 stored 0.
func (v kindVerdict) message() *eventsv1.SessionKind {
	k := &eventsv1.SessionKind{
		SessionType: v.kind,
		Field:       v.field,
		FuelUse:     v.fuelUse,
		CarsOnTrack: v.carsOnTrack,
		Start:       v.start,
		Evidence:    v.evidence,
	}
	if v.totalLaps > 0 {
		laps := v.totalLaps
		k.TotalLaps = &laps
	}
	return k
}

// join says the session was joined part way through (v1's SessionDetector.join,
// paddock#75): its start was not seen, so the start stays unknown. Call after a
// reset.
func (k *kinds) join() { k.joined = true }

// observe feeds one frame (SessionDetector.observe) and says whether the decision
// changed. Only racing frames count.
func (k *kinds) observe(s *state) bool {
	if !s.inRace() {
		k.haveLastT = false
		return false
	}
	k.valid = true
	if s.totalCars >= 0 {
		k.cars = s.totalCars
		k.carsOnTrack = s.carsOnTrack
		if s.totalLaps > 0 {
			k.laps = s.totalLaps
		}
		if k.cars >= 3 || s.racePosition() > 1 {
			k.field = true
		}
	}
	k.observeFuel(s)
	k.observeStart(s)
	v := k.decide()
	changed := !v.sameDecision(k.verdict())
	k.last = v
	return changed
}

func (k *kinds) observeFuel(s *state) {
	if k.fuelKnown && k.fuelOn {
		return
	}
	if s.fuelCapacity <= 0 {
		k.fuelKnown, k.fuelOn = true, false
		return
	}
	if !k.haveTop || s.fuel > k.topFuel {
		k.topFuel, k.haveTop = s.fuel, true
	}
	if s.fuel < k.topFuel-fuelDrop {
		k.fuelKnown, k.fuelOn = true, true
		return
	}
	if k.haveLastT && s.speedKph >= movingKph {
		k.drivingSecs += min(s.t-k.lastT, maxStepSecs)
	}
	k.lastT, k.haveLastT = s.t, true
	if k.drivingSecs >= fuelSettle {
		k.fuelKnown, k.fuelOn = true, false
	}
}

func (k *kinds) observeStart(s *state) {
	if k.startKnown {
		return
	}
	if !k.haveStart {
		k.startPacket, k.haveStart = s.packetID, true
	}
	k.startKph = max(k.startKph, s.speedKph)
	packets := s.packetID - k.startPacket
	if packets >= startWindow || packets < 0 { // < 0: GT7's counter started over
		k.startKnown = true
	}
}

// decide is SessionDetector._decide, with v1's evidence word for word.
func (k *kinds) decide() kindVerdict {
	if !k.valid {
		return unknownKind
	}
	v := kindVerdict{totalLaps: k.laps, carsOnTrack: k.carsOnTrack, start: eventsv1.StartType_START_TYPE_UNKNOWN}
	cars := strconv.Itoa(int(k.cars))
	var why string
	if k.laps > 0 {
		v.kind = eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS
		if k.cars >= 2 || k.field {
			v.kind = eventsv1.SessionType_SESSION_TYPE_LAP_RACE
		}
		why = strconv.Itoa(int(k.laps)) + " laps, " + cars + " cars"
	} else {
		v.kind = eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED
		if k.field {
			v.kind = eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD
		}
		why = "no lap total, " + cars + " cars"
		if k.field && k.cars < 3 {
			why += ", not always first"
		}
	}
	switch {
	case !k.fuelKnown:
		v.fuelUse = eventsv1.FuelUse_FUEL_USE_UNKNOWN
		why += "; fuel use not known yet"
	case k.fuelOn:
		v.fuelUse = eventsv1.FuelUse_FUEL_USE_ON
		why += "; fuel use on"
	default:
		v.fuelUse = eventsv1.FuelUse_FUEL_USE_OFF
		why += "; fuel use off"
	}
	switch {
	case k.joined:
		why += "; joined part way, start not seen"
	case k.startKnown:
		var name string
		switch {
		case k.startKph < gridKph:
			v.start, name = eventsv1.StartType_START_TYPE_GRID, "grid"
		case v.kind == eventsv1.SessionType_SESSION_TYPE_LAP_RACE:
			v.start, name = eventsv1.StartType_START_TYPE_ROLLING, "rolling"
		default:
			v.start, name = eventsv1.StartType_START_TYPE_FLYING, "flying"
		}
		why += "; " + name + " start at " + strconv.FormatFloat(k.startKph, 'f', 0, 64) + " km/h"
	}
	v.field = v.kind == eventsv1.SessionType_SESSION_TYPE_LAP_RACE || v.kind == eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD
	v.evidence = why
	return v
}

// verdict is the verdict so far (SessionDetector.kind).
func (k *kinds) verdict() kindVerdict {
	if k.last.kind == eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED {
		return unknownKind
	}
	return k.last
}

// kind is the verdict's kind, as the contracts' SessionType: one value for each
// of v1's kinds.
func (k *kinds) kind() eventsv1.SessionType { return k.verdict().kind }

// applyKind publishes the verdict as the open session's session.kind, on the frame
// s it changed on (v1's Engine._apply_kind), and keeps it for the summary. The
// detector changes its decision at most once a frame, so a frame publishes at most
// one session.kind for a session: its id is unique.
func (m *Machine) applyKind(s *state, t time.Time) {
	sess := m.session
	v := m.kinds.verdict()
	sess.kind = &v
	kind := v.message()
	// Continued after an ecu restart (paddock#76): the start was seen before it,
	// so a verdict of the same kind keeps it, as v1's set_kind did.
	if before := sess.kindBefore; before != nil && before.GetSessionType() == v.kind &&
		before.GetStart() != eventsv1.StartType_START_TYPE_UNSPECIFIED {
		kind.Start = before.GetStart()
		kind.Evidence += "; continued after an engineer restart"
	}
	sess.summary.Kind, sess.summary.SessionType = kind, v.kind
	seq := strconv.FormatUint(uint64(s.seq), 10)
	m.out = append(m.out, Output{
		Event:   m.event(sess.id+":session.kind:"+m.run+":"+seq, t),
		Session: sess.id,
		Kind: &eventsv1.SessionKindChanged{
			SessionId: sess.id, Kind: proto.CloneOf(kind), At: s.seq, RunId: m.run,
		},
	})
}

// solo says a session of type t has no field, so a position means nothing: GT7
// reports 1 there. LapCompleted.position and SessionSummary.final_position are
// absent for it (contracts v0.1.4), a deliberate difference from v1, which stored
// the 1.
func solo(t eventsv1.SessionType) bool {
	return t == eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS || t == eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED
}
