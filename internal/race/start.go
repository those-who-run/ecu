package race

import (
	"fmt"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// The start check (paddock#74): v1's DriverCheck, from sessionkind.py at v1.0.0,
// and the hold around it in Engine.process. GT7 sends driving frames for its own
// race replays and for the demo race it plays when the PS5 sits idle on the menu.
// Every drive has GT7's cars-on-track flag set from its first driving frame, so a
// start with the flag off is held until the check knows what it is:
//
//   - a drive once the flag comes on;
//   - a replay once the fuel drops (a demo keeps it flat at 100);
//   - otherwise, when the window passes or the session pauses or ends first, a
//     demo if no loading screen came since the last menu (a drive always loads
//     its track first), or if no menu was seen and GT7 reports a field while its
//     flag says there are no cars (a demo joined part way); a drive otherwise.
//
// While the start is held the machine gives each frame and nothing else: no
// event, and the rules are not run. A drive's held frames then go through the
// race state as if never held, so its session starts on the first of them. A
// replay or a demo is session.discarded, and the frames after it are ignored
// until the next start, as v1 ignored them.

// v1's DriverCheck thresholds.
const (
	// startFuelDrop is how far the fuel must fall below the highest level seen in
	// the hold for a replay, in litres.
	startFuelDrop = 0.05
	// startWindowSecs and startWindowPackets bound the hold: longer than any drive
	// with fuel on takes to show a drop.
	startWindowSecs    = 3.0
	startWindowPackets = 180
	// startFieldCars is the field GT7 reports in a demo joined part way.
	startFieldCars = 2
)

// verdictKind is what a held start was (v1's Verdict); verdictNone while the
// check is still out.
type verdictKind int

const (
	verdictNone verdictKind = iota
	verdictDrive
	verdictReplay
	verdictDemo
)

// driverCheck is v1's DriverCheck: see on every frame, start with the held start's
// first frame, then observe each later frame until it gives a verdict; why then
// says what decided it.
type driverCheck struct {
	menuSeen bool // a menu has been seen (v1's loaded is None until one is)
	loaded   bool // a loading screen came since the last menu
	first    *state
	topFuel  float64
	cars     int32
	why      string
}

func (c *driverCheck) see(s *state) {
	switch {
	case s.phase == phaseMenu:
		c.menuSeen, c.loaded = true, false
	case s.phase == phaseLoading && c.menuSeen:
		c.loaded = true
	}
}

func (c *driverCheck) start(s *state) {
	c.first, c.topFuel, c.cars, c.why = s, s.fuel, s.totalCars, ""
}

func (c *driverCheck) observe(s *state) verdictKind {
	first := c.first
	if s.phase != phaseInRace {
		return c.settle() // a pause or an end inside the window
	}
	if s.carsOnTrack {
		return verdictDrive // nothing a replay or a demo does
	}
	c.cars = max(c.cars, s.totalCars)
	c.topFuel = max(c.topFuel, s.fuel)
	if s.fuelCapacity > 0 && s.fuel < c.topFuel-startFuelDrop {
		c.why = fmt.Sprintf("fuel is draining (%.1f -> %.1f), which only a replay does", first.fuel, s.fuel)
		return verdictReplay
	}
	packets := int64(s.packetID) - int64(first.packetID)
	if s.t-first.t >= startWindowSecs || packets < 0 || packets >= startWindowPackets {
		return c.settle()
	}
	return verdictNone
}

func (c *driverCheck) settle() verdictKind {
	if c.menuSeen && !c.loaded {
		c.why = "it came straight from the menu with no loading screen"
		return verdictDemo
	}
	if !c.menuSeen && c.cars >= startFieldCars {
		c.why = fmt.Sprintf("it was joined part way with no menu seen and GT7 reports %d cars", c.cars)
		return verdictDemo
	}
	return verdictDrive
}

// startCheck is the machine's hold: the check, the frames held for it, and what
// a replay or a demo leaves ignored.
type startCheck struct {
	driverCheck
	held       []*state
	before     *state // the frame before the held start (v1's _held_prev)
	beforeTime time.Time
	id         string // the id minted for the held start: its session's, or its discard's
	ignoring   eventsv1.DiscardReason
}

// hold is v1's Engine.process before _step: it holds a start GT7's flag is off
// for, gives the verdict on it, and ignores the frames after a replay or a demo.
// It says whether it took the frame; when it did not, the frame goes through the
// race state. A drive's held frames go through the race state here, in order.
func (m *Machine) hold(s *state) bool {
	c := &m.check
	if len(c.held) > 0 {
		c.held = append(c.held, s)
		m.prev, m.prevTime = s, s.received
		v := c.observe(s)
		c.see(s) // after: a menu that ends the start is no news of it
		if v == verdictNone {
			return true
		}
		held := c.held
		c.held = nil
		if v == verdictDrive {
			m.prev, m.prevTime = c.before, c.beforeTime
			for _, h := range held {
				m.ctx = Context{}
				m.step(h)
			}
			return true
		}
		m.discard(held, v)
		return true
	}
	c.see(s)
	restarted := raceRestarted(m.prev, s)
	starting := raceStarted(m.prev, s) || restarted
	if starting && !s.carsOnTrack {
		c.held, c.before, c.beforeTime, c.id = []*state{s}, m.prev, m.prevTime, m.ids().String()
		// A new start ends what a replay or a demo left ignored: a drive after the
		// hold then runs normally, and another replay or demo sets it again. v1.0.0
		// cleared it only on a frame its hold didn't take, so it dropped the rest
		// of a held drive after a replay or a demo (the maintainer's decision,
		// 2026-10-30, paddock#74).
		c.ignoring = eventsv1.DiscardReason_DISCARD_REASON_UNSPECIFIED
		c.start(s)
		m.prev, m.prevTime = s, s.received
		return true
	}
	if c.ignoring != eventsv1.DiscardReason_DISCARD_REASON_UNSPECIFIED && !starting {
		m.prev, m.prevTime = s, s.received
		return true
	}
	c.ignoring = eventsv1.DiscardReason_DISCARD_REASON_UNSPECIFIED
	return false
}

// discard ends a session still open on the frame before the held start, as v1's
// "closing open session on replay start", and gives session.discarded for the
// held frames.
func (m *Machine) discard(held []*state, v verdictKind) {
	c := &m.check
	if m.session != nil && c.before != nil {
		why := WhyStart
		if raceRestarted(c.before, held[0]) {
			why = WhyRestart
		}
		m.end(c.before, c.beforeTime, why)
	}
	m.lastDriving = nil
	reason := eventsv1.DiscardReason_DISCARD_REASON_REPLAY
	if v == verdictDemo {
		reason = eventsv1.DiscardReason_DISCARD_REASON_DEMO
	}
	c.ignoring = reason
	last := held[len(held)-1]
	m.out = append(m.out, Output{
		Event:   m.event(c.id+":session.discarded:1", last.received),
		Session: c.id,
		Discarded: &eventsv1.SessionDiscarded{
			SessionId: c.id, Reason: reason, Evidence: c.why,
			FirstSeq: held[0].seq, At: last.seq,
		},
	})
	c.id = ""
}

// sessionID is the id for a session starting now: the one minted for its held
// start, else a new one.
func (m *Machine) sessionID() string {
	if id := m.check.id; id != "" {
		m.check.id = ""
		return id
	}
	return m.ids().String()
}
