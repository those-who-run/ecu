package rules

import (
	"bytes"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/pit"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/salsa"
)

// stub is a rule for the engine's tests: it raises what raise returns, and
// records what it was given.
type stub struct {
	resets int
	raise  func(ctx *race.Context) []Callout
}

func (s *stub) Reset() { s.resets++ }

func (s *stub) Evaluate(ctx *race.Context) []Callout {
	if s.raise == nil {
		return nil
	}
	return s.raise(ctx)
}

// testEngine is an engine with the rules given, under the names given, with
// their v1 defaults (or settings changed by set), and no registry.
func testEngine(t *testing.T, logger *slog.Logger, rules ...any) *Engine {
	t.Helper()
	e, err := New(Config{QueueMaxAgeSecs: QueueMaxAgeSecs}, logger)
	if err != nil {
		t.Fatal(err)
	}
	e.rules = nil
	var name string
	for _, x := range rules {
		if n, ok := x.(string); ok {
			name = n
			continue
		}
		s, err := settings(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		e.rules = append(e.rules, slot{name: name, s: s, rule: x.(Rule)})
	}
	return e
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// replay steps a v1 recording through a machine with a fixed run id and e.
func replay(t *testing.T, name string, e *Engine) []race.Output {
	t.Helper()
	path := filepath.Join("..", "track", "testdata", "v1", name+".jsonl.gz")
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join(salsaDir(t), name+".jsonl.gz")
	}
	src, err := salsa.OpenRecording(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	m := race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'})))
	var out []race.Output
	var t0 float64
	for n := 0; ; n++ {
		r, err := src.Next(t.Context())
		if err != nil {
			break
		}
		if n == 0 {
			t0 = r.T
		}
		at := epoch.Add(time.Duration((r.T - t0) * float64(time.Second)))
		out = append(out, m.Step(race.Received{Received: r, Time: at})...)
		out = append(out, e.Step(m.Context())...)
	}
	return out
}

func callouts(out []race.Output) []*eventsv1.Callout {
	var cs []*eventsv1.Callout
	for _, o := range out {
		if o.Callout != nil {
			cs = append(cs, o.Callout)
		}
	}
	return cs
}

// AC1: the rules run after the pit detector. Replaying Suzuka's pit stop with
// every rule stubbed, the rule in fuel's place sees the car in the pit lane on
// the frame of the entry, and on the frame after the entry cut, where the
// crossing that closes the in-lap comes.
func TestThePitsAreUpdatedBeforeTheRules(t *testing.T) {
	var entry, after uint32
	var inLaneAtEntry, inLaneAfter bool
	fuel := &stub{raise: func(ctx *race.Context) []Callout {
		switch {
		case ctx.PitChange == pit.Entered:
			entry, inLaneAtEntry = ctx.Cur.Seq, ctx.Pits.InPitLane
		case entry != 0 && after == 0 && ctx.Racing:
			after, inLaneAfter = ctx.Cur.Seq, ctx.Pits.InPitLane
		}
		return nil
	}}
	var stubs []any
	for _, name := range Order {
		var r Rule = &stub{}
		if name == "fuel" {
			r = fuel
		}
		stubs = append(stubs, name, r)
	}
	replay(t, "real-nuc-suzuka-pit-stop-2026-10-15", testEngine(t, nil, stubs...))
	if entry == 0 || !inLaneAtEntry {
		t.Fatalf("the entry (frame %d) reached the fuel rule with the car in the lane %v", entry, inLaneAtEntry)
	}
	if !inLaneAfter {
		t.Errorf("the frame after the entry cut (%d) reached the fuel rule with the car out of the lane", after)
	}
}

// AC2 and R4: a rule that panics on every frame leaves the other rules'
// callouts as they were on the two-lap race, and is counted once a frame and
// logged with its name.
func TestARuleThatPanicsLeavesTheOthers(t *testing.T) {
	final := func() Rule { return &finalLap{} }
	want := callouts(replay(t, "real-race-2laps-2026-10-14", testEngine(t, nil, "final_lap", final())))
	if len(want) == 0 {
		t.Fatal("no callout on the two-lap race")
	}
	var log bytes.Buffer
	boom := &stub{raise: func(*race.Context) []Callout { panic("boom") }}
	e := testEngine(t, slog.New(slog.NewJSONHandler(&log, nil)), "lap_time", boom, "final_lap", final())
	out := replay(t, "real-race-2laps-2026-10-14", e)
	got := callouts(out)
	if len(got) != len(want) {
		t.Fatalf("%d callouts with a rule that panics, %d without", len(got), len(want))
	}
	for i := range got {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("callout %d: %v, want %v", i, got[i], want[i])
		}
	}
	frames := 0
	for _, o := range out {
		if o.Frame != nil {
			frames++
		}
	}
	if n := e.Stats().Panics["lap_time"]; n != uint64(frames) {
		t.Errorf("%d panics counted, want one a frame, %d", n, frames)
	}
	if !strings.Contains(log.String(), `"rule":"lap_time"`) {
		t.Errorf("the panic is not logged with the rule's name: %.200s", log.String())
	}
}

// finalLap raises on race.final_lap, as finallap does (the package can't be
// imported here: it imports this one).
type finalLap struct{ stub }

func (*finalLap) Evaluate(ctx *race.Context) []Callout {
	if ctx.FinalLap {
		return []Callout{{Kind: "template"}}
	}
	return nil
}

// AC4 and R7: two replays of SUGO's two stops publish the same callouts, ids,
// at, created_at and expires_at included: everything comes from the frames and
// the run id.
func TestTwoReplaysGiveTheSameCallouts(t *testing.T) {
	every := func() Rule {
		return &stub{raise: func(ctx *race.Context) []Callout {
			if ctx.Cur.Seq%97 == 0 {
				return []Callout{{Kind: "a"}, {Kind: "b", Priority: 5}}
			}
			return nil
		}}
	}
	run := func() []*eventsv1.Callout {
		return callouts(replay(t, "real-nuc-sugo-two-stops-2026-10-18",
			testEngine(t, nil, "position", every(), "final_lap", &finalLap{})))
	}
	first, second := run(), run()
	if len(first) < 10 || len(first) != len(second) {
		t.Fatalf("%d callouts, then %d", len(first), len(second))
	}
	ids := map[string]bool{}
	for i := range first {
		a, b := first[i], second[i]
		if a.GetId() != b.GetId() || a.GetAt() != b.GetAt() ||
			!a.GetCreatedAt().AsTime().Equal(b.GetCreatedAt().AsTime()) ||
			!a.GetExpiresAt().AsTime().Equal(b.GetExpiresAt().AsTime()) {
			t.Errorf("callout %d: %v, then %v", i, a, b)
		}
		if ids[a.GetId()] {
			t.Errorf("id %s twice", a.GetId())
		}
		ids[a.GetId()] = true
	}
}

// frame is a context for the pit lane's tests: frame seq, at second seq, in the
// lane or not.
func frame(seq uint32, inLane bool) *race.Context {
	return &race.Context{
		Run: "run", Session: "sid", Time: epoch.Add(time.Duration(seq) * time.Second),
		Cur: race.View{Seq: seq}, Racing: true, Pits: pit.Status{InPitLane: inLane},
	}
}

// The pit lane (contracts' PitLane, v1's _quiet and _release): raised in the
// lane, a speak callout is published at once, a drop one never, and a hold one
// at the exit, as raised there, keeping its at and id.
func TestThePitLaneSpeaksHoldsAndDrops(t *testing.T) {
	raise := &stub{raise: func(ctx *race.Context) []Callout {
		if ctx.Cur.Seq <= 3 {
			return []Callout{{Kind: "k"}}
		}
		return nil
	}}
	e := testEngine(t, nil, "lap_time", raise, "fuel", raise, "position", raise)
	var got [][]*eventsv1.Callout
	for seq, lane := range []bool{false, true, true, false} {
		f := frame(uint32(seq+1), lane)
		if seq == 3 {
			f.PitChange = pit.Exited
		}
		got = append(got, callouts(e.Step(f)))
	}
	rulesOf := func(cs []*eventsv1.Callout) string {
		var names []string
		for _, c := range cs {
			names = append(names, c.GetRule()+"@"+strconv.FormatUint(uint64(c.GetAt()), 10))
		}
		return strings.Join(names, " ")
	}
	want := []string{
		"lap_time@1 fuel@1 position@1",
		"fuel@2",
		"fuel@3",
		"lap_time@2 lap_time@3",
	}
	for i := range want {
		if g := rulesOf(got[i]); g != want[i] {
			t.Errorf("frame %d: %q, want %q", i+1, g, want[i])
		}
	}
	held := got[3][0]
	if !held.GetCreatedAt().AsTime().Equal(epoch.Add(4*time.Second)) ||
		!held.GetExpiresAt().AsTime().Equal(epoch.Add(12*time.Second)) || held.GetHoldUntil() != nil {
		t.Errorf("released: created %v, expires %v, hold %v; want as raised on frame 4", held.GetCreatedAt().AsTime(),
			held.GetExpiresAt().AsTime(), held.GetHoldUntil())
	}
	if held.GetId() != "run:callout:2:lap_time:1" {
		t.Errorf("released with id %s, want the id of the frame raised on", held.GetId())
	}
	if d := e.Stats().Dropped["position"]; d != 2 {
		t.Errorf("%d dropped, want 2", d)
	}
}

// A race that finishes in the lane releases what was held, at the finish; a
// start forgets it.
func TestTheFinishReleasesAndAStartForgets(t *testing.T) {
	raise := &stub{raise: func(ctx *race.Context) []Callout {
		if ctx.Cur.Seq == 1 {
			return []Callout{{Kind: "k"}}
		}
		return nil
	}}
	e := testEngine(t, nil, "lap_time", raise)
	_ = e.Step(frame(1, true))
	flag := frame(2, true)
	flag.Finished = true
	if got := callouts(e.Step(flag)); len(got) != 1 {
		t.Errorf("%d released at the finish, want 1", len(got))
	}
	_ = e.Step(frame(1, true))
	start := frame(3, false)
	start.Started = true
	if got := callouts(e.Step(start)); len(got) != 0 || raise.resets != 1 {
		t.Errorf("%d released at a start, %d resets; want none and 1", len(got), raise.resets)
	}
}

// R6: what the engine stamps: the rule's priority unless the callout has its
// own, expires_at from the rule's max age or the queue's, hold_until from
// hold_secs, created_at from AsOf, and the pit lane.
func TestTheEngineStampsEachCallout(t *testing.T) {
	asOf := race.View{Seq: 1, Time: epoch.Add(-7 * time.Second)}
	raise := func(cs ...Callout) Rule { return &stub{raise: func(*race.Context) []Callout { return cs }} }
	e := testEngine(t, nil,
		"lap_time", raise(Callout{Kind: "template_new_best", Priority: 11}),
		"position", raise(Callout{Kind: "template_gained", Slots: &eventsv1.CalloutSlots{}}),
		"race_summary", raise(Callout{Kind: "template", AsOf: &asOf}))
	cs := callouts(e.Step(frame(9, false)))
	if len(cs) != 3 {
		t.Fatalf("%d callouts", len(cs))
	}
	now := epoch.Add(9 * time.Second)
	check := func(c *eventsv1.Callout, priority int32, created, expires time.Time, hold time.Duration, lane eventsv1.PitLane) {
		t.Helper()
		if c.GetPriority() != priority || !c.GetCreatedAt().AsTime().Equal(created) ||
			!c.GetExpiresAt().AsTime().Equal(expires) || c.GetPitLane() != lane ||
			c.GetSessionId() != "sid" || c.GetRunId() != "run" || c.GetAt() != 9 {
			t.Errorf("%s: %v", c.GetRule(), c)
		}
		if (hold == 0) != (c.GetHoldUntil() == nil) || (hold != 0 && !c.GetHoldUntil().AsTime().Equal(created.Add(hold))) {
			t.Errorf("%s: hold_until %v, want %v after created", c.GetRule(), c.GetHoldUntil(), hold)
		}
	}
	check(cs[0], 11, now, now.Add(8*time.Second), 0, eventsv1.PitLane_PIT_LANE_HOLD)
	check(cs[1], 12, now, now.Add(8*time.Second), time.Second, eventsv1.PitLane_PIT_LANE_DROP)
	check(cs[2], 25, asOf.Time, asOf.Time.Add(30*time.Second), 0, eventsv1.PitLane_PIT_LANE_SPEAK)
}

// paddock#97, R5: a callout that carries a car alarm has its car.alarm event on
// the frame raised on, just before it, stamped with the session, frame and run
// and with the catalogue's id, even when the pit lane holds the callout; with no
// session open there is no event.
func TestACarAlarmIsPublishedBesideItsCallout(t *testing.T) {
	raise := &stub{raise: func(*race.Context) []Callout {
		return []Callout{{Kind: "template_tyre_hot", Alarm: &eventsv1.CarAlarmRaised{
			Check: eventsv1.CarCheck_CAR_CHECK_TYRE_HOT, ValueC: 125, ThresholdC: 120,
		}}}
	}}
	e := testEngine(t, nil, "temps", raise)
	out := e.Step(frame(7, false))
	if len(out) != 2 || out[0].Alarm == nil || out[1].Callout == nil {
		t.Fatalf("outputs %v, want the alarm then the callout", out)
	}
	a := out[0]
	if a.Event.ID != "sid:car.alarm:tyre_hot:run:7" || a.Session != "sid" || !a.Event.Time.Equal(epoch.Add(7*time.Second)) ||
		a.Alarm.GetSessionId() != "sid" || a.Alarm.GetAt() != 7 || a.Alarm.GetRunId() != "run" || a.Alarm.GetValueC() != 125 {
		t.Errorf("alarm %v, %v", a.Event, a.Alarm)
	}
	if a.Message() != a.Alarm {
		t.Error("the output's message is not the alarm")
	}
	out = e.Step(frame(8, true))
	if len(out) != 1 || out[0].Alarm == nil {
		t.Errorf("in the lane: %v, want the alarm alone, the callout held", out)
	}
	none := frame(9, false)
	none.Session = ""
	if out := e.Step(none); len(out) != 1 || out[0].Callout == nil {
		t.Errorf("no session: %v, want the callout alone", out)
	}
}

// R8: a rule with a gauge shows it through the engine.
func TestAGaugeIsShown(t *testing.T) {
	e := testEngine(t, nil, "fuel", &gauged{})
	if g := e.Gauges(); g["fuel_laps_remaining"] != 2.5 {
		t.Errorf("gauges %v", g)
	}
}

type gauged struct{ stub }

func (*gauged) Gauge() (string, float64, bool) { return "fuel_laps_remaining", 2.5, true }

// salsaDir is salsa's testdata/v1, where v1's recordings are.
func salsaDir(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/those-who-run/salsa").Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "testdata", "v1")
}

// A session that ends in the lane without a finish never publishes what was
// held, neither on its last frame nor once no session is open (the pits then
// read as out of the lane): v1 released only on a pit change with a session
// open, and its next start cleared them.
func TestASessionEndedInTheLaneNeverReleases(t *testing.T) {
	raise := &stub{raise: func(ctx *race.Context) []Callout {
		if ctx.Cur.Seq == 1 {
			return []Callout{{Kind: "template_in_lap"}}
		}
		return nil
	}}
	e := testEngine(t, nil, "lap_time", raise)
	_ = e.Step(frame(1, true))
	quit := frame(2, true)
	quit.EndedEarly = &race.View{Seq: 1}
	menu := frame(3, false)
	menu.Session, menu.Racing = "", false
	for _, f := range []*race.Context{quit, menu, frame(4, false)} {
		if got := callouts(e.Step(f)); len(got) != 0 {
			t.Errorf("frame %d: %v released after the session ended", f.Cur.Seq, got)
		}
	}
	if d := e.Stats().Dropped["lap_time"]; d != 1 {
		t.Errorf("%d dropped, want the held one", d)
	}
	// Out of the lane with no exit, the session still open: still held, as v1.
	_ = e.Step(frame(1, true))
	if got := callouts(e.Step(frame(5, false))); len(got) != 0 {
		t.Errorf("released with no pit change: %v", got)
	}
}

// On the flag frame in the lane, and after it, the pit lane still applies: a
// drop callout is dropped, and a hold one is released on the same frame, after
// the rules, as raised then with no hold_until (v1's _quiet, then its release
// once finished).
func TestThePitLaneAppliesAtTheFlag(t *testing.T) {
	raise := &stub{raise: func(ctx *race.Context) []Callout {
		if ctx.Cur.Seq == 2 {
			return []Callout{{Kind: "k"}}
		}
		return nil
	}}
	e := testEngine(t, nil, "position", raise, "lap_time", raise)
	_ = e.Step(frame(1, true))
	flag := frame(2, true)
	flag.Finished = true
	got := callouts(e.Step(flag))
	if len(got) != 1 || got[0].GetRule() != "lap_time" {
		t.Fatalf("at the flag in the lane: %v, want lap_time's alone", got)
	}
	if got[0].GetHoldUntil() != nil || !got[0].GetCreatedAt().AsTime().Equal(flag.Time) {
		t.Errorf("released at the flag: %v", got[0])
	}
	if d := e.Stats().Dropped["position"]; d != 1 {
		t.Errorf("position: %d dropped, want 1", d)
	}
}
