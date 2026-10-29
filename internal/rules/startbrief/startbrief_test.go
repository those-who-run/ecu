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
	gone = &track.Match{Confidence: track.Ambiguous}
)

func newRule() *rule {
	cfg := defaults()
	return &rule{cfg: cfg, minRank: rank(cfg.MinConfidence)}
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
	lapRaceKind = race.Kind{Type: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, Field: true, CarsOnTrack: true}
	timedKind   = race.Kind{Type: eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD, Field: true, CarsOnTrack: true}
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

// R1: the brief waits settle_secs after the first racing frame, then names the
// track, the distance and the grid slot, once ("Sportsland SUGO. Two laps. P ten
// of twenty."). A frame that isn't racing doesn't start the wait.
func TestTheBriefIsSaidOnceAfterTheSettle(t *testing.T) {
	r := newRule()
	check(t, r.Evaluate(&race.Context{Cur: race.View{T: 5}, TrackChanges: []*track.Match{sugo}}))
	check(t, r.Evaluate(frame(10, lapRaceKind, lapRace)))
	check(t, r.Evaluate(frame(11.99, lapRaceKind, lapRace)))
	check(t, r.Evaluate(frame(12, lapRaceKind, lapRace)), rules.Callout{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Track: "Sportsland SUGO", Laps: proto.Int32(2), Position: proto.Int32(10), TotalCars: proto.Int32(20),
	}})
	check(t, r.Evaluate(frame(13, lapRaceKind, lapRace)))
}

// R2, AC2: a timed race with a field and no track yet is "Timed race. P one of
// twenty."; the track found on lap 1 is named on its own, a confidence upgrade of
// it is not news, and another track found once lap 2 is under way is too late.
func TestATrackFoundAfterTheBriefIsNamedOnLapOne(t *testing.T) {
	r := newRule()
	check(t, r.Evaluate(frame(0, timedKind, timedRace)))
	check(t, r.Evaluate(frame(2, timedKind, timedRace)), rules.Callout{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Timed: true, Position: proto.Int32(1), TotalCars: proto.Int32(20),
	}})
	check(t, r.Evaluate(frame(5, timedKind, timedRace, n24h)),
		rules.Callout{Kind: KindTrack, Slots: &eventsv1.CalloutSlots{Track: "Nurburgring 24h"}})
	sure := *n24h
	sure.Confidence = track.Confirmed
	check(t, r.Evaluate(frame(6, timedKind, timedRace, &sure)))
	lap2 := timedRace
	lap2.Lap = 2
	check(t, r.Evaluate(frame(7, timedKind, lap2, sugo)))
}

// R2: two verdicts applied between frames are taken in turn, as v1's on_track
// was called for each: a track named then put back to the one spoken leaves the
// first waiting, and it is said.
func TestEachVerdictAppliedIsTakenInTurn(t *testing.T) {
	r := newRule()
	r.Evaluate(frame(0, lapRaceKind, lapRace, n24h))
	r.Evaluate(frame(2, lapRaceKind, lapRace))
	check(t, r.Evaluate(frame(3, lapRaceKind, lapRace, sugo, n24h)),
		rules.Callout{Kind: KindTrack, Slots: &eventsv1.CalloutSlots{Track: "Sportsland SUGO"}})
}

// R2: late_track_first_lap_only false lifts the first-lap limit; announce_track
// false keeps a late track quiet.
func TestTheLateTrackSettings(t *testing.T) {
	r := newRule()
	r.cfg.FirstLapOnly = false
	r.Evaluate(frame(0, timedKind, timedRace))
	r.Evaluate(frame(2, timedKind, timedRace))
	lap3 := timedRace
	lap3.Lap = 3
	check(t, r.Evaluate(frame(100, timedKind, lap3, sugo)),
		rules.Callout{Kind: KindTrack, Slots: &eventsv1.CalloutSlots{Track: "Sportsland SUGO"}})

	r = newRule()
	r.cfg.AnnounceTrack = false
	r.Evaluate(frame(0, timedKind, timedRace))
	r.Evaluate(frame(2, timedKind, timedRace))
	check(t, r.Evaluate(frame(3, timedKind, timedRace, sugo)))
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

// AC5: a circuit whose layout can't be picked is named by its circuit, as GT7
// writes it; a solo session with no lap total says nothing else.
func TestTheCircuitIsNamedWhenTheLayoutIsNot(t *testing.T) {
	r := newRule()
	r.Evaluate(frame(0, soloKind, solo, spa))
	check(t, r.Evaluate(frame(2, soloKind, solo)),
		rules.Callout{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{Track: "Circuit de Spa-Francorchamps"}})
}

// R1: a layout below min_confidence isn't spoken, and one with no name is "track
// {id}", as v1 named it.
func TestOnlyASureLayoutIsSpoken(t *testing.T) {
	r := newRule()
	r.Evaluate(frame(0, soloKind, solo, &track.Match{TrackID: 9, Name: "Sportsland SUGO", Confidence: track.Ambiguous}))
	check(t, r.Evaluate(frame(2, soloKind, solo)))

	r = newRule()
	r.Evaluate(frame(0, soloKind, solo, &track.Match{TrackID: 77, Confidence: track.Confirmed}))
	check(t, r.Evaluate(frame(2, soloKind, solo)),
		rules.Callout{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{Track: "track 77"}})
}

// R4: a track ruled out is forgotten: the brief doesn't name it, and a name
// waiting to be said is dropped.
func TestATrackRuledOutIsForgotten(t *testing.T) {
	r := newRule()
	r.Evaluate(frame(0, lapRaceKind, lapRace, sugo))
	check(t, r.Evaluate(frame(2, lapRaceKind, lapRace, gone)), rules.Callout{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Laps: proto.Int32(2), Position: proto.Int32(10), TotalCars: proto.Int32(20),
	}})

	r = newRule()
	r.Evaluate(frame(0, lapRaceKind, lapRace))
	r.Evaluate(frame(2, lapRaceKind, lapRace))
	check(t, r.Evaluate(frame(3, lapRaceKind, lapRace, sugo, gone)))
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

// R6: the rules file's settings are read over v1's defaults, and a confidence
// v1 doesn't know is probable, as CONFIDENCE_RANK.get read it.
func TestTheSettingsAreV1s(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       config
		rank       track.Confidence
	}{
		{"defaults", "", defaults(), track.Probable},
		{"set", "rules:\n  start_brief:\n    settle_secs: 3\n    announce_track: false\n    late_track_first_lap_only: false\n    min_confidence: confirmed\n",
			config{SettleSecs: 3, MinConfidence: "confirmed"}, track.Confirmed},
		{"unknown confidence", "rules:\n  start_brief:\n    min_confidence: sure\n",
			config{SettleSecs: 2, AnnounceTrack: true, FirstLapOnly: true, MinConfidence: "sure"}, track.Probable},
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
			if r.cfg != tc.want || r.minRank != tc.rank {
				t.Errorf("got %+v %v, want %+v %v", r.cfg, r.minRank, tc.want, tc.rank)
			}
			if s.Priority != 8 || s.PitLane != eventsv1.PitLane_PIT_LANE_HOLD || s.HoldSecs != 0 || s.MaxAgeSecs != 0 {
				t.Errorf("common settings %+v, want v1's: priority 8, hold, no hold_secs, the queue's max age", s)
			}
		})
	}
	for name, want := range map[string]track.Confidence{"none": track.None, "ambiguous": track.Ambiguous} {
		if got := rank(name); got != want {
			t.Errorf("rank(%q) %v, want %v", name, got, want)
		}
	}
}
