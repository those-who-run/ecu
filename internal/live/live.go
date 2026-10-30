// Package live is ecu's live frame source (paddock#106): it keeps the PS5 sending
// with a heartbeat of its own, listens for GT7's datagrams, decodes them with salsa
// and gives them, in order, through salsa's Source interface, so the race state
// and the publisher cannot tell a live drive from a replayed one.
//
// It ports the lessons of v1's LiveTelemetry (src/those_who_run/telemetry.py at
// v1.0.0), not its code:
//
//   - The heartbeat is ours. gt-telem's sent one every 10 s with no error
//     handling, so one failed send (the host losing its route while the network
//     was pulled) ended it for good, silently, and GT7 stops sending without one.
//     Ours goes every interval (2 s), from a fresh socket each time, and a failed
//     send is logged once per outage and never stops it.
//   - The listener is ours too, and when it cannot bind (port 33740 busy) or its
//     socket dies, it is logged and bound again with exponential backoff, from
//     1 s up to 30 s.
//   - Order matters (a lap counter going backwards reads as a restart), so a
//     packet whose id is not newer than the last one handed on is dropped, unless
//     it is 600 ids or more back: that is a new game session.
//   - No broadcast discovery: it does not work from a pod. The PS5's address is
//     required.
package live

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/those-who-run/salsa"
)

// ReorderWindow is how far back, in packet ids, a packet is taken as late and
// dropped; one further back is a new game session (v1's REORDER_WINDOW).
const ReorderWindow = 600

// The listener's backoff: the first wait before binding again, doubled after each
// failure up to the most, and back to the first once it has bound.
const (
	backoffFirst = time.Second
	backoffMax   = 30 * time.Second
)

// Buffer is how many packets wait for Next. When it is full the oldest is dropped,
// as v1 did: the next packet carries the same state, and the race state only reads
// transitions.
const Buffer = 600

// readBuffer is the socket receive buffer the listener asks for: 1 MiB.
const readBuffer = 1 << 20

// sendTimeout bounds one heartbeat send, its name lookup included.
const sendTimeout = time.Second

// DefaultHeartbeat is the heartbeat interval when TWR_ECU_HEARTBEAT_SECS is unset,
// and minHeartbeat and maxHeartbeat the least and most it may be set to.
const (
	DefaultHeartbeat = 2 * time.Second
	minHeartbeat     = 10 * time.Millisecond
	maxHeartbeat     = time.Hour
)

// DefaultTelemetryTimeout is the time with no packet that counts as the feed lost
// when TWR_ECU_TELEMETRY_TIMEOUT_SECS is unset (v1's GT7_TELEMETRY_TIMEOUT_SECS,
// paddock#87), and maxTelemetryTimeout the most it may be set to.
const (
	DefaultTelemetryTimeout = 3 * time.Second
	maxTelemetryTimeout     = time.Hour
)

// Config is the live source's configuration.
type Config struct {
	// PS5 is the PS5's IP address: TWR_ECU_PS5_IP, required.
	PS5 string
	// Heartbeat is how often the heartbeat goes: TWR_ECU_HEARTBEAT_SECS, in
	// seconds, DefaultHeartbeat when unset.
	Heartbeat time.Duration
	// HeartbeatPort is the PS5's port the heartbeat goes to: salsa.HeartbeatPort
	// unless a test sets another.
	HeartbeatPort int
	// Listen is the address the datagrams are read on: ":33740"
	// (salsa.TelemetryPort) unless a test sets another.
	Listen string
	// TelemetryTimeout is the time with no packet that counts as the feed lost
	// (race.Machine.WatchTelemetry): TWR_ECU_TELEMETRY_TIMEOUT_SECS, in seconds,
	// DefaultTelemetryTimeout when unset.
	TelemetryTimeout time.Duration
}

// ErrNoPS5 is ConfigFromEnv's error when TWR_ECU_PS5_IP is unset or empty: there
// is nothing to listen to.
var ErrNoPS5 = errors.New("TWR_ECU_PS5_IP: the PS5's IP address is required for live mode " +
	"(there is no broadcast discovery)")

// ConfigFromEnv reads the configuration through getenv. TWR_ECU_PS5_IP must be an
// IP address (ErrNoPS5 when it is unset), TWR_ECU_HEARTBEAT_SECS, when set, a
// number of seconds from 0.01 to 3,600, and TWR_ECU_TELEMETRY_TIMEOUT_SECS, when
// set, a number of seconds above 0 and at most 3,600.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		PS5:              strings.TrimSpace(getenv("TWR_ECU_PS5_IP")),
		Heartbeat:        DefaultHeartbeat,
		HeartbeatPort:    salsa.HeartbeatPort,
		Listen:           ":" + strconv.Itoa(salsa.TelemetryPort),
		TelemetryTimeout: DefaultTelemetryTimeout,
	}
	if cfg.PS5 == "" {
		return Config{}, ErrNoPS5
	}
	if net.ParseIP(cfg.PS5) == nil {
		return Config{}, fmt.Errorf("TWR_ECU_PS5_IP: %q is not an IP address", cfg.PS5)
	}
	if v := getenv("TWR_ECU_HEARTBEAT_SECS"); v != "" {
		secs, err := strconv.ParseFloat(v, 64)
		// The bounds keep the interval a positive Duration (time.NewTicker panics on
		// anything else), and the heartbeat one GT7 can live with.
		if err != nil || math.IsNaN(secs) || secs < minHeartbeat.Seconds() || secs > maxHeartbeat.Seconds() {
			return Config{}, fmt.Errorf("TWR_ECU_HEARTBEAT_SECS: %q is not a number of seconds from %v to %v",
				v, minHeartbeat.Seconds(), maxHeartbeat.Seconds())
		}
		cfg.Heartbeat = time.Duration(secs * float64(time.Second))
	}
	if v := getenv("TWR_ECU_TELEMETRY_TIMEOUT_SECS"); v != "" {
		secs, err := strconv.ParseFloat(v, 64)
		d := time.Duration(secs * float64(time.Second))
		if err != nil || math.IsNaN(secs) || d <= 0 || secs > maxTelemetryTimeout.Seconds() {
			return Config{}, fmt.Errorf("TWR_ECU_TELEMETRY_TIMEOUT_SECS: %q is not a number of seconds above 0 and at most %v",
				v, maxTelemetryTimeout.Seconds())
		}
		cfg.TelemetryTimeout = d
	}
	return cfg, nil
}

// Stats is what the source has seen.
type Stats struct {
	// Packets is the packets handed on, Late those dropped for being out of order
	// or repeated, Undecodable those salsa could not decrypt or parse, and
	// Overflow those dropped because Next was not keeping up.
	Packets, Late, Undecodable, Overflow uint64
	// Heartbeats is the heartbeats sent, HeartbeatErrors the sends that failed.
	Heartbeats, HeartbeatErrors uint64
	// Restarts is how many times the listener was started again.
	Restarts uint64
	// Listening is whether the listener holds its port now.
	Listening bool
}

// Source is the live source, a salsa.Source. Received.T is the seconds since the
// source started, on the monotonic clock: Start gives the wall-clock time of 0.
type Source struct {
	cfg    Config
	logger *slog.Logger
	start  time.Time

	// send sends one heartbeat; the tests replace it to make sends fail.
	send func(ctx context.Context) error
	// backoff is the listener's first backoff and maxBackoff its most; the tests
	// shorten them.
	backoff, maxBackoff time.Duration

	packets chan salsa.Received
	stop    context.CancelFunc
	ctx     context.Context
	wg      sync.WaitGroup

	mu   sync.Mutex
	conn net.PacketConn // the listener's socket while it is bound

	// The reading goroutine's own: the last packet id handed on.
	last    int32
	hasLast bool

	counters struct {
		packets, late, undecodable, overflow, heartbeats, heartbeatErrors, restarts atomic.Uint64
		listening                                                                   atomic.Bool
	}
	closed atomic.Bool
}

// Open starts the heartbeat and the listener. It does not wait for the listener to
// bind: a port that is busy is logged and tried again, and never stops ecu.
func Open(cfg Config, logger *slog.Logger) *Source {
	s := newSource(cfg, logger)
	s.run()
	return s
}

// newSource is a source not yet started, for the tests to adjust.
func newSource(cfg Config, logger *slog.Logger) *Source {
	ctx, stop := context.WithCancel(context.Background())
	s := &Source{
		cfg: cfg, logger: logger, start: time.Now(),
		backoff: backoffFirst, maxBackoff: backoffMax,
		packets: make(chan salsa.Received, Buffer),
		ctx:     ctx, stop: stop,
	}
	s.send = s.sendHeartbeat
	return s
}

func (s *Source) run() {
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.heartbeat() }()
	go func() { defer s.wg.Done(); s.listen() }()
	s.logger.Info("listening to the PS5", "ps5", s.cfg.PS5, "heartbeat_port", s.cfg.HeartbeatPort,
		"listen", s.cfg.Listen, "heartbeat_secs", s.cfg.Heartbeat.Seconds())
}

// Start is the wall-clock time of T = 0.
func (s *Source) Start() time.Time { return s.start }

var errClosed = errors.New("live: source closed")

// Next returns the next packet, waiting for one until ctx is done (ctx.Err()) or
// the source is closed.
func (s *Source) Next(ctx context.Context) (salsa.Received, error) {
	if s.closed.Load() {
		return salsa.Received{}, errClosed
	}
	if err := ctx.Err(); err != nil {
		return salsa.Received{}, err
	}
	select {
	case r := <-s.packets:
		return r, nil
	case <-ctx.Done():
		return salsa.Received{}, ctx.Err()
	case <-s.ctx.Done():
		return salsa.Received{}, errClosed
	}
}

// Close stops the heartbeat and the listener, waits for both, and logs the totals.
func (s *Source) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.stop()
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close() // unblocks the read
	}
	s.mu.Unlock()
	s.wg.Wait()
	st := s.Stats()
	s.logger.Info("stopped listening to the PS5", "packets", st.Packets, "late", st.Late,
		"undecodable", st.Undecodable, "overflow", st.Overflow, "heartbeats", st.Heartbeats,
		"heartbeat_errors", st.HeartbeatErrors, "restarts", st.Restarts)
	return nil
}

// Stats is what the source has seen so far.
func (s *Source) Stats() Stats {
	c := &s.counters
	return Stats{
		Packets: c.packets.Load(), Late: c.late.Load(), Undecodable: c.undecodable.Load(),
		Overflow: c.overflow.Load(), Heartbeats: c.heartbeats.Load(),
		HeartbeatErrors: c.heartbeatErrors.Load(), Restarts: c.restarts.Load(),
		Listening: c.listening.Load(),
	}
}

// heartbeat sends the heartbeat at once and then every interval until the source
// closes. A failed send is logged at the start of an outage and when it ends, and
// the next goes at the next interval regardless.
func (s *Source) heartbeat() {
	tick := time.NewTicker(s.cfg.Heartbeat)
	defer tick.Stop()
	failing := false
	for {
		err := s.send(s.ctx)
		switch {
		case s.ctx.Err() != nil:
			return
		case err != nil:
			s.counters.heartbeatErrors.Add(1)
			if !failing {
				s.logger.Warn("the heartbeat to the PS5 failed, retrying", "ps5", s.cfg.PS5, "err", err)
			}
			failing = true
		default:
			s.counters.heartbeats.Add(1)
			if failing {
				s.logger.Info("the heartbeat to the PS5 is getting out again", "ps5", s.cfg.PS5)
			}
			failing = false
		}
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// sendHeartbeat sends one "A" from a socket of its own, as v1 did: nothing a failed
// send leaves on a socket carries over to the next.
func (s *Source) sendHeartbeat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", net.JoinHostPort(s.cfg.PS5, strconv.Itoa(s.cfg.HeartbeatPort)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	}
	_, err = conn.Write([]byte{salsa.HeartbeatA})
	return err
}

// listen binds the port and reads it until the source closes, binding again with
// backoff whenever it cannot bind or its socket dies.
func (s *Source) listen() {
	delay := s.backoff
	var lc net.ListenConfig
	for {
		conn, err := lc.ListenPacket(s.ctx, "udp", s.cfg.Listen)
		if err == nil {
			s.mu.Lock()
			if s.ctx.Err() != nil { // closed while binding
				s.mu.Unlock()
				_ = conn.Close()
				return
			}
			s.conn = conn
			s.mu.Unlock()
			// Room for a few seconds of packets while the step loop is held up (a
			// pause of the collector, a slow disk): the kernel drops what does not
			// fit. Best effort, as the kernel caps it.
			if uc, ok := conn.(*net.UDPConn); ok {
				_ = uc.SetReadBuffer(readBuffer)
			}
			s.counters.listening.Store(true)
			s.logger.Info("listening for telemetry", "addr", conn.LocalAddr().String())
			delay = s.backoff // it did bind: back off afresh
			err = s.read(conn)
			s.counters.listening.Store(false)
			s.mu.Lock()
			s.conn = nil
			s.mu.Unlock()
			_ = conn.Close()
		}
		if s.ctx.Err() != nil {
			return
		}
		s.logger.Error("the telemetry listener stopped", "addr", s.cfg.Listen, "err", err,
			"retry_in_secs", delay.Seconds())
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, s.maxBackoff)
		s.counters.restarts.Add(1)
	}
}

// read reads datagrams until the socket fails, and returns why.
func (s *Source) read(conn net.PacketConn) error {
	// GT7's longest packet is 368 bytes; a longer datagram is cut and fails to
	// decode, as it should.
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		s.handle(buf[:n], time.Since(s.start).Seconds())
	}
}

// handle decodes a datagram and hands it on, unless it is late.
func (s *Source) handle(datagram []byte, t float64) {
	plain, err := salsa.Decrypt(datagram)
	var p salsa.Packet
	if err == nil {
		p, err = salsa.Parse(plain)
	}
	if err != nil {
		if s.counters.undecodable.Add(1) == 1 {
			s.logger.Warn("a datagram that is not a GT7 packet was dropped (only the first is logged)", "err", err)
		}
		return
	}
	if !s.inOrder(p.PacketID) {
		s.counters.late.Add(1)
		return
	}
	s.counters.packets.Add(1)
	r := salsa.Received{T: t, Packet: p}
	for {
		select {
		case s.packets <- r:
			return
		default:
		}
		// Full: drop the oldest. Next may take it first, and then the send goes.
		select {
		case <-s.packets:
			s.counters.overflow.Add(1)
		default:
		}
	}
}

// inOrder reports whether a packet with this id is handed on, and if so makes it
// the last: one that is not newer than the last is dropped, unless it is
// ReorderWindow or more back, a new game session.
func (s *Source) inOrder(id int32) bool {
	if s.hasLast {
		back := int64(s.last) - int64(id)
		if back >= 0 && back < ReorderWindow {
			return false
		}
	}
	s.last, s.hasLast = id, true
	return true
}
