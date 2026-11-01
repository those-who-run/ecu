package record

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// CaptureExt is a capture's extension: <id>.twrcap beside the outing's recording
// (paddock#343).
const CaptureExt = ".twrcap"

// CaptureMagic starts every capture: the format of the stand-alone capture script
// written for paddock#30, which a capture keeps.
const CaptureMagic = "TWRCAP1\n"

// captureHeader is a record's header: the receive time, int64 nanoseconds since the
// Unix epoch, then the datagram's length, uint32; both big-endian.
const captureHeader = 8 + 4

// maxDatagram is the longest datagram a capture holds: UDP's own limit. A reader
// takes a longer length as a broken file rather than allocate it.
const maxDatagram = 1<<16 - 1

// captureBuffer is the capture's write buffer: about 3.5 s of "~" packets at 60 Hz.
const captureBuffer = 64 << 10

// flushEvery is the longest a datagram waits in the write buffer, on the
// datagrams' own clock: a killed pod loses at most the last 10 s (R3).
const flushEvery = 10 * time.Second

// Datagram is one record of a capture: a datagram as the PS5 sent it, still
// encrypted, and when ecu received it.
type Datagram struct {
	// At is the receive time, in nanoseconds since the Unix epoch.
	At int64
	// Bytes is the datagram, exactly as received.
	Bytes []byte
}

// Capture hands a datagram received from the PS5, as received, at the wall-clock
// time at and t seconds after the live source started (Received.T's clock), to the
// writing goroutine without waiting, to be written to the outing's capture
// (paddock#343); a datagram that does not fit in the buffer is dropped from the
// capture and counted. datagram is copied: the caller may reuse it at once.
// Capture may be called from another goroutine than Write's, and not after Close.
func (w *Writer) Capture(t float64, at time.Time, datagram []byte) {
	it := item{raw: append([]byte(nil), datagram...), at: at.UnixNano()}
	it.T = t
	select {
	case w.in <- it:
	default:
		w.captureDropped.Add(1)
	}
}

// capture appends one record to the outing's capture, opening it with the
// outing's first datagram, and says whether it was written. A capture that cannot
// be made or that fails is logged once, and the outing's datagrams after it are
// dropped; the recording goes on.
func (o *outing) capture(at int64, datagram []byte) bool {
	if o.capFailed {
		return false
	}
	if o.capF == nil && !o.openCapture(at) {
		return false
	}
	var head [captureHeader]byte
	binary.BigEndian.PutUint64(head[:8], uint64(at))            //nolint:gosec // the bits of an int64, as the format says
	binary.BigEndian.PutUint32(head[8:], uint32(len(datagram))) //nolint:gosec // a UDP datagram's length fits
	if _, err := o.capW.Write(head[:]); err != nil {
		o.failCapture(err)
		return false
	}
	if _, err := o.capW.Write(datagram); err != nil {
		o.failCapture(err)
		return false
	}
	if at-o.flushedAt >= flushEvery.Nanoseconds() {
		o.flushedAt = at
		if err := o.capW.Flush(); err != nil {
			o.failCapture(err)
		}
	}
	return true
}

// openCapture makes the outing's capture, under the recording's id, and writes its
// magic. The recording's id was free, so a capture of that name is not ecu's and
// is never written over.
func (o *outing) openCapture(at int64) bool {
	if o.f == nil { // the recording could not start, nor can its capture
		o.capFailed = true
		return false
	}
	f, err := os.OpenFile(o.capPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) //nolint:gosec // the directory is the operator's setting
	if err != nil {
		o.failCapture(err)
		return false
	}
	o.capF, o.capW, o.flushedAt = f, bufio.NewWriterSize(f, captureBuffer), at
	if _, err := o.capW.WriteString(CaptureMagic); err != nil {
		o.failCapture(err)
		return false
	}
	o.logger.Info("capturing outing to", "path", o.capPath)
	return true
}

func (o *outing) failCapture(err error) {
	o.logger.Error("the capture stopped", "path", o.capPath, "err", err)
	o.capFailed = true
}

// endCapture writes what is buffered and closes the capture, synced first, so it is
// whole on the disk once the outing has ended.
func (o *outing) endCapture() {
	if o.capF == nil {
		return
	}
	if !o.capFailed {
		if err := o.capW.Flush(); err != nil {
			o.failCapture(err)
		}
	}
	if err := o.capF.Sync(); err != nil && !o.capFailed {
		o.failCapture(err)
	}
	if err := o.capF.Close(); err != nil && !o.capFailed {
		o.failCapture(err)
	}
	if !o.capFailed {
		o.logger.Info("outing captured", "path", o.capPath)
	}
}

// ErrNotACapture is ReadCapture's error for a file that does not start with
// CaptureMagic.
var ErrNotACapture = errors.New("record: not a capture (no " + fmt.Sprintf("%q", CaptureMagic) + " magic)")

// ReadCapture reads a capture: CaptureMagic, then a record per datagram, its receive
// time (int64, nanoseconds since the Unix epoch, big-endian), its length (uint32,
// big-endian) and its bytes. It is the format's reference reader, for the tests;
// salsa gets the one replays use (paddock#30, paddock#32). A capture cut short,
// as a killed ecu leaves it, gives every whole record before the cut and an error
// wrapping io.ErrUnexpectedEOF.
func ReadCapture(r io.Reader) ([]Datagram, error) {
	br := bufio.NewReader(r)
	magic := make([]byte, len(CaptureMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != CaptureMagic {
		return nil, ErrNotACapture
	}
	var all []Datagram
	var head [captureHeader]byte
	for {
		if _, err := io.ReadFull(br, head[:]); errors.Is(err, io.EOF) {
			return all, nil
		} else if err != nil {
			return all, fmt.Errorf("record: capture cut short after %d datagrams: %w", len(all), err)
		}
		n := binary.BigEndian.Uint32(head[8:])
		if n > maxDatagram {
			return all, fmt.Errorf("record: capture record %d says %d bytes, more than a datagram holds", len(all), n)
		}
		d := Datagram{At: int64(binary.BigEndian.Uint64(head[:8])), Bytes: make([]byte, n)} //nolint:gosec // the bits of an int64
		if _, err := io.ReadFull(br, d.Bytes); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return all, fmt.Errorf("record: capture cut short after %d datagrams: %w", len(all), err)
		}
		all = append(all, d)
	}
}

// ReadCaptureFile is ReadCapture of the file at path.
func ReadCaptureFile(path string) ([]Datagram, error) {
	f, err := os.Open(path) //nolint:gosec // the caller's file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ReadCapture(f)
}
