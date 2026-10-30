package temps

import (
	"slices"
	"strings"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
)

var (
	hotFL = [4]float64{125, 90, 90, 90}
	warm  = [4]float64{90, 90, 90, 90}
)

// sim is a stint of frames for the rule, one a second as v1's test sim drives
// them, with the water at 85 and the oil at 110 unless a frame says otherwise.
type sim struct {
	t      float64
	frames []race.View
}

func (s *sim) drive(n int, tyres [4]float64) *sim { return s.driveAt(n, tyres, 85, 110) }

func (s *sim) driveAt(n int, tyres [4]float64, water, oil float64) *sim {
	for range n {
		s.add(telemetryv1.Phase_PHASE_IN_RACE, tyres, water, oil)
	}
	return s
}

// paused is n paused frames, which the rule is not handed.
func (s *sim) paused(n int) *sim {
	for range n {
		s.add(telemetryv1.Phase_PHASE_PAUSED, warm, 85, 110)
	}
	return s
}

func (s *sim) add(phase telemetryv1.Phase, tyres [4]float64, water, oil float64) {
	s.frames = append(s.frames, race.View{
		Seq: uint32(len(s.frames) + 1), T: s.t, Phase: phase, //nolint:gosec // small
		Frame: &telemetryv1.Frame{
			TireFlTemp: tyres[0], TireFrTemp: tyres[1], TireRlTemp: tyres[2], TireRrTemp: tyres[3],
			WaterTemp: water, OilTemp: oil,
		},
	})
	s.t++
}

// last is the last n frames.
func (s *sim) last(n int) []race.View { return s.frames[len(s.frames)-n:] }

// stint is v1's _stint: a warm lap's worth of driving first, so the cold check
// is behind it.
func stint() *sim { return (&sim{}).drive(10, warm) }

// feed is v1's run_rule: each frame the engine hands the rule (in a race), with
// the frame before as Prev.
func feed(r *Rule, frames []race.View, joined bool) []rules.Callout {
	var out []rules.Callout
	var prev *race.View
	for i := range frames {
		cur := frames[i]
		racing := cur.InRace()
		if racing {
			out = append(out, r.Evaluate(&race.Context{Session: "s", Cur: cur, Prev: prev, Racing: true, Joined: joined})...)
			prev = &frames[i]
		}
	}
	return out
}

// said is each callout as its kind, with its corners for a tyre alarm.
func said(calls []rules.Callout) []string {
	var out []string
	for _, c := range calls {
		k := c.Kind
		for _, corner := range c.Slots.GetCorners() {
			k += " " + strings.ToLower(strings.TrimPrefix(corner.String(), "CORNER_"))
		}
		out = append(out, k)
	}
	return out
}

func check(t *testing.T, got []rules.Callout, want ...string) {
	t.Helper()
	if s := said(got); !slices.Equal(s, want) {
		t.Errorf("got %q, want %q", s, want)
	}
}

// R1, AC4: a tyre at 125 °C fires once it has been over the threshold 5 s, not
// at 4.9 s; and again 30 s after the firing, re-armed, it does not (the cooldown).
func TestTempsFiresAfterTheSustainWindowNotBefore(t *testing.T) {
	r := New(DefaultConfig())
	at := func(t0 float64, temp float64) []rules.Callout {
		f := race.View{T: t0, Phase: telemetryv1.Phase_PHASE_IN_RACE, Frame: &telemetryv1.Frame{
			TireFlTemp: temp, TireFrTemp: 90, TireRlTemp: 90, TireRrTemp: 90, WaterTemp: 85, OilTemp: 110,
		}}
		return r.Evaluate(&race.Context{Session: "s", Cur: f, Racing: true})
	}
	at(97, 90)
	for _, t0 := range []float64{100, 101, 102, 103, 104, 104.9} {
		if got := at(t0, 125); len(got) != 0 {
			t.Fatalf("fired at %.1f s, %.1f s over: %v", t0, t0-100, said(got))
		}
	}
	check(t, at(105, 125), "template_tyre_hot front_left")
	at(110, 90) // cooled 35 °C: re-armed
	for t0 := 120.0; t0 <= 135; t0++ {
		if got := at(t0, 125); len(got) != 0 {
			t.Fatalf("fired again at %.0f s, %.0f s after the first", t0, t0-105)
		}
	}
	for t0 := 160.0; t0 < 165; t0++ {
		at(t0, 125)
	}
	check(t, at(165, 125), "template_tyre_hot front_left") // the cooldown is past
}

// v1's test_temps_hot_corner_after_sustain and test_temps_spike_is_ignored.
func TestTempsAHotCornerAfterTheSustainAndNotASpike(t *testing.T) {
	s := stint().drive(5, hotFL)
	check(t, feed(New(DefaultConfig()), s.frames, false))
	s.drive(1, hotFL)
	check(t, feed(New(DefaultConfig()), s.frames, false), "template_tyre_hot front_left")

	s = stint().drive(3, hotFL).drive(3, warm).drive(3, hotFL)
	check(t, feed(New(DefaultConfig()), s.frames, false))
}

// R2: v1's test_temps_corners_are_joined_into_one_phrase and
// test_temps_a_corner_already_hot_joins_the_one_that_fires: the tyres over the
// threshold fire together, in GT7's order, and a corner hot for less than the
// sustain joins the one that fires.
func TestTempsCornersFireTogether(t *testing.T) {
	for _, c := range []struct {
		frames []race.View
		want   string
	}{
		{stint().drive(6, [4]float64{125, 90, 125, 90}).frames, "template_tyre_hot front_left rear_left"},
		{stint().drive(6, [4]float64{125, 125, 125, 125}).frames, "template_tyre_hot front_left front_right rear_left rear_right"},
		{stint().drive(6, [4]float64{125, 125, 125, 90}).frames, "template_tyre_hot front_left front_right rear_left"},
		{stint().drive(1, [4]float64{90, 90, 125, 90}).drive(6, [4]float64{90, 90, 125, 125}).frames, "template_tyre_hot rear_left rear_right"},
		{stint().drive(6, [4]float64{125, 125, 90, 90}).frames, "template_tyre_hot front_left front_right"},
	} {
		check(t, feed(New(DefaultConfig()), c.frames, false), c.want)
	}
}

// R1: v1's test_temps_once_while_hot_then_again_after_cooling_and_cooldown and
// test_temps_cooldown_blocks_a_quick_second_warning.
func TestTempsOnceWhileHotThenAgainAfterCoolingAndTheCooldown(t *testing.T) {
	r := New(DefaultConfig())
	s := stint().drive(130, hotFL)
	check(t, feed(r, s.frames, false), "template_tyre_hot front_left")
	// back under the threshold but not under it less the hysteresis: still off
	check(t, feed(r, s.drive(3, [4]float64{115, 90, 90, 90}).last(3), false))
	check(t, feed(r, s.drive(6, hotFL).last(6), false))
	// cooled properly: re-armed, and the cooldown long past
	s.drive(2, warm)
	check(t, feed(r, s.drive(6, hotFL).last(8), false), "template_tyre_hot front_left")

	r = New(DefaultConfig())
	s = stint().drive(6, hotFL)
	check(t, feed(r, s.frames, false), "template_tyre_hot front_left")
	s.drive(2, warm)
	check(t, feed(r, s.drive(6, hotFL).last(8), false)) // re-armed, in the cooldown
}

// R1, R4: v1's test_temps_pause_restarts_the_sustain_window. Paused frames are not
// handed to the rule, so the gap over them starts the wait again.
func TestTempsAPauseRestartsTheSustainWindow(t *testing.T) {
	r := New(DefaultConfig())
	s := stint().drive(3, hotFL).paused(6)
	check(t, feed(r, s.drive(5, hotFL).frames, false))
	check(t, feed(r, s.drive(1, hotFL).last(1), false), "template_tyre_hot front_left")
}

// R4: a frame out of a race is not evaluated, even one the engine hands every rule.
func TestTempsOnlyOnDrivingFrames(t *testing.T) {
	r := New(DefaultConfig())
	s := stint().drive(10, hotFL)
	for _, f := range s.frames {
		f.Phase = telemetryv1.Phase_PHASE_FINISHED
		if got := r.Evaluate(&race.Context{Session: "s", Cur: f, Racing: true}); len(got) != 0 {
			t.Fatalf("raised %v on a frame not in a race", said(got))
		}
		f.Phase = telemetryv1.Phase_PHASE_IN_RACE
		if got := r.Evaluate(&race.Context{Session: "s", Cur: f}); len(got) != 0 {
			t.Fatalf("raised %v where v1 did not evaluate", said(got))
		}
	}
}

// R3, AC2: v1's test_temps_cold_tyres_once_at_the_start: cold tyres are called
// once, 2 s in, and never in a session joined part way.
func TestTempsColdTyresOnceAtTheStart(t *testing.T) {
	cold := (&sim{}).drive(30, [4]float64{21, 45, 60, 21})
	got := feed(New(DefaultConfig()), cold.frames, false)
	check(t, got, "template_tyres_cold")
	if len(got) == 1 && cold.frames[2].T != 2 {
		t.Fatal("the sim is not one frame a second")
	}
	a := got[0].Alarm
	if a.GetCheck() != eventsv1.CarCheck_CAR_CHECK_TYRES_COLD || a.GetValueC() != 21 || a.GetThresholdC() != 50 ||
		len(a.GetTyres()) != 3 || a.GetTyres()[1].GetCorner() != eventsv1.Corner_CORNER_FRONT_RIGHT {
		t.Errorf("alarm %v: want the three tyres under 50 °C, the coldest 21", a)
	}
	check(t, feed(New(DefaultConfig()), (&sim{}).drive(30, [4]float64{60, 60, 60, 60}).frames, false)) // a fresh load reads a flat 60 to 70
	check(t, feed(New(DefaultConfig()), cold.frames, true))                                            // no start was seen

	// Two seconds in, not before.
	r := New(DefaultConfig())
	check(t, feed(r, cold.frames[:2], false))
	check(t, feed(r, cold.frames[2:3], false), "template_tyres_cold")
	r.Reset() // a new start checks again
	check(t, feed(r, cold.frames[10:13], false), "template_tyres_cold")
}

// v1's test_temps_water_and_oil, with each alarm's value.
func TestTempsWaterAndOil(t *testing.T) {
	got := feed(New(DefaultConfig()), stint().driveAt(6, warm, 115, 135).frames, false)
	check(t, got, "template_water", "template_oil")
	if len(got) == 2 && (got[0].Alarm.GetValueC() != 115 || got[0].Alarm.GetThresholdC() != 110 ||
		got[1].Alarm.GetCheck() != eventsv1.CarCheck_CAR_CHECK_OIL_HOT || got[1].Alarm.GetValueC() != 135 || len(got[1].Alarm.GetTyres()) != 0) {
		t.Errorf("alarms %v, %v", got[0].Alarm, got[1].Alarm)
	}
}

// R1: v1's test_temps_thresholds_can_be_disabled: null or 0 turns a check off, read
// from the rules file as v1 read it.
func TestTempsThresholdsCanBeDisabled(t *testing.T) {
	cfg, err := rules.ParseConfig([]byte("rules:\n  temps:\n    tyre_hot_c: null\n    water_hot_c: 0\n    tyre_cold_c:\n    sustain_secs: 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := rules.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.Rules(), Name) {
		t.Fatalf("rules %v", e.Rules())
	}
	r := fromSettings(cfg.Rules[Name])
	if r.cfg.TyreHotC != 0 || r.cfg.WaterHotC != 0 || r.cfg.TyreColdC != 0 || r.cfg.OilHotC != 130 || r.cfg.SustainSecs != 4 {
		t.Errorf("config %+v", r.cfg)
	}
	s := (&sim{}).drive(10, [4]float64{21, 21, 21, 21}).driveAt(6, hotFL, 150, 110)
	check(t, feed(r, s.frames, false))
}

// R5, AC3: each firing carries its car alarm, with the corners that fired and
// their temperatures, the hottest the value.
func TestTempsATyreAlarmNamesTheCorners(t *testing.T) {
	got := feed(New(DefaultConfig()), stint().drive(6, [4]float64{90, 90, 176.5, 169.5}).frames, false)
	check(t, got, "template_tyre_hot rear_left rear_right")
	a := got[0].Alarm
	if a.GetCheck() != eventsv1.CarCheck_CAR_CHECK_TYRE_HOT || a.GetValueC() != 176.5 || a.GetThresholdC() != 120 ||
		len(a.GetTyres()) != 2 || a.GetTyres()[0].GetCorner() != eventsv1.Corner_CORNER_REAR_LEFT ||
		a.GetTyres()[1].GetTempC() != 169.5 {
		t.Errorf("alarm %v", a)
	}
}
