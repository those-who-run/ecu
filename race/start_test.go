package race

import (
	"slices"
	"strconv"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/salsa"
)

// discard is what v1.0.0's DriverCheck decided on a start it held: the reason,
// its why word for word, and the frames it held, the first to the verdict's.
type discard struct {
	reason    eventsv1.DiscardReason
	why       string
	first, at uint32
}

const (
	whyNoLoading = "it came straight from the menu with no loading screen"
	demo         = eventsv1.DiscardReason_DISCARD_REASON_DEMO
)

// discards are the recordings v1's start check drops, with its verdict on each,
// from v1.0.0's engine replaying them (paddock#74; the four of 2026-10-15 were
// recorded before v1 read the flag, which reads as off throughout, and are
// demos). The goldens have no session on them.
var discards = map[string]discard{
	"real-nuc-sugo-replay-2026-10-18": {
		eventsv1.DiscardReason_DISCARD_REASON_REPLAY,
		"fuel is draining (98.3 -> 97.6), which only a replay does", 62, 114,
	},
	"real-nuc-laguna-seca-demo-2026-10-18": {
		demo, "it was joined part way with no menu seen and GT7 reports 12 cars", 1, 181,
	},
	"real-nuc-nordschleife-rolling-start-2026-10-15": {demo, whyNoLoading, 10, 17},
	"real-nuc-laguna-seca-race-2026-10-15":           {demo, whyNoLoading, 18, 25},
	"real-nuc-tokyo-east-race-2026-10-15":            {demo, whyNoLoading, 19, 26},
	"real-nuc-tsukuba-brz-drift-2026-10-15":          {demo, whyNoLoading, 21, 28},
}

// AC1, AC2, AC3, R2, R4, R6: the SUGO replay and the five demos each give one
// session.discarded, with v1's verdict and why and the frames held, and nothing
// else but their frames, every one of them: no session, lap or verdict, before
// the verdict or after it.
func TestTheReplayAndTheDemosAreDiscarded(t *testing.T) {
	for name, want := range discards {
		t.Run(name, func(t *testing.T) {
			all := packets(t, name)
			out := replay(t, name)
			var frames int
			var got []Output
			for _, o := range out {
				if o.Frame != nil {
					frames++
				} else {
					got = append(got, o)
				}
			}
			if frames != len(all) {
				t.Errorf("%d frames published, want all %d", frames, len(all))
			}
			if len(got) != 1 || got[0].Discarded == nil {
				t.Fatalf("events %v, want one session.discarded", got)
			}
			d, o := got[0].Discarded, got[0]
			if d.GetReason() != want.reason || d.GetEvidence() != want.why ||
				d.GetFirstSeq() != want.first || d.GetAt() != want.at {
				t.Errorf("%v, want %v %q held %d to %d", d, want.reason, want.why, want.first, want.at)
			}
			if o.Session != d.GetSessionId() || o.Event.ID != d.GetSessionId()+":session.discarded:1" ||
				!o.Event.Time.Equal(all[want.at-1].Time) {
				t.Errorf("event %+v for %v", o.Event, d)
			}
		})
	}
}

// R1, AC4: a start with the flag set from its first frame is a drive at once: SUGO's
// two stops open their session on the start's frame, in its own step.
func TestAFlaggedStartIsADriveAtOnce(t *testing.T) {
	m := New(testIDs())
	for _, r := range packets(t, "real-nuc-sugo-two-stops-2026-10-18") {
		for _, o := range m.Step(r) {
			if o.Started != nil {
				if seq := o.Started.GetFirstSeq(); seq != 45 {
					t.Errorf("started on frame %d, want 45", seq)
				}
				if f := m.seq; f != 45 {
					t.Errorf("session.started came with frame %d: held", f)
				}
				return
			}
		}
	}
	t.Fatal("no session.started")
}

// R2, R3: the two-lap race starts with the flag off on frame 1 and sets it on frame
// 4. The three frames before give nothing but themselves, and the rules see none
// of them; frame 4 gives the session, started at frame 1's time, and the rules
// see frames 1 to 4 in turn, as if never held.
func TestAHeldDriveGoesThroughAsIfNeverHeld(t *testing.T) {
	all := packets(t, "real-race-2laps-2026-10-14")
	m := New(testIDs())
	var seen []uint32
	m.Rules(func(ctx *Context) []Output {
		seen = append(seen, ctx.Cur.Seq)
		return nil
	})
	for i, r := range all[:4] {
		out := m.Step(r)
		if i < 3 {
			if len(out) != 1 || out[0].Frame == nil || len(seen) != 0 {
				t.Fatalf("frame %d: %d outputs, the rules saw %v; want the frame alone", i+1, len(out), seen)
			}
			continue
		}
		if out[0].Frame.GetSeq() != 4 {
			t.Fatalf("first output %v, want frame 4", out[0])
		}
		var started *eventsv1.SessionStarted
		for _, o := range out[1:] {
			if o.Started != nil {
				started = o.Started
			}
		}
		if started == nil || started.GetFirstSeq() != 1 || !started.GetStartedAt().AsTime().Equal(all[0].Time) {
			t.Errorf("started %v, want on frame 1 at %v", started, all[0].Time)
		}
	}
	if !slices.Equal(seen, []uint32{1, 2, 3, 4}) {
		t.Errorf("the rules saw frames %v, want 1 to 4", seen)
	}
}

// hand builds packets one per call, a second apart, with GT7's cars-on-track flag
// as given and a 100 l tank.
type hand struct{ all []Received }

func (h *hand) add(lap int16, flag bool, fuel float32) {
	var p salsa.Packet
	p.CurrentLap, p.TotalLaps, p.TotalCars, p.FuelLevel, p.FuelCapacity = lap, 5, 20, fuel, 100
	if flag {
		p.Flags = salsa.FlagCarsOnTrack
	}
	n := len(h.all)
	p.PacketID = int32(n) //nolint:gosec // a handful of packets
	h.all = append(h.all, Received{
		Received: salsa.Received{T: float64(n), Packet: p},
		Time:     epoch.Add(time.Duration(n) * time.Second),
	})
}

// R5, R6: a replay that starts while a session is open ends it first, on the frame
// before; the frames after the replay are ignored, laps and all, until the next
// start, a drive.
func TestAReplayEndsTheOpenSessionAndIsIgnoredUntilTheNextStart(t *testing.T) {
	var h hand
	h.add(0, true, 50)  // 1: the grid
	h.add(1, true, 50)  // 2: the start
	h.add(1, true, 49)  // 3
	h.add(2, true, 48)  // 4: lap 2
	h.add(1, false, 60) // 5: back to lap 1 with the flag off: held
	h.add(1, false, 59) // 6: the fuel drains: a replay
	h.add(2, false, 58) // 7: a lap in the replay
	h.add(-1, false, 0) // 8: the menu
	h.add(0, true, 50)  // 9: the grid
	h.add(1, true, 50)  // 10: the next start
	var whats []string
	for _, o := range events(run(h.all)) {
		switch {
		case o.Started != nil:
			whats = append(whats, "started "+itoa(o.Started.GetFirstSeq()))
		case o.Lap != nil:
			whats = append(whats, "lap "+strconv.Itoa(int(o.Lap.GetLap())))
		case o.Discarded != nil:
			d := o.Discarded
			whats = append(whats, "discarded "+itoa(d.GetFirstSeq())+"-"+itoa(d.GetAt())+" "+d.GetReason().String()+": "+d.GetEvidence())
		case o.Ended != nil:
			whats = append(whats, "ended "+itoa(o.Ended.GetLastSeq())+" "+o.Why.String())
		}
	}
	want := []string{
		"started 2", "lap 1", "lap 2", "ended 4 restart",
		"discarded 5-6 DISCARD_REASON_REPLAY: fuel is draining (60.0 -> 59.0), which only a replay does",
		"started 10", "lap 1", "ended 10 input ended",
	}
	if !slices.Equal(whats, want) {
		t.Errorf("got\n%v\nwant\n%v", whats, want)
	}
}

// The check's own verdicts, as v1's DriverCheck gives them.
func TestTheDriverCheck(t *testing.T) {
	inRace := func(packet int32, t float64, flag bool, fuel float64, cars int32) *state {
		return &state{phase: phaseInRace, packetID: packet, t: t, carsOnTrack: flag, fuel: fuel, fuelCapacity: 100, totalCars: cars}
	}
	for _, c := range []struct {
		name  string
		menu  bool // a menu was seen
		load  bool // and a loading screen after it
		later *state
		want  verdictKind
		why   string
	}{
		{"the flag comes on", true, false, inRace(1, 0.1, true, 100, 1), verdictDrive, ""},
		{"the fuel drains", true, true, inRace(1, 0.1, false, 99.9, 1), verdictReplay,
			"fuel is draining (100.0 -> 99.9), which only a replay does"},
		{"the fuel barely moves", true, false, inRace(1, 0.1, false, 99.96, 1), verdictNone, ""},
		{"3 s with no loading screen", true, false, inRace(10, 3, false, 100, 1), verdictDemo, whyNoLoading},
		{"180 packets after a loading screen", true, true, inRace(180, 1, false, 100, 1), verdictDrive, ""},
		{"the packet id goes back", true, true, inRace(-1, 1, false, 100, 1), verdictDrive, ""},
		{"a pause in the window, no menu, alone", false, false, &state{phase: phasePaused, totalCars: 12}, verdictDrive, ""},
		{"3 s, no menu, a field", false, false, inRace(10, 3, false, 100, 12), verdictDemo,
			"it was joined part way with no menu seen and GT7 reports 12 cars"},
		{"no menu, alone", false, false, inRace(200, 1, false, 100, 1), verdictDrive, ""},
	} {
		var check driverCheck
		if c.menu {
			check.see(&state{phase: phaseMenu})
		}
		if c.load {
			check.see(&state{phase: phaseLoading})
		}
		check.start(inRace(0, 0, false, 100, 1))
		if got := check.observe(c.later); got != c.want || check.why != c.why {
			t.Errorf("%s: %v %q, want %v %q", c.name, got, check.why, c.want, c.why)
		}
	}
}

// A recording that ends while a start is held gives nothing for it, as v1's.
func TestAHoldCutShortGivesNothing(t *testing.T) {
	var h hand
	h.add(0, false, 50)
	h.add(1, false, 50)
	h.add(1, false, 50)
	if evs := events(run(h.all)); len(evs) != 0 {
		t.Errorf("events %v, want none", evs)
	}
}

// The race replay the driver watched on 2026-10-29 (testdata/start, frames 134511
// to 144497 of the live recording): ecu held its start and opened a session, with
// a start brief and a race summary, where v1 opened none. It is a demo by v1's
// check, as v1.0.0 judged it; nothing comes from it, and the next start, session
// 5, is a drive. internal/rules checks that no callout comes either.
func TestTheReplayOf20261029OpensNoSession(t *testing.T) {
	var got []string
	for _, o := range replay(t, "real-nuc-short-shift-replay-2026-10-29") {
		switch {
		case o.Frame != nil:
		case o.Discarded != nil:
			d := o.Discarded
			got = append(got, "discarded "+itoa(d.GetFirstSeq())+"-"+itoa(d.GetAt())+": "+d.GetEvidence())
		case o.Started != nil:
			got = append(got, "started "+itoa(o.Started.GetFirstSeq()))
		case o.Ended != nil:
			got = append(got, "ended "+o.Why.String())
		case len(got) < 2:
			t.Errorf("%s: an event before session 5", o.Event.ID)
		}
	}
	want := []string{"discarded 73-253: " + whyNoLoading, "started 824", "ended input ended"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A held drive after a replay runs normally: its laps close and its session
// ends. v1.0.0 kept ignoring the frames after such a drive's hold, so the race
// went silent until the next start; ecu doesn't (the maintainer's decision,
// 2026-10-30, paddock#74).
func TestAHeldDriveAfterAReplayRunsNormally(t *testing.T) {
	var h hand
	h.add(1, false, 60) // 1: a replay starts, the flag off: held
	h.add(1, false, 59) // 2: the fuel drains: discarded
	h.add(-1, false, 0) // 3: the menu
	h.add(0, false, 50) // 4: the grid
	h.add(1, false, 50) // 5: the start, the flag still off: held
	h.add(1, true, 50)  // 6: the flag comes on: a drive
	h.add(1, true, 49)  // 7
	h.add(2, true, 48)  // 8: lap 2
	h.add(3, true, 47)  // 9: lap 3
	var whats []string
	for _, o := range events(run(h.all)) {
		switch {
		case o.Started != nil:
			whats = append(whats, "started "+itoa(o.Started.GetFirstSeq()))
		case o.Lap != nil:
			whats = append(whats, "lap "+strconv.Itoa(int(o.Lap.GetLap())))
		case o.Discarded != nil:
			whats = append(whats, "discarded "+itoa(o.Discarded.GetFirstSeq())+"-"+itoa(o.Discarded.GetAt()))
		case o.Ended != nil:
			whats = append(whats, "ended "+itoa(o.Ended.GetLastSeq())+" "+o.Why.String())
		}
	}
	want := []string{"discarded 1-2", "started 5", "lap 1", "lap 2", "lap 3", "ended 9 input ended"}
	if !slices.Equal(whats, want) {
		t.Errorf("got\n%v\nwant\n%v", whats, want)
	}
}

// paddock#87 with paddock#74: the feed lost while a start is held is
// telemetry.lost with no session, and the rules are not told, as they are told
// of no held frame; nothing ends or opens. The feed back, the start is still
// held, and the flag coming on makes it a drive from its first frame, the rules
// told of the return on the frame it came on.
func TestALossDuringAHoldOpensAndEndsNothing(t *testing.T) {
	var h hand
	h.add(0, false, 50) // 1: the grid
	h.add(1, false, 50) // 2: the start, the flag off: held
	h.add(1, false, 50) // 3
	h.add(1, true, 50)  // 4: the flag on, after the loss: a drive
	m := New(testIDs())
	m.WatchTelemetry(3 * time.Second)
	var ruled []uint32
	m.Rules(func(ctx *Context) []Output {
		if ctx.Tick {
			t.Error("the rules were told of a loss during the hold")
		}
		ruled = append(ruled, ctx.Cur.Seq)
		if (ctx.Restored != nil) != (ctx.Cur.Seq == 4) {
			t.Errorf("frame %d: restored %v, want it on frame 4 alone", ctx.Cur.Seq, ctx.Restored)
		}
		return nil
	})
	for _, r := range h.all[:3] {
		m.Step(r)
	}
	lost := m.Tick(h.all[2].Time.Add(5 * time.Second))
	if len(lost) != 1 || lost[0].TelemetryLost == nil || lost[0].Session != "" {
		t.Fatalf("tick: %v, want one telemetry.lost with no session", lost)
	}
	back := h.all[3]
	back.Time = h.all[2].Time.Add(6 * time.Second)
	var whats []string
	for _, o := range m.Step(back) {
		switch {
		case o.TelemetryRestored != nil:
			whats = append(whats, "restored")
		case o.Started != nil:
			whats = append(whats, "started "+itoa(o.Started.GetFirstSeq()))
		case o.Ended != nil, o.Discarded != nil:
			whats = append(whats, "ended or discarded")
		}
	}
	if want := []string{"restored", "started 2"}; !slices.Equal(whats, want) {
		t.Errorf("frame 4: %v, want %v", whats, want)
	}
	if want := []uint32{1, 2, 3, 4}; !slices.Equal(ruled, want) {
		t.Errorf("the rules saw %v, want %v", ruled, want)
	}
}
