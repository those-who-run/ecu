package bus

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
)

// stalled is a TCP listener that plays a NATS server gone wrong: each connection
// it accepts is held open, and either never answered (silent), or answered up to
// the client's first PING and then never read again, so the client's writes fill
// the socket and wait.
func stalled(t *testing.T, silent bool) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			if silent {
				continue
			}
			_ = c.(*net.TCPConn).SetReadBuffer(4096)
			go handshake(c)
		}
	}()
	return "nats://" + ln.Addr().String()
}

// handshake answers a client's CONNECT and first PING as a server would, then reads
// nothing more.
func handshake(c net.Conn) {
	_, _ = c.Write([]byte(`INFO {"server_id":"stalled","version":"2.15.1","proto":1,"headers":true,` +
		`"max_payload":1048576}` + "\r\n"))
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if strings.HasPrefix(line, "PING") {
			_, _ = c.Write([]byte("PONG\r\n"))
			return
		}
	}
}

// B1 of the review of ecu#12, and R6: a server that stops reading, or one that
// never answers, holds the publishing goroutine, never a step. The client's own
// writes wait up to 2 s on a full socket, and its connect attempts 2 s on a silent
// server; Publish waits on neither.
func TestAStalledServerNeverHoldsAStep(t *testing.T) {
	all := packets(t, suzuka)
	for _, c := range []struct {
		name   string
		silent bool
	}{{"stops reading", false}, {"never answers", true}} {
		t.Run(c.name, func(t *testing.T) {
			url := stalled(t, c.silent)
			l := &logs{}
			pub, err := Connect(Config{URL: url, Src: "parity"}, l.logger(), Options{ReconnectWait: 20 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if !c.silent {
				waitFor(t, "the handshake", pub.nc.IsConnected)
			}
			var slowest time.Duration
			var steps int
			// 20 passes of the recording, about 25 MB of frames: far more than the
			// socket holds.
			for range 20 {
				m := newMachine()
				for _, r := range all {
					out := m.Step(r)
					start := time.Now()
					pub.Publish(out)
					slowest = max(slowest, time.Since(start))
					steps++
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			pub.Close(ctx)
			returned := time.Now()
			// N8 of the review of ecu#12: Close keeps to its context, even while the
			// goroutine is held by a write.
			if took := returned.Sub(start); took > 300*time.Millisecond {
				t.Errorf("Close took %v with 100 ms to go", took)
			}
			// N11: the final totals come once the write gives up, which Close's
			// cutting the connection makes at once: within milliseconds, and the
			// 100 ms bound pins the cut. Without it, each write to the stalled socket
			// waits up to 2 s and the next one as long again, often past the 3 s
			// cmd/ecu waits after Close.
			select {
			case <-pub.Closed():
				if took := time.Since(returned); took > 100*time.Millisecond {
					t.Errorf("the final totals came %v after Close returned", took)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no final totals within cmd/ecu's 3 s wait")
			}
			if slowest > 100*time.Millisecond {
				t.Errorf("the slowest of %d steps took %v", steps, slowest)
			}
			// The goroutine was held: frames were dropped, and no event was
			// acknowledged. Its totals are final once it has given up its write.
			s := pub.Stats()
			if s.DroppedFrames == 0 || s.Events != 0 || s.DroppedEvents == 0 {
				t.Errorf("stats %+v: the server never stalled the goroutine", s)
			}
			// N10: the totals logged last are final and are these, logged once,
			// even when Close gave up first and logged totals not yet final.
			var published []string
			for line := range strings.Lines(l.String()) {
				if strings.Contains(line, `"msg":"published"`) {
					published = append(published, line)
				}
			}
			want := `"dropped_frames":` + strconv.FormatUint(s.DroppedFrames, 10) +
				`,"dropped_lo_frames":` + strconv.FormatUint(s.DroppedLoFrames, 10) +
				`,"dropped_events":` + strconv.FormatUint(s.DroppedEvents, 10) + `,"final":true}`
			if n := len(published); n == 0 || !strings.Contains(published[n-1], want) ||
				strings.Count(l.String(), `"final":true`) != 1 {
				t.Errorf("published lines %q, want the last one to end %s", published, want)
			}
			if len(published) == 2 && !strings.Contains(published[0], `"final":false`) {
				t.Errorf("the first of two published lines is %q, want it not final", published[0])
			}
		})
	}
}

// B2 of the review of ecu#12: events stepped while the connection comes back never
// overtake the ones waiting for it. Laps 1 to 300 are published a millisecond
// apart while the server is stopped and started again; EVENTS holds each once, in
// order.
func TestEventsStayInOrderAcrossAReconnect(t *testing.T) {
	r := newRig(t, "parity")
	all := laps(300)
	r.nc.Close()
	for n := int32(1); n <= 300; n++ {
		switch n {
		case 50:
			r.srv.stop()
		case 150:
			r.srv.start() // the publisher finds it again within 20 ms, while laps keep coming
		}
		r.pub.Publish(all[n-1 : n])
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if s := r.pub.Close(ctx); s.DroppedEvents != 0 {
		t.Fatalf("stats %+v", s)
	}
	nc, err := nats.Connect(r.srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	r.nc = nc
	if r.js, err = jetstream.New(nc); err != nil {
		t.Fatal(err)
	}
	readLaps(t, r, 300)
}

// laps is lap events 1 to n of one session, as the race state gives them.
func laps(n int32) []race.Output {
	const sid = "01JBUS00000000000000000000"
	var out []race.Output
	for i := int32(1); i <= n; i++ {
		out = append(out, race.Output{
			Event:   ce.Event{ID: sid + ":lap.completed:" + strconv.Itoa(int(i)), Source: race.Source, Time: epoch},
			Session: sid,
			Lap:     &eventsv1.LapCompleted{SessionId: sid, Lap: i},
		})
	}
	return out
}

// readLaps checks that EVENTS holds laps 1 to n, each once, in order.
func readLaps(t *testing.T, r *rig, n int) {
	t.Helper()
	got := r.read(t, "EVENTS", n)
	if len(got) != n {
		t.Fatalf("EVENTS holds %d events, want %d", len(got), n)
	}
	for i, msg := range got {
		var l eventsv1.LapCompleted
		if _, err := ce.Decode(header(msg), msg.Data(), &l); err != nil {
			t.Fatal(err)
		}
		if l.GetLap() != int32(i+1) { //nolint:gosec // a lap number
			t.Fatalf("event %d is lap %d, want lap %d", i+1, l.GetLap(), i+1)
		}
	}
}

// B3 of the review of ecu#12: an event whose publish fails while the connection is
// up (here EVENTS is missing, and a core subscriber answers in its place, so no
// acknowledgement comes) is kept and sent again, ahead of the rest, until EVENTS
// takes it. The warning says why, once.
func TestAnEventNotAcknowledgedIsSentAgain(t *testing.T) {
	r := newRig(t, "parity")
	r.pub.Close(t.Context())
	var err error
	r.pub, err = Connect(Config{URL: r.srv.URL(), Src: "parity"}, r.logs.logger(),
		Options{ReconnectWait: 20 * time.Millisecond, ackTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.js.DeleteStream(t.Context(), "EVENTS"); err != nil {
		t.Fatal(err)
	}
	sub, err := r.nc.SubscribeSync("twr.v1.parity.evt.>")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	r.pub.Publish(laps(5))
	// Lap 1 reaches the subscriber three times: sent, not acknowledged within the
	// ack timeout, sent again, not acknowledged again, and sent a third time. Two
	// missing acknowledgements, and one warning.
	for i := range 3 {
		msg, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if id := msg.Header.Get(ce.IDHeader); id != "parity:01JBUS00000000000000000000:lap.completed:1" {
			t.Fatalf("copy %d is %s, want lap 1 again", i+1, id)
		}
	}
	if s := r.pub.Stats(); s.Events != 0 || s.DroppedEvents != 0 {
		t.Fatalf("stats %+v before EVENTS is back", s)
	}
	addStream(t, r.js, "events")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if s := r.pub.Close(ctx); s.Events != 5 || s.DroppedEvents != 0 {
		t.Fatalf("stats %+v, want 5 events acknowledged", s)
	}
	readLaps(t, r, 5)
	if n := strings.Count(r.logs.String(), "EVENTS did not acknowledge an event"); n != 1 {
		t.Errorf("the warning was logged %d times over two missing acknowledgements, want once", n)
	}
}

// N5 of the review of ecu#12: with the server down, events beyond the queue and
// the hand-over (QueueSize each) are dropped, counted and logged, and no step waits;
// the ones kept are stored in order once the server is back.
func TestAFullEventQueueDropsAndCounts(t *testing.T) {
	r := newRig(t, "parity")
	r.pub.Close(t.Context())
	var err error
	r.pub, err = Connect(Config{URL: r.srv.URL(), Src: "parity"}, r.logs.logger(),
		Options{QueueSize: 4, ReconnectWait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r.nc.Close()
	r.srv.stop()
	waitFor(t, "lost connection", func() bool { return !r.pub.nc.IsConnected() })
	var slowest time.Duration
	for i, o := range laps(20) {
		start := time.Now()
		r.pub.Publish([]race.Output{o})
		slowest = max(slowest, time.Since(start))
		if i < 4 { // the goroutine takes the first four into its queue
			waitFor(t, "the hand-over to empty", func() bool { return len(r.pub.events) == 0 })
		}
	}
	if s := r.pub.Stats(); s.DroppedEvents != 12 || slowest > 100*time.Millisecond {
		t.Errorf("stats %+v, the slowest step %v; want 12 dropped (20 less 4 queued and 4 handed over)", s, slowest)
	}
	if n := strings.Count(r.logs.String(), "event dropped: the queue is full"); n != 12 {
		t.Errorf("%d drops logged, want 12", n)
	}
	r.srv.start()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if s := r.pub.Close(ctx); s.Events != 8 {
		t.Fatalf("stats %+v, want the 8 kept acknowledged", s)
	}
	nc, err := nats.Connect(r.srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	r.nc = nc
	if r.js, err = jetstream.New(nc); err != nil {
		t.Fatal(err)
	}
	readLaps(t, r, 8)
}
