package track

import (
	"math"
	"slices"
	"strings"
	"testing"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

// The synthetic layouts of v1's tests/test_tracks.py and test_twins.py at v1.0.0.

func mk(id int32, name string, dir Direction, line [2]Point, box Box, length float64) Track {
	return Track{ID: id, Name: name, Direction: dir, Line: line, Box: box, HasGeometry: true, LengthM: length}
}

var line100 = [2]Point{{100, 0}, {100, 20}}

// rectangle is a circuit 1000 x 400 m with its finish line on the bottom straight,
// crossed moving +x; a short variant sharing the line; its reverse twin; and a huge
// unrelated box like Special Stage Route X.
func rectangle() []Track {
	return []Track{
		mk(1, "Full", PX, line100, Box{0, 0, 1000, 400}, 0),
		mk(2, "Short", PX, line100, Box{0, 0, 500, 400}, 0),
		mk(3, "Full Reverse", NX, line100, Box{0, 0, 1000, 400}, 0),
		mk(4, "Far Away", PX, [2]Point{{4000, 4000}, {4000, 4020}}, Box{-5000, -5000, 5000, 5000}, 0),
	}
}

// cfg is v1's tests' tolerances: a 20 m box tolerance and a 500 m start radius,
// then what each test changes.
func cfg(change func(*Config)) Config {
	c := Defaults
	c.BoxTolerance, c.StartRadius = 20, 500
	if change != nil {
		change(&c)
	}
	return c
}

// rectLap is points around a rectangle, anticlockwise, starting at (x0, z0) heading
// +x, with the lap boundary mid-straight, heading +x, not on a corner.
func rectLap(x0, x1, z0, z1 float64) []Point {
	const step = 10.0
	var pts []Point
	x, z := x0, z0
	for x < x1 {
		pts = append(pts, Point{x, z})
		x += step
	}
	for z < z1 {
		pts = append(pts, Point{x1, z})
		z += step
	}
	for x > x0 {
		pts = append(pts, Point{x, z1})
		x -= step
	}
	for z > z0 {
		pts = append(pts, Point{x0, z})
		z -= step
	}
	return append(slices.Clone(pts[3:]), pts[:3]...)
}

var (
	fullLap  = rectLap(50, 980, 10, 390) // fills Full's box, leaves Short's
	shortLap = rectLap(50, 480, 10, 390)
)

func drive(d *Detector, pts []Point, lap int32) []Match {
	var changes []Match
	for _, p := range pts {
		if m, changed := d.ObservePoint(p, Point{}, lap); changed {
			changes = append(changes, m)
		}
	}
	return changes
}

func driveLaps(d *Detector, pts []Point, laps int32) {
	for n := range laps {
		drive(d, pts, 1+n)
	}
}

func line(x0, x1, step, z float64) []Point {
	var pts []Point
	for x := x0; (step > 0 && x < x1) || (step < 0 && x > x1); x += step {
		pts = append(pts, Point{x, z})
	}
	return pts
}

func ids(m Match) map[int32]bool {
	out := map[int32]bool{}
	for _, id := range m.CandidateIDs() {
		out[id] = true
	}
	return out
}

func want(t *testing.T, m Match, id int32, c Confidence) {
	t.Helper()
	if m.TrackID != id || m.Confidence != c {
		t.Fatalf("%d %v (%s), want %d %v", m.TrackID, m.Confidence, m.Evidence, id, c)
	}
}

func TestAFamilyNarrowsByBoundsThenLineThenAFullLap(t *testing.T) {
	d := New(rectangle(), cfg(func(c *Config) { c.MinDistance = 300 }))
	first, changed := d.ObservePoint(Point{80, 10}, Point{}, 1)
	if !changed || first.Confidence != Ambiguous || first.TrackID != 0 || len(first.Candidates) != 3 || ids(first)[4] {
		t.Fatalf("first: %+v", first)
	}
	// The first straight crosses the line at x=100 moving +x: Full and Short, not Reverse.
	drive(d, fullLap[:30], 1)
	m := d.Match()
	if m.Confidence != Ambiguous || !slices.Equal(crossedIDs(m), []int32{1, 2}) || !ids(m)[3] || (m.TrackID != 1 && m.TrackID != 2) {
		t.Fatalf("on the straight: %+v", m)
	}
	// Past x=500 Short's box is left behind: Full alone, line crossed, no full lap yet.
	drive(d, fullLap[30:], 1)
	want(t, d.Match(), 1, Probable)
	if !strings.Contains(d.Match().Evidence, "no full lap yet") {
		t.Error(d.Match().Evidence)
	}
	// The lap counter ticks over: the lap fills Full's box.
	changes := drive(d, fullLap[:3], 2)
	want(t, d.Match(), 1, Confirmed)
	if len(changes) == 0 || !strings.Contains(changes[len(changes)-1].Evidence, "a full lap fits") {
		t.Errorf("changes %+v", changes)
	}
	if !slices.Equal(crossedIDs(d.Match()), []int32{1}) {
		t.Errorf("crossed %v", crossedIDs(d.Match()))
	}
}

func TestTheShortLayoutWinsWhenTheLapStaysInsideIt(t *testing.T) {
	d := New(rectangle(), cfg(nil))
	driveLaps(d, shortLap, 2)
	want(t, d.Match(), 2, Confirmed)
	// Full also contained the lap and shares the line, but a completed Short lap
	// fills well under half of Full's box, so Full is ruled out, not just outranked.
	if !slices.Equal(d.Match().CandidateIDs(), []int32{2}) {
		t.Errorf("candidates %v", d.Match().CandidateIDs())
	}
}

func TestTheOnlyLayoutNearTheStartBecomesProbableAfterSomeDriving(t *testing.T) {
	d := New(rectangle(), cfg(func(c *Config) { c.MinDistance = 300 }))
	// Start 1 km from every line, inside Full, Reverse and Far's boxes: nothing to
	// anchor on.
	drive(d, line(900, 600, -10, 390), NoLap)
	m := d.Match()
	if m.Confidence != Ambiguous || m.TrackID != 0 || len(m.Candidates) != 3 || ids(m)[2] ||
		!strings.Contains(m.Evidence, "no finish line within") {
		t.Fatalf("%+v", m)
	}
	tracks := rectangle()
	d = New([]Track{tracks[0], tracks[3]}, cfg(func(c *Config) { c.MinDistance = 300 }))
	drive(d, line(140, 300, 10, 10), NoLap)
	want(t, d.Match(), 0, Ambiguous) // unique, but has barely moved
	drive(d, line(300, 600, 10, 10), NoLap)
	want(t, d.Match(), 1, Probable)
	if !strings.Contains(d.Match().Evidence, "only layout") {
		t.Error(d.Match().Evidence)
	}
}

func TestATeleportIsNoCrossingAndLeavingEveryBoxIsNone(t *testing.T) {
	d := New(rectangle(), cfg(func(c *Config) { c.Teleport = 100 }))
	drive(d, []Point{{140, 10}, {150, 10}}, NoLap)
	// Back to the pits: a jump from one side of the line to the other.
	drive(d, []Point{{50, 10}}, NoLap)
	if d.Match().Confidence != Ambiguous || len(crossedIDs(d.Match())) != 0 {
		t.Errorf("%+v", d.Match())
	}
	if math.Abs(d.DistanceM()-10) > 1e-9 {
		t.Errorf("distance %v", d.DistanceM())
	}
	drive(d, line(50, 7000, 50, 10), NoLap) // off every box
	want(t, d.Match(), 0, None)
}

func frameAt(phase telemetryv1.Phase, x, z float64, lap int32) *telemetryv1.Frame {
	return &telemetryv1.Frame{Phase: phase, PosX: x, PosZ: z, Lap: lap}
}

// R4: a menu or a loading screen at lap 0 starts over; a pause, and a pit
// cut-scene that keeps the lap counter, don't.
func TestAMenuOrLoadingFrameStartsOverButAPauseDoesNot(t *testing.T) {
	const (
		inRace  = telemetryv1.Phase_PHASE_IN_RACE
		atTrack = telemetryv1.Phase_PHASE_AT_TRACK
	)
	d := New(rectangle(), cfg(func(c *Config) { c.MinDistance = 300 }))
	for x := 140.0; x < 700; x += 10 {
		d.Observe(frameAt(inRace, x, 10, 1))
	}
	if d.DistanceM() <= 500 {
		t.Fatal(d.DistanceM())
	}
	d.Observe(frameAt(telemetryv1.Phase_PHASE_PAUSED, 700, 10, 1))
	d.Observe(frameAt(inRace, 700, 10, 1))
	if d.DistanceM() <= 500 {
		t.Error("a pause lost it")
	}
	d.Observe(frameAt(telemetryv1.Phase_PHASE_LOADING, 700, 10, 1)) // a pit cut-scene keeps the lap counter
	d.Observe(frameAt(inRace, 700, 10, 1))
	if d.DistanceM() <= 500 {
		t.Error("a pit cut-scene lost it")
	}
	d.Observe(frameAt(telemetryv1.Phase_PHASE_LOADING, 0, 0, 0)) // a quit, restart or new event
	d.Observe(frameAt(atTrack, 140, 10, 0))
	if d.DistanceM() != 0 {
		t.Error("a loading screen at lap 0 kept it")
	}
	for x := 140.0; x < 300; x += 10 {
		d.Observe(frameAt(inRace, x, 10, 1))
	}
	d.Observe(frameAt(telemetryv1.Phase_PHASE_MENU, 0, 0, -1))
	d.Observe(frameAt(atTrack, 140, 10, 0))
	if d.DistanceM() != 0 {
		t.Error("a menu kept it")
	}
}

// R4: Reset forgets everything; Join forgets everything and reads the frames as a
// lap joined part way, naming nothing before a line.
func TestResetAndJoin(t *testing.T) {
	d := New(rectangle(), cfg(nil))
	driveLaps(d, shortLap, 2)
	want(t, d.Match(), 2, Confirmed)
	d.Reset()
	if d.Match().Evidence != "no position data yet" || d.DistanceM() != 0 || d.LapLengthM() != 0 || !d.IsAlive(1) {
		t.Errorf("after Reset: %+v", d.Match())
	}
	d.Join()
	// Mid-lap on the bottom straight, through the line: no crossing counts.
	drive(d, line(60, 200, 10, 10), 3)
	if len(crossedIDs(d.Match())) != 0 || d.Match().TrackID != 0 {
		t.Errorf("joined, through the line: %+v", d.Match())
	}
	if d.IsAlive(99) {
		t.Error("an id the detector lacks is alive")
	}
}

func TestTheLapCounterConfirmsDespiteAMisplacedTableSegment(t *testing.T) {
	// The table's segment sits 30 m beside the straight the car uses, as at Fuji.
	tracks := []Track{
		mk(1, "Here", PX, [2]Point{{100, 60}, {100, 80}}, Box{0, 0, 1000, 400}, 0),
		mk(2, "Elsewhere", PX, [2]Point{{900, 60}, {900, 80}}, Box{0, 0, 1000, 400}, 0),
	}
	d := New(tracks, cfg(func(c *Config) { c.LineHalfWidth = 5 }))
	drive(d, fullLap, 1)
	if len(crossedIDs(d.Match())) != 0 {
		t.Error(crossedIDs(d.Match()))
	}
	drive(d, fullLap[:3], 2) // GT7 says: a new lap, so x=100 is the line
	want(t, d.Match(), 1, Confirmed)
	if !slices.Equal(d.Match().CandidateIDs(), []int32{1}) { // Elsewhere's line is 800 m off
		t.Error(d.Match().CandidateIDs())
	}
}

func TestTheLapCounterDropsLayoutsWhoseLineIsFarOrFacesTheOtherWay(t *testing.T) {
	tracks := []Track{
		mk(1, "Forward", PX, line100, Box{0, 0, 1000, 400}, 0),
		mk(3, "Reverse", NX, line100, Box{0, 0, 1000, 400}, 0),
		mk(4, "Far", PX, [2]Point{{700, 0}, {700, 20}}, Box{0, 0, 1000, 400}, 0),
	}
	d := New(tracks, cfg(nil))
	drive(d, fullLap, 1)
	if !ids(d.Match())[1] || !ids(d.Match())[3] {
		t.Error(d.Match().CandidateIDs())
	}
	drive(d, fullLap[:3], 2)
	want(t, d.Match(), 1, Confirmed)
	if !slices.Equal(d.Match().CandidateIDs(), []int32{1}) {
		t.Error(d.Match().CandidateIDs())
	}
}

func TestACompletedLapThatDoesNotFillTheBoxRulesTheLayoutOut(t *testing.T) {
	// A new, unknown circuit whose line happens to sit near a known layout's line.
	d := New([]Track{mk(1, "Known", PX, line100, Box{0, 0, 1000, 1000}, 0)}, cfg(nil))
	drive(d, fullLap, 1)
	want(t, d.Match(), 1, Probable)
	drive(d, fullLap[:3], 2) // the lap fills only 40% of Known's box
	want(t, d.Match(), 0, None)
	if math.Abs(d.LapLengthM()-2*(930+380)) > 40 {
		t.Errorf("lap length %v", d.LapLengthM())
	}
}

func TestAKnownLapLengthSeparatesLayoutsWithOneBox(t *testing.T) {
	d := New([]Track{
		mk(1, "Chicane", PX, line100, Box{0, 0, 1000, 400}, 2620),
		mk(2, "No Chicane", PX, line100, Box{0, 0, 1000, 400}, 2300),
	}, cfg(nil))
	driveLaps(d, fullLap, 2)
	want(t, d.Match(), 1, Confirmed)
	if !slices.Equal(d.Match().CandidateIDs(), []int32{1}) {
		t.Error(d.Match().CandidateIDs())
	}
}

func TestTheFirstRacingFrameAnchorsWhenTheFirstFrameWasAMenuScreen(t *testing.T) {
	d := New([]Track{
		mk(1, "Here", PX, line100, Box{0, 0, 1000, 400}, 0),
		mk(4, "Huge", PX, [2]Point{{4000, 4000}, {4000, 4020}}, Box{-5000, -5000, 5000, 5000}, 0),
	}, cfg(func(c *Config) { c.MinDistance = 100 }))
	d.ObservePoint(Point{950, 390}, Point{}, NoLap) // the pre-session camera, nowhere near the line
	if d.Match().TrackID != 0 || !strings.Contains(d.Match().Evidence, "no finish line within") {
		t.Fatalf("%+v", d.Match())
	}
	drive(d, line(140, 400, 10, 10), 1) // racing from the grid
	want(t, d.Match(), 1, Probable)
}

func TestTheWidenedSegmentCatchesACrossingBesideTheRecordedOne(t *testing.T) {
	tracks := []Track{mk(1, "Here", PX, [2]Point{{100, 60}, {100, 80}}, Box{0, 0, 1000, 400}, 0)}
	narrow := New(tracks, cfg(func(c *Config) { c.LineHalfWidth = 5 }))
	wide := New(tracks, cfg(func(c *Config) { c.LineHalfWidth = 50 }))
	for _, d := range []*Detector{narrow, wide} {
		drive(d, []Point{{90, 30}, {110, 30}}, 1) // 30 m beside the 20 m recorded segment
	}
	if narrow.Match().Candidates[0].LineCrossed {
		t.Error("narrow crossed")
	}
	if !wide.Match().Candidates[0].LineCrossed || wide.Match().Confidence != Probable {
		t.Errorf("wide: %+v", wide.Match())
	}
}

// Driving out of the pits before a session is not a lap: at Fuji the pit exit runs
// through Tokyo East's line (2026-10-15).
func TestAPathCrossingOutsideRacingDoesNotCount(t *testing.T) {
	d := New([]Track{mk(1, "Here", PX, line100, Box{0, 0, 1000, 400}, 0)}, cfg(nil))
	drive(d, []Point{{90, 10}, {110, 10}}, NoLap) // no lap counter: the pit lane, at the track
	if d.Match().Candidates[0].LineCrossed {
		t.Error("crossed outside racing")
	}
	drive(d, []Point{{90, 10}, {110, 10}}, 1)
	if !d.Match().Candidates[0].LineCrossed {
		t.Error("not crossed while racing")
	}
}

// A rolling start ticks lap 1 on the finish line. Lines of other layouts crossed
// before or after are elsewhere on the circuit: Suzuka's on a lap of Tokyo East,
// Interlagos's on a lap of Laguna Seca (2026-10-15).
func TestLap1StartingOnALineSettlesTheStart(t *testing.T) {
	d := New([]Track{
		mk(1, "Here", PX, line100, Box{0, 0, 1000, 400}, 0),
		mk(2, "Other", PX, [2]Point{{300, 0}, {300, 20}}, Box{0, 0, 1000, 400}, 0),
	}, cfg(nil))
	drive(d, line(250, 360, 10, 10), 0) // the formation lap crosses Other's line
	want(t, d.Match(), 2, Probable)
	var back []Point
	for z := 20.0; z < 400; z += 10 {
		back = append(back, Point{350, z})
	}
	back = append(back, line(350, 50, -10, 390)...)
	for z := 390.0; z > 0; z -= 10 {
		back = append(back, Point{50, z})
	}
	back = append(back, line(60, 100, 10, 10)...)
	drive(d, back, 0)
	drive(d, line(100, 400, 10, 10), 1) // lap 1 starts on Here's line
	want(t, d.Match(), 1, Probable)
	if !slices.Equal(crossedIDs(d.Match()), []int32{1}) {
		t.Error(crossedIDs(d.Match()))
	}
}

func TestTheLapCounterDropsALineMuchFurtherAwayThanTheNearest(t *testing.T) {
	d := New([]Track{
		mk(1, "Here", PX, line100, Box{0, 0, 1000, 400}, 0),
		mk(2, "Beside", PX, [2]Point{{100, 130}, {100, 150}}, Box{0, 0, 1000, 400}, 0),
	}, cfg(nil))
	drive(d, fullLap, 1)
	drive(d, fullLap[:3], 2) // ticks on Here's line; Beside's is 120 m away
	want(t, d.Match(), 1, Confirmed)
	if !slices.Equal(d.Match().CandidateIDs(), []int32{1}) {
		t.Error(d.Match().CandidateIDs())
	}
}

// GT7 can repeat a position for a frame (pit entry): no step, so no direction.
func TestALapTickOnAFrozenFrameEliminatesNothing(t *testing.T) {
	d := New(rectangle(), cfg(nil))
	drive(d, fullLap, 1)
	before := d.Match().CandidateIDs()
	if !ids(d.Match())[1] || !ids(d.Match())[3] {
		t.Fatal(before)
	}
	last := fullLap[len(fullLap)-1]
	drive(d, []Point{last}, 1)
	drive(d, []Point{last}, 2) // the counter ticks on the repeated frame
	if d.Match().Confidence == None || !slices.Equal(d.Match().CandidateIDs(), before) || !d.IsAlive(1) || !d.IsAlive(3) {
		t.Errorf("%+v", d.Match())
	}
	// The next boundary, with a real step, settles it as usual.
	drive(d, fullLap, 2)
	drive(d, fullLap[:3], 3)
	want(t, d.Match(), 1, Confirmed)
}

func TestVelocitySetsTheDirectionWhenTheStepMisleads(t *testing.T) {
	tracks := []Track{
		mk(1, "Forward", PX, line100, Box{0, 0, 1000, 400}, 0),
		mk(3, "Reverse", NX, line100, Box{0, 0, 1000, 400}, 0),
	}
	start := fullLap[0]
	lap := append(slices.Clone(fullLap), start)
	tick := func(p, v Point) Match {
		d := New(tracks, cfg(nil))
		drive(d, lap, 1)
		m, _ := d.ObservePoint(p, v, 2)
		return m
	}
	// One frame of jitter steps back in x while the car is doing 40 m/s forwards.
	if m := tick(Point{start.X - 0.1, start.Z}, Point{40, 0.5}); m.Confidence != Confirmed || !slices.Equal(m.CandidateIDs(), []int32{1}) {
		t.Errorf("with the velocity: %+v", m)
	}
	// Without the velocity the same step points the wrong way.
	if m := tick(Point{start.X - 0.1, start.Z}, Point{}); !slices.Equal(m.CandidateIDs(), []int32{3}) {
		t.Errorf("without: %v", m.CandidateIDs())
	}
	// A zero velocity (no reading) falls back to the step.
	if m := tick(Point{start.X + 5, start.Z}, Point{}); !slices.Equal(m.CandidateIDs(), []int32{1}) {
		t.Errorf("zero velocity: %v", m.CandidateIDs())
	}
}

// Driving the unlisted reverse of a listed layout: velocity and step agree the car
// goes +x, no live layout faces +x, so none of them is this circuit.
func TestALayoutTheTableLacksIsRuledOutOnTheLapCounter(t *testing.T) {
	tracks := []Track{mk(3, "Listed Reverse", NX, line100, Box{0, 0, 1000, 400}, 0)}
	for _, v := range []Point{{}, {40, 0}} {
		d := New(tracks, cfg(nil))
		for _, p := range fullLap {
			d.ObservePoint(p, v, 1)
		}
		for _, p := range fullLap[:3] {
			d.ObservePoint(p, v, 2)
		}
		if d.Match().Confidence != None || d.IsAlive(3) {
			t.Errorf("velocity %v: %+v", v, d.Match())
		}
	}
	// The same tick with the velocity and the step disagreeing is suspect: kept.
	d := New(tracks, cfg(nil))
	start := fullLap[0]
	drive(d, append(slices.Clone(fullLap), start), 1)
	d.ObservePoint(Point{start.X - 0.1, start.Z}, Point{40, 0.5}, 2) // step -x, velocity +x
	if !d.IsAlive(3) {
		t.Error("ruled out on a suspect reading")
	}
}

func TestLap1IsMeasuredFromTheLapCounterNotThePitExit(t *testing.T) {
	const length = 2 * (930 + 380)
	d := New([]Track{mk(1, "Here", PX, line100, Box{0, 0, 1000, 400}, length)}, cfg(nil))
	drive(d, fullLap, 0) // the out lap of a timed session
	driveLaps(d, fullLap, 2)
	want(t, d.Match(), 1, Confirmed)
	if math.Abs(d.LapLengthM()-length) > 40 {
		t.Errorf("lap length %v", d.LapLengthM())
	}
}

// Two circuits whose layouts share a line and a box: a pair one lap can't split
// (v1's test_twins.py). One circuit's pair is the circuit; a mixed pair is not.
func TestAPairOfOneCircuitIsTheCircuitAndAMixedPairIsNot(t *testing.T) {
	tracks := []Track{
		mk(1, "Alpha", PX, line100, Box{0, 0, 1000, 400}, 0),
		mk(2, "Alpha - Long", PX, line100, Box{0, 0, 1000, 401}, 0),
		mk(3, "Beta", PX, line100, Box{0, 0, 1000, 402}, 0),
	}
	run := func(d *Detector) Match {
		for lap := int32(1); lap <= 2; lap++ {
			drive(d, line(50, 1000, 10, 10), lap)
			drive(d, line(1000, 50, -10, 390), lap)
		}
		drive(d, []Point{{50, 10}}, 2)
		return d.Match()
	}
	twins := run(New(tracks[:2], cfg(nil)))
	if twins.TrackID != 1 || twins.Name != "Alpha" || twins.Confidence != Probable || twins.Family != "Alpha" ||
		!strings.Contains(twins.Evidence, "all Alpha") {
		t.Errorf("twins: %+v", twins)
	}
	mixed := run(New(tracks, cfg(nil)))
	if mixed.Confidence != Ambiguous || mixed.Family != "" {
		t.Errorf("mixed: %+v", mixed)
	}
	// With no layout named after the circuit, the lowest id carries it.
	noBase := run(New([]Track{
		mk(7, "Gamma - Long", PX, line100, Box{0, 0, 1000, 400}, 0),
		mk(5, "Gamma - Short", PX, line100, Box{0, 0, 1000, 401}, 0),
	}, cfg(nil)))
	if noBase.TrackID != 5 || noBase.Name != "Gamma" {
		t.Errorf("no base: %+v", noBase)
	}
}

// One of several crossed layouts with a clear lead in fit is confirmed, and the
// evidence names the next best.
func TestADominantLayoutIsConfirmed(t *testing.T) {
	d := New([]Track{
		mk(1, "Tight", PX, line100, Box{0, 0, 1000, 400}, 0),
		mk(2, "Loose", PX, line100, Box{0, 0, 1000, 600}, 0),
	}, cfg(nil))
	driveLaps(d, fullLap, 2)
	want(t, d.Match(), 1, Confirmed)
	if !strings.Contains(d.Match().Evidence, "next best Loose fits") {
		t.Error(d.Match().Evidence)
	}
}

// A zero Config is v1's tolerances, and a zero field of one takes v1's value:
// New(nil, Config{}) detects as v1 does, rather than reading every step as a jump.
func TestAZeroConfigIsV1s(t *testing.T) {
	if got := New(nil, Config{}).cfg; got != Defaults {
		t.Errorf("Config{}: %+v", got)
	}
	got := New(nil, Config{BoxTolerance: 20, StartRadius: 500}).cfg
	want := Defaults
	want.BoxTolerance, want.StartRadius = 20, 500
	if got != want {
		t.Errorf("two fields set: %+v, want %+v", got, want)
	}
	d := New(rectangle(), Config{})
	drive(d, fullLap, 1)
	drive(d, fullLap[:3], 2)
	id, _ := d.Match().Decision()
	if id != 1 || d.DistanceM() == 0 {
		t.Errorf("Config{}: %+v after %.0f m", d.Match(), d.DistanceM())
	}
}

func TestConfidenceNames(t *testing.T) {
	for c, want := range map[Confidence]string{None: "none", Ambiguous: "ambiguous", Probable: "probable", Confirmed: "confirmed"} {
		if c.String() != want {
			t.Errorf("%d: %q", c, c.String())
		}
	}
	if None >= Ambiguous || Ambiguous >= Probable || Probable >= Confirmed {
		t.Error("not in order")
	}
}

// The whole table, when New is given none, leaves out the rows with no geometry;
// an empty list is none, as in v1.
func TestNewReadsTheTable(t *testing.T) {
	for _, tracks := range [][]Track{nil, {}} {
		d := New(tracks, Defaults)
		if len(d.tracks) != 105 || d.IsAlive(1261) || !d.IsAlive(sugo) {
			t.Errorf("%d layouts", len(d.tracks))
		}
	}
	d := New(nil, Defaults)
	if m := d.Match(); m.Confidence != None || m.Evidence != "no position data yet" {
		t.Errorf("%+v", m)
	}
}
