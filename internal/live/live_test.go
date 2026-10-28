package live

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/those-who-run/salsa"
	"github.com/those-who-run/salsa/salsatest"
)

// logs is a logger's output, safe to read while the source's goroutines write.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logs) logger() *slog.Logger { return slog.New(slog.NewJSONHandler(l, nil)) }

// ps5 is a fake PS5: a UDP socket that takes the heartbeats, and sends datagrams
// to the source as GT7 does.
type ps5 struct {
	t     *testing.T
	conn  *net.UDPConn
	beats chan time.Time
}

func newPS5(t *testing.T) *ps5 {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := &ps5{t: t, conn: conn, beats: make(chan time.Time, 1000)}
	go func() {
		buf := make([]byte, 64)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n == 1 && buf[0] == salsa.HeartbeatA {
				p.beats <- time.Now()
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return p
}

func (p *ps5) port() int { return p.conn.LocalAddr().(*net.UDPAddr).Port }

// send sends a datagram to addr from the PS5's socket.
func (p *ps5) send(addr string, datagram []byte) {
	p.t.Helper()
	to, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.conn.WriteToUDP(datagram, to); err != nil {
		p.t.Fatal(err)
	}
}

// packet is the datagram GT7 would send with this packet id.
func packet(id int32) []byte {
	var p salsa.Packet
	p.PacketID, p.Seed = id, uint32(id)*7919 //nolint:gosec // the test's ids are positive
	return salsatest.Encode(p)
}

// freeAddr is a loopback UDP address nothing holds, kept for the test.
func freeAddr(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := c.LocalAddr().String()
	_ = c.Close()
	return addr
}

// open starts a source heartbeating to the fake PS5 and listening on listen, with
// short intervals.
func open(t *testing.T, p *ps5, listen string, l *logs, adjust func(*Source)) *Source {
	t.Helper()
	s := newSource(Config{PS5: "127.0.0.1", Heartbeat: 50 * time.Millisecond, HeartbeatPort: p.port(), Listen: listen},
		l.logger())
	s.backoff = 20 * time.Millisecond
	if adjust != nil {
		adjust(s)
	}
	s.run()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// eventually waits up to 5 s for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("not %s after 5 s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// next is the next packet's id, failing after 5 s.
func next(t *testing.T, s *Source) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return r.PacketID
}

// The fake PS5 gets an "A" at once and then every interval.
func TestTheHeartbeatGoesEveryInterval(t *testing.T) {
	p := newPS5(t)
	start := time.Now()
	s := open(t, p, freeAddr(t), &logs{}, nil)
	var got []time.Time
	for len(got) < 5 {
		select {
		case b := <-p.beats:
			got = append(got, b)
		case <-time.After(5 * time.Second):
			t.Fatalf("%d heartbeats after 5 s", len(got))
		}
	}
	if d := got[0].Sub(start); d > time.Second {
		t.Errorf("the first heartbeat came after %v", d)
	}
	if d := got[4].Sub(got[0]); d < 150*time.Millisecond || d > 2*time.Second {
		t.Errorf("4 intervals of 50 ms took %v", d)
	}
	// The PS5 can hear a heartbeat before the send returns and is counted.
	eventually(t, "counted", func() bool { return s.Stats().Heartbeats >= 5 })
	if st := s.Stats(); st.HeartbeatErrors != 0 {
		t.Errorf("stats %+v", st)
	}
}

// A failing send is logged once, never stops the heartbeat, and the PS5 hears it
// again at the next interval after the failure ends.
func TestAFailedHeartbeatNeverStopsIt(t *testing.T) {
	p := newPS5(t)
	l := &logs{}
	var failures atomic.Int32
	failures.Store(5)
	s := open(t, p, freeAddr(t), l, func(s *Source) {
		real := s.send
		s.send = func(ctx context.Context) error {
			if failures.Add(-1) >= 0 {
				return errors.New("sendto: network is unreachable")
			}
			return real(ctx)
		}
	})
	select {
	case <-p.beats:
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat after the failures ended")
	}
	if st := s.Stats(); st.HeartbeatErrors != 5 {
		t.Errorf("%d errors counted, want 5", st.HeartbeatErrors)
	}
	eventually(t, "logged as back", func() bool { return strings.Contains(l.String(), "getting out again") })
	if n := strings.Count(l.String(), "the heartbeat to the PS5 failed"); n != 1 {
		t.Errorf("the outage logged %d times, want once: %s", n, l.String())
	}
}

// The real send returns its error rather than hiding it.
func TestASendThatCannotGoIsAnError(t *testing.T) {
	s := newSource(Config{PS5: "127.0.0.1", HeartbeatPort: 9}, (&logs{}).logger())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.sendHeartbeat(ctx); err == nil {
		t.Error("sent with its context done")
	}
}

// Packets are decoded and handed on in order: one older than the last, or the
// same again, is dropped and counted; one 600 ids or more back is a new game
// session and handed on; a datagram that is not GT7's is counted.
func TestPacketsAreHandedOnInOrder(t *testing.T) {
	p := newPS5(t)
	addr := freeAddr(t)
	s := open(t, p, addr, &logs{}, nil)
	eventually(t, "listening", func() bool { return s.Stats().Listening })
	for _, id := range []int32{1000, 1001, 999, 1001, 1002, 401, 400, 402} {
		p.send(addr, packet(id))
	}
	p.send(addr, []byte("not a packet"))
	p.send(addr, packet(403))
	var got []int32
	for range 5 {
		got = append(got, next(t, s))
	}
	want := []int32{1000, 1001, 1002, 401, 402}
	if len(got) != len(want) || got[0] != 1000 || got[1] != 1001 || got[2] != 1002 || got[3] != 401 || got[4] != 402 {
		t.Fatalf("handed on %v, want %v", got, want)
	}
	if id := next(t, s); id != 403 {
		t.Fatalf("then %d, want 403", id)
	}
	if st := s.Stats(); st.Packets != 6 || st.Late != 3 || st.Undecodable != 1 {
		t.Errorf("stats %+v; want 6 packets, 3 late, 1 undecodable", st)
	}
}

// The window's edges: 599 back is late, 600 back is a new session, and any id
// ahead is taken.
func TestTheReorderWindow(t *testing.T) {
	s := newSource(Config{}, (&logs{}).logger())
	for _, c := range []struct {
		id   int32
		want bool
	}{
		{5000, true}, {4401, false}, {5000, false}, {4400, true}, {4401, true}, {9000, true}, {-5, true},
	} {
		if got := s.inOrder(c.id); got != c.want {
			t.Errorf("%d after %d: %v, want %v", c.id, s.last, got, c.want)
		}
	}
}

// T counts up from the source's start, which is its wall-clock time.
func TestTIsSecondsSinceTheStart(t *testing.T) {
	p := newPS5(t)
	addr := freeAddr(t)
	before := time.Now()
	s := open(t, p, addr, &logs{}, nil)
	eventually(t, "listening", func() bool { return s.Stats().Listening })
	p.send(addr, packet(1))
	r, err := s.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(before).Seconds(); r.T <= 0 || r.T > elapsed {
		t.Errorf("T %v, want above 0 and at most %v", r.T, elapsed)
	}
	if s.Start().Before(before) || s.Start().After(time.Now()) {
		t.Errorf("start %v", s.Start())
	}
}

// With the port held by something else, the listener logs it and tries again with
// backoff; once the port is free it binds and packets flow.
func TestTheListenerBindsOnceThePortIsFree(t *testing.T) {
	p := newPS5(t)
	addr := freeAddr(t)
	held, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	l := &logs{}
	s := open(t, p, addr, l, nil)
	eventually(t, "retried twice", func() bool { return s.Stats().Restarts >= 2 })
	if s.Stats().Listening {
		t.Fatal("listening on a port that is held")
	}
	if !strings.Contains(l.String(), `"msg":"the telemetry listener stopped"`) {
		t.Errorf("not logged: %s", l.String())
	}
	_ = held.Close()
	eventually(t, "listening", func() bool { return s.Stats().Listening })
	p.send(addr, packet(7))
	if id := next(t, s); id != 7 {
		t.Errorf("got %d, want 7", id)
	}
}

// When the socket dies, the listener binds again and packets flow again.
func TestTheListenerRebindsWhenItsSocketDies(t *testing.T) {
	p := newPS5(t)
	addr := freeAddr(t)
	s := open(t, p, addr, &logs{}, nil)
	eventually(t, "listening", func() bool { return s.Stats().Listening })
	p.send(addr, packet(1))
	if id := next(t, s); id != 1 {
		t.Fatalf("got %d, want 1", id)
	}
	s.mu.Lock()
	_ = s.conn.Close()
	s.mu.Unlock()
	eventually(t, "restarted", func() bool { return s.Stats().Restarts == 1 && s.Stats().Listening })
	p.send(addr, packet(2))
	if id := next(t, s); id != 2 {
		t.Errorf("got %d, want 2", id)
	}
}

// The backoff doubles from the first wait up to 30 s, and starts afresh once the
// listener has bound.
func TestTheBackoffIsCapped(t *testing.T) {
	d := backoffFirst
	for range 10 {
		d = min(d*2, backoffMax)
	}
	if d != 30*time.Second {
		t.Errorf("backoff reaches %v", d)
	}
}

// When Next falls behind, the oldest packet waiting is dropped and counted.
func TestAFullBufferDropsTheOldest(t *testing.T) {
	s := newSource(Config{}, (&logs{}).logger())
	for id := range int32(Buffer + 1) {
		s.handle(packet(id+1), 0)
	}
	if st := s.Stats(); st.Overflow != 1 || st.Packets != Buffer+1 {
		t.Errorf("stats %+v", st)
	}
	if id := next(t, s); id != 2 {
		t.Errorf("first waiting %d, want 2", id)
	}
}

// Next returns ctx's error when ctx is done, and an error once the source is
// closed, waiting or not.
func TestNextStopsOnContextAndClose(t *testing.T) {
	p := newPS5(t)
	s := open(t, p, freeAddr(t), &logs{}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("done before: %v", err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("done while waiting: %v", err)
	}
	waiting := make(chan error, 1)
	go func() { _, err := s.Next(t.Context()); waiting <- err }()
	time.Sleep(20 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-waiting; !errors.Is(err, errClosed) {
		t.Errorf("closed while waiting: %v", err)
	}
	if _, err := s.Next(t.Context()); !errors.Is(err, errClosed) {
		t.Errorf("after Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }
	cfg, err := ConfigFromEnv(env(map[string]string{"TWR_ECU_PS5_IP": "192.0.2.7"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PS5 != "192.0.2.7" || cfg.Heartbeat != 2*time.Second || cfg.HeartbeatPort != 33739 || cfg.Listen != ":33740" {
		t.Errorf("defaults %+v", cfg)
	}
	cfg, err = ConfigFromEnv(env(map[string]string{"TWR_ECU_PS5_IP": "192.0.2.7", "TWR_ECU_HEARTBEAT_SECS": "0.5"}))
	if err != nil || cfg.Heartbeat != 500*time.Millisecond {
		t.Errorf("0.5 s: %+v, %v", cfg, err)
	}
	for _, c := range []struct {
		vars map[string]string
		want string
	}{
		{nil, "TWR_ECU_PS5_IP: the PS5's IP address is required"},
		{map[string]string{"TWR_ECU_PS5_IP": "ps5.local"}, `"ps5.local" is not an IP address`},
		{map[string]string{"TWR_ECU_PS5_IP": "192.0.2.7", "TWR_ECU_HEARTBEAT_SECS": "0"}, "TWR_ECU_HEARTBEAT_SECS"},
		{map[string]string{"TWR_ECU_PS5_IP": "192.0.2.7", "TWR_ECU_HEARTBEAT_SECS": "two"}, "TWR_ECU_HEARTBEAT_SECS"},
		{map[string]string{"TWR_ECU_PS5_IP": "192.0.2.7", "TWR_ECU_HEARTBEAT_SECS": "NaN"}, "TWR_ECU_HEARTBEAT_SECS"},
	} {
		if _, err := ConfigFromEnv(env(c.vars)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.vars, err, c.want)
		}
	}
}

// Open starts the source, as ecu does.
func TestOpen(t *testing.T) {
	p := newPS5(t)
	s := Open(Config{PS5: "127.0.0.1", Heartbeat: time.Hour, HeartbeatPort: p.port(), Listen: freeAddr(t)}, (&logs{}).logger())
	defer func() { _ = s.Close() }()
	select {
	case <-p.beats:
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat")
	}
}
