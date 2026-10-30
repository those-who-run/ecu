package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/sessions"
)

// The open session across a restart (paddock#76): the drive keeps the race
// state's record of the open session in the `ecu` bucket, sessions.open.<src>,
// through a sessions.Keeper, at the session's start, each lap, pit change, pause
// and resume, and at a stop, and clears it when the session ends. A graceful stop
// ends no session: the next run, given the record at start (race.Machine.Restore),
// continues it or ends it, END_REASON_ENGINEER_STOPPED.

// continueWindowEnv is the continue window, in seconds (v1's
// GT7_CONTINUE_WINDOW_SECS).
const continueWindowEnv = "TWR_ECU_CONTINUE_WINDOW_SECS"

// defaultContinueWindow is v1's CONTINUE_WINDOW_SECS.
const defaultContinueWindow = 600 * time.Second

// continueWindow is TWR_ECU_CONTINUE_WINDOW_SECS: 600 s when unset, 0 never
// continues; a value that is not a number of seconds, 0 or more, is an error.
func continueWindow(getenv func(string) string) (time.Duration, error) {
	v := getenv(continueWindowEnv)
	if v == "" {
		return defaultContinueWindow, nil
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(secs) || math.IsInf(secs, 0) || secs < 0 || secs > 1e6 {
		return 0, fmt.Errorf("%s: %q is not a number of seconds, 0 or more", continueWindowEnv, v)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// loadTimeout bounds the read of the record at start, retried each second
// until then: NATS may still be connecting.
var loadTimeout = 10 * time.Second

// loadKept reads the record an earlier run left under src, tried each second
// for loadTimeout. ok is false when it could not be read (no bucket, or NATS out
// of reach), which is logged; with none there, open is nil and ok true.
func loadKept(ctx context.Context, kv sessions.KV, src string, logger *slog.Logger) (open *statev1.OpenSession, rev uint64, ok bool) {
	wait, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	for {
		open, rev, err := sessions.Load(wait, kv, src)
		if err == nil {
			if open != nil {
				logger.InfoContext(ctx, "an open session from the last run", "session", open.GetSessionId(),
					"last_seen_at", open.GetLastSeenAt().AsTime(), "run", open.GetRunId())
			}
			return open, rev, true
		}
		if errors.Is(err, bus.ErrNoBucket) {
			logger.WarnContext(ctx, "no bucket for the open session", "bucket", sessions.Bucket)
			return nil, 0, false
		}
		select {
		case <-wait.Done():
			logger.WarnContext(ctx, "the last run's open session cannot be read yet", "key", sessions.Key(src), "err", err)
			return nil, 0, false
		case <-time.After(time.Second):
		}
	}
}

// loaded is a record read late, and its revision.
type loaded struct {
	open *statev1.OpenSession
	rev  uint64
}

// lateEvery is how often live tries again to read a record it could not read
// at start.
var lateEvery = 5 * time.Second

// loadLate keeps trying to read the record live could not read at start, and
// hands it to the drive once it has: the drive gives it to the race state while
// the run has opened no session (race.Machine.Restore). After that the Keeper
// ends it before its first write (sessions.Keeper).
func loadLate(ctx context.Context, kv sessions.KV, src string, logger *slog.Logger) <-chan loaded {
	out := make(chan loaded, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(lateEvery):
			}
			open, rev, err := sessions.Load(ctx, kv, src)
			if err == nil {
				logger.InfoContext(ctx, "the last run's open session read", "session", open.GetSessionId())
				out <- loaded{open, rev}
				return
			}
		}
	}()
	return out
}

// keeping is what the drive keeps of the open session: the Keeper, the clock of
// the continue window's timer (time.Now live; nil for a replay, whose frames
// carry their own clock and are checked on each step), and whether the end of
// the input leaves the session open, as a stop does (`ecu replay --keep-open`).
type keeping struct {
	k        *sessions.Keeper
	now      func() time.Time
	keepOpen bool
	// late is the record read after the drive began (loadLate), nil for none to
	// wait for, and window the continue window it is given with.
	late   <-chan loaded
	window time.Duration
}

// restoreLate gives the race state a record read late, if one has come: while
// the run has opened no session it decides it, and the Keeper clears it at its
// revision; otherwise the Keeper ends it before its first write.
func (kp *keeping) restoreLate(m *race.Machine) {
	if kp == nil || kp.late == nil {
		return
	}
	select {
	case l := <-kp.late:
		kp.late = nil
		if m.Restore(l.open, kp.window) {
			kp.k.Loaded(l.rev)
		}
	default:
	}
}

// kept is a publisher that hands the Keeper what a step changed: the end of a
// session an earlier run left, which the Keeper publishes once it has cleared
// that record, a session ended live to clear, and the record after any other
// change it is kept on. It copies the running summary only on those steps.
type kept struct {
	publisher
	m *race.Machine
	k *sessions.Keeper
}

func (p kept) Publish(outs []race.Output) {
	save, held := false, false
	for _, o := range outs {
		switch {
		case o.Kept:
			p.k.End(o)
			held = true
		case o.Ended != nil:
			p.k.Clear(o.Session)
		case o.Started != nil || o.Continued != nil || o.Lap != nil || o.PitEntered != nil || o.PitExited != nil ||
			o.PitTeleport != nil || o.Paused != nil || o.Resumed != nil:
			save = true
		}
	}
	if held {
		rest := make([]race.Output, 0, len(outs))
		for _, o := range outs {
			if !o.Kept {
				rest = append(rest, o)
			}
		}
		outs = rest
	}
	p.publisher.Publish(outs)
	if save {
		p.k.Put(p.m.Open())
	}
}
