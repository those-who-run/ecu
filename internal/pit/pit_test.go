package pit

import (
	"math"
	"strings"
	"testing"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

// v1's tests/test_pitstop.py at v1.0.0, the ones on frames built by hand. The
// ones on recordings are internal/race's pit_test.go, which has ecu's frames.

const (
	inRace  = telemetryv1.Phase_PHASE_IN_RACE
	loading = telemetryv1.Phase_PHASE_LOADING
	paused  = telemetryv1.Phase_PHASE_PAUSED
	atTrack = telemetryv1.Phase_PHASE_AT_TRACK
)

// fr is v1's _frame: lap 3 at 150 km/h with 50 litres, the packet counter at 60 a
// second, changed by the options.
func fr(t float64, opts ...func(*telemetryv1.Frame)) *telemetryv1.Frame {
	f := &telemetryv1.Frame{
		T: t, PacketId: uint32(t * 60), Lap: 3, Phase: inRace, SpeedKph: 150, FuelLevel: 50,
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

func lap(n int32) func(*telemetryv1.Frame)     { return func(f *telemetryv1.Frame) { f.Lap = n } }
func speed(k float64) func(*telemetryv1.Frame) { return func(f *telemetryv1.Frame) { f.SpeedKph = k } }
func x(m float64) func(*telemetryv1.Frame)     { return func(f *telemetryv1.Frame) { f.PosX = m } }
func fuel(l float64) func(*telemetryv1.Frame)  { return func(f *telemetryv1.Frame) { f.FuelLevel = l } }
func phase(p telemetryv1.Phase) func(*telemetryv1.Frame) {
	return func(f *telemetryv1.Frame) { f.Phase = p }
}

func tyres(c float64) func(*telemetryv1.Frame) {
	return func(f *telemetryv1.Frame) { f.TireFlTemp, f.TireFrTemp, f.TireRlTemp, f.TireRrTemp = c, c, c, c }
}

func observeAll(d *Detector, frames ...*telemetryv1.Frame) []Change {
	out := make([]Change, len(frames))
	for i, f := range frames {
		out[i] = d.Observe(f)
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAQuitOrARestartIsNotAStop(t *testing.T) {
	d := New()
	// quitting: the loading frame resets the lap counter
	observeAll(d, fr(0, x(0)), fr(0.1, lap(0), phase(loading), x(500)), fr(0.2, lap(0), phase(atTrack)))
	// a restart: back on the grid at lap 1, far from where the car was
	for i, c := range observeAll(d, fr(1, x(0)), fr(1.02, lap(1), x(900), speed(0))) {
		if c != Unchanged {
			t.Errorf("frame %d: %v", i, c)
		}
	}
	if s := d.Status(); s.InPitLane || len(s.Stops) != 0 || len(s.Teleports) != 0 {
		t.Errorf("status %+v", s)
	}
}

func TestAStopWhoseExitCutWasLostEndsAtRacingSpeed(t *testing.T) {
	d := New()
	changes := observeAll(d,
		fr(0, x(0)),
		fr(0.02, phase(loading), x(200)),
		fr(0.04, speed(60), x(200)),
		fr(5, speed(0), x(260)),
		fr(10, speed(0), x(260), fuel(100)),
		fr(15, speed(80), x(300), fuel(100)),
		fr(16, lap(4), speed(101), x(330), fuel(100)), // no cut: the packet was lost
	)
	if changes[2] != Entered || changes[6] != Exited {
		t.Fatalf("changes %v", changes)
	}
	s := d.Status()
	if s.InPitLane || len(s.Stops) != 1 {
		t.Fatalf("status %+v", s)
	}
	stop := s.Stops[0]
	if stop.InLap != 3 || stop.OutLap != 4 || stop.FuelAdded() != 50 || !strings.HasSuffix(stop.Evidence, "over 100 km/h") {
		t.Errorf("stop %+v", stop)
	}
	if secs, ok := stop.PitLaneSecs(); !ok || !near(secs, 16-0.04) {
		t.Errorf("pit lane %v %v", secs, ok)
	}
}

func TestAPauseInThePitsKeepsTheStopAndIsNotTimeAtRest(t *testing.T) {
	d := New()
	observeAll(d,
		fr(0, x(0)),
		fr(0.02, phase(loading), x(200)),
		fr(0.04, speed(0), x(200)),
		fr(1.04, speed(0), x(200)),
		fr(1.06, speed(0), x(200), phase(paused)),
		fr(60, speed(0), x(200)),
	)
	s := d.Status()
	if !s.InPitLane || !near(s.Stops[0].StationarySecs, 1) { // 0.04 to 1.04
		t.Errorf("status %+v", s)
	}
}

// resetToTrack is v1's _reset_to_track: off at 150 km/h, put back on track 300 m
// away, straight back up to speed.
func resetToTrack(last ...func(*telemetryv1.Frame)) []*telemetryv1.Frame {
	hot := tyres(90)
	return []*telemetryv1.Frame{
		fr(0, x(0), hot),
		fr(0.02, phase(loading), x(300), hot),
		fr(0.04, speed(40), x(300), hot),
		fr(2, speed(80), x(340), hot),
		fr(4, append([]func(*telemetryv1.Frame){speed(110), x(400), hot}, last...)...),
	}
}

func TestAResetToTrackIsATeleportNotAStop(t *testing.T) {
	for _, cut := range []string{"loading", "teleport"} {
		t.Run(cut, func(t *testing.T) {
			frames := resetToTrack()
			if cut == "teleport" {
				frames = append(frames[:1], frames[2:]...)
			}
			d := New()
			var changes []Change
			for _, c := range observeAll(d, frames...) {
				if c != Unchanged {
					changes = append(changes, c)
				}
			}
			if len(changes) != 2 || changes[0] != Entered || changes[1] != Withdrawn {
				t.Fatalf("changes %v", changes)
			}
			s := d.Status()
			if len(s.Stops) != 0 || s.InPitLane || s.Stint() != 1 || s.IsPitLap(3) || len(s.Teleports) != 1 {
				t.Fatalf("status %+v", s)
			}
			jump := s.Teleports[0]
			if jump.Lap != 3 || jump.T != 4 || jump.EntryT >= jump.T ||
				!strings.HasSuffix(jump.Evidence, "over 100 km/h; never at rest, no fuel, no tyres") {
				t.Errorf("teleport %+v", jump)
			}
		})
	}
}

func TestAStopWithoutRestStillCountsWithFuelOrTyres(t *testing.T) {
	d := New()
	observeAll(d, resetToTrack(fuel(80))...)
	if s := d.Status(); len(s.Stops) != 1 || len(s.Teleports) != 0 {
		t.Errorf("with fuel: %+v", s)
	}
	d = New()
	observeAll(d, resetToTrack(tyres(60))...)
	if s := d.Status(); len(s.Stops) != 1 || !s.Stops[0].TyresChanged || s.Stint() != 2 {
		t.Errorf("with tyres: %+v", s)
	}
}

// A withdrawn entry gives its stop number back: the next entry takes it again.
func TestTheNextEntryAfterATeleportTakesItsNumber(t *testing.T) {
	d := New()
	observeAll(d, resetToTrack()...)
	if c := observeAll(d, fr(5, phase(loading), x(800)), fr(5.02, speed(60), x(800))); c[1] != Entered {
		t.Fatalf("changes %v", c)
	}
	if s := d.Status(); len(s.Teleports) != 1 || s.Stops[0].Number != 1 {
		t.Errorf("status %+v", s)
	}
}

func TestPitLapsRolesAndStints(t *testing.T) {
	s := Status{Stops: []Stop{{Number: 1, InLap: 6, OutLap: 7}}}
	var pitLaps []int32
	for l := int32(1); l <= 12; l++ {
		if s.IsPitLap(l) {
			pitLaps = append(pitLaps, l)
		}
	}
	if len(pitLaps) != 2 || pitLaps[0] != 6 || pitLaps[1] != 7 {
		t.Errorf("pit laps %v", pitLaps)
	}
	for l, want := range map[int32]Role{5: RoleNone, 6: RoleIn, 7: RoleOut, 8: RoleNone} {
		if got := s.LapRole(l); got != want {
			t.Errorf("lap %d: %v, want %v", l, got, want)
		}
	}
	if s.Stint() != 2 || s.StintStartLap() != 7 {
		t.Errorf("stint %d from lap %d", s.Stint(), s.StintStartLap())
	}
	var none Status
	if none.Stint() != 1 || none.StintStartLap() != 1 || none.IsPitLap(1) {
		t.Error("no stops")
	}
	if one := (Status{Stops: []Stop{{InLap: 4, OutLap: 4}}}); one.LapRole(4) != RoleInOut {
		t.Errorf("in and out on lap 4: %v", one.LapRole(4))
	}
	// An open stop marks every lap from the in-lap.
	open := Status{InPitLane: true, Stops: []Stop{{InLap: 6}}}
	if open.LapRole(6) != RoleIn || open.LapRole(7) != RoleOut || open.Stint() != 1 {
		t.Errorf("open: %v %v %d", open.LapRole(6), open.LapRole(7), open.Stint())
	}
	if _, ok := open.Stops[0].PitLaneSecs(); ok {
		t.Error("an open stop has a lane time")
	}
}

func TestAResetForgetsTheStops(t *testing.T) {
	d := New()
	observeAll(d, resetToTrack(fuel(80))...)
	d.Reset()
	if s := d.Status(); len(s.Stops) != 0 || len(s.Teleports) != 0 || s.InPitLane {
		t.Errorf("status %+v", s)
	}
}
