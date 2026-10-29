package position

import (
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/rules"
)

// drive feeds the rule one frame every half second from t, with a field, and
// keeps each call with the time it was raised at.
type drive struct {
	r     *rule
	t     float64
	pit   bool
	field bool
	calls []timedCall
}

type timedCall struct {
	t float64
	c call
}

func newDrive() *drive { return &drive{r: &rule{cfg: DefaultConfig}, field: true} }

// hold drives secs at pos.
func (d *drive) hold(pos int32, secs float64) {
	for end := d.t + secs; d.t < end; d.t += 0.5 {
		f := frame{t: d.t, inRace: true, position: pos, hasField: d.field, inPitLane: d.pit}
		if c, ok := d.r.observe(f); ok {
			d.calls = append(d.calls, timedCall{d.t, c})
		}
	}
}

func (d *drive) want(t *testing.T, want ...timedCall) {
	t.Helper()
	if len(d.calls) != len(want) {
		t.Fatalf("calls %+v, want %+v", d.calls, want)
	}
	for i := range want {
		if d.calls[i] != want[i] {
			t.Errorf("call %d: %+v, want %+v", i, d.calls[i], want[i])
		}
	}
}

// R2: the grid slot follows the first 3 s quietly (P20 then P5), nothing is called
// in the first 15 s, and the first call after the grace compares with the grid slot.
func TestGridSlotAndStartGrace(t *testing.T) {
	d := newDrive()
	d.hold(20, 0.5)
	d.hold(5, 4.5)  // the grid slot, settled by 3 s
	d.hold(3, 13.5) // from t 5: settles in the grace, called 3 s after it
	d.want(t, timedCall{18, call{KindGainedPodium, 3, 5}})
}

// R3: a change is called once it has held 3 s, the latest place once things calm
// down, and no sooner than 10 s after the last call.
func TestSettleAndInterval(t *testing.T) {
	d := newDrive()
	d.hold(8, 20)
	d.hold(7, 2.5) // under 3 s: never settles
	d.hold(8, 1)
	d.hold(7, 1)
	d.hold(6, 4) // settling since 23.5, called at 26.5
	d.hold(9, 9) // settled at 31.5, waits for 36.5
	d.want(t,
		timedCall{26.5, call{KindGained, 6, 8}},
		timedCall{36.5, call{KindLost, 9, 6}},
	)
}

// R4: quiet in the pit lane, settling again from the exit; the last place called is
// kept, so one call gives the net change.
func TestPitLane(t *testing.T) {
	d := newDrive()
	d.hold(5, 20)
	d.pit = true
	for pos := int32(6); pos <= 11; pos++ {
		d.hold(pos, 5)
	}
	d.pit = false
	d.hold(10, 4)
	d.want(t, timedCall{53, call{KindLost, 10, 5}})
}

// R1: nothing without a field (a time trial's ghost), nor without a position or
// off a driving frame.
func TestNeedsAField(t *testing.T) {
	d := newDrive()
	d.field = false
	d.hold(2, 20)
	d.hold(1, 20)
	d.want(t)
	r := &rule{cfg: DefaultConfig}
	for i := range 80 {
		ts := float64(i) / 2
		for _, f := range []frame{
			{t: ts, inRace: true, position: 0, hasField: true},
			{t: ts, inRace: false, position: int32(1 + i%5), hasField: true},
		} {
			if c, ok := r.observe(f); ok {
				t.Errorf("called %+v on %+v", c, f)
			}
		}
	}
}

// R6: the situational pools.
func TestKinds(t *testing.T) {
	for _, c := range []struct {
		was, pos int32
		want     string
	}{
		{2, 1, KindGainedLead},
		{5, 3, KindGainedPodium},
		{3, 2, KindGainedPodium},
		{9, 4, KindGained},
		{1, 2, KindLostLead},
		{1, 7, KindLostLead},
		{2, 3, KindLost},
	} {
		if got := kind(c.was, c.pos); got != c.want {
			t.Errorf("P%d to P%d: %s, want %s", c.was, c.pos, got, c.want)
		}
	}
}

// Reset forgets the race but keeps the settings.
func TestReset(t *testing.T) {
	d := newDrive()
	d.hold(8, 20)
	d.hold(7, 4)
	d.r.Reset()
	d.hold(3, 20) // a new grid slot: quiet again
	d.want(t, timedCall{23, call{KindGained, 7, 8}})
}

// R5, R7: the engine's settings for the rule are v1's (priority 12, hold_secs 1,
// pit_lane drop), and the rule's own thresholds are read from the rules file
// with v1's keys over v1's defaults.
func TestSettings(t *testing.T) {
	s := rules.DefaultConfig().Rules[Name]
	if s.Priority != 12 || s.HoldSecs != 1 || s.PitLane != eventsv1.PitLane_PIT_LANE_DROP || s.MaxAgeSecs != 0 {
		t.Errorf("settings %+v, want v1's priority 12, hold_secs 1, pit_lane drop and the queue's max age", s)
	}
	cfg, err := rules.ParseConfig([]byte("rules:\n  position:\n    settle_secs: 2\n    start_grace_secs: 30\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := DefaultConfig
	cfg.Rules[Name].Decode(&got)
	if want := (Config{SettleSecs: 2, MinIntervalSecs: 10, StartGraceSecs: 30}); got != want {
		t.Errorf("config %+v, want %+v", got, want)
	}
}
