package sessions

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/race"
)

// memKV is a KV bucket in memory with JetStream's revisions: each put or delete
// takes the next revision, and DeleteIf checks the key's last.
type memKV struct {
	mu    sync.Mutex
	rev   uint64
	value map[string][]byte
	at    map[string]uint64 // the key's last revision, a delete's included
	fail  error             // every call fails with this while set
	puts  int
}

func newMemKV() *memKV { return &memKV{value: map[string][]byte{}, at: map[string]uint64{}} }

func (k *memKV) Entry(_ context.Context, key string) ([]byte, uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail != nil {
		return nil, 0, k.fail
	}
	v, ok := k.value[key]
	if !ok {
		return nil, 0, bus.ErrNoKey
	}
	return v, k.at[key], nil
}

func (k *memKV) PutRev(_ context.Context, key string, value []byte) (uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail != nil {
		return 0, k.fail
	}
	k.rev++
	k.puts++
	k.value[key], k.at[key] = value, k.rev
	return k.rev, nil
}

func (k *memKV) DeleteIf(_ context.Context, key string, rev uint64) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail != nil {
		return k.fail
	}
	if k.at[key] != rev {
		return bus.ErrRevision
	}
	k.rev++
	delete(k.value, key)
	k.at[key] = k.rev
	return nil
}

func (k *memKV) record(t *testing.T, key string) *statev1.OpenSession {
	t.Helper()
	k.mu.Lock()
	data, ok := k.value[key]
	k.mu.Unlock()
	if !ok {
		return nil
	}
	open := &statev1.OpenSession{}
	if err := ce.Unmarshal(data, open); err != nil {
		t.Fatal(err)
	}
	return open
}

type pubs struct {
	mu   sync.Mutex
	outs []race.Output
}

func (p *pubs) Publish(outs []race.Output) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outs = append(p.outs, outs...)
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func ended(sid string) race.Output {
	return race.Output{
		Session: sid, Kept: true, Why: race.WhyStopped,
		Ended: &eventsv1.SessionEnded{SessionId: sid, Reason: eventsv1.EndReason_END_REASON_ENGINEER_STOPPED},
	}
}

// R1: the latest record waiting behind the first is the one written, under the source's key, with
// the source set; a session ended live clears only its own record.
func TestTheKeeperWritesTheLatestAndClearsItsOwn(t *testing.T) {
	kv := newMemKV()
	k := New(kv, "live", 0, &pubs{}, quiet())
	// The first may be in flight, so it stays; the two behind it collapse.
	k.Put(&statev1.OpenSession{SessionId: "A", Pauses: 1})
	k.Put(&statev1.OpenSession{SessionId: "A", Pauses: 2})
	k.Put(&statev1.OpenSession{SessionId: "A", Pauses: 3})
	if !k.Flush(t.Context()) || kv.puts != 2 {
		t.Fatalf("%d puts, want the last two collapsed to one", kv.puts)
	}
	if r := kv.record(t, "sessions.open.live"); r.GetPauses() != 3 || r.GetSource() != "live" {
		t.Errorf("record %v, want the last, under live", r)
	}
	k.Clear("B") // another session's end: not this record
	k.Flush(t.Context())
	if kv.record(t, "sessions.open.live") == nil {
		t.Fatal("cleared another session's record")
	}
	k.Clear("A")
	k.Flush(t.Context())
	if kv.record(t, "sessions.open.live") != nil {
		t.Error("its own record not cleared")
	}
}

// R3 and AC5 as cut: the end of the session the last run left is published once
// the record read at start is cleared with a revision check, and not when
// another run changed or cleared it first: of two runs, one ends it.
func TestTheRecordedEndIsGuardedByTheRevision(t *testing.T) {
	kv := newMemKV()
	data, _ := ce.Marshal(&statev1.OpenSession{SessionId: "A"})
	rev, _ := kv.PutRev(t.Context(), Key("live"), data)
	open, loaded, err := Load(t.Context(), kv, "live")
	if err != nil || open.GetSessionId() != "A" || loaded != rev {
		t.Fatalf("load %v at %d (%v)", open, loaded, err)
	}
	first, second := &pubs{}, &pubs{}
	one := New(kv, "live", loaded, first, quiet())
	two := New(kv, "live", loaded, second, quiet()) // read the same value
	one.End(ended("A"))
	two.End(ended("A"))
	if !one.Flush(t.Context()) || !two.Flush(t.Context()) {
		t.Fatal("not flushed")
	}
	if len(first.outs) != 1 || len(second.outs) != 0 {
		t.Errorf("the first run published %d ends and the second %d, want 1 and 0", len(first.outs), len(second.outs))
	}
	if kv.record(t, "sessions.open.live") != nil {
		t.Error("the record is still there")
	}
}

// A write that fails stays first and is tried again; what comes behind it waits.
func TestAFailedWriteIsTriedAgain(t *testing.T) {
	kv := newMemKV()
	kv.fail = errors.New("no server")
	p := &pubs{}
	k := New(kv, "live", 0, p, quiet())
	k.Put(&statev1.OpenSession{SessionId: "A"})
	k.Clear("A")
	k.Put(&statev1.OpenSession{SessionId: "B"})
	if k.Flush(t.Context()) || k.Pending() != 3 {
		t.Fatalf("flushed with the server down, %d pending", k.Pending())
	}
	kv.fail = nil
	if !k.Flush(t.Context()) {
		t.Fatal("not flushed once the server is back")
	}
	if r := kv.record(t, "sessions.open.live"); r.GetSessionId() != "B" {
		t.Errorf("record %v, want B's", r)
	}
}

// R1's stop: Run writes what is left once its context is done, within the grace
// from then, however long it ran.
func TestRunWritesTheStopsRecord(t *testing.T) {
	kv := newMemKV()
	k := New(kv, "live", 0, &pubs{}, quiet())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); k.Run(ctx, time.Second) }()
	time.Sleep(50 * time.Millisecond)
	k.Put(&statev1.OpenSession{SessionId: "A", Entries: 3})
	cancel()
	<-done
	if r := kv.record(t, "sessions.open.live"); r.GetEntries() != 3 {
		t.Errorf("record %v after the stop", r)
	}
}

// Load of no record is none, and the keys carry the source.
func TestLoadOfNothing(t *testing.T) {
	open, rev, err := Load(t.Context(), newMemKV(), "replay-x")
	if open != nil || rev != 0 || err != nil {
		t.Errorf("load %v, %d, %v; want none", open, rev, err)
	}
	if Key("parity") != "sessions.open.parity" {
		t.Error(Key("parity"))
	}
}

// gatedKV holds each PutRev until it is let go, to catch a write in flight.
type gatedKV struct {
	*memKV
	started chan string
	gate    chan struct{}
}

func (k *gatedKV) PutRev(ctx context.Context, key string, value []byte) (uint64, error) {
	open := &statev1.OpenSession{}
	_ = ce.Unmarshal(value, open)
	k.started <- open.GetSessionId()
	<-k.gate
	return k.memKV.PutRev(ctx, key, value)
}

// A record given while the one before it is being written is written after it,
// never dropped: a stop's record during a slow lap write is the last word.
func TestARecordDuringAWriteIsNotLost(t *testing.T) {
	kv := &gatedKV{memKV: newMemKV(), started: make(chan string, 4), gate: make(chan struct{})}
	k := New(kv, "live", 0, &pubs{}, quiet())
	k.Loaded(0)
	k.Put(&statev1.OpenSession{SessionId: "A", Entries: 1})
	done := make(chan bool)
	go func() { done <- k.Flush(t.Context()) }()
	if got := <-kv.started; got != "A" {
		t.Fatalf("writing %s, want A", got)
	}
	k.Put(&statev1.OpenSession{SessionId: "A", Entries: 2}) // the stop's, while A's is in flight
	kv.gate <- struct{}{}
	select {
	case <-kv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the stop's record was never written")
	}
	kv.gate <- struct{}{}
	if !<-done {
		t.Fatal("not flushed")
	}
	if r := kv.record(t, Key("live")); r.GetEntries() != 2 || k.Pending() != 0 {
		t.Errorf("record %v, %d pending; want the stop's, entries 2", r, k.Pending())
	}
}

// A run that could not read the key at start ends the session another run left
// there before its first write over it, at the revision it read; a record of its
// own session, or none, it writes over.
func TestTheFirstWriteEndsARecordNeverRead(t *testing.T) {
	kv := newMemKV()
	old := &statev1.OpenSession{SessionId: "OLD", LastSeenAt: nil, Summary: &eventsv1.SessionSummary{LapsCompleted: 3}}
	data, _ := ce.Marshal(old)
	_, _ = kv.PutRev(t.Context(), Key("live"), data)
	p := &pubs{}
	k := New(kv, "live", 0, p, quiet()) // never loaded
	k.Put(&statev1.OpenSession{SessionId: "NEW"})
	if !k.Flush(t.Context()) {
		t.Fatal("not flushed")
	}
	if len(p.outs) != 1 || p.outs[0].Session != "OLD" || !p.outs[0].Kept ||
		p.outs[0].Ended.GetReason() != eventsv1.EndReason_END_REASON_ENGINEER_STOPPED ||
		p.outs[0].Ended.GetSummary().GetLapsCompleted() != 3 {
		t.Fatalf("published %v, want OLD's end from its record", p.outs)
	}
	if r := kv.record(t, Key("live")); r.GetSessionId() != "NEW" {
		t.Errorf("record %v, want NEW's", r)
	}
	// Decided: the next write checks nothing.
	k.Put(&statev1.OpenSession{SessionId: "NEW", Pauses: 1})
	k.Flush(t.Context())
	if len(p.outs) != 1 {
		t.Errorf("%d published, want still 1", len(p.outs))
	}
	// A record of its own session is written over without an end.
	own := New(kv, "live", 0, p, quiet())
	own.Put(&statev1.OpenSession{SessionId: "NEW", Pauses: 2})
	own.Flush(t.Context())
	if len(p.outs) != 1 {
		t.Errorf("its own record ended: %v", p.outs)
	}
}

// noBucketKV is a bucket the bootstrap has not made.
type noBucketKV struct{ *memKV }

func (noBucketKV) Entry(context.Context, string) ([]byte, uint64, error) {
	return nil, 0, bus.ErrNoBucket
}
func (noBucketKV) PutRev(context.Context, string, []byte) (uint64, error) {
	return 0, bus.ErrNoBucket
}

// Live keeps trying while the bucket is missing; a replay keeps nothing and
// never waits.
func TestAMissingBucket(t *testing.T) {
	live := New(noBucketKV{newMemKV()}, "live", 0, &pubs{}, quiet())
	live.Put(&statev1.OpenSession{SessionId: "A"})
	if live.Flush(t.Context()) || live.Pending() != 1 {
		t.Errorf("live dropped its record with no bucket: %d pending", live.Pending())
	}
	replay := New(noBucketKV{newMemKV()}, "replay-x", 0, &pubs{}, quiet())
	replay.Replay()
	replay.Put(&statev1.OpenSession{SessionId: "A"})
	if !replay.Flush(t.Context()) || replay.Pending() != 0 {
		t.Errorf("a replay waited on a missing bucket: %d pending", replay.Pending())
	}
}
