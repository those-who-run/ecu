package race

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

// The recordings with goldens: all of salsa's testdata/v1/. AC1 names the Suzuka
// and SUGO restarts ones; the rest are held to v1 the same way.
var goldenFixtures = []string{
	"real-nuc-suzuka-no-stop-2026-10-15",
	"real-nuc-suzuka-pit-stop-2026-10-15",
	"real-nuc-sugo-restarts-2026-10-18",
	"real-race-2laps-2026-10-14",
	"real-quali-restart-quit-2026-10-14",
	"real-nuc-willow-springs-pit-stop-2026-10-17",
	"real-nuc-sugo-two-stops-2026-10-18",
}

// roundedFixtures are recordings in v1's format 1, with values rounded to fewer
// digits than GT7's float32s carry (salsa's testdata/v1/README.md). v1 kept the
// rounded value as read; salsa reads it into a float32, as GT7 sends it, so the
// fuel levels of these are compared as float32s.
var roundedFixtures = map[string]bool{"real-race-2laps-2026-10-14": true}

// stored is one line of a golden: what v1 stored for a session's start, a lap or
// its end (testdata/golden/make_golden.py).
type stored struct {
	Event   string `json:"event"`
	Session int    `json:"session"`

	Type        string   `json:"type,omitempty"`
	CarCode     int32    `json:"car_code,omitempty"`
	TotalLaps   int32    `json:"total_laps,omitempty"`
	FuelAtStart *float64 `json:"fuel_at_start,omitempty"`
	JoinedOnLap *int32   `json:"joined_on_lap,omitempty"`

	Lap       int32    `json:"lap,omitempty"`
	LapTimeMs *int32   `json:"lap_time_ms,omitempty"`
	Complete  bool     `json:"complete,omitempty"`
	FuelStart *float64 `json:"fuel_start,omitempty"`
	FuelEnd   *float64 `json:"fuel_end,omitempty"`
	FuelUsed  *float64 `json:"fuel_used,omitempty"`
	Position  *int32   `json:"position,omitempty"`
	Frames    int      `json:"frames,omitempty"`

	LapsCompleted *int32 `json:"laps_completed,omitempty"`
	FinalPosition *int32 `json:"final_position,omitempty"`
	Finished      bool   `json:"finished,omitempty"`
	Kind          string `json:"kind,omitempty"`
}

func (s stored) String() string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// asFloat32 is s with its fuel levels at float32 precision.
func (s stored) asFloat32() stored {
	for _, f := range []**float64{&s.FuelAtStart, &s.FuelStart, &s.FuelEnd} {
		if *f != nil {
			*f = ptr(float64(float32(**f)))
		}
	}
	return s
}

func readGolden(t *testing.T, name string) []stored {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", name+".jsonl")) //nolint:gosec // a fixture's name
	if err != nil {
		t.Fatal(err)
	}
	var lines []stored
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var s stored
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		lines = append(lines, s)
	}
	return lines
}

func ptr[T any](v T) *T { return &v }

// nonZero is v, or nil for 0: v1 stored none where the events carry 0.
func nonZero(v int32) *int32 {
	if v == 0 {
		return nil
	}
	return &v
}

// v1Kinds is v1's name for each session kind (sessionkind.Kind), one to one with
// the SessionType values ecu sends.
var v1Kinds = map[eventsv1.SessionType]string{
	eventsv1.SessionType_SESSION_TYPE_UNKNOWN:     "unknown",
	eventsv1.SessionType_SESSION_TYPE_LAP_RACE:    "lap_race",
	eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS:   "solo_laps",
	eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD: "timed_field",
	eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED:  "solo_timed",
}

// frames is each frame of the outputs, by seq.
func frames(out []Output) map[uint32]*telemetryv1.Frame {
	all := map[uint32]*telemetryv1.Frame{}
	for _, o := range out {
		if o.Frame != nil {
			all[o.Frame.GetSeq()] = o.Frame
		}
	}
	return all
}

// asStored is the outputs as v1 stored them, the golden's lines. A lap's frames are
// the frames in its seq range that v1 kept, those in the race or at the flag, less
// the frames v1 read as a frozen pause (skip).
func asStored(out []Output, skip map[uint32]bool) []stored {
	all := frames(out)
	sessions := map[string]int{}
	var lines []stored
	for _, o := range out {
		switch {
		case o.Started != nil:
			sessions[o.Session] = len(sessions) + 1
			s := o.Started
			line := stored{
				Event: "session.started", Session: sessions[o.Session],
				CarCode: s.GetCarCode(), TotalLaps: s.GetTotalLaps(), FuelAtStart: ptr(s.GetFuelAtStart()),
			}
			if s.GetJoinedPartWay() {
				line.JoinedOnLap = ptr(all[s.GetFirstSeq()].GetLap())
			}
			lines = append(lines, line)
		case o.Lap != nil:
			l := o.Lap
			n := 0
			for seq := l.GetFirstSeq(); seq <= l.GetLastSeq(); seq++ {
				if p := all[seq].GetPhase(); (p == phaseInRace || p == phaseFinished) && !skip[seq] {
					n++
				}
			}
			var pos *int32
			if l.Position != nil {
				pos = ptr(l.GetPosition())
			}
			lines = append(lines, stored{
				Event: "lap.completed", Session: sessions[o.Session],
				Lap: l.GetLap(), LapTimeMs: nonZero(l.GetLapTimeMs()), Complete: l.GetComplete(),
				FuelStart: ptr(l.GetFuelAtStart()), FuelEnd: ptr(l.GetFuelAtEnd()), FuelUsed: ptr(l.GetFuelUsed()),
				Position: pos, Frames: n,
			})
		case o.Ended != nil:
			sum := o.Ended.GetSummary()
			lines = append(lines, stored{
				Event: "session.ended", Session: sessions[o.Session],
				LapsCompleted: ptr(sum.GetLapsCompleted()),
				FuelUsed:      ptr(sum.GetFuelUsed()), FinalPosition: nonZero(sum.GetFinalPosition()),
				Finished: o.Why == WhyFinished, Kind: v1Kinds[sum.GetSessionType()],
			})
		}
	}
	return lines
}

func strs(lines []stored, float32s bool) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if float32s {
			l = l.asFloat32()
		}
		out[i] = l.String()
	}
	return out
}

// AC1, and the same for every other recording: the sessions and laps v1 stored,
// and the session's kind as v1's SessionDetector last gave it (paddock#34).
//
// One field of v1's is not compared, on purpose: the start's type, v1's
// store.session_type (race, time trial or practice from GT7's cars-on-track flag).
// contracts v0.1.4 replaced it with the kind (paddock#276), which the end's kind
// holds to v1. The positions are compared as they are: v1 stored none in the
// recordings' one solo session, so leaving them out outside a field (also
// paddock#276) changes nothing here; TestNoPositionOutsideAField pins that
// difference.
func TestSessionsAndLapsEqualV1s(t *testing.T) {
	for _, name := range goldenFixtures {
		t.Run(name, func(t *testing.T) {
			all := packets(t, name)
			golden := readGolden(t, name)
			for i := range golden {
				golden[i].Type = ""
			}
			want := strs(golden, roundedFixtures[name])
			got := strs(asStored(run(all), frozen(all)), roundedFixtures[name])
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// The frozen pause is out of scope: the 45 s of it in the SUGO restarts recording
// changes no lap number or fuel (the goldens), only which frames a lap holds. If v1
// stops reading it, or the recording changes, this says so.
func TestTheFrozenPauseIsInLapTwoOfTheFirstSugoRace(t *testing.T) {
	all := packets(t, "real-nuc-sugo-restarts-2026-10-18")
	skip := frozen(all)
	if len(skip) != 96 {
		t.Errorf("%d frozen frames, want 96", len(skip))
	}
	var inLap2 int
	for _, o := range run(all) {
		if o.Lap != nil && o.Lap.GetLap() == 2 && !o.Lap.GetComplete() {
			for seq := o.Lap.GetFirstSeq(); seq <= o.Lap.GetLastSeq(); seq++ {
				if skip[seq] {
					inLap2++
				}
			}
			break
		}
	}
	if inLap2 != 95 {
		t.Errorf("%d frozen frames in the first race's lap 2, want 95", inLap2)
	}
}

// AC2: GT7 posts the pit-in lap at Suzuka East on a loading frame; the lap closes
// on the first driving frame after it, with its time.
func TestThePitInLapPostedOnALoadingFrameHasItsTime(t *testing.T) {
	out := replay(t, "real-nuc-suzuka-pit-stop-2026-10-15")
	all := frames(out)
	var seq uint32 // the frame the outputs are at
	var bridged []*eventsv1.LapCompleted
	for _, o := range out {
		if o.Frame != nil {
			seq = o.Frame.GetSeq()
			continue
		}
		if o.Lap == nil {
			continue
		}
		before := all[seq-1]
		if before.GetPhase() == phaseLoading && before.GetLap() == o.Lap.GetLap()+1 {
			bridged = append(bridged, o.Lap)
		}
	}
	if len(bridged) != 1 {
		t.Fatalf("%d laps posted on a loading frame, want 1", len(bridged))
	}
	lap := bridged[0]
	if lap.GetLap() != 3 || lap.GetLapTimeMs() != 82544 || !lap.GetComplete() {
		t.Errorf("got lap %d in %d ms, complete %t; want lap 3, complete, in 82544 ms (v1)",
			lap.GetLap(), lap.GetLapTimeMs(), lap.GetComplete())
	}
}

// sessions is the outputs grouped by session, in order.
func sessions(out []Output) [][]Output {
	var all [][]Output
	for _, o := range out {
		if o.Started != nil {
			all = append(all, nil)
		}
		if o.Session != "" {
			all[len(all)-1] = append(all[len(all)-1], o)
		}
	}
	return all
}

// AC3: the SUGO restarts, on lap 2 and then on lap 1, each end their session, and
// the next race starts a new one.
func TestRestartsEndTheirSessions(t *testing.T) {
	out := replay(t, "real-nuc-sugo-restarts-2026-10-18")
	all := frames(out)
	got := sessions(out)
	if len(got) != 3 {
		t.Fatalf("%d sessions, want 3", len(got))
	}
	for i, restartLap := range []int32{2, 1} {
		s := got[i]
		end := s[len(s)-1]
		if end.Ended == nil || end.Why == WhyFinished {
			t.Fatalf("session %d: last output %v, want an end before the flag", i+1, end.Message())
		}
		// GT7 restarts through a loading screen with the lap counter reset.
		at := all[end.Ended.GetLastSeq()]
		if at.GetPhase() != phaseLoading || at.GetLap() != 0 || all[end.Ended.GetLastSeq()-1].GetLap() != restartLap {
			t.Errorf("session %d ended on %v lap %d, after lap %d; want a loading screen at lap 0 after lap %d",
				i+1, at.GetPhase(), at.GetLap(), all[end.Ended.GetLastSeq()-1].GetLap(), restartLap)
		}
		next := got[i+1][0].Started
		if next.GetFirstSeq() <= end.Ended.GetLastSeq() {
			t.Errorf("session %d starts at %d, before session %d ended at %d", i+2, next.GetFirstSeq(), i+1, end.Ended.GetLastSeq())
		}
	}
	lapOneRestart := got[1]
	if n := lapOneRestart[len(lapOneRestart)-1].Ended.GetSummary().GetLapsCompleted(); n != 0 {
		t.Errorf("the lap-1 restart's session completed %d laps, want 0", n)
	}
	for _, o := range lapOneRestart {
		if o.Lap != nil && o.Lap.GetComplete() {
			t.Errorf("the lap-1 restart's session has a complete lap %d", o.Lap.GetLap())
		}
	}
	if last := got[2][len(got[2])-1]; last.Why != WhyFinished {
		t.Errorf("the third race ended %v, want finished", last.Why)
	}
}

// AC4: the two-lap race ends at the flag, "lap 3 of 2", as finished; the replay
// blip GT7 sends after the results, driving frames on lap 3, opens no session.
func TestATwoLapRaceEndsAtTheFlag(t *testing.T) {
	out := replay(t, "real-race-2laps-2026-10-14")
	all := frames(out)
	var race []Output
	for _, s := range sessions(out) {
		if s[0].Started.GetTotalLaps() == 2 {
			race = s
		}
	}
	if race == nil {
		t.Fatal("no two-lap race")
	}
	end := race[len(race)-1]
	if end.Ended == nil {
		t.Fatal("the race has no end")
	}
	if end.Why != WhyFinished || end.Ended.GetReason() != eventsv1.EndReason_END_REASON_FINISHED {
		t.Errorf("the race ended %v (%v), want finished", end.Why, end.Ended.GetReason())
	}
	if f := all[end.Ended.GetLastSeq()]; f.GetPhase() != phaseFinished || f.GetLap() != 3 {
		t.Errorf("the race ended on %v lap %d, want the flag on lap 3", f.GetPhase(), f.GetLap())
	}
	if p := end.Ended.GetSummary().FinishingPosition; p == nil || *p != 6 {
		t.Errorf("finishing position %v, want 6", p)
	}
	var blip int
	for seq := end.Ended.GetLastSeq() + 1; all[seq] != nil; seq++ {
		if all[seq].GetPhase() == phaseInRace {
			blip++
		}
	}
	if blip == 0 {
		t.Error("no driving frames after the results: the recording lost its replay blip")
	}
	var after bool
	for _, o := range out {
		if o.Ended == end.Ended {
			after = true
		} else if after && o.Started != nil {
			t.Errorf("a session started at %d, after the race", o.Started.GetFirstSeq())
		}
	}
}

// encoded is the outputs as they would be published: each one's headers and
// payload.
func encoded(t *testing.T, out []Output) []string {
	t.Helper()
	lines := make([]string, len(out))
	for i, o := range out {
		h, data, err := ce.Encode(o.Message(), o.Event)
		if err != nil {
			t.Fatalf("output %d: %v", i, err)
		}
		head, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		extra, err := json.Marshal([]any{o.Session, o.Why})
		if err != nil {
			t.Fatal(err)
		}
		lines[i] = string(head) + " " + string(data) + " " + string(extra)
	}
	return lines
}

// AC5: two runs over the same recording with the same ids give the same outputs,
// session ids included.
func TestTheSameInputGivesTheSameOutput(t *testing.T) {
	for _, name := range goldenFixtures[:4] {
		t.Run(name, func(t *testing.T) {
			all := packets(t, name)
			first, second := encoded(t, run(all)), encoded(t, run(all))
			if !slices.Equal(first, second) {
				for i := range min(len(first), len(second)) {
					if first[i] != second[i] {
						t.Fatalf("output %d differs:\n%s\n%s", i, first[i], second[i])
					}
				}
				t.Fatalf("%d outputs, then %d", len(first), len(second))
			}
			var ids int
			for _, o := range run(all) {
				if o.Started != nil {
					if _, err := ulid.ParseStrict(o.Session); err != nil || len(o.Session) != 26 ||
						strings.ToUpper(o.Session) != o.Session {
						t.Errorf("session id %q is not a canonical ULID: %v", o.Session, err)
					}
					ids++
				}
			}
			if ids == 0 {
				t.Error("no sessions")
			}
		})
	}
}

// started_at is the ce-time of the session's first frame, exactly: logbook finds
// the frame by its seq and ce-time (logbook#13).
func TestStartedAtIsTheFirstFramesTime(t *testing.T) {
	out := replay(t, "real-nuc-sugo-restarts-2026-10-18")
	byseq := map[uint32]Output{}
	var n int
	for _, o := range out {
		if o.Frame != nil {
			byseq[o.Frame.GetSeq()] = o
			continue
		}
		if o.Started == nil {
			continue
		}
		n++
		first, ok := byseq[o.Started.GetFirstSeq()]
		if !ok {
			t.Fatalf("session %s starts at frame %d, not given before it", o.Session, o.Started.GetFirstSeq())
		}
		if got := o.Started.GetStartedAt().AsTime(); !got.Equal(first.Event.Time) {
			t.Errorf("started_at %v, first frame's time %v", got, first.Event.Time)
		}
		fh, _, err := ce.Encode(first.Message(), first.Event)
		if err != nil {
			t.Fatal(err)
		}
		at, err := protojson.Marshal(o.Started.GetStartedAt())
		if err != nil {
			t.Fatal(err)
		}
		// Both in RFC 3339 to the nanosecond, UTC.
		if want := `"` + fh.Get(ce.TimeHeader) + `"`; string(at) != want {
			t.Errorf("started_at %s, first frame's ce-time %s", at, want)
		}
		if !o.Event.Time.Equal(first.Event.Time) {
			t.Errorf("session.started's ce-time %v, first frame's %v", o.Event.Time, first.Event.Time)
		}
	}
	if n != 3 {
		t.Errorf("%d sessions, want 3", n)
	}
}

// Every output's ids follow subjects.yaml, and every lap's frames are in its
// session.
func TestIDsAndSeqs(t *testing.T) {
	m := New(testIDs())
	var out []Output
	for _, r := range packets(t, "real-nuc-suzuka-no-stop-2026-10-15") {
		out = append(out, m.Step(r)...)
	}
	out = append(out, m.EndOfInput()...)
	var seq, first uint32
	for _, o := range out {
		if o.Event.Source != Source || o.Event.Time.IsZero() {
			t.Fatalf("output %v has source %q, time %v", o.Message(), o.Event.Source, o.Event.Time)
		}
		switch {
		case o.Frame != nil:
			seq++
			if o.Frame.GetSeq() != seq || o.Event.ID != m.Run()+":frame:"+itoa(seq) || o.Session != "" {
				t.Fatalf("frame %d: seq %d, id %q, session %q", seq, o.Frame.GetSeq(), o.Event.ID, o.Session)
			}
		case o.Started != nil:
			first = o.Started.GetFirstSeq()
			if o.Event.ID != o.Session+":session.started:1" || o.Started.GetSessionId() != o.Session || first != seq {
				t.Errorf("start %q at %d, frame %d", o.Event.ID, first, seq)
			}
		case o.Lap != nil:
			l := o.Lap
			if o.Event.ID != o.Session+":lap.completed:"+itoa(uint32(l.GetLap())) || l.GetSessionId() != o.Session { //nolint:gosec // a lap number
				t.Errorf("lap %d has id %q", l.GetLap(), o.Event.ID)
			}
			if l.GetFirstSeq() < first || l.GetLastSeq() < l.GetFirstSeq() || l.GetLastSeq() > seq {
				t.Errorf("lap %d holds %d to %d, outside %d to %d", l.GetLap(), l.GetFirstSeq(), l.GetLastSeq(), first, seq)
			}
			first = l.GetLastSeq() + 1
		case o.Ended != nil:
			if o.Event.ID != o.Session+":session.ended:1" || o.Ended.GetSessionId() != o.Session || o.Ended.GetLastSeq() != seq {
				t.Errorf("end %q at %d, frame %d", o.Event.ID, o.Ended.GetLastSeq(), seq)
			}
		}
	}
	if seq != 884 {
		t.Errorf("%d frames, want 884", seq)
	}
}

func itoa(n uint32) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// A recording that stops mid-race ends its session as the input ended, on its last
// frame; ecu stopping at the same point ends it as stopped. v1 told the two apart
// only by finished being false: the end reasons are new in contracts v0.1.4
// (paddock#276), a deliberate difference from v1.
func TestTheEndOfTheInputAndStoppingEndTheSession(t *testing.T) {
	all := packets(t, "real-nuc-willow-springs-pit-stop-2026-10-17")
	for _, c := range []struct {
		close  func(*Machine) []Output
		why    Why
		reason eventsv1.EndReason
	}{
		{(*Machine).EndOfInput, WhyInputEnded, eventsv1.EndReason_END_REASON_INPUT_ENDED},
		{(*Machine).Stop, WhyStopped, eventsv1.EndReason_END_REASON_ENGINEER_STOPPED},
	} {
		m := New(testIDs())
		for _, r := range all {
			m.Step(r)
		}
		out := c.close(m)
		end := out[len(out)-1]
		if end.Ended == nil || end.Why != c.why || end.Ended.GetReason() != c.reason {
			t.Fatalf("last output %v (%v), want the session ended %v", end.Message(), end.Why, c.reason)
		}
		if end.Ended.GetLastSeq() != uint32(len(all)) { //nolint:gosec // a recording's frame count fits
			t.Errorf("ended at %d, want the last frame, %d", end.Ended.GetLastSeq(), len(all))
		}
		if end.Ended.GetSummary().FinishingPosition != nil {
			t.Error("a session ended before the flag has a finishing position")
		}
	}
}

// contracts v0.2.0 tells a quit through a loading screen at lap 0 (QUIT) from
// going straight back to a menu or the track screen (MENU); before it, both were
// QUIT. A timed session's end at the results stays QUIT until ecu reads the timed
// finish (paddock#85, END_REASON_TIMED_FINISH).
func TestAQuitAndBackToTheMenuAreToldApart(t *testing.T) {
	for _, c := range []struct {
		name string
		want []Why
	}{
		{"real-quali-restart-quit-2026-10-14", []Why{WhyRestart, WhyQuit}},
		{"real-nuc-spa-pit-cut-quit-2026-10-18", []Why{WhyQuit}},
		{"real-nuc-tokyo-east-race-2026-10-15", []Why{WhyMenu}},
		{"real-nuc-tsukuba-brz-drift-2026-10-15", []Why{WhyMenu}},
		{"real-nuc-spa-time-trial-2026-10-18", []Why{WhyQuit}},
		{"real-race-2laps-2026-10-14", []Why{WhyQuit, WhyFinished}},
	} {
		var got []Why
		for _, o := range replay(t, c.name) {
			if o.Ended != nil {
				got = append(got, o.Why)
				if o.Ended.GetReason() != o.Why.Reason() {
					t.Errorf("%s: reason %v for %v", c.name, o.Ended.GetReason(), o.Why)
				}
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: ends %v, want %v", c.name, got, c.want)
		}
	}
}

// Every event id is unique across a replay, events of every kind: EVENTS drops a
// second event with an id it holds as a duplicate. Two changes of the track on
// one frame are one session.track (seven of v1's recordings have them).
func TestEveryEventIDIsUnique(t *testing.T) {
	for _, name := range v1Recordings {
		seen := map[string]bool{}
		for _, o := range replay(t, name) {
			if o.Frame != nil {
				continue
			}
			if seen[o.Event.ID] {
				t.Errorf("%s: %s published twice", name, o.Event.ID)
			}
			seen[o.Event.ID] = true
		}
	}
}
