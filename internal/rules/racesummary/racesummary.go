// Package racesummary is v1's race_summary rule (rules/race_summary.py at v1.0.0,
// paddock#96): the last thing said in a session. At the flag, or the line
// crossing that ended a timed race, the finishing position and the best lap; on
// leaving a solo session without a flag, the laps done and the best lap. Spoken
// once a session.
//
// The position is read off the frame the race finished as of, which keeps a
// place gained in the last seconds (2026-10-14: "Up to sixth" was lost to the
// flag). The best is GT7's raw best on that frame when it has one (it can be
// carried over from an earlier session at the track, as v1's was); else the
// quickest lap posted, leaving out a start lap GT7 leaves out; else any lap (a
// session whose only lap is lap 1). Each lap counts once, on its first post.
// Fuel is not said (those-who-run#245).
//
// A departure from v1 (the maintainer's decision, 2026-10-31): the summary names
// the track (the track slot) when the session's verdict is confirmed by then, a
// full lap fitting one layout; start_brief no longer names it at the start. A
// probable verdict, or a circuit whose layout can't be picked (Fuji), names
// none: in VR a wrong name is worse than none.
//
// Another (the maintainer's decision, 2026-10-31): a ranked session, qualifying or
// practice with the field out (race.Context.Ranked), is no race to win. Its finish
// is template_ranked, the place as a rank by lap time with the best lap, not
// "Chequered flag. Finished first. Brilliant drive.", and leaving one without a
// finish gets template_quit as a solo session does (ranked is in quit_kinds by
// default; v1 read these sessions as timed fields, which say nothing on leaving).
//
// v1's defaults that the engine applies (internal/rules/config.go): priority
// 25, after the final lap time posted on the same frame; max_age_seconds 30, so
// it waits behind that lap time; pit_lane speak, since the flag can fall in a
// lane that crosses the line.
package racesummary

import (
	"slices"
	"strings"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/ecu/internal/track"
)

// Name is the rule's name, its key in the rules file.
const Name = "race_summary"

// The kinds the rule raises (contracts v0.2.3, Callout's comment): at the
// finish, the key that fills the template's {position}, or the template itself
// when no position is said; on leaving a solo session, template_quit.
const (
	KindTemplate               = "template"
	KindTemplatePosition       = "template_position"
	KindTemplatePositionWin    = "template_position_win"
	KindTemplatePositionPodium = "template_position_podium"
	KindTemplateQuit           = "template_quit"
	// KindTemplateRanked is a ranked session's finish, a template of its own.
	KindTemplateRanked = "template_ranked"
)

// Config is the rule's own settings, v1's keys and defaults.
type Config struct {
	// QuitKinds are the session kinds, as v1 names them, that get a summary on
	// leaving without a flag. A quit race is a retirement and stays quiet by
	// default; add lap_race to hear one.
	QuitKinds []string `yaml:"quit_kinds"`
	// QuitMinLaps is the laps a quit session needs for one.
	QuitMinLaps int32 `yaml:"quit_min_laps"`
}

// Default is v1's, solo_timed and solo_laps after a lap, and ecu's ranked kind.
func Default() Config {
	return Config{QuitKinds: []string{"solo_timed", "solo_laps", "ranked"}, QuitMinLaps: 1}
}

var _ = rules.Register(Name, func(s *rules.Settings) rules.Rule {
	cfg := Default()
	s.Decode(&cfg)
	return New(cfg)
})

// sessionType is the contracts' SessionType for v1's kind name, such as
// solo_timed; UNSPECIFIED for a name it has none for, which no session has.
func sessionType(kind string) eventsv1.SessionType {
	return eventsv1.SessionType(eventsv1.SessionType_value["SESSION_TYPE_"+strings.ToUpper(kind)])
}

// Rule is RaceSummaryRule. Its zero value with a config is the rule reset.
type Rule struct {
	quitKinds   []eventsv1.SessionType
	quitMinLaps int32

	lastLapCounted int32
	bestMs         int32 // the quickest lap posted, start laps left out
	bestAnyMs      int32 // lap 1 included, for a solo run with nothing else
	lapsDone       int32
	announced      bool
}

// New is the rule with cfg.
func New(cfg Config) *Rule {
	r := &Rule{quitMinLaps: cfg.QuitMinLaps}
	for _, k := range cfg.QuitKinds {
		if t := sessionType(k); t != eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED {
			r.quitKinds = append(r.quitKinds, t)
		}
	}
	return r
}

// Reset forgets the session, as every start does.
func (r *Rule) Reset() {
	*r = Rule{quitKinds: r.quitKinds, quitMinLaps: r.quitMinLaps}
}

// Evaluate counts each posted lap, then raises the summary on the finish
// (ctx.Finish, the flag frame or a timed finish's crossing, which the callout is
// created as of) or on the session ending without one (ctx.EndedEarly).
func (r *Rule) Evaluate(ctx *race.Context) []rules.Callout {
	if ctx.Racing && ctx.Posted != nil {
		r.posted(ctx.Posted)
	}
	switch {
	case ctx.Finish != nil:
		if c, ok := r.finished(ctx, ctx.Finish); ok {
			return []rules.Callout{c}
		}
	case ctx.EndedEarly != nil && !ctx.Finished:
		if c, ok := r.ended(ctx, ctx.EndedEarly); ok {
			return []rules.Callout{c}
		}
	}
	return nil
}

// posted counts a posted lap time; a second post for a lap already counted is
// ignored.
func (r *Rule) posted(lap *race.PostedLap) {
	if lap.TimeMs <= 0 || lap.Lap <= r.lastLapCounted {
		return
	}
	r.lastLapCounted = lap.Lap
	r.lapsDone++
	if r.bestAnyMs <= 0 || lap.TimeMs < r.bestAnyMs {
		r.bestAnyMs = lap.TimeMs
	}
	if !lap.StartLap && (r.bestMs <= 0 || lap.TimeMs < r.bestMs) {
		r.bestMs = lap.TimeMs
	}
}

// finished is v1's on_finish: the summary as of at, once a session, with the
// position on at when there is a field, in the pool its place picks.
func (r *Rule) finished(ctx *race.Context, at *race.View) (rules.Callout, bool) {
	if r.announced {
		return rules.Callout{}, false
	}
	r.announced = true
	slots := &eventsv1.CalloutSlots{}
	kind := KindTemplate
	// A ranked session's place on the crossing is stale: GT7 ranks the lap a
	// moment later, so the rank is the frame's the finish is found on (the
	// timed finish, 3 s of coasting after the line).
	place := at
	if ctx.Ranked() {
		place = &ctx.Cur
	}
	if pos := place.RacePosition(); pos > 0 && ctx.HasFieldOn(place) {
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
	if ctx.Ranked() {
		kind = KindTemplateRanked // the place, when there is one, as a rank
	}
	r.best(slots, at.BestLapMs)
	slots.Track = sureTrack(ctx.Track)
	asOf := *at
	return rules.Callout{Kind: kind, Slots: slots, AsOf: &asOf}, true
}

// ended is v1's on_end: leaving a session of a kind in quit_kinds, with at least
// quit_min_laps laps, gives template_quit. The laps are counted from GT7's
// counter on last, the last driving frame, too: it knows laps the session
// joined too late to see.
func (r *Rule) ended(ctx *race.Context, last *race.View) (rules.Callout, bool) {
	laps := max(r.lapsDone, last.Lap-1)
	if r.announced || !slices.Contains(r.quitKinds, ctx.Kind.Type) || laps < r.quitMinLaps {
		return rules.Callout{}, false
	}
	r.announced = true
	slots := &eventsv1.CalloutSlots{Laps: &laps}
	r.best(slots, last.BestLapMs)
	slots.Track = sureTrack(ctx.Track)
	return rules.Callout{Kind: KindTemplateQuit, Slots: slots}, true
}

// best sets the best lap's slot (v1's _best_words): GT7's raw best when it has
// one, else the quickest lap posted that counts, else any lap; none with no lap.
func (r *Rule) best(slots *eventsv1.CalloutSlots, gt7BestMs int32) {
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

// sureTrack is the track to name at the end: the verdict's layout when it is
// confirmed (contracts' TRACK_CONFIDENCE_CONFIRMED), as the verdict names it;
// "" for none, or for a verdict any less sure.
func sureTrack(m *track.Match) string {
	if m == nil || m.Confidence != track.Confirmed || m.TrackID == 0 || m.Family != "" {
		return ""
	}
	return m.Name
}
