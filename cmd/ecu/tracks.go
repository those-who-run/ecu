package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"

	"github.com/those-who-run/ecu/race"
	"github.com/those-who-run/ecu/track"
	"github.com/those-who-run/salsa"
)

// `ecu tracks detect` and `ecu tracks learn` (paddock#300): v1's `tracks detect`
// and `tracks learn` (cli/tracks.py and tracktools.py at v1.0.0) over a v1
// recording, offline: no NATS, no PS5, no file but the one named. Their output is
// v1's, line for line, so it diffs against v1's; what they add is on lines of its
// own, each starting with two spaces:
//
//   - --from and --to, the frames to read (counted from 1, both included), as a
//     live recording holds a whole outing in one file;
//   - a recording whose gzip stream ends damaged, as one cut short by a crash or a
//     kill does, is read up to the damage, with a warning on standard error
//     saying what was read, as v1 read it when not strict;
//   - detect says why a layout that was a top candidate was ruled out, and, when
//     it ends with no layout, why each of those whose finish line is nearest where
//     the lap counter first went up was.

// The exit codes: detect's 0 and 1 are its verdict's, as v1's; learn's 1 is a
// recording with no completed lap. A bad flag or a recording that cannot be read
// at all is 2.
const (
	exitTracksMiss  = 1
	exitTracksUsage = 2
)

func tracks(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	usage := "usage: ecu tracks detect [--from N] [--to N] PATH\n" +
		"       ecu tracks learn [--from N] [--to N] [--id N] [--name NAME] PATH"
	if len(args) == 0 || (args[0] != "detect" && args[0] != "learn") {
		_, _ = fmt.Fprintln(stderr, usage)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			return 0
		}
		return exitTracksUsage
	}
	cmd := args[0]
	flags := flag.NewFlagSet("ecu tracks "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	from := flags.Uint("from", 0, "the first frame to read, counted from 1 (default the first)")
	to := flags.Uint("to", 0, "the last frame to read, included (default the last)")
	id := flags.Int("id", 0, "learn: the track id for the printed CSV row")
	name := flags.String("name", "NAME", "learn: the track name for the printed CSV row")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, usage+"\n\n"+
			"Runs v1's track detector over a v1 recording (.jsonl, .jsonl.gz, or - for standard input),\n"+
			"or derives a tracks.csv row from its laps.\n\nflags:")
		flags.PrintDefaults()
	}
	path, err := parseInterspersed(flags, args[1:])
	if err == nil && *to != 0 && *from > *to {
		err = fmt.Errorf("--from %d is after --to %d", *from, *to)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if !errors.Is(err, errFlag) {
			_, _ = fmt.Fprintf(stderr, "ecu tracks %s: %v\n", cmd, err)
		}
		return exitTracksUsage
	}
	frames, err := readTrackFrames(ctx, path, stdin, uint32(*from), uint32(*to), stderr) //nolint:gosec // a frame count
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ecu tracks %s: %v\n", cmd, err)
		return exitTracksUsage
	}
	if cmd == "detect" {
		return detectTrack(frames, stdout)
	}
	return learnTrack(path, frames, int32(*id), *name, stdout, stderr) //nolint:gosec // an id the user gives
}

// numbered is a frame and its number in the recording, from 1.
type numbered struct {
	n uint32
	f *telemetryv1.Frame
}

// readTrackFrames reads the recording's frames from..to (0 for no bound) as ecu
// makes them, the contracts frame race gives for each packet, as the
// detector's goldens are read. A stream that ends damaged after a frame ends the
// frames there, with a warning; one that cannot be read at all is an error.
func readTrackFrames(ctx context.Context, path string, stdin io.Reader, from, to uint32, stderr io.Writer) ([]numbered, error) {
	src, err := openReplay(path, stdin)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	m := race.New(ulid.Make)
	at := time.Unix(0, 0).UTC()
	var out []numbered
	for n := uint32(1); to == 0 || n <= to; n++ {
		r, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		var re *salsa.RecordingError
		if errors.As(err, &re) && n > 1 {
			_, _ = fmt.Fprintf(stderr, "ecu tracks: warning: %v; read frames 1 to %d, using %d of them\n", err, n-1, len(out))
			break
		}
		if err != nil {
			return nil, err
		}
		if n < from {
			continue
		}
		// The machine makes the frame (its phase needs the frames before it in the
		// range); nothing else of it is used.
		out = append(out, numbered{n, m.Step(race.Received{Received: r, Time: at})[0].Frame})
	}
	return out, nil
}

// detectTrack is v1's `tracks detect`: a fresh detector fed every frame, one line
// per change of its decision, then the result. It exits 0 when the result is
// confirmed or probable, and 1 otherwise.
func detectTrack(frames []numbered, stdout io.Writer) int {
	d := track.New(nil, track.Config{})
	var (
		top     []track.Candidate // the last line's first 4 candidates
		tick    *track.Point      // where the lap counter first went up from lap 1 on
		lastLap int32
		hasLap  bool
	)
	for _, nf := range frames {
		f := nf.f
		if racingLap(f) {
			if tick == nil && hasLap && f.GetLap() > lastLap && lastLap >= 1 {
				tick = &track.Point{X: f.GetPosX(), Z: f.GetPosZ()}
			}
			lastLap, hasLap = f.GetLap(), true
		}
		m, changed := d.Observe(f)
		if !changed {
			continue
		}
		cands := m.Candidates[:min(4, len(m.Candidates))]
		shown := make([]string, len(cands))
		for i, c := range cands {
			line := ""
			if c.LineCrossed {
				line = ", line"
			}
			shown[i] = fmt.Sprintf("%s (%.2f%s)", c.Track.Name, c.Fit, line)
		}
		_, _ = fmt.Fprintf(stdout, "t=%9.1f lap %2d  %-9s %s: %s  [%s]\n",
			f.GetT(), f.GetLap(), m.Confidence, pyName(m.Name), m.Evidence, strings.Join(shown, ", "))
		for _, c := range top {
			if why, out := d.RuledOut(c.Track.ID); out {
				_, _ = fmt.Fprintf(stdout, "  ruled out: %s (%d): %s\n", c.Track.Name, c.Track.ID, why)
			}
		}
		top = cands
	}
	final := d.Match()
	found := final.Confidence == track.Confirmed || final.Confidence == track.Probable
	if !found && tick != nil {
		_, _ = fmt.Fprintf(stdout, "  nearest lines to where the lap counter first went up (%.0f, %.0f):\n", tick.X, tick.Z)
		for _, n := range track.NearestLines(*tick, track.Table(), 4) {
			why, out := d.RuledOut(n.ID)
			if !out {
				why = "still fits"
			}
			_, _ = fmt.Fprintf(stdout, "  %s (%d), %.0f m: %s\n", n.Name, n.ID, n.DistanceM, why)
		}
	}
	id := "None"
	if final.TrackID != 0 {
		id = strconv.Itoa(int(final.TrackID))
	}
	_, _ = fmt.Fprintf(stdout, "\nresult: %s %s (%s)\n", final.Confidence, pyName(final.Name), id)
	if found {
		return 0
	}
	return exitTracksMiss
}

// racingLap says GT7's lap counter means something on the frame: in race or
// finished, as v1's LAP_PHASES.
func racingLap(f *telemetryv1.Frame) bool {
	p := f.GetPhase()
	return p == telemetryv1.Phase_PHASE_IN_RACE || p == telemetryv1.Phase_PHASE_FINISHED
}

// pyName is a name as v1 printed it: None for none.
func pyName(name string) string {
	if name == "" {
		return "None"
	}
	return name
}

// learnTrack is v1's `tracks learn`: the row learned from the recording's laps,
// printed as v1 printed it.
func learnTrack(path string, frames []numbered, id int32, name string, stdout, stderr io.Writer) int {
	var points []track.LapPoint
	for _, nf := range frames {
		f := nf.f
		switch {
		case racingLap(f):
			points = append(points, track.LapPoint{Lap: f.GetLap(), P: track.Point{X: f.GetPosX(), Z: f.GetPosZ()}})
		case f.GetPhase() == telemetryv1.Phase_PHASE_AT_TRACK:
			points = append(points, track.LapPoint{Lap: track.NoLap, P: track.Point{X: f.GetPosX(), Z: f.GetPosZ()}})
		}
	}
	l, err := track.Learn(points, nil, track.LearnHalfWidth)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ecu tracks learn: %s: %v\n", path, err)
		return exitTracksMiss
	}
	near := make([]string, len(l.Nearest))
	for i, n := range l.Nearest {
		near[i] = fmt.Sprintf("(%.0f, %d, %s)", n.DistanceM, n.ID, pyRepr(n.Name))
	}
	_, _ = fmt.Fprintf(stdout, "lap length %.0f m over %d completed lap(s)\n", l.LapLengthM, l.LapsUsed)
	_, _ = fmt.Fprintf(stdout, "finish line at %.0f,%.0f .. %.0f,%.0f  direction %s\n",
		l.Line[0].X, l.Line[0].Z, l.Line[1].X, l.Line[1].Z, l.Direction)
	_, _ = fmt.Fprintf(stdout, "box %.0f %.0f %.0f %.0f\n", l.Box.MinX, l.Box.MinZ, l.Box.MaxX, l.Box.MaxZ)
	_, _ = fmt.Fprintf(stdout, "nearest existing lines: [%s]\n", strings.Join(near, ", "))
	_, _ = fmt.Fprintf(stdout, "\ncsv row:\n%s\n", l.CSVRow(id, name))
	return 0
}

// pyRepr is a string as Python's repr writes it: in single quotes, or double
// quotes when it has a single quote and no double one, with backslashes and the
// quote escaped.
func pyRepr(s string) string {
	q := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = `"`
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	return q + strings.ReplaceAll(s, q, `\`+q) + q
}
