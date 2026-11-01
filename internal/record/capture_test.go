package record

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/salsa"
)

// datagrams are n datagrams of GT7's three lengths in turn, of random bytes,
// received from t on, 1/60 s apart: ts on the packets' clock, and each Datagram's
// At its arrival, at plus its t, as arrived dates a packet.
func datagrams(rng *rand.ChaCha8, t float64, n int) ([]float64, []Datagram) {
	lengths := []int{salsa.LenA, salsa.LenB, salsa.LenTilde}
	ts := make([]float64, n)
	all := make([]Datagram, n)
	for i := range all {
		ts[i] = t + float64(i)/60
		b := make([]byte, lengths[i%len(lengths)])
		_, _ = rng.Read(b)
		all[i] = Datagram{At: arrived(salsa.Received{T: ts[i]}).UnixNano(), Bytes: b}
	}
	return ts, all
}

// capture hands every datagram to w from one buffer, reused as the listener reuses
// its own, each with its packet after it, as the frame path hands it over later.
func capture(w *Writer, ts []float64, all []Datagram) {
	buf := make([]byte, 2048)
	for i, d := range all {
		n := copy(buf, d.Bytes)
		w.Capture(ts[i], time.Unix(0, d.At), buf[:n])
		clear(buf)
		r := salsa.Received{T: ts[i]}
		w.Write(r, arrived(r))
	}
}

// captures are the captures in dir, by name.
func captures(tb testing.TB, dir string) []string {
	tb.Helper()
	all, err := filepath.Glob(filepath.Join(dir, "*"+CaptureExt))
	if err != nil {
		tb.Fatal(err)
	}
	slices.Sort(all)
	return all
}

// The capture round trip (paddock#343, R1 and R2): each outing's datagrams, written
// as received, read back from <id>.twrcap beside its recording, under the same
// id, byte for byte with their receive times, in order, and the file is whole
// once the outing ends.
func TestACaptureReadsBackAsTheDatagramsAndTimes(t *testing.T) {
	dir := t.TempDir()
	w := open(t, dir, Options{Idle: 5 * time.Second}, newFakeClock(at))
	rng := rand.NewChaCha8([32]byte{3, 4, 3})
	ts1, first := datagrams(rng, 100, 1500) // past the write buffer, so it flushes
	ts2, second := datagrams(rng, 100+1500.0/60+10, 200)
	capture(w, ts1, first)
	capture(w, ts2, second)
	s := w.Close(t.Context())
	if s.Outings != 2 || s.Frames != 1700 || s.Captured != 1700 || s.Dropped != 0 || s.CaptureDropped != 0 {
		t.Fatalf("stats %+v; want 2 outings, 1,700 frames and datagrams", s)
	}
	recs, err := Recordings(dir)
	if err != nil {
		t.Fatal(err)
	}
	caps := captures(t, dir)
	if len(recs) != 2 || len(caps) != 2 {
		t.Fatalf("recordings %+v, captures %v", recs, caps)
	}
	for i, rec := range recs {
		want := [][]Datagram{first, second}[i]
		if caps[i] != filepath.Join(dir, rec.ID+CaptureExt) {
			t.Fatalf("outing %s's capture is %s", rec.ID, caps[i])
		}
		got, err := ReadCaptureFile(caps[i])
		if err != nil {
			t.Fatalf("%s: %v", rec.ID, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d datagrams, want %d", rec.ID, len(got), len(want))
		}
		for j := range want {
			if got[j].At != want[j].At || !bytes.Equal(got[j].Bytes, want[j].Bytes) {
				t.Fatalf("%s: datagram %d read back as %d %x, wrote %d %x", rec.ID, j, got[j].At, got[j].Bytes, want[j].At, want[j].Bytes)
			}
		}
		if n := whole(t, rec.Path); n != len(want) {
			t.Errorf("%s: %d lines recorded, want %d", rec.ID, n, len(want))
		}
	}
	raw, err := os.ReadFile(caps[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "TWRCAP1\n") {
		t.Errorf("starts %q", raw[:8])
	}
	// R2's layout, by hand: the first record after the magic.
	head := raw[len(CaptureMagic):]
	if at := int64(binary.BigEndian.Uint64(head)); at != second[0].At || //nolint:gosec // the bits of an int64
		int(binary.BigEndian.Uint32(head[8:])) != len(second[0].Bytes) || !bytes.Equal(head[12:12+len(second[0].Bytes)], second[0].Bytes) {
		t.Errorf("the first record is not R2's layout: %x", head[:16])
	}
}

// A packet decoded after later datagrams were captured, as the frame path hands
// it over behind the listener, ends no outing: the gap counts from the latest
// arrival, not the last handed over.
func TestAPacketBehindTheDatagramsEndsNoOuting(t *testing.T) {
	w := open(t, t.TempDir(), Options{Idle: time.Second}, newFakeClock(at))
	ts, all := datagrams(rand.NewChaCha8([32]byte{1}), 0, 300) // 5 s
	for i, d := range all {
		w.Capture(ts[i], time.Unix(0, d.At), d.Bytes)
	}
	r := salsa.Received{T: ts[0]}
	w.Write(r, arrived(r)) // decoded 5 s behind the listener
	w.Capture(5, arrived(salsa.Received{T: 5}), all[0].Bytes)
	if s := w.Close(t.Context()); s.Outings != 1 || s.Captured != 301 || s.Frames != 1 {
		t.Errorf("stats %+v; want one outing", s)
	}
}

// An outing that only captured (every datagram undecodable) still leaves a whole
// recording, of no lines; an outing with only packets, as a replay's, makes no
// capture.
func TestTheCaptureAndTheRecordingStartAndEndTogether(t *testing.T) {
	dir := t.TempDir()
	w := open(t, dir, Options{}, newFakeClock(at))
	w.Capture(1, at, []byte{1, 2, 3})
	w.Close(t.Context())
	recs, err := Recordings(dir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recordings %+v, %v", recs, err)
	}
	if n := whole(t, recs[0].Path); n != 0 {
		t.Errorf("%d lines", n)
	}
	if got, err := ReadCaptureFile(filepath.Join(dir, recs[0].ID+CaptureExt)); err != nil || len(got) != 1 {
		t.Errorf("capture %v, %v", got, err)
	}

	w = open(t, dir, Options{}, newFakeClock(at))
	w.Write(salsa.Received{T: 1}, at)
	w.Close(t.Context())
	if recs, err = Recordings(dir); err != nil || len(recs) != 2 {
		t.Fatalf("recordings %+v, %v", recs, err)
	}
	if caps := captures(t, dir); len(caps) != 1 || caps[0] != filepath.Join(dir, recs[0].ID+CaptureExt) {
		t.Errorf("captures %v; want only the first outing's", caps)
	}
}

// stalled is a Writer whose goroutine has not started, so its buffer fills.
func stalled(dir string) *Writer {
	return &Writer{
		dir: dir, opts: Options{after: time.After}, logger: quiet(), now: clock,
		in: make(chan arrival, Buffer), done: make(chan struct{}),
	}
}

// AC3 (paddock#343, R3): a full buffer drops datagrams from the capture and counts
// them, and neither Capture nor Write waits; what the buffer held is written.
func TestAFullBufferDropsFromTheCaptureAndNeverWaits(t *testing.T) {
	dir := t.TempDir()
	w := stalled(dir)
	d := make([]byte, salsa.LenA)
	start := time.Now()
	for i := range Buffer + 10 {
		w.Capture(float64(i)/60, at, d)
	}
	w.Write(salsa.Received{T: 1}, at)
	if took := time.Since(start); took > time.Second {
		t.Errorf("handing over took %v", took)
	}
	go w.run()
	s := w.Close(t.Context())
	if s.Captured != Buffer || s.CaptureDropped != 10 || s.Dropped != 1 || s.Frames != 0 {
		t.Errorf("stats %+v; want %d captured, 10 dropped from the capture and the packet from the recording", s, Buffer)
	}
	caps := captures(t, dir)
	if len(caps) != 1 {
		t.Fatalf("captures %v", caps)
	}
	if got, err := ReadCaptureFile(caps[0]); err != nil || len(got) != Buffer {
		t.Errorf("read back %d, %v", len(got), err)
	}
}

// R3: the capture is flushed once 10 s of datagrams have gone by, so a killed ecu
// loses at most the last 10 s, while the outing is still open.
func TestTheCaptureIsFlushedEveryTenSeconds(t *testing.T) {
	dir := t.TempDir()
	w := open(t, dir, Options{}, newFakeClock(at))
	defer w.Close(t.Context())
	d := make([]byte, salsa.LenA)
	w.Capture(0, at, d)
	w.Capture(5, at.Add(5*time.Second), d)
	w.Capture(10, at.Add(10*time.Second), d)
	path := filepath.Join(dir, "20261031-200509"+CaptureExt)
	want := int64(len(CaptureMagic) + 3*(captureHeader+len(d)))
	eventually(t, "flushed", func() bool { fi, err := os.Stat(path); return err == nil && fi.Size() == want })
	if got, err := ReadCaptureFile(path); err != nil || len(got) != 3 {
		t.Errorf("read back %d, %v", len(got), err)
	}
}

// Once the feed stops, what the capture and the recording hold goes to the disk
// after 10 s, though the outing stays open until the idle setting has passed: a
// killed ecu loses at most the last 10 s however long the idle setting is
// (paddock#343's review).
func TestAQuietOutingIsFlushedAfterTenSeconds(t *testing.T) {
	dir := t.TempDir()
	c := newFakeClock(at)
	w := open(t, dir, Options{Idle: 10 * time.Minute}, c)
	defer w.Close(t.Context())
	d := make([]byte, salsa.LenTilde)
	w.Capture(0, at, d)
	w.Write(salsa.Received{T: 0}, at)
	if armed := <-c.armed; armed != flushEvery {
		t.Errorf("the timer was set for %v, want %v", armed, flushEvery)
	}
	c.set(at.Add(flushEvery))
	c.fire <- c.now()
	if armed := <-c.armed; armed != 10*time.Minute-flushEvery {
		t.Errorf("set again for %v, want what is left of the idle setting", armed)
	}
	if got, err := ReadCaptureFile(filepath.Join(dir, "20261031-200509"+CaptureExt)); err != nil || len(got) != 1 {
		t.Errorf("the capture read back %d, %v", len(got), err)
	}
	if n, err := gzipLines(filepath.Join(dir, "20261031-200509"+Ext)); err != nil || n != 1 {
		t.Errorf("the recording read back %d lines, %v", n, err)
	}
	if s := w.Stats(); s.Outings != 1 {
		t.Errorf("stats %+v", s)
	}
}

// A capture that cannot be made is logged once and its datagrams are not written;
// one whose flush fails says so (paddock#343's review).
func TestACaptureThatFailsIsLoggedOnce(t *testing.T) {
	dir := t.TempDir()
	rec, err := os.Create(filepath.Join(dir, "x"+Ext)) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rec.Close() }()
	var log bytes.Buffer
	o := &outing{logger: slog.New(slog.NewJSONHandler(&log, nil)), f: rec, capPath: filepath.Join(dir, "gone", "x"+CaptureExt)}
	if o.capture(1, []byte{1}) || o.capture(2, []byte{2}) {
		t.Error("written to a capture that could not be made")
	}
	if n := strings.Count(log.String(), "the capture stopped"); n != 1 {
		t.Errorf("logged %d times: %s", n, log.String())
	}

	o = &outing{logger: quiet(), f: rec, capPath: filepath.Join(dir, "y"+CaptureExt)}
	if !o.capture(0, []byte{1}) {
		t.Fatal("not written")
	}
	_ = o.capF.Close()
	if o.capture(flushEvery.Nanoseconds(), []byte{2}) {
		t.Error("a flush that failed said written")
	}
	o.endCapture()
}

// A capture is pruned after its own keep setting, from its outing's last write,
// and the rest of its outing kept; an archived outing keeps its capture, and a
// newer one is kept (paddock#343's review).
func TestOldCapturesArePrunedBeforeTheirRecordings(t *testing.T) {
	dir := t.TempDir()
	now := at
	old, recent := now.Add(-10*24*time.Hour), now.Add(-2*24*time.Hour)
	touchAt(t, dir, old, "20261001-100000"+Ext, "20261001-100000"+CaptureExt,
		"20261001-110000"+Ext, "20261001-110000"+CaptureExt, "20261001-110000"+NoteExt)
	touchAt(t, dir, recent, "20261029-100000"+Ext, "20261029-100000"+CaptureExt)
	pruned, err := PruneCaptures(dir, 7*24*time.Hour, now, quiet())
	if err != nil || !slices.Equal(pruned, []string{"20261001-100000"}) {
		t.Fatalf("pruned %v, %v", pruned, err)
	}
	want := []string{"20261001-100000" + Ext, "20261001-110000" + NoteExt, "20261001-110000" + CaptureExt,
		"20261001-110000" + Ext, "20261029-100000" + CaptureExt, "20261029-100000" + Ext}
	slices.Sort(want)
	if got := names(t, dir); !slices.Equal(got, want) {
		t.Errorf("left %v, want %v", got, want)
	}

	// The Writer prunes them as it opens.
	touchAt(t, dir, old, "20261002-100000"+Ext, "20261002-100000"+CaptureExt)
	w := open(t, dir, Options{CaptureKeep: 7 * 24 * time.Hour}, newFakeClock(now))
	w.Close(t.Context())
	if _, err := os.Stat(filepath.Join(dir, "20261002-100000"+CaptureExt)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("not pruned by the Writer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "20261002-100000"+Ext)); err != nil {
		t.Errorf("its recording went too: %v", err)
	}
}

// ReadCapture refuses a file that is not a capture, and gives every whole record
// of one cut short with an error.
func TestReadCaptureRefusesABrokenCapture(t *testing.T) {
	if _, err := ReadCapture(strings.NewReader("TWRCAP2\n")); !errors.Is(err, ErrNotACapture) {
		t.Errorf("another magic: %v", err)
	}
	if _, err := ReadCapture(strings.NewReader("")); !errors.Is(err, ErrNotACapture) {
		t.Errorf("empty: %v", err)
	}
	if got, err := ReadCapture(strings.NewReader(CaptureMagic)); err != nil || len(got) != 0 {
		t.Errorf("no records: %v, %v", got, err)
	}
	record := func(at int64, b []byte) []byte {
		r := binary.BigEndian.AppendUint64(nil, uint64(at))  //nolint:gosec // the bits of an int64
		r = binary.BigEndian.AppendUint32(r, uint32(len(b))) //nolint:gosec // small
		return append(r, b...)
	}
	file := append([]byte(CaptureMagic), record(-1, []byte{1, 2})...)
	file = append(file, record(7, []byte{3, 4, 5})...)
	for cut := len(CaptureMagic) + 14 + 1; cut < len(file); cut++ {
		got, err := ReadCapture(bytes.NewReader(file[:cut]))
		if !errors.Is(err, io.ErrUnexpectedEOF) || len(got) != 1 || got[0].At != -1 || !bytes.Equal(got[0].Bytes, []byte{1, 2}) {
			t.Errorf("cut at %d: %v, %v", cut, got, err)
		}
	}
	huge := append([]byte(CaptureMagic), binary.BigEndian.AppendUint32(make([]byte, 8), 1<<20)...)
	if _, err := ReadCapture(bytes.NewReader(huge)); err == nil || !strings.Contains(err.Error(), "more than a datagram") {
		t.Errorf("a 1 MiB record: %v", err)
	}
	if _, err := ReadCaptureFile(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a missing file read")
	}
}

// BenchmarkCapture is what capturing adds to the listener for each datagram: a
// copy and a hand-off that never waits (R3). The writing goroutine runs beside it.
func BenchmarkCapture(b *testing.B) {
	w, err := Open(b.TempDir(), Options{}, quiet(), clock)
	if err != nil {
		b.Fatal(err)
	}
	d := make([]byte, salsa.LenTilde)
	now := time.Now()
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		w.Capture(float64(i)/60, now, d)
	}
	b.StopTimer()
	w.Close(b.Context())
}

// BenchmarkCaptureWrite is the writing goroutine's cost for each "~" datagram: how
// far above 60 Hz it keeps up.
func BenchmarkCaptureWrite(b *testing.B) {
	dir := b.TempDir()
	o := &outing{logger: quiet(), capPath: filepath.Join(dir, "x"+CaptureExt)}
	f, err := os.Create(filepath.Join(dir, "x"+Ext)) //nolint:gosec // the benchmark's own file
	if err != nil {
		b.Fatal(err)
	}
	o.f = f
	d := make([]byte, salsa.LenTilde)
	at := time.Now().UnixNano()
	b.SetBytes(int64(captureHeader + len(d)))
	for i := 0; b.Loop(); i++ {
		if !o.capture(at+int64(i)*16_666_667, d) {
			b.Fatal("not written")
		}
	}
	o.endCapture()
	_ = f.Close()
}
