package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"

	"github.com/those-who-run/ecu/internal/record"
	"github.com/those-who-run/ecu/race"
	"github.com/those-who-run/salsa"
)

// `ecu recordings list` and `ecu recordings archive ID` (paddock#354): the outings
// ecu has recorded in TWR_ECU_RECORDINGS_DIR, or --dir, as v1's `recordings` and
// `recordings archive` keep its own. Offline: no NATS, no PS5.

// The exit codes: 1 for a recording that cannot be archived, 2 for a bad flag or
// a directory that cannot be read.
const (
	exitRecordingsFailed = 1
	exitRecordingsUsage  = 2
)

// issues is --issue, given once per issue.
type issues []int

func (i *issues) String() string { return fmt.Sprint(*i) }

func (i *issues) Set(v string) error {
	n, err := strconv.Atoi(strings.TrimPrefix(v, "#"))
	if err != nil || n <= 0 {
		return fmt.Errorf("%q is not an issue number", v)
	}
	*i = append(*i, n)
	return nil
}

func recordings(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	usage := "usage: ecu recordings list [--dir DIR]\n" +
		"       ecu recordings archive [--dir DIR] [--note NOTE] [--issue N ...] [--force] ID|PATH"
	if len(args) == 0 || (args[0] != "list" && args[0] != "archive") {
		_, _ = fmt.Fprintln(stderr, usage)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			return 0
		}
		return exitRecordingsUsage
	}
	cmd := args[0]
	flags := flag.NewFlagSet("ecu recordings "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", getenv("TWR_ECU_RECORDINGS_DIR"), "the recordings' directory (TWR_ECU_RECORDINGS_DIR)")
	var (
		note  *string
		force *bool
		cites issues
	)
	if cmd == "archive" {
		note = flags.String("note", "", "what the outing shows (optional; archived again, the note is kept unless given)")
		flags.Var(&cites, "issue", "an issue that relies on the outing (optional; again for each)")
		force = flags.Bool("force", false, "archive an outing written to within TWR_ECU_RECORDINGS_IDLE_SECS, which may still be recording")
	}
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, usage+"\n\n"+
			"Lists the outings ecu recorded, or archives one, by its id or its recording's path,\n"+
			"so that it is never pruned.\n\nflags:")
		flags.PrintDefaults()
	}
	refs, err := parseArgs(flags, args[1:])
	if err == nil && cmd == "list" && len(refs) > 0 {
		err = fmt.Errorf("list takes no arguments, got %q", refs)
	}
	if err == nil && cmd == "archive" && len(refs) != 1 {
		err = fmt.Errorf("archive takes one outing, its id or its recording's path; got %d", len(refs))
	}
	var id string
	if err == nil && cmd == "archive" {
		*dir, id = outingRef(refs[0], *dir)
	}
	if err == nil && *dir == "" {
		err = errors.New("no directory: set TWR_ECU_RECORDINGS_DIR or give --dir")
	}
	var idle time.Duration
	if err == nil && cmd == "archive" {
		idle, err = recordingsIdle(getenv)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if !errors.Is(err, errFlag) {
			_, _ = fmt.Fprintf(stderr, "ecu recordings %s: %v\n", cmd, err)
		}
		return exitRecordingsUsage
	}
	if cmd == "archive" {
		return archiveRecording(ctx, *dir, id, *note, cites, *force, idle, stdout, stderr)
	}
	return listRecordings(ctx, *dir, stdout, stderr)
}

// outingRef is the directory and the outing id ref names: a recording's path, as
// v1's `recordings archive` takes one, or an id in dir.
func outingRef(ref, dir string) (string, string) {
	if fi, err := os.Stat(ref); err == nil && fi.Mode().IsRegular() { //nolint:gosec // the recording the operator names
		return filepath.Dir(ref), strings.TrimSuffix(filepath.Base(ref), record.Ext)
	}
	return dir, strings.TrimSuffix(ref, record.Ext)
}

// listRecordings prints each outing, oldest first: its id, start, length, frames
// and size, whether it is archived and whether it ended whole, then one line per
// session in it and the archive note. A note whose recording is gone is warned of.
func listRecordings(ctx context.Context, dir string, stdout, stderr io.Writer) int {
	all, err := record.Recordings(dir)
	if err == nil {
		var orphans []string
		orphans, err = record.OrphanNotes(dir)
		for _, name := range orphans {
			_, _ = fmt.Fprintf(stderr, "ecu recordings list: warning: %s is an archive note whose recording is gone\n", name)
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ecu recordings list:", err)
		return exitRecordingsUsage
	}
	for _, r := range all {
		s, err := summariseOuting(ctx, r.Path, r.Start)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "ecu recordings list: %v\n", err)
			_, _ = fmt.Fprintf(stdout, "%s  %s  (unreadable)\n", r.ID, r.Start.Format(time.RFC3339))
			continue
		}
		parts := []string{r.ID, r.Start.Format(time.RFC3339), lengthOf(s.length),
			fmt.Sprintf("%d frames", s.frames), fmt.Sprintf("%.1f MB", float64(r.Size)/1e6)}
		if r.Note != nil {
			parts = append(parts, "archived"+citing(r.Note.Issues))
		}
		if s.problem != "" {
			parts = append(parts, "("+s.problem+")")
		}
		_, _ = fmt.Fprintln(stdout, strings.Join(parts, "  "))
		for _, line := range s.lines() {
			_, _ = fmt.Fprintln(stdout, "   "+line)
		}
		if len(s.sessions) == 0 {
			_, _ = fmt.Fprintln(stdout, "   no session")
		}
		if r.Note != nil && r.Note.Note != "" {
			_, _ = fmt.Fprintln(stdout, "   note: "+r.Note.Note)
		}
	}
	if len(all) == 0 {
		_, _ = fmt.Fprintf(stdout, "no recordings in %s\n", dir)
	}
	return 0
}

// archiveRecording archives the outing id with its note and issues, and prints
// what it archived. An outing written to within idle may still be recording, and
// its note would not match the recording once it ends: it is refused, or with
// force archived with a warning to archive it again.
func archiveRecording(ctx context.Context, dir, id, note string, cites []int, force bool, idle time.Duration,
	stdout, stderr io.Writer,
) int {
	start, _, ok := record.ParseID(id)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "ecu recordings archive: %q is not an outing id (YYYYMMDD-HHMMSS)\n", id)
		return exitRecordingsUsage
	}
	path := filepath.Join(dir, id+record.Ext)
	fi, err := os.Stat(path) //nolint:gosec // an outing id in the operator's directory
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ecu recordings archive:", err)
		return exitRecordingsFailed
	}
	if since := time.Since(fi.ModTime()); since < idle {
		if !force {
			_, _ = fmt.Fprintf(stderr, "ecu recordings archive: %s was written to %v ago, within %s (%v): it may still be "+
				"recording; archive it once the outing has ended, or give --force\n", id, since.Round(time.Second), idleSecsEnv, idle)
			return exitRecordingsFailed
		}
		_, _ = fmt.Fprintf(stderr, "ecu recordings archive: warning: %s may still be recording: archive it again once the outing has ended\n", id)
	}
	s, err := summariseOuting(ctx, path, start)
	var n record.Note
	if err == nil {
		n, err = record.Archive(dir, id, s.note(note, cites), time.Now())
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ecu recordings archive:", err)
		return exitRecordingsFailed
	}
	_, _ = fmt.Fprintf(stdout, "archived %s  %d frames  %.1f MB%s\n", id, n.Frames, float64(n.Bytes)/1e6, citing(n.Issues))
	for _, line := range n.Sessions {
		_, _ = fmt.Fprintln(stdout, "   "+line)
	}
	if n.Note != "" {
		_, _ = fmt.Fprintln(stdout, "   note: "+n.Note)
	}
	if s.problem != "" {
		_, _ = fmt.Fprintf(stderr, "ecu recordings archive: warning: the recording is %s\n", s.problem)
	}
	return 0
}

func citing(issues []int) string {
	var b strings.Builder
	for _, n := range issues {
		fmt.Fprintf(&b, " #%d", n)
	}
	return b.String()
}

// lengthOf is a recording's length as h:mm:ss, or m:ss under an hour.
func lengthOf(d time.Duration) string {
	secs := int(d.Round(time.Second).Seconds())
	if secs >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", secs/3600, secs/60%60, secs%60)
	}
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

// outingSummary is what a recording holds: its frames, the time from its first
// to its last, what is wrong with its stream if anything, and the sessions the
// race state saw in it.
type outingSummary struct {
	frames   int
	length   time.Duration
	problem  string // "" for a stream that ends whole
	sessions []*sessionSum
}

// cutOff is a recording that stops short: empty, or a gzip stream that ends
// before its end, as the outing being recorded does, or one a crash cut short.
const cutOff = "incomplete: still being written or cut off"

// lines is one line per session, as `ecu replay` sums them up: kind, laps, and
// why it ended (INPUT_ENDED for one still open where the recording stops).
func (s outingSummary) lines() []string {
	out := make([]string, 0, len(s.sessions))
	for _, sum := range s.sessions {
		out = append(out, fmt.Sprintf("%s  %d laps  %s",
			strings.TrimPrefix(sum.kind.String(), "SESSION_TYPE_"), sum.laps, sum.ended))
	}
	return out
}

// note is the archive note for the recording: its frames, sessions, and the
// tracks, cars and kinds in them, as v1's sidecar has them ("lap_race
// (rolling)"), with the note and issues given.
func (s outingSummary) note(note string, cites []int) record.Note {
	n := record.Note{Frames: s.frames, Sessions: s.lines(), Note: note, Issues: cites}
	for _, sum := range s.sessions {
		n.Tracks = addNew(n.Tracks, sum.track)
		n.Cars = addNew(n.Cars, sum.car)
		if sum.kind != eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED {
			start := "unknown"
			if sum.start != eventsv1.StartType_START_TYPE_UNSPECIFIED {
				start = strings.ToLower(strings.TrimPrefix(sum.start.String(), "START_TYPE_"))
			}
			n.Kinds = addNew(n.Kinds, strings.ToLower(strings.TrimPrefix(sum.kind.String(), "SESSION_TYPE_"))+" ("+start+")")
		}
	}
	return n
}

func addNew(list []string, v string) []string {
	if v == "" || slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// summariseOuting reads the recording at path through salsa's recording Source
// and the race state, as a replay would, dated from start. A stream that stops
// short, an empty file included, is read up to where it stops and called cut off;
// one damaged otherwise, up to the damage and called damaged. A file that cannot
// be opened is an error.
func summariseOuting(ctx context.Context, path string, start time.Time) (outingSummary, error) {
	s := outingSummary{}
	fi, err := os.Stat(path) //nolint:gosec // a recording in the operator's directory
	if err != nil {
		return s, err
	}
	if fi.Size() == 0 {
		s.problem = cutOff
		return s, nil
	}
	src, err := salsa.OpenRecording(path)
	var re *salsa.RecordingError
	if errors.As(err, &re) {
		s.problem = problemOf(re)
		return s, nil
	}
	if err != nil {
		return s, err
	}
	defer func() { _ = src.Close() }()
	m := race.New(ulid.Make)
	sums := &sessionSums{}
	var t0 float64
	for {
		p, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.As(err, &re) {
			s.problem = problemOf(re)
			break
		}
		if err != nil {
			return outingSummary{}, err
		}
		if s.frames == 0 {
			t0 = p.T
		}
		s.frames++
		s.length = seconds(p.T - t0)
		sums.see(m.Step(race.Received{Received: p, Time: start.Add(seconds(p.T - t0))}))
	}
	sums.see(m.EndOfInput())
	s.sessions = sums.all
	return s, nil
}

// problemOf says what is wrong with a stream: cut off when it stops short, which
// gzip reports as an unexpected EOF, damaged otherwise.
func problemOf(re *salsa.RecordingError) string {
	if errors.Is(re, io.ErrUnexpectedEOF) {
		return cutOff
	}
	return fmt.Sprintf("damaged at line %d: %v", re.Line, re.Err)
}
