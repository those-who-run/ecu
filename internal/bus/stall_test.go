package bus

import (
	"bufio"
	"context"
	"net"
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
			pub, err := Connect(Config{URL: url, Src: "parity"}, (&logs{}).logger(), Options{ReconnectWait: 20 * time.Millisecond})
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
			s := pub.Close(ctx)
			if slowest > 100*time.Millisecond {
				t.Errorf("the slowest of %d steps took %v", steps, slowest)
			}
			// The goroutine was held: frames were dropped, and no event was
			// acknowledged.
			if s.DroppedFrames == 0 || s.Events != 0 || s.DroppedEvents == 0 {
				t.Errorf("stats %+v: the server never stalled the goroutine", s)
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
	const sid = "01JBUS00000000000000000000"
	lap := func(n int32) []race.Output {
		return []race.Output{{
			Event:   ce.Event{ID: "x", Source: race.Source, Time: epoch},
			Session: sid,
			Lap:     &eventsv1.LapCompleted{SessionId: sid, Lap: n},
		}}
	}
	r.nc.Close()
	for n := int32(1); n <= 300; n++ {
		switch n {
		case 50:
			r.srv.stop()
		case 150:
			r.srv.start() // the publisher finds it again within 20 ms, while laps keep coming
		}
		r.pub.Publish(lap(n))
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
	got := r.read(t, "EVENTS", 300)
	if len(got) != 300 {
		t.Fatalf("EVENTS holds %d events, want 300", len(got))
	}
	for i, msg := range got {
		var l eventsv1.LapCompleted
		if _, err := ce.Decode(header(msg), msg.Data(), &l); err != nil {
			t.Fatal(err)
		}
		if l.GetLap() != int32(i+1) { //nolint:gosec // a lap number
			t.Fatalf("event %d is lap %d: out of order", i+1, l.GetLap())
		}
	}
}
