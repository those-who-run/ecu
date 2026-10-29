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
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/record"
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
func TestALiveDriveReachesThePublisherAndIsRecorded(t *testing.T) {
	in := readAll(t, openRecording(t, recordingPath(t, suzuka)))
	want := &fakePublisher{}
	_ = drive(t.Context(), &slice{all: in}, func(salsa.Received) time.Time { return time.Unix(0, 0) }, machine(), nil, want, nil)

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
	src := live.Open(live.Config{PS5: "127.0.0.1", Heartbeat: time.Hour, HeartbeatPort: ps5.LocalAddr().(*net.UDPAddr).Port,
		Listen: listen.String()}, logger)
	dir := t.TempDir()
	rec, err := record.Open(dir, logger, time.Now)
	if err != nil {
		t.Fatal(err)
	}
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
			machine(), nil, got, rec)
	}()

	// The PS5 answers the heartbeat: wait for it, and for the listener, which
	// starts beside the heartbeat and may bind after it (a packet sent before then
	// is lost, as GT7's would be), then send in batches the socket's buffer holds.
	buf := make([]byte, 8)
	_ = ps5.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, _, err := ps5.ReadFromUDP(buf); err != nil || n != 1 || buf[0] != salsa.HeartbeatA {
		t.Fatalf("no heartbeat: %v", err)
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
	for i, r := range in {
		if _, err := ps5.WriteToUDP(salsatest.Encode(r.Packet), listen); err != nil {
			t.Fatal(err)
		}
		if i%10 == 9 { // and one sent again, late
			if _, err := ps5.WriteToUDP(salsatest.Encode(in[i-3].Packet), listen); err != nil {
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
	if st := src.Stats(); st.Late != uint64(len(in)/10) || st.Packets != uint64(len(in)) {
		t.Errorf("source %+v; want %d packets and %d late", st, len(in), len(in)/10)
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
	_ = drive(t.Context(), &slice{all: cut}, at, machine(), nil, ended, nil)
	if last := ended.outs[len(ended.outs)-1]; last.Ended == nil || last.Why != race.WhyInputEnded {
		t.Errorf("last output %+v, want the session ended as the input ended", last)
	}

	stopped := &fakePublisher{}
	ctx, cancel := context.WithCancel(t.Context())
	src := &cancelAfter{slice: slice{all: cut}, n: len(cut), cancel: cancel}
	_ = drive(ctx, src, at, machine(), nil, stopped, nil)
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
