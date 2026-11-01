package race

import (
	"testing"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

func TestDerivePhase(t *testing.T) {
	cases := []struct {
		name            string
		lap, total      int32
		paused, loading bool
		want            telemetryv1.Phase
	}{
		{"loading wins over everything", 3, 2, true, true, phaseLoading},
		{"loading in the menus", -1, 0, false, true, phaseLoading},
		{"menus", -1, 0, false, false, phaseMenu},
		{"menus while paused", -1, 0, true, false, phaseMenu},
		{"paused", 2, 5, true, false, phasePaused},
		{"paused past the flag", 6, 5, true, false, phasePaused},
		{"the flag: lap 3 of 2", 3, 2, false, false, phaseFinished},
		{"the last lap", 2, 2, false, false, phaseInRace},
		{"a timed race never finishes by laps", 9, 0, false, false, phaseInRace},
		{"lap 1", 1, 0, false, false, phaseInRace},
		{"the track screen", 0, 2, false, false, phaseAtTrack},
	}
	for _, c := range cases {
		if got := derivePhase(c.lap, c.total, c.paused, c.loading); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// st is a frame's state for the helpers: its phase and lap.
func st(phase telemetryv1.Phase, lap int32) *state { return &state{phase: phase, lap: lap} }

type transition struct {
	name       string
	prev, next *state
	want       bool
}

func check(t *testing.T, helper func(prev, s *state) bool, cases []transition) {
	t.Helper()
	for _, c := range cases {
		if got := helper(c.prev, c.next); got != c.want {
			t.Errorf("%s: got %t, want %t", c.name, got, c.want)
		}
	}
}

func TestRacingFrame(t *testing.T) {
	check(t, racingFrame, []transition{
		{"no frame before", nil, st(phaseInRace, 1), false},
		{"driving on", st(phaseInRace, 1), st(phaseInRace, 1), true},
		{"the flag", st(phaseInRace, 2), st(phaseFinished, 3), true},
		{"from the track screen", st(phaseAtTrack, 0), st(phaseInRace, 1), false},
		{"into a pause", st(phaseInRace, 1), st(phasePaused, 1), false},
		{"out of a pause", st(phasePaused, 1), st(phaseInRace, 1), false},
	})
}

func TestLapCompleted(t *testing.T) {
	check(t, lapCompleted, []transition{
		{"no frame before", nil, st(phaseInRace, 2), false},
		{"a crossing", st(phaseInRace, 1), st(phaseInRace, 2), true},
		{"the flag", st(phaseInRace, 2), st(phaseFinished, 3), true},
		{"no crossing", st(phaseInRace, 2), st(phaseInRace, 2), false},
		{"two laps at once", st(phaseInRace, 1), st(phaseInRace, 3), false},
		{"lap 0 to 1 is the start", st(phaseInRace, 0), st(phaseInRace, 1), false},
		{"after a loading frame", st(phaseLoading, 4), st(phaseInRace, 4), false},
	})
}

func TestRaceStarted(t *testing.T) {
	check(t, raceStarted, []transition{
		{"the first frame, driving", nil, st(phaseInRace, 1), true},
		{"the first frame, joined part way", nil, st(phaseInRace, 7), true},
		{"the first frame, in the menus", nil, st(phaseMenu, -1), false},
		{"from the track screen", st(phaseAtTrack, 0), st(phaseInRace, 1), true},
		{"from the menus", st(phaseMenu, -1), st(phaseInRace, 1), true},
		{"the replay blip after the results", st(phaseAtTrack, 0), st(phaseInRace, 6), false},
		{"out of a loading screen", st(phaseLoading, 0), st(phaseInRace, 1), false},
		{"out of a pause", st(phasePaused, 0), st(phaseInRace, 1), false},
		{"driving on", st(phaseInRace, 1), st(phaseInRace, 1), false},
		{"not driving", st(phaseAtTrack, 0), st(phaseAtTrack, 0), false},
	})
}

func TestRaceEnded(t *testing.T) {
	check(t, raceEnded, []transition{
		{"no frame before", nil, st(phaseMenu, -1), false},
		{"the flag", st(phaseInRace, 2), st(phaseFinished, 3), true},
		{"to the menus", st(phaseInRace, 2), st(phaseMenu, -1), true},
		{"to the track screen", st(phaseInRace, 2), st(phaseAtTrack, 0), true},
		{"quit from the pause menu", st(phasePaused, 2), st(phaseLoading, 0), true},
		{"a loading screen with the lap reset", st(phaseLoading, 2), st(phaseLoading, 0), true},
		{"a loading blip keeps the lap", st(phaseInRace, 3), st(phaseLoading, 4), false},
		{"into a pause", st(phaseInRace, 2), st(phasePaused, 2), false},
		{"before lap 1", st(phaseInRace, 0), st(phaseMenu, -1), false},
		{"from the track screen", st(phaseAtTrack, 0), st(phaseMenu, -1), false},
		{"from past the flag", st(phaseFinished, 3), st(phaseMenu, -1), false},
	})
}

func TestRaceRestarted(t *testing.T) {
	check(t, raceRestarted, []transition{
		{"no frame before", nil, st(phaseInRace, 1), false},
		{"back to lap 1", st(phaseInRace, 3), st(phaseInRace, 1), true},
		{"back to lap 1 from the pause menu", st(phasePaused, 2), st(phaseInRace, 1), true},
		{"lap 1 to lap 1", st(phaseInRace, 1), st(phaseInRace, 1), false},
		{"through a loading screen", st(phaseLoading, 2), st(phaseInRace, 1), false},
		{"to lap 0", st(phaseInRace, 2), st(phaseInRace, 0), false},
		{"forwards", st(phaseInRace, 1), st(phaseInRace, 2), false},
	})
}

func TestJoinedPartWay(t *testing.T) {
	check(t, joinedPartWay, []transition{
		{"the first frame on lap 7", nil, st(phaseInRace, 7), true},
		{"the first frame on lap 1", nil, st(phaseInRace, 1), false},
		{"the first frame in the menus", nil, st(phaseMenu, -1), false},
		{"a frame before", st(phaseAtTrack, 0), st(phaseInRace, 2), false},
	})
}

func TestRacePosition(t *testing.T) {
	for in, want := range map[int32]int32{-1: 0, 0: 0, 1: 1, 16: 16} {
		if got := (&state{position: in}).racePosition(); got != want {
			t.Errorf("position %d: got %d, want %d", in, got, want)
		}
	}
}
