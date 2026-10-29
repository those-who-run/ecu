// Package rules holds v1's callout rules, ported to Go: what each one decides to
// say, as a callout's kind and slots. Phrasing is radio's (paddock#89).
package rules

import (
	"strconv"

	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/track"
)

// The start brief (paddock#95): v1's StartBriefRule, from rules/start_brief.py at
// v1.0.0. Two seconds into a session the driver hears the track if it is known,
// the distance and the grid slot. A track found after the brief is named on its
// own while the first lap is driven. A session joined part way (paddock#75,
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

// The start brief's kinds (contracts v0.2.3, Callout's comment).
const (
	// KindBrief is the brief: one sentence for each slot set.
	KindBrief = "template"
	// KindBriefTrack is the track named on its own, after the brief.
	KindBriefTrack = "template_track"
	// KindBriefJoined is the brief of a session joined part way.
	KindBriefJoined = "template_joined"
)

// StartBriefConfig is the start brief's own settings (v1's rules.start_brief).
type StartBriefConfig struct {
	// SettleSecs is how long after the first driving frame the brief waits for
	// GT7's grid position to settle (settle_secs).
	SettleSecs float64
	// AnnounceTrack names a track found after the brief (announce_track).
	AnnounceTrack bool
	// LateTrackFirstLapOnly names it only while the first lap is driven
	// (late_track_first_lap_only).
	LateTrackFirstLapOnly bool
	// MinConfidence is how sure a named layout must be to be spoken
	// (min_confidence).
	MinConfidence track.Confidence
}

// DefaultStartBriefConfig is v1's DEFAULT_RULES for the start brief.
func DefaultStartBriefConfig() StartBriefConfig {
	return StartBriefConfig{SettleSecs: 2, AnnounceTrack: true, LateTrackFirstLapOnly: true, MinConfidence: track.Probable}
}

// StartBriefFrame is what the brief reads on an evaluated frame: the frame, and
// the session's verdicts as the rules see them (applied after the rules of the
// frame before, as v1's on_track and on_session were).
type StartBriefFrame struct {
	T           float64 // the frame's time, in seconds
	Lap         int32   // GT7's current lap
	TotalLaps   int32   // 0 for a timed session
	Position    int32   // the race position, 0 for none
	TotalCars   int32
	CarsOnTrack bool // GT7's cars-on-track flag

	// Kind is the session's kind, UNKNOWN (or unset) before the first verdict;
	// Field and KindCarsOnTrack are its field and cars-on-track flag.
	Kind            eventsv1.SessionType
	Field           bool
	KindCarsOnTrack bool

	// Track is the session's track verdict, nil before it has one.
	Track *track.Match
	// Joined says the session was joined part way, or continued after an ecu
	// restart: its start was not seen.
	Joined bool
}

// hasField is v1's Rule.has_field: once the kind is known, a field GT7 flags with
// cars on track; before it, GT7's flag and more than one car.
func (f *StartBriefFrame) hasField() bool {
	if f.Kind != eventsv1.SessionType_SESSION_TYPE_UNKNOWN && f.Kind != eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED {
		return f.Field && f.KindCarsOnTrack
	}
	return f.CarsOnTrack && f.TotalCars > 1
}

// StartBriefCall is a callout the brief raises: its kind and slots.
type StartBriefCall struct {
	Kind  string
	Slots *eventsv1.CalloutSlots
}

// StartBrief is the rule. Reset it on every session start.
type StartBrief struct {
	cfg StartBriefConfig

	started   bool
	startedAt float64
	startLap  int32
	briefed   bool

	seen    *track.Match // the verdict last read, to see it change (v1's on_track)
	name    string       // the track's name to speak, "" for none
	spoken  string       // the name last spoken
	pending string       // a name to speak on its own, "" for none
}

// NewStartBrief is the rule with cfg, reset.
func NewStartBrief(cfg StartBriefConfig) *StartBrief {
	return &StartBrief{cfg: cfg}
}

// Reset forgets the session: a start resets the rule.
func (r *StartBrief) Reset() {
	*r = StartBrief{cfg: r.cfg}
}

// speakable is the name to speak for m: the layout's when it is sure enough, else
// the circuit's when only that is known; "" for none.
func (r *StartBrief) speakable(m *track.Match) string {
	if m.TrackID != 0 && m.Confidence >= r.cfg.MinConfidence {
		if m.Name != "" {
			return m.Name
		}
		return "track " + strconv.Itoa(int(m.TrackID))
	}
	return m.Family
}

// onTrack is v1's on_track: the session's verdict changed.
func (r *StartBrief) onTrack(m *track.Match, joined bool) {
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
		r.pending = name
	}
}

// observeTrack calls onTrack when the verdict differs from the one last read.
func (r *StartBrief) observeTrack(m *track.Match, joined bool) {
	if m == nil || (r.seen != nil && sameVerdict(*m, *r.seen)) {
		return
	}
	r.seen = m
	r.onTrack(m, joined)
}

// sameVerdict says a and b are the same decision, as the session keeps its
// verdict (v1 called on_track only when the decision changed).
func sameVerdict(a, b track.Match) bool {
	aID, aConf := a.Decision()
	bID, bConf := b.Decision()
	return aID == bID && aConf == bConf
}

// Evaluate is StartBriefRule.evaluate, on a frame the rules are evaluated on.
func (r *StartBrief) Evaluate(f *StartBriefFrame) []StartBriefCall {
	r.observeTrack(f.Track, f.Joined)
	if !r.started {
		r.started, r.startedAt, r.startLap = true, f.T, f.Lap
	}
	var out []StartBriefCall
	if r.pending != "" {
		name := r.pending
		r.pending = ""
		tooLate := r.cfg.LateTrackFirstLapOnly && f.Lap > max(r.startLap, 1)
		if name != r.spoken && !tooLate {
			r.spoken = name
			out = append(out, StartBriefCall{Kind: KindBriefTrack, Slots: &eventsv1.CalloutSlots{Track: name}})
		}
	}
	if r.briefed || f.T-r.startedAt < r.cfg.SettleSecs {
		return out
	}
	r.briefed = true
	if f.Joined {
		return append(out, StartBriefCall{Kind: KindBriefJoined, Slots: joinedSlots(f)})
	}
	slots := &eventsv1.CalloutSlots{}
	said := false
	if r.name != "" {
		slots.Track, r.spoken, said = r.name, r.name, true
	}
	field := f.hasField()
	switch {
	case f.TotalLaps > 0 && r.startLap <= 1:
		slots.Laps, said = proto.Int32(f.TotalLaps), true
	case f.TotalLaps > 0:
		// Started past lap 1 (the engineer was restarted): say what is left, the
		// current lap included.
		if left := max(f.TotalLaps-f.Lap+1, 0); left > 0 {
			slots.LapsLeft, said = proto.Int32(left), true
		}
	case field:
		// No lap total and other cars: a timed race (or qualifying, which GT7
		// reports the same way). A lone car with no lap total is practice.
		slots.Timed, said = true, true
	}
	if field && f.Position > 0 {
		slots.Position, slots.TotalCars, said = proto.Int32(f.Position), proto.Int32(f.TotalCars), true
	}
	if said {
		out = append(out, StartBriefCall{Kind: KindBrief, Slots: slots})
	}
	return out
}

// joinedSlots are template_joined's: the lap, with the lap total in a lap-count
// race, and the place when there is a field.
func joinedSlots(f *StartBriefFrame) *eventsv1.CalloutSlots {
	slots := &eventsv1.CalloutSlots{Lap: proto.Int32(f.Lap)}
	if f.TotalLaps > 0 {
		slots.Laps = proto.Int32(f.TotalLaps)
	}
	if f.hasField() && f.Position > 0 {
		slots.Position, slots.TotalCars = proto.Int32(f.Position), proto.Int32(f.TotalCars)
	}
	return slots
}
