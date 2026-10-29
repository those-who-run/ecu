// Package laptime is v1's lap_time rule (rules/lap_time.py at v1.0.0,
// paddock#91): each lap closed with a time is called with its time and its delta
// to the best before it, a new best with its own kind and priority, and a lap
// with a pit stop with its own kind and no delta. ecu decides the kind and slots
// (contracts v0.2.3's Callout, "lap_time"); radio phrases them.
//
// The rule runs on the lap the race state closed on the frame (race.Context.Lap,
// lap.completed, paddock#83), where v1's ran on GT7's posted time. The lap model
// closes a lap where v1's store closed it, on the lap counter's bump seen against
// the last driving frame, once per lap number; on v1's 25 recordings every lap
// v1's rule called is a lap closed with the same time on the same frame, but one.
// On SUGO's replay (real-nuc-sugo-replay-2026-10-18, frame 864) GT7 jumps the
// counter from 4 to 13 on its way to the results, and v1 called "lap twelve"
// with a time for a lap neither v1's store nor the lap model closed. ecu says
// nothing there, a known difference the parity harness maps (paddock#83's
// decision). The lap's pit role and whether it counts for the best come from the
// lap model too, where v1's rule worked them out itself
// (those-who-run/those-who-run#177).
package laptime

import (
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"

	"github.com/those-who-run/ecu/internal/pit"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
)

// Name is the rule's name, its key in the rules file.
const Name = "lap_time"

var _ = rules.Register(Name, func(s *rules.Settings) rules.Rule {
	cfg := defaults()
	s.Decode(&cfg)
	return &rule{cfg: cfg}
})

// The kinds the rule raises (contracts v0.2.3's Callout, "lap_time"). A lap's
// call is the rule's template, and its kind is the key of the phrase that fills
// its {delta}; a new best and a lap with a pit stop have templates of their own.
const (
	KindDeltaFirst       = "delta_first"
	KindDeltaEqual       = "delta_equal"
	KindDeltaSlower      = "delta_slower"
	KindDeltaSlowerSmall = "delta_slower_small"
	KindDeltaSlowerLarge = "delta_slower_large"
	KindNewBest          = "template_new_best"
	KindNewBestSmall     = "template_new_best_small"
	KindInLap            = "template_in_lap"
	KindOutLap           = "template_out_lap"
	KindPitLap           = "template_pit_lap"
)

// config is the rule's own settings, v1's rules.lap_time keys. The thresholds
// choose the situational kinds, which ecu always names: radio falls back to the
// general phrase when its script lacks the situational one.
type config struct {
	NewBestPriority int32 `yaml:"new_best_priority"` // a new best's priority
	SmallDeltaMs    int32 `yaml:"small_delta_ms"`    // slower by less: delta_slower_small
	LargeDeltaMs    int32 `yaml:"large_delta_ms"`    // slower by more: delta_slower_large
	SmallGapMs      int32 `yaml:"small_gap_ms"`      // a new best by less: template_new_best_small
}

// defaults are v1's (config.DEFAULT_RULES and LapTimeRule.reset).
func defaults() config {
	return config{NewBestPriority: 11, SmallDeltaMs: 300, LargeDeltaMs: 2000, SmallGapMs: 200}
}

// rule is LapTimeRule. Its own best (own_best_ms) is the context's
// QuickestCountingMs, so it keeps only what the lap model doesn't: the last lap
// called and lap 1's time.
type rule struct {
	cfg     config
	lastLap int32 // the last lap called this session (v1's last_announced_lap)
	lap1Ms  int32 // lap 1's time, counted or not, 0 before it closed
}

func (r *rule) Reset() { r.lastLap, r.lap1Ms = 0, 0 }

// Evaluate calls the lap closed on the frame, if any.
func (r *rule) Evaluate(ctx *race.Context) []rules.Callout {
	if !ctx.Racing || ctx.Lap == nil {
		return nil
	}
	c, ok := r.call(*ctx.Lap, ctx.Cur.HasLapCount(), ctx.PrevBestMs())
	if !ok {
		return nil
	}
	return []rules.Callout{c}
}

// call is the call for lap, closed on a frame of a race with a lap total when
// hasLapCount, against prevBest, the best before it (race.Context.PrevBestMs:
// GT7's raw best on the frame before, else the quickest counting lap). It is
// false for a lap with no time, or a lap already called.
//
// There is no delta with no best yet, nor on lap 2 of a lap race when its only
// reference is lap 1. The delta is the difference of the times as spoken, each
// rounded half up to tenths, so a lap that sounds the same as the best is
// delta_equal, never a new best by zero point zero; delta_ms carries the exact
// difference. A lap with a pit stop has its own kind and no delta.
func (r *rule) call(lap race.ClosedLap, hasLapCount bool, prevBest int32) (rules.Callout, bool) {
	n, ms := lap.Lap, lap.TimeMs
	if ms <= 0 || n <= r.lastLap {
		return rules.Callout{}, false
	}
	r.lastLap = n
	if n == 1 {
		r.lap1Ms = ms
	}

	c := rules.Callout{Slots: &eventsv1.CalloutSlots{Lap: &n, LapTimeMs: &ms}}
	switch lap.Role {
	case pit.RoleIn:
		c.Kind = KindInLap
		return c, true
	case pit.RoleOut:
		c.Kind = KindOutLap
		return c, true
	case pit.RoleInOut:
		c.Kind = KindPitLap
		return c, true
	case pit.RoleNone:
	}

	onlyLap1 := hasLapCount && n == 2 && prevBest == r.lap1Ms
	if prevBest <= 0 || onlyLap1 {
		c.Kind = KindDeltaFirst
		return c, true
	}
	delta := ms - prevBest
	c.Slots.DeltaMs = &delta
	spoken := spokenDeltaMs(ms, prevBest)
	switch {
	case spoken < 0:
		c.Kind, c.Priority = KindNewBest, r.cfg.NewBestPriority
		if -spoken < r.cfg.SmallGapMs {
			c.Kind = KindNewBestSmall
		}
	case spoken == 0:
		c.Kind = KindDeltaEqual
	case spoken < r.cfg.SmallDeltaMs:
		c.Kind = KindDeltaSlowerSmall
	case spoken > r.cfg.LargeDeltaMs:
		c.Kind = KindDeltaSlowerLarge
	default:
		c.Kind = KindDeltaSlower
	}
	return c, true
}

// tenthsHalfUp is a time in whole tenths of a second, ties up (v1's
// verbalise.tenths_half_up): 102250 is 1023, 102350 is 1024.
func tenthsHalfUp(ms int32) int32 {
	return (ms + 50) / 100
}

// spokenDeltaMs is the difference of two times as spoken, in milliseconds, a
// multiple of 100 (v1's verbalise.spoken_delta_ms).
func spokenDeltaMs(ms, refMs int32) int32 {
	return (tenthsHalfUp(ms) - tenthsHalfUp(refMs)) * 100
}
