package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/sessions"
	"github.com/those-who-run/salsa"
)

// The restart pair (paddock#76), from internal/track's copy of v1's recordings.
const (
	beforeRestart = "real-nuc-suzuka-before-restart-2026-10-18"
	joinedMidRace = "real-nuc-suzuka-joined-mid-race-2026-10-18"
)

// revKV is a KV bucket in memory with revisions, as sessions.KV.
type revKV struct {
	mu    sync.Mutex
	rev   uint64
	value map[string][]byte
	at    map[string]uint64
}

func newRevKV() *revKV { return &revKV{value: map[string][]byte{}, at: map[string]uint64{}} }

func (k *revKV) Entry(_ context.Context, key string) ([]byte, uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.value[key]
	if !ok {
		return nil, 0, bus.ErrNoKey
	}
	return v, k.at[key], nil
}

func (k *revKV) PutRev(_ context.Context, key string, value []byte) (uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rev++
	k.value[key], k.at[key] = append([]byte(nil), value...), k.rev
	return k.rev, nil
}

func (k *revKV) DeleteIf(_ context.Context, key string, rev uint64) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.at[key] != rev {
		return bus.ErrRevision
	}
	k.rev++
	delete(k.value, key)
	k.at[key] = k.rev
	return nil
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// stopAt is a run of the drive over in that stops, as SIGTERM stops ecu, once
// every packet is taken, keeping the open session in kv; or, with end, runs to
// the end of the input.
func keptDrive(t *testing.T, kv *revKV, in []salsa.Received, start time.Time, seed byte, end bool) *fakePublisher {
	t.Helper()
	open, rev, err := sessions.Load(t.Context(), kv, "live")
	if err != nil {
		t.Fatal(err)
	}
	pub := &fakePublisher{}
	k := sessions.New(kv, "live", rev, pub, discard())
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u', seed})))
	m.Restore(open, defaultContinueWindow)
	t0 := in[0].T
	at := func(r salsa.Received) time.Time {
		return start.Add(time.Duration(math.Round((r.T - t0) * float64(time.Second))))
	}
	var src salsa.Source = &slice{all: in}
	ctx := t.Context()
	if !end {
		c, cancel := context.WithCancel(t.Context())
		ctx, src = c, &cancelAfter{slice: slice{all: in}, n: len(in), cancel: cancel}
	}
	_ = drive(ctx, src, at, m, nil, pub, nil, nil, &keeping{k: k})
	if !k.Flush(t.Context()) {
		t.Fatal("the keeper did not flush")
	}
	return pub
}

// R1, R2 and R3 through the drive: a stop keeps the session in the KV and ends
// none; the next run, joining 8.7 s after the last frame, continues it under the
// same id and ends it once, at its real end, then clears the key.
func TestAStopKeepsTheSessionForTheNextRun(t *testing.T) {
	kv := newRevKV()
	first := keptDrive(t, kv, readAll(t, openRecording(t, trackRecording(t, beforeRestart))), epochOf(0), 1, false)
	var sid string
	for _, o := range first.outs {
		if o.Started != nil {
			sid = o.Started.GetSessionId()
		}
		if o.Ended != nil {
			t.Fatalf("the stop ended the session (%v): R2 says none", o.Why)
		}
	}
	open, _, err := sessions.Load(t.Context(), kv, "live")
	if err != nil || open.GetSessionId() != sid || open.GetSource() != "live" || open.GetLap().GetLap() != 7 {
		t.Fatalf("the record %v (%v), want session %s on lap 7", open, err, sid)
	}
	from := open.GetLastSeenAt().AsTime().Add(8700 * time.Millisecond)
	second := keptDrive(t, kv, readAll(t, openRecording(t, trackRecording(t, joinedMidRace))), from, 2, true)
	var continued, ended int
	for _, o := range second.outs {
		switch {
		case o.Started != nil:
			t.Errorf("the second run started %s", o.Started.GetSessionId())
		case o.Continued != nil:
			continued++
		case o.Ended != nil:
			ended++
			if o.Session != sid || o.Why != race.WhyInputEnded {
				t.Errorf("ended %s (%v), want %s at the end of the input", o.Session, o.Why, sid)
			}
		}
	}
	if continued != 1 || ended != 1 {
		t.Errorf("%d continued and %d ended, want one each", continued, ended)
	}
	if open, _, _ := sessions.Load(t.Context(), kv, "live"); open != nil {
		t.Errorf("the record %v is still there after the end", open)
	}
}

func epochOf(secs int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, secs, 0, time.UTC)
}

// blocked is a source that gives nothing until its context is done.
type blocked struct{}

func (blocked) Next(ctx context.Context) (salsa.Received, error) {
	<-ctx.Done()
	return salsa.Received{}, ctx.Err()
}
func (blocked) Close() error { return nil }

// R5 and AC3: with no frames coming, the once-a-second timer ends the recorded
// session once the window has passed, from the record, engineer_stopped, and
// clears it.
func TestTheTimerEndsTheRecordedSessionWithNoFrames(t *testing.T) {
	kv := newRevKV()
	keptDrive(t, kv, readAll(t, openRecording(t, trackRecording(t, beforeRestart))), time.Now().Add(-40*time.Minute), 1, false) // the race lasts 24 min
	open, rev, err := sessions.Load(t.Context(), kv, "live")
	if err != nil || open == nil {
		t.Fatalf("no record: %v", err)
	}
	pub := &fakePublisher{}
	k := sessions.New(kv, "live", rev, pub, discard())
	m := machine()
	m.Restore(open, defaultContinueWindow)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); k.Run(ctx, time.Second) }()
	driven := make(chan struct{})
	go func() {
		defer close(driven)
		_ = drive(ctx, blocked{}, nil, m, nil, pub, nil, nil, &keeping{k: k, now: time.Now})
	}()
	deadline := time.Now().Add(5 * time.Second)
	var end *race.Output
	for end == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		pub.mu.Lock()
		for i := range pub.outs {
			if pub.outs[i].Ended != nil {
				end = &pub.outs[i]
			}
		}
		pub.mu.Unlock()
	}
	cancel()
	<-driven
	<-done
	if end == nil {
		t.Fatal("the recorded session never ended")
	}
	if end.Session != open.GetSessionId() || end.Ended.GetReason() != eventsv1.EndReason_END_REASON_ENGINEER_STOPPED {
		t.Errorf("ended %s (%v), want %s engineer_stopped", end.Session, end.Ended.GetReason(), open.GetSessionId())
	}
	if again, _, _ := sessions.Load(t.Context(), kv, "live"); again != nil {
		t.Error("the record is still there")
	}
}

// R7: the continue window's setting.
func TestTheContinueWindow(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 600 * time.Second, "0": 0, "90": 90 * time.Second, "1.5": 1500 * time.Millisecond} {
		got, err := continueWindow(func(string) string { return v })
		if err != nil || got != want {
			t.Errorf("%q: %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"-1", "x", "NaN", "Inf"} {
		if _, err := continueWindow(func(string) string { return v }); err == nil {
			t.Errorf("%q: no error", v)
		}
	}
}

// The record written while the session runs is current at each lap: its last
// lap moves on.
func TestTheRecordIsKeptAtEachLap(t *testing.T) {
	kv := newRevKV()
	in := readAll(t, openRecording(t, trackRecording(t, beforeRestart)))
	pub := &fakePublisher{}
	k := sessions.New(kv, "live", 0, pub, discard())
	m := machine()
	at := func(r salsa.Received) time.Time { return epochOf(0).Add(time.Duration(r.T * float64(time.Second))) }
	var laps []int32
	rec := recorder{kv: kv, k: k, laps: &laps, t: t}
	_ = drive(t.Context(), &slice{all: in}, at, m, nil, rec, nil, nil, &keeping{k: k, keepOpen: true})
	if len(laps) < 6 || laps[len(laps)-1] != 7 {
		t.Errorf("the record's laps after each publish %v, want up to 7", laps)
	}
}

// recorder flushes the keeper after each publish and notes the record's lap.
type recorder struct {
	kv   *revKV
	k    *sessions.Keeper
	laps *[]int32
	t    *testing.T
}

func (r recorder) Publish([]race.Output) {
	if !r.k.Flush(r.t.Context()) {
		return
	}
	open, _, _ := sessions.Load(r.t.Context(), r.kv, "live")
	if l := open.GetLap().GetLap(); l > 0 && (len(*r.laps) == 0 || (*r.laps)[len(*r.laps)-1] != l) {
		*r.laps = append(*r.laps, l)
	}
}

// laterDrive is the second run of the pair with the record not read at start:
// late, when set, hands it over as loadLate would, before the first packet.
func laterDrive(t *testing.T, kv *revKV, late bool) *fakePublisher {
	t.Helper()
	open, rev, err := sessions.Load(t.Context(), kv, "live")
	if err != nil || open == nil {
		t.Fatalf("no record: %v", err)
	}
	pub := &fakePublisher{}
	k := sessions.New(kv, "live", 0, pub, discard())
	kp := &keeping{k: k, window: defaultContinueWindow}
	if late {
		ch := make(chan loaded, 1)
		ch <- loaded{open, rev}
		kp.late = ch
	}
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u', 3})))
	in := readAll(t, openRecording(t, trackRecording(t, joinedMidRace)))
	start := open.GetLastSeenAt().AsTime().Add(8700 * time.Millisecond)
	t0 := in[0].T
	at := func(r salsa.Received) time.Time {
		return start.Add(time.Duration(math.Round((r.T - t0) * float64(time.Second))))
	}
	_ = drive(t.Context(), &slice{all: in}, at, m, nil, pub, nil, nil, kp)
	if !k.Flush(t.Context()) {
		t.Fatal("the keeper did not flush")
	}
	return pub
}

// A record read late, before the run's first session, is still continued.
func TestARecordReadLateIsContinued(t *testing.T) {
	kv := newRevKV()
	keptDrive(t, kv, readAll(t, openRecording(t, trackRecording(t, beforeRestart))), epochOf(0), 1, false)
	var continued int
	for _, o := range laterDrive(t, kv, true).outs {
		if o.Continued != nil {
			continued++
		}
		if o.Started != nil {
			t.Error("a new session started")
		}
	}
	if continued != 1 {
		t.Errorf("%d continued, want 1", continued)
	}
}

// A record never read is not left behind: the run's first write ends its
// session from it, engineer_stopped, then keeps the run's own.
func TestARecordNeverReadIsEndedAtTheFirstWrite(t *testing.T) {
	kv := newRevKV()
	keptDrive(t, kv, readAll(t, openRecording(t, trackRecording(t, beforeRestart))), epochOf(0), 1, false)
	old, _, _ := sessions.Load(t.Context(), kv, "live")
	var oldEnded, started int
	for _, o := range laterDrive(t, kv, false).outs {
		if o.Ended != nil && o.Session == old.GetSessionId() &&
			o.Ended.GetReason() == eventsv1.EndReason_END_REASON_ENGINEER_STOPPED {
			oldEnded++
		}
		if o.Started != nil {
			started++
		}
	}
	if oldEnded != 1 || started != 1 {
		t.Errorf("the old session ended %d times and %d started; want 1 and 1", oldEnded, started)
	}
}

// errNotYet is the client's error for a read before its first connection.
var errNotYet = errors.New("bucket ecu: nats: outbound buffer limit exceeded")

// notYet is a sessions.KV on a connection not yet made: each read fails at
// once, as the client's does before its first connection (bus.Connect).
type notYet struct {
	*revKV
	up    atomic.Bool
	reads atomic.Int32
}

func (k *notYet) Entry(ctx context.Context, key string) ([]byte, uint64, error) {
	k.reads.Add(1)
	if !k.up.Load() {
		return nil, 0, errNotYet
	}
	return k.revKV.Entry(ctx, key)
}

// The record is read once NATS is first reached, not tried before and again a
// second later (2026-10-31's start, the status's twin).
func TestTheRecordIsReadOnceNATSIsReached(t *testing.T) {
	kv := &notYet{revKV: newRevKV()}
	connected := func(context.Context) error {
		time.Sleep(50 * time.Millisecond)
		kv.up.Store(true)
		return nil
	}
	start := time.Now()
	open, _, ok := loadKept(t.Context(), kv, connected, "live", slog.New(slog.DiscardHandler))
	if !ok || open != nil {
		t.Fatalf("ok %v, open %v; want ok and no record", ok, open)
	}
	if n := kv.reads.Load(); n != 1 {
		t.Errorf("%d reads, want 1: none before the connection", n)
	}
	if took := time.Since(start); took >= time.Second {
		t.Errorf("read after %v, want no retry's second", took)
	}
}
