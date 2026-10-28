// Package track is ecu's track table and track detector (those-who-run/paddock#80):
// GT7's telemetry carries no track id, so the layout has to be recognised from
// where the car goes. It is v1's tracks.py and trackdetect.py, from
// those-who-run/those-who-run at v1.0.0, ported rule for rule.
//
// The table is tracks.csv, embedded in the binary: v1's
// src/those_who_run/data/tracks.csv at v1.0.0, unchanged (adapted from gt-telem,
// which adapted it from Bornhall/gt7telemetry, both GPL-3.0; NOTICE). Coordinates
// are GT7 world x/z in metres; each track has its own origin, so boxes of
// unrelated tracks overlap freely and location alone never identifies a layout.
// Rows learned from our own laps have ids from 100001.
//
// The Detector is pure: it is fed frames and has no clock, no network and no
// files. Publishing its verdict is the caller's (paddock#81). A lint rule in
// .golangci.yml keeps NATS, the network and the file system out of this package.
package track

import (
	_ "embed" // the table, tracks.csv
	"encoding/csv"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

//go:embed tracks.csv
var tableCSV string

// Point is a position in GT7's world, x and z in metres.
type Point struct{ X, Z float64 }

// Box is a bounding box in GT7's world: min x, min z, max x and max z.
type Box struct{ MinX, MinZ, MaxX, MaxZ float64 }

// Direction is the way a car crosses a finish line, as the table encodes it: the
// sign of its x movement, or of its z movement when x did not change at all
// (travelDirection).
type Direction uint8

// The directions. DirectionNone is no movement, or a row without one.
const (
	DirectionNone Direction = iota
	PX
	NX
	PZ
	NZ
)

func (d Direction) String() string {
	switch d {
	case PX:
		return "PX"
	case NX:
		return "NX"
	case PZ:
		return "PZ"
	case NZ:
		return "NZ"
	default:
		return ""
	}
}

// Track is one layout of the table.
type Track struct {
	ID   int32
	Name string
	// Direction is the sign of travel across the line, DirectionNone when the row
	// has none.
	Direction Direction
	// Line is the finish line, p1 to p2, and Box the bounding box of a lap; both
	// are set only when HasGeometry.
	Line        [2]Point
	Box         Box
	HasGeometry bool
	// LengthM is the lap length when known, 0 otherwise; it separates layouts
	// with one box.
	LengthM float64
}

// Twins are the layouts the table cannot tell apart, by id, and the circuit each
// belongs to where the name does not say (FamilyName reads it from the name
// otherwise). Each shares its finish line and direction with its base layout to
// 0.1 m, the two boxes overlap 0.98 or more, and the table has no lap length for
// either: a lap fits both the same, so the detector names the circuit (#275).
// Fuji and Fuji Short are a pair like these whose names already say it.
//
// Spa (2026-10-18, #275): nothing we have separates the Grand Prix and 24h
// layouts. Whole laps in two outings (20261018-141557, 20261018-142627) were 6.92
// to 6.99 km and their box filled each row's box 0.993 to 0.999, the two never
// more than 0.006 apart, against the 0.05 the detector needs. GT7's telemetry has
// no layout id, and a pit stop is a loading cut-scene, so a pit lane path is never
// in the frames.
var Twins = map[int32]string{
	1269: "Circuit de Spa-Francorchamps",                     // Spa 24h Layout
	742:  "Autodromo Nazionale Monza",                        // Autodromo Nazionale Monza No Chicane
	854:  "24 Heures du Mans Racing Circuit",                 // 24 Heures du Mans Racing Circuit No Chicane
	1249: "Circuit de Barcelona-Catalunya Grand Prix Layout", // GP Layout No Chicane
}

// Family is the circuit the layout belongs to: see Twins and FamilyName.
func (t *Track) Family() string {
	if f, ok := Twins[t.ID]; ok {
		return f
	}
	return FamilyName(t.Name)
}

// boxArea is the area of the layout's box, infinite without one.
func (t *Track) boxArea() float64 {
	if !t.HasGeometry {
		return math.Inf(1)
	}
	return (t.Box.MaxX - t.Box.MinX) * (t.Box.MaxZ - t.Box.MinZ)
}

// FamilyName is the circuit a layout belongs to: the name without its layout part.
//
// "Fuji International Speedway (Short)" and "Tokyo Expressway - East Clockwise"
// give "Fuji International Speedway" and "Tokyo Expressway".
func FamilyName(name string) string {
	name, _, _ = strings.Cut(name, " (")
	name, _, _ = strings.Cut(name, " - ")
	return strings.TrimSpace(name)
}

var (
	tableOnce sync.Once
	table     []Track
	byID      map[int32]*Track
)

// Table is every row of the table, in its order. The slice is shared: callers
// must not change it.
func Table() []Track {
	tableOnce.Do(func() {
		var err error
		table, err = parseTable(tableCSV)
		if err != nil {
			panic("track: the embedded tracks.csv: " + err.Error())
		}
		byID = make(map[int32]*Track, len(table))
		for i := range table {
			byID[table[i].ID] = &table[i]
		}
	})
	return table
}

// ByID is the row with the id, if the table has one.
func ByID(id int32) (Track, bool) {
	Table()
	t, ok := byID[id]
	if !ok {
		return Track{}, false
	}
	return *t, true
}

// Name is the layout's name, or "track <id>" for an id the table lacks.
func Name(id int32) string {
	if t, ok := ByID(id); ok {
		return t.Name
	}
	return fmt.Sprintf("track %d", id)
}

// tableColumns are the table's columns, in order.
var tableColumns = []string{"id", "name", "direction", "p1x", "p1z", "p2x", "p2z", "minx", "minz", "maxx", "maxz", "length_m"}

// parseTable reads the table: comment lines start with #, then a header and one
// row per layout.
func parseTable(text string) ([]Track, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.Comment = '#'
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || strings.Join(rows[0], ",") != strings.Join(tableColumns, ",") {
		return nil, fmt.Errorf("the header is not %s", strings.Join(tableColumns, ","))
	}
	tracks := make([]Track, 0, len(rows)-1)
	for _, row := range rows[1:] {
		t, err := parseRow(row)
		if err != nil {
			return nil, fmt.Errorf("row %q: %w", strings.Join(row, ","), err)
		}
		tracks = append(tracks, t)
	}
	return tracks, nil
}

func parseRow(row []string) (Track, error) {
	id, err := strconv.ParseInt(row[0], 10, 32)
	if err != nil {
		return Track{}, err
	}
	t := Track{ID: int32(id), Name: row[1]}
	geo := row[3:11]
	if !containsEmpty(geo) {
		var v [8]float64
		for i, s := range geo {
			if v[i], err = strconv.ParseFloat(s, 64); err != nil {
				return Track{}, err
			}
		}
		t.Line = [2]Point{{v[0], v[1]}, {v[2], v[3]}}
		t.Box = Box{min(v[4], v[6]), min(v[5], v[7]), max(v[4], v[6]), max(v[5], v[7])}
		t.HasGeometry = true
	}
	// The source table calls the z axis "Y"; normalise so the names match the frame.
	switch d := strings.TrimSpace(row[2]); d {
	case "":
	case "PX":
		t.Direction = PX
	case "NX":
		t.Direction = NX
	case "PZ", "PY":
		t.Direction = PZ
	case "NZ", "NY":
		t.Direction = NZ
	default:
		return Track{}, fmt.Errorf("direction %q", d)
	}
	if length := strings.TrimSpace(row[11]); length != "" {
		if t.LengthM, err = strconv.ParseFloat(length, 64); err != nil {
			return Track{}, err
		}
	}
	return t, nil
}

func containsEmpty(values []string) bool {
	for _, v := range values {
		if v == "" {
			return true
		}
	}
	return false
}
