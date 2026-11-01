package track

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// v1TableSum is the sha256 of v1's src/those_who_run/data/tracks.csv at v1.0.0.
const v1TableSum = "7fabb9897295d077563358cdc59be45997818361684f0404aa0cb470aa1c6a60"

// amendments are ecu's changes to v1's table, each a field of a row as v1.0.0 had
// it and as ecu has it (README.md, "Track detection", says why).
var amendments = []struct {
	id      int32
	v1, ecu string
}{
	// paddock#81: Fuji's maxx, from the live race of 2026-10-29, which reached
	// x 667.4.
	{fuji, "615.2125244140625", "670.0"},
	{fujiShort, "615.6531372070312", "670.0"},
}

// v1CSV is tracks.csv with the amendments undone: v1's at v1.0.0.
func v1CSV(tb testing.TB) string {
	tb.Helper()
	lines := strings.Split(tableCSV, "\n")
	for _, a := range amendments {
		prefix := strconv.Itoa(int(a.id)) + ","
		i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
		if i < 0 || strings.Count(lines[i], ","+a.ecu+",") != 1 {
			tb.Fatalf("row %d: no field %s to undo", a.id, a.ecu)
		}
		lines[i] = strings.Replace(lines[i], ","+a.ecu+",", ","+a.v1+",", 1)
	}
	return strings.Join(lines, "\n")
}

// v1Table is the table as v1.0.0 has it.
func v1Table(tb testing.TB) []Track {
	tb.Helper()
	tracks, err := parseTable(v1CSV(tb))
	if err != nil {
		tb.Fatal(err)
	}
	return tracks
}

// AC3 and R1: the table is v1's tracks.csv at v1.0.0, byte for byte but for the
// amendments, and reads as v1 read it: 108 layouts, 105 with geometry, and lengths
// only on the two rows we learned ourselves, from 100001.
func TestTheTableIsV1s(t *testing.T) {
	if sum := sha256.Sum256([]byte(v1CSV(t))); hex.EncodeToString(sum[:]) != v1TableSum {
		t.Errorf("tracks.csv without the amendments: sha256 %x, v1's is %s", sum, v1TableSum)
	}
	for _, a := range amendments {
		tr, _ := ByID(a.id)
		if want, _ := strconv.ParseFloat(a.ecu, 64); tr.Box.MaxX != want {
			t.Errorf("%d: maxx %v, want %s", a.id, tr.Box.MaxX, a.ecu)
		}
	}
	tracks := Table()
	var geometry int
	var lengths []int32
	for _, tr := range tracks {
		if tr.HasGeometry {
			geometry++
			if tr.Direction == DirectionNone {
				t.Errorf("%d: geometry and no direction", tr.ID)
			}
			if tr.Box.MinX >= tr.Box.MaxX || tr.Box.MinZ >= tr.Box.MaxZ {
				t.Errorf("%d: box %+v", tr.ID, tr.Box)
			}
		}
		if tr.LengthM != 0 {
			lengths = append(lengths, tr.ID)
		}
	}
	if len(tracks) != 108 || geometry != 105 {
		t.Errorf("%d layouts, %d with geometry; want 108 and 105", len(tracks), geometry)
	}
	if !slices.Equal(lengths, []int32{sugo, gillesVilleneuve}) {
		t.Errorf("lengths on %v", lengths)
	}
	for id, want := range map[int32]string{
		sugo: "Sportsland SUGO", n24h: "Nurburgring 24h", 471: "Tsukuba Circuit",
		1225: "Alsace - Test Course Reverse", 999999: "track 999999",
	} {
		if got := Name(id); got != want {
			t.Errorf("Name(%d) = %q, want %q", id, got, want)
		}
	}
	if tr, _ := ByID(n24h); tr.Direction != NX || !tr.HasGeometry {
		t.Errorf("Nurburgring 24h: %+v", tr)
	}
	if tr, _ := ByID(sugo); tr.LengthM != 3584 || tr.Direction != PX {
		t.Errorf("SUGO: %+v", tr)
	}
	if tr, _ := ByID(1261); tr.HasGeometry {
		t.Errorf("Special Stage Route X 400 has no geometry in the table: %+v", tr)
	}
}

// The table's z axis is "Y" in the source; a bad row is an error.
func TestParseTable(t *testing.T) {
	header := strings.Join(tableColumns, ",") + "\n"
	got, err := parseTable("# a comment, with commas\n" + header + "1,A,PY,0,0,0,10,5,5,-5,-5,\n2,B,NY,,,,,,,,,\n")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Direction != PZ || got[1].Direction != NZ || got[1].HasGeometry {
		t.Errorf("got %+v", got)
	}
	if got[0].Box != (Box{-5, -5, 5, 5}) {
		t.Errorf("box %+v, want the corners sorted", got[0].Box)
	}
	for _, bad := range []string{
		"",
		"id,name\n",
		header + "x,A,PX,0,0,0,0,0,0,1,1,\n",
		header + "1,A,QX,0,0,0,0,0,0,1,1,\n",
		header + "1,A,PX,0,0,0,y,0,0,1,1,\n",
		header + "1,A,PX,0,0,0,0,0,0,1,1,z\n",
		header + "1,A,PX\n",
	} {
		if _, err := parseTable(bad); err == nil {
			t.Errorf("%q: read, want an error", bad)
		}
	}
}

func TestDirectionNames(t *testing.T) {
	for d, want := range map[Direction]string{PX: "PX", NX: "NX", PZ: "PZ", NZ: "NZ", DirectionNone: ""} {
		if d.String() != want {
			t.Errorf("%d: %q", d, d.String())
		}
	}
}

func TestFamilyNameDropsTheLayoutPart(t *testing.T) {
	for name, want := range map[string]string{
		"Fuji International Speedway (Short)": "Fuji International Speedway",
		"Tokyo Expressway - East Clockwise":   "Tokyo Expressway",
		"Nurburgring 24h":                     "Nurburgring 24h",
	} {
		if got := FamilyName(name); got != want {
			t.Errorf("FamilyName(%q) = %q, want %q", name, got, want)
		}
	}
}

// Each twin shares a line and a box with the layout named after its circuit
// (v1's test_twins.py).
func TestEachTwinSharesALineAndABoxWithItsCircuit(t *testing.T) {
	byName := map[string]Track{}
	for _, tr := range Table() {
		byName[tr.Name] = tr
	}
	for id, family := range Twins {
		twin, _ := ByID(id)
		base, ok := byName[family]
		if !ok {
			t.Errorf("%d: no layout named %q", id, family)
			continue
		}
		if twin.Family() != family || base.Family() != family {
			t.Errorf("%d: families %q and %q", id, twin.Family(), base.Family())
		}
		if twin.Direction != base.Direction {
			t.Errorf("%d: directions differ", id)
		}
		for i := range 2 {
			if math.Hypot(twin.Line[i].X-base.Line[i].X, twin.Line[i].Z-base.Line[i].Z) >= 0.5 {
				t.Errorf("%d: lines differ at p%d", id, i+1)
			}
		}
		if boxIoU(twin.Box, base.Box) < 0.98 {
			t.Errorf("%d: boxes overlap %.3f", id, boxIoU(twin.Box, base.Box))
		}
	}
	if tr, _ := ByID(spa24h); tr.Family() != spaCircuit {
		t.Errorf("Spa 24h's family %q", tr.Family())
	}
	if tr, _ := ByID(fujiShort); tr.Family() != fujiCircuit {
		t.Errorf("Fuji Short's family %q", tr.Family())
	}
}
