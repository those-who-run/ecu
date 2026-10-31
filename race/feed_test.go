package race

import (
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/salsa"
)

// watched steps the frames through a machine that watches the feed with v1's 3 s.
func watched(t *testing.T, all []Received) (*Machine, []Output) {
	t.Helper()
	m := New(testIDs())
	m.WatchTelemetry(3 * time.Second)
	var out []Output
	for _, r := range all {
		out = append(out, m.Step(r)...)
	}
	return m, out
}

// later is r received after a gap: d after the frame before it, on both clocks.
func later(r Received, prev Received, d time.Duration) Received {
	r.T = prev.T + d.Seconds()
	r.Time = prev.Time.Add(d)
	return r
}

func sessionOf(out []Output) string {
	for _, o := range out {
		if o.Started != nil {
			return o.Session
		}
	}
	return ""
}

// AC1: the feed stops mid-race for 5 s: one telemetry.lost 3 s after the last
// packet, on the session's subject, the session left open, and one
// telemetry.restored on the first packet after the gap, with the gap.
func TestALossMidRaceIsLostThenRestoredAndTheSessionStaysOpen(t *testing.T) {
	var d drive
	d.add(0, 0, 50, -1, 8) // 1: the grid
	d.add(1, 0, 50, -1, 8) // 2: the start
	d.add(1, 0, 49, -1, 7) // 3: the last packet before the gap
	m, out := watched(t, d.all)
	sid, last := sessionOf(out), d.all[2]

	due, ok := m.LostAt()
	if !ok || !due.Equal(last.Time.Add(3*time.Second)) {
		t.Fatalf("lost at %v %v, want 3 s after the last packet", due, ok)
	}
	if got := m.Tick(due.Add(-time.Millisecond)); got != nil {
		t.Fatalf("lost before the timeout: %v", got)
	}
	lost := m.Tick(due)
	if len(lost) != 1 || lost[0].TelemetryLost == nil {
		t.Fatalf("on the timeout %v, want telemetry.lost alone", lost)
	}
	l := lost[0]
	if l.Session != sid || l.Event.ID != sid+":telemetry.lost:"+m.Run()+":3" || !l.Event.Time.Equal(due) ||
		l.TelemetryLost.GetSessionId() != sid || l.TelemetryLost.GetRunId() != m.Run() || l.TelemetryLost.GetAt() != 3 ||
		l.TelemetryLost.GetPhase() != telemetryv1.Phase_PHASE_IN_RACE || l.TelemetryLost.GetSilenceMs() != 3000 {
		t.Errorf("lost %v %v", l.Event, l.TelemetryLost)
	}
	if c := m.Context(); !c.Tick || c.Lost != l.TelemetryLost || c.Session != sid || c.Cur.Seq != 3 || !c.Time.Equal(due) {
		t.Errorf("the rules' context on the tick: %+v", c)
	}
	if got := m.Tick(due.Add(time.Hour)); got != nil {
		t.Errorf("lost twice: %v", got)
	}
	if _, ok := m.LostAt(); ok {
		t.Error("LostAt while lost")
	}

	var back drive
	back.add(1, 0, 49, -1, 7)
	back.add(2, 0, 47, 61000, 7)
	resumed := later(back.all[0], last, 5*time.Second)
	out = m.Step(resumed)
	if len(out) < 2 || out[1].TelemetryRestored == nil {
		t.Fatalf("on resuming %v, want the frame then telemetry.restored", out)
	}
	r := out[1]
	if r.Session != sid || r.Event.ID != sid+":telemetry.restored:"+m.Run()+":4" || !r.Event.Time.Equal(resumed.Time) ||
		r.TelemetryRestored.GetLostAt() != 3 || r.TelemetryRestored.GetAt() != 4 ||
		r.TelemetryRestored.GetGapMs() != 5000 || r.TelemetryRestored.GetLostPhase() != telemetryv1.Phase_PHASE_IN_RACE {
		t.Errorf("restored %v %v", r.Event, r.TelemetryRestored)
	}
	if c := m.Context(); c.Tick || c.Restored != r.TelemetryRestored || c.Lost != nil {
		t.Errorf("the rules' context on resuming: %+v", c)
	}
	// The session carried on: the lap closes in it.
	out = m.Step(later(back.all[1], resumed, time.Second))
	if evs := events(out); len(evs) != 1 || evs[0].Lap == nil || evs[0].Session != sid {
		t.Errorf("after the gap %v, want lap 1 closed in the session", evs)
	}
	if _, ok := m.LostAt(); !ok {
		t.Error("not watching again after the restore")
	}
}

// AC2: lost in a pause, the session ends paused on its last frame, and nothing but
// a start is read after it, as v1's Engine.feed_lost and ended_paused.
func TestALossInAPauseEndsTheSessionPaused(t *testing.T) {
	var d drive
	d.add(0, 0, 50, -1, 8)                // 1: the grid
	d.add(1, 0, 50, -1, 8)                // 2: the start
	d.add(1, 0, 49, -1, 7)                // 3
	d.add(1, salsa.FlagPaused, 49, -1, 7) // 4: paused, the last packet
	m, out := watched(t, d.all)
	sid := sessionOf(out)
	due, _ := m.LostAt()
	out = m.Tick(due)
	if len(out) != 3 || out[0].TelemetryLost == nil || out[1].Lap == nil || out[2].Ended == nil {
		t.Fatalf("lost in a pause: %v, want telemetry.lost, the lap cut short and session.ended", out)
	}
	if out[0].TelemetryLost.GetPhase() != telemetryv1.Phase_PHASE_PAUSED || out[0].Session != sid {
		t.Errorf("lost %v", out[0].TelemetryLost)
	}
	end := out[2]
	if end.Why != WhyTelemetryLost || end.Ended.GetReason() != eventsv1.EndReason_END_REASON_TELEMETRY_LOST ||
		!end.Ended.GetSummary().GetEndedPaused() || end.Ended.GetLastSeq() != 4 || end.Session != sid ||
		!end.Event.Time.Equal(d.all[3].Time) {
		t.Errorf("end %v %v at %v", end.Why, end.Ended, end.Event.Time)
	}
	if c := m.Context(); c.Session != sid || c.Lost == nil {
		t.Errorf("the rules' context: the session the loss ended, got %+v", c)
	}

	// The PS5 wakes in the paused race and it carries on: ignored, with no
	// session, until the next start.
	var back drive
	back.add(1, salsa.FlagPaused, 49, -1, 7) // 5
	back.add(1, 0, 49, -1, 7)                // 6
	back.add(2, 0, 47, 61000, 7)             // 7: a lap, not seen
	back.add(-1, 0, 47, 61000, -1)           // 8: the menu
	back.add(0, 0, 50, -1, 8)                // 9: the grid
	back.add(1, 0, 50, -1, 8)                // 10: a start
	all := []Received{later(back.all[0], d.all[3], time.Minute)}
	for _, r := range back.all[1:] {
		all = append(all, later(r, all[len(all)-1], time.Second))
	}
	var got []Output
	for i, r := range all {
		o := m.Step(r)
		if i == 0 {
			if len(o) != 2 || o[1].TelemetryRestored == nil || o[1].Session != "" ||
				o[1].Event.ID != m.Run()+":telemetry.restored:5" {
				t.Errorf("resumed %v, want telemetry.restored with no session", o)
			}
			continue
		}
		got = append(got, events(o)...)
	}
	if len(got) != 1 || got[0].Started == nil || got[0].Started.GetFirstSeq() != 10 || got[0].Session == sid {
		t.Errorf("after the loss %v, want only the next start, on frame 10", got)
	}
}

// AC3's half here: silence in a menu is telemetry.lost too, on evt._ as no session
// is open, with the menu's phase for the telemetry rule to stay quiet on (#98).
func TestALossInAMenuIsLostWithNoSession(t *testing.T) {
	var d drive
	d.add(-1, 0, 50, -1, -1) // 1: a menu
	m, _ := watched(t, d.all)
	due, _ := m.LostAt()
	out := m.Tick(due.Add(500 * time.Millisecond))
	if len(out) != 1 || out[0].TelemetryLost == nil || out[0].Session != "" ||
		out[0].Event.ID != m.Run()+":telemetry.lost:1" || out[0].TelemetryLost.GetSilenceMs() != 3500 ||
		out[0].TelemetryLost.GetPhase() != telemetryv1.Phase_PHASE_MENU {
		t.Errorf("lost in a menu: %v", out)
	}
}

// A machine not told to watch (a replay) never loses the feed, and neither does
// one before its first frame: the first connection is not a restore.
func TestNoLossUnwatchedOrBeforeTheFirstFrame(t *testing.T) {
	m := New(testIDs())
	m.WatchTelemetry(3 * time.Second)
	if _, ok := m.LostAt(); ok {
		t.Error("LostAt before the first frame")
	}
	if out := m.Tick(epoch.Add(time.Hour)); out != nil {
		t.Errorf("lost before the first frame: %v", out)
	}
	var d drive
	d.add(1, 0, 50, -1, 8)
	out := m.Step(d.all[0])
	for _, o := range out {
		if o.TelemetryRestored != nil {
			t.Errorf("the first frame is a restore: %v", o)
		}
	}

	unwatched := New(testIDs())
	unwatched.Step(d.all[0])
	if _, ok := unwatched.LostAt(); ok {
		t.Error("LostAt unwatched")
	}
	if out := unwatched.Tick(epoch.Add(time.Hour)); out != nil {
		t.Errorf("lost unwatched: %v", out)
	}
}

func TestMillisHoldsToAnInt32(t *testing.T) {
	if millis(-time.Second) != 0 || millis(1500*time.Millisecond) != 1500 || millis(1000*time.Hour) != 1<<31-1 {
		t.Error("millis")
	}
}
