package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/those-who-run/ecu/internal/live"
	"github.com/those-who-run/ecu/internal/record"
	"github.com/those-who-run/ecu/race"
	"github.com/those-who-run/salsa"
	"github.com/those-who-run/salsa/salsatest"
)

// The recording the drive tests send: a six-lap race at Suzuka East with a stop,
// from salsa's testdata/v1/, as internal/bus's tests use.
const suzuka = "real-nuc-suzuka-pit-stop-2026-10-15"

func recordingPath(tb testing.TB, name string) string {
	tb.Helper()
	// go test puts its own toolchain's go first on PATH.
	out, err := exec.CommandContext(tb.Context(), "go", "list", "-m", "-f", "{{.Dir}}",
		"github.com/those-who-run/salsa").Output()
	if err != nil {
		tb.Fatalf("finding salsa's testdata/v1: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "testdata", "v1", name+".jsonl.gz")
}

// readAll is every packet a source gives until io.EOF.
func readAll(tb testing.TB, src salsa.Source) []salsa.Received {
	tb.Helper()
	defer func() { _ = src.Close() }()
	var all []salsa.Received
	for {
		r, err := src.Next(tb.Context())
		if errors.Is(err, io.EOF) {
			return all
		}
		if err != nil {
			tb.Fatal(err)
		}
		all = append(all, r)
	}
}

func openRecording(tb testing.TB, path string) salsa.Source {
	tb.Helper()
	src, err := salsa.OpenRecording(path)
	if err != nil {
		tb.Fatal(err)
	}
	return src
}

// machine is a race state with fixed ids, so two runs over the same packets give
// the same ids.
func machine() *race.Machine {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'})))
}

// fakePublisher keeps what it is given.
type fakePublisher struct {
	mu     sync.Mutex
	outs   []race.Output
	frames int
}

func (p *fakePublisher) Publish(outs []race.Output) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, o := range outs {
		if o.Frame != nil {
			p.frames++
		}
	}
	p.outs = append(p.outs, outs...)
}

func (p *fakePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frames
}

// events is each event's id (which names its kind), in order, with why a session ended.
func events(outs []race.Output) []string {
	var all []string
	for _, o := range outs {
		if o.Frame == nil {
			all = append(all, o.Event.ID+" "+o.Why.String())
		}
	}
	return all
}

// retimed is a Source whose packets carry the recording's T, by packet id, in
// place of live's, which is the wall clock it received them on. The fake PS5
// sends far faster than GT7 does, so live's T would squeeze the drive into a
// second or so, and nothing that waits on the source's clock, such as the 3 s a
// position holds before race.position (paddock#84), would happen as in the
// replay. The race state reads only T, so with the recording's it sees the drive.
type retimed struct {
	salsa.Source
	t map[int32]float64 // the recording's T of each packet id, unique in it
}

func (r *retimed) Next(ctx context.Context) (salsa.Received, error) {
	p, err := r.Source.Next(ctx)
	if err == nil {
		p.T = r.t[p.PacketID]
	}
	return p, err
}

// A fake PS5 sends a v1 recording's packets, encrypted as GT7 sends them, with a
// few sent again late among them: every packet reaches the publisher once, in
// order, and the events are the replay's of the same packets, event for event.
// The drive is recorded in v1's format, and the recording reads back through
// salsa's recording Source as the packets the race state got, with no trace of
// the PS5's address in it (paddock#266, AC4).
//
// Beside the recording, the capture holds exactly the PS5's datagrams, byte for
// byte, in order, with receive times that never go back, each of which decrypts,
// and none of a second address's sent among them, and no trace of the PS5's
// address either (paddock#343, AC1 and AC4). Under each heartbeat kind the fake
// PS5 gets that kind and sends the same packets as that kind's datagrams, 316
// bytes for B and 344 for "~", and the events are the same (AC2).
func TestALiveDriveReachesThePublisherAndIsRecorded(t *testing.T) {
	in := readAll(t, openRecording(t, recordingPath(t, suzuka)))
	want := &fakePublisher{}
	_ = drive(t.Context(), &slice{all: in}, func(salsa.Received) time.Time { return time.Unix(0, 0) }, machine(), nil, want, nil, nil, nil)
	for _, kind := range []byte{salsa.HeartbeatA, salsa.HeartbeatB, salsa.HeartbeatTilde} {
		t.Run(string(kind), func(t *testing.T) { liveDrive(t, in, want, kind) })
	}
}

// asKind is p as a packet of the heartbeat kind's layout: the A block as it is,
// with a B block and, for "~", a "~" block after it.
func asKind(p salsa.Packet, kind byte) salsa.Packet {
	if kind == salsa.HeartbeatB || kind == salsa.HeartbeatTilde {
		p.B = &salsa.B{Heave: p.Speed / 100, Sway: -1}
	}
	if kind == salsa.HeartbeatTilde {
		p.Tilde = &salsa.Tilde{TildeByte1: 7, TildeVector: [4]float32{1, -1, 2, -2}}
	}
	return p
}

func liveDrive(t *testing.T, in []salsa.Received, want *fakePublisher, kind byte) {

	ps5, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps5.Close() }()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := c.LocalAddr().(*net.UDPAddr)
	_ = c.Close()
	var log bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&lockedWriter{w: &log}, nil))
	dir := t.TempDir()
	rec, err := record.Open(dir, record.Options{}, logger, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	src := live.Open(live.Config{PS5: "127.0.0.1", Heartbeat: time.Hour, HeartbeatPort: ps5.LocalAddr().(*net.UDPAddr).Port,
		Listen: listen.String(), HeartbeatKind: kind, Capture: rec.Capture}, logger)
	// A second address on the network sends too: never captured.
	stranger, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stranger.Close() }()
	got := &fakePublisher{}
	ts := make(map[int32]float64, len(in))
	for _, r := range in {
		ts[r.PacketID] = r.T
	}
	ctx, cancel := context.WithCancel(t.Context())
	driven := make(chan struct{})
	go func() {
		defer close(driven)
		start := src.Start()
		_ = drive(ctx, &retimed{Source: src, t: ts}, func(r salsa.Received) time.Time { return start.Add(time.Duration(r.T * float64(time.Second))) },
			machine(), nil, got, rec, nil, nil)
	}()

	// The PS5 answers the heartbeat: wait for it, and for the listener, which
	// starts beside the heartbeat and may bind after it (a packet sent before then
	// is lost, as GT7's would be), then send in batches the socket's buffer holds.
	buf := make([]byte, 8)
	_ = ps5.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, _, err := ps5.ReadFromUDP(buf); err != nil || n != 1 || buf[0] != kind {
		t.Fatalf("no %q heartbeat: %v, got %q", kind, err, buf[:n])
	}
	for deadline := time.Now().Add(5 * time.Second); !src.Stats().Listening; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("not listening after 5 s")
		}
	}
	waitFor := func(frames int) {
		deadline := time.Now().Add(10 * time.Second)
		for got.count() < frames {
			if time.Now().After(deadline) {
				t.Fatalf("%d frames published, want %d; source %+v", got.count(), frames, src.Stats())
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	var sent [][]byte
	send := func(d []byte) {
		if _, err := ps5.WriteToUDP(d, listen); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, d)
	}
	for i, r := range in {
		send(salsatest.Encode(asKind(r.Packet, kind)))
		if i%10 == 9 { // and one sent again, late
			send(salsatest.Encode(asKind(in[i-3].Packet, kind)))
		}
		if i%100 == 0 {
			if _, err := stranger.WriteToUDP(salsatest.Encode(asKind(r.Packet, kind)), listen); err != nil {
				t.Fatal(err)
			}
		}
		if i%50 == 49 {
			waitFor(i + 1)
		}
	}
	waitFor(len(in))
	cancel()
	<-driven
	_ = src.Close()
	s := rec.Close(t.Context())

	if got.count() != len(in) {
		t.Errorf("%d frames published, want %d", got.count(), len(in))
	}
	// The stranger's datagrams carry ids already seen, so they are late.
	strangers := uint64((len(in) + 99) / 100)
	if st := src.Stats(); st.Late != uint64(len(in)/10)+strangers || st.Packets != uint64(len(in)) || st.Foreign != strangers {
		t.Errorf("source %+v; want %d packets, %d late and %d foreign", st, len(in), uint64(len(in)/10)+strangers, strangers)
	}
	w, g := events(want.outs), events(got.outs)
	// The replay ends its open session as the input ended, live as ecu stopping.
	if len(w) == 0 || len(w) != len(g) || strings.Join(w[:len(w)-1], "\n") != strings.Join(g[:len(g)-1], "\n") {
		t.Fatalf("events differ:\nreplay %v\nlive   %v", w, g)
	}
	if len(g) < 8 {
		t.Errorf("only %d events: %v", len(g), g)
	}

	back := readAll(t, openRecording(t, s.Path))
	if s.Frames != uint64(len(in)) || len(back) != len(in) {
		t.Fatalf("recorded %+v, read back %d; want %d", s, len(back), len(in))
	}
	for i := range in {
		b, r := back[i].Packet, in[i].Packet
		if b.PacketID != r.PacketID || b.CurrentLap != r.CurrentLap || b.FuelLevel != r.FuelLevel || b.Flags != r.Flags {
			t.Fatalf("packet %d read back as %+v, sent %+v", i, b, r)
		}
		if i > 0 && back[i].T < back[i-1].T {
			t.Fatalf("t goes back at %d", i)
		}
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	plain := strings.Join(linesOf(t, s.Path), "\n")
	if strings.Contains(plain, "127.0.0.1") || bytes.Contains(raw, []byte("127.0.0.1")) {
		t.Error("the recording holds the PS5's address")
	}

	capPath := strings.TrimSuffix(s.Path, record.Ext) + record.CaptureExt
	captured, err := record.ReadCaptureFile(capPath)
	if err != nil {
		t.Fatal(err)
	}
	if s.Captured != uint64(len(sent)) || s.CaptureDropped != 0 || len(captured) != len(sent) {
		t.Fatalf("captured %+v, read back %d; want the PS5's %d datagrams", s, len(captured), len(sent))
	}
	for i, c := range captured {
		if !bytes.Equal(c.Bytes, sent[i]) {
			t.Fatalf("datagram %d captured as %x, sent %x", i, c.Bytes, sent[i])
		}
		if i > 0 && c.At < captured[i-1].At {
			t.Fatalf("the receive time goes back at %d", i)
		}
		if plain, err := salsa.Decrypt(c.Bytes); err != nil || plain.Kind.String() != string(kind) {
			t.Fatalf("datagram %d: %v, kind %v", i, err, plain.Kind)
		}
	}
	// The address as text anywhere, and as the four bytes of an IPv4 address in
	// what is not the format's own: a record's header is a time and a length, and
	// a time ending in 0x7f before a length of 0x128 would read as 127.0.0.1.
	raw, err = os.ReadFile(capPath) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("127.0.0.1")) {
		t.Error("the capture holds the PS5's address")
	}
	for i, c := range captured {
		if bytes.Contains(c.Bytes, []byte{127, 0, 0, 1}) {
			t.Errorf("captured datagram %d holds the PS5's address", i)
		}
	}
}

func linesOf(t *testing.T, path string) []string {
	t.Helper()
	var all []string
	for _, r := range readAll(t, openRecording(t, path)) {
		line, _ := record.AppendV1(nil, r.T, &r.Packet)
		all = append(all, string(line))
	}
	return all
}

// lockedWriter serialises writes from the source's and the recorder's goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// slice is a source of packets held in memory.
type slice struct{ all []salsa.Received }

func (s *slice) Next(context.Context) (salsa.Received, error) {
	if len(s.all) == 0 {
		return salsa.Received{}, io.EOF
	}
	r := s.all[0]
	s.all = s.all[1:]
	return r, nil
}

func (s *slice) Close() error { return nil }

// At the end of its input the drive ends the open session as the input ended; when
// its context is done, as ecu stopping.
func TestTheDriveEndsTheSession(t *testing.T) {
	in := readAll(t, openRecording(t, recordingPath(t, suzuka)))
	cut := in[:len(in)/2] // mid-race
	at := func(r salsa.Received) time.Time { return time.Unix(0, int64(math.Round(r.T*1e9))) }

	ended := &fakePublisher{}
	_ = drive(t.Context(), &slice{all: cut}, at, machine(), nil, ended, nil, nil, nil)
	if last := ended.outs[len(ended.outs)-1]; last.Ended == nil || last.Why != race.WhyInputEnded {
		t.Errorf("last output %+v, want the session ended as the input ended", last)
	}

	stopped := &fakePublisher{}
	ctx, cancel := context.WithCancel(t.Context())
	src := &cancelAfter{slice: slice{all: cut}, n: len(cut), cancel: cancel}
	_ = drive(ctx, src, at, machine(), nil, stopped, nil, nil, nil)
	if last := stopped.outs[len(stopped.outs)-1]; last.Ended == nil || last.Why != race.WhyStopped {
		t.Errorf("last output %+v, want the session ended as ecu stopped", last)
	}
}

// cancelAfter gives n packets, then cancels and returns the context's error.
type cancelAfter struct {
	slice
	n      int
	cancel context.CancelFunc
}

func (c *cancelAfter) Next(ctx context.Context) (salsa.Received, error) {
	if c.n == 0 {
		c.cancel()
		return salsa.Received{}, ctx.Err()
	}
	c.n--
	return c.slice.Next(ctx)
}
