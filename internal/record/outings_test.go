package record

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/salsa"
)

// drivePackets are n packets from t on, 1/60 s apart.
func drivePackets(t float64, n int) []salsa.Received {
	all := make([]salsa.Received, n)
	for i := range all {
		all[i].T = t + float64(i)/60
		all[i].PacketID = int32(i + 1) //nolint:gosec // a small count
	}
	return all
}

// whole reads a recording as plain gzip, as a copy of it would be read, and
// gives its lines, failing on a stream that does not end whole.
func whole(tb testing.TB, path string) int {
	tb.Helper()
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		tb.Fatal(err)
	}
	b, err := io.ReadAll(gz)
	if err != nil {
		tb.Fatalf("%s is not a whole gzip: %v", path, err)
	}
	return strings.Count(string(b), "\n")
}

// AC1 (paddock#354, R1): packets, then 10 s of silence on the packets' own clock,
// then more packets give two recordings, the second named -2 as it starts in the
// same second here, each holding its outing's packets.
func TestAnIdleFeedStartsANewRecording(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{Idle: 3 * time.Second}, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	first, second := drivePackets(100, 700), drivePackets(100+700.0/60+10, 300)
	for _, r := range append(slices.Clone(first), second...) {
		w.Write(r)
	}
	s := w.Close(t.Context())
	if s.Outings != 2 || s.Frames != 1000 || s.Dropped != 0 {
		t.Fatalf("stats %+v; want 2 outings, 1,000 frames", s)
	}
	all, err := Recordings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != "20261031-200509" || all[1].ID != "20261031-200509-2" {
		t.Fatalf("recordings %+v", all)
	}
	for i, want := range [][]salsa.Received{first, second} {
		got := readBack(t, all[i].Path)
		if len(got) != len(want) || got[0].T != want[0].T || got[len(got)-1].T != want[len(want)-1].T {
			t.Errorf("outing %d: read back %d packets, want %d", i+1, len(got), len(want))
		}
	}
	// A gap under the timeout is the same outing.
	dir = t.TempDir()
	w, err = Open(dir, Options{Idle: 3 * time.Second}, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range append(drivePackets(0, 10), drivePackets(2.5, 10)...) {
		w.Write(r)
	}
	if s := w.Close(t.Context()); s.Outings != 1 {
		t.Errorf("a 2.5 s gap made %d outings", s.Outings)
	}
}

// An outing's file is closed, a whole gzip, once the feed has been idle for the
// timeout, while ecu still runs: a copy of it then is the whole outing. Only the
// outing being written can be cut short.
func TestAnEndedOutingIsAWholeGzipWhileEcuRuns(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{Idle: 100 * time.Millisecond}, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range drivePackets(0, 1000) { // one member and part of another
		w.Write(r)
	}
	path := filepath.Join(dir, "20261031-200509.jsonl.gz")
	deadline := time.Now().Add(5 * time.Second)
	for !closedWhole(path) {
		if time.Now().After(deadline) {
			t.Fatal("the outing is not a whole gzip 5 s after its last packet")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := whole(t, path); n != 1000 {
		t.Errorf("%d lines, want 1,000", n)
	}
	// The next packet, long after on the wall clock, starts the next outing.
	for _, r := range drivePackets(1000, 5) {
		w.Write(r)
	}
	s := w.Close(t.Context())
	if s.Outings != 2 || s.Path != filepath.Join(dir, "20261031-200509-2.jsonl.gz") || whole(t, s.Path) != 5 {
		t.Errorf("stats %+v", s)
	}
}

// closedWhole says whether path reads as a whole gzip of 1,000 lines.
func closedWhole(path string) bool {
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return false
	}
	b, err := io.ReadAll(gz)
	return err == nil && strings.Count(string(b), "\n") == 1000
}

// touch makes each file in dir.
func touch(tb testing.TB, dir string, names ...string) {
	tb.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
}

func names(tb testing.TB, dir string) []string {
	tb.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatal(err)
	}
	var all []string
	for _, e := range entries {
		all = append(all, e.Name())
	}
	return all
}

// AC2 (paddock#354, R2, R5): a recording older than the keep setting is pruned,
// with what is recorded beside it under its id; an archived one is not, nor a
// newer one, nor a file not named for an outing.
func TestOldRecordingsArePrunedButNotArchivedOnes(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "20260901-100000.jsonl.gz", "20260901-100000.capture.gz")                         // old
	touch(t, dir, "20260901-100000-2.jsonl.gz")                                                     // old, the second of its second
	touch(t, dir, "20260902-100000.jsonl.gz", "20260902-100000.capture.gz", "20260902-100000.json") // old, archived
	touch(t, dir, "20261030-100000.jsonl.gz", "20261030-100000.capture.gz")                         // new
	touch(t, dir, "notes.txt", "2026.jsonl.gz", ".ecu-probe-1")                                     // not outings
	before := at.Add(-30 * 24 * time.Hour)
	pruned, err := Prune(dir, before, quiet())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(pruned)
	if !slices.Equal(pruned, []string{"20260901-100000", "20260901-100000-2"}) {
		t.Errorf("pruned %v", pruned)
	}
	want := []string{".ecu-probe-1", "2026.jsonl.gz",
		"20260902-100000.capture.gz", "20260902-100000.json", "20260902-100000.jsonl.gz",
		"20261030-100000.capture.gz", "20261030-100000.jsonl.gz", "notes.txt"}
	if got := names(t, dir); !slices.Equal(got, want) {
		t.Errorf("left %v\nwant %v", got, want)
	}
}

// The Writer prunes when it opens, with Options.Keep, and not with 0.
func TestTheWriterPrunes(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "20260901-100000.jsonl.gz", "20261031-100000.jsonl.gz")
	w, err := Open(dir, Options{}, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	w.Close(t.Context())
	if got := names(t, dir); len(got) != 2 {
		t.Errorf("Keep 0 pruned: %v", got)
	}
	w, err = Open(dir, Options{Keep: 30 * 24 * time.Hour}, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	w.Close(t.Context())
	if got := names(t, dir); !slices.Equal(got, []string{"20261031-100000.jsonl.gz"}) {
		t.Errorf("left %v", got)
	}
}

// R3: archiving writes the outing's note beside it, v1's sidecar; archived again,
// it keeps its note unless given one, adds the issues and keeps when it was first
// archived.
func TestArchivingKeepsANoteAndItsIssues(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "20261031-200509.jsonl.gz")
	first, err := Archive(dir, "20261031-200509", Note{Note: "the gap", Issues: []int{113}, Frames: 1}, at)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "20261031-200509" || first.Date != "2026-10-31" || first.Bytes != 1 || len(first.SHA256) != 64 ||
		first.ArchivedAt != "2026-10-31T20:05:09Z" || !slices.Equal(first.Issues, []int{113}) {
		t.Errorf("note %+v", first)
	}
	again, err := Archive(dir, "20261031-200509", Note{Issues: []int{354, 113}}, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again.Note != "the gap" || !slices.Equal(again.Issues, []int{113, 354}) || again.ArchivedAt != first.ArchivedAt {
		t.Errorf("archived again %+v", again)
	}
	read, err := ReadNote(dir, "20261031-200509")
	if err != nil || read == nil || read.Note != "the gap" || !slices.Equal(read.Issues, []int{113, 354}) {
		t.Errorf("read back %+v, %v", read, err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"20261031-200509.json", "20261031-200509.jsonl.gz"}) {
		t.Errorf("files %v", got)
	}
	if _, err := Archive(dir, "20261031-200510", Note{}, at); err == nil {
		t.Error("an outing with no recording was archived")
	}
	if _, err := Archive(dir, "../x", Note{}, at); err == nil {
		t.Error("a path was taken for an id")
	}
}

// Recordings are listed oldest first, a second of the same second after the
// first, as v1's outing_order sorts them, with their notes.
func TestRecordingsAreListedInOrder(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "20261031-200509-10.jsonl.gz", "20261031-200509-2.jsonl.gz", "20261031-200509.jsonl.gz",
		"20261030-000000.jsonl.gz", "20261031-200509.capture.gz", "x.jsonl.gz")
	if _, err := Archive(dir, "20261031-200509-2", Note{Note: "kept"}, at); err != nil {
		t.Fatal(err)
	}
	all, err := Recordings(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range all {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{"20261030-000000", "20261031-200509", "20261031-200509-2", "20261031-200509-10"}) {
		t.Errorf("ids %v", ids)
	}
	if all[2].Note == nil || all[2].Note.Note != "kept" || all[1].Note != nil {
		t.Errorf("notes %+v", all)
	}
}
