package track

import (
	"math"
	"testing"
)

func TestSegmentCrossingAndTheDirectionConvention(t *testing.T) {
	a, b := Point{100, 0}, Point{100, 50}
	for _, c := range []struct {
		p, q Point
		want bool
		why  string
	}{
		{Point{90, 25}, Point{110, 25}, true, "across"},
		{Point{90, 50}, Point{110, 50}, true, "touching the end counts"},
		{Point{90, 60}, Point{110, 60}, false, "past the end"},
		{Point{0, 25}, Point{50, 25}, false, "short of it"},
		{Point{90, 25}, Point{90, 30}, false, "parallel"},
	} {
		if got := segmentCrosses(c.p, c.q, a, b); got != c.want {
			t.Errorf("%s: %v", c.why, got)
		}
	}
	// The table keys direction on the sign of x first, then z: not the dominant axis.
	for q, want := range map[Point]Direction{{-1, 100}: NX, {1, -100}: PX, {0, 5}: PZ, {0, -5}: NZ, {0, 0}: DirectionNone} {
		if got := travelDirection(Point{}, q); got != want {
			t.Errorf("travelDirection to %v = %v, want %v", q, got, want)
		}
	}
}

func TestBoxesAndDistances(t *testing.T) {
	if !boxContains(Box{0, 0, 100, 100}, Box{10, 10, 90, 90}, 0) ||
		boxContains(Box{0, 0, 100, 100}, Box{10, 10, 120, 90}, 0) ||
		!boxContains(Box{0, 0, 100, 100}, Box{10, 10, 120, 90}, 25) {
		t.Error("boxContains")
	}
	if boxIoU(Box{0, 0, 10, 10}, Box{0, 0, 10, 10}) != 1 ||
		math.Abs(boxIoU(Box{0, 0, 10, 10}, Box{5, 0, 15, 10})-1.0/3) > 1e-12 ||
		boxIoU(Box{0, 0, 10, 10}, Box{20, 20, 30, 30}) != 0 ||
		boxIoU(Box{}, Box{}) != 0 {
		t.Error("boxIoU")
	}
	if distanceToSegment(Point{5, 5}, Point{0, 0}, Point{10, 0}) != 5 ||
		distanceToSegment(Point{20, 0}, Point{0, 0}, Point{10, 0}) != 10 ||
		distanceToSegment(Point{3, 4}, Point{0, 0}, Point{0, 0}) != 5 {
		t.Error("distanceToSegment")
	}
}

func TestWidenSegmentKeepsItsMidpointAndDirection(t *testing.T) {
	got := widenSegment(Point{0, 0}, Point{0, 20}, 50)
	if math.Abs(got[0].X) > 1e-9 || math.Abs(got[0].Z+40) > 1e-9 || math.Abs(got[1].X) > 1e-9 || math.Abs(got[1].Z-60) > 1e-9 {
		t.Errorf("got %v", got)
	}
	if got := widenSegment(Point{1, 1}, Point{1, 1}, 50); got != [2]Point{{1, 1}, {1, 1}} {
		t.Errorf("a point: %v", got)
	}
}
