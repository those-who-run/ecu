package race

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/salsa"
)

// racing is a racing frame's state with a lap total, a car count and a position.
func racing(laps, cars, position int32) *state {
	return &state{phase: phaseInRace, lap: 1, totalLaps: laps, totalCars: cars, position: position}
}

// v1's tests of the kind (tests/test_sessionkind.py at v1.0.0), ported: the same
// frames give the same verdicts.
func TestKindFromTheLapTotalAndTheCars(t *testing.T) {
	for _, c := range []struct {
		laps, cars int32
		want       eventsv1.SessionType
	}{
		{3, 16, eventsv1.SessionType_SESSION_TYPE_LAP_RACE},
		{6, 2, eventsv1.SessionType_SESSION_TYPE_LAP_RACE}, // one AI opponent, Suzuka East
		{4, 1, eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS},
		{0, 20, eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD},
		{0, 1, eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED},
	} {
		var k kinds
		k.observe(racing(c.laps, c.cars, 1))
		if got := k.kind(); got != c.want {
			t.Errorf("%d laps, %d cars: %v, want %v", c.laps, c.cars, got, c.want)
		}
	}
}

func TestAGhostIsNotAFieldButBeingPassedIs(t *testing.T) {
	var k kinds
	k.observe(racing(0, 1, 1))
	k.observe(racing(0, 2, 1)) // a time trial's ghost
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED {
		t.Errorf("with a ghost: %v, want solo timed", got)
	}
	k.observe(racing(0, 2, 2))
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD {
		t.Errorf("passed: %v, want a timed field", got)
	}
}

func TestMinusOneCarsSaysNothingAndALateLapTotalCounts(t *testing.T) {
	var k kinds
	k.observe(racing(3, 12, 0))
	k.observe(racing(0, -1, -1))
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_LAP_RACE || k.laps != 3 {
		t.Errorf("after -1 cars: %v with %d laps, want a lap race of 3", got, k.laps)
	}

	k = kinds{}
	k.observe(racing(0, -1, -1))
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED {
		t.Errorf("a time trial, -1 all the way: %v, want solo timed", got)
	}
	k.observe(racing(2, 20, 0))
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_LAP_RACE {
		t.Errorf("the lap total posted late: %v, want a lap race", got)
	}
}

func TestOnlyRacingFramesCount(t *testing.T) {
	var k kinds
	k.observe(&state{phase: phaseAtTrack, totalLaps: 3, totalCars: 16})
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_UNKNOWN {
		t.Errorf("no racing frame: %v, want unknown", got)
	}
}

// The start carries the first frame's verdict and the summary the final one, and
// a lap's position follows the verdict when the lap ends (contracts v0.1.4). A
// ghost, then being passed: solo timed at the start, a timed field by the end.
func TestTheStartHasTheFirstVerdictAndTheEndTheFinalOne(t *testing.T) {
	d := drive{timed: true, cars: 2}
	d.add(0, 0, 50, -1, 1)    // 1: the track screen
	d.add(1, 0, 50, -1, 1)    // 2: the start, first with a ghost
	d.add(2, 0, 49, 60000, 1) // 3: lap 1, still first
	d.add(2, 0, 49, 60000, 2) // 4: passed: a field
	d.add(3, 0, 48, 61000, 2) // 5: lap 2
	d.add(0, 0, 48, 61000, 2) // 6: back to the track screen
	evs := events(run(d.all))
	if len(evs) != 6 || evs[3].Best == nil {
		t.Fatalf("%d events, want start, laps 1 and 2, lap 2's best, lap 3 cut short, end", len(evs))
	}
	if got := evs[0].Started.GetSessionType(); got != eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED {
		t.Errorf("start: %v, want the first frame's verdict, solo timed", got)
	}
	if lap := evs[1].Lap; lap.Position != nil {
		t.Errorf("lap 1, ended solo: position %d, want none", lap.GetPosition())
	}
	if lap := evs[2].Lap; lap.Position == nil || lap.GetPosition() != 2 {
		t.Errorf("lap 2, ended in a field: position %v, want 2", lap.Position)
	}
	sum := evs[5].Ended.GetSummary()
	if sum.GetSessionType() != eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD || sum.GetFinalPosition() != 2 ||
		sum.FinishingPosition != nil {
		t.Errorf("end: %v, want a timed field, final position 2 and no finishing position", sum)
	}
}

// A lap total GT7 posts after the start makes a lap race of what began as a timed
// field, and a lap race at the flag has a finishing position by the final verdict.
func TestALapTotalPostedLateMakesALapRace(t *testing.T) {
	var d drive
	d.add(0, 0, 50, -1, 5) // 1: the track screen
	d.add(1, 0, 50, -1, 5) // 2: the start, before the lap total
	d.all[1].TotalLaps = 0
	d.add(1, 0, 49, -1, 4)
	for lap := int16(2); lap <= 6; lap++ { // laps 1 to 5, then the flag on "lap 6 of 5"
		d.add(lap, 0, 49, 60000, 3)
	}
	evs := events(run(d.all))
	if got := evs[0].Started.GetSessionType(); got != eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD {
		t.Errorf("start: %v, want a timed field", got)
	}
	end := evs[len(evs)-1]
	sum := end.Ended.GetSummary()
	if end.Why != WhyFinished || sum.GetSessionType() != eventsv1.SessionType_SESSION_TYPE_LAP_RACE ||
		sum.FinishingPosition == nil || sum.GetFinishingPosition() != 3 {
		t.Errorf("end: %v %v, want a lap race finished in P3", end.Why, sum)
	}
}

// Outside a field there is no position: GT7 reports 1 in solo laps, and v1 stored
// it for each lap and as the final position. Leaving it out is a deliberate
// difference from v1 (contracts v0.1.4, paddock#276), as is no finishing position
// for solo laps at the flag, where v1 stored 1.
func TestNoPositionOutsideAField(t *testing.T) {
	for _, c := range []struct {
		name  string
		timed bool
		want  eventsv1.SessionType
	}{
		{"solo laps", false, eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS},
		{"solo timed", true, eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED},
	} {
		d := drive{timed: c.timed, cars: 1}
		d.add(0, 0, 50, -1, 1)
		for lap := int16(1); lap <= 6; lap++ { // a five-lap run to the flag, or six timed laps
			d.add(lap, 0, 50, 60000, 1)
		}
		d.add(0, salsa.FlagLoading, 50, 60000, 1)
		evs := events(run(d.all))
		if got := evs[0].Started.GetSessionType(); got != c.want {
			t.Errorf("%s: start %v", c.name, got)
		}
		var laps int
		for _, o := range evs {
			if o.Lap != nil {
				laps++
				if o.Lap.Position != nil {
					t.Errorf("%s: lap %d has position %d, want none (v1 stored 1)", c.name, o.Lap.GetLap(), o.Lap.GetPosition())
				}
			}
		}
		end := evs[len(evs)-1]
		sum := end.Ended.GetSummary()
		if laps == 0 || sum.GetSessionType() != c.want || sum.FinalPosition != nil || sum.FinishingPosition != nil {
			t.Errorf("%s: %d laps, end %v %v, want no final or finishing position (v1 stored 1)", c.name, laps, end.Why, sum)
		}
		if !c.timed && end.Why != WhyFinished {
			t.Errorf("%s: ended %v, want at the flag", c.name, end.Why)
		}
	}
}

// A race against one AI car, then the frames GT7 replays around the results with
// -1 cars: the -1 says nothing, so it stays a lap race (Suzuka East, 2026-10-15). If
// the -1 counted, the two cars would be forgotten and it would read as solo laps,
// losing its final position.
func TestATwoCarRaceStaysOneThroughMinusOneCars(t *testing.T) {
	var k kinds
	k.observe(racing(6, 2, 1))
	k.observe(racing(6, -1, -1))
	if got := k.kind(); got != eventsv1.SessionType_SESSION_TYPE_LAP_RACE {
		t.Errorf("after -1 cars: %v, want a lap race", got)
	}

	d := drive{cars: 2}
	d.add(0, 0, 50, -1, 1)
	for lap := int16(1); lap <= 5; lap++ {
		d.add(lap, 0, 50, 60000, 1)
	}
	for range 3 { // the results: -1 cars, no position
		d.add(5, 0, 50, 60000, -1)
		d.all[len(d.all)-1].TotalCars = -1
	}
	d.add(0, 0, 50, 60000, -1)
	evs := events(run(d.all))
	sum := evs[len(evs)-1].Ended.GetSummary()
	if sum.GetSessionType() != eventsv1.SessionType_SESSION_TYPE_LAP_RACE || sum.GetFinalPosition() != 1 {
		t.Errorf("end: %v, want a lap race in P1", sum)
	}
}

// Each start resets the kind, as v1's Engine does: a time trial after a lap race
// is solo timed, not a lap race that kept the race's lap total and field.
func TestEachStartJudgesTheKindAfresh(t *testing.T) {
	var d drive
	d.add(0, 0, 50, -1, 5)
	for lap := int16(1); lap <= 6; lap++ { // a five-lap race to the flag
		d.add(lap, 0, 50, 60000, 5)
	}
	race := len(d.all)
	d.add(-1, 0, 50, 60000, -1) // the menu
	d.add(0, 0, 50, 60000, -1)  // the track screen
	d.add(1, 0, 50, 60000, -1)  // a time trial starts
	d.add(1, 0, 50, 60000, -1)
	for i := range d.all[race:] {
		d.all[race+i].TotalLaps, d.all[race+i].TotalCars = 0, -1
	}
	var starts []eventsv1.SessionType
	for _, o := range events(run(d.all)) {
		if o.Started != nil {
			starts = append(starts, o.Started.GetSessionType())
		}
	}
	want := []eventsv1.SessionType{eventsv1.SessionType_SESSION_TYPE_LAP_RACE, eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED}
	if len(starts) != 2 || starts[0] != want[0] || starts[1] != want[1] {
		t.Errorf("starts %v, want %v", starts, want)
	}
}

// kindLine is a line of a kind golden (testdata/kind/make_golden.py): a session
// opened, its kind changed, or it ended with the kind it stored.
type kindLine struct {
	Event   string      `json:"event"`
	Session int         `json:"session"`
	Frame   uint32      `json:"frame"`
	Kind    *goldenKind `json:"kind"`
}

// goldenKind is v1's SessionKind.record(), as summary.json stores it.
type goldenKind struct {
	Kind        string `json:"kind"`
	TotalLaps   int32  `json:"total_laps"`
	Field       bool   `json:"field"`
	FuelUse     *bool  `json:"fuel_use"`
	CarsOnTrack bool   `json:"cars_on_track"`
	Start       string `json:"start"`
	Evidence    string `json:"evidence"`
}

// show is a line as the test compares it. The end's frame is left out: v1 counted
// the frame it was on when it ended a session, ecu gives the session's last.
func (l kindLine) show() string {
	if l.Event == "session.started" {
		return fmt.Sprintf("%s %d frame %d", l.Event, l.Session, l.Frame)
	}
	frame := fmt.Sprintf(" frame %d", l.Frame)
	if l.Event == "session.ended" {
		frame = ""
	}
	k := "no kind"
	if g := l.Kind; g != nil {
		fuel := "null"
		if g.FuelUse != nil {
			fuel = strconv.FormatBool(*g.FuelUse)
		}
		k = fmt.Sprintf("%s laps %d field %t fuel %s cars_on_track %t start %s %q",
			g.Kind, g.TotalLaps, g.Field, fuel, g.CarsOnTrack, g.Start, g.Evidence)
	}
	return fmt.Sprintf("%s %d%s %s", l.Event, l.Session, frame, k)
}

var (
	kindNames = map[eventsv1.SessionType]string{
		eventsv1.SessionType_SESSION_TYPE_UNKNOWN:     "unknown",
		eventsv1.SessionType_SESSION_TYPE_LAP_RACE:    "lap_race",
		eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS:   "solo_laps",
		eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD: "timed_field",
		eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED:  "solo_timed",
	}
	startNames = map[eventsv1.StartType]string{
		eventsv1.StartType_START_TYPE_UNKNOWN: "unknown",
		eventsv1.StartType_START_TYPE_GRID:    "grid",
		eventsv1.StartType_START_TYPE_ROLLING: "rolling",
		eventsv1.StartType_START_TYPE_FLYING:  "flying",
	}
)

// asGoldenKind is a published kind as a golden's.
func asGoldenKind(k *eventsv1.SessionKind) *goldenKind {
	if k == nil {
		return nil
	}
	g := &goldenKind{
		Kind: kindNames[k.GetSessionType()], TotalLaps: k.GetTotalLaps(), Field: k.GetField(),
		CarsOnTrack: k.GetCarsOnTrack(), Start: startNames[k.GetStart()], Evidence: k.GetEvidence(),
	}
	switch k.GetFuelUse() {
	case eventsv1.FuelUse_FUEL_USE_ON:
		g.FuelUse = new(true)
	case eventsv1.FuelUse_FUEL_USE_OFF:
		g.FuelUse = new(false)
	}
	return g
}

// kindLines are the outputs as a golden's lines: each start, each published kind
// and each end with its summary's kind.
func kindLines(out []Output) []kindLine {
	var lines []kindLine
	n := 0
	for _, o := range out {
		switch {
		case o.Started != nil:
			n++
			lines = append(lines, kindLine{Event: "session.started", Session: n, Frame: o.Started.GetFirstSeq()})
		case o.Kind != nil:
			lines = append(lines, kindLine{Event: "session.kind", Session: n, Frame: o.Kind.GetAt(), Kind: asGoldenKind(o.Kind.GetKind())})
		case o.Ended != nil:
			lines = append(lines, kindLine{Event: "session.ended", Session: n, Kind: asGoldenKind(o.Ended.GetSummary().GetKind())})
		}
	}
	return lines
}

func readKindGolden(t *testing.T, name string) []kindLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "kind", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var lines []kindLine
	for line := range strings.Lines(string(data)) {
		var l kindLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	return lines
}

func showKinds(lines []kindLine) []string {
	s := make([]string, len(lines))
	for i, l := range lines {
		s[i] = l.show()
	}
	return s
}

// kindDepartures are ecu's named departures from v1's kind, by recording: each
// replaces what v1 said (the golden's text) with what ecu says.
//
// A grid behind the line (paddock#148): v1 read the start from the first 18
// packets of racing alone, so a car that stood on the grid, launched and crossed
// the line at speed read as a rolling start. SUGO's 12-lap race of 2026-10-18 is
// one: the car stands in slot 10 at 0 km/h for 6 s on the track screen, launches,
// and starts lap 1 at 113 km/h 4.3 s later, as the grid start of 2026-10-31 does
// (TestAGridBehindTheLineIsAGridStart). Its FIXTURES.yaml entry says rolling
// because v1 said so.
var kindDepartures = map[string][][2]string{
	"real-nuc-sugo-two-stops-2026-10-18": {{
		`start rolling "12 laps, 20 cars; fuel use on; rolling start at 129 km/h"`,
		`start grid "12 laps, 20 cars; fuel use on; grid start at 129 km/h, after standing 6 s"`,
	}},
	"real-nuc-suzuka-east-grid-start-2026-10-31": {{
		`start rolling "5 laps, 20 cars; fuel use on; rolling start at 101 km/h"`,
		`start grid "5 laps, 20 cars; fuel use on; grid start at 101 km/h, after standing 6 s"`,
	}},
}

// asEcu is a recording's golden lines with ecu's departures (kindDepartures)
// made.
func asEcu(name string, want []string) []string {
	out := make([]string, len(want))
	for i, l := range want {
		for _, d := range kindDepartures[name] {
			l = strings.ReplaceAll(l, d[0], d[1])
		}
		out[i] = l
	}
	return out
}

// R1, R2, AC3: on every one of v1's recordings, ecu publishes session.kind where
// v1's engine applied a new kind to the session (v1's "session ..." log line), with
// v1's verdict and its evidence word for word, and each session.ended's summary
// keeps the kind v1 stored for the session, but for ecu's named departures
// (kindDepartures). On the replay and the demos v1's start check drops
// (paddock#74, discards) there is no session, and no kind.
func TestSessionKindIsV1sOnEveryRecording(t *testing.T) {
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			got, want := showKinds(kindLines(replay(t, name))), asEcu(name, showKinds(readKindGolden(t, name)))
			if _, ok := discards[name]; len(want) == 0 && !ok {
				t.Fatal("an empty golden")
			}
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// AC1: the last session.kind of each session is the kind and start v1's
// tests/fixtures/FIXTURES.yaml names for it, none for the four demos of 2026-10-15
// that v1's start check drops (paddock#74), but for the start of a session joined part way: the start
// was not seen, so it is unknown (R4), as v1's engine stores it, where
// FIXTURES.yaml names the start v1's detector reads fed from the recording's
// first frame for three of them (2laps' first session, the Willow Springs and
// Gilles Villeneuve pit stops), and for SUGO's two stops, a grid behind the line
// v1 read as rolling (kindDepartures, paddock#148).
func TestTheLastKindOfEachSessionIsFixturesYAMLs(t *testing.T) {
	for name, want := range map[string][]string{
		"real-race-2laps-2026-10-14":                     {"solo_timed (start not seen)", "lap_race (rolling)"},
		"real-quali-restart-quit-2026-10-14":             {"timed_field (flying)", "timed_field (flying)"},
		"real-n24h-race-lap1-positions-2026-10-14":       {"lap_race (rolling)"},
		"real-nuc-unknown-track-2026-10-15":              {"solo_timed (flying)"},
		"real-nuc-suzuka-east-2026-10-15":                {"solo_laps (flying)"},
		"real-nuc-suzuka-no-stop-2026-10-15":             {"lap_race (rolling)"},
		"real-nuc-suzuka-pit-stop-2026-10-15":            {"lap_race (rolling)"},
		"real-nuc-sugo-timed-2026-10-15":                 {"solo_timed (flying)"},
		"real-nuc-fuji-timed-2026-10-15":                 {"solo_timed (flying)"},
		"real-nuc-tokyo-east-race-2026-10-15":            nil, // a demo (paddock#74)
		"real-nuc-nordschleife-rolling-start-2026-10-15": nil, // a demo (paddock#74)
		"real-nuc-laguna-seca-race-2026-10-15":           nil, // a demo (paddock#74)
		"real-nuc-tsukuba-brz-drift-2026-10-15":          nil, // a demo (paddock#74)
		"real-nuc-suzuka-timed-race-2026-10-16":          {"timed_field (flying)"},
		"real-nuc-willow-springs-pit-stop-2026-10-17":    {"lap_race (start not seen)"},
		"real-nuc-gilles-villeneuve-race-2026-10-17":     {"lap_race (rolling)"},
		"real-nuc-gilles-villeneuve-pit-stop-2026-10-17": {"lap_race (start not seen)"},
		"real-nuc-sugo-two-stops-2026-10-18":             {"lap_race (grid)"}, // FIXTURES.yaml: rolling
		"real-nuc-sugo-restarts-2026-10-18":              {"lap_race (rolling)", "lap_race (rolling)", "lap_race (rolling)"},
		"real-nuc-suzuka-joined-mid-race-2026-10-18":     {"timed_field (start not seen)"},
		"real-nuc-suzuka-before-restart-2026-10-18":      {"timed_field (flying)"},
		"real-nuc-spa-pit-cut-quit-2026-10-18":           {"timed_field (flying)"},
		"real-nuc-spa-time-trial-2026-10-18":             {"solo_timed (flying)"},
	} {
		var got []string
		joined := false
		var last *eventsv1.SessionKind
		for _, o := range replay(t, name) {
			switch {
			case o.Started != nil:
				joined, last = o.Started.GetJoinedPartWay(), nil
			case o.Kind != nil:
				last = o.Kind.GetKind()
			case o.Ended != nil:
				if !proto.Equal(last, o.Ended.GetSummary().GetKind()) {
					t.Errorf("%s: the summary's kind %v, want the last session.kind %v", name, o.Ended.GetSummary().GetKind(), last)
				}
				start := startNames[last.GetStart()]
				if joined {
					if start != "unknown" {
						t.Errorf("%s: joined part way, the start is %s, want unknown", name, start)
					}
					start = "start not seen"
				}
				got = append(got, kindNames[last.GetSessionType()]+" ("+start+")")
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

// AC2: Fuji's time trial is solo timed with no field throughout, despite the
// ghost.
func TestFujiIsSoloTimedDespiteTheGhost(t *testing.T) {
	var n int
	for _, o := range replay(t, "real-nuc-fuji-timed-2026-10-15") {
		if o.Kind == nil {
			continue
		}
		n++
		if k := o.Kind.GetKind(); k.GetSessionType() != eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED || k.GetField() {
			t.Errorf("frame %d: %v, want solo timed with no field", o.Kind.GetAt(), k)
		}
	}
	if n == 0 {
		t.Error("no session.kind")
	}
}

// R1: v1's rule, made up, as no grid ahead of the line has been recorded: the car
// under 30 km/h through the first 18 packets is a grid start; over it, rolling in
// a lap race. A change of the evidence alone (the cars and the speed it names) is not a
// change of the decision. With no tank, fuel use is off at once.
func TestTheStartAndWhatCountsAsAChange(t *testing.T) {
	for _, c := range []struct {
		kph  float64
		want eventsv1.StartType
	}{
		{0, eventsv1.StartType_START_TYPE_GRID},
		{29, eventsv1.StartType_START_TYPE_GRID},
		{30, eventsv1.StartType_START_TYPE_ROLLING},
	} {
		var k kinds
		var changes int
		for p := range int32(startWindow + 5) {
			s := racing(5, 16, 3)
			s.packetID, s.t, s.speedKph = 1000+p, float64(p)/60, min(c.kph, float64(p)*5)
			if k.observe(s) {
				changes++
			}
		}
		v := k.verdict()
		if v.start != c.want || v.fuelUse != eventsv1.FuelUse_FUEL_USE_OFF || changes != 2 {
			t.Errorf("%v km/h: %v, %d changes; want %v, fuel use off at once, then the start", c.kph, v, changes, c.want)
		}
	}

	var k kinds
	k.observe(racing(5, 16, 3))
	evidence := k.verdict().evidence
	if k.observe(racing(5, 15, 3)) || k.verdict().evidence == evidence {
		t.Errorf("one car fewer: the evidence %q, and published; want new evidence, not published", k.verdict().evidence)
	}
}

// R4: a session joined part way leaves the start unknown, whatever the speed.
func TestAJoinedSessionsStartIsUnknown(t *testing.T) {
	var k kinds
	k.join()
	for p := range int32(startWindow + 1) {
		s := racing(5, 16, 3)
		s.packetID, s.speedKph = p, 120
		k.observe(s)
	}
	if v := k.verdict(); v.start != eventsv1.StartType_START_TYPE_UNKNOWN || !strings.Contains(v.evidence, "joined part way") {
		t.Errorf("joined: %v, want the start unknown", v)
	}
}

// R2, R3: session.kind carries the run, the frame and the session, with the
// catalogue's id <sid>:session.kind:<run>:<seq> (the publisher puts <src>: in
// front), at most one a frame for a session, and field true for a lap race and a
// timed field only.
func TestSessionKindCarriesTheRunAndTheFrame(t *testing.T) {
	var n int
	for _, name := range v1Recordings {
		m := New(testIDs())
		perFrame := map[string]bool{}
		for _, r := range packets(t, name) {
			for _, o := range m.Step(r) {
				k := o.Kind
				if k == nil {
					continue
				}
				n++
				at := strconv.FormatUint(uint64(k.GetAt()), 10)
				if k.GetRunId() != m.Run() || k.GetSessionId() != o.Session ||
					o.Event.ID != o.Session+":session.kind:"+m.Run()+":"+at || o.Event.Source != Source {
					t.Errorf("%s: %s %v", name, o.Event.ID, k)
				}
				if perFrame[o.Session+at] {
					t.Errorf("%s: two session.kind on frame %s", name, at)
				}
				perFrame[o.Session+at] = true
				st := k.GetKind().GetSessionType()
				if field := st == eventsv1.SessionType_SESSION_TYPE_LAP_RACE || st == eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD; k.GetKind().GetField() != field {
					t.Errorf("%s: frame %s: field %t for %v", name, at, k.GetKind().GetField(), st)
				}
			}
		}
	}
	if n == 0 {
		t.Error("no session.kind")
	}
}

// paddock#146: GT7 flags cars on track in a time trial too, so the flag says
// nothing about a field. On each of v1's four time trials every racing frame is
// flagged, and the session still starts, runs and ends solo timed, with no field
// and no position on any lap: the type is the kind's, never the flag's.
func TestATimeTrialIsSoloTimedThoughGT7FlagsCarsOnTrack(t *testing.T) {
	for _, name := range []string{
		"real-nuc-sugo-timed-2026-10-15",
		"real-nuc-fuji-timed-2026-10-15",
		"real-nuc-spa-time-trial-2026-10-18",
		"real-nuc-unknown-track-2026-10-15",
	} {
		t.Run(name, func(t *testing.T) {
			racing := 0
			for i, r := range packets(t, name) {
				if s := newState(uint32(i+1), r.T, &r.Packet); s.inRace() { //nolint:gosec // a recording's frame count
					if !s.carsOnTrack {
						t.Fatalf("frame %d racing without the flag", i+1)
					}
					racing++
				}
			}
			if racing == 0 {
				t.Fatal("no racing frame")
			}
			solo := eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED
			var starts int
			for _, o := range replay(t, name) {
				switch {
				case o.Started != nil:
					starts++
					if got := o.Started.GetSessionType(); got != solo {
						t.Errorf("session.started: %v, want solo timed", got)
					}
				case o.Kind != nil:
					if k := o.Kind.GetKind(); k.GetSessionType() != solo || k.GetField() || !k.GetCarsOnTrack() {
						t.Errorf("session.kind: %v, want solo timed, no field, flagged", k)
					}
				case o.Lap != nil && o.Lap.Position != nil:
					t.Errorf("lap %d: position %d, want none", o.Lap.GetLap(), o.Lap.GetPosition())
				case o.Ended != nil:
					if got := o.Ended.GetSummary().GetSessionType(); got != solo {
						t.Errorf("session.ended: %v, want solo timed", got)
					}
				}
			}
			if starts != 1 {
				t.Errorf("%d sessions, want 1", starts)
			}
		})
	}
}

// paddock#146, #145: the practice at Suzuka on 2026-10-31 (testdata/sessions),
// a custom race's practice with the AI field out on track, reads as v1.0.0 reads
// it, a timed field with a flying start and no fuel use, as its v1 golden says.
// That is not the cars-on-track flag: GT7 reports 16 cars and no lap total, and
// ranks the driver by lap time, P1 on the first racing frame and P2 to P4 as the
// AI post their times, which is what qualifying sends too
// (real-quali-restart-quit-2026-10-14). GT7 sends no mode, so the kind cannot
// tell this practice from qualifying; contracts' SESSION_TYPE_SOLO_TIMED, which
// names practice, assumed practice was driven alone. Telling them apart needs a
// decision on the session types, so this pins what ecu does until then.
func TestAPracticeWithTheFieldOutReadsAsATimedFieldAsV1s(t *testing.T) {
	const name = "real-nuc-suzuka-practice-2026-10-31"
	got, want := showKinds(kindLines(replay(t, name))), showKinds(readKindGolden(t, name))
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(want) == 0 || !strings.HasPrefix(want[len(want)-1], "session.ended 1 timed_field laps 0 field true fuel false") ||
		!strings.Contains(want[len(want)-1], "start flying") {
		t.Errorf("the golden's verdict %q, want a timed field, fuel off, flying", want)
	}
}

// paddock#148: GT7's "Grid Start" at Suzuka East on 2026-10-31
// (testdata/sessions), 5 laps against 19 AI cars. The car stands in slot 10 on
// the track screen for 6 s, launches, and starts lap 1 at 97 km/h 4.7 s later:
// the grid is behind the line. v1.0.0 reads it as a rolling start at 101 km/h
// (its golden), and so did ecu 0.1.13 live; ecu now reads a grid start, on the
// same frame, with nothing else changed (kindDepartures).
func TestAGridBehindTheLineIsAGridStart(t *testing.T) {
	const name = "real-nuc-suzuka-east-grid-start-2026-10-31"
	v1 := showKinds(readKindGolden(t, name))
	if len(v1) == 0 || !strings.Contains(v1[len(v1)-1], "start rolling") {
		t.Fatalf("v1's golden %q, want v1's rolling start", v1)
	}
	got, want := showKinds(kindLines(replay(t, name))), asEcu(name, v1)
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(want[len(want)-1], "start grid") {
		t.Errorf("the verdict %q, want a grid start", want[len(want)-1])
	}
}

// at is a frame at GT7's packet p (60 a second) going kph: on the track screen,
// or racing on lap 1.
func at(phase telemetryv1.Phase, p int32, kph float64) *state {
	s := &state{phase: phase, packetID: p, speedKph: kph}
	if phase == phaseInRace {
		s.lap = 1
	}
	return s
}

// paddock#148: the grid is read from the track screen before the start, by
// GT7's packets: a stand of 4 s or more, then lap 1 within 10 s of moving off.
func TestTheGridIsALongStandThenAQuickLaunch(t *testing.T) {
	// screen is a track screen, thinned to a frame every 0.5 s: standing for
	// stand seconds, then moving for launch seconds; what lap 1 reads.
	screen := func(stand, launch float64) float64 {
		var g grid
		p := int32(1000)
		for i := range int(stand*2) + 1 {
			p = 1000 + int32(i*30)
			g.observe(at(phaseAtTrack, p, 0))
		}
		end := p + int32(launch*60)
		for q := p + 30; q < end; q += 30 {
			g.observe(at(phaseAtTrack, q, 50))
		}
		return g.observe(at(phaseInRace, end, 97))
	}
	for _, c := range []struct {
		name          string
		stand, launch float64
		want          float64
	}{
		{"the grid of 2026-10-31", 6, 4.5, 6},
		{"the grid at its limits", 4, 10, 4},
		{"too short a stand: the pit box", 3, 4.5, 0},
		{"too long a launch: the pit lane", 6, 15, 0},
	} {
		if got := screen(c.stand, c.launch); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	var g grid
	for p := range int32(10) { // a rolling start never stands
		g.observe(at(phaseAtTrack, p*30, 80))
	}
	if got := g.observe(at(phaseInRace, 300, 120)); got != 0 {
		t.Errorf("rolling: %v, want none", got)
	}

	// A loading screen between the stand and the start forgets it, and a restart,
	// racing to racing, has no track screen.
	g = grid{}
	for p := range int32(13) {
		g.observe(at(phaseAtTrack, p*30, 0))
	}
	g.observe(at(phaseLoading, 400, 0))
	if got := g.observe(at(phaseInRace, 430, 90)); got != 0 {
		t.Errorf("after a loading screen: %v, want none", got)
	}
	if got := g.observe(at(phaseInRace, 460, 0)); got != 0 {
		t.Errorf("racing to racing: %v, want none", got)
	}

	// The last standstill decides: a long stand, a roll, then a short stop is
	// not a grid.
	g = grid{}
	for p := range int32(13) {
		g.observe(at(phaseAtTrack, p*30, 0))
	}
	g.observe(at(phaseAtTrack, 500, 40))
	g.observe(at(phaseAtTrack, 530, 0))
	g.observe(at(phaseAtTrack, 560, 0))
	if got := g.observe(at(phaseInRace, 700, 90)); got != 0 {
		t.Errorf("the last standstill short: %v, want none", got)
	}

	// The kind names it: a grid start at the window's speed, and how long the car
	// stood.
	k := kinds{stood: 6.2}
	for p := range int32(startWindow + 1) {
		s := racing(5, 20, 10)
		s.packetID, s.speedKph = p, 97+float64(p)/6
		k.observe(s)
	}
	if v := k.verdict(); v.start != eventsv1.StartType_START_TYPE_GRID ||
		!strings.HasSuffix(v.evidence, "; grid start at 100 km/h, after standing 6 s") {
		t.Errorf("stood 6.2 s: %v %q, want a grid start", v.start, v.evidence)
	}
}
