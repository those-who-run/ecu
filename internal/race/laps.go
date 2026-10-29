package race

import (
	"strconv"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/pit"
)

// The lap model (paddock#83): what each lap was, decided once, here, for every
// rule and every subscriber. A lap closes where v1's SessionStore stored one
// (observe and closeLap in machine.go, v1's observe and _flush_lap): on the lap
// counter's bump, seen against the last driving frame across a pause or a
// loading screen, once per lap number. GT7 posts the lap's time on the frame of
// the bump (every real recording, 2026-10-17), so the bump is also where v1's
// lap_time rule took the time (lap_time_posted, posted_lap): on all 25
// recordings each complete lap v1 stored was posted on its bump frame, with the
// time v1 stored. The one post without a bump is SUGO's replay jumping to the
// results, a time for a lap v1 never stored, so it closes nothing here either.
//
// Each lap.completed carries the lap's pit role, read from the pit detector's
// status as last applied to the session, as v1's store and rules read it, and
// whether it counts for the session's best, as v1's lap_time rule decided. The
// session's best is one thing: lap.best gives each change of it, and
// race.finished and the summary give it as the last lap.best did (contracts
// v0.2.3), so none of them can disagree.

// timedLap is a complete lap with a time, as the best lap and the total time read
// it.
type timedLap struct {
	lap    int32
	ms     int32
	counts bool // it counts for the session's best (LapCompleted's counts_for_best)
}

// lapBook is the session's laps as the best lap and the total time read them,
// with GT7's own best and the pit status the laps' roles come from.
type lapBook struct {
	timed     []timedLap // the complete laps with a time, in the order they closed
	gt7BestMs int32      // GT7's own best lap, 0 for none (v1's Session.gt7_best_ms)
	// best is the session's best as its last lap.best gave it, nil before one.
	best *timedLap
	// pits is the pit detector's status as last applied to the session: on each
	// entry, exit or entry withdrawn, and at a race's end (v1's Session.pits,
	// SessionStore.set_pits, and each rule's pits). The detector can reset itself
	// inside a session without telling (the lap counter going back without a
	// restart the machine sees), which v1's store and rules never saw.
	pits pit.Status
}

// countsForBest is v1's lap_time rule's `counts` for the lap that s, the frame
// that posted its time, closed: a timed lap that had nothing to do with the pits,
// unless it is a start lap GT7 leaves out of its best. GT7 leaves out lap 1 of a
// session with a lap total (the grid is ahead of the line: 57.1 s against 60 s
// laps at Suzuka East, 2026-10-15), and lap 1 of a timed session when it posts
// the time with its own best still blank (a timed race from a grid, Suzuka East
// 2026-10-16); qualifying, out of the pits onto the line, counts it.
func countsForBest(lap int32, s *state, role pit.Role) bool {
	if s.lastLapMs <= 0 || role != pit.RoleNone {
		return false
	}
	startLap := lap == 1 && (s.totalLaps > 0 || s.bestLapMs <= 0)
	return !startLap
}

// pitRole is the contracts' PitRole for a lap's role.
func pitRole(r pit.Role) eventsv1.PitRole {
	switch r {
	case pit.RoleIn:
		return eventsv1.PitRole_PIT_ROLE_IN_LAP
	case pit.RoleOut:
		return eventsv1.PitRole_PIT_ROLE_OUT_LAP
	case pit.RoleInOut:
		return eventsv1.PitRole_PIT_ROLE_IN_AND_OUT
	default:
		return eventsv1.PitRole_PIT_ROLE_NONE
	}
}

// currentBest is the session's best lap, as v1's results.best_lap chose it with
// one difference (R3): GT7's own best when it has one and a lap of this session
// has that time, the first such lap, else the quickest lap that counts for the
// best, the first of a tie. GT7 keeps its best across sessions at a track
// (Circuit Gilles Villeneuve's 1:37.524 of the race, still there in the pit-stop
// session after it, 2026-10-17), so a best on no lap seen here is not this
// session's. The difference: without GT7's best, v1 fell back to the quickest
// complete lap, pit laps included, leaving out lap 1 of a lap race only when
// another lap was timed; here a pit lap, and a lap that does not count, is never
// a best but by GT7's own. On v1's 25 recordings that differs in two sessions,
// each with lap 1 its only timed lap (the 24 h race and the first qualifying of
// 2026-10-14), where v1 stored lap 1 and ecu has no best; contracts v0.2.3 says
// so, and the parity harness maps it.
func (b *lapBook) currentBest() (timedLap, bool) {
	if l, ok := b.gt7Lap(); ok {
		return l, true
	}
	var best timedLap
	for _, l := range b.timed {
		if l.counts && (best.ms == 0 || l.ms < best.ms) {
			best = l
		}
	}
	return best, best.ms != 0
}

// gt7Lap is the first lap of the session timed at GT7's own best, with GT7's time.
func (b *lapBook) gt7Lap() (timedLap, bool) {
	if b.gt7BestMs <= 0 {
		return timedLap{}, false
	}
	for _, l := range b.timed {
		if l.ms == b.gt7BestMs {
			return l, true
		}
	}
	return timedLap{}, false
}

// totalTimeMs is the race's time, the sum of its complete laps' times (v1's
// summary total_time_ms); false when no lap was timed.
func (b *lapBook) totalTimeMs() (int32, bool) {
	var total int32
	for _, l := range b.timed {
		total += l.ms
	}
	return total, len(b.timed) > 0
}

// fastestMs is the quickest complete lap of any kind, lap 1 from a grid and pit
// laps too (v1's summary fastest_lap_ms); false when no lap was timed.
func (b *lapBook) fastestMs() (int32, bool) {
	var fastest int32
	for _, l := range b.timed {
		if fastest == 0 || l.ms < fastest {
			fastest = l.ms
		}
	}
	return fastest, fastest != 0
}

// lapSummary puts the lap model's best, fastest and GT7's best in the session's
// summary, as v1's SessionStore.end_session stored best_lap_ms, fastest_lap_ms and
// gt7_best_lap_ms; the best is the last lap.best's.
func lapSummary(summary *eventsv1.SessionSummary, book *lapBook) {
	if best := book.best; best != nil {
		summary.BestLap, summary.BestLapTimeMs = &best.lap, &best.ms
	}
	if fastest, ok := book.fastestMs(); ok {
		summary.FastestLapMs = &fastest
	}
	if book.gt7BestMs > 0 {
		gt7 := book.gt7BestMs
		summary.GameBestLapMs = &gt7
	}
}

// publishBest publishes lap.best when the session's best changed with the end of
// lap ended, whose last frame is at: a lap that counts beat the best, or GT7's own
// best moved to a lap of the session. It is published once per ended lap at
// most, as of that lap's end, so its id is <sid>:lap.best:<ended>.
func (m *Machine) publishBest(ended int32, at uint32, t time.Time) {
	sess := m.session
	book := &sess.laps
	best, ok := book.currentBest()
	if !ok || (book.best != nil && best.lap == book.best.lap && best.ms == book.best.ms) {
		return
	}
	ev := &eventsv1.LapBestChanged{
		SessionId: sess.id,
		Lap:       best.lap,
		LapTimeMs: best.ms,
		At:        at,
		EndedLap:  ended,
	}
	if book.best != nil {
		previous, gap := book.best.ms, book.best.ms-best.ms
		ev.PreviousBestMs, ev.GapMs = &previous, &gap
	}
	book.best = &best
	m.out = append(m.out, Output{
		Event:   m.event(sess.id+":lap.best:"+strconv.Itoa(int(ended)), t),
		Session: sess.id,
		Best:    ev,
	})
}
