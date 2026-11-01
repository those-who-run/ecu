package record

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/those-who-run/salsa"
)

// fakeClock is a Writer's clock and timer in a test: now is what the test sets,
// each timer the Writer sets is reported on armed, and fires when the test sends
// on fire. With gate set, the Writer waits on it as it sets each timer.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	armed chan time.Duration
	fire  chan time.Time
	gate  chan struct{}
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{t: t, armed: make(chan time.Duration, 64), fire: make(chan time.Time, 1)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.armed <- d
	if c.gate != nil {
		<-c.gate
	}
	return c.fire
}

// open is a Writer on c's clock and timer.
func open(tb testing.TB, dir string, opts Options, c *fakeClock) *Writer {
	tb.Helper()
	opts.after = c.after
	w, err := Open(dir, opts, quiet(), c.now)
	if err != nil {
		tb.Fatal(err)
	}
	return w
}

// eventually waits for cond, failing after 10 s: for what the writing goroutine
// does, never a wait on a time it keeps.
func eventually(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			tb.Fatalf("not %s after 10 s", what)
		}
	}
}

// drivePackets are n packets from t on, 1/60 s apart.
func drivePackets(t float64, n int) []salsa.Received {
	all := make([]salsa.Received, n)
	for i := range all {
		all[i].T = t + float64(i)/60
		all[i].PacketID = int32(i + 1) //nolint:gosec // a small count
	}
	return all
}

// arrived is when a packet of drivePackets arrived: at plus its T.
func arrived(r salsa.Received) time.Time {
	return at.Add(time.Duration(r.T * float64(time.Second)))
}

// whole reads a recording as plain gzip, as a copy of it would be read, and
// gives its lines, failing on a stream that does not end whole.
func whole(tb testing.TB, path string) int {
	tb.Helper()
	n, err := gzipLines(path)
	if err != nil {
		tb.Fatalf("%s is not a whole gzip: %v", path, err)
	}
	return n
}

func gzipLines(path string) (int, error) {
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	b, err := io.ReadAll(gz)
	return strings.Count(string(b), "\n"), err
}

// AC1 (paddock#354, R1): packets, then 10 s of silence, then more packets give
// two recordings, each holding its outing's packets, the second named for the
// next second free, as it starts in the same second here; a gap under the idle
// setting is the same outing.
func TestAnIdleFeedStartsANewRecording(t *testing.T) {
	dir := t.TempDir()
	c := newFakeClock(at)
	w := open(t, dir, Options{Idle: 5 * time.Second}, c)
	first, second := drivePackets(100, 700), drivePackets(100+700.0/60+10, 300)
	for _, r := range append(slices.Clone(first), second...) {
		w.Write(r, arrived(r))
	}
	s := w.Close(t.Context())
	if s.Outings != 2 || s.Frames != 1000 || s.Dropped != 0 {
		t.Fatalf("stats %+v; want 2 outings, 1,000 frames", s)
	}
	all, err := Recordings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != "20261031-200509" || all[1].ID != "20261031-200510" {
		t.Fatalf("recordings %+v", all)
	}
	for i, want := range [][]salsa.Received{first, second} {
		got := readBack(t, all[i].Path)
		if len(got) != len(want) || got[0].T != want[0].T || got[len(got)-1].T != want[len(want)-1].T {
			t.Errorf("outing %d: read back %d packets, want %d", i+1, len(got), len(want))
		}
	}

	w = open(t, t.TempDir(), Options{Idle: 5 * time.Second}, newFakeClock(at))
	for _, r := range append(drivePackets(0, 10), drivePackets(4.5, 10)...) {
		w.Write(r, arrived(r))
	}
	if s := w.Close(t.Context()); s.Outings != 1 {
		t.Errorf("a 4.5 s gap made %d outings", s.Outings)
	}
}

// An outing's file is closed, a whole gzip, once now is the idle setting past the
// last packet's arrival, while ecu still runs: a copy of it then is the whole
// outing. A timer that fires before then ends nothing and is set again for what
// is left.
func TestAnEndedOutingIsAWholeGzipWhileEcuRuns(t *testing.T) {
	dir := t.TempDir()
	c := newFakeClock(at)
	idle := time.Minute
	w := open(t, dir, Options{Idle: idle}, c)
	in := drivePackets(0, 1000) // one member and part of another
	for _, r := range in {
		w.Write(r, arrived(r))
	}
	eventually(t, "written", func() bool { return w.Stats().Frames == 1000 })
	if d := <-c.armed; d != idle {
		t.Errorf("the timer was set for %v, want %v", d, idle)
	}
	last := arrived(in[len(in)-1])
	path := filepath.Join(dir, "20261031-200509.jsonl.gz")

	c.set(last.Add(idle / 2))
	c.fire <- c.now()
	if d := <-c.armed; d != idle/2 {
		t.Errorf("set again for %v, want %v", d, idle/2)
	}
	if _, err := gzipLines(path); err == nil {
		t.Error("the outing ended before the feed was idle")
	}

	c.set(last.Add(idle))
	c.fire <- c.now()
	eventually(t, "a whole gzip", func() bool { n, err := gzipLines(path); return err == nil && n == 1000 })

	next := drivePackets(1000, 5)
	for _, r := range next {
		w.Write(r, arrived(r))
	}
	s := w.Close(t.Context())
	if s.Outings != 2 || s.Path != filepath.Join(dir, c.now().UTC().Format(stampLayout)+Ext) || whole(t, s.Path) != 5 {
		t.Errorf("stats %+v", s)
	}
}

// A timer that fires with a packet waiting ends nothing: the packet goes first,
// and its arrival, inside the idle setting, keeps the outing, however late the
// writing goroutine is.
func TestAPacketWaitingKeepsTheOuting(t *testing.T) {
	c := newFakeClock(at)
	c.gate = make(chan struct{})
	w := open(t, t.TempDir(), Options{Idle: time.Minute}, c)
	in := drivePackets(0, 2)
	w.Write(in[0], arrived(in[0]))
	<-c.armed // the goroutine is held as it sets the timer
	w.Write(in[1], arrived(in[1]))
	c.set(at.Add(time.Hour)) // the goroutine is very late
	c.fire <- c.now()
	close(c.gate)
	if s := w.Close(t.Context()); s.Outings != 1 || s.Frames != 2 {
		t.Errorf("stats %+v; want both packets in one outing", s)
	}
}

// An outing whose file cannot be made drops its packets; the next outing tries
// again.
func TestTheNextOutingTriesAgain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recordings")
	c := newFakeClock(at)
	w := open(t, dir, Options{Idle: time.Minute}, c)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	first := salsa.Received{T: 1}
	w.Write(first, arrived(first))
	eventually(t, "dropped", func() bool { return w.Stats().Dropped == 1 })
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	second := salsa.Received{T: 1000}
	w.Write(second, arrived(second))
	s := w.Close(t.Context())
	if s.Outings != 1 || s.Frames != 1 || s.Dropped != 1 || whole(t, s.Path) != 1 {
		t.Errorf("stats %+v", s)
	}
}

// touchAt makes each file in dir, last written at when.
func touchAt(tb testing.TB, dir string, when time.Time, names ...string) {
	tb.Helper()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			tb.Fatal(err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
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

// AC2 (paddock#354, R2, R5): a recording last written longer ago than the keep
// setting is pruned, with what is recorded beside it under its id; an archived
// one is not, nor a newer one, nor one that started long ago but was written to
// lately (a file of the days ecu wrote one per run), nor a file not named for an
// outing.
func TestOldRecordingsArePrunedButNotArchivedOnes(t *testing.T) {
	dir := t.TempDir()
	old := at.Add(-40 * 24 * time.Hour)
	touchAt(t, dir, old, "20260901-100000.jsonl.gz", "20260901-100000.capture.gz")
	touchAt(t, dir, old, "20260901-100000-2.jsonl.gz") // an id of v1's second outing in a second
	touchAt(t, dir, old, "20260902-100000.jsonl.gz", "20260902-100000.capture.gz", "20260902-100000.json")
	touchAt(t, dir, at.Add(-24*time.Hour), "20260801-100000.jsonl.gz") // a run's file, written yesterday
	touchAt(t, dir, at.Add(-24*time.Hour), "20261030-100000.jsonl.gz", "20261030-100000.capture.gz")
	touchAt(t, dir, old, "notes.txt", "2026.jsonl.gz", ".ecu-probe-1")
	pruned, err := Prune(dir, 30*24*time.Hour, at, quiet())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(pruned)
	if !slices.Equal(pruned, []string{"20260901-100000", "20260901-100000-2"}) {
		t.Errorf("pruned %v", pruned)
	}
	want := []string{".ecu-probe-1", "2026.jsonl.gz", "20260801-100000.jsonl.gz",
		"20260902-100000.capture.gz", "20260902-100000.json", "20260902-100000.jsonl.gz",
		"20261030-100000.capture.gz", "20261030-100000.jsonl.gz", "notes.txt"}
	if got := names(t, dir); !slices.Equal(got, want) {
		t.Errorf("left %v\nwant %v", got, want)
	}
}

// The Writer prunes when it opens and when each outing starts, with
// Options.Keep, and not with 0.
func TestTheWriterPrunes(t *testing.T) {
	dir := t.TempDir()
	touchAt(t, dir, at.Add(-40*24*time.Hour), "20260901-100000.jsonl.gz")
	touchAt(t, dir, at.Add(-20*24*time.Hour), "20261011-100000.jsonl.gz")
	c := newFakeClock(at)
	open(t, dir, Options{}, c).Close(t.Context())
	if got := names(t, dir); len(got) != 2 {
		t.Errorf("Keep 0 pruned: %v", got)
	}
	w := open(t, dir, Options{Keep: 30 * 24 * time.Hour}, c)
	eventually(t, "pruned at open", func() bool { return len(names(t, dir)) == 1 })
	c.set(at.Add(15 * 24 * time.Hour))
	r := salsa.Received{T: 1}
	w.Write(r, c.now())
	s := w.Close(t.Context())
	if got := names(t, dir); !slices.Equal(got, []string{filepath.Base(s.Path)}) {
		t.Errorf("left %v; want only the new outing", got)
	}
}

// v1's ArchivedOuting's fields (archive.py at v1.0.0): list_archive builds one
// from every sidecar, and a key missing is a TypeError.
var v1SidecarKeys = []string{"archived_at", "bytes", "cars", "date", "frames", "id", "issues", "kinds",
	"note", "sessions", "sha256", "tracks"}

// R3: archiving writes the outing's note beside it, v1's sidecar with every key
// v1's archive needs; archived again, it keeps its note unless given one, adds
// the issues and keeps when it was first archived.
func TestArchivingKeepsANoteAndItsIssues(t *testing.T) {
	dir := t.TempDir()
	touchAt(t, dir, at, "20261031-200509.jsonl.gz")
	first, err := Archive(dir, "20261031-200509", Note{Note: "the gap", Issues: []int{113}, Frames: 1}, at)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "20261031-200509" || first.Date != "2026-10-31" || first.Bytes != 1 || len(first.SHA256) != 64 ||
		first.ArchivedAt != "2026-10-31T20:05:09Z" || !slices.Equal(first.Issues, []int{113}) {
		t.Errorf("note %+v", first)
	}
	again, err := Archive(dir, "20261031-200509", Note{Issues: []int{354, 113}, Tracks: []string{"Suzuka"}}, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again.Note != "the gap" || !slices.Equal(again.Issues, []int{113, 354}) || again.ArchivedAt != first.ArchivedAt {
		t.Errorf("archived again %+v", again)
	}
	read, err := ReadNote(dir, "20261031-200509")
	if err != nil || read == nil || read.Note != "the gap" || !slices.Equal(read.Tracks, []string{"Suzuka"}) {
		t.Errorf("read back %+v, %v", read, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "20261031-200509.json")) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(keys)); !slices.Equal(got, v1SidecarKeys) {
		t.Errorf("keys %v\nv1's %v", got, v1SidecarKeys)
	}
	if keys["cars"] == nil || keys["kinds"] == nil {
		t.Errorf("an unknown list is not []: %s", b)
	}
	if fi, err := os.Stat(filepath.Join(dir, "20261031-200509.json")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("note's mode %v, %v", fi.Mode(), err)
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
// first, as v1's outing_order sorts them, with their notes; a note whose
// recording is gone is an orphan.
func TestRecordingsAreListedInOrder(t *testing.T) {
	dir := t.TempDir()
	touchAt(t, dir, at, "20261031-200509-10.jsonl.gz", "20261031-200509-2.jsonl.gz", "20261031-200509.jsonl.gz",
		"20261030-000000.jsonl.gz", "20261031-200509.capture.gz", "x.jsonl.gz", "20261029-000000.json", "x.json")
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
	if orphans, err := OrphanNotes(dir); err != nil || !slices.Equal(orphans, []string{"20261029-000000.json"}) {
		t.Errorf("orphans %v, %v", orphans, err)
	}
}
