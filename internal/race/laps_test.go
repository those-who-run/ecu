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
	"github.com/those-who-run/ecu/internal/pit"
)

// lapLine is a line of a lap golden (testdata/lap/make_golden.py): a session
// opened, a lap v1 stored, or a session ended with the best lap it stored.
type lapLine struct {
	Event         string `json:"event"`
	Session       int    `json:"session"`
	Frame         uint32 `json:"frame"`
	Lap           int32  `json:"lap"`
	LapTimeMs     *int32 `json:"lap_time_ms"`
	Complete      bool   `json:"complete"`
	Pit           string `json:"pit"`
	CountsForBest *bool  `json:"counts_for_best"`
	BestLapMs     *int32 `json:"best_lap_ms"`
	FastestLapMs  *int32 `json:"fastest_lap_ms"`
	GT7BestLapMs  *int32 `json:"gt7_best_lap_ms"`
	BestLap       *int32 `json:"best_lap"`
}

var roleNames = map[eventsv1.PitRole]string{
	eventsv1.PitRole_PIT_ROLE_NONE:       "none",
	eventsv1.PitRole_PIT_ROLE_IN_LAP:     "in",
	eventsv1.PitRole_PIT_ROLE_OUT_LAP:    "out",
	eventsv1.PitRole_PIT_ROLE_IN_AND_OUT: "in_out",
}

func ms(p *int32) string {
	if p == nil || *p == 0 {
		return "none"
	}
	return fmt.Sprint(*p)
}

// lapLines are the outputs of a replay as a lap golden's lines, stepping it frame
// by frame to know the frame each lap closed on. A session's end carries its
// summary's best lap, fastest lap and GT7's best, and the last lap.best's lap and
// time, which must be the summary's.
func lapLines(t *testing.T, name string) []string {
	t.Helper()
	m := New(testIDs())
	var lines []string
	n := 0
	var best *eventsv1.LapBestChanged
	add := func(out []Output, frame uint32) {
		for _, o := range out {
			switch {
			case o.Started != nil:
				n++
				best = nil
				lines = append(lines, fmt.Sprintf("session.started %d frame %d", n, o.Started.GetFirstSeq()))
			case o.Lap != nil:
				l := o.Lap
				lines = append(lines, fmt.Sprintf("lap.completed %d frame %d lap %d time %s complete %t pit %s counts %t",
					n, frame, l.GetLap(), ms(&l.LapTimeMs), l.GetComplete(), roleNames[l.GetPitRole()], l.GetCountsForBest()))
			case o.Best != nil:
				best = o.Best
			case o.Ended != nil:
				sum := o.Ended.GetSummary()
				lines = append(lines, fmt.Sprintf("session.ended %d best lap %s in %s fastest %s gt7 %s",
					n, ms(sum.BestLap), ms(sum.BestLapTimeMs), ms(sum.FastestLapMs), ms(sum.GameBestLapMs)))
				if best != nil && (best.GetLap() != sum.GetBestLap() || best.GetLapTimeMs() != sum.GetBestLapTimeMs()) {
					t.Errorf("session %d: the last lap.best is lap %d in %d, the summary's lap %d in %d",
						n, best.GetLap(), best.GetLapTimeMs(), sum.GetBestLap(), sum.GetBestLapTimeMs())
				}
				if best == nil && sum.BestLap != nil {
					t.Errorf("session %d: no lap.best, the summary's best lap %d", n, sum.GetBestLap())
				}
			}
		}
	}
	all := packets(t, name)
	for _, r := range all {
		out := m.Step(r)
		add(out, out[0].Frame.GetSeq())
	}
	add(m.EndOfInput(), uint32(len(all))) //nolint:gosec // a recording's frame count fits
	return lines
}

// readLapGolden is a golden as lapLines writes it. A lap the lap_time rule never
// took a time for does not count. The best lap is v1's (results.best_lap, which
// v1 stored as best_lap_ms) but where contracts v0.2.3 says ecu's differs: v1's
// fallback, with no lap at GT7's best, took the quickest complete lap whether or
// not it counted; ecu takes none that does not.
func readLapGolden(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "lap", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	counts := map[int32]bool{} // the session's laps that count
	for line := range strings.Lines(string(data)) {
		var l lapLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		switch l.Event {
		case "session.started":
			clear(counts)
			lines = append(lines, fmt.Sprintf("session.started %d frame %d", l.Session, l.Frame))
		case "lap.completed":
			role := l.Pit
			if role == "" {
				role = "none"
			}
			c := l.CountsForBest != nil && *l.CountsForBest
			counts[l.Lap] = c
			lines = append(lines, fmt.Sprintf("lap.completed %d frame %d lap %d time %s complete %t pit %s counts %t",
				l.Session, l.Frame, l.Lap, ms(l.LapTimeMs), l.Complete, role, c))
		case "session.ended":
			lap, best := l.BestLap, l.BestLapMs
			gt7 := l.GT7BestLapMs != nil && best != nil && *best == *l.GT7BestLapMs
			if lap != nil && !gt7 && !counts[*lap] {
				lap, best = nil, nil
			}
			lines = append(lines, fmt.Sprintf("session.ended %d best lap %s in %s fastest %s gt7 %s",
				l.Session, ms(lap), ms(best), ms(l.FastestLapMs), ms(l.GT7BestLapMs)))
		}
	}
	return lines
}

// AC3, R1, R2, R5: on every one of v1's recordings, each lap.completed closes on
// the frame v1's store closed the lap, with v1's lap, time, complete and pit role,
// and counts for the best exactly when v1's lap_time rule counted it; each
// summary's best lap, fastest lap and GT7's best are what v1 stored, the best but
// for the difference contracts v0.2.3 gives; and the last lap.best of each session
// is the summary's best (R3). On the replay and the demos v1's start check drops
// (paddock#74, discards) there is no session, and no lap.
func TestEveryLapIsV1sOnEveryRecording(t *testing.T) {
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			got, want := lapLines(t, name), readLapGolden(t, name)
			if _, ok := discards[name]; len(want) == 0 && !ok {
				t.Fatal("an empty golden")
			}
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// AC1: for each recording v1's tests/test_best_lap_stored.py reads, the summary's
// best lap is v1's best_lap_ms, and the last lap.best is the results card's best
// lap (results.best_lap), with no difference to map.
func TestTheBestLapIsTheOneV1Stored(t *testing.T) {
	for _, name := range []string{
		"real-race-2laps-2026-10-14",
		"real-nuc-suzuka-no-stop-2026-10-15",
		"real-nuc-suzuka-pit-stop-2026-10-15",
		"real-nuc-suzuka-timed-race-2026-10-16",
		"real-nuc-gilles-villeneuve-race-2026-10-17",
	} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "lap", name+".jsonl")) //nolint:gosec // a golden of the tests
			if err != nil {
				t.Fatal(err)
			}
			var want []lapLine
			for line := range strings.Lines(string(data)) {
				var l lapLine
				if err := json.Unmarshal([]byte(line), &l); err != nil {
					t.Fatal(err)
				}
				if l.Event == "session.ended" {
					want = append(want, l)
				}
			}
			var ends []*eventsv1.SessionSummary
			var lastBest []*eventsv1.LapBestChanged
			var best *eventsv1.LapBestChanged
			for _, o := range replay(t, name) {
				switch {
				case o.Started != nil:
					best = nil
				case o.Best != nil:
					best = o.Best
				case o.Ended != nil:
					ends = append(ends, o.Ended.GetSummary())
					lastBest = append(lastBest, best)
				}
			}
			if len(ends) != len(want) {
				t.Fatalf("%d sessions, want %d", len(ends), len(want))
			}
			for i, w := range want {
				if w.BestLap == nil || w.BestLapMs == nil {
					t.Fatalf("session %d: v1 stored no best lap", i+1)
				}
				if got := ends[i]; got.BestLapTimeMs == nil || got.GetBestLapTimeMs() != *w.BestLapMs {
					t.Errorf("session %d: summary's best %s ms, v1's %d", i+1, ms(got.BestLapTimeMs), *w.BestLapMs)
				}
				if b := lastBest[i]; b == nil || b.GetLap() != *w.BestLap {
					t.Errorf("session %d: last lap.best %v, want the card's lap %d", i+1, b, *w.BestLap)
				}
			}
		})
	}
}

// AC2: at Suzuka East (2026-10-15) the car crosses the line into the pits in the
// entry's cut-scene, so GT7 posts lap 3 on the loading frame; ecu closes lap 3 on
// the driving frame after it, seen against the last driving frame, as an in-lap
// that does not count for the best.
func TestThePitInLapPostedOnALoadingFrameIsAnInLap(t *testing.T) {
	all := packets(t, "real-nuc-suzuka-pit-stop-2026-10-15")
	m := New(testIDs())
	var prev Received
	var found bool
	for i, r := range all {
		for _, o := range m.Step(r) {
			if o.Lap == nil || o.Lap.GetLap() != 3 {
				continue
			}
			found = true
			if i == 0 || !prev.Flags.Loading() {
				t.Errorf("lap 3 closed on frame %d, after a frame that is no loading screen", i+1)
			}
			if o.Lap.GetPitRole() != eventsv1.PitRole_PIT_ROLE_IN_LAP || !o.Lap.GetComplete() ||
				o.Lap.GetLapTimeMs() != 82544 || o.Lap.GetCountsForBest() {
				t.Errorf("lap 3: %v, want an in-lap of 82.544 s that does not count", o.Lap)
			}
		}
		prev = r
	}
	if !found {
		t.Fatal("no lap 3")
	}
}

// R2: v1's lap_time rule's `counts`. A start lap from a grid ahead of the line
// does not count: lap 1 of a session with a lap total, and lap 1 of a timed one
// posted with GT7's best still blank. A pit lap, and a lap with no time, never
// counts.
func TestWhichLapsCountForTheBest(t *testing.T) {
	for _, c := range []struct {
		name      string
		lap       int32
		totalLaps int32
		gt7Best   int32
		lastLapMs int32
		role      pit.Role
		want      bool
	}{
		{"lap 2 of a lap race", 2, 5, 60000, 60000, pit.RoleNone, true},
		{"lap 1 of a lap race, GT7 counting it", 1, 5, 60000, 60000, pit.RoleNone, false},
		{"lap 1 of a timed race, GT7's best blank", 1, 0, -1, 62000, pit.RoleNone, false},
		{"lap 1 of qualifying, GT7's best set", 1, 0, 62000, 62000, pit.RoleNone, true},
		{"an in-lap", 3, 5, 60000, 82000, pit.RoleIn, false},
		{"an out-lap", 4, 5, 60000, 69000, pit.RoleOut, false},
		{"an in and out lap", 4, 5, 60000, 90000, pit.RoleInOut, false},
		{"no time", 2, 5, 60000, -1, pit.RoleNone, false},
	} {
		s := &state{totalLaps: c.totalLaps, bestLapMs: c.gt7Best, lastLapMs: c.lastLapMs}
		if got := countsForBest(c.lap, s, c.role); got != c.want {
			t.Errorf("%s: %t, want %t", c.name, got, c.want)
		}
	}
}

func TestTheSessionsBest(t *testing.T) {
	laps := []timedLap{{1, 57100, false}, {2, 60200, true}, {3, 82000, false}, {4, 59900, true}, {5, 59900, true}}
	for _, c := range []struct {
		name string
		gt7  int32
		want timedLap
	}{
		{"the quickest that counts, the first of a tie", 0, timedLap{4, 59900, true}},
		{"GT7's own best", 60200, timedLap{2, 60200, true}},
		{"GT7's own best on a lap that does not count", 57100, timedLap{1, 57100, false}},
		{"GT7's best on no lap seen", 58000, timedLap{4, 59900, true}},
	} {
		b := lapBook{timed: laps, gt7BestMs: c.gt7}
		if got, ok := b.currentBest(); !ok || got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if _, ok := (&lapBook{timed: []timedLap{{1, 57100, false}}}).currentBest(); ok {
		t.Error("only lap 1, which does not count: a best lap")
	}
	if _, ok := (&lapBook{}).currentBest(); ok {
		t.Error("no lap timed: a best lap")
	}
}

// R3: lap.best on each change of the session's best, as of the lap whose end
// changed it, with the best before it and the gap; GT7's own best moving to a
// slower lap than ecu's gives a negative gap. A lap that does not change the best
// gives none.
func TestLapBestOnEachChange(t *testing.T) {
	m := New(testIDs())
	m.session = &session{id: "s"}
	book := &m.session.laps
	closeLap := func(lap, lapMs, gt7 int32, counts bool) *eventsv1.LapBestChanged {
		m.out = nil
		book.timed = append(book.timed, timedLap{lap, lapMs, counts})
		book.gt7BestMs = gt7
		m.publishBest(lap, uint32(lap*100), epoch) //nolint:gosec // small
		if len(m.out) == 0 {
			return nil
		}
		if id := m.out[0].Event.ID; id != fmt.Sprintf("s:lap.best:%d", lap) {
			t.Errorf("id %s", id)
		}
		return m.out[0].Best
	}
	if b := closeLap(1, 57100, -1, false); b != nil {
		t.Errorf("lap 1 from the grid: %v, want no best", b)
	}
	b := closeLap(2, 60200, -1, true)
	if b.GetLap() != 2 || b.GetLapTimeMs() != 60200 || b.PreviousBestMs != nil || b.GapMs != nil || b.GetAt() != 200 || b.GetEndedLap() != 2 {
		t.Errorf("first best: %v", b)
	}
	if b := closeLap(3, 61000, -1, true); b != nil {
		t.Errorf("a slower lap: %v, want no best", b)
	}
	b = closeLap(4, 59800, -1, true)
	if b.GetLap() != 4 || b.GetPreviousBestMs() != 60200 || b.GetGapMs() != 400 {
		t.Errorf("new best: %v", b)
	}
	// GT7 left lap 4 out (a penalty, say) and sets its best at lap 2's time.
	b = closeLap(5, 60500, 60200, true)
	if b.GetLap() != 2 || b.GetLapTimeMs() != 60200 || b.GetPreviousBestMs() != 59800 || b.GetGapMs() != -400 || b.GetEndedLap() != 5 {
		t.Errorf("GT7's best: %v", b)
	}
}
