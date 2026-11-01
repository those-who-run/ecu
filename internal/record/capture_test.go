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
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/salsa"
)

// datagrams are n datagrams of GT7's three lengths in turn, of random bytes, received from
// t on, 1/60 s apart, on the packets' clock and at the wall-clock time base plus t.
func datagrams(rng *rand.ChaCha8, base time.Time, t float64, n int) ([]float64, []Datagram) {
	lengths := []int{salsa.LenA, salsa.LenB, salsa.LenTilde}
	ts := make([]float64, n)
	all := make([]Datagram, n)
	for i := range all {
		ts[i] = t + float64(i)/60
		b := make([]byte, lengths[i%len(lengths)])
		_, _ = rng.Read(b)
		all[i] = Datagram{At: base.Add(time.Duration(ts[i] * float64(time.Second))).UnixNano(), Bytes: b}
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
		w.Write(salsa.Received{T: ts[i]})
	}
}

// The capture round trip (paddock#343, R1 and R2): each outing's datagrams, written
// as received, read back from <id>.twrcap beside its recording, byte for byte
// with their receive times, in order, and the file is whole once the outing ends.
func TestACaptureReadsBackAsTheDatagramsAndTimes(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{Idle: 3 * time.Second}, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.NewChaCha8([32]byte{3, 4, 3})
	base := time.Date(2026, 10, 31, 9, 23, 15, 123456789, time.UTC)
	ts1, first := datagrams(rng, base, 100, 1500) // past the write buffer, so it flushes
	ts2, second := datagrams(rng, base, 100+1500.0/60+10, 200)
	capture(w, ts1, first)
	capture(w, ts2, second)
	s := w.Close(t.Context())
	if s.Outings != 2 || s.Frames != 1700 || s.Captured != 1700 || s.Dropped != 0 || s.CaptureDropped != 0 {
		t.Fatalf("stats %+v; want 2 outings, 1,700 frames and datagrams", s)
	}
	for i, id := range []string{"20261031-200509", "20261031-200509-2"} {
		want := [][]Datagram{first, second}[i]
		got, err := ReadCaptureFile(filepath.Join(dir, id+CaptureExt))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d datagrams, want %d", id, len(got), len(want))
		}
		for j := range want {
			if got[j].At != want[j].At || !bytes.Equal(got[j].Bytes, want[j].Bytes) {
				t.Fatalf("%s: datagram %d read back as %d %x, wrote %d %x", id, j, got[j].At, got[j].Bytes, want[j].At, want[j].Bytes)
			}
		}
		if n := whole(t, filepath.Join(dir, id+Ext)); n != len(want) {
			t.Errorf("%s: %d lines recorded, want %d", id, n, len(want))
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "20261031-200509-2"+CaptureExt)) //nolint:gosec // the test's own file
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
// it over behind the listener, ends no outing: the gap counts from the latest.
func TestAPacketBehindTheDatagramsEndsNoOuting(t *testing.T) {
	w, err := Open(t.TempDir(), Options{Idle: time.Second}, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	ts, all := datagrams(rand.NewChaCha8([32]byte{1}), at, 0, 300) // 5 s
	for i, d := range all {
		w.Capture(ts[i], time.Unix(0, d.At), d.Bytes)
	}
	w.Write(salsa.Received{T: ts[0]}) // decoded 5 s behind the listener
	w.Capture(5, at, all[0].Bytes)
	if s := w.Close(t.Context()); s.Outings != 1 || s.Captured != 301 || s.Frames != 1 {
		t.Errorf("stats %+v; want one outing", s)
	}
}

// An outing that only captured (every datagram undecodable) still leaves a whole
// recording, of no lines; an outing with only packets, as a replay's, makes no
// capture.
func TestTheCaptureAndTheRecordingStartAndEndTogether(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{}, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	w.Capture(1, at, []byte{1, 2, 3})
	w.Close(t.Context())
	if n := whole(t, filepath.Join(dir, "20261031-200509"+Ext)); n != 0 {
		t.Errorf("%d lines", n)
	}
	if got, err := ReadCaptureFile(filepath.Join(dir, "20261031-200509"+CaptureExt)); err != nil || len(got) != 1 {
		t.Errorf("capture %v, %v", got, err)
	}
	w, err = Open(dir, Options{}, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(salsa.Received{T: 1})
	w.Close(t.Context())
	if _, err := os.Stat(filepath.Join(dir, "20261031-200509-2"+CaptureExt)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a capture with no datagrams: %v", err)
	}
}

// stalled is a Writer whose goroutine has not started, so its buffer fills.
func stalled(dir string) *Writer {
	return &Writer{
		dir: dir, logger: quiet(), now: clock,
		in: make(chan item, Buffer), done: make(chan struct{}),
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
	w.Write(salsa.Received{T: 1})
	if took := time.Since(start); took > time.Second {
		t.Errorf("handing over took %v", took)
	}
	go w.run()
	s := w.Close(t.Context())
	if s.Captured != Buffer || s.CaptureDropped != 10 || s.Dropped != 1 || s.Frames != 0 {
		t.Errorf("stats %+v; want %d captured, 10 dropped from the capture and the packet from the recording", s, Buffer)
	}
	got, err := ReadCaptureFile(filepath.Join(dir, "20261031-200509"+CaptureExt))
	if err != nil || len(got) != Buffer {
		t.Errorf("read back %d, %v", len(got), err)
	}
}

// R3: the capture is flushed once 10 s of datagrams have gone by, so a killed ecu
// loses at most the last 10 s, while the outing is still open.
func TestTheCaptureIsFlushedEveryTenSeconds(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{}, quiet(), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(t.Context())
	d := make([]byte, salsa.LenA)
	w.Capture(0, at, d)
	w.Capture(5, at.Add(5*time.Second), d)
	w.Capture(10, at.Add(10*time.Second), d)
	path := filepath.Join(dir, "20261031-200509"+CaptureExt)
	want := int64(len(CaptureMagic) + 3*(captureHeader+len(d)))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if fi, err := os.Stat(path); err == nil && fi.Size() == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not flushed to %d bytes after 5 s", path, want)
		}
	}
	got, err := ReadCaptureFile(path)
	if err != nil || len(got) != 3 {
		t.Errorf("read back %d, %v", len(got), err)
	}
}

// A capture that cannot be made (a file of its name that is not ecu's, never
// written over) is logged once and its datagrams counted as dropped; the
// recording goes on.
func TestACaptureThatCannotStartLeavesTheRecording(t *testing.T) {
	dir := t.TempDir()
	theirs := filepath.Join(dir, "20261031-200509"+CaptureExt)
	if err := os.WriteFile(theirs, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	w, err := Open(dir, Options{}, slog.New(slog.NewJSONHandler(&log, nil)), clock)
	if err != nil {
		t.Fatal(err)
	}
	w.Capture(1, at, []byte{1})
	w.Write(salsa.Received{T: 1})
	w.Capture(2, at, []byte{2})
	s := w.Close(t.Context())
	if s.Frames != 1 || s.Captured != 0 || s.CaptureDropped != 2 {
		t.Errorf("stats %+v", s)
	}
	if n := strings.Count(log.String(), "the capture stopped"); n != 1 {
		t.Errorf("logged %d times: %s", n, log.String())
	}
	if b, _ := os.ReadFile(theirs); string(b) != "theirs" { //nolint:gosec // the test's own file
		t.Errorf("written over: %q", b)
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
