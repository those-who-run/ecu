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
// _flush_lap and end_session in store.py. What else v1's engine reads (replays and
// demo races held at the start, joining part way and continuing after a restart,
// the frozen pause, the timed-race finish, pits, track and session kind) is not
// here yet.
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

// Why is why a session ended. The contracts' EndReason has no value for a session
// ended by a restart or by the next start, so the machine keeps its own (End.Why)
// beside the one on the wire.
type Why int

// Why a session ended.
const (
	// WhyFinished is the chequered flag.
	WhyFinished Why = iota + 1
	// WhyQuit is back to a menu or the track screen, or a loading screen with the
	// lap counter reset, before the flag (v1's race_ended).
	WhyQuit
	// WhyRestart is GT7 jumping back to a lower lap while driving (v1's
	// race_restarted).
	WhyRestart
	// WhyStart is another start while the session was open (v1's race_started).
	WhyStart
	// WhyStopped is the input ending, or the machine being closed, with the
	// session open.
	WhyStopped
)

var whyNames = map[Why]string{
	WhyFinished: "finished", WhyQuit: "quit", WhyRestart: "restart", WhyStart: "start", WhyStopped: "stopped",
}

func (w Why) String() string {
	if name, ok := whyNames[w]; ok {
		return name
	}
	return "Why(" + strconv.Itoa(int(w)) + ")"
}

// Reason is the contracts' EndReason for w: unknown for a restart or a start, which
// it has no value for.
func (w Why) Reason() eventsv1.EndReason {
	switch w {
	case WhyFinished:
		return eventsv1.EndReason_END_REASON_FINISHED
	case WhyQuit:
		return eventsv1.EndReason_END_REASON_QUIT
	case WhyStopped:
		return eventsv1.EndReason_END_REASON_ENGINEER_STOPPED
	default:
		return eventsv1.EndReason_END_REASON_UNKNOWN
	}
}

// Output is a frame or an event, with its CloudEvents id and time: exactly one of
// Frame, Started, Lap and Ended is set, each the contracts v0.1 message of that
// name. Start and End carry what paddock#33 asks of the session events that the
// v0.1 messages have no field for.
type Output struct {
	// Event is the ce-id (and Nats-Msg-Id), ce-source and ce-time, the id built
	// as contracts' subjects.yaml says: <run>:frame:<seq> for a frame, and
	// <sid>:session.started:1, <sid>:lap.completed:<lap> and
	// <sid>:session.ended:1 for the events.
	Event ce.Event
	// Session is the id of the session an event belongs to; "" for a frame.
	Session string

	Frame   *telemetryv1.Frame
	Started *eventsv1.SessionStarted
	Lap     *eventsv1.LapCompleted
	Ended   *eventsv1.SessionEnded

	// Start is set with Started.
	Start *Start
	// End is set with Ended.
	End *End
}

// Start is what a session start carries beyond SessionStarted.
type Start struct {
	// FuelAtStart is the fuel in the tank on the session's first frame, in litres.
	FuelAtStart float64
}

// End is what a session end carries beyond SessionEnded.
type End struct {
	// Why is why it ended; SessionEnded's reason is Why.Reason().
	Why Why
	// LastSeq is the seq of the session's last frame: the frame it ended on.
	LastSeq uint32
	// FuelUsed is the fuel its laps used, in litres: the sum of each lap's fuel
	// used, rounded to 3 decimals, as v1's end_session adds it up.
	FuelUsed float64
	// FinalPosition is the position on the last frame, or the last position seen
	// in the session when that frame has none; 0 when there was none (v1's
	// final_position).
	FinalPosition int32
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
	finished    bool   // the open session reached the flag (v1's Engine.finished)
	session     *session

	out []Output
}

// session is the open session: v1's store.Session, cut to what the events need.
type session struct {
	id           string
	lapStartFuel float64
	lap          lapFrames
	completed    int32
	lapsFuelUsed float64 // the sum of each lap's fuel used, in the order they closed
	lastPosition int32   // the last race position seen, 0 for none
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
	return &Machine{ids: ids, run: ids().String()}
}

// Run is the machine's run id, a ULID: the <run> in each frame's id.
func (m *Machine) Run() string { return m.run }

// Step takes the next packet and returns its frame, then the events it gives rise
// to, in the order they happened: the end of a session the packet's start closes,
// the start, a lap closed, and the end of the session.
func (m *Machine) Step(r Received) []Output {
	m.seq++
	s := newState(m.seq, &r.Packet)
	m.out = []Output{{
		Event: m.event(m.run+":frame:"+strconv.FormatUint(uint64(m.seq), 10), r.Time),
		Frame: frame(m.seq, r.T, s.phase, &r.Packet),
	}}

	// Engine._step, the parts that open, close and end.
	prev := m.prev
	restarted := raceRestarted(prev, &s)
	if raceStarted(prev, &s) || restarted {
		if m.session != nil && prev != nil {
			why := WhyStart
			if restarted {
				why = WhyRestart
			}
			m.end(prev, m.prevTime, why)
		}
		m.lastDriving = nil
		m.finished = false
		m.start(&s, r.Time, joinedPartWay(prev, &s))
	}

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
	if s.phase == phaseFinished {
		m.finished = true
	}
	ended := raceEnded(prev, &s)
	if m.session != nil {
		m.observe(seen, &s, r.Time)
		if ended {
			why := WhyQuit
			if m.finished {
				why = WhyFinished
			}
			m.end(&s, r.Time, why)
		}
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

// Close ends the open session, if there is one, on the last frame, as stopped: the
// input has ended (the end of a recording) or ecu is stopping.
func (m *Machine) Close() []Output {
	m.out = nil
	if m.session != nil && m.prev != nil {
		m.end(m.prev, m.prevTime, WhyStopped)
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
		SessionType:   sessionType(s),
		CarCode:       s.carCode,
		JoinedPartWay: joined,
		FirstSeq:      s.seq,
		StartedAt:     timestamppb.New(t),
	}
	if s.totalLaps > 0 {
		total := s.totalLaps
		started.TotalLaps = &total
	}
	m.out = append(m.out, Output{
		Event:   m.event(id+":session.started:1", t),
		Session: id,
		Started: started,
		Start:   &Start{FuelAtStart: s.fuel},
	})
}

// sessionType is v1's store.session_type: what v1 stored as the session's type.
// It reads GT7's cars-on-track flag on the first frame and nothing else, as v1's
// store did; the flag being off says nothing about a replay here (v1's
// sessionkind.py: it was only set from 2026-10-16, and one race mode never sets
// it).
func sessionType(s *state) eventsv1.SessionType {
	switch {
	case s.carsOnTrack:
		return eventsv1.SessionType_SESSION_TYPE_RACE
	case s.totalLaps == 0:
		return eventsv1.SessionType_SESSION_TYPE_TIME_TRIAL
	default:
		return eventsv1.SessionType_SESSION_TYPE_PRACTICE
	}
}

// observe is v1's SessionStore.observe: a driving frame joins the lap in progress,
// and a crossing closes the lap.
func (m *Machine) observe(prev, s *state, t time.Time) {
	if s.inRace() || racingFrame(prev, s) {
		m.session.lap.add(s)
		if p := s.racePosition(); p != 0 {
			m.session.lastPosition = p
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
	if complete && s.lastLapMs > 0 {
		ev.LapTimeMs = s.lastLapMs
	}
	if p := s.racePosition(); p != 0 {
		ev.Position = &p
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
	final := s.racePosition()
	if final == 0 {
		final = sess.lastPosition
	}
	summary := &eventsv1.SessionSummary{LapsCompleted: sess.completed}
	if why == WhyFinished && final != 0 {
		summary.FinishingPosition = &final
	}
	m.out = append(m.out, Output{
		Event:   m.event(sess.id+":session.ended:1", t),
		Session: sess.id,
		Ended:   &eventsv1.SessionEnded{SessionId: sess.id, Reason: why.Reason(), Summary: summary},
		End:     &End{Why: why, LastSeq: s.seq, FuelUsed: round3(sess.lapsFuelUsed), FinalPosition: final},
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
