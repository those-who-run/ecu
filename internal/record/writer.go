package record

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/those-who-run/salsa"
)

// Buffer is how many packets wait for the writing goroutine: about 68 s at 60 Hz.
// A packet beyond it is dropped from the recording and counted; the frame path
// never waits on the disk.
const Buffer = 4096

// memberEvery is how many lines go in one gzip member. v1 flushed its writer every
// 600 frames; closing a member instead leaves a file whose members up to the last
// 10 s are whole if ecu is killed mid-drive, and salsa and v1 both read a file of
// many members as one stream.
const memberEvery = 600

// Stats is what a Writer recorded.
type Stats struct {
	// Path is the recording, "" while nothing has been written.
	Path string
	// Frames is the lines written.
	Frames uint64
	// Dropped is the packets not written: the buffer full, a value v1 cannot write,
	// or the file failing.
	Dropped uint64
}

// Writer records the packets ecu drives on, one recording per Writer, in v1's
// format (AppendV1). The file is made in its directory on the first packet, named
// for the UTC time then, as v1 names its outings: 20261028-213621.jsonl.gz, with
// -2, -3 and so on when a recording of that second exists. Write hands a packet to
// the writing goroutine without waiting. Write is called from one goroutine, and
// not after Close.
type Writer struct {
	dir    string
	logger *slog.Logger
	now    func() time.Time

	in   chan salsa.Received
	done chan struct{}

	path            atomic.Pointer[string]
	frames, dropped atomic.Uint64
}

// Open checks that dir is a directory ecu can write to, making it if it is missing,
// and starts the writing goroutine. now names the file; it is time.Now outside the
// tests.
func Open(dir string, logger *slog.Logger, now func() time.Time) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("recordings: %w", err)
	}
	probe, err := os.CreateTemp(dir, ".ecu-probe-*")
	if err != nil {
		return nil, fmt.Errorf("recordings: %w", err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	w := &Writer{
		dir: dir, logger: logger, now: now,
		in:   make(chan salsa.Received, Buffer),
		done: make(chan struct{}),
	}
	go w.run()
	logger.Info("recording", "dir", dir)
	return w, nil
}

// Write hands r to the writing goroutine, or drops and counts it when the buffer is
// full.
func (w *Writer) Write(r salsa.Received) {
	select {
	case w.in <- r:
	default:
		w.dropped.Add(1)
	}
}

// Close writes what Write has handed over, closes the recording and logs and
// returns the totals. ctx bounds the wait: a disk that stops answering is left
// behind with the goroutine still on it.
func (w *Writer) Close(ctx context.Context) Stats {
	close(w.in)
	select {
	case <-w.done:
	case <-ctx.Done():
		w.logger.WarnContext(ctx, "the recording was still being written when the time ran out")
	}
	s := w.Stats()
	w.logger.InfoContext(ctx, "recorded", "path", s.Path, "frames", s.Frames, "dropped", s.Dropped)
	return s
}

// Stats is what has been recorded so far.
func (w *Writer) Stats() Stats {
	s := Stats{Frames: w.frames.Load(), Dropped: w.dropped.Load()}
	if p := w.path.Load(); p != nil {
		s.Path = *p
	}
	return s
}

// run is the writing goroutine. A file that fails stops the recording: the error
// is logged once, and every packet after it is counted as dropped.
func (w *Writer) run() {
	defer close(w.done)
	var (
		f       *os.File
		gz      *gzip.Writer
		line    []byte
		inPart  int
		failed  bool
		skipped bool
	)
	fail := func(err error) {
		w.logger.Error("the recording stopped", "path", f.Name(), "err", err)
		failed = true
	}
	for r := range w.in {
		if failed {
			w.dropped.Add(1)
			continue
		}
		var ok bool
		line, ok = AppendV1(line[:0], r.T, &r.Packet)
		if !ok {
			if !skipped {
				w.logger.Warn("a packet with a value that is not finite is left out of the recording",
					"packet_id", r.PacketID)
				skipped = true
			}
			w.dropped.Add(1)
			continue
		}
		if f == nil {
			var err error
			if f, err = w.create(); err != nil {
				w.logger.Error("the recording could not start", "dir", w.dir, "err", err)
				failed = true
				w.dropped.Add(1)
				continue
			}
			gz = gzip.NewWriter(f)
		}
		if _, err := gz.Write(line); err != nil {
			fail(err)
			w.dropped.Add(1)
			continue
		}
		w.frames.Add(1)
		if inPart++; inPart == memberEvery {
			inPart = 0
			if err := gz.Close(); err != nil {
				fail(err)
				continue
			}
			gz.Reset(f)
		}
	}
	if f == nil {
		return
	}
	if !failed && inPart > 0 {
		if err := gz.Close(); err != nil {
			fail(err)
		}
	}
	if err := f.Close(); err != nil && !failed {
		fail(err)
	}
}

// create makes the recording's file, never over another.
func (w *Writer) create() (*os.File, error) {
	stamp := w.now().UTC().Format("20060102-150405")
	for n := 1; ; n++ {
		name := stamp
		if n > 1 {
			name += "-" + strconv.Itoa(n)
		}
		path := filepath.Join(w.dir, name+".jsonl.gz")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) //nolint:gosec // the directory is the operator's setting
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		w.path.Store(&path)
		w.logger.Info("recording to", "path", path)
		return f, nil
	}
}
