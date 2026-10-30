// Package status is what ecu says it is doing (paddock#88): ecu.status,
// ecu.session, ecu.pit and ecu.fuel in the `live` KV bucket, each key carrying the
// source (contracts' subjects.yaml), and the body of /healthz. It is v1's
// `GET /api/v1/status` and `/healthz` (v1's status.py and App.status, App.idle and
// App.live_status in app.py, at v1.0.0), moved onto the bus so a late joiner reads
// the current value at once and then watches it.
//
// The step loop hands a Tracker the race state's read-only view after each step
// or tick (race.Machine.Live), which is a copy under a lock and nothing else: no
// encoding and no network on the frame path. A Writer, on its own goroutine, reads
// the tracker once a second, encodes the values and writes the ones that changed.
//
// Idle is v1's: busy while driving frames (in a race, or paused in one) arrive,
// that is while the last one is under the telemetry timeout old, on the wall
// clock of when ecu took it. A replay or a demo GT7 plays keeps driving frames
// coming, so it is busy too, and its activity says which. A session left open by
// a feed that stopped is idle.
package status

import (
	"math"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/race"
	fuelrule "github.com/those-who-run/ecu/internal/rules/fuel"
)

// Config is what the status says of the ecu run itself.
type Config struct {
	// Src is the source the keys carry: live, parity or replay-<run>.
	Src string
	// Version is ecu's version.
	Version string
	// TelemetryTimeout is the time with no packet that counts as the feed lost,
	// and with no driving frame as idle (v1's GT7_TELEMETRY_TIMEOUT_SECS).
	TelemetryTimeout time.Duration
	// Listening says the telemetry listener holds its port; nil is always true,
	// as for a replay, which reads a recording.
	Listening func() bool
	// Gauge is the rule engine's gauge of a name (rules.Engine.Gauge), read for
	// the fuel model's laps remaining, the fuel rule's gauge (paddock#92, R8);
	// nil has none. It is called on the step loop's
	// goroutine only, from Observe.
	Gauge func(name string) (float64, bool)
	// Now is the wall clock; nil is time.Now.
	Now func() time.Time
}

// Tracker holds what the status shows, as of the last step. It is safe for
// concurrent use: the step loop writes it, the writer and /healthz read it.
type Tracker struct {
	cfg     Config
	started time.Time

	mu        sync.Mutex
	live      race.Live
	laps      float64 // the fuel's laps remaining, when lapsKnown
	lapsKnown bool
	races     int32
	packetAt  time.Time // when the last packet was taken; zero before one
	drivingAt time.Time // ... and the last driving frame
	stopped   bool      // the drive has ended: nothing is driving any more
}

// New is a tracker for an ecu run that starts now.
func New(cfg Config) *Tracker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Listening == nil {
		cfg.Listening = func() bool { return true }
	}
	return &Tracker{cfg: cfg, started: cfg.Now()}
}

// Observe takes the race state's view after a step or a tick, and the outputs it
// gave. It only copies: the step loop calls it after every step.
func (t *Tracker) Observe(l race.Live, outs []race.Output) {
	var laps float64
	var known bool
	if t.cfg.Gauge != nil && l.Session.ID != "" {
		laps, known = t.cfg.Gauge(fuelrule.GaugeName)
	}
	now := t.cfg.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if l.Packets != t.live.Packets {
		t.packetAt = now
		// v1's App._handle: a frame in a race or paused in one is driving.
		if l.Phase == telemetryv1.Phase_PHASE_IN_RACE || l.Phase == telemetryv1.Phase_PHASE_PAUSED {
			t.drivingAt = now
		}
	}
	for _, o := range outs {
		// A session continued after an ecu restart is a start of this run's, as
		// v1 counted it (paddock#76).
		if o.Started != nil || o.Continued != nil {
			t.races++
		}
	}
	t.live, t.laps, t.lapsKnown = l, laps, known
}

// Stop says the drive has ended, with the session it held: from now on ecu is
// idle, whatever the time since the last driving frame.
func (t *Tracker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
}

// Values are the four values, as of one moment: Session, Pit and Fuel are nil
// while no session is open, when the keys have no value.
type Values struct {
	Status  *statev1.EcuStatus
	Session *statev1.LiveSession
	Pit     *statev1.PitState
	Fuel    *statev1.FuelState
}

// Values are the values as of now.
func (t *Tracker) Values() Values {
	now := t.cfg.Now()
	listening := t.cfg.Listening()
	// A copy under the lock, so the step loop never waits on the encoding.
	t.mu.Lock()
	l, packetAt, races, laps, lapsKnown := t.live, t.packetAt, t.races, t.laps, t.lapsKnown
	idle, idleSecs := t.idle(now)
	t.mu.Unlock()
	st := &statev1.EcuStatus{
		Source:             t.cfg.Src,
		RunId:              l.Run,
		Version:            t.cfg.Version,
		StartedAt:          timestamppb.New(t.started),
		Activity:           activity(l, idle),
		Idle:               idle,
		IdleSecs:           idleSecs,
		TelemetryConnected: !packetAt.IsZero() && now.Sub(packetAt) < t.cfg.TelemetryTimeout,
		TelemetryListening: listening,
		Packets:            l.Packets,
		RacesStarted:       races,
		UpdatedAt:          timestamppb.New(now),
	}
	if !packetAt.IsZero() {
		st.LastPacketAgeSecs = proto.Float64(round1(now.Sub(packetAt).Seconds()))
	}
	v := Values{Status: st}
	s := l.Session
	if s.ID == "" {
		return v
	}
	v.Session = liveSession(s)
	v.Pit = &statev1.PitState{SessionId: s.ID, InPitLane: s.InPitLane, Stint: s.Stint, Stops: s.Stops}
	v.Fuel = &statev1.FuelState{SessionId: s.ID, FuelLevel: round3(s.FuelLevel), FuelCapacity: round3(s.FuelCapacity)}
	if lapsKnown {
		v.Fuel.LapsRemaining = proto.Float64(laps)
	}
	return v
}

// idle is v1's App.idle: whether a restart would cut into a drive, and the
// seconds since the last driving frame (since the start before one), rounded to
// 1 decimal; 0 while busy.
func (t *Tracker) idle(now time.Time) (bool, float64) {
	since := t.started
	if !t.drivingAt.IsZero() {
		since = t.drivingAt
	}
	d := now.Sub(since)
	if !t.stopped && !t.drivingAt.IsZero() && d < t.cfg.TelemetryTimeout {
		return false, 0
	}
	return true, round1(max(d.Seconds(), 0))
}

// activity is v1's status.activity: idle exactly when idle is; otherwise the
// replay or the demo being ignored, a pause, or driving, a start the start check
// still holds included.
func activity(l race.Live, idle bool) statev1.Activity {
	switch {
	case idle:
		return statev1.Activity_ACTIVITY_IDLE
	case l.Ignoring == eventsv1.DiscardReason_DISCARD_REASON_REPLAY:
		return statev1.Activity_ACTIVITY_REPLAY
	case l.Ignoring == eventsv1.DiscardReason_DISCARD_REASON_DEMO:
		return statev1.Activity_ACTIVITY_DEMO
	case l.Phase == telemetryv1.Phase_PHASE_PAUSED:
		return statev1.Activity_ACTIVITY_PAUSED
	default:
		return statev1.Activity_ACTIVITY_DRIVING
	}
}

// liveSession is v1's status.live_session.
func liveSession(s race.LiveSession) *statev1.LiveSession {
	ls := &statev1.LiveSession{
		SessionId:   s.ID,
		SessionType: s.Type,
		CarName:     s.CarName,
	}
	if ls.SessionType == eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED {
		ls.SessionType = eventsv1.SessionType_SESSION_TYPE_UNKNOWN
	}
	if s.TrackName != "" {
		ls.Track = &statev1.LiveTrack{Name: s.TrackName, Sure: s.TrackSure}
		if s.TrackID != 0 {
			ls.Track.TrackId = proto.Int32(s.TrackID)
		}
	}
	if !s.HasFrame {
		return ls
	}
	ls.Lap = proto.Int32(s.Lap)
	if s.TotalLaps > 0 {
		ls.TotalLaps = proto.Int32(s.TotalLaps)
	}
	// Only where the results card shows it, and not when GT7 sent none.
	if s.PositionShown && s.Position > 0 {
		ls.Position = proto.Int32(s.Position)
	}
	if s.PositionShown && s.TotalCars > 0 {
		ls.TotalCars = proto.Int32(s.TotalCars)
	}
	return ls
}

// Health is the status as /healthz reports it, as of now (paddock#88, R4): v1's
// /healthz fields that are still ecu's, by v1's names, with the activity and the
// last packet's age (paddock#87's R6), null before the first.
func (t *Tracker) Health() map[string]any {
	st := t.Values().Status
	var age any
	if st.LastPacketAgeSecs != nil {
		age = st.GetLastPacketAgeSecs()
	}
	return map[string]any{
		"idle":                 st.GetIdle(),
		"idle_secs":            st.GetIdleSecs(),
		"activity":             activityNames[st.GetActivity()],
		"telemetry_connected":  st.GetTelemetryConnected(),
		"telemetry_listening":  st.GetTelemetryListening(),
		"last_packet_age_secs": age,
		"packets":              st.GetPackets(),
		"races_started":        st.GetRacesStarted(),
	}
}

// activityNames are v1's names for each activity.
var activityNames = map[statev1.Activity]string{
	statev1.Activity_ACTIVITY_IDLE: "idle", statev1.Activity_ACTIVITY_DRIVING: "driving",
	statev1.Activity_ACTIVITY_PAUSED: "paused", statev1.Activity_ACTIVITY_REPLAY: "replay",
	statev1.Activity_ACTIVITY_DEMO: "demo",
}

// round1 and round3 are Python's round(x, 1) and round(x, 3), as v1 rounds: the
// decimal nearest x's exact binary value.
func round1(x float64) float64 { return roundTo(x, 1) }
func round3(x float64) float64 { return roundTo(x, 3) }

func roundTo(x float64, places int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	// FormatFloat always gives a number ParseFloat reads.
	r, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', places, 64), 64)
	return r
}
