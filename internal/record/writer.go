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
	"strings"
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

// Options are how a Writer splits and keeps its outings (paddock#354).
type Options struct {
	// Idle is the silence that ends an outing: the outing's file is closed once
	// Idle passes after the last packet arrived, and the next packet starts a new
	// one. 0 never ends an outing before Close.
	Idle time.Duration
	// Keep is how long an outing is kept, from its last write: older ones that are
	// not archived are pruned when the Writer opens and when each outing starts. 0
	// keeps every one.
	Keep time.Duration

	// after is the Writer's timer, time.After unless a test sets it.
	after func(time.Duration) <-chan time.Time
}

// Stats is what a Writer recorded.
type Stats struct {
	// Path is the last outing's recording, "" while nothing has been written.
	Path string
	// Outings is the recordings started.
	Outings uint64
	// Frames is the lines written, every outing's.
	Frames uint64
	// Dropped is the packets not written: the buffer full, a value v1 cannot write,
	// or the file failing.
	Dropped uint64
}

// Writer records the packets ecu drives on in v1's format (AppendV1), one recording
// per outing, as v1's OutingRecorder does: an outing starts with the first packet,
// and with the first packet that arrives Options.Idle or more after the one before
// it, and ends once Idle has passed with no packet, or at Close. Its file is made
// in the directory then, named for the UTC time, as v1 names its outings:
// 20261028-213621.jsonl.gz, or the next second free when an outing of that second
// exists, so every id is v1's YYYYMMDD-HHMMSS. The name without its extension is
// the outing's id, and every file of the outing is named <id>.<something>, so
// pruning and archiving, which go by id, take them all.
//
// An outing's file is closed, whole, when the outing ends: a crash cuts short at
// most the outing being written. Write hands a packet to the writing goroutine
// without waiting. Write is called from one goroutine, and not after Close.
type Writer struct {
	dir    string
	opts   Options
	logger *slog.Logger
	now    func() time.Time

	in   chan arrival
	done chan struct{}

	path                     atomic.Pointer[string]
	outings, frames, dropped atomic.Uint64
}

// arrival is a packet and when it arrived.
type arrival struct {
	r  salsa.Received
	at time.Time
}

// Open checks that dir is a directory ecu can write to, making it if it is missing,
// and starts the writing goroutine, which prunes first. now names the files, dates
// the pruning and tells when the feed has been idle; it is time.Now outside the
// tests.
func Open(dir string, opts Options, logger *slog.Logger, now func() time.Time) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("recordings: %w", err)
	}
	probe, err := os.CreateTemp(dir, ".ecu-probe-*")
	if err != nil {
		return nil, fmt.Errorf("recordings: %w", err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	if opts.after == nil {
		opts.after = time.After
	}
	w := &Writer{
		dir: dir, opts: opts, logger: logger, now: now,
		in:   make(chan arrival, Buffer),
		done: make(chan struct{}),
	}
	go w.run()
	logger.Info("recording", "dir", dir, "idle_secs", opts.Idle.Seconds(), "keep_days", opts.Keep.Hours()/24)
	return w, nil
}

// Write hands r, which arrived at at, to the writing goroutine, or drops and counts
// it when the buffer is full.
func (w *Writer) Write(r salsa.Received, at time.Time) {
	select {
	case w.in <- arrival{r, at}:
	default:
		w.dropped.Add(1)
	}
}

// Close writes what Write has handed over, closes the outing being recorded and
// logs and returns the totals. ctx bounds the wait: a disk that stops answering is
// left behind with the goroutine still on it.
func (w *Writer) Close(ctx context.Context) Stats {
	close(w.in)
	select {
	case <-w.done:
	case <-ctx.Done():
		w.logger.WarnContext(ctx, "the recording was still being written when the time ran out")
	}
	s := w.Stats()
	w.logger.InfoContext(ctx, "recorded", "path", s.Path, "outings", s.Outings, "frames", s.Frames, "dropped", s.Dropped)
	return s
}

// Stats is what has been recorded so far.
func (w *Writer) Stats() Stats {
	s := Stats{Outings: w.outings.Load(), Frames: w.frames.Load(), Dropped: w.dropped.Load()}
	if p := w.path.Load(); p != nil {
		s.Path = *p
	}
	return s
}

// run is the writing goroutine. Every time is a packet's arrival, not when the
// goroutine reads it, so a disk that holds the goroutine up never splits an
// outing: one ends on a packet that arrived Idle or more after the one before it,
// and, so that its file is whole while the feed is quiet, once now is Idle past the
// last packet's arrival with no packet waiting.
func (w *Writer) run() {
	defer close(w.done)
	w.prune()
	var (
		o       *outing
		last    time.Time // the last packet's arrival
		skipped bool
		wake    <-chan time.Time // set while a timer is out
	)
	for {
		a, ok, woke := w.next(wake)
		if woke {
			wake = nil
			select {
			case a, ok = <-w.in: // a packet waiting goes first: its arrival decides
			default:
				if o == nil {
					continue
				}
				if left := last.Add(w.opts.Idle).Sub(w.now()); left > 0 {
					wake = w.opts.after(left)
					continue
				}
				w.end(o)
				o = nil
				continue
			}
		}
		if !ok {
			if o != nil {
				w.end(o)
			}
			return
		}
		if o != nil && w.opts.Idle > 0 && a.at.Sub(last) >= w.opts.Idle {
			w.end(o)
			o = nil
		}
		last = a.at
		if wake == nil && w.opts.Idle > 0 {
			wake = w.opts.after(last.Add(w.opts.Idle).Sub(w.now()))
		}
		line, written := AppendV1(nil, a.r.T, &a.r.Packet)
		if !written {
			if !skipped {
				w.logger.Warn("a packet with a value that is not finite is left out of the recording",
					"packet_id", a.r.PacketID)
				skipped = true
			}
			w.dropped.Add(1)
			continue
		}
		if o == nil {
			w.prune()
			o = w.start()
		}
		if o.write(line) {
			w.frames.Add(1)
		} else {
			w.dropped.Add(1)
		}
	}
}

// next is the next packet handed over, or woke when wake has fired, which is
// looked at first.
func (w *Writer) next(wake <-chan time.Time) (a arrival, ok, woke bool) {
	select {
	case <-wake:
		return a, true, true
	default:
	}
	select {
	case <-wake:
		return a, true, true
	case a, ok = <-w.in:
		return a, ok, false
	}
}

// outing is the outing being recorded: its id and its files. The raw datagrams
// (paddock#343) are a second file beside the recording, opened in start and
// closed in end under the same id.
type outing struct {
	id     string
	path   string
	f      *os.File
	gz     *gzip.Writer
	inPart int
	failed bool
	logger *slog.Logger
}

// start makes the outing's recording, never over another and never under an id
// any file in the directory has: the next second free. An outing whose file
// cannot be made drops its packets, logged once; the next outing tries again.
func (w *Writer) start() *outing {
	o := &outing{logger: w.logger}
	t := w.now().UTC()
	for {
		o.id = t.Format(stampLayout)
		o.path = filepath.Join(w.dir, o.id+Ext)
		if w.taken(o.id) {
			t = t.Add(time.Second)
			continue
		}
		f, err := os.OpenFile(o.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) //nolint:gosec // the directory is the operator's setting
		if errors.Is(err, fs.ErrExist) {
			t = t.Add(time.Second)
			continue
		}
		if err != nil {
			w.logger.Error("the recording could not start", "dir", w.dir, "err", err)
			o.failed = true
			return o
		}
		o.f, o.gz = f, gzip.NewWriter(f)
		break
	}
	if err := syncDir(w.dir); err != nil {
		w.logger.Warn("the recordings' directory could not be synced", "dir", w.dir, "err", err)
	}
	w.outings.Add(1)
	w.path.Store(&o.path)
	w.logger.Info("recording outing to", "path", o.path)
	return o
}

// taken says whether a file in the directory is named for the outing id, <id>.*.
// The names are matched as they are, never as a pattern, whatever the
// directory's path holds.
func (w *Writer) taken(id string) bool {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return false // the file's creation says what is wrong
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), id+".") {
			return true
		}
	}
	return false
}

// write writes one line, closing a gzip member every memberEvery lines, and says
// whether it was written. A file that fails stops the outing's recording: the
// error is logged once, and every packet after it in the outing is dropped.
func (o *outing) write(line []byte) bool {
	if o.failed {
		return false
	}
	if _, err := o.gz.Write(line); err != nil {
		o.fail(err)
		return false
	}
	if o.inPart++; o.inPart == memberEvery {
		o.inPart = 0
		if err := o.gz.Close(); err != nil {
			o.fail(err)
			return true
		}
		o.gz.Reset(o.f)
	}
	return true
}

func (o *outing) fail(err error) {
	o.logger.Error("the recording stopped", "path", o.path, "err", err)
	o.failed = true
}

// end closes the outing's recording: its last gzip member, then the file, synced
// first, so the file is whole on the disk once the outing has ended.
func (w *Writer) end(o *outing) {
	if o.f == nil {
		return
	}
	if !o.failed && o.inPart > 0 {
		if err := o.gz.Close(); err != nil {
			o.fail(err)
		}
	}
	if err := o.f.Sync(); err != nil && !o.failed {
		o.fail(err)
	}
	if err := o.f.Close(); err != nil && !o.failed {
		o.fail(err)
	}
	if !o.failed {
		w.logger.Info("outing recorded", "path", o.path)
	}
}

// prune removes the outings last written more than Options.Keep ago that are not
// archived.
func (w *Writer) prune() {
	if w.opts.Keep <= 0 {
		return
	}
	if _, err := Prune(w.dir, w.opts.Keep, w.now(), w.logger); err != nil {
		w.logger.Warn("the recordings could not be pruned", "dir", w.dir, "err", err)
	}
}
