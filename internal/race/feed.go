package race

import (
	"math"
	"strconv"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// The telemetry watchdog (paddock#87): v1's App._watchdog, which read whether a
// packet had come within GT7_TELEMETRY_TIMEOUT_SECS (3 s) once a second, logged
// "telemetry lost" and "telemetry connected", and told the engine of a loss
// (Engine.feed_lost), at v1.0.0.
//
// Frames are what is missing when the feed stops, so no Step can see it. The
// machine keeps no clock: the caller asks LostAt when the feed counts as lost
// if nothing comes, waits for a packet no later than that, and calls Tick with
// the time when none came. Live does (cmd/ecu); a replay never watches, as v1's
// replay had no watchdog, so its end of stream is never a loss.
//
// A loss is telemetry.lost whatever the phase (the telemetry rule decides what is
// said, paddock#98); the first frame after it is telemetry.restored, with the
// gap. Lost in a pause with a session open, the session ends paused, with
// END_REASON_TELEMETRY_LOST (the PS5 switched off or put to rest,
// those-who-run/those-who-run#229), and the frames are ignored until the next
// start; lost while driving, it stays open, as the feed may come back.

// feed is the watchdog's state.
type feed struct {
	timeout time.Duration           // 0: not watching
	lost    *eventsv1.TelemetryLost // the loss in progress, nil while connected
}

// WatchTelemetry makes the machine watch the feed, with timeout the time with no
// packet that counts as a loss (v1's GT7_TELEMETRY_TIMEOUT_SECS); 0 stops it. A
// machine that is not told to never reports a loss.
func (m *Machine) WatchTelemetry(timeout time.Duration) { m.feed.timeout = max(timeout, 0) }

// LostAt is when the feed counts as lost if no packet comes before then: the last
// frame's receive time plus the timeout. ok is false while the machine does not
// watch, before the first frame (the first connection is not a restore), and
// while a loss is in progress.
func (m *Machine) LostAt() (at time.Time, ok bool) {
	if m.feed.timeout <= 0 || m.prev == nil || m.feed.lost != nil {
		return time.Time{}, false
	}
	return m.prevTime.Add(m.feed.timeout), true
}

// Tick is the watchdog's check with the time now, on the clock the frames' receive
// times are on. Once now is at or past LostAt, it returns telemetry.lost, and the
// session's end when the loss came in a pause, then what the rules (Rules) make
// of a Context with Tick set; otherwise it returns nothing and leaves the
// Context as it was.
func (m *Machine) Tick(now time.Time) []Output {
	due, ok := m.LostAt()
	if !ok || now.Before(due) {
		return nil
	}
	m.now = now
	prev := m.prev
	seq := strconv.FormatUint(uint64(prev.seq), 10)
	lost := &eventsv1.TelemetryLost{
		RunId: m.run, Phase: prev.phase, SilenceMs: millis(now.Sub(m.prevTime)), At: prev.seq,
	}
	m.feed.lost = lost
	sid, id := "", m.run+":telemetry.lost:"+seq
	if m.session != nil {
		sid = m.session.id
		lost.SessionId, id = sid, sid+":telemetry.lost:"+m.run+":"+seq
	}
	m.out = []Output{{Event: m.event(id, now), Session: sid, TelemetryLost: lost}}
	m.tickContext(prev, now)
	// v1's Engine.feed_lost: in a pause, flagged or frozen (pause.go), with a
	// session open and driven, the session ends paused on its last frame.
	if m.session != nil && m.pauses.open && m.lastDriving != nil {
		m.endPaused(prev, m.prevTime, WhyTelemetryLost)
	}
	// The rules that watch the feed hear of it (Rules), but not while the start
	// check holds a start or ignores a replay or a demo, whose frames they have
	// not seen either (start.go, paddock#74).
	if !m.check.quiet() {
		m.runRules()
	}
	return m.out
}

// tickContext is the rules' Context for a tick: no frame, so Cur is the last one,
// which the rules have seen already, and Time is the tick's.
func (m *Machine) tickContext(prev *state, now time.Time) {
	m.ctx = Context{Run: m.run, Time: now, Cur: prev.view(), Tick: true, Lost: m.feed.lost}
	c := &m.ctx
	kind := m.kinds.verdict()
	c.Kind = Kind{Type: kind.kind, Field: kind.field, CarsOnTrack: kind.carsOnTrack}
	c.Finished = m.finish.kind != eventsv1.FinishKind_FINISH_KIND_UNSPECIFIED
	if sess := m.session; sess != nil {
		c.Session, c.Track, c.Pits, c.timed = sess.id, sess.track, sess.laps.pits, sess.laps.timed
	}
}

// restore is telemetry.restored on s, the first frame after a loss, with the gap
// from the last frame before it, by their receive times. It goes first after the
// frame, on the session open before s.
func (m *Machine) restore(s *state, t time.Time) {
	lost := m.feed.lost
	if lost == nil {
		return
	}
	m.feed.lost = nil
	seq := strconv.FormatUint(uint64(s.seq), 10)
	restored := &eventsv1.TelemetryRestored{
		RunId: m.run, LostAt: lost.GetAt(), GapMs: millis(t.Sub(m.prevTime)), At: s.seq, LostPhase: lost.GetPhase(),
	}
	sid, id := "", m.run+":telemetry.restored:"+seq
	if m.session != nil {
		sid = m.session.id
		restored.SessionId, id = sid, sid+":telemetry.restored:"+m.run+":"+seq
	}
	m.out = append(m.out, Output{Event: m.event(id, t), Session: sid, TelemetryRestored: restored})
	m.ctx.Restored = restored
}

// millis is d in whole milliseconds, held to an int32.
func millis(d time.Duration) int32 {
	return int32(min(max(d.Milliseconds(), 0), math.MaxInt32))
}
