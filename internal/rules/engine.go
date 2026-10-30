package rules

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/pit"
	"github.com/those-who-run/ecu/internal/race"
)

// Engine runs the enabled rules on each frame and turns what they raise into
// callouts. It sits after race.Machine.Step: the machine fills the frame's
// context at v1's place for the rules in Engine._step (after the pits, before the
// timed finish, the track, the kind and the lap store) and adds what v1 told the
// rules later in the step (the lap closed, the finish, the end), so running the
// rules once Step returns is running them where v1 did. Nothing a rule does
// reaches the machine. An Engine is not safe for concurrent use.
type Engine struct {
	rules  []slot
	maxAge float64 // the queue's max age, in seconds
	logger *slog.Logger

	held  []*eventsv1.Callout // raised in the pit lane, published at its release
	stats Stats
}

type slot struct {
	name string
	s    *Settings
	rule Rule
}

// Stats are the engine's counts since it started (paddock#100 exports them).
type Stats struct {
	// Raised counts the callouts rules raised, by rule; Dropped those dropped in
	// the pit lane, never published; Panics each rule's recovered panics.
	Raised, Dropped, Panics map[string]uint64
}

// New is an engine with the enabled rules of cfg that are registered, in Order.
// A rule's settings it cannot read are an error, to stop ecu at start-up.
func New(cfg Config, logger *slog.Logger) (*Engine, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	e := &Engine{maxAge: cfg.QueueMaxAgeSecs, logger: logger, stats: Stats{
		Raised: map[string]uint64{}, Dropped: map[string]uint64{}, Panics: map[string]uint64{},
	}}
	if e.maxAge <= 0 {
		e.maxAge = QueueMaxAgeSecs
	}
	for _, name := range Order {
		build, ok := builders[name]
		s := cfg.Rules[name]
		if s == nil {
			s, _ = settings(name, nil)
		}
		if !ok || !s.Enabled {
			continue
		}
		r := build(s)
		if err := s.Err(); err != nil {
			return nil, err
		}
		e.rules = append(e.rules, slot{name, s, r})
	}
	return e, nil
}

// Rules are the names of the rules the engine runs, in order.
func (e *Engine) Rules() []string {
	names := make([]string, len(e.rules))
	for i, r := range e.rules {
		names[i] = r.name
	}
	return names
}

// Stats are the engine's counts so far; the maps are copies.
func (e *Engine) Stats() Stats {
	cp := func(m map[string]uint64) map[string]uint64 {
		out := make(map[string]uint64, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return Stats{Raised: cp(e.stats.Raised), Dropped: cp(e.stats.Dropped), Panics: cp(e.stats.Panics)}
}

// Gauges are the values the rules show between callouts (R8), by name.
func (e *Engine) Gauges() map[string]float64 {
	out := map[string]float64{}
	for _, r := range e.rules {
		if g, ok := r.rule.(Gauge); ok {
			if name, v, ok := g.Gauge(); ok {
				out[name] = v
			}
		}
	}
	return out
}

// Step runs the rules on the frame ctx, the machine's Context after its Step,
// and returns the callouts to publish, as outputs: the ones held in the pit lane
// and released on this frame first, then this frame's, rule by rule in Order and
// each rule's in the order it raised them, as v1's queue took them.
//
// The pit lane (contracts' PitLane): from the frame of the entry to the frame of
// the exit, a callout raised is published at once when its rule's pit_lane is
// speak, held when hold, and dropped, never published, when drop, as v1's
// Engine._quiet, on the flag frame and after it too. The held ones are published
// at the exit or at an entry withdrawn (the frame's pit.exited or pit.teleport,
// v1's _apply_pits), first among the frame's callouts, or, once the race has
// finished in the lane, after the frame's rules have run, so a hold callout
// raised on the flag frame goes out on it (v1's `if self.finished and
// self.pit_held`). Released, a callout is as raised then: created_at is the
// release and expires_at the release plus its life, with no hold_until, and at
// and id stay those of the frame it was raised on (v1's _release). A session
// that ends without a finish with callouts held never publishes them, nor does a
// start: v1 kept them until the next start, which cleared them.
//
// A frame the machine ignored (ctx.Ignored: after a session ended paused) runs
// no rule, as v1 never stepped it. A paused frame, flagged or frozen, is not
// Racing, so no rule evaluates on it, and the frame after a pause compares
// against the last driving frame with Bridged PHASE_PAUSED (paddock#77).
func (e *Engine) Step(ctx *race.Context) []race.Output {
	if ctx.Ignored {
		return nil
	}
	if ctx.Started {
		e.forget()
		for _, r := range e.rules {
			e.reset(r)
		}
	}
	var out []race.Output
	if len(e.held) > 0 && (ctx.PitChange == pit.Exited || ctx.PitChange == pit.Withdrawn) {
		out = e.release(ctx)
	}
	inLane := ctx.Pits.InPitLane
	for _, r := range e.rules {
		for i, c := range e.evaluate(r, ctx) {
			e.stats.Raised[r.name]++
			msg := e.callout(r, ctx, c, i+1)
			if inLane {
				switch r.s.PitLane {
				case eventsv1.PitLane_PIT_LANE_DROP:
					e.stats.Dropped[r.name]++
					continue
				case eventsv1.PitLane_PIT_LANE_SPEAK:
				default:
					e.held = append(e.held, msg)
					continue
				}
			}
			out = append(out, output(msg))
		}
	}
	if len(e.held) > 0 && ctx.Finished {
		out = append(e.release(ctx), out...)
	}
	if ctx.EndedEarly != nil {
		e.forget()
	}
	return out
}

// forget drops the callouts held in the lane, unpublished.
func (e *Engine) forget() {
	for _, c := range e.held {
		e.stats.Dropped[c.GetRule()]++
	}
	e.held = nil
}

// release publishes the held callouts as raised on this frame.
func (e *Engine) release(ctx *race.Context) []race.Output {
	out := make([]race.Output, 0, len(e.held))
	for _, c := range e.held {
		life := c.GetExpiresAt().AsTime().Sub(c.GetCreatedAt().AsTime())
		c.CreatedAt = timestamppb.New(ctx.Time)
		c.ExpiresAt = timestamppb.New(ctx.Time.Add(life))
		c.HoldUntil = nil
		out = append(out, output(c))
	}
	e.held = nil
	return out
}

func (e *Engine) reset(r slot) {
	defer e.recover(r, "reset")
	r.rule.Reset()
}

func (e *Engine) evaluate(r slot, ctx *race.Context) (out []Callout) {
	defer e.recover(r, "evaluate")
	return r.rule.Evaluate(ctx)
}

// recover is R4: a rule that panics is logged with its name and counted, and
// gives nothing on this frame; the frame and the other rules carry on.
func (e *Engine) recover(r slot, in string) {
	if v := recover(); v != nil {
		e.stats.Panics[r.name]++
		e.logger.Error("rule failed", "rule", r.name, "in", in, "panic", fmt.Sprint(v))
	}
}

// callout is the contracts' Callout for c, which rule r raised as the n-th of the
// frame ctx. Its id is <run>:callout:<seq>:<rule>:<n>, from the frame it was
// raised on, so the same frames and run id give the same ids (R7); the publisher
// puts <src>: in front, as for an event.
func (e *Engine) callout(r slot, ctx *race.Context, c Callout, n int) *eventsv1.Callout {
	priority := c.Priority
	if priority == 0 {
		priority = r.s.Priority
	}
	created := ctx.Time
	if c.AsOf != nil {
		created = c.AsOf.Time
	}
	maxAge := r.s.MaxAgeSecs
	if maxAge <= 0 {
		maxAge = e.maxAge
	}
	seq := strconv.FormatUint(uint64(ctx.Cur.Seq), 10)
	msg := &eventsv1.Callout{
		SessionId: ctx.Session,
		Id:        ctx.Run + ":callout:" + seq + ":" + r.name + ":" + strconv.Itoa(n),
		Rule:      r.name,
		Kind:      c.Kind,
		Slots:     c.Slots,
		Priority:  priority,
		At:        ctx.Cur.Seq,
		RunId:     ctx.Run,
		CreatedAt: timestamppb.New(created),
		ExpiresAt: timestamppb.New(created.Add(secs(maxAge))),
		PitLane:   r.s.PitLane,
	}
	if r.s.HoldSecs > 0 {
		msg.HoldUntil = timestamppb.New(created.Add(secs(r.s.HoldSecs)))
	}
	return msg
}

func output(c *eventsv1.Callout) race.Output {
	return race.Output{
		Event:   ce.Event{ID: c.GetId(), Source: race.Source, Time: c.GetCreatedAt().AsTime()},
		Session: c.GetSessionId(),
		Callout: c,
	}
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
