package record

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/those-who-run/salsa"
)

// The v1 recordings are salsa's testdata/v1/, read from the salsa module the build
// already uses, as internal/race's tests read them.
var (
	fixturesOnce sync.Once
	fixturesDir  string
	fixturesErr  error
)

func fixtures(tb testing.TB) string {
	tb.Helper()
	fixturesOnce.Do(func() {
		// go test puts its own toolchain's go first on PATH.
		out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}",
			"github.com/those-who-run/salsa").Output()
		if err != nil {
			fixturesErr = err
			return
		}
		fixturesDir = filepath.Join(strings.TrimSpace(string(out)), "testdata", "v1")
	})
	if fixturesErr != nil {
		tb.Fatalf("finding salsa's testdata/v1: %v", fixturesErr)
	}
	return fixturesDir
}

// lines is every line of a gzip recording, without its line ending.
func lines(tb testing.TB, path string) []string {
	tb.Helper()
	f, err := os.Open(path) //nolint:gosec // the test's own fixture
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		tb.Fatal(err)
	}
	var all []string
	sc := bufio.NewScanner(gz)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}
	return all
}

// The recordings v1 1.0.0 wrote live with every field of format version 2 and no
// track id, which v1 only set later: AppendV1 must give back each of their lines
// byte for byte from the packet salsa reads from it.
var exact = []string{
	"real-nuc-sugo-restarts-2026-10-18",
	"real-nuc-sugo-two-stops-2026-10-18",
}

// A packet written by AppendV1 is v1's line for it, exactly: the format v1's
// replay and salsa's OpenRecording read.
func TestAppendV1WritesV1sLinesByteForByte(t *testing.T) {
	for _, name := range exact {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(fixtures(t), name+".jsonl.gz")
			want := lines(t, path)
			src, err := salsa.OpenRecording(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = src.Close() }()
			for i, line := range want {
				r, err := src.Next(t.Context())
				if err != nil {
					t.Fatalf("line %d: %v", i+1, err)
				}
				got, ok := AppendV1(nil, r.T, &r.Packet)
				if !ok {
					t.Fatalf("line %d: not written", i+1)
				}
				if string(got) != line+"\n" {
					t.Fatalf("line %d:\n got %s\nwant %s", i+1, got, line)
				}
			}
		})
	}
}

// Numbers are written as Python's repr writes a float.
func TestAppendV1WritesNumbersAsPython(t *testing.T) {
	for _, c := range []struct {
		v    float64
		want string
	}{
		{0, "0.0"}, {math.Copysign(0, -1), "-0.0"}, {100, "100.0"}, {-1, "-1.0"},
		{0.1, "0.1"}, {0.0001, "0.0001"}, {0.00001, "1e-05"}, {1.5e-7, "1.5e-07"},
		{1e15, "1000000000000000.0"}, {1e16, "1e+16"}, {1.5e16, "1.5e+16"}, {-2.5e20, "-2.5e+20"},
		{float64(float32(0.3)), "0.30000001192092896"}, {6927968.485709712, "6927968.485709712"},
	} {
		w := v1Line{}
		w.float("x", c.v)
		if got := strings.TrimPrefix(string(w.b), `"x":`); got != c.want {
			t.Errorf("%v: got %s, want %s", c.v, got, c.want)
		}
	}
}

// v1's phases, from the lap counters and the paused and loading bits.
func TestAppendV1DerivesV1sPhase(t *testing.T) {
	for _, c := range []struct {
		lap, total int16
		flags      salsa.Flags
		want       string
	}{
		{-1, -1, 0, "menu"}, {-1, 3, salsa.FlagLoading, "loading"}, {2, 3, salsa.FlagPaused, "paused"},
		{-1, 3, salsa.FlagPaused, "menu"}, {4, 3, 0, "finished"}, {4, 0, 0, "in_race"},
		{1, 3, 0, "in_race"}, {0, 3, 0, "at_track"},
	} {
		p := salsa.Packet{}
		p.CurrentLap, p.TotalLaps, p.Flags = c.lap, c.total, c.flags
		line, _ := AppendV1(nil, 0, &p)
		if !bytes.Contains(line, []byte(`"phase":"`+c.want+`"`)) {
			t.Errorf("lap %d of %d, flags %v: %s, want phase %s", c.lap, c.total, c.flags, line, c.want)
		}
	}
}

// A value that is not finite is not JSON: the line is not written.
func TestAppendV1RefusesAValueThatIsNotFinite(t *testing.T) {
	for _, v := range []float32{float32(math.NaN()), float32(math.Inf(1))} {
		p := salsa.Packet{}
		p.FuelLevel = v
		got, ok := AppendV1([]byte("kept"), 1, &p)
		if ok || string(got) != "kept" {
			t.Errorf("%v: %q, %v; want the buffer as it was and false", v, got, ok)
		}
	}
}
