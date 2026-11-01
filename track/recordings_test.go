package track

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

// v1Recordings are the recordings v1's tests/fixtures/FIXTURES.yaml lists at v1.0.0.
var v1Recordings = []string{
	"real-race-2laps-2026-10-14",
	"real-quali-restart-quit-2026-10-14",
	"real-n24h-race-lap1-positions-2026-10-14",
	"real-nuc-unknown-track-2026-10-15",
	"real-nuc-suzuka-east-2026-10-15",
	"real-nuc-suzuka-no-stop-2026-10-15",
	"real-nuc-suzuka-pit-stop-2026-10-15",
	"real-nuc-sugo-timed-2026-10-15",
	"real-nuc-fuji-timed-2026-10-15",
	"real-nuc-tokyo-east-race-2026-10-15",
	"real-nuc-nordschleife-rolling-start-2026-10-15",
	"real-nuc-laguna-seca-race-2026-10-15",
	"real-nuc-tsukuba-brz-drift-2026-10-15",
	"real-nuc-suzuka-timed-race-2026-10-16",
	"real-nuc-willow-springs-pit-stop-2026-10-17",
	"real-nuc-gilles-villeneuve-race-2026-10-17",
	"real-nuc-gilles-villeneuve-pit-stop-2026-10-17",
	"real-nuc-sugo-two-stops-2026-10-18",
	"real-nuc-sugo-replay-2026-10-18",
	"real-nuc-laguna-seca-demo-2026-10-18",
	"real-nuc-sugo-restarts-2026-10-18",
	"real-nuc-suzuka-joined-mid-race-2026-10-18",
	"real-nuc-suzuka-before-restart-2026-10-18",
	"real-nuc-spa-pit-cut-quit-2026-10-18",
	"real-nuc-spa-time-trial-2026-10-18",
}

// liveRecordings are recordings of ecu's own, cut from live outings, each with
// v1's verdicts on the table as ecu has it (testdata/live/make_fixture.py says
// where each comes from).
var liveRecordings = []string{
	"real-nuc-short-shift-fuji-race-2026-10-29", // paddock#81: Fuji's box was short
}

// The table's ids the tests name.
const (
	n24h, gp, nordschleife, tourist = 348, 349, 95, 347
	sugo                            = 100001 // learned 2026-10-15, the first row we added ourselves
	gillesVilleneuve                = 100002 // learned 2026-10-17 from a daily race
	fuji, fujiShort                 = 16, 837
	suzuka, suzukaEast              = 10, 442
	tokyoEast, lagunaSeca           = 361, 41
	spa, spa24h                     = 462, 1269
	lagoMaggioreEastReverse         = 1231
	spaCircuit                      = "Circuit de Spa-Francorchamps"
	fujiCircuit                     = "Fuji International Speedway"
	willowSprings                   = 122
)

// verdict is a line of a golden: one change of the decision, or the match after the
// last frame (testdata/golden/make_golden.py).
type verdict struct {
	Frame      int     `json:"frame"`
	Lap        int32   `json:"lap"`
	TrackID    *int32  `json:"track_id"`
	Name       *string `json:"name"`
	Confidence string  `json:"confidence"`
	Family     *string `json:"family"`
	Candidates []int32 `json:"candidates"`
	Crossed    []int32 `json:"crossed"`
	Evidence   string  `json:"evidence"`
	Final      bool    `json:"final"`
}

func verdictOf(frame int, lap int32, m Match, final bool) verdict {
	v := verdict{
		Frame: frame, Lap: lap, Confidence: m.Confidence.String(), Evidence: m.Evidence, Final: final,
		Candidates: m.CandidateIDs(), Crossed: []int32{},
	}
	if m.TrackID != 0 {
		id, name := m.TrackID, m.Name
		v.TrackID, v.Name = &id, &name
	}
	if m.Family != "" {
		f := m.Family
		v.Family = &f
	}
	for _, c := range m.Candidates {
		if c.LineCrossed {
			v.Crossed = append(v.Crossed, c.Track.ID)
		}
	}
	return v
}

// step is one change of the detector's decision over a recording.
type step struct {
	frame int // from 1
	lap   int32
	phase telemetryv1.Phase
	match Match
}

// detect runs a fresh detector over a recording, as v1's tracks detect did
// (tracktools.detect_recording), told it joined part way when join is set. It
// returns the final match and every change.
func detect(tb testing.TB, name string, join bool) (Match, []step) {
	tb.Helper()
	d := New(nil, Defaults)
	if join {
		d.Join()
	}
	var steps []step
	for i, f := range frames(tb, name) {
		if m, changed := d.Observe(f); changed {
			steps = append(steps, step{i + 1, f.GetLap(), f.GetPhase(), m})
		}
	}
	return d.Match(), steps
}

func readGolden(t *testing.T, path string) []verdict {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // under testdata
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []verdict
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var v verdict
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if v.Candidates == nil {
			v.Candidates = []int32{}
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// AC2: on every recording of v1's FIXTURES.yaml and each of liveRecordings, each
// change of the verdict (the frame, the track, the confidence, the family, the
// candidates, those crossed and the evidence) and the final match are v1's, as
// make_golden.py wrote them from v1.0.0 with the table as ecu has it (Fuji's box
// widened, paddock#81). The .joined goldens are the same with the detector told
// it joined part way before the first frame, as v1's engine does for a session
// joined mid-race.
func TestEveryRecordingGivesV1sVerdicts(t *testing.T) {
	goldens, err := filepath.Glob(filepath.Join("testdata", "golden", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var plain []string
	for _, path := range goldens {
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if base, ok := strings.CutSuffix(name, ".joined"); ok {
			name = base
		} else {
			plain = append(plain, name)
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			join := strings.HasSuffix(path, ".joined.jsonl")
			want := readGolden(t, path)
			final, steps := detect(t, name, join)
			var got []verdict
			for _, s := range steps {
				got = append(got, verdictOf(s.frame, s.lap, s.match, false))
			}
			fs := frames(t, name)
			got = append(got, verdictOf(len(fs), fs[len(fs)-1].GetLap(), final, true))
			for i := range max(len(got), len(want)) {
				switch {
				case i >= len(got):
					t.Errorf("v1 also changed to %+v", want[i])
				case i >= len(want):
					t.Errorf("v1 never changed to %+v", got[i])
				case !reflect.DeepEqual(got[i], want[i]):
					t.Errorf("change %d:\n got %s\nwant %s", i+1, show(got[i]), show(want[i]))
				}
			}
		})
	}
	slices.Sort(plain)
	all := slices.Concat(v1Recordings, liveRecordings)
	slices.Sort(all)
	if !slices.Equal(plain, all) {
		t.Errorf("goldens for %v, want one for each of the %d recordings", plain, len(all))
	}
}

func show(v verdict) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// named is every track a change named.
func named(steps []step) map[int32]bool {
	out := map[int32]bool{}
	for _, s := range steps {
		if s.match.TrackID != 0 {
			out[s.match.TrackID] = true
		}
	}
	return out
}

func firstStep(steps []step, ok func(step) bool) (step, bool) {
	for _, s := range steps {
		if ok(s) {
			return s, true
		}
	}
	return step{}, false
}

func confirmedStep(t *testing.T, steps []step) step {
	t.Helper()
	s, ok := firstStep(steps, func(s step) bool { return s.match.Confidence == Confirmed })
	if !ok {
		t.Fatal("never confirmed")
	}
	return s
}

func wantFinal(t *testing.T, m Match, id int32, c Confidence) {
	t.Helper()
	if m.TrackID != id || m.Confidence != c {
		t.Fatalf("final %d %v (%s), want %d %v", m.TrackID, m.Confidence, m.Evidence, id, c)
	}
}

func crossedIDs(m Match) []int32 {
	var out []int32
	for _, c := range m.Candidates {
		if c.LineCrossed {
			out = append(out, c.Track.ID)
		}
	}
	slices.Sort(out)
	return out
}

// AC1, the Nürburgring 24h race, from the pre-race screen to lap 2: probable before
// the lights (the rolling start crosses the line before lap 1), confirmed when lap
// 1 closes, with the GP layout and both Nordschleife layouts live on the way.
func TestN24hRaceIsProbableBeforeTheLightsAndConfirmedAfterLap1(t *testing.T) {
	const name = "real-n24h-race-lap1-positions-2026-10-14"
	final, steps := detect(t, name, false)
	wantFinal(t, final, n24h, Confirmed)
	if !strings.Contains(final.Evidence, "a full lap fits") || final.Candidates[0].Fit <= 0.85 {
		t.Errorf("final: %s, fit %.2f", final.Evidence, final.Candidates[0].Fit)
	}
	start := slices.IndexFunc(frames(t, name), func(f *telemetryv1.Frame) bool {
		return f.GetPhase() == telemetryv1.Phase_PHASE_IN_RACE
	}) + 1
	probable, ok := firstStep(steps, func(s step) bool { return s.match.Confidence == Probable && s.match.TrackID == n24h })
	if !ok || probable.frame >= start {
		t.Errorf("probable at frame %d (found %v), the race starts at %d", probable.frame, ok, start)
	}
	confirmed := confirmedStep(t, steps)
	if confirmed.lap != 2 || !slices.Equal(confirmed.match.CandidateIDs(), []int32{n24h}) {
		t.Errorf("confirmed on lap %d with %v", confirmed.lap, confirmed.match.CandidateIDs())
	}
	seen := map[int32]bool{}
	for _, s := range steps {
		for _, id := range s.match.CandidateIDs() {
			seen[id] = true
		}
	}
	if !seen[gp] || !seen[nordschleife] || !seen[tourist] {
		t.Errorf("candidates on the way %v, want the GP and both Nordschleife layouts", seen)
	}
}

// AC1: Sportsland SUGO, added to GT7 on 2026-10-15 and to the table from this very
// lap with tracks learn, so the fit is near perfect.
func TestSUGOIsConfirmedFromItsLearnedRow(t *testing.T) {
	final, steps := detect(t, "real-nuc-unknown-track-2026-10-15", false)
	wantFinal(t, final, sugo, Confirmed)
	if final.Candidates[0].Fit <= 0.95 {
		t.Errorf("fit %.3f", final.Candidates[0].Fit)
	}
	if s := confirmedStep(t, steps); s.lap != 2 {
		t.Errorf("confirmed on lap %d, want 2", s.lap)
	}
}

// AC1: Circuit Gilles Villeneuve, learned from a daily race: a rolling start,
// probable once over the line, confirmed when lap 1 is complete.
func TestGillesVilleneuveRaceIsConfirmedAtTheEndOfLap1(t *testing.T) {
	final, steps := detect(t, "real-nuc-gilles-villeneuve-race-2026-10-17", false)
	wantFinal(t, final, gillesVilleneuve, Confirmed)
	if Name(gillesVilleneuve) != "Circuit Gilles Villeneuve" {
		t.Errorf("name %q", Name(gillesVilleneuve))
	}
	if _, ok := firstStep(steps, func(s step) bool { return s.match.Confidence == Probable }); !ok {
		t.Error("never probable")
	}
	if s := confirmedStep(t, steps); s.lap != 2 {
		t.Errorf("confirmed on lap %d, want 2", s.lap)
	}
}

// AC1: without the SUGO row the lap is a track the table lacks, and that is none,
// not Fuji: Fuji's recorded line sits 99 m from where the lap counter ticked, so on
// the lap counter alone the Fuji pair looked crossed; the completed lap fills only
// 35% of their box, which rules them out.
func TestATrackTheTableLacksIsNoneNotFuji(t *testing.T) {
	var without []Track
	for _, tr := range Table() {
		if tr.ID != sugo {
			without = append(without, tr)
		}
	}
	d := New(without, Defaults)
	for _, f := range frames(t, "real-nuc-unknown-track-2026-10-15") {
		if m, _ := d.Observe(f); m.TrackID != 0 {
			t.Fatalf("named %d (%s)", m.TrackID, m.Evidence)
		}
	}
	if m := d.Match(); m.Confidence != None || len(m.Candidates) != 0 {
		t.Errorf("final %v with %v", m.Confidence, m.CandidateIDs())
	}
}

// AC1: Suzuka East is confirmed on the lap counter. The full Suzuka shares the line
// and contained the lap, but the lap fills a quarter of it.
func TestSuzukaEastIsConfirmedOnTheLapCounter(t *testing.T) {
	final, steps := detect(t, "real-nuc-suzuka-east-2026-10-15", false)
	wantFinal(t, final, suzukaEast, Confirmed)
	if final.Candidates[0].Fit <= 0.9 {
		t.Errorf("fit %.3f", final.Candidates[0].Fit)
	}
	decided := confirmedStep(t, steps)
	if decided.lap != 2 || slices.Contains(decided.match.CandidateIDs(), suzuka) {
		t.Errorf("confirmed on lap %d with %v", decided.lap, decided.match.CandidateIDs())
	}
	var onLap1 int
	for _, s := range steps {
		if s.lap == 1 {
			onLap1++
			if s.match.Confidence != Ambiguous && s.match.Confidence != Probable {
				t.Errorf("lap 1: %v", s.match.Confidence)
			}
		}
	}
	if onLap1 == 0 {
		t.Error("no verdict on lap 1")
	}
}

// AC1: a timed session at Fuji. The pit exit crosses Tokyo East's line, 118 m from
// Fuji's, which v1 once announced. Fuji and Fuji Short share a line and a box, so
// the answer is the circuit, on the layout named after it (#275).
func TestFujiFromThePitsIsNeverTokyoEast(t *testing.T) {
	final, steps := detect(t, "real-nuc-fuji-timed-2026-10-15", false)
	for id := range named(steps) {
		if id != fuji && id != fujiShort {
			t.Errorf("named %d", id)
		}
	}
	wantFinal(t, final, fuji, Probable)
	if got := crossedIDs(final); !slices.Equal(got, []int32{fuji, fujiShort}) {
		t.Errorf("crossed %v", got)
	}
	if final.Name != fujiCircuit || final.Family != fujiCircuit {
		t.Errorf("name %q, family %q", final.Name, final.Family)
	}
}

// AC1: Tokyo East is never Suzuka mid-lap, and Laguna Seca never Interlagos.
func TestNoLayoutIsNamedFromAnotherCircuitsLineMidLap(t *testing.T) {
	for name, id := range map[string]int32{
		"real-nuc-tokyo-east-race-2026-10-15":  tokyoEast,
		"real-nuc-laguna-seca-race-2026-10-15": lagunaSeca,
	} {
		final, steps := detect(t, name, false)
		if got := named(steps); len(got) != 1 || !got[id] {
			t.Errorf("%s: named %v, want only %d", name, got, id)
		}
		if final.TrackID != id || final.Confidence != Confirmed {
			t.Errorf("%s: final %d %v", name, final.TrackID, final.Confidence)
		}
	}
}

// AC1: a timed session at SUGO from the pits. The pit exit crossed Kyoto's line,
// and the out lap was counted into lap 1, which failed SUGO's known length and
// left nothing.
func TestSUGOTimedSessionFromThePitsIsConfirmed(t *testing.T) {
	final, steps := detect(t, "real-nuc-sugo-timed-2026-10-15", false)
	if got := named(steps); len(got) != 1 || !got[sugo] {
		t.Errorf("named %v", got)
	}
	wantFinal(t, final, sugo, Confirmed)
}

// AC1: the Spa time trial is the circuit (the table can't split Spa from Spa 24h)
// from the first crossing, held still, never swapping between the twins (#275).
func TestSpaIsTheCircuitAndHoldsStill(t *testing.T) {
	final, steps := detect(t, "real-nuc-spa-time-trial-2026-10-18", false)
	for _, s := range steps {
		if s.match.Name != "" && (s.match.TrackID != spa || s.match.Name != spaCircuit || s.match.Confidence != Probable) {
			t.Errorf("frame %d: %d %q %v", s.frame, s.match.TrackID, s.match.Name, s.match.Confidence)
		}
	}
	if final.Family != spaCircuit {
		t.Errorf("family %q", final.Family)
	}
	if got := crossedIDs(final); !slices.Equal(got, []int32{spa, spa24h}) {
		t.Errorf("crossed %v", got)
	}
}

// AC1 and R4: Spa's pit cut-scene, a loading screen that keeps the lap counter,
// doesn't reset the detector: after it the match is still the circuit.
func TestSpaKeepsItsTrackThroughThePitCut(t *testing.T) {
	d := New(nil, Defaults)
	cut := false
	var after []string
	for _, f := range frames(t, "real-nuc-spa-pit-cut-quit-2026-10-18") {
		m, _ := d.Observe(f)
		cut = cut || (f.GetPhase() == telemetryv1.Phase_PHASE_LOADING && f.GetLap() == 5)
		if cut && f.GetPhase() == telemetryv1.Phase_PHASE_IN_RACE {
			after = append(after, m.Name)
		}
	}
	if !cut || len(after) == 0 {
		t.Fatalf("cut %v, %d frames after it", cut, len(after))
	}
	for _, name := range after {
		if name != spaCircuit {
			t.Fatalf("after the cut: %q", name)
		}
	}
}

// AC1 and R4: joined mid-lap at Suzuka, the start was near Lago Maggiore's line and
// the path crossed it. Joined part way, Suzuka is named from the line, and never
// Lago Maggiore; read as a fresh start, v1 named Lago Maggiore 28 s in.
func TestSuzukaJoinedMidRaceIsSuzukaFromTheLine(t *testing.T) {
	const name = "real-nuc-suzuka-joined-mid-race-2026-10-18"
	final, steps := detect(t, name, true)
	if got := named(steps); len(got) != 1 || !got[suzuka] {
		t.Errorf("joined: named %v", got)
	}
	first := steps[slices.IndexFunc(steps, func(s step) bool { return s.match.TrackID != 0 })]
	if first.lap != 8 {
		t.Errorf("named on lap %d, want 8, at the line", first.lap)
	}
	wantFinal(t, final, suzuka, Probable)
	if _, fresh := detect(t, name, false); !named(fresh)[lagoMaggioreEastReverse] {
		t.Error("read as a fresh start, v1 named Lago Maggiore; the port doesn't")
	}
}

// R4: joined part way at Willow Springs, nothing is named before the line.
func TestWillowSpringsJoinedNamesNothingBeforeTheLine(t *testing.T) {
	final, steps := detect(t, "real-nuc-willow-springs-pit-stop-2026-10-17", true)
	if got := named(steps); len(got) != 1 || !got[willowSprings] {
		t.Errorf("named %v", got)
	}
	wantFinal(t, final, willowSprings, Confirmed)
}

// paddock#81: the 3-lap race at Fuji on 2026-10-29 reached x 667 on laps 1 and 2,
// beyond v1.0.0's box for Fuji and Fuji Short (x 615.2 and 615.7) and the 50 m it
// allows, so both were ruled out before the first line crossing and the race was
// never named. With their box widened to x 670 the circuit is probable at the
// first crossing, frame 476, and holds to the end.
func TestTheFujiRaceOf20261029IsFuji(t *testing.T) {
	const name = "real-nuc-short-shift-fuji-race-2026-10-29"
	final, steps := detect(t, name, false)
	wantFinal(t, final, fuji, Probable)
	if final.Family != fujiCircuit {
		t.Errorf("family %q", final.Family)
	}
	first, ok := firstStep(steps, func(s step) bool { return s.match.TrackID != 0 })
	if !ok || first.frame != 476 || first.lap != 2 {
		t.Errorf("named on frame %d lap %d (found %v), want 476, lap 2", first.frame, first.lap, ok)
	}
	if got := named(steps); len(got) != 1 || !got[fuji] {
		t.Errorf("named %v", got)
	}
	fs := frames(t, name)
	var maxX float64
	for _, f := range fs {
		maxX = max(maxX, f.GetPosX())
	}
	if maxX < 667 {
		t.Errorf("the fixture reaches x %.1f, want beyond 667", maxX)
	}

	// With v1.0.0's rows nothing fits from the first crossing on.
	d := New(v1Table(t), Defaults)
	var none int
	for i, f := range fs {
		if m, changed := d.Observe(f); changed && m.Confidence == None {
			none = i + 1
			break
		}
	}
	if none != 476 {
		t.Errorf("with v1.0.0's Fuji rows: none on frame %d, want 476", none)
	}
	// And why (paddock#300): the path left the box on lap 1.
	for id, want := range map[int32]string{
		fuji: "path left its box, x 667 > 615+50", fujiShort: "path left its box, x 667 > 616+50",
	} {
		if why, out := d.RuledOut(id); !out || why != want {
			t.Errorf("%d ruled out %v: %q, want %q", id, out, why, want)
		}
	}
	if _, out := New(nil, Defaults).RuledOut(fuji); out {
		t.Error("a fresh detector rules Fuji out")
	}
}
