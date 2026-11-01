package bus

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/those-who-run/ecu/race"
)

// frames is a recording's frames as the race state gives them, with no events.
func frames(t *testing.T) []race.Output {
	t.Helper()
	var all []race.Output
	m := newMachine()
	for _, r := range packets(t, suzuka) {
		for _, o := range m.Step(r) {
			if o.Frame != nil {
				all = append(all, o)
			}
		}
	}
	return all
}

// paused is a TCP proxy to target that stops passing the client's bytes on while
// paused is set, so the client's writes fill the socket and wait, as against a
// server that falls behind, and passes them on again once it is cleared.
func paused(t *testing.T, target string, hold *atomic.Bool) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", strings.TrimPrefix(target, "nats://"))
			if err != nil {
				_ = c.Close()
				continue
			}
			t.Cleanup(func() { _ = c.Close(); _ = s.Close() })
			// A small buffer fills soon while paused; much smaller, and delayed
			// acknowledgements slow the stream to a crawl once it flows again.
			_ = c.(*net.TCPConn).SetReadBuffer(64 << 10)
			go func() { _, _ = io.Copy(c, s) }()
			go func() {
				buf := make([]byte, 64<<10)
				for {
					for hold.Load() {
						time.Sleep(time.Millisecond)
					}
					n, err := c.Read(buf)
					if n > 0 {
						if _, err := s.Write(buf[:n]); err != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return "nats://" + ln.Addr().String()
}

// paddock#35: PublishWait never drops a frame for a full hand-over. While the
// server takes nothing, PublishWait waits, with nothing dropped; once it takes
// frames again, every frame published reaches TELEMETRY.
func TestPublishWaitDropsNoFrame(t *testing.T) {
	r := newRig(t, "parity")
	var hold atomic.Bool
	pub, err := Connect(Config{URL: paused(t, r.srv.URL(), &hold), Src: "parity"}, r.logs.logger(), Options{FrameBuffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.WaitConnected(t.Context()); err != nil {
		t.Fatal(err)
	}
	var all []race.Output
	for range 10 { // more than the sockets' buffers hold, under ten run ids, so none is a duplicate
		m := race.New(ulid.Make)
		for _, p := range packets(t, suzuka) {
			for _, o := range m.Step(p) {
				if o.Frame != nil {
					all = append(all, o)
				}
			}
		}
	}
	hold.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pub.PublishWait(t.Context(), all)
	}()
	select {
	case <-done:
		t.Fatal("PublishWait returned while the server took nothing")
	case <-time.After(500 * time.Millisecond):
	}
	if s := pub.Stats(); s.DroppedFrames != 0 {
		t.Fatalf("stats %+v while waiting, want nothing dropped", s)
	}
	hold.Store(false)
	<-done
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if s := pub.Close(ctx); s.Frames != uint64(len(all)) || s.DroppedFrames != 0 {
		t.Fatalf("stats %+v, want all %d frames and none dropped", s, len(all))
	}
	waitFor(t, "TELEMETRY to hold every frame", func() bool {
		st, err := r.js.Stream(t.Context(), "TELEMETRY")
		if err != nil {
			return false
		}
		info, err := st.Info(t.Context())
		return err == nil && info.State.Msgs == uint64(len(all))
	})
}

// PublishWait waits only until its context is done: against a server that stops
// reading, the frames beyond the hand-over are dropped and counted then.
func TestPublishWaitGivesUpWithItsContext(t *testing.T) {
	pub, err := Connect(Config{URL: stalled(t, false), Src: "parity"}, (&logs{}).logger(), Options{FrameBuffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.WaitConnected(t.Context()); err != nil {
		t.Fatal(err)
	}
	one := frames(t)
	var all []race.Output
	for range 10 { // more than the socket's buffers hold
		all = append(all, one...)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	pub.PublishWait(ctx, all)
	if took := time.Since(start); took < 250*time.Millisecond {
		t.Errorf("PublishWait returned after %v, before its context was done", took)
	}
	if s := pub.Stats(); s.DroppedFrames == 0 {
		t.Errorf("stats %+v, want frames dropped once the context was done", s)
	}
	grace, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	pub.Close(grace)
}

// WaitConnected returns the context's error when the server cannot be reached.
func TestWaitConnectedGivesUp(t *testing.T) {
	pub, err := Connect(Config{URL: "nats://127.0.0.1:1", Src: "parity"}, (&logs{}).logger(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := pub.WaitConnected(ctx); err == nil {
		t.Error("connected to nothing")
	}
	if s := pub.Close(ctx); !s.Unflushed {
		t.Errorf("stats %+v, want unflushed: it never connected", s)
	}
	select {
	case <-pub.Gone():
	case <-time.After(5 * time.Second):
		t.Error("a connection never made is not gone 5 s after Close")
	}
}

// paddock#35: a final flush that fails (a server that stops reading after the
// handshake never answers its PING) is reported in Stats, so a replay does not
// exit 0 on it; and Gone follows Closed once the client is done.
func TestAFailedFlushIsReported(t *testing.T) {
	pub, err := Connect(Config{URL: stalled(t, false), Src: "parity"}, (&logs{}).logger(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.WaitConnected(t.Context()); err != nil {
		t.Fatal(err)
	}
	pub.Publish(frames(t)[:3])
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if s := pub.Close(ctx); !s.Unflushed || s.DroppedFrames != 0 {
		t.Errorf("stats %+v, want the flush reported failed", s)
	}
	select {
	case <-pub.Gone():
	case <-time.After(5 * time.Second):
		t.Error("the connection not gone 5 s after Close")
	}
}

// A publisher closed in time on a server that takes everything reports nothing
// unflushed, and its connection goes.
func TestAFlushInTime(t *testing.T) {
	r := newRig(t, "parity")
	r.pub.Publish(frames(t)[:3])
	if s := r.close(t); s.Unflushed || s.Frames != 3 {
		t.Errorf("stats %+v, want 3 frames flushed", s)
	}
	select {
	case <-r.pub.Gone():
	case <-time.After(5 * time.Second):
		t.Error("the connection not gone 5 s after Close")
	}
}
