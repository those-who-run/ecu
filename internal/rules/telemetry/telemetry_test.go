package telemetry_test

import (
	"testing"
	"time"

	"github.com/those-who-run/contracts/callout"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/ecu/internal/rules/rulestest"
	_ "github.com/those-who-run/ecu/internal/rules/telemetry"
)

// timeout is the watchdog's, ecu's default (TWR_ECU_TELEMETRY_TIMEOUT_SECS).
const timeout = 3 * time.Second

// said is a telemetry callout, with whether it came on the watchdog's tick.
type said struct {
	c    *eventsv1.Callout
	tick bool
}

// stopped runs a recording live, as ecu's drive does, with the watchdog on and
// every rule of v1's defaults, until stop is true of a frame; then the feed stops
// for gap, the watchdog ticks at its deadline, and the rest of the recording
// resumes gap later. The machine runs the rules (Machine.Rules). It returns the
// telemetry rule's callouts, the frame the feed stopped on, and what the machine
// published on the tick, the rules' callouts included.
func stopped(t *testing.T, name string, stop func(*race.Context) bool, gap time.Duration) ([]said, race.View, []race.Output) {
	t.Helper()
	e, err := rules.New(rules.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := race.New(rulestest.IDs())
	m.Rules(e.Step)
	m.WatchTelemetry(timeout)
	var out []said
	keep := func(os []race.Output, tick bool) {
		for _, c := range ours(os) {
			out = append(out, said{c, tick})
		}
	}
	var last race.View
	var lost []race.Output
	var shift time.Duration
	for _, r := range rulestest.Packets(t, name) {
		r.Time = r.Time.Add(shift)
		keep(m.Step(r), false)
		if shift == 0 && stop(m.Context()) {
			last = m.Context().Cur
			due, ok := m.LostAt()
			if !ok {
				t.Fatal("not watching the feed")
			}
			if lost = m.Tick(due); len(lost) == 0 {
				t.Fatalf("no loss %v after frame %d", timeout, last.Seq)
			}
			keep(lost, true)
			shift = gap
		}
	}
	if shift == 0 {
		t.Fatalf("%s: the feed never stopped", name)
	}
	return out, last, lost
}

func phase(p telemetryv1.Phase) func(*race.Context) bool {
	return func(ctx *race.Context) bool { return ctx.Cur.Phase == p && ctx.Cur.Seq > 100 }
}

// AC1 and AC4: a feed that stops for 4 s on a driving frame gives "Lost
// telemetry." once, on the tick 3 s after the last packet, on .p0 (priority 5),
// and "Telemetry back." on resuming, both with no slots.
func TestALossWhileDrivingIsSaidOnceAndItsEndToo(t *testing.T) {
	got, last, _ := stopped(t, "real-race-2laps-2026-10-14", phase(telemetryv1.Phase_PHASE_IN_RACE), 4*time.Second)
	if len(got) != 2 {
		t.Fatalf("%d telemetry callouts %v, want lost then back", len(got), got)
	}
	lost, back := got[0], got[1]
	if !lost.tick || lost.c.GetKind() != "template_lost" || lost.c.GetAt() != last.Seq ||
		!lost.c.GetCreatedAt().AsTime().Equal(last.Time.Add(timeout)) || lost.c.GetSessionId() == "" {
		t.Errorf("lost: %v", lost.c)
	}
	if back.tick || back.c.GetKind() != "template_back" || back.c.GetAt() != last.Seq+1 ||
		!back.c.GetCreatedAt().AsTime().After(last.Time.Add(4*time.Second)) {
		t.Errorf("back: %v", back.c)
	}
	for _, s := range got {
		c := s.c
		if c.GetPriority() != 5 || callout.LevelOf(c.GetPriority()) != callout.P0 || c.GetSlots() != nil ||
			c.GetPitLane() != eventsv1.PitLane_PIT_LANE_SPEAK || c.GetHoldUntil() != nil {
			t.Errorf("fields: %v", c)
		}
	}
	if lost.c.GetId() == back.c.GetId() {
		t.Errorf("one id for both: %s", lost.c.GetId())
	}
}

// AC2: a feed that stops in a menu gives nothing, and nothing on resuming.
func TestALossInAMenuIsNotSaid(t *testing.T) {
	got, _, _ := stopped(t, "real-race-2laps-2026-10-14", func(ctx *race.Context) bool {
		return ctx.Cur.Phase == telemetryv1.Phase_PHASE_MENU
	}, 4*time.Second)
	if len(got) != 0 {
		t.Errorf("in a menu: %v", got)
	}
}

// v1's phases: a loss in a pause is said too, and its end. The loss ends the
// session paused (paddock#87), and the frames that follow are ignored until the
// next start, but the PS5 waking still gives "Telemetry back." on the first
// packet, as v1's watchdog said it.
func TestALossInAPauseIsSaidAndItsEndAfterTheSessionEndedPaused(t *testing.T) {
	got, last, lost := stopped(t, "real-nuc-sugo-restarts-2026-10-18", phase(telemetryv1.Phase_PHASE_PAUSED), 4*time.Second)
	var ended *eventsv1.SessionEnded
	for _, o := range lost {
		if o.Ended != nil {
			ended = o.Ended
		}
	}
	if ended == nil || ended.GetReason() != eventsv1.EndReason_END_REASON_TELEMETRY_LOST || !ended.GetSummary().GetEndedPaused() {
		t.Fatalf("on the loss %v, want the session ended paused", lost)
	}
	if len(got) == 2 && got[1].c.GetAt() != last.Seq+1 {
		t.Errorf("back on frame %d, want the first packet back, %d", got[1].c.GetAt(), last.Seq+1)
	}
	if len(got) != 2 || got[0].c.GetKind() != "template_lost" || got[1].c.GetKind() != "template_back" {
		t.Errorf("in a pause: %v, want lost then back", got)
	}
}

// In the pit lane the loss is spoken, not held for an exit that needs frames
// (pit_lane speak): it is published on the tick.
func TestALossInThePitLaneIsSpoken(t *testing.T) {
	got, _, _ := stopped(t, "real-nuc-suzuka-pit-stop-2026-10-15", func(ctx *race.Context) bool {
		return ctx.Pits.InPitLane && ctx.Cur.InRace()
	}, 4*time.Second)
	if len(got) != 2 || !got[0].tick || got[0].c.GetKind() != "template_lost" || got[1].c.GetKind() != "template_back" {
		t.Errorf("in the lane: %v, want lost on the tick then back", got)
	}
}

// R3: the phases are the rules file's, and enabled: false silences the rule.
func TestThePhasesAndEnabledAreTheRulesFiles(t *testing.T) {
	for _, tc := range []struct {
		file string
		want int
	}{
		{"rules: {telemetry: {phases: [paused]}}", 0},
		{"rules: {telemetry: {phases: [in_race]}}", 2},
		{"rules: {telemetry: {enabled: false}}", 0},
	} {
		cfg, err := rules.ParseConfig([]byte(tc.file))
		if err != nil {
			t.Fatal(err)
		}
		e, err := rules.New(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		lost := &race.Context{Tick: true, Lost: &eventsv1.TelemetryLost{Phase: telemetryv1.Phase_PHASE_IN_RACE}}
		n := len(ours(e.Step(lost))) + len(ours(e.Step(&race.Context{Restored: &eventsv1.TelemetryRestored{}})))
		if n != tc.want {
			t.Errorf("%s: %d callouts, want %d", tc.file, n, tc.want)
		}
	}
}

// R2: a restore is said only after a loss that was, once; and a start between
// the two (the first frame after the gap can be one) doesn't forget it.
func TestTheEndIsSaidOnlyAfterALossThatWas(t *testing.T) {
	e, err := rules.New(rules.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	restored := &race.Context{Restored: &eventsv1.TelemetryRestored{}}
	if got := ours(e.Step(restored)); len(got) != 0 {
		t.Errorf("back with no loss: %v", got)
	}
	e.Step(&race.Context{Tick: true, Lost: &eventsv1.TelemetryLost{Phase: telemetryv1.Phase_PHASE_PAUSED}})
	start := &race.Context{Started: true, Restored: &eventsv1.TelemetryRestored{}}
	if got := ours(e.Step(start)); len(got) != 1 || got[0].GetKind() != "template_back" {
		t.Errorf("back on a start: %v", got)
	}
	if got := ours(e.Step(restored)); len(got) != 0 {
		t.Errorf("back twice: %v", got)
	}
}

// ours are the telemetry rule's callouts among out.
func ours(out []race.Output) []*eventsv1.Callout {
	var cs []*eventsv1.Callout
	for _, o := range out {
		if o.Callout != nil && o.Callout.GetRule() == "telemetry" {
			cs = append(cs, o.Callout)
		}
	}
	return cs
}
