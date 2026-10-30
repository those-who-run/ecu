// Package race is ecu's race state: a pure state machine that takes GT7 packets one
// at a time, as salsa gives them, and returns a frame for each and the session and
// lap events, exactly where v1 opened a session, closed a lap and ended a session
// (those-who-run/paddock#33).
//
// It has no clock, no network and no files: each packet carries the time it was
// received, and the caller gives the ids, so the same packets and the same ids give
// the same outputs. Publishing the outputs is the caller's (paddock#34). A lint rule
// in .golangci.yml keeps NATS, the network and the file system out of this package.
//
// The rules are v1's, from those-who-run/those-who-run at v1.0.0: the transition
// helpers of state.py (state.go here), the session and lap steps of
// Engine._step in engine.py, and the lap bookkeeping of SessionStore.observe,
// _flush_lap and end_session in store.py, and the session's kind from
// SessionDetector in sessionkind.py and Engine._apply_kind (kind.go here,
// paddock#82), and the session's track from
// Engine._apply_track (track.go here, paddock#81), fed by internal/track's
// detector, and the pit stops from Engine._apply_pits (pit.go here, paddock#78), fed
// by internal/pit's, and the finish, by the flag or timed, from Engine._timed_finish
// (finish.go here, paddock#85), the final lap and the settled position from
// v1's FinalLapRule and PositionRule (position.go here, paddock#84), and the lap
// model: each lap's pit role and whether it counts for the best, from
// Engine._step's lap_time rule, and the best, from results.best_lap with
// contracts v0.2.3's one difference (laps.go here, paddock#83), and the
// telemetry watchdog's loss and return, from App._watchdog and Engine.feed_lost
// (feed.go here, paddock#87), the caller giving it the time. What else v1's engine reads (replays and demo races
// held at the start, continuing after a restart and the frozen pause) is not here
// yet.
package race

import (
	"io"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/pit"
	"github.com/those-who-run/ecu/internal/track"
	"github.com/those-who-run/salsa"
)

// Source is the ce-source of everything the machine gives.
const Source = "/twr/ecu/gt7"

// IDs gives a new ULID on each call: the machine's run id, then each session's id.
// In production it is ulid.Make, on the real clock with real entropy.
type IDs func() ulid.ULID

// NewIDs is an IDs on the clock now with the entropy given, monotonic within a
// millisecond. A fixed clock and seeded entropy give the same ids on every run, for
// tests and replays. It panics if entropy fails, as ulid.MustNew does.
func NewIDs(now func() time.Time, entropy io.Reader) IDs {
	monotonic := ulid.Monotonic(entropy, 0)
	return func() ulid.ULID { return ulid.MustNew(ulid.Timestamp(now()), monotonic) }
}

// Received is a packet as ecu received it.
type Received struct {
	// Received is the packet, and its time on the source's own clock in seconds
	// (the frame's t).
	salsa.Received
	// Time is when ecu received the packet, on the wall clock: the frame's ce-time,
	// and the time of the events it gives rise to.
	Time time.Time
}

// Why is why a session ended: one for each of the contracts' EndReason values the
// machine sends (Why.Reason), kept beside the one on the wire (Output.Why).
type Why int

// Why a session ended.
const (
	// WhyFinished is the chequered flag.
	WhyFinished Why = iota + 1
	// WhyQuit is a loading screen with the lap counter reset before the flag, as
	// quitting from the pause menu or restarting a race gives (v1's race_ended on a
	// loading frame).
	WhyQuit
	// WhyRestart is GT7 jumping back to a lower lap while driving (v1's
	// race_restarted).
	WhyRestart
	// WhyStart is another start while the session was open (v1's race_started).
	WhyStart
	// WhyInputEnded is the input coming to its end, as a replayed recording does,
	// with the session open (Machine.EndOfInput).
	WhyInputEnded
	// WhyStopped is ecu stopping with the session open (Machine.Stop).
	WhyStopped
	// WhyMenu is straight back to a menu or the track screen before the flag (v1's
	// race_ended on a menu or track-screen frame).
	WhyMenu
	// WhyTimedFinish is the end of a session after its timed finish (v1's
	// race_ended once Engine._timed_finish had set finished).
	WhyTimedFinish
	// WhyTelemetryLost is the feed lost in a pause (v1's Engine._end_paused "the
	// feed was lost", feed.go): the session ends paused.
	WhyTelemetryLost
)

var whyNames = map[Why]string{
	WhyFinished: "finished", WhyQuit: "quit", WhyRestart: "restart", WhyStart: "start",
	WhyInputEnded: "input ended", WhyStopped: "stopped", WhyMenu: "menu",
	WhyTimedFinish: "timed finish", WhyTelemetryLost: "telemetry lost",
}

func (w Why) String() string {
	if name, ok := whyNames[w]; ok {
		return name
	}
	return "Why(" + strconv.Itoa(int(w)) + ")"
}

// Reason is the contracts' EndReason for w (contracts v0.2.0).
func (w Why) Reason() eventsv1.EndReason {
	switch w {
	case WhyFinished:
		return eventsv1.EndReason_END_REASON_FINISHED
	case WhyQuit:
		return eventsv1.EndReason_END_REASON_QUIT
	case WhyRestart:
		return eventsv1.EndReason_END_REASON_RESTARTED
	case WhyStart:
		return eventsv1.EndReason_END_REASON_NEXT_SESSION_STARTED
	case WhyInputEnded:
		return eventsv1.EndReason_END_REASON_INPUT_ENDED
	case WhyStopped:
		return eventsv1.EndReason_END_REASON_ENGINEER_STOPPED
	case WhyMenu:
		return eventsv1.EndReason_END_REASON_MENU
	case WhyTimedFinish:
		return eventsv1.EndReason_END_REASON_TIMED_FINISH
	case WhyTelemetryLost:
		return eventsv1.EndReason_END_REASON_TELEMETRY_LOST
	default:
		return eventsv1.EndReason_END_REASON_UNKNOWN
	}
}

// Output is a frame or an event, with its CloudEvents id and time: exactly one of
// Frame, Started, Lap, Best, Track, Kind, PitEntered, PitExited, PitTeleport,
// FinalLap, Position, Finished, Ended, TelemetryLost, TelemetryRestored and Callout is set, each the contracts message of that name
// (Callout is the rule engine's, internal/rules). SessionStarted's
// source is left empty: only the publisher knows the <src> it publishes under, and
// sets it (internal/bus).
type Output struct {
	// Event is the ce-id (and Nats-Msg-Id), ce-source and ce-time, the id built
	// as contracts' subjects.yaml says: <run>:frame:<seq> for a frame, and
	// <sid>:session.started:1, <sid>:lap.completed:<lap> and
	// <sid>:session.ended:1 for the events, <sid>:lap.best:<n> with the lap
	// whose end changed the best, <sid>:session.track:<run>:<seq>
	// and <sid>:session.kind:<run>:<seq> for a track or kind verdict, with the
	// seq of the frame it changed on, <sid>:pit.entered:<entry>,
	// <sid>:pit.exited:<stop> and <sid>:pit.teleport:<entry> for the pits,
	// <sid>:race.final_lap:1 and <sid>:race.position:<run>:<seq> for the final
	// lap and a settled position, and <sid>:race.finished:1 for the finish. An event's id is the catalogue's
	// without its <src>: the publisher puts <src>: in front (paddock#279).
	Event ce.Event
	// Session is the id of the session an event belongs to; "" for a frame.
	Session string

	Frame   *telemetryv1.Frame
	Started *eventsv1.SessionStarted
	Lap     *eventsv1.LapCompleted
	Best    *eventsv1.LapBestChanged
	Track   *eventsv1.SessionTrackChanged
	Kind    *eventsv1.SessionKindChanged
	Ended   *eventsv1.SessionEnded

	PitEntered  *eventsv1.PitEntered
	PitExited   *eventsv1.PitExited
	PitTeleport *eventsv1.PitTeleport
	FinalLap    *eventsv1.RaceFinalLapStarted
	Position    *eventsv1.RacePositionChanged
	Finished    *eventsv1.RaceFinished

	// TelemetryLost and TelemetryRestored are the watchdog's (feed.go,
	// paddock#87): <sid>:telemetry.lost:<run>:<seq> with a session open and
	// <run>:telemetry.lost:<seq> with none, and the same for restored, seq being
	// the event's at.
	TelemetryLost     *eventsv1.TelemetryLost
	TelemetryRestored *eventsv1.TelemetryRestored

	// Callout is a callout the rule engine raised (internal/rules, paddock#89),
	// on twr.v1.<src>.callout.<level>: never the machine's own.
	Callout *eventsv1.Callout

	// Why is why the session ended, set with Ended; SessionEnded's reason is
	// Why.Reason().
	Why Why
}

// Message is the contracts message the output carries.
func (o Output) Message() proto.Message {
	switch {
	case o.Frame != nil:
		return o.Frame
	case o.Started != nil:
		return o.Started
	case o.Lap != nil:
		return o.Lap
	case o.Best != nil:
		return o.Best
	case o.Track != nil:
		return o.Track
	case o.Kind != nil:
		return o.Kind
	case o.PitEntered != nil:
		return o.PitEntered
	case o.PitExited != nil:
		return o.PitExited
	case o.PitTeleport != nil:
		return o.PitTeleport
	case o.FinalLap != nil:
		return o.FinalLap
	case o.Position != nil:
		return o.Position
	case o.Finished != nil:
		return o.Finished
	case o.Callout != nil:
		return o.Callout
	case o.TelemetryLost != nil:
		return o.TelemetryLost
	case o.TelemetryRestored != nil:
		return o.TelemetryRestored
	default:
		return o.Ended
	}
}

// Machine is the race state. It holds all of it: nothing else does. A Machine is
// not safe for concurrent use.
type Machine struct {
	ids IDs
	run string
	seq uint32

	prev        *state
	prevTime    time.Time
	lastDriving *state // the last IN_RACE frame of this race (v1's Engine.last_driving)
	finish      finish // the race's finish, by the flag or timed (v1's Engine.finished)
	finalLap    bool   // the final lap was announced (v1's FinalLapRule.announced)
	places      places // the settled position (v1's PositionRule)
	kinds       kinds  // the session's kind (v1's Engine.kinds)
	tracks      *track.Detector
	pits        *pit.Detector
	session     *session

	out    []Output
	ctx    Context // the frame as the rules read it (context.go, paddock#89)
	joined bool    // the session was joined part way (v1's rule.joined)
	// trackChanges are the verdicts applied to a session since the rules last
	// saw one, for Context.TrackChanges.
	trackChanges []*track.Match

	feed feed // the telemetry watchdog (feed.go, paddock#87)
	// endedPaused: the session ended in a pause, and the frames are ignored until
	// the next start (v1's Engine.ended_paused).
	endedPaused bool
}

// session is the open session: v1's store.Session, cut to what the events need.
type session struct {
	id           string
	lapStartFuel float64
	lap          lapFrames
	completed    int32
	lapsFuelUsed float64      // the sum of each lap's fuel used, in the order they closed
	lastPosition int32        // the last race position seen, 0 for none
	track        *track.Match // the verdict applied to the session, nil for none (v1's Engine.track)
	kind         *kindVerdict // the kind verdict applied to the session, nil for none (v1's Session.kind)
	pits         pitCounts
	laps         lapBook // the lap model's view of the laps (laps.go, paddock#83)
}

// lapFrames are the frames of the lap in progress (v1's Session.buffer): their seqs,
// not the frames.
type lapFrames struct {
	n              int
	first, penult  uint32 // the first frame, and the one before the last
	last           uint32
	lapOfLastFrame int32
}

func (b *lapFrames) add(s *state) {
	if b.n == 0 {
		b.first = s.seq
	}
	b.penult, b.last = b.last, s.seq
	b.lapOfLastFrame = s.lap
	b.n++
}

// New is a machine with no session open. It mints its run id from ids, then each
// session's id as the session starts.
func New(ids IDs) *Machine {
	return &Machine{ids: ids, run: ids().String(), tracks: track.New(nil, track.Config{}), pits: pit.New()}
}

// Run is the machine's run id, a ULID: the <run> in each frame's id.
func (m *Machine) Run() string { return m.run }

// Step takes the next packet and returns its frame, then the events it gives rise
// to, in the order they happened: the end of a session the packet's start closes,
// the start, a pit entry, exit or entry withdrawn, the final lap and a settled
// position, a change of the session's track, a change of its kind, a lap closed
// and the best it gave the session, the race's finish, and the end of the session.
func (m *Machine) Step(r Received) []Output {
	m.seq++
	s := newState(m.seq, r.T, &r.Packet)
	s.frame, s.received = frame(m.seq, r.T, s.phase, &r.Packet), r.Time
	m.ctx = Context{}
	m.out = []Output{{
		Event: m.event(m.run+":frame:"+strconv.FormatUint(uint64(m.seq), 10), r.Time),
		Frame: s.frame,
	}}
	m.restore(&s, r.Time)

	// Engine._step, the parts that open, close and end.
	prev := m.prev
	restarted := raceRestarted(prev, &s)
	starting := raceStarted(prev, &s) || restarted
	// v1's Engine.process: after a session ended paused, nothing but a start is
	// read; the frame goes out and the rules see it with no session.
	if m.endedPaused && !starting {
		m.ctx.Run, m.ctx.Time, m.ctx.Cur = m.run, r.Time, s.view()
		m.prev, m.prevTime = &s, r.Time
		return m.out
	}
	m.endedPaused = false
	if starting {
		if m.session != nil && prev != nil {
			why := WhyStart
			if restarted {
				why = WhyRestart
			}
			m.end(prev, m.prevTime, why)
		}
		m.lastDriving = nil
		m.finish = finish{}
		m.finalLap, m.places = false, places{}
		m.kinds = kinds{}
		m.pits.Reset()
		m.joined, m.trackChanges = joinedPartWay(prev, &s), nil
		// The track detector starts over by itself after a menu or a loading screen
		// at lap 0, so only a restart, which teleports the car back to the grid with
		// neither, resets it here, as v1's engine does: a rolling start keeps what
		// it saw before the line. A session joined part way names no track before a
		// line crossing.
		if restarted {
			m.tracks.Reset()
		}
		if joinedPartWay(prev, &s) {
			m.tracks.Join()
			m.kinds.join()
		}
	}
	// v1's timed finish reads the kind before the detector sees this frame, and
	// after a start has reset it.
	kindBefore := m.kinds.verdict()
	// v1 feeds the detector every frame, after a start has reset it and before the
	// lap store reads the frame: the start carries the verdict on its first frame,
	// and a lap the verdict when it ends.
	kindChanged := m.kinds.observe(&s)
	if starting {
		m.start(&s, r.Time, joinedPartWay(prev, &s))
		// A rolling start crosses the line before lap 1, so the detector may know
		// the track already when the session opens (paddock#81, R3).
		if match := m.tracks.Match(); match.Confidence != track.None {
			m.applyTrack(match, &s, r.Time)
		}
	}
	// Before the lap store, as v1's Engine._step: the crossing that closes an
	// in-lap can come on the very frame after the pit cut (paddock#78, R6).
	m.observePits(m.out[0].Frame, r.Time)
	// Rules and the lap store compare against the last driving frame. A pause or a
	// loading screen in between hides nothing: GT7 posts a lap on a loading frame
	// when the car crosses the line in a pit cut-scene (into the pits at Suzuka East
	// 2026-10-15, out of them at Willow Springs 2026-10-17), so the crossing is only
	// visible from the frame before it.
	seen := prev
	if prev != nil &&
		(prev.phase == phaseLoading || prev.phase == phasePaused) &&
		(s.phase == phaseInRace || s.phase == phaseFinished) &&
		m.lastDriving != nil && !restarted {
		seen = m.lastDriving
	}
	// v1's rules run after the pits and before the timed finish, the track, the
	// kind and the lap store, in Engine._step: the final lap and a settled position
	// go here, read the pit lane after this frame and the kind before it, and come
	// before the lap the same crossing closes, as v1 decided them.
	m.observeRace(seen, &s, r.Time, kindBefore)
	// The rules' view of the frame (context.go): filled here, at v1's place for
	// the rules, then the lap, the finish and the end below add to it.
	m.ruleContext(prev, seen, &s, r.Time, starting, kindBefore)
	// The detector sees every frame, a session open or not; a change of its
	// decision is the session's to keep or not, before the lap store reads the
	// frame, as in v1's Engine._step.
	if match, changed := m.tracks.Observe(m.out[0].Frame); changed && m.session != nil {
		m.applyTrack(match, &s, r.Time)
	}
	// After the track, as in v1's Engine._step: a change of the kind is the open
	// session's. A start always changes it, from none.
	if kindChanged && m.session != nil {
		m.applyKind(&s, r.Time)
	}

	ended := raceEnded(prev, &s)
	if m.session != nil {
		m.observe(seen, &s, r.Time)
	}
	// v1's Engine._step decides the finish after the pits and the rules, before the
	// track, the kind and the lap store see the frame: _timed_finish, then the
	// FINISHED phase. Nothing those three change is read by it but the kind, which
	// it takes from before this frame (kindBefore), so deciding it here is the
	// same. It goes after the lap store so that the final lap's lap.completed comes
	// first and race.finished's total time and best lap count that lap, which
	// closes on the flag frame.
	m.observeFinish(seen, &s, r.Time, kindBefore)
	m.ctx.Finished = m.finish.kind != eventsv1.FinishKind_FINISH_KIND_UNSPECIFIED
	if ended && s.phase != phaseFinished && !m.ctx.Finished && m.lastDriving != nil {
		last := m.lastDriving.view()
		m.ctx.EndedEarly = &last
	}
	if m.session != nil && ended {
		m.finalTrack()
		// v1's set_pits on a race's end, before end_session: the lap the session
		// ends in takes its role from the pits as they are now.
		m.session.laps.pits = m.pits.Status()
		why := WhyQuit
		switch {
		case m.finish.kind == eventsv1.FinishKind_FINISH_KIND_FLAG:
			why = WhyFinished
		case m.finish.kind == eventsv1.FinishKind_FINISH_KIND_TIMED:
			why = WhyTimedFinish
		case s.phase == phaseMenu || s.phase == phaseAtTrack:
			why = WhyMenu
		}
		m.end(&s, r.Time, why)
	}

	if s.inRace() {
		driving := s
		m.lastDriving = &driving
	} else if ended {
		m.lastDriving = nil
	}
	m.prev, m.prevTime = &s, r.Time
	return m.out
}

// EndOfInput ends the open session, if there is one, on the last frame, because
// the input came to its end, as a replayed recording does (END_REASON_INPUT_ENDED).
func (m *Machine) EndOfInput() []Output { return m.close(WhyInputEnded) }

// Stop ends the open session, if there is one, on the last frame, because ecu is
// stopping (END_REASON_ENGINEER_STOPPED).
func (m *Machine) Stop() []Output { return m.close(WhyStopped) }

func (m *Machine) close(why Why) []Output {
	m.out = nil
	if m.session != nil && m.prev != nil {
		m.end(m.prev, m.prevTime, why)
	}
	return m.out
}

func (m *Machine) event(id string, t time.Time) ce.Event {
	return ce.Event{ID: id, Source: Source, Time: t}
}

// start opens a session on its first driving frame (v1's SessionStore.start_session).
func (m *Machine) start(s *state, t time.Time, joined bool) {
	id := m.ids().String()
	m.session = &session{id: id, lapStartFuel: s.fuel}
	started := &eventsv1.SessionStarted{
		SessionId:     id,
		Game:          eventsv1.Game_GAME_GT7,
		SessionType:   m.kinds.kind(),
		CarCode:       s.carCode,
		JoinedPartWay: joined,
		FirstSeq:      s.seq,
		StartedAt:     timestamppb.New(t),
		FuelAtStart:   s.fuel,
	}
	if s.totalLaps > 0 {
		total := s.totalLaps
		started.TotalLaps = &total
	}
	m.out = append(m.out, Output{
		Event:   m.event(id+":session.started:1", t),
		Session: id,
		Started: started,
	})
}

// observe is v1's SessionStore.observe: a driving frame joins the lap in progress,
// and a crossing closes the lap.
func (m *Machine) observe(prev, s *state, t time.Time) {
	if s.inRace() || racingFrame(prev, s) {
		m.session.lap.add(s)
		if p := s.racePosition(); p != 0 {
			m.session.lastPosition = p
		}
		if s.bestLapMs > 0 {
			m.session.laps.gt7BestMs = s.bestLapMs
		}
	}
	if lapCompleted(prev, s) {
		m.closeLap(prev.lap, s, t, true)
	}
}

// closeLap is v1's SessionStore._flush_lap: the lap's event, from the frames since
// the last crossing, with s the frame that closed it.
func (m *Machine) closeLap(lap int32, s *state, t time.Time, complete bool) {
	sess := m.session
	frames := sess.lap
	if frames.n == 0 {
		return
	}
	sess.lap = lapFrames{}
	last := frames.last
	// The frame that closed the lap belongs to the next lap's buffer, unless it was
	// the chequered flag and there is no next lap.
	if complete && s.inRace() && frames.n > 1 {
		last = frames.penult
		sess.lap.add(s)
	}
	used := round3(sess.lapStartFuel - s.fuel)
	ev := &eventsv1.LapCompleted{
		SessionId:   sess.id,
		Lap:         lap,
		Complete:    complete,
		FuelAtStart: sess.lapStartFuel,
		FuelAtEnd:   s.fuel,
		FuelUsed:    used,
		FirstSeq:    frames.first,
		LastSeq:     last,
	}
	// The lap model's verdicts (laps.go): the role from the pit status as last
	// applied to the session, and whether the lap counts for the best, as v1's
	// lap_time rule decided on the frame that posted the time.
	role := sess.laps.pits.LapRole(lap)
	ev.PitRole = pitRole(role)
	if complete {
		m.ctx.Lap = &ClosedLap{Lap: lap, TimeMs: max(s.lastLapMs, 0), Role: role}
	}
	if complete && s.lastLapMs > 0 {
		m.ctx.Lap.Counts = countsForBest(lap, s, role)
		ev.LapTimeMs = s.lastLapMs
		ev.CountsForBest = countsForBest(lap, s, role)
		sess.laps.timed = append(sess.laps.timed, timedLap{lap: lap, ms: s.lastLapMs, counts: ev.CountsForBest})
	}
	// Outside a field the position is left out, by the verdict at the lap's end.
	if p := s.racePosition(); p != 0 && !solo(m.kinds.kind()) {
		ev.Position = &p
		if complete {
			m.ctx.Lap.Position = p
		}
	}
	sess.lapStartFuel = s.fuel
	sess.lapsFuelUsed += used
	if complete {
		sess.completed++
	}
	m.out = append(m.out, Output{
		Event:   m.event(sess.id+":lap.completed:"+strconv.Itoa(int(lap)), t),
		Session: sess.id,
		Lap:     ev,
	})
	m.publishBest(lap, last, t)
}

// end closes the open session on s, its last frame (v1's SessionStore.end_session):
// the frames since the last crossing close as one incomplete lap, numbered by the
// lap of its last frame.
func (m *Machine) end(s *state, t time.Time, why Why) {
	sess := m.session
	if sess.lap.n > 0 {
		m.closeLap(sess.lap.lapOfLastFrame, s, t, false)
	}
	if s.phase == phaseFinished {
		why = WhyFinished
	}
	// The final verdict decides the positions: none outside a field, and a
	// finishing position only for a lap race at the flag (contracts v0.1.4).
	kind := m.kinds.kind()
	final := s.racePosition()
	if final == 0 {
		final = sess.lastPosition
	}
	summary := &eventsv1.SessionSummary{
		LapsCompleted: sess.completed,
		FuelUsed:      round3(sess.lapsFuelUsed),
		SessionType:   kind,
		Finished:      m.finish.kind != eventsv1.FinishKind_FINISH_KIND_UNSPECIFIED,
		EndedPaused:   why == WhyTelemetryLost,
	}
	if total, ok := sess.laps.totalTimeMs(); ok && summary.Finished {
		summary.TotalTimeMs = &total
	}
	lapSummary(summary, &sess.laps)
	if sess.track != nil {
		summary.Track = verdict(*sess.track)
	}
	if sess.kind != nil {
		summary.Kind = sess.kind.message()
	}
	pitSummary(summary, m.pits.Status())
	if final != 0 && !solo(kind) {
		summary.FinalPosition = &final
		if why == WhyFinished && kind == eventsv1.SessionType_SESSION_TYPE_LAP_RACE {
			summary.FinishingPosition = &final
		}
	}
	m.out = append(m.out, Output{
		Event:   m.event(sess.id+":session.ended:1", t),
		Session: sess.id,
		Ended: &eventsv1.SessionEnded{
			SessionId: sess.id, Reason: why.Reason(), Summary: summary, LastSeq: s.seq,
		},
		Why: why,
	})
	m.session = nil
}

// round3 is Python's round(x, 3), which v1 rounds fuel with: the decimal nearest
// x's exact binary value, ties to even.
func round3(x float64) float64 {
	// FormatFloat always gives a number ParseFloat reads.
	r, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 3, 64), 64)
	return r
}
