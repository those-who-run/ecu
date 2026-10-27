package race

import (
	"bytes"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/salsa"
)

// drive builds packets by hand, one per call, a second apart: a five-lap race in a
// field of 20 cars unless timed or cars say otherwise.
type drive struct {
	all   []Received
	timed bool  // no lap total
	cars  int16 // GT7's car count; 0 for 20
}

func (d *drive) add(lap int16, flags salsa.Flags, fuel float32, lastLapMs int32, position int16) {
	var p salsa.Packet
	p.CurrentLap, p.TotalLaps, p.Flags = lap, 5, flags|salsa.FlagCarsOnTrack
	if d.timed {
		p.TotalLaps = 0
	}
	p.TotalCars = 20
	if d.cars != 0 {
		p.TotalCars = d.cars
	}
	p.FuelLevel, p.LastLapMs, p.RacePosition = fuel, lastLapMs, position
	n := len(d.all)
	d.all = append(d.all, Received{
		Received: salsa.Received{T: float64(n), Packet: p},
		Time:     epoch.Add(time.Duration(n) * time.Second),
	})
}

// events is the outputs without the frames.
func events(out []Output) []Output {
	var evs []Output
	for _, o := range out {
		if o.Frame == nil {
			evs = append(evs, o)
		}
	}
	return evs
}

func TestARestartEndsTheSessionAndStartsTheNext(t *testing.T) {
	var d drive
	d.add(0, 0, 50, -1, 8)     // 1: the grid
	d.add(1, 0, 50, -1, 8)     // 2: the start
	d.add(1, 0, 48, -1, 7)     // 3
	d.add(2, 0, 45, 61000, 6)  // 4: lap 1 in 61 s
	d.add(2, 0, 44, 61000, -1) // 5
	d.add(1, 0, 50, 61000, 8)  // 6: restarted, straight back to lap 1
	d.add(1, 0, 49, 61000, 8)  // 7
	evs := events(run(d.all))
	if len(evs) != 7 {
		t.Fatalf("%d events, want 7: start, lap 1, lap 2, end, then start, lap 1, end", len(evs))
	}
	first, lap1, lap2, end, next, stopped := evs[0], evs[1].Lap, evs[2].Lap, evs[3], evs[4], evs[6]
	if first.Started.GetFirstSeq() != 2 || first.Started.GetFuelAtStart() != 50 ||
		first.Started.GetSessionType() != eventsv1.SessionType_SESSION_TYPE_LAP_RACE || first.Started.GetTotalLaps() != 5 {
		t.Errorf("start: %v", first.Started)
	}
	if lap1.GetLap() != 1 || !lap1.GetComplete() || lap1.GetLapTimeMs() != 61000 || lap1.GetFirstSeq() != 2 ||
		lap1.GetLastSeq() != 3 || lap1.GetFuelUsed() != 5 || lap1.GetPosition() != 6 {
		t.Errorf("lap 1: %v", lap1)
	}
	// The restart ends the session on the frame before it, mid-lap 2.
	if lap2.GetLap() != 2 || lap2.GetComplete() || lap2.GetLapTimeMs() != 0 || lap2.GetFirstSeq() != 4 ||
		lap2.GetLastSeq() != 5 || lap2.GetFuelAtEnd() != 44 || lap2.Position != nil {
		t.Errorf("lap 2: %v", lap2)
	}
	summary := end.Ended.GetSummary()
	if end.Why != WhyRestart || end.Ended.GetReason() != eventsv1.EndReason_END_REASON_RESTARTED ||
		end.Ended.GetLastSeq() != 5 || summary.GetFuelUsed() != 6 || summary.GetFinalPosition() != 6 ||
		summary.GetLapsCompleted() != 1 || summary.FinishingPosition != nil ||
		summary.GetSessionType() != eventsv1.SessionType_SESSION_TYPE_LAP_RACE ||
		!end.Event.Time.Equal(d.all[4].Time) {
		t.Errorf("end: %v %v at %v", end.Ended, end.Why, end.Event.Time)
	}
	if next.Started.GetFirstSeq() != 6 || next.Session == first.Session {
		t.Errorf("next start: %v", next.Started)
	}
	if stopped.Why != WhyInputEnded || stopped.Ended.GetReason() != eventsv1.EndReason_END_REASON_INPUT_ENDED ||
		stopped.Ended.GetLastSeq() != 7 {
		t.Errorf("input ended: %v %v", stopped.Why, stopped.Ended)
	}
}

func TestAStartWhileASessionIsOpenEndsIt(t *testing.T) {
	var d drive
	d.add(1, 0, 50, -1, 3)                // 1: the first frame, driving
	d.add(0, salsa.FlagPaused, 50, -1, 3) // 2: paused, the lap counter reset
	d.add(0, 0, 50, -1, 3)                // 3: the track screen
	d.add(1, 0, 50, -1, 3)                // 4: a start
	evs := events(run(d.all))
	if len(evs) != 6 {
		t.Fatalf("%d events, want 6: start, lap 1, end, then start, lap 1, end", len(evs))
	}
	if end := evs[2]; end.Why != WhyStart || end.Ended.GetReason() != eventsv1.EndReason_END_REASON_NEXT_SESSION_STARTED ||
		end.Ended.GetLastSeq() != 3 || end.Ended.GetSummary().GetFinalPosition() != 3 {
		t.Errorf("end: %v %v", end.Why, end.Ended)
	}
	// The lap that the start cuts short is numbered by its last frame's lap.
	if lap := evs[1].Lap; lap.GetLap() != 1 || lap.GetComplete() || lap.GetFirstSeq() != 1 || lap.GetLastSeq() != 1 {
		t.Errorf("lap: %v", lap)
	}
	if evs[3].Started.GetFirstSeq() != 4 {
		t.Errorf("start: %v", evs[3].Started)
	}
}

func TestACrossingWithNoFrameBeforeItStaysInItsLap(t *testing.T) {
	var d drive
	d.add(0, 0, 50, -1, 1)                 // 1: the track screen
	d.add(1, 0, 50, -1, 1)                 // 2: the start, its first driving frame
	d.add(1, salsa.FlagLoading, 49, -1, 1) // 3
	d.add(2, salsa.FlagLoading, 49, 0, 1)  // 4: GT7 posts lap 1 on a loading frame, with no time
	d.add(2, 0, 48, 0, 1)                  // 5: the crossing, seen from frame 2
	evs := events(run(d.all))
	if len(evs) != 4 {
		t.Fatalf("%d events, want start, lap 1, lap 2, end", len(evs))
	}
	// Only frames 2 and 5 drove, so the crossing frame is the lap's second.
	if lap := evs[1].Lap; lap.GetLap() != 1 || !lap.GetComplete() || lap.GetLapTimeMs() != 0 ||
		lap.GetFirstSeq() != 2 || lap.GetLastSeq() != 2 {
		t.Errorf("lap 1: %v", lap)
	}
	if lap := evs[2].Lap; lap.GetLap() != 2 || lap.GetFirstSeq() != 5 || lap.GetLastSeq() != 5 {
		t.Errorf("lap 2: %v", lap)
	}
}

func TestCloseWithNoSessionGivesNothing(t *testing.T) {
	m := New(testIDs())
	if out := m.EndOfInput(); out != nil {
		t.Errorf("a new machine closed with %v", out)
	}
	if out := m.Stop(); out != nil {
		t.Errorf("a new machine stopped with %v", out)
	}
	var d drive
	d.add(-1, 0, 0, -1, -1)
	if out := m.Step(d.all[0]); len(out) != 1 || out[0].Frame == nil {
		t.Errorf("a menu frame gave %v", out)
	}
	if out := m.EndOfInput(); out != nil {
		t.Errorf("closed in the menus with %v", out)
	}
}

func TestWhy(t *testing.T) {
	cases := []struct {
		why    Why
		name   string
		reason eventsv1.EndReason
	}{
		{WhyFinished, "finished", eventsv1.EndReason_END_REASON_FINISHED},
		{WhyQuit, "quit", eventsv1.EndReason_END_REASON_QUIT},
		{WhyRestart, "restart", eventsv1.EndReason_END_REASON_RESTARTED},
		{WhyStart, "start", eventsv1.EndReason_END_REASON_NEXT_SESSION_STARTED},
		{WhyInputEnded, "input ended", eventsv1.EndReason_END_REASON_INPUT_ENDED},
		{WhyStopped, "stopped", eventsv1.EndReason_END_REASON_ENGINEER_STOPPED},
		{0, "Why(0)", eventsv1.EndReason_END_REASON_UNKNOWN},
	}
	for _, c := range cases {
		if c.why.String() != c.name || c.why.Reason() != c.reason {
			t.Errorf("%d: %q %v, want %q %v", int(c.why), c.why.String(), c.why.Reason(), c.name, c.reason)
		}
	}
}

func TestMessage(t *testing.T) {
	f, s, l, e := &telemetryv1.Frame{}, &eventsv1.SessionStarted{}, &eventsv1.LapCompleted{}, &eventsv1.SessionEnded{}
	for _, c := range []struct {
		o    Output
		want proto.Message
	}{{Output{Frame: f}, f}, {Output{Started: s}, s}, {Output{Lap: l}, l}, {Output{Ended: e}, e}} {
		if c.o.Message() != c.want {
			t.Errorf("%+v: got %T", c.o, c.o.Message())
		}
	}
}

// round3 is Python's round(x, 3), ties to even on the exact binary value.
func TestRound3(t *testing.T) {
	for in, want := range map[float64]float64{
		15.829000473022461: 15.829,
		-16.86054229736328: -16.861,
		0.0625:             0.062, // an exact tie: to even
		0.1875:             0.188,
		2.5:                2.5,
		-0.0004:            0,
	} {
		if got := round3(in); got != want {
			t.Errorf("round3(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestNewIDsRepeatsWithTheSameClockAndEntropy(t *testing.T) {
	a, b := testIDs(), testIDs()
	for range 3 {
		x, y := a(), b()
		if x != y || x.Time() != uint64(epoch.UnixMilli()) { //nolint:gosec // epoch is after 1970
			t.Fatalf("%v and %v", x, y)
		}
	}
	other := NewIDs(func() time.Time { return epoch }, bytes.NewReader(bytes.Repeat([]byte{1}, 64)))
	if other() == testIDs()() {
		t.Error("other entropy gave the same id")
	}
	if New(testIDs()).Run() != testIDs()().String() {
		t.Error("the run id is not the generator's first id")
	}
}
