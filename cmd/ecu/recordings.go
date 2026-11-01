package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

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
		"       ecu recordings archive [--dir DIR] --note NOTE --issue N [--issue N ...] ID"
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
	note := flags.String("note", "", "archive: what the outing shows")
	var cites issues
	flags.Var(&cites, "issue", "archive: an issue that relies on the outing (again for each)")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, usage+"\n\n"+
			"Lists the outings ecu recorded, or archives one so that it is never pruned.\n\nflags:")
		flags.PrintDefaults()
	}
	var id string
	var err error
	if cmd == "archive" {
		id, err = parseInterspersed(flags, args[1:])
	} else {
		if err = flags.Parse(args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
			err = errFlag
		}
		if err == nil && flags.NArg() > 0 {
			err = fmt.Errorf("list takes no arguments, got %q", flags.Args())
		}
	}
	if err == nil && *dir == "" {
		err = errors.New("no directory: set TWR_ECU_RECORDINGS_DIR or give --dir")
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
		return archiveRecording(ctx, *dir, strings.TrimSuffix(id, record.Ext), *note, cites, stdout, stderr)
	}
	return listRecordings(ctx, *dir, stdout, stderr)
}

// listRecordings prints each outing, oldest first: its id, start, length, frames
// and size, whether it is archived and whether it ended whole, then one line per
// session in it and the archive note.
func listRecordings(ctx context.Context, dir string, stdout, stderr io.Writer) int {
	all, err := record.Recordings(dir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ecu recordings list:", err)
		return exitRecordingsUsage
	}
	for _, r := range all {
		s, err := summariseOuting(ctx, r)
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
		if !s.complete {
			parts = append(parts, "(incomplete: still being written or cut off)")
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
// what it archived.
func archiveRecording(ctx context.Context, dir, id, note string, cites []int, stdout, stderr io.Writer) int {
	start, _, ok := record.ParseID(id)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "ecu recordings archive: %q is not an outing id (YYYYMMDD-HHMMSS)\n", id)
		return exitRecordingsUsage
	}
	s, err := summariseOuting(ctx, record.Recording{ID: id, Path: filepath.Join(dir, id+record.Ext), Start: start})
	if err == nil {
		var n record.Note
		n, err = record.Archive(dir, id, record.Note{Frames: s.frames, Sessions: s.lines(), Note: note, Issues: cites}, time.Now())
		if err == nil {
			_, _ = fmt.Fprintf(stdout, "archived %s  %d frames  %.1f MB%s\n", id, n.Frames, float64(n.Bytes)/1e6, citing(n.Issues))
			for _, line := range n.Sessions {
				_, _ = fmt.Fprintln(stdout, "   "+line)
			}
			if n.Note != "" {
				_, _ = fmt.Fprintln(stdout, "   note: "+n.Note)
			}
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ecu recordings archive:", err)
		return exitRecordingsFailed
	}
	if !s.complete {
		_, _ = fmt.Fprintln(stderr, "ecu recordings archive: warning: the recording is incomplete: still being written or cut off")
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
// to its last, whether its gzip stream ended whole, and the sessions the race
// state saw in it.
type outingSummary struct {
	frames   int
	length   time.Duration
	complete bool
	sessions []*sessionSum
}

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

// summariseOuting reads the recording through salsa's recording Source and the
// race state, as a replay would, dated from its start. A stream that ends damaged
// after a frame, as one still being written or cut off by a crash does, is read up
// to the damage and called incomplete; one that cannot be read at all is an
// error.
func summariseOuting(ctx context.Context, r record.Recording) (outingSummary, error) {
	src, err := salsa.OpenRecording(r.Path)
	if err != nil {
		return outingSummary{}, err
	}
	defer func() { _ = src.Close() }()
	m := race.New(ulid.Make)
	sums := &sessionSums{}
	s := outingSummary{complete: true}
	var t0 float64
	for {
		p, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		var re *salsa.RecordingError
		if errors.As(err, &re) && s.frames > 0 {
			s.complete = false
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
		sums.see(m.Step(race.Received{Received: p, Time: r.Start.Add(seconds(p.T - t0))}))
	}
	sums.see(m.EndOfInput())
	s.sessions = sums.all
	return s, nil
}
