package track

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Learning a row of the table from a recorded lap (paddock#300): v1's learn_track
// and LearnedTrack, from tracktools.py at v1.0.0, ported rule for rule. The rows
// with ids from 100001 (SUGO, Circuit Gilles Villeneuve) came from it. It is fed
// points, not a file: reading the recording is the caller's.

// LapPoint is a position the car was at, with GT7's lap counter while racing (in
// race or finished) and NoLap otherwise (at the track before the start). The
// caller gives no point for a frame in a menu, a loading screen or a pause, as v1
// read only those three phases.
type LapPoint struct {
	Lap int32
	P   Point
}

// Nearby is a row of the table whose finish line is near a point: the distance,
// rounded to the metre as v1 rounded it (half to even), the id and the name.
type Nearby struct {
	DistanceM float64
	ID        int32
	Name      string
}

// Learned is a table row derived from a recorded lap (v1's LearnedTrack).
type Learned struct {
	// Line is the finish line, Direction the way the car crossed it.
	Line      [2]Point
	Direction Direction
	// Box is the bounding box of every racing point from lap 1 on.
	Box Box
	// LapLengthM is the length of the last completed lap, and LapsUsed the laps
	// completed.
	LapLengthM float64
	LapsUsed   int
	// Nearest are the five rows whose finish lines are nearest the learned one's
	// middle, nearest first.
	Nearest []Nearby
}

// ErrNoLap is Learn's error for points with no completed lap.
var ErrNoLap = errors.New("no completed lap (the lap counter never incremented)")

// LearnHalfWidth is v1's half width of a learned line, in metres.
const LearnHalfWidth = 15.0

// Learn derives a finish line, its direction and the lap box from the points, as
// v1's learn_track: the line is where GT7's lap counter first went up, across the
// direction of travel and halfWidth either side of the car; the box covers every
// racing point from lap 1 on; the length is the last completed lap's. tracks are
// the rows to measure the nearest lines against, the whole Table when none are
// given. Points with no completed lap give ErrNoLap.
func Learn(points []LapPoint, tracks []Track, halfWidth float64) (Learned, error) {
	if len(tracks) == 0 {
		tracks = Table()
	}
	var (
		boundaries [][2]Point
		lastLap    int32
		hasLast    bool
		prev       *Point
	)
	for _, pt := range points {
		racing := pt.Lap != NoLap
		if racing && hasLast && pt.Lap > lastLap && lastLap >= 1 && prev != nil {
			boundaries = append(boundaries, [2]Point{*prev, pt.P})
		}
		if racing {
			lastLap, hasLast = pt.Lap, true
		}
		p := pt.P
		prev = &p
	}
	if len(boundaries) == 0 {
		return Learned{}, ErrNoLap
	}
	a, b := boundaries[0][0], boundaries[0][1]
	direction := travelDirection(a, b)
	if direction == DirectionNone {
		direction = PX
	}
	dx, dz := b.X-a.X, b.Z-a.Z
	length := math.Hypot(dx, dz)
	if length == 0 {
		length = 1
	}
	nx, nz := -dz/length, dx/length // across the direction of travel
	line := [2]Point{
		{b.X - nx*halfWidth, b.Z - nz*halfWidth},
		{b.X + nx*halfWidth, b.Z + nz*halfWidth},
	}

	var racing []Point
	for _, pt := range points {
		if pt.Lap != NoLap && pt.Lap >= 1 {
			racing = append(racing, pt.P)
		}
	}
	box := Box{racing[0].X, racing[0].Z, racing[0].X, racing[0].Z}
	for _, p := range racing {
		box = Box{min(box.MinX, p.X), min(box.MinZ, p.Z), max(box.MaxX, p.X), max(box.MaxZ, p.Z)}
	}
	// The last completed lap is the one before the lap counter's last value (v1's
	// `(last_lap or 1) - 1`, where a last lap of 0 counts as 1); with no point on
	// it, every racing point is.
	lap := lastLap
	if lap == 0 {
		lap = 1
	}
	var lapPts []Point
	for _, pt := range points {
		if pt.Lap != NoLap && pt.Lap == lap-1 {
			lapPts = append(lapPts, pt.P)
		}
	}
	if len(lapPts) == 0 {
		lapPts = racing
	}
	var lapLen float64
	for i := 1; i < len(lapPts); i++ {
		lapLen += math.Hypot(lapPts[i].X-lapPts[i-1].X, lapPts[i].Z-lapPts[i-1].Z)
	}
	return Learned{
		Line: line, Direction: direction, Box: box, LapLengthM: lapLen, LapsUsed: len(boundaries),
		Nearest: NearestLines(b, tracks, 5),
	}, nil
}

// NearestLines are the n rows of tracks with geometry whose finish lines are
// nearest p, nearest first, then by id and name, as v1 sorted them.
func NearestLines(p Point, tracks []Track, n int) []Nearby {
	type near struct {
		d float64
		t *Track
	}
	var all []near
	for i := range tracks {
		if t := &tracks[i]; t.HasGeometry {
			all = append(all, near{distanceToSegment(p, t.Line[0], t.Line[1]), t})
		}
	}
	slices.SortStableFunc(all, func(a, b near) int {
		switch {
		case a.d != b.d:
			return cmpFloat(a.d, b.d)
		case a.t.ID != b.t.ID:
			return int(a.t.ID - b.t.ID)
		default:
			return strings.Compare(a.t.Name, b.t.Name)
		}
	})
	out := make([]Nearby, 0, n)
	for _, x := range all[:min(n, len(all))] {
		out = append(out, Nearby{DistanceM: math.RoundToEven(x.d), ID: x.t.ID, Name: x.t.Name})
	}
	return out
}

func cmpFloat(a, b float64) int {
	if a < b {
		return -1
	}
	return 1
}

// CSVRow is the row in tracks.csv's format, under the id and name given, as v1's
// csv_row writes it: coordinates to the millimetre, the length to the metre.
func (l Learned) CSVRow(id int32, name string) string {
	fields := []string{strconv.Itoa(int(id)), name, l.Direction.String()}
	for _, v := range []float64{
		l.Line[0].X, l.Line[0].Z, l.Line[1].X, l.Line[1].Z, l.Box.MinX, l.Box.MinZ, l.Box.MaxX, l.Box.MaxZ,
	} {
		fields = append(fields, fmt.Sprintf("%.3f", v))
	}
	return strings.Join(append(fields, fmt.Sprintf("%.0f", l.LapLengthM)), ",")
}
