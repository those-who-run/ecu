package status

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/contracts/ce"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
)

// paddock#88, R1 and R6: the writer writes each key on change and at most once a
// flush, ecu.status every flush while idle and every 5 s while busy, and deletes
// the session's keys when it ends; every key carries the tracker's source.
func TestTheWriterWritesOnChange(t *testing.T) {
	for _, src := range []string{"live", "replay-sugo-x2"} {
		t.Run(src, func(t *testing.T) {
			r := newRig(Config{Src: src})
			kv := newFakeKV()
			var logs bytes.Buffer
			w := NewWriter(r.t, kv, slog.New(slog.NewTextHandler(&logs, nil)))
			key := func(name string) string { return "ecu." + name + "." + src }
			ops := func(want ...string) {
				t.Helper()
				if got := kv.take(); !slices.Equal(got, want) {
					t.Errorf("ops %q, want %q", got, want)
				}
			}

			// The first flush says what is there: ecu idle, no session, whatever
			// an earlier run left.
			w.Flush(t.Context())
			ops("put "+key("status"), "delete "+key("session"), "delete "+key("pit"), "delete "+key("fuel"))
			// Idle: ecu.status every flush, the rest not again.
			r.clock.add(time.Second)
			w.Flush(t.Context())
			ops("put " + key("status"))

			frames := recording(t, gilles)
			mid := midLap2(t, frames)
			for _, f := range frames[:mid] {
				r.step(f)
			}
			w.Flush(t.Context())
			ops("put "+key("status"), "put "+key("session"), "put "+key("pit"), "put "+key("fuel"))
			var st statev1.EcuStatus
			if err := ce.Unmarshal(kv.values[key("status")], &st); err != nil || st.GetSource() != src ||
				st.GetActivity() != statev1.Activity_ACTIVITY_DRIVING {
				t.Fatalf("ecu.status %v, %v", &st, err)
			}

			// Busy, nothing changed but the counters and the fuel: ecu.status
			// waits 5 s.
			r.step(frames[mid])
			w.Flush(t.Context())
			ops("put " + key("fuel"))
			r.clock.add(5 * time.Second)
			w.Flush(t.Context()) // idle now, as the clock moved on with no frame
			ops("put " + key("status"))

			// The session ends, with the recording: its keys go.
			for _, f := range frames[mid+1:] {
				r.step(f)
			}
			outs := r.m.EndOfInput()
			r.t.Observe(r.m.Live(), outs)
			w.Flush(t.Context())
			got := kv.take()
			for _, want := range []string{"delete " + key("session"), "delete " + key("pit"), "delete " + key("fuel")} {
				if !slices.Contains(got, want) {
					t.Errorf("ops %q have no %q", got, want)
				}
			}
			for k := range kv.values {
				if !strings.HasSuffix(k, "."+src) {
					t.Errorf("key %s does not carry the source %s", k, src)
				}
			}
			if _, ok := kv.values[key("status")]; !ok || len(kv.values) != 1 {
				t.Errorf("left %v, want ecu.status alone", kv.values)
			}
		})
	}
}

// A write that fails is logged once and tried again on the next flush.
func TestAFailedWriteIsTriedAgain(t *testing.T) {
	r := newRig(Config{})
	kv := newFakeKV()
	kv.fail = errors.New("bucket live: not found")
	var logs bytes.Buffer
	w := NewWriter(r.t, kv, slog.New(slog.NewTextHandler(&logs, nil)))
	w.Flush(t.Context())
	r.clock.add(time.Second)
	w.Flush(t.Context())
	if n := strings.Count(logs.String(), "cannot be written"); n != 1 {
		t.Errorf("logged %d times: %s", n, logs.String())
	}
	kv.fail = nil
	r.clock.add(time.Second)
	w.Flush(t.Context())
	if got := kv.take(); len(got) != 4 {
		t.Errorf("after the failure: %q, want all four", got)
	}
	if !strings.Contains(logs.String(), "written again") {
		t.Errorf("the recovery is not logged: %s", logs.String())
	}
}

// Run flushes at once, then each second, and once more when ctx is done, with
// the grace counted from then: a run longer than the grace still gets its last
// write.
func TestRunFlushesToTheEnd(t *testing.T) {
	r := newRig(Config{})
	kv := newFakeKV()
	w := NewWriter(r.t, kv, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	const grace = 100 * time.Millisecond
	go func() { defer close(done); w.Run(ctx, grace) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		kv.mu.Lock()
		n := len(kv.ops)
		kv.mu.Unlock()
		if n >= 5 { // the first flush's four, and a tick's ecu.status
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ops %q after 5 s", kv.take())
		}
		time.Sleep(10 * time.Millisecond)
	}
	kv.take()
	time.Sleep(2 * grace) // the run outlasts the grace
	kv.take()
	cancel()
	<-done
	if got := kv.take(); !slices.Equal(got, []string{"put ecu.status.live"}) {
		t.Errorf("the last flush: %q", got)
	}
}
