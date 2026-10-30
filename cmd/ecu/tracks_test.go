package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// trackRecording is where one of internal/track's goldens' recordings is: its own
// v1 copies, its live cuts, else salsa's.
func trackRecording(tb testing.TB, name string) string {
	tb.Helper()
	for _, dir := range []string{"v1", "live"} {
		p := filepath.Join("..", "..", "internal", "track", "testdata", dir, name+".jsonl.gz")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return recordingPath(tb, name)
}

// runTracks runs `ecu tracks` with the args, and gives what it printed on each
// stream and its exit code.
func runTracks(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = tracks(t.Context(), args, strings.NewReader(""), &out, &errOut)
	return out.String(), errOut.String(), code
}

// v1Lines are the lines of an ecu tracks output v1 also prints: those ecu adds
// start with two spaces.
func v1Lines(out string, code int) string {
	var keep []string
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(line, "  ") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n") + "exit " + strconv.Itoa(code) + "\n"
}

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "tracks", name)) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// AC1 and R1, R2: on every recording internal/track's goldens cover (v1's
// FIXTURES.yaml and the Fuji race of 2026-10-29), `ecu tracks detect` prints v1's
// verdict lines and result line, and exits as v1 does, as testdata/tracks/
// make_golden.py wrote them from v1.0.0 with ecu's table; AC5: with no NATS.
func TestTracksDetectIsV1sOnEveryRecording(t *testing.T) {
	t.Setenv("TWR_NATS_URL", "")
	paths, err := filepath.Glob(filepath.Join("testdata", "tracks", "*.detect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 26 {
		t.Errorf("%d detect goldens, want v1's 25 and the Fuji race", len(paths))
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".detect.txt")
		t.Run(name, func(t *testing.T) {
			out, _, code := runTracks(t, "detect", trackRecording(t, name))
			if got, want := v1Lines(out, code), golden(t, name+".detect.txt"); got != want {
				t.Errorf("got\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// AC2 and R2: detect exits 0 when the result is confirmed or probable and 1
// otherwise, as v1: 0 on Suzuka East, confirmed. The unknown track is SUGO, which
// the table has had since that recording was made (its learned row, 100001), so
// v1.0.0 exits 0 on it too, confirmed; a 1 is the two-lap race of 2026-10-14,
// ambiguous, and the Fuji race read only to frame 400, before its first crossing.
func TestTracksDetectExitsOnItsVerdict(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{trackRecording(t, "real-nuc-suzuka-east-2026-10-15")}, 0},
		{[]string{trackRecording(t, "real-nuc-unknown-track-2026-10-15")}, 0},
		{[]string{trackRecording(t, "real-race-2laps-2026-10-14")}, 1},
		{[]string{"--to", "400", trackRecording(t, "real-nuc-short-shift-fuji-race-2026-10-29")}, 1},
	} {
		if _, _, code := runTracks(t, append([]string{"detect"}, c.args...)...); code != c.code {
			t.Errorf("%v: exit %d, want %d", c.args, code, c.code)
		}
	}
}

// AC3 and R3: learn on SUGO's timed session and the Gilles Villeneuve race prints
// v1's row, line, direction, box, length and nearest lines; the Fuji race's
// nearest line is Fuji's, 0 m away.
func TestTracksLearnIsV1s(t *testing.T) {
	for _, name := range []string{
		"real-nuc-sugo-timed-2026-10-15", "real-nuc-gilles-villeneuve-race-2026-10-17",
		"real-nuc-short-shift-fuji-race-2026-10-29",
	} {
		t.Run(name, func(t *testing.T) {
			out, _, code := runTracks(t, "learn", trackRecording(t, name))
			if got, want := out+"exit "+strconv.Itoa(code)+"\n", golden(t, name+".learn.txt"); got != want {
				t.Errorf("got\n%s\nwant\n%s", got, want)
			}
		})
	}
	out, _, _ := runTracks(t, "learn", "--id", "100003", "--name", "Fuji", trackRecording(t, "real-nuc-short-shift-fuji-race-2026-10-29"))
	if !strings.Contains(out, "(0, 16, 'Fuji International Speedway')") || !strings.Contains(out, "\n100003,Fuji,PX,") {
		t.Errorf("the Fuji race:\n%s", out)
	}
}

// AC4 and R4: learn on a recording cut before its first line crossing says there
// is no completed lap and exits non-zero.
func TestTracksLearnWithNoLap(t *testing.T) {
	path := trackRecording(t, "real-nuc-short-shift-fuji-race-2026-10-29")
	out, errOut, code := runTracks(t, "learn", "--to", "400", path)
	if code == 0 || out != "" || !strings.Contains(errOut, "no completed lap") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// writeGz writes the lines, gzipped, to a temporary file.
func writeGz(t *testing.T, data []byte) string {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cut.jsonl.gz")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixtureLines are a recording's lines.
func fixtureLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // a fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return strings.SplitAfter(strings.TrimSuffix(string(b), "\n"), "\n")
}

// --from and --to read the frames between, both included: the same as a recording
// of only those frames.
func TestTracksFromTo(t *testing.T) {
	path := trackRecording(t, "real-nuc-short-shift-fuji-race-2026-10-29")
	lines := fixtureLines(t, path)
	cut := writeGz(t, []byte(strings.Join(lines[29:900], ""))) // frames 30 to 900
	for _, cmd := range []string{"detect", "learn"} {
		want, _, wantCode := runTracks(t, cmd, cut)
		got, _, code := runTracks(t, cmd, "--from", "30", "--to", "900", path)
		if got != want || code != wantCode {
			t.Errorf("%s --from 30 --to 900: exit %d\n%s\nwant exit %d\n%s", cmd, code, got, wantCode, want)
		}
	}
	for _, bad := range [][]string{{"detect", "--from", "9", "--to", "8", path}, {"detect"}, {"list"}, {}} {
		if _, _, code := runTracks(t, bad...); code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, code)
		}
	}
}

// A recording whose gzip stream ends damaged, as the live recording of 2026-10-29
// does, is read up to the damage, with a warning saying what was read.
func TestTracksReadsADamagedRecordingToTheDamage(t *testing.T) {
	path := trackRecording(t, "real-nuc-short-shift-fuji-race-2026-10-29")
	whole, err := os.ReadFile(path) //nolint:gosec // a fixture
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "damaged.jsonl.gz")
	if err := os.WriteFile(cut, whole[:len(whole)/2], 0o600); err != nil { //nolint:gosec // a temporary directory
		t.Fatal(err)
	}
	out, errOut, code := runTracks(t, "detect", cut)
	if !strings.Contains(errOut, "warning:") || !strings.Contains(errOut, "damaged after line") ||
		!strings.Contains(errOut, "read frames 1 to") || !strings.Contains(out, "\nresult: ") || code == 2 {
		t.Errorf("exit %d, stdout\n%s\nstderr %s", code, out, errOut)
	}
}

// detect says why a top candidate was ruled out: at Suzuka East the four of
// lap 1's line left their boxes before the lap closed.
func TestTracksDetectSaysWhyALayoutWasRuledOut(t *testing.T) {
	out, _, _ := runTracks(t, "detect", trackRecording(t, "real-nuc-suzuka-east-2026-10-15"))
	if !strings.Contains(out, "\n  ruled out: Autodrome Lago Maggiore - East End Reverse (790): path left its box, x 926 > 862+50\n") {
		t.Errorf("got\n%s", out)
	}
}

func TestPyRepr(t *testing.T) {
	for in, want := range map[string]string{`Fuji`: `'Fuji'`, `Le Mans' Bugatti`: `"Le Mans' Bugatti"`, `a'"b`: `'a\'"b'`, `a\b`: `'a\\b'`} {
		if got := pyRepr(in); got != want {
			t.Errorf("pyRepr(%q) = %s, want %s", in, got, want)
		}
	}
}
