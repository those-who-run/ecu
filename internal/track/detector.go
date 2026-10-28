package track

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

// Confidence is how sure a match is, from none to confirmed; a higher one is
// surer.
type Confidence uint8

// The confidences, in order.
const (
	None Confidence = iota
	Ambiguous
	Probable
	Confirmed
)

func (c Confidence) String() string {
	switch c {
	case Ambiguous:
		return "ambiguous"
	case Probable:
		return "probable"
	case Confirmed:
		return "confirmed"
	default:
		return "none"
	}
}

// MaxReportedCandidates caps the candidates of a match that names no layout: an
// unanchored pool can be most of the table.
const MaxReportedCandidates = 8

// NoLap is the lap given to ObservePoint for a frame that is not racing, where
// GT7's lap counter means nothing.
const NoLap int32 = -1

// Candidate is a layout that still fits.
type Candidate struct {
	Track       *Track
	LineCrossed bool
	// Fit is the overlap (IoU) of the driven box and the layout's box, 0 to 1.
	Fit float64
}

// ranks says a ranks above b: crossed first, then the best fit, then the tighter
// layout.
func ranks(a, b Candidate) bool {
	if a.LineCrossed != b.LineCrossed {
		return a.LineCrossed
	}
	if a.Fit != b.Fit {
		return a.Fit > b.Fit
	}
	return a.Track.boxArea() < b.Track.boxArea()
}

// rank sorts the candidates best first, keeping the table's order among equals.
func rank(c []Candidate) {
	sort.SliceStable(c, func(i, j int) bool { return ranks(c[i], c[j]) })
}

// Match is the detector's verdict.
type Match struct {
	// TrackID is the layout named, 0 when none is; Name is its name, or the
	// circuit's when Family is set.
	TrackID int32
	Name    string
	// Confidence is how sure the verdict is.
	Confidence Confidence
	// Candidates are the layouts that still fit, best first.
	Candidates []Candidate
	// Evidence says, in words, why.
	Evidence string
	// Family is set when the layout is unknown but every crossed candidate is the
	// same circuit (Fuji and Fuji Short share a line and a box in the table): the
	// match is then the circuit, Name is this, and Candidates the layouts.
	Family string
}

// Decision is what callers act on: the track and how sure the verdict is. The
// candidates, their fit and the wording shift every frame while the pool narrows;
// those are on the match but do not count as a change.
func (m Match) Decision() (int32, Confidence) { return m.TrackID, m.Confidence }

// CandidateIDs are the candidates' ids, best first.
func (m Match) CandidateIDs() []int32 {
	ids := make([]int32, len(m.Candidates))
	for i, c := range m.Candidates {
		ids[i] = c.Track.ID
	}
	return ids
}

// noMatch is the match before any position.
var noMatch = Match{Confidence: None, Evidence: "no position data yet"}

// Config is the detector's tolerances, in metres unless they say otherwise.
type Config struct {
	// BoxTolerance is how far the path may leave a layout's box.
	BoxTolerance float64
	// StartRadius is how near a finish line the car must have been.
	StartRadius float64
	// Teleport is the step beyond which the car jumped rather than drove.
	Teleport float64
	// MinDistance is how far the car drives before a lone layout is probable.
	MinDistance float64
	// Dominance is the lead in fit that picks one of several crossed layouts.
	Dominance float64
	// LineRadius is how far from a layout's line a lap boundary may be.
	LineRadius float64
	// LineHalfWidth is half the width a table line is widened to.
	LineHalfWidth float64
	// TickRadius is how near a line lap 1 must start to settle it.
	TickRadius float64
	// MinFit is the least a whole lap must fill a layout's box (IoU).
	MinFit float64
	// LengthTolerance is how far off a known lap length a lap may be, a fraction.
	LengthTolerance float64
}

// Defaults are v1's tolerances.
var Defaults = Config{
	BoxTolerance:    50,
	StartRadius:     1000,
	Teleport:        100,
	MinDistance:     500,
	Dominance:       0.05,
	LineRadius:      150,
	LineHalfWidth:   50,
	TickRadius:      30,
	MinFit:          0.5,
	LengthTolerance: 0.08,
}

// Detector recognises the track layout from where the car drives: v1's
// TrackDetector (trackdetect.py at v1.0.0), ported rule for rule, with v1's
// docstrings as its comments.
//
// GT7 sends no track id. gt-telem guesses one by matching a lap's bounding box
// against its table at 95% overlap, after lap 1, only when "cars on track" is set;
// that missed the Nürburgring 24h layout outright (2026-10-14) because the table's
// box is ~580 m taller than the real lap, and it never runs in practice.
//
// This detector keeps a set of candidate layouts and narrows it with independent
// physical facts, each cheap to check on every frame:
//
//  1. Starts near the line. A grid or a pit lane sits within a few hundred metres
//     of the layout's finish line. This is what separates the real layout from
//     unrelated tracks whose (huge) boxes happen to contain the same coordinates,
//     such as Special Stage Route X.
//  2. Path stays inside the box. Every layout is dropped as soon as the car leaves
//     its bounding box (with a tolerance for table error). Family members that
//     share a finish line (GP vs 24h, Indy vs full) fall out as the car drives
//     beyond the shorter layout.
//  3. Finish line crossed, in the right direction. The decisive fact, but it only
//     arrives at the end of the first lap. Reverse layouts share a line and a box
//     with their forward twin and differ only here. Two witnesses: GT7's own lap
//     counter (when it increments the car is on the line, whatever the table
//     says) and the car's path cutting the table's segment, widened to span the
//     track because the recorded segments are only a car's width. The lap counter
//     outranks the path. Lines of unrelated layouts lie all over a circuit, so a
//     path crossing counts only while racing and only until the lap counter has
//     shown where this circuit's line is: at Fuji the pit exit runs through Tokyo
//     East's line, and a lap of Tokyo East cuts Suzuka's (2026-10-15).
//  4. A completed lap fits. The lap the counter closes is a whole lap of the
//     circuit: a layout whose box it fills poorly, or whose known length it
//     misses, is ruled out (lapBoundary).
//
// Joined part way (Join: the engineer started mid-lap, #265), facts 1 and 3 are
// not what they seem: the first position is wherever the car was, not near a
// line, and the path to the next tick is part of a lap. Lines of other layouts lie
// all over a circuit, so a path crossing counts for nothing, and the first lap
// boundary only says where the line is: the box and the distance before it are not
// a whole lap, so no layout is judged on them. At Suzuka (2026-10-18) the start
// mid-lap was near Lago Maggiore's line and the path crossed it; the detector said
// "probable Lago Maggiore - East Reverse" 28 s in.
//
// Ranking: layouts whose line has been crossed outrank the rest; among those,
// higher overlap between the driven box and the layout's box wins. Confidence is
// reported honestly: some pairs (Fuji vs Fuji Short, Spa vs Spa 24h, Monza vs No
// Chicane: Twins) have identical geometry in the table, and the layout stays
// unknown forever. The circuit is known, though: a pool of crossed layouts that is
// all one circuit is a probable match named by the circuit.
//
// The detector is pure: feed it frames, read Match. It is not safe for concurrent
// use.
type Detector struct {
	cfg    Config
	tracks []Track // the rows with geometry
	index  map[int32]int
	// lines are the tracks' lines, widened: table lines are two frames of one
	// car's crossing, 7 to 68 m long, and can sit a track-width away from where
	// the next driver crosses.
	lines []([2]Point)

	pendingReset bool
	anchors      []Point // places the car is known to be on the circuit
	prev         *Point
	velocity     Point
	box          *Box
	distance     float64
	lastLap      int32
	hasLastLap   bool
	lapDistance  float64 // metres since the last lap boundary
	lapComplete  bool    // a full lap has been driven: the box is the lap's box
	lapLength    float64 // length of the last completed lap, 0 before one
	crossed      []bool
	lineKnown    bool // the lap counter has put the car on a layout's line
	alive        []bool
	match        Match
	joined       bool // see Join
	partial      bool // the lap in progress is the one joined part way
}

// New is a detector over the tracks (the whole Table when nil) with the
// tolerances given. Rows without geometry are left out.
func New(tracks []Track, cfg Config) *Detector {
	if tracks == nil {
		tracks = Table()
	}
	d := &Detector{cfg: cfg, index: map[int32]int{}}
	for _, t := range tracks {
		if t.HasGeometry {
			d.index[t.ID] = len(d.tracks)
			d.tracks = append(d.tracks, t)
			d.lines = append(d.lines, widenSegment(t.Line[0], t.Line[1], cfg.LineHalfWidth))
		}
	}
	d.Reset()
	return d
}

// Reset forgets everything: call on a race restart (the car is teleported to the
// grid) or a new start.
func (d *Detector) Reset() {
	*d = Detector{
		cfg:     d.cfg,
		tracks:  d.tracks,
		index:   d.index,
		lines:   d.lines,
		crossed: make([]bool, len(d.tracks)),
		alive:   make([]bool, len(d.tracks)),
		match:   noMatch,
	}
	for i := range d.alive {
		d.alive[i] = true
	}
}

// Join says the frames start part way through a lap (see Detector): call before
// the first one.
func (d *Detector) Join() {
	d.Reset()
	d.joined, d.partial = true, true
}

// Match is the current verdict.
func (d *Detector) Match() Match { return d.match }

// IsAlive says the layout still fits everything seen since the last reset.
func (d *Detector) IsAlive(id int32) bool {
	i, ok := d.index[id]
	return ok && d.alive[i]
}

// DistanceM is the metres driven since the reset, teleports excluded.
func (d *Detector) DistanceM() float64 { return d.distance }

// LapLengthM is the length of the last completed lap, once the lap counter has
// closed one, and 0 before.
func (d *Detector) LapLengthM() float64 { return d.lapLength }

// Observe feeds one frame. It returns the match, and whether its decision changed.
//
// Evidence accumulates from the first frame at a track, through the pre-race
// phase (a rolling start drives most of a kilometre before lap 1 begins) and
// across pauses. Coming back from a menu or a loading screen means a new venue,
// so the next positioned frame starts over.
//
// Except a loading screen that keeps the lap counter: that is GT7's pit cut-scene
// in the middle of a race (SUGO, Suzuka, Willow Springs, Gilles Villeneuve; at Spa
// on 2026-10-18 it wiped the track after five laps, #275). The car is at the same
// circuit, often a few hundred metres on, so the lap it is on is not a whole lap
// of driving and is judged on its line only. A restart, a quit or a new event
// loads at lap 0.
func (d *Detector) Observe(f *telemetryv1.Frame) (Match, bool) {
	phase := f.GetPhase()
	switch phase {
	case telemetryv1.Phase_PHASE_LOADING:
		if f.GetLap() >= 1 {
			d.partial = true
		} else {
			d.pendingReset = true
		}
		return d.match, false
	case telemetryv1.Phase_PHASE_MENU:
		d.pendingReset = true
		return d.match, false
	case telemetryv1.Phase_PHASE_AT_TRACK, telemetryv1.Phase_PHASE_IN_RACE, telemetryv1.Phase_PHASE_FINISHED:
	default:
		return d.match, false // paused: the car is where it was
	}
	if d.pendingReset {
		d.Reset()
	}
	// The chequered flag is a lap boundary too: a lap-count race reports
	// total_laps + 1 with the FINISHED phase on the frame that closes the last lap.
	lap := NoLap
	if phase != telemetryv1.Phase_PHASE_AT_TRACK {
		lap = f.GetLap()
	}
	return d.ObservePoint(Point{f.GetPosX(), f.GetPosZ()}, Point{f.GetVelX(), f.GetVelZ()}, lap)
}

// ObservePoint feeds one position. lap is GT7's lap counter while racing, NoLap
// otherwise. It returns the match, and whether its decision changed.
//
// velocity is the car's world-frame (x, z) velocity on this frame. When it is not
// zero, it sets the direction of travel used against the table's line
// directions; otherwise the step from the previous position does. One position
// step is a poor witness: a frozen frame has none, and on a straight running
// mostly along z one frame of drift can flip its x sign.
//
// Anchors (positions known to be on the circuit, used to find layouts whose
// finish line is close by): the first frame, the first racing frame, and every
// point where the lap counter incremented. The first frame alone is not enough:
// GT7's pre-session screens report positions that are not the car's.
func (d *Detector) ObservePoint(p, velocity Point, lap int32) (Match, bool) {
	racing := lap != NoLap
	d.velocity = velocity
	if len(d.anchors) == 0 && !d.joined {
		d.anchors = append(d.anchors, p)
	}
	if d.box == nil {
		d.box = &Box{p.X, p.Z, p.X, p.Z}
	} else {
		d.box = &Box{min(d.box.MinX, p.X), min(d.box.MinZ, p.Z), max(d.box.MaxX, p.X), max(d.box.MaxZ, p.Z)}
	}
	moved := false
	if d.prev != nil {
		step := math.Hypot(p.X-d.prev.X, p.Z-d.prev.Z)
		if step < d.cfg.Teleport {
			moved = true
			d.distance += step
			d.lapDistance += step
			if racing && !d.lineKnown && !d.joined {
				d.noteCrossings(*d.prev, p)
			}
		}
		// else: reset to pits, restart, or a replayed results screen; the
		// segment is not a path the car drove.
	}
	if racing {
		switch {
		case d.joined && !d.hasLastLap:
			// mid-lap: neither an anchor nor the start of a lap
		case (!d.hasLastLap || d.lastLap < 1) && lap >= 1:
			d.anchors = append(d.anchors, p)
			if moved {
				d.firstLap(*d.prev, p)
			}
		case d.hasLastLap && lap > d.lastLap && d.lastLap >= 1 && moved:
			d.lapBoundary(*d.prev, p)
		}
		d.lastLap, d.hasLastLap = lap, true
	}
	d.prev = &p
	d.eliminate()
	m := d.decide()
	id, conf := d.match.Decision()
	newID, newConf := m.Decision()
	d.match = m
	return m, newID != id || newConf != conf
}

// direction is which way the car is going between a and b: its velocity, else
// the step.
func (d *Detector) direction(a, b Point) Direction {
	if d.velocity.X != 0 || d.velocity.Z != 0 {
		return travelDirection(Point{}, d.velocity)
	}
	return travelDirection(a, b)
}

func (d *Detector) noteCrossings(a, b Point) {
	direction := d.direction(a, b)
	for i := range d.tracks {
		if d.crossed[i] || !d.alive[i] {
			continue
		}
		if direction == d.tracks[i].Direction && segmentCrosses(a, b, d.lines[i][0], d.lines[i][1]) {
			d.crossed[i] = true
		}
	}
}

// firstLap: the lap counter started lap 1 between a and b.
//
// A flying or rolling start ticks on the finish line, which settles which lines
// were real: layouts whose line is under the car are crossed, and crossings of any
// other line were somewhere else on the circuit. A standing start ticks on the
// grid, away from every line, and settles nothing. Either way lap 1 starts here:
// driving to the line from the pit or the formation lap is not part of it.
func (d *Detector) firstLap(a, b Point) {
	direction := d.direction(a, b)
	onLine := make([]bool, len(d.tracks))
	found := false
	for i := range d.tracks {
		t := &d.tracks[i]
		if d.alive[i] && t.Direction == direction && distanceToSegment(b, t.Line[0], t.Line[1]) <= d.cfg.TickRadius {
			onLine[i], found = true, true
		}
	}
	if found {
		d.crossed = onLine
		d.lineKnown = true
	}
	d.lapDistance = 0
}

// lapBoundary: GT7 incremented the lap counter between a and b, so the car is on
// the finish line.
//
// Every live layout whose line is within LineRadius and faces the way the car is
// going has been crossed; every other live layout cannot be this circuit and is
// dropped. This is what makes a short or misplaced table segment survivable.
//
// The lap just closed is a whole lap of this circuit, so the driven box is now the
// lap's box and the distance since the previous boundary is its length. A layout
// whose box the lap fills poorly is a neighbour in coordinate space, not the
// circuit: a new track's line often lands near a known one's, because most
// layouts sit near their own origin. A known lap length that is off rules a layout
// out the same way.
//
// Lines can be up to LineRadius off, but a line much further away than the
// nearest one is another circuit's: Tokyo East's sits 118 m from Fuji's, beside
// the pit lane.
//
// When no live layout faces the way the car is going, it depends on how sure that
// direction is. With none at all (a frozen frame: no step, no velocity) or with
// the velocity and the step disagreeing (one frame of drift on a straight along
// z), the reading is suspect, so the boundary settles nothing and eliminates
// nothing. A direction both agree on is a fact: the car is driving a layout the
// table lacks, such as the reverse of a listed one, and every live layout goes.
func (d *Detector) lapBoundary(a, b Point) {
	direction := d.direction(a, b)
	d.anchors = append(d.anchors, b)
	dist := make([]float64, len(d.tracks))
	nearest, facing := math.Inf(1), false
	for i := range d.tracks {
		t := &d.tracks[i]
		dist[i] = math.Inf(1)
		if d.alive[i] && t.Direction == direction {
			dist[i] = distanceToSegment(b, t.Line[0], t.Line[1])
			nearest, facing = min(nearest, dist[i]), true
		}
	}
	step := travelDirection(a, b)
	whole := !d.partial
	d.partial = false
	if !facing && (direction == DirectionNone || (step != DirectionNone && step != direction)) {
		if whole {
			d.lapLength = d.lapDistance // still a whole lap, just no direction
		}
		d.lapDistance = 0
		return
	}
	if !facing {
		nearest = 0
	}
	reach := min(d.cfg.LineRadius, nearest+d.cfg.LineHalfWidth)
	for i := range d.tracks {
		if !d.alive[i] {
			continue
		}
		t := &d.tracks[i]
		if dist[i] > reach {
			d.alive[i] = false
			continue
		}
		d.crossed[i] = true
		if !whole {
			continue // joined part way: the line is all this lap says
		}
		if boxIoU(*d.box, t.Box) < d.cfg.MinFit ||
			(t.LengthM != 0 && math.Abs(d.lapDistance-t.LengthM) > d.cfg.LengthTolerance*t.LengthM) {
			d.alive[i] = false
		}
	}
	d.lineKnown = true
	if whole {
		d.lapComplete = true
		d.lapLength = d.lapDistance
	}
	d.lapDistance = 0
}

func (d *Detector) eliminate() {
	for i := range d.tracks {
		if d.alive[i] && !boxContains(d.tracks[i].Box, *d.box, d.cfg.BoxTolerance) {
			d.alive[i] = false
		}
	}
}

func (d *Detector) candidate(i int) Candidate {
	return Candidate{Track: &d.tracks[i], LineCrossed: d.crossed[i], Fit: boxIoU(*d.box, d.tracks[i].Box)}
}

func (d *Detector) decide() Match {
	var alive, near, crossedAny []int
	for i := range d.tracks {
		if !d.alive[i] {
			continue
		}
		alive = append(alive, i)
		if len(d.anchors) > 0 {
			t := &d.tracks[i]
			closest := math.Inf(1)
			for _, an := range d.anchors {
				closest = min(closest, distanceToSegment(an, t.Line[0], t.Line[1]))
			}
			if closest <= d.cfg.StartRadius {
				near = append(near, i)
			}
		}
		if d.crossed[i] {
			crossedAny = append(crossedAny, i)
		}
	}
	if len(alive) == 0 {
		return Match{Confidence: None, Evidence: "no layout fits the lap counter and path"}
	}
	// Anchor the candidate pool on something physical: a finish line close to
	// where the car has been, or a line actually crossed. Without either, dozens
	// of unrelated layouts contain the path.
	var pool []int
	switch {
	case len(near) > 0:
		pool = near
	case len(crossedAny) > 0:
		pool = crossedAny
	default:
		top := make([]Candidate, 0, len(alive))
		for _, i := range alive {
			c := d.candidate(i)
			c.LineCrossed = false
			top = append(top, c)
		}
		rank(top)
		why := fmt.Sprintf("no finish line within %.0f m of where the car has been"+
			" and none crossed yet; %d layouts contain the path", d.cfg.StartRadius, len(alive))
		return Match{Confidence: Ambiguous, Candidates: top[:min(len(top), MaxReportedCandidates)], Evidence: why}
	}
	cands := make([]Candidate, 0, len(pool))
	for _, i := range pool {
		cands = append(cands, d.candidate(i))
	}
	rank(cands)
	best := cands[0]
	var crossed []Candidate
	for _, c := range cands {
		if c.LineCrossed {
			crossed = append(crossed, c)
		}
	}
	if len(crossed) > 0 {
		unique := len(crossed) == 1
		dominant := len(crossed) > 1 && crossed[0].Fit-crossed[1].Fit >= d.cfg.Dominance
		var conf Confidence
		var why string
		switch {
		case (unique || dominant) && d.lapComplete:
			conf = Confirmed
			why = fmt.Sprintf("finish line crossed, a full lap fits the layout (%.2f)", best.Fit)
			if dominant {
				why += fmt.Sprintf("; next best %s fits %.2f", crossed[1].Track.Name, crossed[1].Fit)
			}
		case unique || dominant:
			conf = Probable
			why = "finish line crossed in the right direction, no full lap yet"
		default:
			families := map[string]bool{}
			for _, c := range crossed {
				families[c.Track.Family()] = true
			}
			if len(families) == 1 {
				return family(crossed[0].Track.Family(), crossed, cands)
			}
			conf = Ambiguous
			names := make([]string, 0, len(crossed)-1)
			for _, c := range crossed[1:] {
				names = append(names, c.Track.Name)
			}
			why = fmt.Sprintf("finish line shared with %s; bounds do not separate them", strings.Join(names, ", "))
		}
		return Match{TrackID: best.Track.ID, Name: best.Track.Name, Confidence: conf, Candidates: cands, Evidence: why}
	}
	if len(cands) == 1 && d.distance >= d.cfg.MinDistance {
		why := fmt.Sprintf("only layout containing the path after %.1f km, line not yet crossed", d.distance/1000)
		return Match{TrackID: best.Track.ID, Name: best.Track.Name, Confidence: Probable, Candidates: cands, Evidence: why}
	}
	// Several layouts and no crossing: naming one would be a guess.
	why := fmt.Sprintf("%d layouts near the start still fit, line not yet crossed", len(cands))
	return Match{Confidence: Ambiguous, Candidates: cands[:min(len(cands), MaxReportedCandidates)], Evidence: why}
}

// family: every crossed layout is one circuit and the geometry cannot pick one
// (Spa and Spa 24h, Fuji and Fuji Short: Twins). The circuit is what is known, so
// that is the match: probable, named by the circuit, on the layout that carries
// its name (else the lowest id), so the answer holds still while the fit of each
// twin swaps by a thousandth from frame to frame (#275).
func family(name string, crossed, cands []Candidate) Match {
	var base *Track
	for _, c := range crossed {
		if c.Track.Name == name {
			base = c.Track
			break
		}
	}
	if base == nil {
		for _, c := range crossed {
			if base == nil || c.Track.ID < base.ID {
				base = c.Track
			}
		}
	}
	names := make([]string, 0, len(crossed))
	for _, c := range crossed {
		names = append(names, c.Track.Name)
	}
	slices.Sort(names)
	why := fmt.Sprintf("finish line shared by %s; bounds do not separate them; all %s", strings.Join(names, ", "), name)
	return Match{TrackID: base.ID, Name: name, Confidence: Probable, Candidates: cands, Evidence: why, Family: name}
}
