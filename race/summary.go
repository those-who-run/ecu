package race

import (
	"math"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/pit"
)

// The session summary (paddock#86): v1's summary.json, which SessionStore.end_session
// built from what the engine had told the store all session long. ecu keeps it as
// it goes instead, a running summary in the open session (session.summary), updated
// on each frame the session takes and with each event it publishes: the start, a
// lap, a pit change, the track and the kind. Ending the session is then adding
// what only the end knows (ended_at, ended_paused, the finish and the end frame's
// own figures) and publishing it. The running summary is the contracts'
// SessionSummary itself, which the KV's OpenSession.summary carries (continue.go,
// paddock#76). Reading it back is more than taking that value: some of its fields
// are written each frame from the session's own (session's comment says which),
// and continue.go rebuilds those from it first.

// minStintFuel is v1's MIN_STINT_FUEL: a stint that burnt less is left out of
// fuel_burned_by_stint (a stop straight after another, a stint the flag cut short).
const minStintFuel = 1.0

// fuelStints is v1's FuelStints (stints.py): the fuel burnt, stint by stint, from
// the level frame by frame. Every drop goes to the current stint, so a refuel never
// cancels what was burnt before it, and a stint ends where the pit detector says a
// stop was done, not where the level goes up.
type fuelStints struct {
	stints []float64 // the fuel burnt in each, from [0]
	last   float64
	seen   bool // last holds a level
}

func newFuelStints() fuelStints { return fuelStints{stints: []float64{0}} }

// observe takes a frame's fuel level and gives the drop since the last frame, 0
// when it did not fall (v1's FuelStints.observe).
func (f *fuelStints) observe(level float64) float64 {
	last, seen := f.last, f.seen
	f.last, f.seen = level, true
	if !seen || level >= last {
		return 0
	}
	f.stints[len(f.stints)-1] += last - level
	return last - level
}

// onPit starts a stint for each stop the pit detector has seen done (v1's
// FuelStints.on_pit).
func (f *fuelStints) onPit(status pit.Status) {
	for int32(len(f.stints)) < status.Stint() { //nolint:gosec // a session's stints fit
		f.stints = append(f.stints, 0)
	}
}

// used is the fuel of each stint that burnt minStintFuel or more, rounded (v1's
// fuel_burned_by_stint); all is every stint's (v1's fuel_stints).
func (f *fuelStints) used() (used, all []float64) {
	all = make([]float64, len(f.stints))
	for i, x := range f.stints {
		all[i] = round3(x)
		if x >= minStintFuel {
			used = append(used, round3(x))
		}
	}
	return used, all
}

// openSummary is the running summary of a session that opens on s, from its
// session.started.
func openSummary(started *eventsv1.SessionStarted, s *state) *eventsv1.SessionSummary {
	return &eventsv1.SessionSummary{
		SessionType:  started.GetSessionType(),
		StartedAt:    started.GetStartedAt(),
		CarCode:      started.GetCarCode(),
		CarName:      started.GetCarName(),
		TotalLaps:    started.TotalLaps,
		JoinedOnLap:  started.JoinedOnLap,
		FuelAtStart:  started.GetFuelAtStart(),
		FuelAtEnd:    s.fuel,
		FuelCapacity: s.fuelCapacity,
		Stints:       1,
		Gaps:         []*eventsv1.SessionGap{},
	}
}

// sawFrame brings the running summary up to s, a frame the open session took:
// the fuel burnt, the last position and field when s is driven (v1's
// SessionStore.observe), and the time driven and paused, the fuel and the tank
// as of s.
func (m *Machine) sawFrame(prev, s *state) {
	sess := m.session
	sum := sess.summary
	if s.inRace() || racingFrame(prev, s) {
		sess.fuelBurned += sess.stints.observe(s.fuel)
		if s.totalCars > 0 {
			sess.lastCars = s.totalCars
		}
	}
	m.summaryAsOf(s)
	sum.FuelBurned = round3(sess.fuelBurned)
	sum.FuelBurnedByStint, sum.FuelStints = sess.stints.used()
}

// summaryAsOf sets the running summary's figures that are the frame's own, as of
// s: the time driven and paused, the fuel in the tank and its size, the position
// and the field (v1's end_session reads them from the session's last frame).
func (m *Machine) summaryAsOf(s *state) {
	sess := m.session
	sum := sess.summary
	paused := m.pauses.pausedFor(s.t)
	// After an ecu restart, the time driven and paused before it, then this run's
	// (v1's end_session adds before's driving_ms and paused_ms).
	sum.DrivingMs = drivingMillis(s.t-sess.startT-paused) + sess.drivenBefore
	sum.PausedMs = pausedMillis(paused) + sess.pausedBefore
	sum.FuelAtEnd, sum.FuelCapacity = s.fuel, s.fuelCapacity
	sum.TotalCars = nil
	if cars := s.totalCars; cars > 0 {
		sum.TotalCars = &cars
	} else if sess.lastCars > 0 {
		cars := sess.lastCars
		sum.TotalCars = &cars
	}
	final := s.racePosition()
	if final == 0 {
		final = sess.lastPosition
	}
	sum.FinalPosition = nil
	if final != 0 && !solo(m.kinds.kind()) {
		sum.FinalPosition = &final
	}
}

// summaryLap adds a lap the session published to the running summary: the laps,
// the count of complete ones, the fuel used and the best laps.
func (m *Machine) summaryLap(ev *eventsv1.LapCompleted) {
	sess := m.session
	sum := sess.summary
	sum.Laps = append(sum.Laps, proto.CloneOf(ev))
	sum.LapsCompleted = sess.completed
	sum.FuelUsed = round3(sess.lapsFuelUsed)
	sum.BestLap, sum.BestLapTimeMs, sum.FastestLapMs, sum.GameBestLapMs = nil, nil, nil, nil
	lapSummary(sum, &sess.laps)
}

// summaryPits applies the pit detector's view to the running summary, as v1's
// set_pits did: each stop done starts a fuel stint, and the stints and stops are
// the detector's.
func (m *Machine) summaryPits(status pit.Status) {
	sess := m.session
	sess.stints.onPit(status)
	sess.summary.PitStops = nil
	pitSummary(sess.summary, status, sess.pits.before)
	sess.summary.FuelBurnedByStint, sess.summary.FuelStints = sess.stints.used()
}

// endSummary is the summary session.ended carries: the running summary as of s,
// the session's last frame, with what only the end knows. now is when the end was
// decided, the time of the packet that decided it (v1's clock() in end_session).
func (m *Machine) endSummary(s *state, now time.Time, why Why) *eventsv1.SessionSummary {
	sess := m.session
	m.summaryAsOf(s)
	sum := sess.summary
	kind := m.kinds.kind()
	sum.SessionType = kind
	sum.Finished = m.finish.kind != eventsv1.FinishKind_FINISH_KIND_UNSPECIFIED
	sum.TotalTimeMs = nil
	if total, ok := sess.laps.totalTimeMs(); ok && sum.Finished {
		sum.TotalTimeMs = &total
	}
	sum.FinishingPosition = nil
	if sum.FinalPosition != nil && why == WhyFinished && kind == eventsv1.SessionType_SESSION_TYPE_LAP_RACE {
		sum.FinishingPosition = proto.Int32(sum.GetFinalPosition())
	}
	// The pause clock's view of the session (v1's _end_session): whether it ends
	// paused (endPaused), and, ended in a pause, it ended when that pause began,
	// on the frames' clock from this run's first frame (v1's paused_since and
	// start_wall): not from started_at, which is the first run's after a restart.
	sum.EndedPaused = m.endedPaused
	ended := now
	if m.pauses.open {
		ended = minTime(now, sess.startWall.Add(secs(max(m.pauses.sinceT-sess.startT, 0))))
	}
	sum.EndedAt = timestamppb.New(ended)
	return sum
}

// drivingMillis is v1's max(round(secs * 1000), 0): ties to even, never negative.
func drivingMillis(secs float64) int32 {
	return max(int32(math.RoundToEven(secs*1000)), 0) //nolint:gosec // a session's time fits
}

func secs(s float64) time.Duration { return time.Duration(math.Round(s * float64(time.Second))) }

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
