package race

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/salsa"
)

// finishLine is a line of a finish golden (testdata/finish/make_golden.py): a
// session opened, the race finished in it, or it ended with what it stored.
type finishLine struct {
	Event       string `json:"event"`
	Session     int    `json:"session"`
	Frame       uint32 `json:"frame"`
	Kind        string `json:"kind"`
	At          uint32 `json:"at"`
	Lap         int32  `json:"lap"`
	Position    *int32 `json:"position"`
	SessionKind string `json:"session_kind"`
	Finished    bool   `json:"finished"`
	TotalTimeMs *int32 `json:"total_time_ms"`
	BestLapMs   *int32 `json:"best_lap_ms"`
	BestLap     *int32 `json:"best_lap"`
}

func show(p *int32) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprint(*p)
}

// finishLines are the outputs of a replay as a golden's lines, stepping it frame by
// frame to know the frame a finish was published on. A session's end carries the
// best lap of its race.finished: ecu's summary has none yet (paddock#83, #86).
func finishLines(t *testing.T, name string) []string {
	t.Helper()
	m := New(testIDs())
	var lines []string
	n := 0
	var best *eventsv1.RaceFinished
	add := func(out []Output, frame uint32) {
		for _, o := range out {
			switch {
			case o.Started != nil:
				n++
				best = nil
				lines = append(lines, fmt.Sprintf("session.started %d frame %d", n, o.Started.GetFirstSeq()))
			case o.Finished != nil:
				f := o.Finished
				best = f
				kind := map[eventsv1.FinishKind]string{eventsv1.FinishKind_FINISH_KIND_FLAG: "flag", eventsv1.FinishKind_FINISH_KIND_TIMED: "timed"}[f.GetKind()]
				lines = append(lines, fmt.Sprintf("race.finished %d frame %d %s at %d lap %d position %s total %s",
					n, frame, kind, f.GetAt(), f.GetLap(), show(f.Position), show(f.TotalTimeMs)))
			case o.Ended != nil:
				sum := o.Ended.GetSummary()
				var lap, ms *int32
				if best != nil {
					lap, ms = best.BestLap, best.BestLapTimeMs
				}
				line := fmt.Sprintf("session.ended %d finished %t total %s", n, sum.GetFinished(), show(sum.TotalTimeMs))
				if sum.GetFinished() {
					line += fmt.Sprintf(" best lap %s in %s", show(lap), show(ms))
				}
				lines = append(lines, line)
			}
		}
	}
	for _, r := range packets(t, name) {
		out := m.Step(r)
		add(out, out[0].Frame.GetSeq())
	}
	add(m.EndOfInput(), 0)
	return lines
}

// readFinishGolden is a golden as finishLines writes it. v1's position is the one
// GT7 showed; ecu leaves it out outside a field (solo laps), as for a lap.
func readFinishGolden(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "finish", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	var total *int32 // the finished race's total, from its session's end
	var all []finishLine
	for line := range strings.Lines(string(data)) {
		var l finishLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		all = append(all, l)
	}
	for i, l := range all {
		switch l.Event {
		case "session.started":
			lines = append(lines, fmt.Sprintf("session.started %d frame %d", l.Session, l.Frame))
		case "race.finished":
			for _, e := range all[i:] {
				if e.Event == "session.ended" {
					total = e.TotalTimeMs
					break
				}
			}
			position := l.Position
			if l.SessionKind == "solo_laps" || l.SessionKind == "solo_timed" {
				position = nil
			}
			lines = append(lines, fmt.Sprintf("race.finished %d frame %d %s at %d lap %d position %s total %s",
				l.Session, l.Frame, l.Kind, l.At, l.Lap, show(position), show(total)))
		case "session.ended":
			line := fmt.Sprintf("session.ended %d finished %t total %s", l.Session, l.Finished, show(l.TotalTimeMs))
			if l.Finished {
				line += fmt.Sprintf(" best lap %s in %s", show(l.BestLap), show(l.BestLapMs))
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// R1–R4, AC4: on every one of v1's recordings, ecu publishes race.finished where
// v1's engine set finished with a session open, on the same frame, flag or timed,
// as of the same frame (the flag frame or the timed finish's crossing), with v1's
// lap and position; its total time and best lap are what v1's summary stored, and
// the session ends finished, with v1's total time, exactly when v1's did. The
// recordings v1 opens no session on are held to v1's engine with its DriverCheck
// taking every start for a drive (make_golden.py --drive), as ecu does until
// paddock#74.
func TestTheFinishIsV1sOnEveryRecording(t *testing.T) {
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			got, want := finishLines(t, name), readFinishGolden(t, name)
			if len(want) == 0 {
				t.Fatal("an empty golden")
			}
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// finishes is every race.finished of a replay.
func finishes(out []Output) []Output {
	var f []Output
	for _, o := range out {
		if o.Finished != nil {
			f = append(f, o)
		}
	}
	return f
}

// lastEnd is the last session.ended of a replay.
func lastEnd(out []Output) Output {
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Ended != nil {
			return out[i]
		}
	}
	return Output{}
}

// AC1: the five-minute timed race at Suzuka East finishes once, timed, at the line
// of lap 5 in P1, and the frames GT7 replays after the results finish nothing
// again. The session then ends as a timed finish (R4).
func TestTheTimedRaceFinishesOnceAtTheLine(t *testing.T) {
	out := replay(t, "real-nuc-suzuka-timed-race-2026-10-16")
	f := finishes(out)
	if len(f) != 1 {
		t.Fatalf("%d race.finished, want 1", len(f))
	}
	ev := f[0].Finished
	if ev.GetKind() != eventsv1.FinishKind_FINISH_KIND_TIMED || ev.GetLap() != 5 || ev.Position == nil || ev.GetPosition() != 1 {
		t.Errorf("%v, want timed, lap 5, P1", ev)
	}
	if ev.GetSessionId() != f[0].Session || f[0].Event.ID != f[0].Session+":race.finished:1" || f[0].Event.Source != Source {
		t.Errorf("%s %v: want the session's id and <sid>:race.finished:1", f[0].Event.ID, ev)
	}
	end := lastEnd(out)
	if end.Ended == nil || end.Why != WhyTimedFinish || !end.Ended.GetSummary().GetFinished() {
		t.Errorf("last output %v (%v), want the session ended at its timed finish, finished", end.Message(), end.Why)
	}
	// The frames GT7 replays after the results are FINISHED-looking frames past
	// the race; none of them is a second finish, and they open no session.
	var starts int
	for _, o := range out {
		if o.Started != nil {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("%d sessions, want 1", starts)
	}
}

// A session that ends after its timed finish but before the results screen (the
// input ending during GT7's coast, here Suzuka's timed race cut 8 frames after the
// decision) is still finished, with its total time: ecu has published
// race.finished for it, and the contract says its summary is then finished. A
// deliberate departure from v1.0.0, which ended it with _end_session(prev) and
// stored finished false and no total.
func TestASessionCutAfterItsTimedFinishIsFinished(t *testing.T) {
	all := packets(t, "real-nuc-suzuka-timed-race-2026-10-16")[:760]
	out := run(all)
	if f := finishes(out); len(f) != 1 || f[0].Finished.GetKind() != eventsv1.FinishKind_FINISH_KIND_TIMED {
		t.Fatalf("%d race.finished, want the timed finish", len(f))
	}
	end := lastEnd(out)
	sum := end.Ended.GetSummary()
	if end.Why != WhyInputEnded || !sum.GetFinished() || sum.TotalTimeMs == nil || sum.GetTotalTimeMs() != 333162 {
		t.Errorf("ended %v, %v; want the input ended, finished, total 333162 ms", end.Why, sum)
	}
}

// AC2: the two-lap race and Suzuka East's race without a stop finish once each at
// the flag, with the finishing position: P6 and P2.
func TestALapRaceFinishesAtTheFlag(t *testing.T) {
	for name, want := range map[string]int32{
		"real-race-2laps-2026-10-14":         6,
		"real-nuc-suzuka-no-stop-2026-10-15": 2,
	} {
		out := replay(t, name)
		f := finishes(out)
		if len(f) != 1 {
			t.Fatalf("%s: %d race.finished, want 1", name, len(f))
		}
		if ev := f[0].Finished; ev.GetKind() != eventsv1.FinishKind_FINISH_KIND_FLAG || ev.Position == nil || ev.GetPosition() != want {
			t.Errorf("%s: %v, want the flag in P%d", name, ev, want)
		}
		end := lastEnd(out).Ended
		if end.GetReason() != eventsv1.EndReason_END_REASON_FINISHED || end.GetSummary().GetFinishingPosition() != want {
			t.Errorf("%s: ended %v, want finished in P%d", name, end, want)
		}
	}
}

// AC3: qualifying, quit from the menu, has no finish.
func TestQualifyingQuitFromTheMenuHasNoFinish(t *testing.T) {
	out := replay(t, "real-quali-restart-quit-2026-10-14")
	if f := finishes(out); len(f) != 0 {
		t.Errorf("%d race.finished, want none", len(f))
	}
	for _, o := range out {
		if o.Ended != nil && o.Ended.GetSummary().GetFinished() {
			t.Errorf("%s ended finished", o.Session)
		}
	}
}

// coast is a timed race with a field built by hand at 8 frames a second (times a
// float holds exactly): lap 1
// driven, the line crossed with lap 1's time posted, then the frames after it,
// each with the throttle given at the speed given.
func coast(after func(i int) (throttle uint8, kph float32, paused bool)) []Received {
	d := drive{timed: true}
	d.add(0, 0, 50, -1, 3) // the grid
	for range 10 {
		d.add(1, 0, 50, -1, 3)
	}
	d.add(2, 0, 49, 60000, 3) // the line
	for i := range 60 {
		throttle, kph, paused := after(i)
		var flags salsa.Flags
		if paused {
			flags = salsa.FlagPaused
		}
		d.add(2, flags, 49, 60000, 3)
		p := &d.all[len(d.all)-1].Packet
		p.Throttle, p.Speed = throttle, kph/3.6
	}
	for i := range d.all {
		d.all[i].T = float64(i) / 8
	}
	return d.all
}

// R2: hands off at speed for 3 s after the line is the finish, as of the line
// crossing; inputs within 0.5 s of the line still count as the driver's.
func TestThreeSecondsHandsOffAfterTheLineIsTheFinish(t *testing.T) {
	all := coast(func(i int) (uint8, float32, bool) {
		if i < 4 {
			return 200, 150, false // the driver, up to 0.5 s after the line
		}
		return 0, 120, false
	})
	m := New(testIDs())
	var at uint32
	var f []Output
	for _, r := range all {
		out := m.Step(r)
		if got := finishes(out); len(got) > 0 {
			f, at = append(f, got...), out[0].Frame.GetSeq()
		}
	}
	if len(f) != 1 {
		t.Fatalf("%d race.finished, want 1", len(f))
	}
	// The line is frame 12 (t 1.375 s), the coast starts at frame 17 (t 2 s), and
	// 3 s of it is frame 41 (t 5 s).
	if ev := f[0].Finished; ev.GetKind() != eventsv1.FinishKind_FINISH_KIND_TIMED || ev.GetAt() != 12 || ev.GetLap() != 1 || at != 41 {
		t.Errorf("%v on frame %d, want timed as of the line, frame 12, lap 1, on frame 41", ev, at)
	}
}

// R2: inputs more than 0.5 s after the line are the driver driving another lap;
// slow or paused, the car is not coasting.
func TestDrivingOnSlowOrPausedIsNoFinish(t *testing.T) {
	for name, after := range map[string]func(i int) (uint8, float32, bool){
		"throttle 0.75 s after the line": func(i int) (uint8, float32, bool) {
			if i == 5 {
				return 100, 150, false
			}
			return 0, 150, false
		},
		"at 19 km/h": func(int) (uint8, float32, bool) { return 0, 19, false },
		"paused each second": func(i int) (uint8, float32, bool) {
			return 0, 150, i%10 == 9
		},
	} {
		if f := finishes(run(coast(after))); len(f) != 0 {
			t.Errorf("%s: %v, want no finish", name, f[0].Finished)
		}
	}
}

// R3: a time trial (solo timed) coasting after the line is not a finish: only a
// timed race with a field is.
func TestASoloTimedSessionHasNoTimedFinish(t *testing.T) {
	all := coast(func(int) (uint8, float32, bool) { return 0, 150, false })
	for i := range all {
		all[i].TotalCars = 1
		all[i].RacePosition = 1
	}
	if f := finishes(run(all)); len(f) != 0 {
		t.Errorf("%v, want no finish", f[0].Finished)
	}
}

func TestBestLap(t *testing.T) {
	laps := []timedLap{{1, 57100}, {2, 60200}, {3, 59900}, {4, 59900}}
	for _, c := range []struct {
		name     string
		gt7      int32
		skipLap1 bool
		want     timedLap
	}{
		{"quickest, lap 1 counted", 0, false, timedLap{1, 57100}},
		{"quickest, lap 1 of a lap race left out, the first of a tie", 0, true, timedLap{3, 59900}},
		{"GT7's own best", 60200, true, timedLap{2, 60200}},
		{"GT7's best on no lap seen", 58000, true, timedLap{3, 59900}},
	} {
		sess := &session{timedLaps: laps, gt7BestMs: c.gt7}
		if got, ok := sess.bestLap(c.skipLap1); !ok || got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	sess := &session{timedLaps: []timedLap{{1, 57100}}}
	if got, _ := sess.bestLap(true); got != (timedLap{1, 57100}) {
		t.Errorf("lap 1 alone: %v, want it", got)
	}
	if _, ok := (&session{}).bestLap(false); ok {
		t.Error("no lap timed: a best lap")
	}
}
