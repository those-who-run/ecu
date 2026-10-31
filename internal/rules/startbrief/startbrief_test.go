package startbrief

import (
	"testing"

	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/ecu/internal/track"
)

var (
	sugo = &track.Match{TrackID: 9, Name: "Sportsland SUGO", Confidence: track.Probable}
	n24h = &track.Match{TrackID: 4, Name: "Nurburgring 24h", Confidence: track.Probable}
	spa  = &track.Match{Name: "Circuit de Spa-Francorchamps", Family: "Circuit de Spa-Francorchamps", Confidence: track.Ambiguous}
)

func newRule() *rule {
	return &rule{cfg: defaults()}
}

// frame is a racing frame at t, lap 1, with the kind and the track given.
func frame(t float64, kind race.Kind, v race.View, changes ...*track.Match) *race.Context {
	v.T, v.Phase = t, telemetryv1.Phase_PHASE_IN_RACE
	if v.Lap == 0 {
		v.Lap = 1
	}
	return &race.Context{Cur: v, Racing: true, Kind: kind, TrackChanges: changes}
}

var (
	lapRaceKind = race.Kind{Type: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, Field: true}
	timedKind   = race.Kind{Type: eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD, Field: true}
	soloKind    = race.Kind{Type: eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED}
	unknownKind = race.Kind{Type: eventsv1.SessionType_SESSION_TYPE_UNKNOWN}
	lapRace     = race.View{TotalLaps: 2, Position: 10, TotalCars: 20, CarsOnTrack: true}
	timedRace   = race.View{Position: 1, TotalCars: 20, CarsOnTrack: true}
	solo        = race.View{Position: -1, TotalCars: 1}
)

func check(t *testing.T, got []rules.Callout, want ...rules.Callout) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d callouts %v, want %v", len(got), got, want)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || !proto.Equal(got[i].Slots, want[i].Slots) {
			t.Errorf("callout %d: got %s %v, want %s %v", i, got[i].Kind, got[i].Slots, want[i].Kind, want[i].Slots)
		}
	}
}

// R1: the brief waits settle_secs after the first racing frame, then says the
// distance and the grid slot, once ("Two laps. P ten of twenty."), and not the
// track it knows (the decision of 2026-10-31). A frame that isn't racing doesn't
// start the wait.
func TestTheBriefIsSaidOnceAfterTheSettle(t *testing.T) {
	r := newRule()
	check(t, r.Evaluate(&race.Context{Cur: race.View{T: 5}, TrackChanges: []*track.Match{sugo}}))
	check(t, r.Evaluate(frame(10, lapRaceKind, lapRace)))
	check(t, r.Evaluate(frame(11.99, lapRaceKind, lapRace)))
	check(t, r.Evaluate(frame(12, lapRaceKind, lapRace)), rules.Callout{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Laps: proto.Int32(2), Position: proto.Int32(10), TotalCars: proto.Int32(20),
	}})
	check(t, r.Evaluate(frame(13, lapRaceKind, lapRace)))
}

// The decision of 2026-10-31, a departure from v1: no track is named at the
// start. A timed race with no track yet is "Timed race. P one of twenty.", and a
// track found on lap 1 after it, sure or not, a layout or a circuit, is not
// named on its own (v1's template_track); a solo session that only knows its
// track has nothing to brief.
func TestNoTrackIsNamedAtTheStart(t *testing.T) {
	r := newRule()
	check(t, r.Evaluate(frame(0, timedKind, timedRace)))
	check(t, r.Evaluate(frame(2, timedKind, timedRace)), rules.Callout{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Timed: true, Position: proto.Int32(1), TotalCars: proto.Int32(20),
	}})
	sure := *n24h
	sure.Confidence = track.Confirmed
	check(t, r.Evaluate(frame(5, timedKind, timedRace, n24h)))
	check(t, r.Evaluate(frame(6, timedKind, timedRace, &sure, sugo)))

	for _, m := range []*track.Match{sugo, &sure, spa} {
		r = newRule()
		r.Evaluate(frame(0, soloKind, solo, m))
		check(t, r.Evaluate(frame(2, soloKind, solo)))
	}
}

// R3, R5, AC1, AC3, AC4: a session joined part way is "Back with you. Lap
// {lap}.", with the lap total in a lap race and the place with a field, and never
// a track or a grid slot, even with the track known.
func TestAJoinedSessionIsWelcomedBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind race.Kind
		view race.View
		want *eventsv1.CalloutSlots
	}{
		{"practice", soloKind, race.View{Lap: 7, Position: -1, TotalCars: 1},
			&eventsv1.CalloutSlots{Lap: proto.Int32(7)}},
		{"timed race", unknownKind, race.View{Lap: 7, Position: 1, TotalCars: 17, CarsOnTrack: true},
			&eventsv1.CalloutSlots{Lap: proto.Int32(7), Position: proto.Int32(1), TotalCars: proto.Int32(17)}},
		{"lap race", lapRaceKind, race.View{Lap: 5, TotalLaps: 12, Position: 7, TotalCars: 20, CarsOnTrack: true},
			&eventsv1.CalloutSlots{Lap: proto.Int32(5), Laps: proto.Int32(12), Position: proto.Int32(7), TotalCars: proto.Int32(20)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRule()
			joinedFrame := func(at float64, changes ...*track.Match) *race.Context {
				c := frame(at, tc.kind, tc.view, changes...)
				c.Joined = true
				return c
			}
			check(t, r.Evaluate(joinedFrame(0, sugo)))
			check(t, r.Evaluate(joinedFrame(2)), rules.Callout{Kind: KindJoined, Slots: tc.want})
			check(t, r.Evaluate(joinedFrame(3, n24h)))
		})
	}
}

// R1: a race first seen past lap 1, not joined, says the laps to go, and none
// once the last lap is past.
func TestARaceStartedLateSaysTheLapsToGo(t *testing.T) {
	late := lapRace
	late.Lap, late.TotalLaps = 3, 12
	r := newRule()
	r.Evaluate(frame(0, lapRaceKind, late))
	check(t, r.Evaluate(frame(2, lapRaceKind, late)), rules.Callout{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		LapsLeft: proto.Int32(10), Position: proto.Int32(10), TotalCars: proto.Int32(20),
	}})

	late.Lap, late.Position = 13, -1
	r = newRule()
	r.Evaluate(frame(0, lapRaceKind, late))
	check(t, r.Evaluate(frame(2, lapRaceKind, late)))
}

// R1: before the kind is known the field is GT7's flag and more than one car; a
// lone car with no lap total and no track has nothing to brief.
func TestNothingToSayIsNoBrief(t *testing.T) {
	r := newRule()
	alone := race.View{Position: 1, TotalCars: 1}
	r.Evaluate(frame(0, unknownKind, alone))
	check(t, r.Evaluate(frame(2, unknownKind, alone)))

	r = newRule()
	field := race.View{Position: 4, TotalCars: 8, CarsOnTrack: true}
	r.Evaluate(frame(0, unknownKind, field))
	check(t, r.Evaluate(frame(2, unknownKind, field)), rules.Callout{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Timed: true, Position: proto.Int32(4), TotalCars: proto.Int32(8),
	}})
}

// Reset: a start briefs again (SUGO's restarts, the qualifying restart).
func TestAStartBriefsAgain(t *testing.T) {
	r := newRule()
	r.Evaluate(frame(0, lapRaceKind, lapRace))
	if len(r.Evaluate(frame(2, lapRaceKind, lapRace))) != 1 {
		t.Fatal("no brief")
	}
	r.Reset()
	r.Evaluate(frame(100, lapRaceKind, lapRace))
	if len(r.Evaluate(frame(102, lapRaceKind, lapRace))) != 1 {
		t.Error("no brief after the reset")
	}
}

// R6: the rules file's settings are read over v1's defaults; v1's track
// settings still load, and do nothing.
func TestTheSettingsAreV1s(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       config
	}{
		{"defaults", "", defaults()},
		{"set", "rules:\n  start_brief:\n    settle_secs: 3\n    announce_track: false\n    late_track_first_lap_only: false\n    min_confidence: confirmed\n",
			config{SettleSecs: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := rules.ParseConfig([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			s := cfg.Rules[Name]
			r, ok := build(s).(*rule)
			if err := s.Err(); err != nil || !ok {
				t.Fatal(err)
			}
			if r.cfg != tc.want {
				t.Errorf("got %+v, want %+v", r.cfg, tc.want)
			}
			if s.Priority != 8 || s.PitLane != eventsv1.PitLane_PIT_LANE_HOLD || s.HoldSecs != 0 || s.MaxAgeSecs != 0 {
				t.Errorf("common settings %+v, want v1's: priority 8, hold, no hold_secs, the queue's max age", s)
			}
		})
	}
}

// The ranked kind (the maintainer's decision, 2026-10-31): a ranked session's
// brief is template_ranked with the field's size, not "Timed race. P one of
// sixteen.": the place before anyone has a time is no rank.
func TestARankedSessionHasItsOwnBrief(t *testing.T) {
	ranked := race.Kind{Type: eventsv1.SessionType_SESSION_TYPE_RANKED, Field: true}
	r := newRule()
	check(t, r.Evaluate(frame(10, ranked, race.View{Position: 1, TotalCars: 16})))
	check(t, r.Evaluate(frame(12, ranked, race.View{Position: 1, TotalCars: 16})),
		rules.Callout{Kind: KindRanked, Slots: &eventsv1.CalloutSlots{TotalCars: proto.Int32(16)}})

	r.Reset() // a timed race is still v1's
	r.Evaluate(frame(10, timedKind, timedRace))
	check(t, r.Evaluate(frame(12, timedKind, timedRace)), rules.Callout{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Timed: true, Position: proto.Int32(1), TotalCars: proto.Int32(20),
	}})
}
