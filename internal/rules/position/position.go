// Package position is v1's position rule (rules/position.py at v1.0.0,
// paddock#94): the callout for places gained and lost. GT7 updates the position
// live and places change in bursts, so a change is held settle_secs and the latest
// position is called once things calm down, no sooner than min_interval_secs after
// the last call. The first start_grace_secs of a race are quiet apart from
// settling the grid slot (nine calls in the first minute at Suzuka East,
// 2026-10-15), and the first call after the grace compares with it. Only with a
// field (v1's has_field): never in a time trial, where a ghost can count as a
// second car.
//
// It reads the raw position on every frame, as v1 did, not race.position
// (race.Context.Settled): that event has only the 3 s settle, and while this rule
// waits out its grace or its interval its settle timer runs on through every
// place that differs from the last call, so it can call a place race.position
// never publishes (P18 on frame 234 of SUGO's restarts, P13 on frame 823 of
// SUGO's two stops; paddock#84).
//
// Quiet in the pit lane, where places drop one by one while the car stands in the
// box (P5 to P11 at Circuit Gilles Villeneuve, 2026-10-17); the last position
// called is kept, so one call after the exit gives the net change. GT7 gives no
// warning of the entry, so each call is held a second with pit_lane drop (v1's
// hold_secs 1 and pit_lane drop, the engine's defaults for the rule), and radio
// drops it if the entry follows in that second (SUGO, 2026-10-18,
// those-who-run#244).
package position

import (
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
)

// Name is the rule's name, its key in the rules file.
const Name = "position"

// The rule's kinds, v1's phrase keys (contracts v0.2.3's Callout): the
// situational pool where v1 had one, which radio falls back from to the general
// key when its script lacks it.
const (
	KindGained       = "template_gained"
	KindGainedLead   = "template_gained_lead"   // into first
	KindGainedPodium = "template_gained_podium" // into second or third
	KindLost         = "template_lost"
	KindLostLead     = "template_lost_lead" // out of first
)

// Config is the rule's own thresholds, in seconds, with v1's keys.
type Config struct {
	SettleSecs      float64 `yaml:"settle_secs"`
	MinIntervalSecs float64 `yaml:"min_interval_secs"`
	StartGraceSecs  float64 `yaml:"start_grace_secs"`
}

// DefaultConfig is v1's defaults (config.DEFAULT_RULES, rules.example.yaml).
var DefaultConfig = Config{SettleSecs: 3, MinIntervalSecs: 10, StartGraceSecs: 15}

var _ = rules.Register(Name, func(s *rules.Settings) rules.Rule {
	cfg := DefaultConfig
	s.Decode(&cfg)
	return &rule{cfg: cfg}
})

// frame is what the rule reads of a frame.
type frame struct {
	t         float64 // v1's state.t
	inRace    bool    // v1's state.in_race
	position  int32   // GT7's live position, 0 for none
	hasField  bool    // v1's has_field, by the kind before the frame
	inPitLane bool    // the pit status after the frame's pit step
}

// call is a call the rule raises.
type call struct {
	kind     string
	position int32 // the place called
	previous int32 // the last place called, or the grid slot
}

// rule is PositionRule.
type rule struct {
	cfg          Config
	announced    int32 // the last position called, or the grid slot; 0 for none
	started      bool
	startedAt    float64 // the first frame with a field and a position
	pending      bool    // a new position is settling since pendingSince
	pendingSince float64
	spoken       bool
	lastSpokenAt float64
}

// Reset forgets the race, as a start does.
func (r *rule) Reset() { *r = rule{cfg: r.cfg} }

// Evaluate raises the call PositionRule.evaluate raised on the frame, if any.
func (r *rule) Evaluate(ctx *race.Context) []rules.Callout {
	if !ctx.Racing {
		return nil
	}
	c, ok := r.observe(frame{
		t:         ctx.Cur.T,
		inRace:    ctx.Cur.InRace(),
		position:  ctx.Cur.RacePosition(),
		hasField:  ctx.HasField(),
		inPitLane: ctx.Pits.InPitLane,
	})
	if !ok {
		return nil
	}
	return []rules.Callout{{
		Kind:  c.kind,
		Slots: &eventsv1.CalloutSlots{Position: &c.position, PreviousPosition: &c.previous},
	}}
}

// observe is PositionRule.evaluate on f.
func (r *rule) observe(f frame) (call, bool) {
	if !f.inRace || !f.hasField || f.position <= 0 {
		return call{}, false
	}
	pos := f.position
	if !r.started {
		r.startedAt, r.started = f.t, true
	}
	if r.announced == 0 || f.t-r.startedAt < r.cfg.SettleSecs {
		// The grid slot takes a moment to settle: qualifying reported P20 on the
		// first driving frame and P1 on the next (2026-10-14). Follow it quietly.
		r.announced = pos
		return call{}, false
	}
	if f.t-r.startedAt < r.cfg.StartGraceSecs {
		return call{}, false
	}
	if f.inPitLane {
		r.pending = false // settle again from the pit exit
		return call{}, false
	}
	if pos == r.announced {
		r.pending = false
		return call{}, false
	}
	if !r.pending {
		r.pendingSince, r.pending = f.t, true
	}
	if f.t-r.pendingSince < r.cfg.SettleSecs {
		return call{}, false
	}
	if r.spoken && f.t-r.lastSpokenAt < r.cfg.MinIntervalSecs {
		return call{}, false
	}
	c := call{kind: kind(r.announced, pos), position: pos, previous: r.announced}
	r.announced, r.pending = pos, false
	r.spoken, r.lastSpokenAt = true, f.t
	return c, true
}

// kind is the phrase v1 picked for a move from was to pos.
func kind(was, pos int32) string {
	switch {
	case pos < was && pos == 1:
		return KindGainedLead
	case pos < was && pos <= 3:
		return KindGainedPodium
	case pos < was:
		return KindGained
	case was == 1:
		return KindLostLead
	default:
		return KindLost
	}
}
