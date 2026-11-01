package fuel

import (
	"math"
	"slices"
	"testing"

	"github.com/those-who-run/ecu/internal/rules/rulestest"
	"github.com/those-who-run/ecu/race"
)

// drive replays a recording through the machine into a fuel rule of its own,
// with v1's defaults, and returns the rule as the recording leaves it, with the
// laps its model measured, in order.
func drive(t *testing.T, name string) (r *Rule, measured []int32) {
	t.Helper()
	m := race.New(rulestest.IDs())
	r = New(DefaultConfig())
	m.Rules(func(ctx *race.Context) []race.Output {
		if ctx.Started {
			r.Reset()
		}
		before := r.Model().Consumption()
		r.Evaluate(ctx)
		if !slices.Equal(before, r.Model().Consumption()) {
			measured = append(measured, ctx.Prev.Lap)
		}
		return nil
	})
	for _, p := range rulestest.Packets(t, name) {
		m.Step(p)
	}
	return r, measured
}

// AC2: at Suzuka the fuel used over the race is the sum of both stints, each
// under the whole tank (those-who-run/those-who-run#186 AC2). Its pit calls'
// frames are the golden's (TestTheFuelCalloutsAreV1sOnEveryRecording).
func TestSuzukaFuelUsedIsBothStints(t *testing.T) {
	r, _ := drive(t, "real-nuc-suzuka-pit-stop-2026-10-15")
	used := r.Model().Used()
	if len(used) != 2 {
		t.Fatalf("stints %v, want two", used)
	}
	if total := r.Model().Total(); math.Abs(total-(used[0]+used[1])) > 1e-9 {
		t.Errorf("total %v, want the sum of %v", total, used)
	}
	for i, u := range used {
		if u <= 0 || u >= 100 {
			t.Errorf("stint %d used %v, want under the whole tank", i+1, u)
		}
	}
}

// AC3: at Circuit Gilles Villeneuve, joined on lap 5, the joined lap (5), the
// in-lap (6) and the out-lap (7) are not measured: lap 8 is the only lap in the
// average, and the gauge reads it.
func TestGillesVilleneuveMeasuresOnlyLap8(t *testing.T) {
	r, measured := drive(t, "real-nuc-gilles-villeneuve-pit-stop-2026-10-17")
	if !slices.Equal(measured, []int32{8}) {
		t.Errorf("measured laps %v, want [8]", measured)
	}
	if name, v, ok := r.Gauge(); name != GaugeName || !ok || v <= 0 {
		t.Errorf("gauge %s %v %v, want laps remaining", name, v, ok)
	}
}

// AC6: at Willow Springs, after the stop, laps remaining uses only laps after it
// (those-who-run/those-who-run#186 AC3): the joined lap and the pit laps are
// out, so lap 8, the first lap after the stop, is the only one measured.
func TestWillowSpringsUsesOnlyLapsAfterTheStop(t *testing.T) {
	r, measured := drive(t, "real-nuc-willow-springs-pit-stop-2026-10-17")
	if !slices.Equal(measured, []int32{8}) {
		t.Errorf("measured laps %v, want [8]", measured)
	}
	if got := r.Model().Consumption(); len(got) != 1 {
		t.Errorf("the average is over %v, want lap 8 alone", got)
	}
}
