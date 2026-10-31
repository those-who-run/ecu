package race

import (
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/pit"
	"github.com/those-who-run/ecu/internal/track"
)

// The rules' view of a frame (paddock#89). v1's rules were told about the race
// through six hooks (on_track, on_session, on_pit, on_bridge, on_finish, on_end)
// and read the rest off RaceState; here the machine fills one read-only Context on
// every Step, and internal/rules hands it to each rule. Each value is as v1's
// rules saw it at their place in Engine._step: after the pits, before the timed
// finish, the track, the kind and the lap store. What v1 gave the rules later in
// the step (the lap closed on the frame, the timed finish, the end without a
// flag) is here too, as of the end of the step, so a rule reads it on the same
// frame v1's would have.

// View is a frame as the rules read it: v1's RaceState. Frame has every field
// GT7 sent (the temperatures among them); the rest are the race state's reading
// of it.
type View struct {
	Seq      uint32
	T        float64 // the packet's time on the source's clock, in seconds (v1's state.t)
	PacketID int32
	Phase    telemetryv1.Phase
	// Lap is GT7's current lap and TotalLaps its lap total, 0 for a timed session.
	Lap, TotalLaps int32
	// LastLapMs and BestLapMs are GT7's own, -1 or 0 when it has none. GT7 keeps
	// its best across sessions at a track.
	LastLapMs, BestLapMs int32
	Fuel                 float64
	// Position is GT7's race position as it sent it, -1 outside a race; it is not
	// settled (RacePosition is v1's state.position). TotalCars is -1 in a time
	// trial.
	Position, TotalCars int32
	SpeedKph            float64
	CarsOnTrack         bool
	Throttle, Brake     int32 // 0 to 255
	Frame               *telemetryv1.Frame
	// Time is when ecu received the frame, on the wall clock (the replay's clock
	// in a replay): a callout raised as of this frame is created at it.
	Time time.Time
}

// InRace is v1's state.in_race: the phase is IN_RACE.
func (v *View) InRace() bool { return v.Phase == phaseInRace }

// HasLapCount is v1's has_lap_count: the race has a lap total.
func (v *View) HasLapCount() bool { return v.TotalLaps > 0 }

// LapsLeft is v1's laps_left: the laps still to run, this one included, and
// false for a timed session.
func (v *View) LapsLeft() (int32, bool) {
	if v.TotalLaps <= 0 {
		return 0, false
	}
	return max(v.TotalLaps-v.Lap+1, 0), true
}

// RacePosition is v1's state.position: the position, or 0 for none.
func (v *View) RacePosition() int32 {
	if v.Position > 0 {
		return v.Position
	}
	return 0
}

func (s *state) view() View {
	return View{
		Seq: s.seq, T: s.t, PacketID: s.packetID, Phase: s.phase,
		Lap: s.lap, TotalLaps: s.totalLaps, LastLapMs: s.lastLapMs, BestLapMs: s.bestLapMs,
		Fuel: s.fuel, Position: s.position, TotalCars: s.totalCars, SpeedKph: s.speedKph,
		CarsOnTrack: s.carsOnTrack, Throttle: s.throttle, Brake: s.brake, Frame: s.frame,
		Time: s.received,
	}
}

// LapCompleted is v1's lap_completed: the lap counter went up by one while
// driving, from prev to cur.
func LapCompleted(prev, cur *View) bool {
	return prev != nil && lapCompleted(prev.state(), cur.state())
}

// LapTimePosted is v1's lap_time_posted: a lap time is there for the lap that
// just ended.
func LapTimePosted(prev, cur *View) bool {
	return prev != nil && lapTimePosted(prev.state(), cur.state())
}

// RacingFrame is v1's racing_frame: cur continues a race from prev, the flag
// frame included.
func RacingFrame(prev, cur *View) bool {
	return prev != nil && racingFrame(prev.state(), cur.state())
}

// state is the part of a view the transition helpers read.
func (v *View) state() *state {
	return &state{seq: v.Seq, t: v.T, phase: v.Phase, lap: v.Lap, totalLaps: v.TotalLaps, lastLapMs: v.LastLapMs}
}

// Kind is the session's kind as the rules read it (v1's rule.session).
type Kind struct {
	// Type is the kind, SESSION_TYPE_UNKNOWN before a verdict.
	Type eventsv1.SessionType
	// Field: the kind has other cars racing for position. The rules read it,
	// never GT7's cars-on-track flag, which is true in a time trial too
	// (paddock#146).
	Field bool
}

// ClosedLap is the complete lap that closed on the frame (lap.completed), as the
// lap model decided it (laps.go, paddock#83).
type ClosedLap struct {
	Lap      int32
	TimeMs   int32 // 0 when GT7 posted no time
	Role     pit.Role
	Counts   bool  // it counts for the session's best (counts_for_best)
	Position int32 // the position at the line, 0 for none or outside a field
}

// PostedLap is a lap time GT7 posted on the frame (v1's lap_time_posted, against
// Prev), whether or not a lap closed on it: SUGO's replay posts lap 12 on frame
// 864, a counter jump that closes nothing, and v1's rules counted it. It can post
// on more than one frame for one lap: a rule keeps the first, as v1's did.
type PostedLap struct {
	// Lap is v1's posted_lap: the counter less one, and 1 at least, so a time
	// posted while the counter still reads 1 is lap 1's.
	Lap    int32
	TimeMs int32 // GT7's last lap time
	// StartLap is a lap GT7 leaves out of its best, as race_summary's _start_lap
	// read it: posted against a frame (Prev) on lap 1 or before, in a session with
	// a lap total or with GT7's best still blank on this frame. It reads the frame
	// before, not Lap: a counter jumping from 1 to 3 posts lap 2, which v1 left
	// out. lap_time's own start_lap is on ClosedLap.Counts.
	StartLap bool
}

// TimedLap is a complete lap with a time, closed before the frame.
type TimedLap struct {
	Lap    int32
	Ms     int32
	Counts bool
}

// Context is one frame as the rules read it. The machine refills it on each Step;
// it is valid until the next, and a rule never changes it.
type Context struct {
	// Run is the machine's run id; Session the open session's id at the rules'
	// place in the step (the session a frame ends is still open there), "" with
	// none. Time is when ecu received the frame: a callout's created_at.
	Run, Session string
	Time         time.Time

	// Cur is this frame. Prev is the frame the rules compare against, nil before
	// the first: the frame before, or the last driving frame across a pause or a
	// loading screen (v1's seen, Engine.last_driving).
	Cur  View
	Prev *View
	// Bridged is PHASE_LOADING or PHASE_PAUSED when Prev is the last driving frame
	// across frames of that phase (v1's on_bridge), and PHASE_UNSPECIFIED
	// otherwise.
	Bridged telemetryv1.Phase
	// Racing is where v1 called a rule's evaluate: Cur is in a race, or continues
	// one from Prev (the flag frame).
	Racing bool

	// Ignored: the machine did not step this frame (v1 never called _step on
	// it), as after a session ended paused until the next start (paddock#77):
	// only the rules that watch the feed see it, and the rest of the context is
	// only Run, Time, Cur and Restored.
	Ignored bool

	// Started: a session started or restarted on this frame. The engine resets
	// the rules before it hands them the frame (v1's reset). Joined: the session
	// was joined part way (v1's rule.joined), set from its start.
	Started, Joined bool

	// Kind is the session's kind before this frame, as v1's rules read it.
	Kind Kind
	// Track is the track applied to the session at the rules' place, nil for none.
	// A new verdict is a new pointer: a rule that compares it with the last one it
	// saw has v1's on_track, a frame later, as v1's rules had it.
	Track *track.Match
	// TrackChanges are the verdicts applied to the session since the rules' place
	// on the frame before, oldest first, each as v1 called on_track with it: the
	// last of them is Track. A change applied after the rules' place on a frame
	// is here on the next, as v1's rules saw it on their next evaluate.
	TrackChanges []*track.Match
	// Pits is the pit status as last applied to the session, after this frame's
	// pit step (v1's rule.pits): Pits.InPitLane holds from the entry's frame to
	// the exit's. PitChange is this frame's entry, exit or entry withdrawn (v1's
	// on_pit), pit.Unchanged otherwise. Both are zero with no session open.
	Pits      pit.Status
	PitChange pit.Change

	// FinalLap is race.final_lap on this frame; Settled the race.position settled
	// on it, nil for none. Cur.Position is the raw position, which the position
	// callout needs (paddock#84, AC4).
	FinalLap bool
	Settled  *eventsv1.RacePositionChanged

	// Posted is the lap time posted on this frame, nil for none. Lap is the
	// complete lap that closed on it (lap.completed), nil for none.
	Posted *PostedLap
	Lap    *ClosedLap

	// Finished: the race has finished, by the flag or timed, as of this frame
	// (v1's Engine.finished). Finish is the frame the race finished as of, on the
	// frame the finish was found (where race.finished is): the flag frame, or the
	// line crossing of a timed finish (v1's on_finish), with FinishKind; nil on
	// every other frame. A finish found with no session open is here too, as v1's
	// rules had it.
	Finished   bool
	Finish     *View
	FinishKind eventsv1.FinishKind
	// EndedEarly is the last driving frame when the session ended on this frame
	// without a flag or a finish, quit or back to a menu (v1's on_end), nil
	// otherwise. Kind is still the kind before this frame. A restart, a new
	// start and the end of the input are not this, as in v1.
	EndedEarly *View

	// The telemetry watchdog (feed.go, paddock#87), what the telemetry rule
	// (paddock#98) reads in place of v1's TelemetryVoice.observe. Tick: no frame
	// came, the watchdog called the feed lost (Machine.Tick); Cur is the last
	// frame, which the rules saw already, Time is the tick's, and the engine runs
	// only the rules that watch the feed (rules.Watcher). Lost is the loss on a
	// tick, nil otherwise: its Phase is the last frame's, as v1's voice read
	// engine.prev. Restored is the loss's end on the first frame after it, nil
	// otherwise: its LostPhase is the lost frame's phase.
	Tick     bool
	Lost     *eventsv1.TelemetryLost
	Restored *eventsv1.TelemetryRestored

	timed []timedLap // the session's timed laps before this frame
}

// HasField is v1's Rule.has_field for Cur: other cars are racing for position,
// by the kind before this frame.
func (c *Context) HasField() bool { return c.HasFieldOn(&c.Cur) }

// HasFieldOn is v1's Rule.has_field(v), by the kind before this frame, never
// GT7's cars-on-track flag (paddock#146): the kind's field once there is a
// verdict, else the field the kind reads from v alone.
func (c *Context) HasFieldOn(v *View) bool {
	if c.Kind.Type != eventsv1.SessionType_SESSION_TYPE_UNKNOWN {
		return c.Kind.Field
	}
	return frameField(v.TotalLaps, v.TotalCars, v.RacePosition())
}

// TimedLaps are the session's complete laps with a time, closed before this
// frame, in the order they closed. It copies them.
func (c *Context) TimedLaps() []TimedLap {
	out := make([]TimedLap, len(c.timed))
	for i, l := range c.timed {
		out[i] = TimedLap{Lap: l.lap, Ms: l.ms, Counts: l.counts}
	}
	return out
}

// QuickestCountingMs is the quickest lap that counts for the best, closed before
// this frame, 0 for none: v1's lap_time own_best_ms.
func (c *Context) QuickestCountingMs() int32 {
	var best int32
	for _, l := range c.timed {
		if l.counts && (best == 0 || l.ms < best) {
			best = l.ms
		}
	}
	return best
}

// PrevBestMs is v1's lap_time prev_best for the lap closed on this frame: GT7's
// raw best on Prev, which can be from an earlier session at the track, else the
// quickest counting lap before it (paddock#83's decision). It is not lap.best.
func (c *Context) PrevBestMs() int32 {
	if c.Prev != nil && c.Prev.BestLapMs > 0 {
		return c.Prev.BestLapMs
	}
	return c.QuickestCountingMs()
}

// Context is the frame the last Step took, as the rules read it. Valid until the
// next Step.
func (m *Machine) Context() *Context { return &m.ctx }

// ruleContext fills the context at the rules' place in Step (observeRace's),
// keeping what the step has set on it before: the pit change, the final lap and
// the settled position. prev is the frame before s, seen the frame the rules
// compare against.
func (m *Machine) ruleContext(prev, seen, s *state, t time.Time, starting bool, kind kindVerdict) {
	c := &m.ctx
	c.Run, c.Time, c.Cur, c.Started, c.Joined = m.run, t, s.view(), starting, m.joined
	if seen != nil {
		v := seen.view()
		c.Prev = &v
		if seen != prev {
			c.Bridged = prev.phase
		}
	}
	c.Racing = s.inRace() || racingFrame(seen, s)
	c.Kind = Kind{Type: kind.kind, Field: kind.field}
	if lapTimePosted(seen, s) {
		c.Posted = &PostedLap{
			Lap:      max(s.lap-1, 1),
			TimeMs:   s.lastLapMs,
			StartLap: seen.lap <= 1 && (s.totalLaps > 0 || s.bestLapMs <= 0),
		}
	}
	c.TrackChanges, m.trackChanges = m.trackChanges, nil
	if sess := m.session; sess != nil {
		c.Session, c.Track, c.Pits, c.timed = sess.id, sess.track, sess.laps.pits, sess.laps.timed
	}
}
