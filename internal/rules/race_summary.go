// Package rules holds the callout rules: v1's rules/*.py at v1.0.0, as decisions.
// A rule says which callout to raise, its kind and slots; radio phrases it
// (paddock#89, contracts v0.2.3's Callout).
package rules

import (
	"slices"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// RaceSummaryConfig is race_summary's part of the rules file (v1's
// rules.example.yaml): only what decides the callout. The template keys are
// radio's.
type RaceSummaryConfig struct {
	Priority int32         // priority: 25, after the final lap time posted on the same frame
	MaxAge   time.Duration // max_age_seconds: 30 s, so it waits behind that lap time
	PitLane  eventsv1.PitLane
	// quit_kinds: the session kinds that get a summary on leaving without a
	// flag. A quit race is a retirement and stays quiet by default.
	QuitKinds   []eventsv1.SessionType
	QuitMinLaps int32 // quit_min_laps: the laps a quit session needs for one
}

// DefaultRaceSummary is v1's DEFAULT_RULES race_summary: priority 25, 30 s,
// spoken in the pit lane (the flag can fall in a lane that crosses the line),
// a quit summary for solo_timed and solo_laps after a lap.
func DefaultRaceSummary() RaceSummaryConfig {
	return RaceSummaryConfig{
		Priority: 25,
		MaxAge:   30 * time.Second,
		PitLane:  eventsv1.PitLane_PIT_LANE_SPEAK,
		QuitKinds: []eventsv1.SessionType{
			eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED,
			eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS,
		},
		QuitMinLaps: 1,
	}
}

// The kinds race_summary raises (contracts v0.2.3, Callout's comment): at the
// flag, the key that fills the template's {position}, or the template itself
// when no position is said; on leaving a solo session, template_quit.
const (
	KindTemplate               = "template"
	KindTemplatePosition       = "template_position"
	KindTemplatePositionWin    = "template_position_win"
	KindTemplatePositionPodium = "template_position_podium"
	KindTemplateQuit           = "template_quit"
)

// raceSummaryName is the rule's name, v1's.
const raceSummaryName = "race_summary"

// Call is a callout a rule decided on: what to say, not how. The engine adds the
// ids, the times and the pit lane, from the rule's config.
type Call struct {
	Rule  string
	Kind  string
	Slots *eventsv1.CalloutSlots
}

// PostedLap is a lap time GT7 posted (v1's lap_time_posted), for the lap it
// belongs to (v1's posted_lap: one behind the counter).
type PostedLap struct {
	Lap int32
	Ms  int32
	// StartLap: a lap 1 GT7 leaves out of its own best (v1's _start_lap): the
	// session has a lap total, or GT7's best is still blank on the frame that
	// posted it. A lap 1 GT7 counts (qualifying from the pits, a rolling
	// start) is a lap.
	StartLap bool
}

// Finish is the frame a race finished at: the flag frame of a lap race, or the
// line crossing that ended a timed one (race.finished's at).
type Finish struct {
	Position  int32 // the position on that frame; 0 for none
	Field     bool  // other cars race for position (v1's has_field)
	GT7BestMs int32 // GT7's own best on that frame, raw; 0 for none
}

// End is a session ending without a flag (v1's on_end): quit, or back to the menu.
type End struct {
	Kind       eventsv1.SessionType // the session's kind; UNKNOWN when there is none
	CurrentLap int32                // GT7's lap counter on the last driving frame
	GT7BestMs  int32                // GT7's own best on the last driving frame, raw; 0 for none
}

// RaceSummary is v1's RaceSummaryRule: the finish and the best lap at the flag,
// or the laps and the best lap on leaving a solo session. Spoken once a session.
// The zero value with a config is the rule reset, as a start resets it.
//
// The best is GT7's raw best on the frame when it has one (it can be carried
// over from an earlier session at the track, as v1's was); else the best lap
// counted, leaving out a start lap; else any lap (a session whose only lap is
// lap 1). Each lap counts once, on its first post. Fuel is not said
// (those-who-run#245).
type RaceSummary struct {
	cfg RaceSummaryConfig

	lastLapCounted int32
	bestMs         int32 // the best lap counted, start laps left out
	bestAnyMs      int32 // lap 1 included, for a solo run with nothing else
	lapsDone       int32
	announced      bool
}

// NewRaceSummary is the rule with cfg.
func NewRaceSummary(cfg RaceSummaryConfig) *RaceSummary {
	return &RaceSummary{cfg: cfg}
}

// Config is the rule's config.
func (r *RaceSummary) Config() RaceSummaryConfig { return r.cfg }

// Reset forgets the session, as every start does.
func (r *RaceSummary) Reset() { *r = RaceSummary{cfg: r.cfg} }

// Posted takes a posted lap time (v1's evaluate). A second post for a lap
// already counted is ignored.
func (r *RaceSummary) Posted(lap PostedLap) {
	if lap.Ms <= 0 || lap.Lap <= r.lastLapCounted {
		return
	}
	r.lastLapCounted = lap.Lap
	r.lapsDone++
	if r.bestAnyMs <= 0 || lap.Ms < r.bestAnyMs {
		r.bestAnyMs = lap.Ms
	}
	if !lap.StartLap && (r.bestMs <= 0 || lap.Ms < r.bestMs) {
		r.bestMs = lap.Ms
	}
}

// Finished is the race over (v1's on_finish, and evaluate on the first FINISHED
// frame): the summary, once a session. The position is read off the finish's
// frame, which keeps a place gained in the last seconds (2026-10-14: "Up to
// sixth" was lost to the flag).
func (r *RaceSummary) Finished(f Finish) (Call, bool) {
	if r.announced {
		return Call{}, false
	}
	r.announced = true
	slots := &eventsv1.CalloutSlots{}
	kind := KindTemplate
	if f.Field && f.Position > 0 {
		pos := f.Position
		slots.Position = &pos
		switch {
		case pos == 1:
			kind = KindTemplatePositionWin
		case pos <= 3:
			kind = KindTemplatePositionPodium
		default:
			kind = KindTemplatePosition
		}
	}
	r.best(slots, f.GT7BestMs)
	return Call{Rule: raceSummaryName, Kind: kind, Slots: slots}, true
}

// Ended is the session ending without a flag (v1's on_end). A session of a kind
// in quit_kinds with at least quit_min_laps laps gets template_quit; the laps
// are counted from GT7's counter too, which knows laps the session joined too
// late to see. Never after the finish.
func (r *RaceSummary) Ended(e End) (Call, bool) {
	laps := max(r.lapsDone, e.CurrentLap-1)
	if r.announced || !slices.Contains(r.cfg.QuitKinds, e.Kind) || laps < r.cfg.QuitMinLaps {
		return Call{}, false
	}
	r.announced = true
	slots := &eventsv1.CalloutSlots{Laps: &laps}
	r.best(slots, e.GT7BestMs)
	return Call{Rule: raceSummaryName, Kind: KindTemplateQuit, Slots: slots}, true
}

// best sets the best lap's slot (v1's _best_words): GT7's raw best when it has
// one, else the best counted lap, else any lap; none when there is no lap.
func (r *RaceSummary) best(slots *eventsv1.CalloutSlots, gt7BestMs int32) {
	best := gt7BestMs
	if best <= 0 {
		best = r.bestMs
	}
	if best <= 0 {
		best = r.bestAnyMs
	}
	if best > 0 {
		slots.BestLapMs = &best
	}
}
