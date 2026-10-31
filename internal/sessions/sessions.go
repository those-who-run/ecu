// Package sessions keeps ecu's open session in the `ecu` KV bucket, under
// sessions.open.<src> (paddock#76; contracts' statev1.OpenSession), so that a
// restarted ecu continues it or ends it from there (race.Machine.Restore).
//
// The step loop never waits on it: it hands a Keeper the race state's record
// after the steps that change it (race.Machine.Open) and the session ends it
// should clear, and the Keeper's own goroutine (Run) writes them in order, as
// internal/status's Writer writes the live status. A write that fails is tried
// again each second; the records waiting behind it collapse to the last.
//
// Clearing the key goes with a revision check, a KV compare-and-set: a session
// ended live is cleared only while the key still holds that session, and the end
// of a session an earlier run left (race.Output.Kept) is published only once the
// Keeper has cleared the very value the run read at start. Of two runs that read
// it, one clears it and ends the session; the other cannot clear it and ends
// nothing. EVENTS' duplicate window, on the end's Nats-Msg-Id, is the backup.
package sessions

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/those-who-run/contracts/ce"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/race"
)

// Bucket is the KV bucket the open session is kept in (contracts' subjects.yaml).
const Bucket = "ecu"

// Key is the open session's key for the source src.
func Key(src string) string { return "sessions.open." + src }

// KV is the part of a KV bucket the Keeper uses (internal/bus's KV).
type KV interface {
	Entry(ctx context.Context, key string) ([]byte, uint64, error)
	PutRev(ctx context.Context, key string, value []byte) (uint64, error)
	DeleteIf(ctx context.Context, key string, rev uint64) error
}

// Publisher publishes the end of a session an earlier run left (bus.Publisher).
type Publisher interface {
	Publish(outs []race.Output)
}

// The Keeper's pace: a failed write is tried again every retry, and each call
// is bounded by opTimeout.
const (
	retry     = time.Second
	opTimeout = 2 * time.Second
)

// Load is the record an earlier run left under src's key and its revision, or
// nil with none.
func Load(ctx context.Context, kv KV, src string) (*statev1.OpenSession, uint64, error) {
	data, rev, err := kv.Entry(ctx, Key(src))
	if errors.Is(err, bus.ErrNoKey) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	open := &statev1.OpenSession{}
	if err := ce.Unmarshal(data, open); err != nil {
		return nil, 0, err
	}
	return open, rev, nil
}

// op is one write, in the order the step loop gave them: a record to put, a
// session ended live to clear, or the end of the loaded record's session to
// publish once the record is cleared.
type op struct {
	put   *statev1.OpenSession
	clear string
	end   *race.Output
}

// Keeper writes the open session's record under one source's key, from its own
// goroutine (Run). Its methods for the step loop never wait. A Keeper is safe for
// concurrent use.
type Keeper struct {
	kv     KV
	key    string
	src    string
	pub    Publisher
	logger *slog.Logger
	now    func() time.Time
	replay bool // a missing bucket keeps nothing for the rest of the run

	mu     sync.Mutex
	loaded uint64 // the revision of the record the run read at start, 0 for none
	// decided says the record under the key is the run's to write over: it
	// read it at start (Loaded), or found none, or its own, or ended the one it
	// found before its first write (check).
	decided  bool
	ops      []op
	wake     chan struct{}
	failing  bool // the last write failed, and that was logged
	noBucket bool // the bucket is missing, and that was logged
}

// New is a Keeper of src's record in kv, which the run read at revision loaded
// (Load; 0 for none or not read yet), publishing the end of that record's
// session on pub.
func New(kv KV, src string, loaded uint64, pub Publisher, logger *slog.Logger) *Keeper {
	return &Keeper{
		kv: kv, key: Key(src), src: src, pub: pub, logger: logger, loaded: loaded, decided: loaded > 0,
		wake: make(chan struct{}, 1), now: time.Now,
	}
}

// Replay makes it a replay's Keeper: with the bucket missing, as outside the
// stack, it keeps nothing for the rest of the run and never waits on it. Live's
// tries again each second.
func (k *Keeper) Replay() { k.replay = true }

// Loaded says the run read the record at revision rev, after New (Load; 0 for
// none) and gave it to the race state, which decides it: the revision the end
// of its session clears it at.
func (k *Keeper) Loaded(rev uint64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.loaded, k.decided = rev, true
}

// Put keeps open as the open session's record: the latest of those waiting is
// the one written.
func (k *Keeper) Put(open *statev1.OpenSession) {
	if open == nil {
		return
	}
	open.Source = k.src
	k.add(op{put: open})
}

// Clear clears the record of the session sid, which ended live.
func (k *Keeper) Clear(sid string) { k.add(op{clear: sid}) }

// End publishes o, the end of the session an earlier run left (race.Output.Kept),
// once the record the run read is cleared, and never if another run has changed
// or cleared it since.
func (k *Keeper) End(o race.Output) { k.add(op{end: &o}) }

func (k *Keeper) add(o op) {
	k.mu.Lock()
	// ops[0] may be the one Flush is writing: a record never collapses into it.
	if n := len(k.ops); o.put != nil && n > 1 && k.ops[n-1].put != nil {
		k.ops[n-1] = o
	} else {
		k.ops = append(k.ops, o)
	}
	k.mu.Unlock()
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// Pending is how many writes wait.
func (k *Keeper) Pending() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.ops)
}

// Run writes what the step loop gives until ctx is done, then what is left,
// within grace of then: the grace starts when ctx is done, as the status
// writer's does, so a stop's record gets all of it. The caller stops the step
// loop before ctx is done.
func (k *Keeper) Run(ctx context.Context, grace time.Duration) {
	tick := time.NewTicker(retry)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
			defer cancel()
			for !k.Flush(final) {
				select {
				case <-final.Done():
					k.logger.WarnContext(final, "the open session's record was not all written by the end", "key", k.key, "pending", k.Pending())
					return
				case <-time.After(retry / 4):
				}
			}
			return
		case <-k.wake:
			k.Flush(ctx)
		case <-tick.C:
			k.Flush(ctx)
		}
	}
}

// Flush writes what waits, in order, until one fails, and says whether it is
// all written.
func (k *Keeper) Flush(ctx context.Context) bool {
	for {
		k.mu.Lock()
		if len(k.ops) == 0 {
			k.mu.Unlock()
			return true
		}
		o := k.ops[0]
		k.mu.Unlock()
		var err error
		if !k.noBucket {
			err = k.check(ctx, o)
			if err == nil {
				err = k.do(ctx, o)
			}
		}
		if errors.Is(err, bus.ErrNoBucket) && k.replay {
			// A replay outside the stack keeps nothing for the rest of the run:
			// said once, never waited on.
			if !k.noBucket {
				k.logger.ErrorContext(ctx, "the open session cannot be kept: no bucket", "bucket", Bucket, "err", err)
				k.noBucket = true
			}
			err = nil
		}
		if err != nil {
			if !k.failing {
				k.logger.WarnContext(ctx, "the open session's record cannot be written: trying again each second", "key", k.key, "err", err)
				k.failing = true
			}
			return false
		}
		if k.failing {
			k.logger.InfoContext(ctx, "the open session's record is written again", "key", k.key)
			k.failing = false
		}
		k.mu.Lock()
		k.ops = k.ops[1:]
		k.mu.Unlock()
	}
}

func (k *Keeper) do(ctx context.Context, o op) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	switch {
	case o.put != nil:
		data, err := ce.Marshal(o.put)
		if err != nil {
			k.logger.ErrorContext(ctx, "the open session's record cannot be encoded", "key", k.key, "err", err)
			return nil
		}
		_, err = k.kv.PutRev(ctx, k.key, data)
		return err
	case o.end != nil:
		sid := o.end.Session
		k.mu.Lock()
		loaded := k.loaded
		k.mu.Unlock()
		if loaded == 0 {
			k.logger.ErrorContext(ctx, "a recorded session's end with no record read: not published", "session", sid)
			return nil
		}
		err := k.kv.DeleteIf(ctx, k.key, loaded)
		if errors.Is(err, bus.ErrRevision) {
			k.logger.WarnContext(ctx, "the session the last run left was changed or cleared by another: not ended here",
				"session", sid, "key", k.key)
			return nil
		}
		if err != nil {
			return err
		}
		k.logger.InfoContext(ctx, "the session the last run left is not continued: ended",
			"session", sid, "why", o.end.NotContinued)
		k.pub.Publish([]race.Output{*o.end})
		return nil
	default:
		return k.clear(ctx, o.clear)
	}
}

// check is what goes before the run's first write when it never read the key at
// start (the read failed, or came after the run's first session): a record
// another session left there is that session's only trace, so it ends it from
// the record, once it has cleared it with a revision check, before writing over
// it. A record of the session o is about, or none, is the run's to write.
func (k *Keeper) check(ctx context.Context, o op) error {
	k.mu.Lock()
	decided := k.decided
	k.mu.Unlock()
	if decided {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	data, rev, err := k.kv.Entry(ctx, k.key)
	if errors.Is(err, bus.ErrNoKey) {
		k.decide()
		return nil
	}
	if err != nil {
		return err
	}
	open := &statev1.OpenSession{}
	if err := ce.Unmarshal(data, open); err != nil || open.GetSessionId() == "" || open.GetSessionId() == o.session() {
		k.decide()
		return nil //nolint:nilerr // nothing to end: a record of no session, or this one's
	}
	if err := k.kv.DeleteIf(ctx, k.key, rev); err != nil {
		return err // changed since it was read: read it again
	}
	k.decide()
	end := race.EndRecorded(open, k.now(), "the run could not read it before its first session")
	k.logger.InfoContext(ctx, "the session the last run left is not continued: ended",
		"session", end.Session, "why", end.NotContinued)
	k.pub.Publish([]race.Output{end})
	return nil
}

func (k *Keeper) decide() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.decided = true
}

// session is the id of the session the op is about.
func (o op) session() string {
	switch {
	case o.put != nil:
		return o.put.GetSessionId()
	case o.end != nil:
		return o.end.Session
	}
	return o.clear
}

// clear clears the key while it holds the session sid: a value another session
// or run has put since stays.
func (k *Keeper) clear(ctx context.Context, sid string) error {
	data, rev, err := k.kv.Entry(ctx, k.key)
	if errors.Is(err, bus.ErrNoKey) {
		return nil
	}
	if err != nil {
		return err
	}
	open := &statev1.OpenSession{}
	if err := ce.Unmarshal(data, open); err != nil || open.GetSessionId() != sid {
		return nil //nolint:nilerr // not this session's record: not ours to clear
	}
	err = k.kv.DeleteIf(ctx, k.key, rev)
	if errors.Is(err, bus.ErrRevision) {
		return nil // changed since it was read: no longer this record
	}
	return err
}
