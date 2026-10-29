package race

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/track"
)

// v1Recordings are the recordings of v1's tests/fixtures/FIXTURES.yaml at v1.0.0,
// each with a golden in testdata/track/.
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

// trackLine is a line of a track golden (testdata/track/make_golden.py): a session
// opened, its verdict changed, or it ended with the track it stored.
type trackLine struct {
	Event   string       `json:"event"`
	Session int          `json:"session"`
	Frame   uint32       `json:"frame"`
	Track   *goldenTrack `json:"track"`
}

// goldenTrack is a verdict as v1 stored it: candidates are their ids, best first,
// all of them.
type goldenTrack struct {
	TrackID    *int32  `json:"track_id"`
	Name       *string `json:"name"`
	Confidence string  `json:"confidence"`
	Family     *string `json:"family"`
	Candidates []int32 `json:"candidates"`
	Evidence   string  `json:"evidence"`
}

// show is a line as the test compares it. The end's frame is left out: v1 counted
// the frame it was on when it ended a session, ecu gives the session's last. A
// golden's candidates are cut to the eight ecu publishes.
func (l trackLine) show() string {
	frame := fmt.Sprintf(" frame %d", l.Frame)
	if l.Event == "session.ended" {
		frame = ""
	}
	tr := "no track"
	if g := l.Track; g != nil {
		id, name, family := int32(0), "", ""
		if g.TrackID != nil {
			id = *g.TrackID
		}
		if g.Name != nil {
			name = *g.Name
		}
		if g.Family != nil {
			family = *g.Family
		}
		cands := g.Candidates[:min(len(g.Candidates), track.MaxReportedCandidates)]
		tr = fmt.Sprintf("%s %d %q family %q %v %q", g.Confidence, id, name, family, cands, g.Evidence)
	}
	if l.Event == "session.started" {
		tr = ""
	}
	return fmt.Sprintf("%s %d%s %s", l.Event, l.Session, frame, tr)
}

func readTrackGolden(t *testing.T, name string) []trackLine {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "track", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []trackLine
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var l trackLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lastPerFrame(lines)
}

// lastPerFrame keeps a session's last verdict change on each frame: ecu publishes
// one session.track a frame, where v1 logged each change, so a verdict held as the
// session opens and the line crossed on that frame are one event.
func lastPerFrame(lines []trackLine) []trackLine {
	var out []trackLine
	for _, l := range lines {
		if n := len(out); n > 0 && l.Event == "session.track" && out[n-1].Event == "session.track" &&
			out[n-1].Session == l.Session && out[n-1].Frame == l.Frame {
			out[n-1] = l
			continue
		}
		out = append(out, l)
	}
	return out
}

var confidenceNames = map[eventsv1.TrackConfidence]string{
	eventsv1.TrackConfidence_TRACK_CONFIDENCE_NONE:      "none",
	eventsv1.TrackConfidence_TRACK_CONFIDENCE_AMBIGUOUS: "ambiguous",
	eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE:  "probable",
	eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED: "confirmed",
}

// asGolden is a published verdict as a golden's track.
func asGolden(v *eventsv1.TrackVerdict) *goldenTrack {
	if v == nil {
		return nil
	}
	g := &goldenTrack{Confidence: confidenceNames[v.GetConfidence()], Evidence: v.GetEvidence(), Candidates: []int32{}}
	if v.TrackId != nil {
		g.TrackID = v.TrackId
	}
	if v.GetName() != "" {
		g.Name = new(v.GetName())
	}
	if v.GetFamily() != "" {
		g.Family = new(v.GetFamily())
	}
	for _, c := range v.GetCandidates() {
		g.Candidates = append(g.Candidates, c.GetTrackId())
	}
	return g
}

// trackLines are the outputs as a golden's lines: each start, each published
// verdict and each end with its summary's track.
func trackLines(out []Output) []trackLine {
	var lines []trackLine
	n := 0
	for _, o := range out {
		switch {
		case o.Started != nil:
			n++
			lines = append(lines, trackLine{Event: "session.started", Session: n, Frame: o.Started.GetFirstSeq()})
		case o.Track != nil:
			lines = append(lines, trackLine{Event: "session.track", Session: n, Frame: o.Track.GetAt(), Track: asGolden(o.Track.GetTrack())})
		case o.Ended != nil:
			lines = append(lines, trackLine{Event: "session.ended", Session: n, Track: asGolden(o.Ended.GetSummary().GetTrack())})
		}
	}
	return lines
}

// discarded are the recordings v1's engine opens no session on, and the issue
// that brings ecu in line: v1 holds a start GT7's cars-on-track flag is off for
// and drops it as the replay or GT7 demo it is (DriverCheck), where ecu opens a
// session. The four of 2026-10-15 were recorded before v1 read the flag, which
// reads as off throughout, so v1 1.0.0 takes them for demos too. On these, each
// verdict ecu publishes is checked against v1's detector instead
// (detectorAgrees).
var discarded = map[string]string{
	"real-nuc-tokyo-east-race-2026-10-15":            "paddock#74",
	"real-nuc-nordschleife-rolling-start-2026-10-15": "paddock#74",
	"real-nuc-laguna-seca-race-2026-10-15":           "paddock#74",
	"real-nuc-tsukuba-brz-drift-2026-10-15":          "paddock#74",
	"real-nuc-sugo-replay-2026-10-18":                "paddock#74",
	"real-nuc-laguna-seca-demo-2026-10-18":           "paddock#74",
}

// R2, AC5: on every one of v1's recordings, ecu publishes session.track where v1's
// engine applied a verdict to the session (v1's "track ..." log line), with v1's
// verdict, its first 8 candidates and its evidence, and each session.ended's
// summary keeps the track v1 stored for the session (R5).
func TestSessionTrackIsV1sOnEveryRecording(t *testing.T) {
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			out := replay(t, name)
			got := trackLines(out)
			want := readTrackGolden(t, name)
			if issue, ok := discarded[name]; ok {
				if len(want) != 0 {
					t.Errorf("v1 opened a session: take it out of discarded (%s)", issue)
				}
				detectorAgrees(t, name, out)
				return
			}
			if g, w := shows(got), shows(want); !slices.Equal(g, w) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(g, "\n"), strings.Join(w, "\n"))
			}
		})
	}
}

// detectorVerdict is a line of internal/track's goldens: a change of v1's
// detector's decision, on the recording's frame.
type detectorVerdict struct {
	Frame      uint32  `json:"frame"`
	TrackID    *int32  `json:"track_id"`
	Confidence string  `json:"confidence"`
	Family     *string `json:"family"`
	Final      bool    `json:"final"`
}

// detectorAgrees checks that each verdict published is v1's detector's decision
// (internal/track's golden for the recording) at its frame: the latest change up
// to the frame, or, on the frame a session opens, the latest before it, which the
// detector had as the session opened. The decision is the track, the confidence
// and the circuit; the evidence's wording changes between the golden's lines. A
// session joined part way is held to the detector told so (the golden's .joined
// form, here in testdata/track/ where internal/track has none). At least one
// verdict must be published.
func detectorAgrees(t *testing.T, name string, out []Output) {
	t.Helper()
	published := publishedTracks(out)
	if len(published) == 0 {
		t.Error("no session.track published")
	}
	starts := map[uint32]bool{}
	path := filepath.Join("..", "track", "testdata", "golden", name+".jsonl")
	for _, o := range out {
		if o.Started != nil {
			starts[o.Started.GetFirstSeq()] = true
			if o.Started.GetJoinedPartWay() {
				path = filepath.Join("testdata", "track", name+".joined.jsonl")
			}
		}
	}
	changes := readDetectorGolden(t, path)
	latest := func(frame uint32, upTo bool) *detectorVerdict {
		var l *detectorVerdict
		for j := range changes {
			if changes[j].Frame < frame || (upTo && changes[j].Frame == frame) {
				l = &changes[j]
			}
		}
		return l
	}
	same := func(v *detectorVerdict, g *goldenTrack) bool {
		return v != nil && reflect.DeepEqual(v.TrackID, g.TrackID) && v.Confidence == g.Confidence &&
			reflect.DeepEqual(v.Family, g.Family)
	}
	for _, p := range published {
		g := asGolden(p.GetTrack())
		upTo := latest(p.GetAt(), true)
		if !same(upTo, g) && (!starts[p.GetAt()] || !same(latest(p.GetAt(), false), g)) {
			t.Errorf("frame %d published %+v; v1's detector had %+v", p.GetAt(), *g, upTo)
		}
	}
}

// readDetectorGolden is the changes of a detector golden, without its final line.
func readDetectorGolden(t *testing.T, path string) []detectorVerdict {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var changes []detectorVerdict
	for line := range strings.Lines(string(data)) {
		var v detectorVerdict
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatal(err)
		}
		if !v.Final {
			changes = append(changes, v)
		}
	}
	return changes
}

func shows(lines []trackLine) []string {
	s := make([]string, len(lines))
	for i, l := range lines {
		s[i] = l.show()
	}
	return s
}

// publishedTracks are the verdicts published, in order.
func publishedTracks(out []Output) []*eventsv1.SessionTrackChanged {
	var tr []*eventsv1.SessionTrackChanged
	for _, o := range out {
		if o.Track != nil {
			tr = append(tr, o.Track)
		}
	}
	return tr
}

// The cap: the detector keeps v1's whole list of candidates, and at most 8, the
// best, are published. Suzuka mid-lap, fed from its first frame to a detector not
// told the session was joined, still fits 25 layouts on frame 58, as v1 found
// (internal/track's golden).
func TestAtMost8CandidatesArePublished(t *testing.T) {
	m, d := New(testIDs()), track.New(nil, track.Config{})
	var match track.Match
	for _, r := range packets(t, "real-nuc-suzuka-joined-mid-race-2026-10-18")[:58] {
		match, _ = d.Observe(m.Step(r)[0].Frame)
	}
	if len(match.Candidates) != 25 {
		t.Fatalf("%d candidates on frame 58, want v1's 25", len(match.Candidates))
	}
	v := verdict(match)
	if len(v.GetCandidates()) != track.MaxReportedCandidates {
		t.Fatalf("%d candidates published, want %d", len(v.GetCandidates()), track.MaxReportedCandidates)
	}
	for i, c := range v.GetCandidates() {
		want := match.Candidates[i]
		if c.GetTrackId() != want.Track.ID || c.GetName() != want.Track.Name ||
			c.GetLineCrossed() != want.LineCrossed || c.GetFit() != round3(want.Fit) {
			t.Errorf("candidate %d: %v, want %d (crossed %t, fit %.3f)", i, c, want.Track.ID, want.LineCrossed, want.Fit)
		}
	}
}

// Every verdict published on every recording names at most 8 candidates.
func TestNoVerdictPublishesMoreThan8Candidates(t *testing.T) {
	for _, name := range v1Recordings {
		out := replay(t, name)
		for _, tr := range publishedTracks(out) {
			if n := len(tr.GetTrack().GetCandidates()); n > track.MaxReportedCandidates {
				t.Errorf("%s: frame %d published %d candidates", name, tr.GetAt(), n)
			}
		}
		for _, o := range out {
			if n := len(o.Ended.GetSummary().GetTrack().GetCandidates()); n > track.MaxReportedCandidates {
				t.Errorf("%s: the summary has %d candidates", name, n)
			}
		}
	}
}

// R1: the session's verdict changes only for one that ranks higher (confidence,
// then naming a layout), or when the layout it holds is ruled out; never for a
// change of candidates or wording alone.
func TestTheSessionKeepsItsBestVerdict(t *testing.T) {
	suzuka, _ := track.ByID(10)
	m := New(testIDs())
	m.session = &session{id: "S"}
	s := &state{seq: 1}
	apply := func(id int32, c track.Confidence, evidence string) bool {
		m.out = nil
		m.applyTrack(track.Match{TrackID: id, Confidence: c, Evidence: evidence,
			Candidates: []track.Candidate{{Track: &suzuka}}}, s, epoch)
		s.seq++
		return len(m.out) == 1
	}
	steps := []struct {
		id        int32
		c         track.Confidence
		evidence  string
		published bool
		why       string
	}{
		{0, track.Ambiguous, "a", true, "the first verdict"},
		{0, track.Ambiguous, "b", false, "wording alone"},
		{0, track.None, "c", false, "a blip down to nothing"},
		{10, track.Probable, "d", true, "a layout, surer"},
		{0, track.Ambiguous, "e", false, "a blip down, the layout still alive"},
		{10, track.Probable, "f", false, "the same decision"},
		{10, track.Confirmed, "g", true, "surer"},
		{999999, track.Confirmed, "h", true, "another layout as sure"},
		{0, track.Probable, "i", true, "below, but 999999 is not a layout the detector holds alive"},
	}
	for _, st := range steps {
		if got := apply(st.id, st.c, st.evidence); got != st.published {
			t.Errorf("%s (%d %v): published %t, want %t", st.why, st.id, st.c, got, st.published)
		}
	}
	if got := m.out[0]; got.Event.ID != "S:session.track:"+m.Run()+":9" || got.Track.GetAt() != 9 || got.Session != "S" {
		t.Errorf("the last verdict: id %s, at %d, session %s", got.Event.ID, got.Track.GetAt(), got.Session)
	}
}

// AC1 and AC2: at Spa, the session's verdict is the circuit, probable, from the
// first line crossing, and the summary keeps it, after the pit cut and the quit
// too.
func TestSpaIsTheCircuitToTheEnd(t *testing.T) {
	for _, name := range []string{"real-nuc-spa-pit-cut-quit-2026-10-18", "real-nuc-spa-time-trial-2026-10-18"} {
		out := replay(t, name)
		published := publishedTracks(out)
		if len(published) == 0 {
			t.Fatalf("%s: no session.track", name)
		}
		last := published[len(published)-1].GetTrack()
		if last.GetName() != "Circuit de Spa-Francorchamps" || last.GetFamily() != last.GetName() ||
			last.GetConfidence() != eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE {
			t.Errorf("%s: the last session.track is %v", name, last)
		}
		for _, p := range published[:len(published)-1] {
			if p.GetTrack().TrackId != nil {
				t.Errorf("%s: a layout before the circuit: %v", name, p)
			}
		}
		for _, o := range out {
			if o.Ended != nil && o.Ended.GetSummary().GetTrack().GetName() != last.GetName() {
				t.Errorf("%s: the summary's track is %v", name, o.Ended.GetSummary().GetTrack())
			}
		}
	}
}
