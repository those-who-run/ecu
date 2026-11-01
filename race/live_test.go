package race

import (
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// paddock#88: the live view follows the session, its pits and its last frame,
// and copies it out without allocating, after every step.
func TestTheLiveView(t *testing.T) {
	m := New(testIDs())
	if l := m.Live(); l.Run != m.Run() || l.Packets != 0 || l.Phase != 0 || l.Session.ID != "" {
		t.Fatalf("before a frame: %+v", l)
	}
	var open string
	var inLane, sawTrack bool
	var stops int32
	for _, r := range packets(t, "real-nuc-sugo-two-stops-2026-10-18") {
		for _, o := range m.Step(r) {
			switch {
			case o.Started != nil:
				open = o.Session
			case o.Ended != nil:
				open = ""
			case o.PitEntered != nil:
				inLane = true
			case o.PitExited != nil:
				inLane = false
				stops++
			}
		}
		l := m.Live()
		if l.Packets != m.seq || l.Phase != m.prev.phase || l.Session.ID != open {
			t.Fatalf("frame %d: %+v, open %q", m.seq, l, open)
		}
		s := l.Session
		if open == "" {
			continue
		}
		if s.InPitLane != inLane || s.Stops != stops || s.Stint != stops+1 {
			t.Fatalf("frame %d: pits %+v, want in the lane %v after %d stops", m.seq, s, inLane, stops)
		}
		if !s.HasFrame || s.Lap < 1 || s.TotalLaps <= 0 || s.FuelCapacity <= 0 || s.CarCode == 0 {
			t.Fatalf("frame %d: %+v", m.seq, s)
		}
		if s.TrackName != "" {
			sawTrack = true
		}
	}
	if stops != 2 || !sawTrack {
		t.Errorf("%d stops, a track %v", stops, sawTrack)
	}
	if n := testing.AllocsPerRun(100, func() { _ = m.Live() }); n != 0 {
		t.Errorf("Live allocates %v times", n)
	}
}

// A GT7 replay is ignored once the start check knows it: the view says so.
func TestTheLiveViewOfAReplay(t *testing.T) {
	m := New(testIDs())
	discarded := false
	for _, r := range packets(t, "real-nuc-sugo-replay-2026-10-18") {
		for _, o := range m.Step(r) {
			discarded = discarded || o.Discarded != nil
		}
		if l := m.Live(); l.Session.ID != "" {
			t.Fatalf("frame %d: a session", m.seq)
		}
	}
	if !discarded || m.Live().Ignoring != eventsv1.DiscardReason_DISCARD_REASON_REPLAY {
		t.Errorf("discarded %v, ignoring %v", discarded, m.Live().Ignoring)
	}
}
