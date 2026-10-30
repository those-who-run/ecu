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
)

// placesLine is a line of a final lap and position golden
// (testdata/final-lap-position/make_golden.py).
type placesLine struct {
	Event    string `json:"event"`
	Session  int    `json:"session"`
	Frame    uint32 `json:"frame"`
	Lap      int32  `json:"lap"`
	Previous int32  `json:"previous"`
	Position int32  `json:"position"`
}

func readPlacesGolden(t *testing.T, name string) []placesLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "final-lap-position", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var all []placesLine
	for line := range strings.Lines(string(data)) {
		var l placesLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		all = append(all, l)
	}
	return all
}

// showPlaces is a golden's lines as placesLines writes them, without v1's calls.
func showPlaces(all []placesLine) []string {
	var lines []string
	for _, l := range all {
		switch l.Event {
		case "session.started":
			lines = append(lines, fmt.Sprintf("session.started %d frame %d", l.Session, l.Frame))
		case "race.final_lap":
			lines = append(lines, fmt.Sprintf("race.final_lap %d frame %d lap %d", l.Session, l.Frame, l.Lap))
		case "race.position":
			lines = append(lines, fmt.Sprintf("race.position %d frame %d P%d to P%d", l.Session, l.Frame, l.Previous, l.Position))
		}
	}
	return lines
}

// placesLines are a replay's starts, final laps and positions as a golden's lines.
// Each event is on the frame it names (its at), which the test checks too.
func placesLines(t *testing.T, out []Output) []string {
	t.Helper()
	var lines []string
	n := 0
	frame := uint32(0)
	for _, o := range out {
		switch {
		case o.Frame != nil:
			frame = o.Frame.GetSeq()
		case o.Started != nil:
			n++
			lines = append(lines, fmt.Sprintf("session.started %d frame %d", n, o.Started.GetFirstSeq()))
		case o.FinalLap != nil:
			if o.FinalLap.GetAt() != frame {
				t.Errorf("race.final_lap at %d, published on frame %d", o.FinalLap.GetAt(), frame)
			}
			lines = append(lines, fmt.Sprintf("race.final_lap %d frame %d lap %d", n, o.FinalLap.GetAt(), o.FinalLap.GetLap()))
		case o.Position != nil:
			p := o.Position
			if p.GetAt() != frame {
				t.Errorf("race.position at %d, published on frame %d", p.GetAt(), frame)
			}
			lines = append(lines, fmt.Sprintf("race.position %d frame %d P%d to P%d", n, p.GetAt(), p.GetPreviousPosition(), p.GetPosition()))
		}
	}
	return lines
}

// R1, R2, R3: on every one of v1's recordings, ecu publishes race.final_lap where
// v1's FinalLapRule called the final lap, and race.position where v1's
// PositionRule, with only its 3 s settle (no start grace, no time between calls),
// took a new position, on the same frame, with the same lap or the same places,
// in an open session. On the replay and the demos v1's start check drops
// (paddock#74, discards) there is no session, and neither.
func TestTheFinalLapAndPositionsAreV1sOnEveryRecording(t *testing.T) {
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			want := showPlaces(readPlacesGolden(t, name))
			if _, ok := discards[name]; len(want) == 0 && !ok {
				t.Fatal("an empty golden")
			}
			if got := placesLines(t, replay(t, name)); !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// AC4: every position v1 called (PositionRule as rules.example.yaml has it, 15 s
// of grace and 10 s between calls) has a race.position at or before its frame, in
// its session, as contracts v0.2.3's RacePositionChanged says: events are a
// superset of calls, not one to one. The place called need not be the last one
// settled: while v1's call waits out its grace or interval, its settle timer runs
// on through every place that differs from the last call, so it can call a place
// held under 3 s: P13 on frame 171 at Suzuka, after P12 settled on frame 164,
// which race.position gives once it has held, on frame 174, and P18 on frame
// 234 of SUGO's restarts, which never holds (race.position goes P16 to P19).
func TestEveryV1PositionCallHasARacePosition(t *testing.T) {
	calls := 0
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			all := readPlacesGolden(t, name)
			for _, c := range all {
				if c.Event != "position.call" {
					continue
				}
				calls++
				if !slices.ContainsFunc(all, func(l placesLine) bool {
					return l.Event == "race.position" && l.Session == c.Session && l.Frame <= c.Frame
				}) {
					t.Errorf("v1 called P%d on frame %d of session %d, with no race.position at or before it", c.Position, c.Frame, c.Session)
				}
			}
		})
	}
	if calls == 0 {
		t.Error("no v1 position call in any golden")
	}
}

// positionsAndFinalLaps are a replay's race.position and race.final_lap events.
func positionsAndFinalLaps(out []Output) (positions, finalLaps []Output) {
	for _, o := range out {
		switch {
		case o.Position != nil:
			positions = append(positions, o)
		case o.FinalLap != nil:
			finalLaps = append(finalLaps, o)
		}
	}
	return positions, finalLaps
}

// AC1: SUGO's twelve laps with two stops give one race.final_lap, on lap 12, and
// no race.position inside the pit lane; the first after each exit gives the net
// change against the last place published before the entry (R3).
func TestSugoGivesTheNetChangeAfterEachStop(t *testing.T) {
	out := replay(t, "real-nuc-sugo-two-stops-2026-10-18")
	positions, finalLaps := positionsAndFinalLaps(out)
	if len(finalLaps) != 1 || finalLaps[0].FinalLap.GetLap() != 12 {
		t.Fatalf("race.final_lap %v, want one, on lap 12", finalLaps)
	}
	if id := finalLaps[0].Session + ":race.final_lap:1"; finalLaps[0].Event.ID != id {
		t.Errorf("id %s, want %s", finalLaps[0].Event.ID, id)
	}
	var stops [][2]uint32 // the entry's and the exit's frames
	for _, o := range out {
		switch {
		case o.PitEntered != nil:
			stops = append(stops, [2]uint32{o.PitEntered.GetAt(), 0})
		case o.PitExited != nil:
			stops[len(stops)-1][1] = o.PitExited.GetAt()
		}
	}
	if len(stops) != 2 {
		t.Fatalf("%d stops, want 2", len(stops))
	}
	for _, stop := range stops {
		var before, after *Output
		for i := range positions {
			p := &positions[i]
			at := p.Position.GetAt()
			switch {
			case at >= stop[0] && at <= stop[1]:
				t.Errorf("race.position %v in the pit lane, frames %d to %d", p.Position, stop[0], stop[1])
			case at < stop[0]:
				before = p
			case after == nil:
				after = p
			}
		}
		if before == nil || after == nil {
			t.Fatalf("no race.position before or after the stop at frames %d to %d", stop[0], stop[1])
		}
		if after.Position.GetPreviousPosition() != before.Position.GetPosition() {
			t.Errorf("after the stop at frames %d to %d: %v, want the change from P%d",
				stop[0], stop[1], after.Position, before.Position.GetPosition())
		}
	}
	for _, p := range positions {
		ev := p.Position
		if ev.GetSessionId() != p.Session || ev.GetRunId() == "" || ev.PreviousPosition == nil ||
			p.Event.ID != fmt.Sprintf("%s:race.position:%s:%d", p.Session, ev.GetRunId(), ev.GetAt()) {
			t.Errorf("%s %v: want the session's id, the run, a previous position and <sid>:race.position:<run>:<seq>", p.Event.ID, ev)
		}
	}
}

// AC2: the two-lap race from P10 gives race.final_lap on lap 2, and race.position
// events from the grid slot, P10, up to P7. GT7 shows P6 at the flag (race.finished
// says so), but v1's rule, and so race.position, needs a place to hold 3 s, and the
// pass came under 3 s before the line: the session ends before P6 settles.
func TestTheTwoLapRaceGoesFromP10(t *testing.T) {
	out := replay(t, "real-race-2laps-2026-10-14")
	var race string // the second session, the race
	for _, o := range out {
		if o.Started != nil {
			race = o.Session
		}
	}
	positions, finalLaps := positionsAndFinalLaps(out)
	if len(finalLaps) != 1 || finalLaps[0].Session != race || finalLaps[0].FinalLap.GetLap() != 2 {
		t.Fatalf("race.final_lap %v, want one, on lap 2 of the race", finalLaps)
	}
	var places []int32
	for i, p := range positions {
		if p.Session != race {
			t.Errorf("race.position %v outside the race", p.Position)
		}
		if i == 0 {
			places = append(places, p.Position.GetPreviousPosition())
		}
		places = append(places, p.Position.GetPosition())
	}
	if want := []int32{10, 9, 8, 7}; !slices.Equal(places, want) {
		t.Errorf("places %v, want %v", places, want)
	}
	if f := finishes(out); len(f) != 1 || f[0].Finished.GetPosition() != 6 {
		t.Errorf("race.finished %v, want P6", f)
	}
}

// AC3: Fuji's time trial with a ghost (a second car GT7 flags, but no field) gives
// no race.position and no race.final_lap.
func TestATimeTrialGivesNoPositionOrFinalLap(t *testing.T) {
	positions, finalLaps := positionsAndFinalLaps(replay(t, "real-nuc-fuji-timed-2026-10-15"))
	if len(positions) != 0 || len(finalLaps) != 0 {
		t.Errorf("%d race.position and %d race.final_lap, want none", len(positions), len(finalLaps))
	}
}

// R2, R3: a position settles once it has held 3 s, and not in the race's first
// 3 s (the grid slot, followed quietly); a frame with no position (GT7's -1 or 0)
// is passed over, a change that goes back before it settles is none, and in the
// pit lane the position is followed but not taken, settling again from the exit.
func TestAPositionSettlesAfterThreeSeconds(t *testing.T) {
	var p places
	at := func(t float64, pos int32) *state {
		return &state{t: t, phase: phaseInRace, position: pos}
	}
	for _, c := range []struct {
		t     float64
		pos   int32
		inPit bool
		want  int32 // the position settled, 0 for none
	}{
		{0, 20, false, 0},  // the first frame: P20
		{0.5, 5, false, 0}, // the grid slot settles: P5
		{2.9, 4, false, 0}, // still the first 3 s: P4 is the grid slot
		{3, -1, false, 0},  // no position
		{3, 0, false, 0},
		{4, 3, false, 0}, // P3 from 4 s
		{5, 4, false, 0}, // back to the grid slot: no change
		{6, 3, false, 0}, // P3 again from 6 s
		{8.9, 3, false, 0},
		{9, 3, false, 3}, // held 3 s
		{10, 7, true, 0}, // in the pit lane
		{20, 9, true, 0},
		{21, 9, false, 0}, // out: settling from the exit
		{24, 9, false, 9},
	} {
		previous := int32(0)
		got := int32(0)
		if prev, ok := p.observe(at(c.t, c.pos), true, c.inPit); ok {
			previous, got = prev, c.pos
		}
		if got != c.want {
			t.Fatalf("P%d at %v s (pit lane %t): settled P%d, want P%d", c.pos, c.t, c.inPit, got, c.want)
		}
		if got == 3 && previous != 4 || got == 9 && previous != 3 {
			t.Errorf("P%d settled from P%d", got, previous)
		}
	}
}

// R2: before the kind is known (a start's first frame), a field is GT7's
// cars-on-track flag and more than one car; after, the kind's field and flag.
func TestHasFieldIsV1s(t *testing.T) {
	s := &state{carsOnTrack: true, totalCars: 2}
	if !hasField(unknownKind, s) {
		t.Error("two cars flagged before the kind: want a field")
	}
	if hasField(unknownKind, &state{carsOnTrack: true, totalCars: 1}) {
		t.Error("one car before the kind: want no field")
	}
	ghost := kindVerdict{kind: eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED, field: false, carsOnTrack: true}
	if hasField(ghost, s) {
		t.Error("a time trial's ghost: want no field")
	}
	race := kindVerdict{kind: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, field: true, carsOnTrack: true}
	if !hasField(race, &state{}) {
		t.Error("a lap race flagged: want a field")
	}
}
