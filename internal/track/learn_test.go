package track

import (
	"errors"
	"testing"
)

// R3 and R4 (paddock#300): Learn is v1's learn_track. cmd/ecu's tests hold it to
// v1's output on real laps; these are the edges.
func TestLearn(t *testing.T) {
	if _, err := Learn([]LapPoint{{NoLap, Point{0, 0}}, {1, Point{1, 0}}, {1, Point{2, 0}}}, nil, LearnHalfWidth); !errors.Is(err, ErrNoLap) {
		t.Errorf("no completed lap: %v, want ErrNoLap", err)
	}
	// Two laps of a square, heading +z at the line; the line across it, 15 m
	// either side; the length that of the last lap completed.
	var pts []LapPoint
	square := []Point{{0, 0}, {0, 100}, {100, 100}, {100, 0}}
	for lap := int32(1); lap <= 2; lap++ {
		for _, p := range square {
			pts = append(pts, LapPoint{lap, p})
		}
	}
	pts = append(pts, LapPoint{3, Point{0, 1}})
	l, err := Learn(pts, nil, LearnHalfWidth)
	if err != nil {
		t.Fatal(err)
	}
	if l.LapsUsed != 2 || l.Direction != NX || l.LapLengthM != 300 || l.Box != (Box{0, 0, 100, 100}) {
		t.Errorf("learned %+v", l)
	}
	if got, want := l.CSVRow(7, "Square"), "7,Square,NX,0.000,15.000,0.000,-15.000,0.000,0.000,100.000,100.000,300"; got != want {
		t.Errorf("row %s, want %s", got, want)
	}
	if len(l.Nearest) != 5 {
		t.Errorf("%d nearest lines, want 5", len(l.Nearest))
	}
}

// v1 rounds a distance half to even, and sorts ties by id.
func TestNearestLines(t *testing.T) {
	tracks := []Track{
		{ID: 2, Name: "B", Line: [2]Point{{2.5, 0}, {2.5, 1}}, HasGeometry: true},
		{ID: 1, Name: "A", Line: [2]Point{{2.5, 0}, {2.5, 1}}, HasGeometry: true},
		{ID: 3, Name: "C", Line: [2]Point{{3.5, 0}, {3.5, 1}}, HasGeometry: true},
		{ID: 4, Name: "D"},
	}
	got := NearestLines(Point{}, tracks, 4)
	want := []Nearby{{2, 1, "A"}, {2, 2, "B"}, {4, 3, "C"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %v, want %v", i, got[i], want[i])
		}
	}
}
