package rules

import (
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// LapTime is v1's lap_time rule (paddock#91; v1's rules/lap_time.py): each lap
// the lap model closes with a time is called with its time and its delta to the
// best before it. ecu decides the meaning, the kind and slots contracts v0.2.3's
// Callout lists for lap_time; radio phrases it.
//
// The rule runs on the lap model's laps (paddock#83) rather than on GT7's posted
// time, as v1's did: the lap model closes a lap where v1's store closed it, on
// the lap counter's bump seen against the last driving frame, once per lap
// number, and on v1's 25 recordings each lap v1's rule called is a lap the lap
// model closes with the same time on the same frame, but one. On SUGO's replay
// (real-nuc-sugo-replay-2026-10-18, frame 864) GT7 jumps the counter from 4 to
// 13 on its way to the results, and v1 called "lap twelve" with the time of a
// lap neither v1's store nor the lap model ever closed. ecu raises nothing
// there: a known difference the parity harness maps (paddock#83's decision).
//
// The lap's pit role and whether it counts for the best come from the lap model
// too (LapCompleted's pit_role and counts_for_best), where v1's rule worked
// them out itself (those-who-run/those-who-run#177).
type LapTime struct {
	cfg LapTimeConfig

	lastLap   int32 // the last lap called this session (v1's last_announced_lap)
	ownBestMs int32 // the quickest lap that counts for the best, 0 for none
	lap1Ms    int32 // lap 1's time, counted or not, 0 before it closed
}

// LapTimeConfig is the rule's own settings, v1's rules.lap_time keys and
// defaults (config.DEFAULT_RULES). The thresholds choose the situational kinds,
// which ecu always names: radio falls back to the general phrase when its
// script lacks the situational one (contracts v0.2.3's Callout).
type LapTimeConfig struct {
	Priority        int32 // a lap's call (priority)
	NewBestPriority int32 // a new best (new_best_priority)
	SmallDeltaMs    int32 // slower by less than this is delta_slower_small (small_delta_ms)
	LargeDeltaMs    int32 // slower by more than this is delta_slower_large (large_delta_ms)
	SmallGapMs      int32 // a new best by less than this is template_new_best_small (small_gap_ms)
}

// DefaultLapTimeConfig is v1's: priority 20, a new best 11, and 300 ms,
// 2,000 ms and 200 ms for the situational kinds. Its pit_lane is hold.
func DefaultLapTimeConfig() LapTimeConfig {
	return LapTimeConfig{
		Priority:        20,
		NewBestPriority: 11,
		SmallDeltaMs:    300,
		LargeDeltaMs:    2000,
		SmallGapMs:      200,
	}
}

// LapTimeName is the rule's name, the callouts' rule.
const LapTimeName = "lap_time"

// LapTimePitLane is the rule's default pit_lane, v1's: a call due in a lane that
// crosses the line is kept to the exit.
const LapTimePitLane = eventsv1.PitLane_PIT_LANE_HOLD

// The kinds the rule raises (contracts v0.2.3's Callout, "lap_time").
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

// LapTimeFrame is what the rule reads of the frame that closed the lap, beside
// the lap itself.
type LapTimeFrame struct {
	// HasLapCount: the session has a lap total, on the frame that closed the lap
	// (v1's state.has_lap_count: GT7's total_laps above 0).
	HasLapCount bool
	// PrevGameBestMs is GT7's own best lap on the frame the rule compares against
	// (the last driving frame, across a pause or a loading screen), 0 or less for
	// none: v1's prev.best_lap_ms. It can be a best GT7 carried over from an
	// earlier session at the track (Circuit Gilles Villeneuve's 1:37.524, on no
	// lap of the pit-stop session, 2026-10-17), and the rule measures against it
	// as v1 did. It is not the lap model's lap.best.
	PrevGameBestMs int32
}

// LapCall is one callout the rule raised: its kind, slots and priority. The
// rest of the callout (expiry, hold, pit lane, ids and times) is the engine's.
type LapCall struct {
	Kind     string
	Slots    *eventsv1.CalloutSlots
	Priority int32
}

// NewLapTime is the rule with cfg.
func NewLapTime(cfg LapTimeConfig) *LapTime {
	return &LapTime{cfg: cfg}
}

// Reset forgets the session: called on every session start.
func (r *LapTime) Reset() {
	r.lastLap, r.ownBestMs, r.lap1Ms = 0, 0, 0
}

// Lap is the call for lap, a lap the lap model closed on this frame, or false
// when there is none: a lap with no time (a partial lap), or a lap already
// called.
//
// The best before the lap is GT7's own on the frame before when it has one,
// else the quickest lap of the session that counts for the best (not a pit lap,
// not a start lap GT7 leaves out). There is no delta with no best yet, nor on
// lap 2 of a lap race when its only reference is lap 1. The delta is the
// difference of the times as spoken, each rounded half up to tenths, so a lap
// that sounds the same as the best is delta_equal, never a new best by zero
// point zero; delta_ms carries the exact difference. A lap with a pit stop has
// its own kind, no delta, and is never a best.
func (r *LapTime) Lap(lap *eventsv1.LapCompleted, f LapTimeFrame) (LapCall, bool) {
	ms := lap.GetLapTimeMs()
	n := lap.GetLap()
	if ms <= 0 || n <= r.lastLap {
		return LapCall{}, false
	}
	r.lastLap = n

	prevBest := r.ownBestMs
	if f.PrevGameBestMs > 0 {
		prevBest = f.PrevGameBestMs
	}
	if lap.GetCountsForBest() && (r.ownBestMs <= 0 || ms < r.ownBestMs) {
		r.ownBestMs = ms
	}
	if n == 1 {
		r.lap1Ms = ms
	}

	call := LapCall{
		Priority: r.cfg.Priority,
		Slots:    &eventsv1.CalloutSlots{Lap: &n, LapTimeMs: &ms},
	}
	switch lap.GetPitRole() {
	case eventsv1.PitRole_PIT_ROLE_IN_LAP:
		call.Kind = KindInLap
		return call, true
	case eventsv1.PitRole_PIT_ROLE_OUT_LAP:
		call.Kind = KindOutLap
		return call, true
	case eventsv1.PitRole_PIT_ROLE_IN_AND_OUT:
		call.Kind = KindPitLap
		return call, true
	}

	onlyLap1 := f.HasLapCount && n == 2 && prevBest == r.lap1Ms
	if prevBest <= 0 || onlyLap1 {
		call.Kind = KindDeltaFirst
		return call, true
	}
	delta := ms - prevBest
	call.Slots.DeltaMs = &delta
	spoken := spokenDeltaMs(ms, prevBest)
	switch {
	case spoken < 0:
		call.Kind, call.Priority = KindNewBest, r.cfg.NewBestPriority
		if -spoken < r.cfg.SmallGapMs {
			call.Kind = KindNewBestSmall
		}
	case spoken == 0:
		call.Kind = KindDeltaEqual
	case spoken < r.cfg.SmallDeltaMs:
		call.Kind = KindDeltaSlowerSmall
	case spoken > r.cfg.LargeDeltaMs:
		call.Kind = KindDeltaSlowerLarge
	default:
		call.Kind = KindDeltaSlower
	}
	return call, true
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
