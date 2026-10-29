// Package rules holds ecu's callout rules: v1's built-in rules (rules/*.py at
// v1.0.0), each a pure decision over the race's frames, with no I/O (paddock#89).
package rules

import eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"

// The position rule (paddock#94): v1's PositionRule (rules/position.py at v1.0.0),
// the callout for places gained and lost. GT7 updates the position live and places
// change in bursts, so a change is held settle_secs and the latest position is
// called once things calm down, no sooner than min_interval_secs after the last
// call. The first start_grace_secs of a race are quiet apart from settling the
// grid slot (nine calls in the first minute at Suzuka East, 2026-10-15), and the
// first call after the grace compares with it.
//
// It reads the raw position on every frame, as v1 did, not race.position: that
// event has only the 3 s settle, and while this rule waits out its grace or its
// interval its settle timer runs on through every place that differs from the last
// call, so it can call a place race.position never publishes (P18 on frame 234 of
// SUGO's restarts, P13 on frame 823 of SUGO's two stops; paddock#84).
//
// Quiet in the pit lane, where places drop one by one while the car stands in the
// box (P5 to P11 at Circuit Gilles Villeneuve, 2026-10-17); the last position
// called is kept, so one call after the exit gives the net change. GT7 gives no
// warning of the entry, so each call is held a second (PositionHoldSecs) with
// pit_lane drop, and radio drops it if the entry follows in that second (SUGO,
// 2026-10-18, those-who-run#244).

// The position rule's kinds, v1's phrase keys (contracts v0.2.3's Callout): the
// situational pool where v1 had one, which radio falls back from to the general
// key when its script lacks it.
const (
	KindGained       = "template_gained"
	KindGainedLead   = "template_gained_lead"   // into first
	KindGainedPodium = "template_gained_podium" // into second or third
	KindLost         = "template_lost"
	KindLostLead     = "template_lost_lead" // out of first
)

// The position rule's common settings as v1's config.DEFAULT_RULES has them.
const (
	PositionPriority = 12
	PositionHoldSecs = 1.0
	PositionPitLane  = eventsv1.PitLane_PIT_LANE_DROP
)

// PositionConfig is the rule's own thresholds, in seconds (v1's settle_secs,
// min_interval_secs and start_grace_secs).
type PositionConfig struct {
	SettleSecs      float64
	MinIntervalSecs float64
	StartGraceSecs  float64
}

// DefaultPositionConfig is v1's defaults (config.DEFAULT_RULES, rules.example.yaml).
var DefaultPositionConfig = PositionConfig{SettleSecs: 3, MinIntervalSecs: 10, StartGraceSecs: 15}

// PositionFrame is what the rule reads of a frame the engine evaluates the rules on.
type PositionFrame struct {
	T         float64 // the frame's time, in seconds, on the race's clock
	InRace    bool    // a driving frame (v1's RaceState.in_race)
	Position  int32   // GT7's live race position, 0 for none
	HasField  bool    // v1's Rule.has_field: other cars are racing for position
	InPitLane bool    // the pit detector's state, as of this frame
}

// PositionCall is a call the rule raises: its kind and slots.
type PositionCall struct {
	Kind     string
	Position int32 // the place called
	Previous int32 // the last place called, or the grid slot
}

// Slots are the call's slots (contracts v0.2.3's CalloutSlots).
func (c PositionCall) Slots() *eventsv1.CalloutSlots {
	return &eventsv1.CalloutSlots{Position: &c.Position, PreviousPosition: &c.Previous}
}

// Position is v1's PositionRule. Reset it on every start.
type Position struct {
	cfg          PositionConfig
	announced    int32 // the last position called, or the grid slot; 0 for none
	started      bool
	startedAt    float64 // the first frame with a field and a position
	pending      bool    // a new position is settling since pendingSince
	pendingSince float64
	spoken       bool
	lastSpokenAt float64
}

// NewPosition is the rule with cfg.
func NewPosition(cfg PositionConfig) *Position { return &Position{cfg: cfg} }

// Reset forgets the race, as a start does.
func (p *Position) Reset() { *p = Position{cfg: p.cfg} }

// Evaluate is PositionRule.evaluate on f: the call it raises, if any.
func (p *Position) Evaluate(f PositionFrame) (PositionCall, bool) {
	if !f.InRace || !f.HasField || f.Position <= 0 {
		return PositionCall{}, false
	}
	pos := f.Position
	if !p.started {
		p.startedAt, p.started = f.T, true
	}
	if p.announced == 0 || f.T-p.startedAt < p.cfg.SettleSecs {
		// The grid slot takes a moment to settle: qualifying reported P20 on the
		// first driving frame and P1 on the next (2026-10-14). Follow it quietly.
		p.announced = pos
		return PositionCall{}, false
	}
	if f.T-p.startedAt < p.cfg.StartGraceSecs {
		return PositionCall{}, false
	}
	if f.InPitLane {
		p.pending = false // settle again from the pit exit
		return PositionCall{}, false
	}
	if pos == p.announced {
		p.pending = false
		return PositionCall{}, false
	}
	if !p.pending {
		p.pendingSince, p.pending = f.T, true
	}
	if f.T-p.pendingSince < p.cfg.SettleSecs {
		return PositionCall{}, false
	}
	if p.spoken && f.T-p.lastSpokenAt < p.cfg.MinIntervalSecs {
		return PositionCall{}, false
	}
	call := PositionCall{Kind: positionKind(p.announced, pos), Position: pos, Previous: p.announced}
	p.announced, p.pending = pos, false
	p.spoken, p.lastSpokenAt = true, f.T
	return call, true
}

// positionKind is the phrase v1 picked for a move from was to pos.
func positionKind(was, pos int32) string {
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
