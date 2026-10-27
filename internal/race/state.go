package race

import (
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/salsa"
)

// The phases, by the contract's names for them.
const (
	phaseMenu     = telemetryv1.Phase_PHASE_MENU
	phaseLoading  = telemetryv1.Phase_PHASE_LOADING
	phaseAtTrack  = telemetryv1.Phase_PHASE_AT_TRACK
	phaseInRace   = telemetryv1.Phase_PHASE_IN_RACE
	phasePaused   = telemetryv1.Phase_PHASE_PAUSED
	phaseFinished = telemetryv1.Phase_PHASE_FINISHED
)

// state is what the transition helpers read from a frame: v1's RaceState, cut to
// the fields they and the lap bookkeeping use.
type state struct {
	seq         uint32
	phase       telemetryv1.Phase
	lap         int32 // GT7's current lap
	totalLaps   int32
	lastLapMs   int32
	fuel        float64
	position    int32 // GT7's race position, -1 outside a race
	carsOnTrack bool
	carCode     int32
}

func (s *state) inRace() bool { return s.phase == phaseInRace }

// racePosition is v1's RaceState.position: the live race position, or none (0)
// outside a race, where GT7 sends -1.
func (s *state) racePosition() int32 {
	if s.position > 0 {
		return s.position
	}
	return 0
}

func newState(seq uint32, p *salsa.Packet) state {
	lap, total := int32(p.CurrentLap), int32(p.TotalLaps)
	return state{
		seq:         seq,
		phase:       derivePhase(lap, total, p.Flags.Paused(), p.Flags.Loading()),
		lap:         lap,
		totalLaps:   total,
		lastLapMs:   p.LastLapMs,
		fuel:        float64(p.FuelLevel),
		position:    int32(p.RacePosition),
		carsOnTrack: p.Flags.CarsOnTrack(),
		carCode:     p.CarCode,
	}
}

// The helpers below are v1's, from src/those_who_run/state.py at v1.0.0, ported as
// they are. Their comments are v1's docstrings: each records what a recording
// showed.

// derivePhase maps GT7's lap counter and flag bits onto a phase.
//
// Observed from a real session (2026-10-14): current_lap is -1 in menus, 0 at a
// track before the start, >= 1 while driving, and total_laps + 1 once the
// chequered flag falls. Time trials and practice report total_laps == 0 and GT7's
// "cars on track" flag false, so the flag is not used to decide whether laps are
// being driven. After the results screen GT7 replays the final frames of the race
// for a moment; those frames are FINISHED because the lap counter is past the
// total.
func derivePhase(currentLap, totalLaps int32, paused, loading bool) telemetryv1.Phase {
	switch {
	case loading:
		return phaseLoading
	case currentLap < 0:
		return phaseMenu
	case paused:
		return phasePaused
	case totalLaps > 0 && currentLap > totalLaps:
		return phaseFinished
	case currentLap >= 1:
		return phaseInRace
	default:
		return phaseAtTrack
	}
}

// racingFrame says s continues a race from prev, including the finish-line frame
// where the phase flips to FINISHED on the same packet as the final lap time.
func racingFrame(prev, s *state) bool {
	return prev != nil && prev.inRace() && (s.phase == phaseInRace || s.phase == phaseFinished)
}

// lapCompleted says the lap counter advanced by one while driving.
func lapCompleted(prev, s *state) bool {
	return racingFrame(prev, s) && prev.lap >= 1 && s.lap == prev.lap+1
}

// raceStarted says s is the first driving frame of a session. Loading screens and
// pauses are not starts, and neither are the replayed frames GT7 shows after the
// results screen: those come from the track screen carrying the finished race's
// lap counter (lap 6 after a five-lap timed race, 2026-10-16), while a real start
// is on lap 1.
func raceStarted(prev, s *state) bool {
	if !s.inRace() {
		return false
	}
	if prev == nil {
		return true
	}
	return (prev.phase == phaseMenu || prev.phase == phaseAtTrack) && prev.lap < 1 && s.lap <= 1
}

// raceEnded says the session is over: chequered flag, back to a menu or the track
// screen, or a loading screen with the lap counter reset (quitting from the pause
// menu goes IN_RACE -> PAUSED -> LOADING(lap 0) -> AT_TRACK, seen 2026-10-14). A
// loading blip that keeps the lap counter only suspends the race.
func raceEnded(prev, s *state) bool {
	if prev == nil || (prev.phase != phaseInRace && prev.phase != phasePaused && prev.phase != phaseLoading) {
		return false
	}
	if prev.lap < 1 {
		return false
	}
	if s.phase == phaseMenu || s.phase == phaseAtTrack || s.phase == phaseFinished {
		return true
	}
	return s.phase == phaseLoading && s.lap < 1
}

// raceRestarted says GT7 jumped straight back to lap 1 while driving: the restart
// option in the pause menu (seen in qualifying 2026-10-14, with the fuel refilled
// too). A race restart, on lap 1 as on lap 2, goes through a loading screen at lap
// 0 instead (SUGO 2026-10-18, #157), which raceEnded ends the session on; so a
// restart on lap 1 never reads lap 1 -> lap 1.
func raceRestarted(prev, s *state) bool {
	return prev != nil &&
		(prev.phase == phaseInRace || prev.phase == phasePaused) &&
		s.inRace() &&
		s.lap >= 1 && s.lap < prev.lap
}

// joinedPartWay is v1's engine.joined_part_way: the engineer started part way
// through a session (an upgrade, an OOM, a node reboot; Suzuka 2026-10-18, lap 7 at
// 124 km/h, #265): its first frame is a race frame past lap 1 with nothing before
// it. A start seen from the grid, the track screen or a loading screen has a frame
// before it. Joined on lap 1 it cannot be told from a flying start, which crosses
// the line at speed with nothing before it either when the engineer comes up on
// that frame, so it reads as a start.
func joinedPartWay(prev, s *state) bool {
	return prev == nil && s.inRace() && s.lap > 1
}
