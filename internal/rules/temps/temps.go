// Package temps is v1's temps rule (rules/temps.py at v1.0.0, paddock#97):
// temperature warnings, hot tyres per corner, cold tyres at the start, water and
// oil. Its comments are v1's, where each records what a recording showed.
//
// Every check is an alarm. The value has to stay above its threshold for
// sustain_secs (a spin heats one tyre for a moment), the alarm then fires once
// and only re-arms when the value has dropped hysteresis_c below the threshold,
// and never within cooldown_secs of the last warning. Paused frames are not
// evaluated, so a gap longer than the sustain window restarts it.
//
// Corners are reported together: when one tyre's alarm fires, every other tyre
// already over the threshold is included rather than reported a moment later on
// its own (the rears 0.3 s apart while drifting, 2026-10-15).
//
// Thresholds are starting points. Recordings so far show tyres from 21 °C on a
// cold start to 154 °C while sliding, and water and oil pinned at 85 and 110
// whatever happens; GT7 may only move those with engine damage.
//
// Each firing also raises a car.alarm event (contracts' CarAlarmRaised) from the
// same decision, so the callout and the event never disagree.
package temps

import (
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/ecu/race"
)

// Name is the rule's name, its key in the rules file.
const Name = "temps"

// The rule's kinds: the keys of its phrases in v1's script.yaml (contracts
// v0.2.3, Callout).
const (
	KindTyreHot   = "template_tyre_hot" // corners
	KindTyresCold = "template_tyres_cold"
	KindWater     = "template_water"
	KindOil       = "template_oil"
)

// corners are the tyres in GT7's order, as v1's CORNERS.
var corners = [4]eventsv1.Corner{
	eventsv1.Corner_CORNER_FRONT_LEFT, eventsv1.Corner_CORNER_FRONT_RIGHT,
	eventsv1.Corner_CORNER_REAR_LEFT, eventsv1.Corner_CORNER_REAR_RIGHT,
}

// thresholds are the settings v1 read as off when null or 0.
var thresholds = []string{"tyre_hot_c", "tyre_cold_c", "water_hot_c", "oil_hot_c"}

var _ = rules.Register(Name, func(s *rules.Settings) rules.Rule { return fromSettings(s) })

// fromSettings is the rule from its settings, over v1's defaults: a threshold set
// to null is off, as v1's `float(raw) if raw else None` read it.
func fromSettings(s *rules.Settings) *Rule {
	cfg := DefaultConfig()
	s.Decode(&cfg)
	for _, key := range thresholds {
		if s.Null(key) {
			cfg.set(key, 0)
		}
	}
	return New(cfg)
}

// Config is the rule's own settings, v1's keys of the same names; priority and
// pit_lane are the engine's. A threshold of 0 (or null in the file) turns its
// check off. The templates are radio's (paddock#90).
type Config struct {
	SustainSecs    float64 `yaml:"sustain_secs"`
	CooldownSecs   float64 `yaml:"cooldown_secs"`
	HysteresisC    float64 `yaml:"hysteresis_c"`
	TyreHotC       float64 `yaml:"tyre_hot_c"`
	TyreColdC      float64 `yaml:"tyre_cold_c"`
	ColdSettleSecs float64 `yaml:"cold_settle_secs"`
	WaterHotC      float64 `yaml:"water_hot_c"`
	OilHotC        float64 `yaml:"oil_hot_c"`
}

// DefaultConfig is v1's: config.DEFAULT_RULES and TempsRule.reset.
func DefaultConfig() Config {
	return Config{
		SustainSecs: 5, CooldownSecs: 60, HysteresisC: 10,
		TyreHotC: 120, TyreColdC: 50, ColdSettleSecs: 2, WaterHotC: 110, OilHotC: 130,
	}
}

func (c *Config) set(key string, v float64) {
	switch key {
	case "tyre_hot_c":
		c.TyreHotC = v
	case "tyre_cold_c":
		c.TyreColdC = v
	case "water_hot_c":
		c.WaterHotC = v
	case "oil_hot_c":
		c.OilHotC = v
	}
}

// Rule is TempsRule.
type Rule struct {
	cfg Config

	tyres       [4]alarm
	water, oil  alarm
	startedAt   float64
	started     bool // startedAt is set
	coldChecked bool
}

// New is the rule with cfg.
func New(cfg Config) *Rule {
	r := &Rule{cfg: cfg}
	r.Reset()
	return r
}

// Reset is v1's reset: new alarms, and the cold check to come.
func (r *Rule) Reset() {
	c := r.cfg
	for i := range r.tyres {
		r.tyres[i] = newAlarm(c.TyreHotC, c)
	}
	r.water = newAlarm(c.WaterHotC, c)
	r.oil = newAlarm(c.OilHotC, c)
	r.startedAt, r.started, r.coldChecked = 0, false, false
}

// Evaluate is v1's evaluate, on the frames v1 called it on that are in a race.
func (r *Rule) Evaluate(ctx *race.Context) []rules.Callout {
	if !ctx.Racing || !ctx.Cur.InRace() {
		return nil
	}
	cur := &ctx.Cur
	t := cur.T
	f := cur.Frame
	temps := [4]float64{f.GetTireFlTemp(), f.GetTireFrTemp(), f.GetTireRlTemp(), f.GetTireRrTemp()}
	var out []rules.Callout
	if !r.started {
		r.startedAt, r.started = t, true
	}
	if ctx.Joined {
		r.coldChecked = true // cold tyres are a start call: no start was seen
	}
	if !r.coldChecked && r.cfg.TyreColdC != 0 && t-r.startedAt >= r.cfg.ColdSettleSecs {
		r.coldChecked = true
		if c, ok := r.cold(temps); ok {
			out = append(out, c)
		}
	}

	var fired [4]bool
	anyFired := false
	for i := range r.tyres {
		fired[i] = r.tyres[i].update(temps[i], t)
		anyFired = anyFired || fired[i]
	}
	if anyFired {
		for i := range r.tyres {
			fired[i] = fired[i] || r.tyres[i].join(temps[i], t)
		}
		out = append(out, r.hot(fired, temps))
	}
	if r.water.update(f.GetWaterTemp(), t) {
		out = append(out, single(KindWater, eventsv1.CarCheck_CAR_CHECK_WATER_HOT, f.GetWaterTemp(), r.cfg.WaterHotC))
	}
	if r.oil.update(f.GetOilTemp(), t) {
		out = append(out, single(KindOil, eventsv1.CarCheck_CAR_CHECK_OIL_HOT, f.GetOilTemp(), r.cfg.OilHotC))
	}
	return out
}

// cold is "Tyres cold." when the coldest tyre is under the threshold, with every
// tyre under it in the alarm and the coldest its value.
func (r *Rule) cold(temps [4]float64) (rules.Callout, bool) {
	a := &eventsv1.CarAlarmRaised{Check: eventsv1.CarCheck_CAR_CHECK_TYRES_COLD, ThresholdC: r.cfg.TyreColdC}
	for i, v := range temps {
		if v < r.cfg.TyreColdC {
			a.Tyres = append(a.Tyres, &eventsv1.TyreTemp{Corner: corners[i], TempC: v})
			if len(a.Tyres) == 1 || v < a.ValueC {
				a.ValueC = v
			}
		}
	}
	if len(a.Tyres) == 0 {
		return rules.Callout{}, false
	}
	return rules.Callout{Kind: KindTyresCold, Alarm: a}, true
}

// hot is "{corners} hot." for the tyres that fired, in GT7's order, with the
// hottest of them the alarm's value.
func (r *Rule) hot(fired [4]bool, temps [4]float64) rules.Callout {
	slots := &eventsv1.CalloutSlots{}
	a := &eventsv1.CarAlarmRaised{Check: eventsv1.CarCheck_CAR_CHECK_TYRE_HOT, ThresholdC: r.cfg.TyreHotC}
	for i, f := range fired {
		if !f {
			continue
		}
		slots.Corners = append(slots.Corners, corners[i])
		a.Tyres = append(a.Tyres, &eventsv1.TyreTemp{Corner: corners[i], TempC: temps[i]})
		if len(a.Tyres) == 1 || temps[i] > a.ValueC {
			a.ValueC = temps[i]
		}
	}
	return rules.Callout{Kind: KindTyreHot, Slots: slots, Alarm: a}
}

// single is the water's or the oil's warning: no slots, and its value.
func single(kind string, check eventsv1.CarCheck, value, threshold float64) rules.Callout {
	return rules.Callout{Kind: kind, Alarm: &eventsv1.CarAlarmRaised{Check: check, ValueC: value, ThresholdC: threshold}}
}

// alarm is v1's _Alarm: one check's sustain, hysteresis and cooldown.
type alarm struct {
	threshold                     float64 // 0 is off
	sustain, cooldown, hysteresis float64

	aboveSince, lastT, firedAt          float64
	isAbove, hasLast, hasFired, unarmed bool
}

func newAlarm(threshold float64, c Config) alarm {
	return alarm{threshold: threshold, sustain: c.SustainSecs, cooldown: c.CooldownSecs, hysteresis: c.HysteresisC}
}

// update is the alarm on value at t: true when it fires.
func (a *alarm) update(value, t float64) bool {
	if a.threshold == 0 {
		return false
	}
	if a.hasLast && t-a.lastT > a.sustain {
		a.isAbove = false // a pause or a telemetry gap
	}
	a.lastT, a.hasLast = t, true
	if value <= a.threshold {
		a.isAbove = false
		if value < a.threshold-a.hysteresis {
			a.unarmed = false
		}
		return false
	}
	if !a.isAbove {
		a.aboveSince, a.isAbove = t, true
	}
	if t-a.aboveSince < a.sustain || a.unarmed {
		return false
	}
	return a.fire(t)
}

// join is v1's join: fire with another alarm that just fired, over the
// threshold, armed and past the cooldown, without waiting out the sustain window.
func (a *alarm) join(value, t float64) bool {
	if a.threshold == 0 || value <= a.threshold || a.unarmed {
		return false
	}
	if !a.fire(t) {
		return false
	}
	if !a.isAbove {
		a.aboveSince, a.isAbove = t, true
	}
	return true
}

// fire fires the alarm at t unless it is within the cooldown of the last.
func (a *alarm) fire(t float64) bool {
	if a.hasFired && t-a.firedAt < a.cooldown {
		return false
	}
	a.firedAt, a.hasFired, a.unarmed = t, true, true
	return true
}
