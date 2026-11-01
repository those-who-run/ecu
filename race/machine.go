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
// It is public, with the track and pit packages it builds on, for logbook's
// import of v1's history (those-who-run/paddock#264): another module builds
// against what it exports.
//
// The rules are v1's, from those-who-run/those-who-run at v1.0.0: the transition
// helpers of state.py (state.go here), the session and lap steps of
// Engine._step in engine.py, and the lap bookkeeping of SessionStore.observe,
// _flush_lap and end_session in store.py, and the session's kind from
// SessionDetector in sessionkind.py and Engine._apply_kind (kind.go here,
// paddock#82), and the session's track from
// Engine._apply_track (track.go here, paddock#81), fed by track's
// detector, and the pit stops from Engine._apply_pits (pit.go here, paddock#78), fed
// by pit's, and the finish, by the flag or timed, from Engine._timed_finish
// (finish.go here, paddock#85), the final lap and the settled position from
// v1's FinalLapRule and PositionRule (position.go here, paddock#84), and the lap
// model: each lap's pit role and whether it counts for the best, from
// Engine._step's lap_time rule, and the best, from results.best_lap with
// contracts v0.2.3's one difference (laps.go here, paddock#83), the telemetry
// watchdog's loss and return, from App._watchdog and Engine.feed_lost (feed.go
// here, paddock#87), the caller giving it the time, the start check that holds a
// start GT7's cars-on-track flag is off for and drops a replay or a demo race,
// v1's DriverCheck and Engine.process (start.go here, paddock#74), and the
// pauses, flagged and frozen, from v1's PauseClock and Engine._end_paused
// (pause.go here, paddock#77), and the session's summary, v1's summary.json from
// SessionStore.end_session, kept as a running summary all session (summary.go
// here, paddock#86), and continuing a session after an ecu restart, from v1's
// SessionStore._continue and continue_mismatch (continue.go here, paddock#76).
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
	"github.com/those-who-run/ecu/pit"
	"github.com/those-who-run/ecu/track"
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
	// WhyStopped is ecu stopping with the session open: the end of a session an
	// earlier run left open that this one does not continue (continue.go,
	// paddock#76), or Machine.Stop.
	WhyStopped
	// WhyMenu is straight back to a menu or the track screen before the flag (v1's
	// race_ended on a menu or track-screen frame).
	WhyMenu
	// WhyTimedFinish is the end of a session after its timed finish (v1's
	// race_ended once Engine._timed_finish had set finished).
	WhyTimedFinish
	// WhyTelemetryLost is the feed lost during a pause (Machine.Tick, v1's
	// _end_paused "the feed was lost"): the session ends paused.
	WhyTelemetryLost
	// WhyPauseLimit is a pause that lasted the pause limit (Machine.SetPauseLimit,
	// v1's _end_paused "paused for N min").
	WhyPauseLimit
)

var whyNames = map[Why]string{
	WhyFinished: "finished", WhyQuit: "quit", WhyRestart: "restart", WhyStart: "start",
	WhyInputEnded: "input ended", WhyStopped: "stopped", WhyMenu: "menu",
	WhyTimedFinish: "timed finish", WhyTelemetryLost: "telemetry lost", WhyPauseLimit: "pause limit",
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
	case WhyPauseLimit:
		return eventsv1.EndReason_END_REASON_PAUSE_LIMIT
	default:
		return eventsv1.EndReason_END_REASON_UNKNOWN
	}
}

// Output is a frame or an event, with its CloudEvents id and time: exactly one of
// Frame, Started, Lap, Best, Track, Kind, PitEntered, PitExited, PitTeleport,
// FinalLap, Position, Finished, Paused, Resumed, Discarded, Ended, TelemetryLost, TelemetryRestored, Callout
// and Alarm is set, each the contracts message of that name (Callout and Alarm are the rule engine's,
// internal/rules). SessionStarted's
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
	// lap and a settled position, <sid>:race.finished:1 for the finish, and
	// <sid>:session.paused:<pause> and <sid>:session.resumed:<pause> for a pause,
	// and <sid>:session.discarded:1 for a held start that was not a drive. An
	// event's id is the catalogue's without its <src>: the publisher puts <src>: in
	// front (paddock#279).
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
	// Discarded is a held start that was a replay or a demo (start.go,
	// paddock#74): no session.started came for it.
	Discarded *eventsv1.SessionDiscarded

	Paused  *eventsv1.SessionPaused
	Resumed *eventsv1.SessionResumed

	// Continued is a session an earlier ecu run left open, continued by this one
	// (continue.go, paddock#76), with the id <sid>:session.continued:<run>.
	Continued *eventsv1.SessionContinued

	// TelemetryLost and TelemetryRestored are the watchdog's (feed.go,
	// paddock#87): <sid>:telemetry.lost:<run>:<seq> with a session open and
	// <run>:telemetry.lost:<seq> with none, and the same for restored, seq being
	// the event's at.
	TelemetryLost     *eventsv1.TelemetryLost
	TelemetryRestored *eventsv1.TelemetryRestored

	// Callout is a callout the rule engine raised (internal/rules, paddock#89),
	// on twr.v1.<src>.callout.<level>: never the machine's own.
	Callout *eventsv1.Callout
	// Alarm is a car alarm a rule raised beside its callout (the temps rule,
	// paddock#97), on twr.v1.<src>.evt.<sid>.car.alarm, with the id
	// <sid>:car.alarm:<check>:<run>:<seq>: never the machine's own.
	Alarm *eventsv1.CarAlarmRaised

	// Why is why the session ended, set with Ended; SessionEnded's reason is
	// Why.Reason().
	Why Why
	// Kept says Ended ends the session an earlier ecu run left open, from its KV
	// record (continue.go, paddock#76): the caller publishes it only once it has
	// cleared that record, with a revision check, so two runs never both end it.
	// NotContinued says, in words, why it was not continued.
	Kept         bool
	NotContinued string
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
	case o.Paused != nil:
		return o.Paused
	case o.Resumed != nil:
		return o.Resumed
	case o.Continued != nil:
		return o.Continued
	case o.Callout != nil:
		return o.Callout
	case o.Discarded != nil:
		return o.Discarded
	case o.TelemetryLost != nil:
		return o.TelemetryLost
	case o.TelemetryRestored != nil:
		return o.TelemetryRestored
	case o.Alarm != nil:
		return o.Alarm
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
	published   int32  // the place the session's last race.position gave, 0 for none
	kinds       kinds  // the session's kind (v1's Engine.kinds)
	grid        grid   // the car standing on the grid before a start (kind.go, paddock#148)
	tracks      *track.Detector
	pits        *pit.Detector
	session     *session
	pauses      pauses // the pause clock (pause.go, v1's Engine.pauses)
	endedPaused bool   // the session ended in a pause: ignored until the next start
	kept        kept   // the session an earlier run left open (continue.go, paddock#76)
	opened      bool   // the run has opened a session: too late for Restore

	out    []Output
	now    time.Time // when ecu received the packet being taken, or the tick's time
	ctx    Context   // the frame as the rules read it (context.go, paddock#89)
	joined bool      // the session was joined part way (v1's rule.joined)
	// trackChanges are the verdicts applied to a session since the rules last
	// saw one, for Context.TrackChanges.
	trackChanges []*track.Match

	check startCheck              // the start check and its hold (start.go, paddock#74)
	rules func(*Context) []Output // run on each frame the race state takes (Rules)
	feed  feed                    // the telemetry watchdog (feed.go, paddock#87)
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

	// The running summary (summary.go, paddock#86), and what it is kept from.
	// sawFrame and summaryAsOf write the summary's fuel_burned, fuel stints,
	// driving_ms, paused_ms and total_cars from these fields on every frame, so a
	// summary read back after an ecu restart (paddock#76) is not enough alone:
	// the continuing run rebuilds these from it first (continue.go), as v1's
	// SessionStore._continue did: startT and startWall for this run, fuelBurned,
	// stints and lastCars, and the time driven and paused before the restart, to
	// add to this run's (the pause clock counts this run's from 0).
	summary    *eventsv1.SessionSummary
	startT     float64    // this run's first frame's t, for the time driven (v1's start_t)
	startWall  time.Time  // ... and when ecu received it (v1's start_wall)
	stints     fuelStints // the fuel burnt, stint by stint (v1's fuel_stints)
	fuelBurned float64    // every drop added up (v1's fuel_burned)
	lastCars   int32      // the last field size seen, 0 for none (v1's last_cars)

	// For the KV record and a continued session (continue.go, paddock#76).
	lastSeen     time.Time // when ecu received the session's last frame
	lastLap      int32     // ... and its lap counter
	drivenBefore int32     // the time driven before an ecu restart, in ms
	pausedBefore int32     // ... and paused
	// gapLap is the lap an ecu restart fell in, until it closes: one lap, not
	// complete, with GT7's time for it (R4), from the summary's last gap.
	gapLap int32
	// carried says the session's track verdict was carried over the restart
	// (TrackVerdict.carried), until the next verdict replaces it.
	carried bool
	// kindBefore is the kind verdict before the restart, whose start a verdict
	// of the same kind keeps (v1's set_kind).
	kindBefore *eventsv1.SessionKind
}

// newSession is a session that opens on s, its running summary empty until its
// session.started fills it (openSummary).
func newSession(id string, s *state) *session {
	return &session{
		id: id, lapStartFuel: s.fuel, startT: s.t, startWall: s.received,
		summary: &eventsv1.SessionSummary{}, stints: newFuelStints(),
	}
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

// Rules has the machine run rules on each frame the race state takes, after its
// step, and give what they return after the frame's events: the rule engine's
// Step (internal/rules, paddock#89). A frame held by the start check, or ignored
// after a replay or a demo, is not given to them; a drive's held frames are, each
// in turn, when the check lets them go (paddock#74).
func (m *Machine) Rules(rules func(*Context) []Output) { m.rules = rules }

// Run is the machine's run id, a ULID: the <run> in each frame's id.
func (m *Machine) Run() string { return m.run }

// Step takes the next packet and returns its frame, then the events it gives rise
// to, in the order they happened, and what the rules (Rules) raised on it. A start
// held by the start check gives the frame alone until its verdict, which gives
// the held frames' events and callouts in turn, or a replay's or a demo's
// session.discarded (start.go). The events are: the end of a session the packet's start closes,
// the start, a pit entry, exit or entry withdrawn, the final lap and a settled
// position, a change of the session's track, a change of its kind, a lap closed
// and the best it gave the session, the race's finish, and the end of the session.
func (m *Machine) Step(r Received) []Output {
	m.seq++
	m.now = r.Time
	s := newState(m.seq, r.T, &r.Packet)
	// A frozen frame is read as the pause it is before anything else sees it, the
	// frame that goes out included (v1's Engine.process).
	// A held start's frames are stepped later (start.go): what the clock read on
	// each goes with it.
	s.pause = m.pauses.observe(&s, &r.Packet, r.Time)
	// The grid is watched on every frame, the start check's included, as the
	// pause clock is: a start held or after a replay is stepped later.
	s.stood = m.grid.observe(&s)
	s.frame, s.received = frame(m.seq, r.T, s.phase, &r.Packet), r.Time
	m.ctx = Context{}
	// The session an earlier run left open ends first once the continue window
	// has passed, as if the timer had fired (continue.go, paddock#76).
	m.out = nil
	m.expire(r.Time)
	m.out = append(m.out, Output{
		Event: m.event(m.run+":frame:"+strconv.FormatUint(uint64(m.seq), 10), r.Time),
		Frame: s.frame,
	})
	// After a loss, telemetry.restored goes first after the frame (feed.go).
	m.restore(&s, r.Time)
	// The start check (start.go, paddock#74) holds a start GT7's flag is off for,
	// and ignores a replay or a demo; what it does not take goes through here.
	if !m.hold(&s) {
		m.step(&s)
	}
	return m.out
}

// step is v1's Engine._step: one frame through the race state, then the rules.
// Its events go on m.out.
func (m *Machine) step(s *state) {
	t := s.received
	// Engine._step, the parts that open, close and end.
	prev := m.prev
	restarted := raceRestarted(prev, s)
	starting := raceStarted(prev, s) || restarted
	// v1's Engine.process: after a session ended paused, nothing but a start is
	// read (pause.go's ignore), and a pause that lasts the limit ends the session
	// paused on this frame, which is not stepped.
	if m.endedPaused && !starting {
		m.ignore(s)
		return
	}
	m.endedPaused = false
	m.pauseEvents(s)
	if m.pauseLimitPassed(s) {
		m.endPaused(s, t, WhyPauseLimit)
		m.ignore(s)
		return
	}

	if starting {
		if m.session != nil && prev != nil {
			why := WhyStart
			if restarted {
				why = WhyRestart
			}
			m.end(prev, m.prevTime, why)
		}
		m.pauses.reset()
		m.lastDriving = nil
		m.finish = finish{}
		m.finalLap, m.places, m.published = false, places{}, 0
		ranked := restarted && m.kinds.ranked
		m.kinds = kinds{stood: s.stood}
		if ranked {
			m.kinds.carryRank() // a ranked session restarted is ranked (kind.go)
		}
		m.pits.Reset()
		m.joined, m.trackChanges = joinedPartWay(prev, s), nil
		// The track detector starts over by itself after a menu or a loading screen
		// at lap 0, so only a restart, which teleports the car back to the grid with
		// neither, resets it here, as v1's engine does: a rolling start keeps what
		// it saw before the line. A session joined part way names no track before a
		// line crossing.
		if restarted {
			m.tracks.Reset()
		}
		if joinedPartWay(prev, s) {
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
	kindChanged := m.kinds.observe(s)
	if starting {
		m.start(s, t, joinedPartWay(prev, s), prev == nil)
		// A rolling start crosses the line before lap 1, so the detector may know
		// the track already when the session opens (paddock#81, R3).
		if match := m.tracks.Match(); match.Confidence != track.None {
			m.applyTrack(match, s, t)
		}
	}
	// Before the lap store, as v1's Engine._step: the crossing that closes an
	// in-lap can come on the very frame after the pit cut (paddock#78, R6).
	m.observePits(s.frame, t)
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
	m.observeRace(seen, s, t, kindBefore)
	// The rules' view of the frame (context.go): filled here, at v1's place for
	// the rules, then the lap, the finish and the end below add to it.
	m.ruleContext(prev, seen, s, t, starting, kindBefore)
	// The detector sees every frame, a session open or not; a change of its
	// decision is the session's to keep or not, before the lap store reads the
	// frame, as in v1's Engine._step.
	if match, changed := m.tracks.Observe(s.frame); changed && m.session != nil {
		m.applyTrack(match, s, t)
	}
	// After the track, as in v1's Engine._step: a change of the kind is the open
	// session's. A start always changes it, from none.
	if kindChanged && m.session != nil {
		m.applyKind(s, t)
	}

	ended := raceEnded(prev, s)
	if m.session != nil {
		m.observe(seen, s, t)
	}
	// v1's Engine._step decides the finish after the pits and the rules, before the
	// track, the kind and the lap store see the frame: _timed_finish, then the
	// FINISHED phase. Nothing those three change is read by it but the kind, which
	// it takes from before this frame (kindBefore), so deciding it here is the
	// same. It goes after the lap store so that the final lap's lap.completed comes
	// first and race.finished's total time and best lap count that lap, which
	// closes on the flag frame.
	m.observeFinish(seen, s, t, kindBefore)
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
		m.summaryPits(m.session.laps.pits)
		why := WhyQuit
		switch {
		case m.finish.kind == eventsv1.FinishKind_FINISH_KIND_FLAG:
			why = WhyFinished
		case m.finish.kind == eventsv1.FinishKind_FINISH_KIND_TIMED:
			why = WhyTimedFinish
		case s.phase == phaseMenu || s.phase == phaseAtTrack:
			why = WhyMenu
		}
		m.end(s, t, why)
	}

	if s.inRace() {
		driving := *s
		m.lastDriving = &driving
	} else if ended {
		m.lastDriving = nil
	}
	m.prev, m.prevTime = s, t
	m.runRules()
}

// runRules runs the rules (Rules) on the Context as the step or the tick left
// it, and adds what they give to m.out (paddock#89).
func (m *Machine) runRules() {
	if m.rules != nil {
		m.out = append(m.out, m.rules(&m.ctx)...)
	}
}

// EndOfInput ends the open session, if there is one, on the last frame, because
// the input came to its end, as a replayed recording does (END_REASON_INPUT_ENDED):
// paused, as of the pause's start, when the frames stop in a pause.
func (m *Machine) EndOfInput() []Output { return m.close(WhyInputEnded) }

// Stop ends the open session, if there is one, on the last frame, because ecu is
// stopping (END_REASON_ENGINEER_STOPPED): paused, as of the pause's start, when it
// stops in a pause. ecu keeps the session for the next run instead (Open,
// paddock#76); Stop is for a drive that keeps nothing.
func (m *Machine) Stop() []Output { return m.close(WhyStopped) }

func (m *Machine) close(why Why) []Output {
	m.out = nil
	switch {
	case m.session == nil || m.prev == nil:
	case m.pauses.open:
		m.endPaused(m.prev, m.prevTime, why)
	default:
		m.end(m.prev, m.prevTime, why)
	}
	return m.out
}

func (m *Machine) event(id string, t time.Time) ce.Event {
	return ce.Event{ID: id, Source: Source, Time: t}
}

// start opens a session on its first driving frame (v1's SessionStore.start_session).
// The first start of the run decides the session an earlier run left open
// (continue.go): a join that matches it continues it, and anything else ends it
// first. first says s is the run's first frame.
func (m *Machine) start(s *state, t time.Time, joined, first bool) {
	m.opened = true
	continued, carry := m.resume(s, t, joined, first)
	if continued {
		return
	}
	id := m.sessionID()
	m.session = newSession(id, s)
	started := &eventsv1.SessionStarted{
		SessionId:     id,
		Game:          eventsv1.Game_GAME_GT7,
		SessionType:   m.kinds.kind(),
		CarCode:       s.carCode,
		CarName:       carName(s.carCode),
		JoinedPartWay: joined,
		FirstSeq:      s.seq,
		StartedAt:     timestamppb.New(t),
		FuelAtStart:   s.fuel,
		FuelCapacity:  s.fuelCapacity,
	}
	if s.totalLaps > 0 {
		total := s.totalLaps
		started.TotalLaps = &total
	}
	// Joined part way, the lap it was joined on (v1's store.start_session,
	// paddock#75).
	if joined {
		lap := s.lap
		started.JoinedOnLap = &lap
	}
	m.session.summary = openSummary(started, s)
	m.out = append(m.out, Output{
		Event:   m.event(id+":session.started:1", t),
		Session: id,
		Started: started,
	})
	if carry != nil {
		m.carryTrack(*carry, s, t)
	}
}

// observe is v1's SessionStore.observe: a driving frame joins the lap in progress,
// and a crossing closes the lap.
func (m *Machine) observe(prev, s *state, t time.Time) {
	m.session.lastSeen, m.session.lastLap = t, s.lap
	if s.inRace() || racingFrame(prev, s) {
		m.session.lap.add(s)
		if p := s.racePosition(); p != 0 {
			m.session.lastPosition = p
		}
		if s.bestLapMs > 0 {
			m.session.laps.gt7BestMs = s.bestLapMs
		}
	}
	m.sawFrame(prev, s)
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
	// The lap an ecu restart fell in (paddock#76, R4): not all of it was seen, so
	// it is not complete, but GT7 still timed it, as v1's _flush_lap stored it. The
	// rules still hear of the crossing (timed).
	timed := complete
	if sess.gapLap > 0 && lap == sess.gapLap {
		complete, sess.gapLap = false, 0
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
	if timed {
		m.ctx.Lap = &ClosedLap{Lap: lap, TimeMs: max(s.lastLapMs, 0), Role: role}
	}
	if timed && s.lastLapMs > 0 {
		m.ctx.Lap.Counts = countsForBest(lap, s, role)
		ev.LapTimeMs = s.lastLapMs
	}
	if complete && s.lastLapMs > 0 {
		ev.CountsForBest = countsForBest(lap, s, role)
		sess.laps.timed = append(sess.laps.timed, timedLap{lap: lap, ms: s.lastLapMs, counts: ev.CountsForBest})
	}
	// Outside a field the position is left out, by the verdict at the lap's end.
	if p := s.racePosition(); p != 0 && !solo(m.kinds.kind()) {
		ev.Position = &p
		if timed {
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
	m.summaryLap(ev)
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
	// The running summary as of s, with the end's own fields (summary.go). The
	// final verdict on the kind decides the positions: none outside a field, and
	// a finishing position only for a lap race at the flag (contracts v0.1.4).
	m.summaryPits(m.pits.Status())
	summary := m.endSummary(s, m.now, why)
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
