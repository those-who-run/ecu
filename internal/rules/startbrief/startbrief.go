// Package startbrief is v1's start_brief rule (rules/start_brief.py at v1.0.0,
// paddock#95): two seconds into a session the driver hears the track if it is
// known, the distance and the grid slot. A track found after the brief is named
// on its own while the first lap is driven. A session joined part way (paddock#75,
// paddock#76) had no start to brief: it gets "Back with you" with the lap and the
// place, and no track.
//
// v1's docstring: spoken a couple of seconds after the first driving frame,
// because GT7 takes a frame or two to settle the grid position (qualifying
// reported P20 and then P1 on consecutive frames, 2026-10-14). On a rolling start
// the track is usually known before the lights; on a standing start it is not, so
// the name is spoken on its own once the detector names a track, as long as the
// first lap is still being driven. Once the line is crossed the driver knows where
// they are, so a name that only became certain at the line (Suzuka East,
// 2026-10-15) is not read out a lap late. A changed verdict is spoken again under
// the same limit; a confidence upgrade for the same track is not. When the
// detector cannot pick between layouts of one circuit (Fuji and Fuji Short look
// the same in the table), the circuit's name is spoken.
//
// The track slot is the name as the verdict gives it, as GT7's table writes it
// ("Circuit de Spa-Francorchamps"); radio says it as v1's track_words did.
package startbrief

import (
	"strconv"

	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/ecu/internal/track"
)

// Name is the rule's name, its key in the rules file.
const Name = "start_brief"

// The rule's kinds (contracts v0.2.3, Callout's comment).
const (
	// KindBrief is the brief: one sentence for each slot set.
	KindBrief = "template"
	// KindTrack is the track named on its own, after the brief.
	KindTrack = "template_track"
	// KindJoined is the brief of a session joined part way.
	KindJoined = "template_joined"
)

var _ = rules.Register(Name, build)

// build is the rule with its settings over v1's defaults.
func build(s *rules.Settings) rules.Rule {
	cfg := defaults()
	s.Decode(&cfg)
	return &rule{cfg: cfg, minRank: rank(cfg.MinConfidence)}
}

// config is the rule's own settings, with v1's defaults (DEFAULT_RULES).
type config struct {
	// SettleSecs is how long after the first driving frame the brief waits for
	// GT7's grid position to settle.
	SettleSecs float64 `yaml:"settle_secs"`
	// AnnounceTrack names a track found after the brief.
	AnnounceTrack bool `yaml:"announce_track"`
	// FirstLapOnly names it only while the first lap is driven.
	FirstLapOnly bool `yaml:"late_track_first_lap_only"`
	// MinConfidence is how sure a named layout must be to be spoken.
	MinConfidence string `yaml:"min_confidence"`
}

func defaults() config {
	return config{SettleSecs: 2, AnnounceTrack: true, FirstLapOnly: true, MinConfidence: "probable"}
}

// rank is v1's CONFIDENCE_RANK, probable for a name it doesn't know.
func rank(name string) track.Confidence {
	switch name {
	case "none":
		return track.None
	case "ambiguous":
		return track.Ambiguous
	case "confirmed":
		return track.Confirmed
	default:
		return track.Probable
	}
}

// rule is StartBriefRule.
type rule struct {
	cfg     config
	minRank track.Confidence

	started   bool
	startedAt float64
	startLap  int32
	briefed   bool

	name    string // the track's name to speak, "" for none
	spoken  string // the name last spoken
	pending string // a name to speak on its own, "" for none
}

func (r *rule) Reset() {
	*r = rule{cfg: r.cfg, minRank: r.minRank}
}

// speakable is the name to speak for m: the layout's when it is sure enough, else
// the circuit's when only that is known; "" for none.
func (r *rule) speakable(m *track.Match) string {
	if m.TrackID != 0 && m.Confidence >= r.minRank {
		if m.Name != "" {
			return m.Name
		}
		return "track " + strconv.Itoa(int(m.TrackID))
	}
	return m.Family
}

// onTrack is v1's on_track: the session's verdict changed.
func (r *rule) onTrack(m *track.Match, joined bool) {
	name := r.speakable(m)
	if name == "" {
		// The engine only downgrades when the layout was ruled out: forget it.
		r.name, r.pending = "", ""
		return
	}
	r.name = name
	if joined {
		return // the driver has been driving it: no news
	}
	if r.briefed && r.cfg.AnnounceTrack && name != r.spoken {
		// A callout needs a frame, so it is raised on the next evaluated one.
		r.pending = name
	}
}

// Evaluate takes each track verdict applied, as v1's on_track, then, on a racing
// frame, is StartBriefRule.evaluate.
func (r *rule) Evaluate(ctx *race.Context) []rules.Callout {
	for _, m := range ctx.TrackChanges {
		r.onTrack(m, ctx.Joined)
	}
	if !ctx.Racing {
		return nil
	}
	cur := &ctx.Cur
	if !r.started {
		r.started, r.startedAt, r.startLap = true, cur.T, cur.Lap
	}
	var out []rules.Callout
	if r.pending != "" {
		name := r.pending
		r.pending = ""
		tooLate := r.cfg.FirstLapOnly && cur.Lap > max(r.startLap, 1)
		if name != r.spoken && !tooLate {
			r.spoken = name
			out = append(out, rules.Callout{Kind: KindTrack, Slots: &eventsv1.CalloutSlots{Track: name}})
		}
	}
	if r.briefed || cur.T-r.startedAt < r.cfg.SettleSecs {
		return out
	}
	r.briefed = true
	if ctx.Joined {
		return append(out, rules.Callout{Kind: KindJoined, Slots: joined(ctx)})
	}
	if slots, said := r.brief(ctx); said {
		out = append(out, rules.Callout{Kind: KindBrief, Slots: slots})
	}
	return out
}

// brief is the brief's slots, one for each sentence v1 said: the track, the laps
// (or the laps to go, or timed) and the grid slot. said is false with none.
func (r *rule) brief(ctx *race.Context) (slots *eventsv1.CalloutSlots, said bool) {
	cur := &ctx.Cur
	slots = &eventsv1.CalloutSlots{}
	if r.name != "" {
		slots.Track, r.spoken, said = r.name, r.name, true
	}
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
		// No lap total and other cars: a timed race (or qualifying, which GT7
		// reports the same way). A lone car with no lap total is practice.
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
