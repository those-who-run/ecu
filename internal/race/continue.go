package race

import (
	"fmt"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	"github.com/those-who-run/ecu/internal/pit"
	"github.com/those-who-run/ecu/internal/track"
)

// Continuing a session after an ecu restart (paddock#76): v1's SessionStore
// continue_mismatch, _continue and release_held (store.py at v1.0.0), and
// Engine._step's carried_track (engine.py), with the session kept in the KV
// instead of v1's store.
//
// While a session is open, the caller keeps Open's record of it in the `ecu`
// bucket, sessions.open.<src> (statev1.OpenSession), and a graceful stop leaves
// it there: no session.ended. A new run gives the machine that record
// (Restore). The run's first start then decides it, as v1's start_session did:
// a session joined part way (paddock#75) within the continue window of when the
// record was last seen, in the same car, type, lap total and field, and not on
// an earlier lap than the record's, continues it, with the same id, a
// session.continued with the gap, and the track carried over; anything else, or
// the window passing first (Expire, once a second, and each Step), ends the
// recorded session from the record alone, END_REASON_ENGINEER_STOPPED. That end
// is Output.Kept: the caller clears the record with a revision check before it
// publishes it, so a second run never ends it again.
//
// What carries on: the laps, the stints and stops (the pit detector starts from
// them, pit.Detector.Resume), the <entry> and <pause> counts the events' ids
// use, the fuel burnt by stint, the time driven and paused before, the kind's
// start, whether race.final_lap went out, and the place the last race.position
// gave. The lap the gap falls in is one lap, not complete, with GT7's time for
// it (closeLap, from the summary's last gap).

// carriedEvidence is v1's evidence for a carried track, and its wording for a
// carried kind's start is the same restart's (kind.go).
const carriedEvidence = "carried over from before the engineer restart"

// kept is the session an earlier run left open, from its KV record, until the
// run's first start or the window decides it.
type kept struct {
	open   *statev1.OpenSession
	window time.Duration
}

// Restore gives the machine the record of the session an earlier ecu run left
// open (sessions.open.<src>) and the continue window
// (TWR_ECU_CONTINUE_WINDOW_SECS; 0 never continues): before the first packet.
// A nil record, or one with no session, is none.
func (m *Machine) Restore(open *statev1.OpenSession, window time.Duration) {
	m.kept = kept{}
	if open.GetSessionId() != "" {
		m.kept = kept{open: open, window: max(window, 0)}
	}
}

// Waiting says the machine holds a record Restore gave it that is not decided
// yet: the caller calls Expire once a second until it is not.
func (m *Machine) Waiting() bool { return m.kept.open != nil }

// Expire is the continue window's timer (R5): once the window has passed since
// the recorded session was last seen, it returns that session's end, Output.Kept,
// on the clock the frames' receive times are on, and the machine waits no more.
// Otherwise it returns nothing.
func (m *Machine) Expire(now time.Time) []Output {
	m.out = nil
	m.expire(now)
	return m.out
}

func (m *Machine) expire(now time.Time) {
	k := m.kept
	if k.open == nil {
		return
	}
	since := now.Sub(k.open.GetLastSeenAt().AsTime())
	if k.window > 0 && since <= k.window {
		return
	}
	m.endKept(now, fmt.Sprintf("%.1f min after it was last seen, over %.1f", since.Minutes(), k.window.Minutes()))
}

// resume decides the recorded session on the run's first start, s: it continues
// it and says so, or ends it and says not, and the caller opens a new session.
func (m *Machine) resume(s *state, t time.Time, joined bool) bool {
	k := m.kept
	if k.open == nil {
		return false
	}
	why := "a session seen from its start"
	if joined {
		why = continueMismatch(k.open, s, t.Sub(k.open.GetLastSeenAt().AsTime()), k.window)
	}
	if why != "" {
		m.endKept(t, why)
		return false
	}
	m.kept = kept{}
	m.continueSession(k.open, s, t)
	return true
}

// continueMismatch is v1's continue_mismatch: why a join on s, since after the
// recorded session was last seen, does not continue it, "" when it does. The type
// is v1's session_type: a race when GT7 flags cars on track, else solo, which the
// lap total tells apart.
func continueMismatch(open *statev1.OpenSession, s *state, since, window time.Duration) string {
	sum := open.GetSummary()
	cars, wasCars := s.totalCars, sum.GetTotalCars()
	wasRace := sum.GetKind().GetCarsOnTrack()
	switch {
	case window <= 0:
		return "the continue window is 0"
	case since > window:
		return fmt.Sprintf("%.1f min after it was last seen, over %.1f", since.Minutes(), window.Minutes())
	case sum.GetCarCode() != s.carCode:
		return "another car"
	case sum.GetKind() != nil && wasRace != s.carsOnTrack:
		return fmt.Sprintf("cars on track %t, not %t", s.carsOnTrack, wasRace)
	case sum.GetTotalLaps() != s.totalLaps:
		return fmt.Sprintf("%d laps, not %d", s.totalLaps, sum.GetTotalLaps())
	case cars > 0 && wasCars > 0 && cars != wasCars:
		return fmt.Sprintf("%d cars, not %d", cars, wasCars)
	case open.GetLap() != nil && s.lap < open.GetLap().GetLap():
		return fmt.Sprintf("lap %d, before its last lap %d", s.lap, open.GetLap().GetLap())
	}
	return ""
}

// continueSession is v1's _continue: the recorded session opens again on s, the
// first frame of it this run sees, with what it had (see the top).
func (m *Machine) continueSession(open *statev1.OpenSession, s *state, t time.Time) {
	m.check.id = "" // a held start's id is not the session's
	sum := proto.CloneOf(open.GetSummary())
	id := open.GetSessionId()
	sess := newSession(id, s)
	sess.summary = sum
	gap := &eventsv1.SessionGap{
		StartedAt: open.GetLastSeenAt(), EndedAt: timestamppb.New(t), Lap: s.lap,
		Reason: eventsv1.GapReason_GAP_REASON_ENGINEER_RESTART,
	}
	sum.Gaps = append(sum.Gaps, gap)
	// The lap the gap falls in is one lap (R4), from where it started.
	sess.gapLap = gap.GetLap()
	if l := open.GetLap(); l != nil && l.GetLap() == s.lap {
		sess.lapStartFuel = l.GetFuelAtStart()
	}
	// What sawFrame and summaryAsOf write each frame, from the summary.
	sess.drivenBefore, sess.pausedBefore = sum.GetDrivingMs(), sum.GetPausedMs()
	sess.fuelBurned, sess.lastCars, sess.lastPosition = sum.GetFuelBurned(), sum.GetTotalCars(), sum.GetFinalPosition()
	if all := sum.GetFuelStints(); len(all) > 0 {
		sess.stints.stints = append([]float64(nil), all...)
	}
	// The laps, as the lap model reads them.
	sess.completed = sum.GetLapsCompleted()
	for _, l := range sum.GetLaps() {
		sess.lapsFuelUsed += l.GetFuelUsed()
		if l.GetComplete() && l.GetLapTimeMs() > 0 {
			sess.laps.timed = append(sess.laps.timed, timedLap{lap: l.GetLap(), ms: l.GetLapTimeMs(), counts: l.GetCountsForBest()})
		}
	}
	sess.laps.gt7BestMs = sum.GetGameBestLapMs()
	if sum.BestLap != nil && sum.BestLapTimeMs != nil {
		sess.laps.best = &timedLap{lap: sum.GetBestLap(), ms: sum.GetBestLapTimeMs(), counts: true}
	}
	sess.kindBefore = sum.GetKind()
	// The pits: the detector starts from the stops done and the one the car is in,
	// and the ids' counts go on.
	sess.pits = pitCounts{entries: open.GetEntries(), before: sum.GetPitStops()}
	done := make([]pit.Stop, len(sum.GetPitStops()))
	for i, p := range sum.GetPitStops() {
		done[i] = stopOf(p)
	}
	sess.pits.stops = int32(len(done)) //nolint:gosec // a session's stops fit
	var inPit *pit.Stop
	if e := open.GetPitEntry(); e != nil {
		// The entry's time on this run's frame clock, from when it was received.
		enteredT := s.t - t.Sub(e.GetEnteredAt().AsTime()).Seconds()
		inPit = &pit.Stop{
			Number: sess.pits.stops + 1, InLap: e.GetInLap(), EntryT: enteredT, FuelBefore: e.GetFuelBefore(),
			FuelAfter: s.fuel, PositionBefore: e.GetPositionBefore(), Evidence: e.GetEvidence(),
		}
		sess.pits.entry, sess.pits.enteredT, sess.pits.enteredAt = e.GetEntry(), enteredT, e.GetEnteredAt().AsTime()
		sess.pits.entered, sess.pits.earlierRun = e, true
	}
	m.pits.Resume(done, inPit)
	sess.laps.pits = m.pits.Status()
	// The counts and the race's facts.
	m.pauses.count = open.GetPauses()
	m.finalLap = open.GetFinalLapStarted()
	if p := open.GetPublishedPosition(); p > 0 {
		// The grid's settling is over: the next change is from the place last given.
		m.places = places{announced: p, started: true, startedAt: s.t - settleSecs}
		m.published = p
	}
	m.session = sess
	// The track, carried: known at once, and still open to the detector ruling it
	// out (R6, paddock#81 R4), as any verdict is (applyTrack).
	var carried *eventsv1.TrackVerdict
	if match, ok := carriedTrack(sum.GetTrack()); ok {
		sess.track, sess.carried = &match, true
		carried = verdict(match)
		carried.Carried = true
		sum.Track = carried
	}
	m.out = append(m.out, Output{
		Event:   m.event(id+":session.continued:"+m.run, t),
		Session: id,
		Continued: &eventsv1.SessionContinued{
			SessionId: id, Gap: gap, Track: proto.CloneOf(carried), At: s.seq, RunId: m.run,
		},
	})
	if carried != nil {
		m.trackChanges = append(m.trackChanges, sess.track)
		seq := strconv.FormatUint(uint64(s.seq), 10)
		m.out = append(m.out, Output{
			Event:   m.event(id+":session.track:"+m.run+":"+seq, t),
			Session: id,
			Track: &eventsv1.SessionTrackChanged{
				SessionId: id, Track: proto.CloneOf(carried), At: s.seq, RunId: m.run,
			},
		})
	}
}

// carriedTrack is v1's carried_track: the track the session had found, as the
// detector's verdict: a sure layout, or the circuit when only that was known (the
// Spa twins); false when it had placed neither.
func carriedTrack(v *eventsv1.TrackVerdict) (track.Match, bool) {
	switch c := v.GetConfidence(); {
	case v.TrackId != nil && (c == eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE ||
		c == eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED):
		conf := track.Probable
		if c == eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED {
			conf = track.Confirmed
		}
		return track.Match{
			TrackID: v.GetTrackId(), Name: v.GetName(), Confidence: conf, Evidence: carriedEvidence, Family: v.GetFamily(),
		}, true
	case v.GetFamily() != "":
		return track.Match{Name: v.GetFamily(), Confidence: track.Ambiguous, Evidence: carriedEvidence, Family: v.GetFamily()}, true
	}
	return track.Match{}, false
}

// stopOf is a stop the summary recorded, as the pit detector holds it: done, its
// times on no clock but their difference, the time in the pit lane.
func stopOf(p *eventsv1.PitStop) pit.Stop {
	return pit.Stop{
		Number: p.GetStop(), InLap: p.GetInLap(), OutLap: p.GetOutLap(),
		ExitT: float64(p.GetPitLaneMs()) / 1000, StationarySecs: float64(p.GetStationaryMs()) / 1000,
		FuelBefore: p.GetFuelBefore(), FuelAfter: p.GetFuelAfter(), TyresChanged: p.GetTyresChanged(),
		PitLaneKph: p.GetPitLaneKph(), Exited: true, PositionBefore: p.GetPositionBefore(),
		PositionAfter: p.GetPositionAfter(), Evidence: p.GetEvidence(),
	}
}

// endKept ends the recorded session from its record alone, at now (v1's
// release_held, with the summary a shutdown wrote): the lap it was on closes as
// a partial lap, a stop it was in is an open stop, and a session left in a
// pause ended paused, when the pause began. It is Output.Kept.
func (m *Machine) endKept(now time.Time, why string) {
	open := m.kept.open
	m.kept = kept{}
	sum := proto.CloneOf(open.GetSummary())
	id := open.GetSessionId()
	if e := open.GetPitEntry(); e != nil {
		stop := &eventsv1.PitStop{
			Stop: int32(len(sum.GetPitStops())) + 1, InLap: e.GetInLap(), //nolint:gosec // a session's stops fit
			FuelBefore: e.GetFuelBefore(), FuelAfter: sum.GetFuelAtEnd(),
			FuelAdded: round3(max(sum.GetFuelAtEnd()-e.GetFuelBefore(), 0)),
			Evidence:  e.GetEvidence(), PositionBefore: e.PositionBefore,
		}
		sum.PitStops = append(sum.PitStops, stop)
	}
	if l := open.GetLap(); l != nil {
		used := round3(l.GetFuelAtStart() - sum.GetFuelAtEnd())
		lap := &eventsv1.LapCompleted{
			SessionId: id, Lap: l.GetLap(), FuelAtStart: l.GetFuelAtStart(), FuelAtEnd: sum.GetFuelAtEnd(),
			FuelUsed: used, PitRole: l.GetPitRole(), Position: sum.FinalPosition,
		}
		sum.Laps = append(sum.Laps, lap)
		sum.FuelUsed = round3(sum.GetFuelUsed() + used)
	}
	ended := open.GetLastSeenAt()
	if p := open.GetPause(); p != nil {
		sum.EndedPaused = true
		if p.GetSince().AsTime().Before(ended.AsTime()) {
			ended = p.GetSince()
		}
	}
	sum.EndedAt = ended
	m.out = append(m.out, Output{
		Event:   m.event(id+":session.ended:1", now),
		Session: id,
		Ended: &eventsv1.SessionEnded{
			SessionId: id, Reason: WhyStopped.Reason(), Summary: sum,
		},
		Why: WhyStopped, Kept: true, NotContinued: why,
	})
}

// Open is the open session's record for the KV (statev1.OpenSession), as of its
// last frame, or nil with none open: what a later run continues or ends it from.
// The caller keeps it at the session's start and end, at each lap, pit entry,
// exit or withdrawal, pause and resume, and when ecu stops, off the frame path;
// Open itself copies the running summary, so it is called on those, not on every
// frame. The source is the caller's to set.
func (m *Machine) Open() *statev1.OpenSession {
	sess := m.session
	if sess == nil {
		return nil
	}
	sum := proto.CloneOf(sess.summary)
	// The record's summary has only the stops a pit.exited carried.
	if m.pits.InPitLane() && len(sum.GetPitStops()) > 0 {
		sum.PitStops = sum.PitStops[:len(sum.PitStops)-1]
	}
	sum.EndedAt, sum.EndedPaused = nil, false
	open := &statev1.OpenSession{
		SessionId: sess.id, RunId: m.run, LastSeenAt: timestamppb.New(sess.lastSeen), Summary: sum,
		Pauses: m.pauses.count, Entries: sess.pits.entries, FinalLapStarted: m.finalLap,
	}
	if sess.lastSeen.IsZero() {
		open.LastSeenAt = sum.GetStartedAt()
	}
	if sess.lastLap > 0 {
		open.Lap = &statev1.OpenLap{
			Lap: sess.lastLap, FuelAtStart: sess.lapStartFuel, PitRole: pitRole(sess.laps.pits.LapRole(sess.lastLap)),
		}
	}
	if m.pauses.open && m.pauses.paused != nil {
		open.Pause = proto.CloneOf(m.pauses.paused)
	}
	if m.pits.InPitLane() && sess.pits.entered != nil {
		open.PitEntry = proto.CloneOf(sess.pits.entered)
	}
	if m.published > 0 {
		p := m.published
		open.PublishedPosition = &p
	}
	return open
}
