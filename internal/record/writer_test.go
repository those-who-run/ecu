package record

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/salsa"
)

var at = time.Date(2026, 10, 31, 20, 5, 9, 0, time.UTC)

func clock() time.Time { return at }

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// packets is every packet of a v1 recording.
func packets(tb testing.TB, name string) []salsa.Received {
	tb.Helper()
	src, err := salsa.OpenRecording(filepath.Join(fixtures(tb), name+".jsonl.gz"))
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	var all []salsa.Received
	for {
		r, err := src.Next(tb.Context())
		if errors.Is(err, io.EOF) {
			return all
		}
		if err != nil {
			tb.Fatal(err)
		}
		all = append(all, r)
	}
}

// readBack is every packet of a recording, as salsa's recording Source reads it.
func readBack(tb testing.TB, path string) []salsa.Received {
	tb.Helper()
	src, err := salsa.OpenRecording(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	var all []salsa.Received
	for {
		r, err := src.Next(tb.Context())
		if errors.Is(err, io.EOF) {
			return all
		}
		if err != nil {
			tb.Fatal(err)
		}
		all = append(all, r)
	}
}

// A recording the Writer makes reads back through salsa's recording Source as the
// packets written, across several gzip members, and its lines are v1's.
func TestARecordingReadsBackThroughSalsa(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	in := packets(t, exact[0]) // 1,221 frames: three members
	for i, r := range in {
		w.Write(r)
		if i%500 == 499 {
			time.Sleep(10 * time.Millisecond) // well inside the buffer either way
		}
	}
	s := w.Close(t.Context())
	want := filepath.Join(dir, "20261031-200509.jsonl.gz")
	if s.Path != want || s.Frames != uint64(len(in)) || s.Dropped != 0 {
		t.Fatalf("stats %+v; want %s, %d frames, none dropped", s, want, len(in))
	}
	out := readBack(t, s.Path)
	if len(out) != len(in) {
		t.Fatalf("read back %d packets, wrote %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("packet %d: read back %+v, wrote %+v", i, out[i], in[i])
		}
	}
	orig := lines(t, filepath.Join(fixtures(t), exact[0]+".jsonl.gz"))
	got := lines(t, s.Path)
	for i := range orig {
		if got[i] != orig[i] {
			t.Fatalf("line %d differs from v1's:\n got %s\nwant %s", i+1, got[i], orig[i])
		}
	}
}

// A second recording in the same second takes -2, never the first one's file; a
// Writer that gets no packet makes no file.
func TestRecordingsNeverOverwriteEachOther(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for range 2 {
		w, err := Open(dir, quiet(), clock)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(salsa.Received{T: 1})
		paths = append(paths, w.Close(t.Context()).Path)
	}
	if paths[0] != filepath.Join(dir, "20261031-200509.jsonl.gz") || paths[1] != filepath.Join(dir, "20261031-200509-2.jsonl.gz") {
		t.Errorf("paths %v", paths)
	}
	w, err := Open(dir, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if s := w.Close(t.Context()); s.Path != "" {
		t.Errorf("no packets, yet %s", s.Path)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("%d files, want 2", len(entries))
	}
}

// A directory that is missing is made; one ecu cannot write to stops it at start.
func TestOpenChecksTheDirectory(t *testing.T) {
	made := filepath.Join(t.TempDir(), "a", "b")
	w, err := Open(made, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	w.Close(t.Context())
	if fi, err := os.Stat(made); err != nil || !fi.IsDir() {
		t.Errorf("%s not made: %v", made, err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file, quiet(), clock); err == nil {
		t.Error("a file as the directory was taken")
	}
}

// A packet v1 cannot write is left out and counted, and logged once; the rest are
// written.
func TestAPacketThatCannotBeWrittenIsCounted(t *testing.T) {
	var log bytes.Buffer
	w, err := Open(t.TempDir(), slog.New(slog.NewJSONHandler(&log, nil)), clock)
	if err != nil {
		t.Fatal(err)
	}
	bad := salsa.Received{}
	bad.FuelLevel = float32(math.NaN())
	w.Write(bad)
	w.Write(bad)
	w.Write(salsa.Received{T: 2})
	s := w.Close(t.Context())
	if s.Frames != 1 || s.Dropped != 2 {
		t.Errorf("stats %+v; want 1 frame and 2 dropped", s)
	}
	if n := strings.Count(log.String(), "not finite"); n != 1 {
		t.Errorf("logged %d times: %s", n, log.String())
	}
	if got := readBack(t, s.Path); len(got) != 1 || got[0].T != 2 {
		t.Errorf("read back %+v", got)
	}
}

// When the directory goes, the recording stops, logged, and the frame path never
// waits on it; Write past the buffer drops and counts.
func TestAFailingRecordingNeverHoldsTheFramePath(t *testing.T) {
	dir := t.TempDir()
	var log bytes.Buffer
	w, err := Open(dir, slog.New(slog.NewJSONHandler(&log, nil)), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for range Buffer * 2 {
		w.Write(salsa.Received{T: 1})
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Write took %v", d)
	}
	s := w.Close(t.Context())
	if s.Frames != 0 || s.Dropped != Buffer*2 {
		t.Errorf("stats %+v; want every packet dropped", s)
	}
	if !strings.Contains(log.String(), "the recording could not start") {
		t.Errorf("not logged: %s", log.String())
	}
}

// Close gives up on a writer that is stuck once its context is done.
func TestCloseIsBounded(t *testing.T) {
	w, err := Open(t.TempDir(), quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w.Close(ctx) // returns whether or not the goroutine finished first
	<-w.done
}
