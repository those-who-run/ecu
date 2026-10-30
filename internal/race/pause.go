package race

import (
	"math"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/salsa"
)

// Pauses (paddock#77): v1's PauseClock (pause.py at v1.0.0) and the pause half of
// Engine.process. GT7 flags most of a pause: its frames are paused for as long as
// the pause menu is up. From the pause menu it can also send a frozen stream
// instead: frames marked in the race that do not change at all, the car stopped in
// mid-air at racing speed (SUGO 2026-10-18 has 1.2 s flagged, then 45 s of
// identical frames at 26.9 km/h until the driver picked Restart). The clock reads a
// frame identical to the ones before it for freezeSecs, with the car moving, as
// paused, before anything else sees the frame: the frame goes out as paused, and
// the start check, the detectors, the rules and the lap store all read it so.
//
// A car standing still can send identical frames (a grid start, the pit box), so
// a frozen frame at a standstill is not a pause: GT7 flags a real pause there
// anyway. The first second of a freeze still reads as driving, as it can only be
// told apart afterwards; the pause is dated from its first frame all the same.
//
// The clock counts per session, on the frames' clock, so a replay gives the same
// totals. A pause never ends a session on its own: the session ends paused, as of
// the pause's start, when the feed is lost during the pause (Machine.Tick, feed.go),
// the frames stop in it (Machine.EndOfInput, Machine.Stop), or a pause limit
// passes (Machine.SetPauseLimit), and the frames after it are ignored until the
// next start.

const (
	freezeSecs   = 1.0 // identical frames for this long, with the car moving, are a pause
	freezeMinKph = 1.0 // below this, identical frames are a car standing still
)

// still is what changes on every frame while the car is driven (v1's _still): the
// phase, the lap total, the flags and the position can change with the pause menu
// itself.
type still struct {
	pos, vel         salsa.Vec3
	speed, rpm, fuel float32
	throttle, brake  uint8
	lap              int16
	rot, angVel      salsa.Vec3
}

func stillOf(p *salsa.Packet) still {
	return still{p.Position, p.Velocity, p.Speed, p.EngineRPM, p.FuelLevel, p.Throttle, p.Brake,
		p.CurrentLap, p.Rotation, p.AngularVelocity}
}

// pauses is v1's PauseClock, and the pause the session's events are about.
type pauses struct {
	key    still
	seen   bool      // a frame was seen: key holds
	runT   float64   // the first frame of the current run of identical frames
	runAt  time.Time // ... and when ecu received it
	secs   float64   // the session's pauses that ended, in seconds (v1's paused_secs)
	open   bool      // a pause is in progress (v1's since is not None)
	sinceT float64   // when it began, on the frames' clock (v1's since)
	since  time.Time // ... and when ecu received that frame
	frozen bool      // it began as a frozen stream, not flagged

	count     int32 // the session's pauses published, the <pause> of the last
	published bool  // session.paused went out for the pause in progress
	limit     float64
}

// observe is v1's PauseClock.observe: s as paused when it is a frozen frame, and
// the clock moved on. It says whether a pause began or ended on s.
func (c *pauses) observe(s *state, p *salsa.Packet, at time.Time) (began, ended bool) {
	if k := stillOf(p); !c.seen || k != c.key {
		c.key, c.seen, c.runT, c.runAt = k, true, s.t, at
	}
	frozen := s.phase == phaseInRace && s.speedKph > freezeMinKph && s.t-c.runT >= freezeSecs
	if frozen {
		s.phase = phasePaused
	}
	switch {
	case s.phase == phasePaused && !c.open:
		c.open, c.frozen = true, frozen
		c.sinceT, c.since = s.t, at
		if frozen {
			c.sinceT, c.since = c.runT, c.runAt
		}
		return true, false
	case s.phase != phasePaused && c.open:
		c.secs += max(s.t-c.sinceT, 0)
		c.open = false
		return false, true
	}
	return false, false
}

// reset is v1's PauseClock.reset, at a session's start: no pause counted yet. A
// pause in progress stays one.
func (c *pauses) reset() {
	c.secs, c.count, c.published = 0, 0, false
}

// pausedFor is v1's paused_for: the session's time paused up to t, the pause in
// progress included, in seconds.
func (c *pauses) pausedFor(t float64) float64 {
	if !c.open {
		return c.secs
	}
	return c.secs + max(t-c.sinceT, 0)
}

func (c *pauses) kind() eventsv1.PauseKind {
	if c.frozen {
		return eventsv1.PauseKind_PAUSE_KIND_FROZEN
	}
	return eventsv1.PauseKind_PAUSE_KIND_FLAGGED
}

// pausedMillis is v1's round(secs * 1000): to the nearest millisecond, ties to even.
func pausedMillis(secs float64) int32 {
	return int32(math.RoundToEven(secs * 1000)) //nolint:gosec // a session's time fits
}

// SetPauseLimit ends a session paused once a pause has lasted secs, on the
// frames' clock (v1's pause_limit_secs, TWR_ECU_PAUSE_LIMIT_SECS): 0, the default,
// is no limit, so a pause never ends a session on its own.
func (m *Machine) SetPauseLimit(secs float64) { m.pauses.limit = max(secs, 0) }

// pauseEvents publishes the start or the end of a pause in the open session, on
// the frame it was read on: session.paused for a pause that began with the session
// open, and session.resumed for one session.paused went out for.
func (m *Machine) pauseEvents(s *state, t time.Time, began, ended bool) {
	c := &m.pauses
	sess := m.session
	switch {
	case began && sess != nil:
		c.count++
		c.published = true
		m.out = append(m.out, Output{
			Event:   m.event(sess.id+":session.paused:"+strconv.Itoa(int(c.count)), t),
			Session: sess.id,
			Paused: &eventsv1.SessionPaused{
				SessionId: sess.id, Kind: c.kind(), SinceT: c.sinceT, Since: timestamppb.New(c.since),
				PausedMs: pausedMillis(c.secs), At: s.seq, Pause: c.count,
			},
		})
	case ended && c.published && sess != nil:
		c.published = false
		m.out = append(m.out, Output{
			Event:   m.event(sess.id+":session.resumed:"+strconv.Itoa(int(c.count)), t),
			Session: sess.id,
			Resumed: &eventsv1.SessionResumed{
				SessionId: sess.id, Kind: c.kind(), SinceT: c.sinceT, Since: timestamppb.New(c.since),
				PauseMs: pausedMillis(s.t - c.sinceT), PausedMs: pausedMillis(c.secs), At: s.seq, Pause: c.count,
			},
		})
	}
}

// pauseLimitPassed is v1's check before Engine._step: the pause in progress has
// lasted the limit, with a session open that has been driven.
func (m *Machine) pauseLimitPassed(s *state) bool {
	c := &m.pauses
	return c.limit > 0 && c.open && s.t-c.sinceT >= c.limit && m.session != nil && m.lastDriving != nil
}

// endPaused is v1's Engine._end_paused: the session ends on s as of the start of
// its pause, and the frames are ignored until the next start, so a race that
// carries on after it is not watched.
func (m *Machine) endPaused(s *state, t time.Time, why Why) {
	m.endedPaused = true // before the end: its summary says so
	m.end(s, t, why)
	m.lastDriving = nil
}

// ignore is a frame the machine does not step, after a session ended paused (v1's
// ended_paused): only the frame goes out, with telemetry.restored when it ends a
// loss, and the rules are told only the rules that watch the feed run. The
// context keeps what the step set before (Restored).
func (m *Machine) ignore(s *state, t time.Time) []Output {
	m.ctx.Run, m.ctx.Time, m.ctx.Cur, m.ctx.Ignored = m.run, t, s.view(), true
	m.prev, m.prevTime = s, t
	return m.out
}
