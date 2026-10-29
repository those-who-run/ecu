package rules

import (
	"testing"

	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/track"
)

var (
	sugo = &track.Match{TrackID: 9, Name: "Sportsland SUGO", Confidence: track.Probable}
	n24h = &track.Match{TrackID: 4, Name: "Nurburgring 24h", Confidence: track.Probable}
	spa  = &track.Match{Name: "Circuit de Spa-Francorchamps", Family: "Circuit de Spa-Francorchamps", Confidence: track.Ambiguous}
)

// lapRace is a frame of a 2-lap race with a field, P10 of 20, the kind known.
func lapRace(t float64) *StartBriefFrame {
	return &StartBriefFrame{
		T: t, Lap: 1, TotalLaps: 2, Position: 10, TotalCars: 20, CarsOnTrack: true,
		Kind: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, Field: true, KindCarsOnTrack: true,
	}
}

// timedField is a frame of a timed race with a field, P1 of 20, the kind known.
func timedField(t float64) *StartBriefFrame {
	return &StartBriefFrame{
		T: t, Lap: 1, Position: 1, TotalCars: 20, CarsOnTrack: true,
		Kind: eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD, Field: true, KindCarsOnTrack: true,
	}
}

func check(t *testing.T, got []StartBriefCall, want ...StartBriefCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d calls %v, want %v", len(got), got, want)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || !proto.Equal(got[i].Slots, want[i].Slots) {
			t.Errorf("call %d: got %s %v, want %s %v", i, got[i].Kind, got[i].Slots, want[i].Kind, want[i].Slots)
		}
	}
}

// R1: the brief waits settle_secs after the first evaluated frame, then names the
// track, the distance and the grid slot, once ("Sportsland SUGO. Two laps. P ten
// of twenty.").
func TestTheBriefIsSaidOnceAfterTheSettle(t *testing.T) {
	r := NewStartBrief(DefaultStartBriefConfig())
	for _, at := range []float64{10, 11, 11.99} {
		f := lapRace(at)
		f.Track = sugo
		check(t, r.Evaluate(f))
	}
	f := lapRace(12)
	f.Track = sugo
	check(t, r.Evaluate(f), StartBriefCall{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Track: "Sportsland SUGO", Laps: proto.Int32(2), Position: proto.Int32(10), TotalCars: proto.Int32(20),
	}})
	check(t, r.Evaluate(lapRace(13)))
}

// R1, AC2: a timed race with a field and no track yet is "Timed race. P one of
// twenty."; the track found on lap 1 is named on its own, a confidence upgrade of
// it is not news, and another track found once lap 2 is under way is too late.
func TestATrackFoundAfterTheBriefIsNamedOnLapOne(t *testing.T) {
	r := NewStartBrief(DefaultStartBriefConfig())
	check(t, r.Evaluate(timedField(0)))
	check(t, r.Evaluate(timedField(2)), StartBriefCall{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Timed: true, Position: proto.Int32(1), TotalCars: proto.Int32(20),
	}})
	f := timedField(5)
	f.Track = n24h
	check(t, r.Evaluate(f), StartBriefCall{Kind: KindBriefTrack, Slots: &eventsv1.CalloutSlots{Track: "Nurburgring 24h"}})
	sure := *n24h
	sure.Confidence = track.Confirmed
	f = timedField(6)
	f.Track = &sure
	check(t, r.Evaluate(f))
	f = timedField(7)
	f.Lap, f.Track = 2, sugo
	check(t, r.Evaluate(f))
}

// R2: late_track_first_lap_only false lifts the first-lap limit.
func TestTheFirstLapLimitCanBeLifted(t *testing.T) {
	cfg := DefaultStartBriefConfig()
	cfg.LateTrackFirstLapOnly = false
	r := NewStartBrief(cfg)
	r.Evaluate(timedField(0))
	r.Evaluate(timedField(2))
	f := timedField(100)
	f.Lap, f.Track = 3, sugo
	check(t, r.Evaluate(f), StartBriefCall{Kind: KindBriefTrack, Slots: &eventsv1.CalloutSlots{Track: "Sportsland SUGO"}})
}

// R2: announce_track false keeps a late track quiet.
func TestALateTrackCanBeKeptQuiet(t *testing.T) {
	cfg := DefaultStartBriefConfig()
	cfg.AnnounceTrack = false
	r := NewStartBrief(cfg)
	r.Evaluate(timedField(0))
	r.Evaluate(timedField(2))
	f := timedField(3)
	f.Track = sugo
	check(t, r.Evaluate(f))
}

// R3, R5, AC1, AC3, AC4: a session joined part way is "Back with you. Lap
// {lap}.", with the lap total in a lap race and the place with a field, and no
// track or grid slot, even with the track known.
func TestAJoinedSessionIsWelcomedBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame *StartBriefFrame
		want  *eventsv1.CalloutSlots
	}{
		{"practice", &StartBriefFrame{Lap: 7, TotalCars: 1, Kind: eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED},
			&eventsv1.CalloutSlots{Lap: proto.Int32(7)}},
		{"timed race", &StartBriefFrame{Lap: 7, Position: 1, TotalCars: 17, CarsOnTrack: true},
			&eventsv1.CalloutSlots{Lap: proto.Int32(7), Position: proto.Int32(1), TotalCars: proto.Int32(17)}},
		{"lap race", &StartBriefFrame{Lap: 5, TotalLaps: 12, Position: 7, TotalCars: 20, CarsOnTrack: true},
			&eventsv1.CalloutSlots{Lap: proto.Int32(5), Laps: proto.Int32(12), Position: proto.Int32(7), TotalCars: proto.Int32(20)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewStartBrief(DefaultStartBriefConfig())
			f := *tc.frame
			f.Joined, f.Track = true, sugo
			check(t, r.Evaluate(&f))
			f.T = 2
			check(t, r.Evaluate(&f), StartBriefCall{Kind: KindBriefJoined, Slots: tc.want})
			f.T, f.Track = 3, n24h
			check(t, r.Evaluate(&f))
		})
	}
}

// AC5: a circuit whose layout can't be picked is named by its circuit, as GT7
// writes it; a solo session with no lap total says nothing else.
func TestTheCircuitIsNamedWhenTheLayoutIsNot(t *testing.T) {
	r := NewStartBrief(DefaultStartBriefConfig())
	f := &StartBriefFrame{T: 0, Lap: 1, TotalCars: 1, Kind: eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED, Track: spa}
	check(t, r.Evaluate(f))
	f.T = 2
	check(t, r.Evaluate(f), StartBriefCall{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{Track: "Circuit de Spa-Francorchamps"}})
}

// R1: a layout below min_confidence isn't spoken, and a layout with no name is
// "track {id}", as v1 named it.
func TestOnlyASureLayoutIsSpoken(t *testing.T) {
	unsure := &track.Match{TrackID: 9, Name: "Sportsland SUGO", Confidence: track.Ambiguous}
	r := NewStartBrief(DefaultStartBriefConfig())
	f := &StartBriefFrame{T: 0, Lap: 1, TotalCars: 1, Track: unsure}
	r.Evaluate(f)
	f.T = 2
	check(t, r.Evaluate(f))

	r = NewStartBrief(DefaultStartBriefConfig())
	f = &StartBriefFrame{T: 0, Lap: 1, TotalCars: 1, Track: &track.Match{TrackID: 77, Confidence: track.Confirmed}}
	r.Evaluate(f)
	f.T = 2
	check(t, r.Evaluate(f), StartBriefCall{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{Track: "track 77"}})
}

// R4: a track ruled out is forgotten: the brief doesn't name it, and a name
// waiting to be said is dropped.
func TestATrackRuledOutIsForgotten(t *testing.T) {
	gone := &track.Match{Confidence: track.Ambiguous}
	r := NewStartBrief(DefaultStartBriefConfig())
	f := lapRace(0)
	f.Track = sugo
	r.Evaluate(f)
	f = lapRace(2)
	f.Track = gone
	check(t, r.Evaluate(f), StartBriefCall{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Laps: proto.Int32(2), Position: proto.Int32(10), TotalCars: proto.Int32(20),
	}})

	// Pending on the frame the verdict is ruled out: v1 cleared it in on_track.
	r = NewStartBrief(DefaultStartBriefConfig())
	r.Evaluate(lapRace(0))
	r.Evaluate(lapRace(2))
	r.onTrack(sugo, false)
	r.onTrack(gone, false)
	check(t, r.Evaluate(lapRace(3)))
}

// R1: a race first seen past lap 1, not joined, says the laps to go.
func TestARaceStartedLateSaysTheLapsToGo(t *testing.T) {
	r := NewStartBrief(DefaultStartBriefConfig())
	f := lapRace(0)
	f.Lap, f.TotalLaps = 3, 12
	r.Evaluate(f)
	f.T = 2
	check(t, r.Evaluate(f), StartBriefCall{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		LapsLeft: proto.Int32(10), Position: proto.Int32(10), TotalCars: proto.Int32(20),
	}})
}

// R1: before the kind is known the field is GT7's flag and more than one car; a
// lone car with no lap total and no track has nothing to brief.
func TestNothingToSayIsNoBrief(t *testing.T) {
	r := NewStartBrief(DefaultStartBriefConfig())
	f := &StartBriefFrame{Lap: 1, Position: 1, TotalCars: 1}
	r.Evaluate(f)
	f.T = 2
	check(t, r.Evaluate(f))

	r = NewStartBrief(DefaultStartBriefConfig())
	f = &StartBriefFrame{Lap: 1, Position: 4, TotalCars: 8, CarsOnTrack: true}
	r.Evaluate(f)
	f.T = 2
	check(t, r.Evaluate(f), StartBriefCall{Kind: KindBrief, Slots: &eventsv1.CalloutSlots{
		Timed: true, Position: proto.Int32(4), TotalCars: proto.Int32(8),
	}})
}

// Reset: a start briefs again (SUGO's restarts and qualifying restart).
func TestAStartBriefsAgain(t *testing.T) {
	r := NewStartBrief(DefaultStartBriefConfig())
	r.Evaluate(lapRace(0))
	if len(r.Evaluate(lapRace(2))) != 1 {
		t.Fatal("no brief")
	}
	r.Reset()
	r.Evaluate(lapRace(100))
	if len(r.Evaluate(lapRace(102))) != 1 {
		t.Error("no brief after the reset")
	}
}
