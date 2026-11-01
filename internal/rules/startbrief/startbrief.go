// Package startbrief is v1's start_brief rule (rules/start_brief.py at v1.0.0,
// paddock#95), without the track: two seconds into a session the driver hears
// the distance and the grid slot. A session joined part way (paddock#75,
// paddock#76) had no start to brief: it gets "Back with you" with the lap and the
// place.
//
// v1's docstring: spoken a couple of seconds after the first driving frame,
// because GT7 takes a frame or two to settle the grid position (qualifying
// reported P20 and then P1 on consecutive frames, 2026-10-14).
//
// A departure from v1 (the maintainer's decision, 2026-10-31): v1 named the track
// in the brief when its detector had one, and named one found later on its own
// while the first lap was driven (template_track). ecu names no track at the
// start: every wrong track spoken so far (Fuji 2026-10-29, Road Atlanta
// 2026-10-30, Willow Springs then Fuji after a restart on lap 1 at SUGO
// 2026-10-31) was named before the detector could be sure, and in VR a wrong
// name is worse than none. race_summary names the track at the end, when the
// detector is sure by then.
//
// Another (the maintainer's decision, 2026-10-31): a ranked session, qualifying or
// practice with the field out (race.Context.Ranked), gets a brief of its own,
// template_ranked with the field's size, in place of v1's "Timed race. P one of
// sixteen.": it is not a race, and its place before anyone has a lap time is no
// rank (P1 from slot 11 in the practice of 2026-10-31).
package startbrief

import (
	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/ecu/race"
)

// Name is the rule's name, its key in the rules file.
const Name = "start_brief"

// The rule's kinds (contracts v0.2.3, Callout's comment). v1's template_track,
// the track named on its own, is never raised.
const (
	// KindBrief is the brief: one sentence for each slot set.
	KindBrief = "template"
	// KindJoined is the brief of a session joined part way.
	KindJoined = "template_joined"
	// KindRanked is a ranked session's brief: the cars it is ranked against.
	KindRanked = "template_ranked"
)

var _ = rules.Register(Name, build)

// build is the rule with its settings over v1's defaults.
func build(s *rules.Settings) rules.Rule {
	cfg := defaults()
	s.Decode(&cfg)
	return &rule{cfg: cfg}
}

// config is the rule's own settings, with v1's defaults (DEFAULT_RULES). v1's
// announce_track, late_track_first_lap_only and min_confidence were about the
// track: a rules file that sets them still loads, and they do nothing.
type config struct {
	// SettleSecs is how long after the first driving frame the brief waits for
	// GT7's grid position to settle.
	SettleSecs float64 `yaml:"settle_secs"`
}

func defaults() config {
	return config{SettleSecs: 2}
}

// rule is StartBriefRule.
type rule struct {
	cfg config

	started   bool
	startedAt float64
	startLap  int32
	briefed   bool
}

func (r *rule) Reset() {
	*r = rule{cfg: r.cfg}
}

// Evaluate is StartBriefRule.evaluate on a racing frame, less the track.
func (r *rule) Evaluate(ctx *race.Context) []rules.Callout {
	if !ctx.Racing {
		return nil
	}
	cur := &ctx.Cur
	if !r.started {
		r.started, r.startedAt, r.startLap = true, cur.T, cur.Lap
	}
	if r.briefed || cur.T-r.startedAt < r.cfg.SettleSecs {
		return nil
	}
	r.briefed = true
	if ctx.Joined {
		return []rules.Callout{{Kind: KindJoined, Slots: joined(ctx)}}
	}
	if ctx.Ranked() {
		slots := &eventsv1.CalloutSlots{}
		if cars := ctx.Cur.TotalCars; cars > 0 {
			slots.TotalCars = proto.Int32(cars)
		}
		return []rules.Callout{{Kind: KindRanked, Slots: slots}}
	}
	if slots, said := r.brief(ctx); said {
		return []rules.Callout{{Kind: KindBrief, Slots: slots}}
	}
	return nil
}

// brief is the brief's slots, one for each sentence v1 said bar the track: the
// laps (or the laps to go, or timed) and the grid slot. said is false with none.
func (r *rule) brief(ctx *race.Context) (slots *eventsv1.CalloutSlots, said bool) {
	cur := &ctx.Cur
	slots = &eventsv1.CalloutSlots{}
	field := ctx.HasField()
	left, lapCount := cur.LapsLeft()
	switch {
	case lapCount && r.startLap <= 1:
		slots.Laps, said = proto.Int32(cur.TotalLaps), true
	case lapCount:
		// Joined mid-race (the engineer was restarted): say what is left.
		if left > 0 {
			slots.LapsLeft, said = proto.Int32(left), true
		}
	case field:
		// No lap total and other cars: a timed race (a ranked session has its
		// own brief, above). A lone car with no lap total is a time trial or
		// practice alone.
		slots.Timed, said = true, true
	}
	if p := cur.RacePosition(); field && p > 0 {
		slots.Position, slots.TotalCars, said = proto.Int32(p), proto.Int32(cur.TotalCars), true
	}
	return slots, said
}

// joined are template_joined's slots: the lap, with the lap total in a lap-count
// race, and the place when there is a field.
func joined(ctx *race.Context) *eventsv1.CalloutSlots {
	cur := &ctx.Cur
	slots := &eventsv1.CalloutSlots{Lap: proto.Int32(cur.Lap)}
	if cur.HasLapCount() {
		slots.Laps = proto.Int32(cur.TotalLaps)
	}
	if p := cur.RacePosition(); ctx.HasField() && p > 0 {
		slots.Position, slots.TotalCars = proto.Int32(p), proto.Int32(cur.TotalCars)
	}
	return slots
}
