package status

import (
	"testing"
	"time"

	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/race"
	"github.com/those-who-run/salsa"
)

// paddock#88, AC1: SUGO's two stops through the race state: ecu.status says
// driving through the race and never paused, idle 3 s after the last driving
// frame; ecu.pit says in the pit lane from each pit.entered to its pit.exited;
// ecu.session has the lap and the position, P9 at the end.
func TestSugoTwoStops(t *testing.T) {
	r := newRig(Config{})
	var (
		inLane, sawDriving bool
		entries, exits     int
		last               Values // as of the frame before the session ended
		ended              bool
		lastDriving        time.Time
	)
	for _, f := range recording(t, sugoTwoStops) {
		before := r.t.Values()
		outs := r.step(f)
		v := r.t.Values()
		if driving(r.m.Live()) {
			lastDriving = r.clock.Now()
		}
		for _, o := range outs {
			switch {
			case o.PitEntered != nil:
				inLane = true
				entries++
			case o.PitExited != nil:
				inLane = false
				exits++
			case o.Ended != nil:
				last, ended = before, true
			}
		}
		switch a := v.Status.GetActivity(); a {
		case statev1.Activity_ACTIVITY_DRIVING:
			sawDriving = true
		case statev1.Activity_ACTIVITY_IDLE:
		default:
			t.Fatalf("frame %d: activity %v", v.Status.GetPackets(), a)
		}
		if v.Session == nil {
			if v.Pit != nil || v.Fuel != nil {
				t.Fatalf("frame %d: pit or fuel with no session", v.Status.GetPackets())
			}
			continue
		}
		if v.Pit.GetInPitLane() != inLane {
			t.Fatalf("frame %d: in the pit lane %v, the events say %v", v.Status.GetPackets(), v.Pit.GetInPitLane(), inLane)
		}
		if v.Pit.GetStops() != int32(exits) || v.Pit.GetStint() != int32(exits)+1 {
			t.Fatalf("frame %d: %v after %d exits", v.Status.GetPackets(), v.Pit, exits)
		}
		if v.Fuel.GetSessionId() != v.Session.GetSessionId() || v.Fuel.GetFuelCapacity() <= 0 {
			t.Fatalf("frame %d: fuel %v", v.Status.GetPackets(), v.Fuel)
		}
	}
	if !sawDriving || entries != 2 || exits != 2 || !ended {
		t.Fatalf("driving %v, %d entries, %d exits, ended %v", sawDriving, entries, exits, ended)
	}
	if s := last.Session; s.GetPosition() != 9 || s.GetLap() < 1 || s.GetTotalCars() == 0 {
		t.Errorf("the session at its end: %v, want P9", s)
	}
	if v := r.t.Values(); v.Session != nil {
		t.Errorf("a session after the race: %v", v.Session)
	}
	// The recording goes on in the menus after the race: idle 3 s after the last
	// driving frame, as if it had stopped there.
	if !r.t.Values().Status.GetIdle() {
		t.Error("busy in the menus after the race")
	}
	r = newRig(Config{})
	for _, f := range recording(t, sugoTwoStops) {
		r.step(f)
		if r.clock.Now().Equal(lastDriving) {
			break
		}
	}
	r.clock.add(2900 * time.Millisecond)
	if v := r.t.Values().Status; v.GetIdle() || v.GetIdleSecs() != 0 || !v.GetTelemetryConnected() {
		t.Errorf("2.9 s after the last driving frame: %v", v)
	}
	r.clock.add(100 * time.Millisecond)
	v := r.t.Values().Status
	if !v.GetIdle() || v.GetActivity() != statev1.Activity_ACTIVITY_IDLE || v.GetIdleSecs() != 3 {
		t.Errorf("3 s after the last driving frame: %v", v)
	}
	if v.GetTelemetryConnected() || v.GetLastPacketAgeSecs() != 3 {
		t.Errorf("3 s with no packet: connected %v, age %v", v.GetTelemetryConnected(), v.GetLastPacketAgeSecs())
	}
}

// driving says l's last frame is a driving one: in a race, or paused in one.
func driving(l race.Live) bool {
	return l.Phase == telemetryv1.Phase_PHASE_IN_RACE || l.Phase == telemetryv1.Phase_PHASE_PAUSED
}

// paddock#88, AC2: a GT7 replay says replay while it plays, and has no session.
func TestAGT7ReplaySaysReplay(t *testing.T) {
	r := newRig(Config{})
	discarded, replaying := false, 0
	for _, f := range recording(t, sugoReplay) {
		for _, o := range r.step(f) {
			discarded = discarded || o.Discarded != nil
		}
		v := r.t.Values()
		if v.Session != nil {
			t.Fatalf("frame %d: a session %v", v.Status.GetPackets(), v.Session)
		}
		// Once it is known for a replay, every driving frame says so.
		if discarded && driving(r.m.Live()) {
			replaying++
			if a := v.Status.GetActivity(); a != statev1.Activity_ACTIVITY_REPLAY {
				t.Fatalf("frame %d: %v while the replay plays", v.Status.GetPackets(), a)
			}
		}
	}
	if replaying == 0 {
		t.Error("no frame of the replay was seen as one")
	}
}

// v1's paused: a pause in the race says paused, with the lap of the last
// driving frame.
func TestPausedInTheRace(t *testing.T) {
	frames := recording(t, gilles)
	mid := midLap2(t, frames)
	r := newRig(Config{})
	for _, f := range frames[:mid+1] {
		r.step(f)
	}
	p := frames[mid]
	p.T += 0.1
	p.PacketID++
	p.Flags |= salsa.FlagPaused
	r.step(p)
	v := r.t.Values()
	if v.Status.GetActivity() != statev1.Activity_ACTIVITY_PAUSED || v.Status.GetIdle() {
		t.Errorf("paused: %v", v.Status)
	}
	if v.Session.GetLap() != int32(frames[mid].CurrentLap) {
		t.Errorf("lap %d, want the last driving frame's", v.Session.GetLap())
	}
}

// Before any frame ecu is idle since its start, with no packet age; a stopped
// drive is idle at once.
func TestIdleBeforeAndAfter(t *testing.T) {
	r := newRig(Config{Listening: func() bool { return false }})
	r.clock.add(1500 * time.Millisecond)
	st := r.t.Values().Status
	if !st.GetIdle() || st.GetIdleSecs() != 1.5 || st.LastPacketAgeSecs != nil || st.GetTelemetryConnected() ||
		st.GetTelemetryListening() {
		t.Errorf("before a frame: %v", st)
	}
	if h := r.t.Health(); h["last_packet_age_secs"] != nil || h["activity"] != "idle" || h["idle_secs"] != 1.5 {
		t.Errorf("health before a frame: %v", h)
	}
	frames := recording(t, gilles)
	for _, f := range frames[:midLap2(t, frames)] {
		r.step(f)
	}
	if r.t.Values().Status.GetIdle() {
		t.Fatal("idle mid-race")
	}
	r.t.Stop()
	if !r.t.Values().Status.GetIdle() {
		t.Error("busy after the drive stopped")
	}
}

// The fuel's laps remaining are the fuel rule's gauge, while a session is open.
func TestTheFuelReadsTheGauge(t *testing.T) {
	asked := ""
	r := newRig(Config{Gauge: func(name string) (float64, bool) { asked = name; return 2.5, true }})
	frames := recording(t, gilles)
	for _, f := range frames[:midLap2(t, frames)] {
		r.step(f)
	}
	if v := r.t.Values().Fuel; v.GetLapsRemaining() != 2.5 || asked != "gt7_fuel_laps_remaining" {
		t.Errorf("fuel %v, gauge asked %q", v, asked)
	}
}
