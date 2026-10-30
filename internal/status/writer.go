package status

import (
	"bytes"
	"context"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/those-who-run/contracts/ce"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
)

// KV is the part of a KV bucket the writer uses (internal/bus's KV).
type KV interface {
	Put(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
}

// The writer's pace (contracts' live.proto): a value is written on change, at
// most once a second; ecu.status every second while idle, so idle_secs stays
// current, and at least every busyEvery while busy, so a reader can take a value
// far older as ecu gone.
const (
	every     = time.Second
	busyEvery = 5 * time.Second
	// opTimeout bounds one Put or Delete.
	opTimeout = 2 * time.Second
)

// Writer writes a tracker's values to the `live` bucket under the tracker's
// source, ecu.status.<src> and the rest, from its own goroutine (Run).
type Writer struct {
	t      *Tracker
	kv     KV
	logger *slog.Logger

	keys    [4]string
	written [4][]byte          // what each key holds as far as the writer knows; nil for none
	known   [4]bool            // written is known: false until the first write or delete
	at      time.Time          // when ecu.status was last written
	last    *statev1.EcuStatus // ... and what it said, steady (steady)
	failing bool               // the last write failed, and that was logged
}

// The keys, in the order Values has them.
const (
	keyStatus = iota
	keySession
	keyPit
	keyFuel
)

// NewWriter is a writer of t's values to kv.
func NewWriter(t *Tracker, kv KV, logger *slog.Logger) *Writer {
	src := t.cfg.Src
	return &Writer{
		t: t, kv: kv, logger: logger,
		keys: [4]string{"ecu.status." + src, "ecu.session." + src, "ecu.pit." + src, "ecu.fuel." + src},
	}
}

// Run writes once a second until ctx is done, then once more, within grace of
// then, so the last values (the session ended, ecu idle) are what is left. The
// grace starts when ctx is done, not when Run does, so a long run still gets
// all of it. The caller stops the tracker (Tracker.Stop) before ctx is done.
func (w *Writer) Run(ctx context.Context, grace time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	w.Flush(ctx)
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
			w.Flush(final)
			cancel()
			return
		case <-tick.C:
			w.Flush(ctx)
		}
	}
}

// Flush writes the values that are due now: each that changed since it was
// written, and ecu.status while idle or once busyEvery has passed.
func (w *Writer) Flush(ctx context.Context) {
	v := w.t.Values()
	now := v.Status.GetUpdatedAt().AsTime()
	msgs := [4]proto.Message{v.Status, nil, nil, nil}
	if v.Session != nil {
		msgs[keySession], msgs[keyPit], msgs[keyFuel] = v.Session, v.Pit, v.Fuel
	}
	for i, m := range msgs {
		var data []byte
		if m != nil {
			var err error
			if data, err = ce.Marshal(m); err != nil {
				w.logger.ErrorContext(ctx, "the live status cannot be encoded", "key", w.keys[i], "err", err)
				continue
			}
		}
		if i == keyStatus {
			if !w.statusDue(v.Status, now) {
				continue
			}
		} else if w.known[i] && bytes.Equal(data, w.written[i]) {
			continue
		}
		if w.write(ctx, i, data) && i == keyStatus {
			w.at, w.last = now, steady(v.Status)
		}
	}
}

// statusDue says ecu.status is to be written: its first write, idle, busyEvery
// since the last, or a change other than the counters and ages that move on
// every packet.
func (w *Writer) statusDue(st *statev1.EcuStatus, now time.Time) bool {
	if !w.known[keyStatus] || st.GetIdle() || now.Sub(w.at) >= busyEvery {
		return true
	}
	return !proto.Equal(w.last, steady(st))
}

// steady is st without what moves on by itself: the packets, the ages and when it
// was written.
func steady(st *statev1.EcuStatus) *statev1.EcuStatus {
	s := proto.CloneOf(st)
	s.Packets, s.IdleSecs, s.LastPacketAgeSecs, s.UpdatedAt = 0, 0, nil, nil
	return s
}

// write puts data under key i, or deletes the key for nil, and says whether it
// worked. A failure is logged once until a write works again, and the key is
// tried again on the next flush.
func (w *Writer) write(ctx context.Context, i int, data []byte) bool {
	op, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var err error
	if data == nil {
		err = w.kv.Delete(op, w.keys[i])
	} else {
		err = w.kv.Put(op, w.keys[i], data)
	}
	if err != nil {
		if !w.failing {
			w.logger.WarnContext(ctx, "the live status cannot be written: trying again each second", "key", w.keys[i], "err", err)
			w.failing = true
		}
		return false
	}
	if w.failing {
		w.logger.InfoContext(ctx, "the live status is written again", "key", w.keys[i])
		w.failing = false
	}
	w.written[i], w.known[i] = data, true
	return true
}
