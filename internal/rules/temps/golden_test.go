package temps

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules/rulestest"
)

// goldenLine is a line of a golden (testdata/make_golden.py).
type goldenLine struct {
	Event      string     `json:"event"`
	Session    int        `json:"session"`
	Frame      uint32     `json:"frame"`
	Kind       string     `json:"kind"`
	Corners    []string   `json:"corners"`
	TyresC     [4]float64 `json:"tyres_c"`
	WaterC     float64    `json:"water_c"`
	OilC       float64    `json:"oil_c"`
	Priority   int32      `json:"priority"`
	HoldSecs   float64    `json:"hold_secs"`
	MaxAgeSecs *float64   `json:"max_age_secs"`
	PitLane    string     `json:"pit_lane"`
	Outcome    string     `json:"outcome"`
}

// abbrev are the golden's corners, in GT7's order.
var abbrev = map[eventsv1.Corner]string{
	eventsv1.Corner_CORNER_FRONT_LEFT: "fl", eventsv1.Corner_CORNER_FRONT_RIGHT: "fr",
	eventsv1.Corner_CORNER_REAR_LEFT: "rl", eventsv1.Corner_CORNER_REAR_RIGHT: "rr",
}

// alarmOf is the car.alarm v1's firing stands for (contracts' CarAlarmRaised), as
// a line: the check, the tyres with their temperatures, the value and the
// threshold.
func (l goldenLine) alarmOf() string {
	idx := map[string]int{"fl": 0, "fr": 1, "rl": 2, "rr": 3}
	switch l.Kind {
	case KindTyreHot:
		var tyres []string
		value := 0.0
		for _, c := range l.Corners {
			v := l.TyresC[idx[c]]
			tyres = append(tyres, fmt.Sprintf("%s=%g", c, v))
			value = max(value, v)
		}
		return fmt.Sprintf("tyre_hot %s value %g threshold 120", strings.Join(tyres, ","), value)
	case KindTyresCold:
		var tyres []string
		value := 50.0
		for i, v := range l.TyresC {
			if v < 50 {
				tyres = append(tyres, fmt.Sprintf("%s=%g", []string{"fl", "fr", "rl", "rr"}[i], v))
				value = min(value, v)
			}
		}
		return fmt.Sprintf("tyres_cold %s value %g threshold 50", strings.Join(tyres, ","), value)
	case KindWater:
		return fmt.Sprintf("water_hot  value %g threshold 110", l.WaterC)
	default:
		return fmt.Sprintf("oil_hot  value %g threshold 130", l.OilC)
	}
}

func (l goldenLine) String() string {
	return format(l.Session, l.Frame, l.Kind, l.Corners, l.alarmOf())
}

// format is a call as the test compares it: where, what, and its car.alarm.
func format(session int, frame uint32, kind string, corners []string, alarm string) string {
	return fmt.Sprintf("session %d frame %d %s %s | %s", session, frame, kind, strings.Join(corners, ","), alarm)
}

// published is a golden's calls ecu publishes, as lines: all but those raised and
// dropped in the pit lane, or held there and never released.
func published(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.Lines(string(data)) {
		var l goldenLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		if l.Event != "callout" {
			continue
		}
		if l.Priority != 14 || l.HoldSecs != 0 || l.MaxAgeSecs != nil || l.PitLane != "hold" {
			t.Fatalf("%s: not v1's priority 14, no hold, no max age and pit_lane hold", l)
		}
		if l.Outcome != "spoken" {
			t.Fatalf("%s: outcome %s, which this test does not handle yet", l, l.Outcome)
		}
		lines = append(lines, l.String())
	}
	return lines
}

// replay runs a recording through the machine and an engine with v1's defaults
// (rulestest.Replay), and returns the rule's callouts as a golden's lines, each
// with the car.alarm published just before it, checking what the engine stamped
// on both.
func replay(t *testing.T, name string) []string {
	t.Helper()
	sessions := map[string]int{}
	var lines []string
	var alarm *race.Output
	for _, o := range rulestest.Replay(t, name) {
		if o.Started != nil {
			sessions[o.Session] = len(sessions) + 1
		}
		if o.Alarm != nil {
			alarm = &o
			continue
		}
		c := o.Callout
		if c == nil || c.GetRule() != Name {
			continue
		}
		created := c.GetCreatedAt().AsTime()
		if c.GetPriority() != 14 || c.HoldUntil != nil || c.GetExpiresAt().AsTime().Sub(created) != 8*time.Second ||
			c.GetPitLane() != eventsv1.PitLane_PIT_LANE_HOLD {
			t.Errorf("callout %v: not priority 14, no hold, an 8 s life and pit_lane hold", c)
		}
		var corners []string
		for _, corner := range c.GetSlots().GetCorners() {
			corners = append(corners, abbrev[corner])
		}
		lines = append(lines, format(sessions[c.GetSessionId()], c.GetAt(), c.GetKind(), corners, show(t, alarm, c)))
		alarm = nil
	}
	if alarm != nil {
		t.Errorf("car.alarm %v with no callout", alarm.Alarm)
	}
	return lines
}

// show is the car.alarm o, published just before callout c, as goldenLine's
// alarmOf, checking its session, frame, run and id are c's.
func show(t *testing.T, o *race.Output, c *eventsv1.Callout) string {
	t.Helper()
	if o == nil {
		t.Errorf("callout %v with no car.alarm before it", c)
		return ""
	}
	a := o.Alarm
	check := strings.ToLower(strings.TrimPrefix(a.GetCheck().String(), "CAR_CHECK_"))
	if a.GetSessionId() != c.GetSessionId() || o.Session != c.GetSessionId() || a.GetAt() != c.GetAt() ||
		a.GetRunId() != c.GetRunId() || a.GetRunId() == "" ||
		o.Event.ID != fmt.Sprintf("%s:car.alarm:%s:%s:%d", c.GetSessionId(), check, c.GetRunId(), c.GetAt()) ||
		!o.Event.Time.Equal(c.GetCreatedAt().AsTime()) {
		t.Errorf("car.alarm %v (id %s) does not match callout %v", a, o.Event.ID, c)
	}
	var tyres []string
	for _, tt := range a.GetTyres() {
		tyres = append(tyres, fmt.Sprintf("%s=%g", abbrev[tt.GetCorner()], tt.GetTempC()))
	}
	return fmt.Sprintf("%s %s value %g threshold %g", check, strings.Join(tyres, ","), a.GetValueC(), a.GetThresholdC())
}

// pending are calls ecu raises that v1 did not, by recording and line, each with
// the open paddock issue that removes it (internal/rules' golden test has the
// same, and says why). Each must still be raised until its fix lands.
var pending = map[string]map[string]string{
	"real-nuc-sugo-restarts-2026-10-18": {
		"session 1 frame 554 template_tyre_hot rl,rr | tyre_hot rl=178.83792114257812,rr=134.8776092529297 value 178.83792114257812 threshold 120": "https://github.com/those-who-run/paddock/issues/77",
	},
}

// R1–R6, AC1, AC2, AC3: on every one of v1's recordings, the rule raises
// v1's TempsRule's calls (rules.example.yaml's thresholds), on the same frame of
// the same session, with the kind contracts' table names and the corners as
// slots; the engine stamps v1's priority 14 and pit_lane hold; and each one comes
// with one car.alarm on its frame naming the corners, their temperatures and the
// threshold. Suzuka joined mid-race has no cold call (AC2); the Tsukuba drift's
// three calls (AC1) are pinned without the session, below: the replay and the
// demos, the drift among them, are dropped by v1's start check and ecu's
// (paddock#74) and have none.
func TestTheCallsAreV1sOnEveryRecording(t *testing.T) {
	total := 0
	for _, name := range rulestest.Recordings {
		t.Run(name, func(t *testing.T) {
			want := published(t, name)
			total += len(want)
			var got []string
			extra := maps.Clone(pending[name])
			for _, l := range replay(t, name) {
				if why, ok := extra[l]; ok {
					t.Logf("pending: %s (%s)", l, why)
					delete(extra, l)
					continue
				}
				got = append(got, l)
			}
			for line, why := range extra {
				t.Errorf("pending %s (%s) is no longer raised: delete its line", line, why)
			}
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
	// v1's only calls on its recordings are the Tsukuba drift's, a demo its start
	// check drops (paddock#74): TestTheDriftsHotRearsAreOneCallEachTime pins them.
	if total != 0 {
		t.Errorf("%d calls in the goldens, want none", total)
	}
}

// AC1: the Tsukuba drift's frames fed to the rule directly, as v1's
// test_real_drift_rears_are_one_warning_each_time fed them (run_rule: every frame
// in a race or continuing one from the frame before), with no session and no
// start check, since the session itself is a GT7 demo that v1 and ecu discard
// (paddock#74). The hot rears are one call each time, three in all, on v1's
// frames.
func TestTheDriftsHotRearsAreOneCallEachTime(t *testing.T) {
	r := New(DefaultConfig())
	m := race.New(rulestest.IDs())
	var prev *race.View
	var got []string
	for _, p := range rulestest.Packets(t, "real-nuc-tsukuba-brz-drift-2026-10-15") {
		m.Step(p)
		cur := m.Context().Cur
		ctx := &race.Context{Cur: cur, Prev: prev, Racing: cur.InRace() || race.RacingFrame(prev, &cur)}
		for _, c := range r.Evaluate(ctx) {
			l := fmt.Sprintf("frame %d %s", cur.Seq, c.Kind)
			for _, corner := range c.Slots.GetCorners() {
				l += " " + abbrev[corner]
			}
			got = append(got, l)
		}
		prev = &cur
	}
	want := []string{
		"frame 45 template_tyre_hot rl rr",
		"frame 164 template_tyre_hot rl rr",
		"frame 286 template_tyre_hot rl rr",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want v1's %q", got, want)
	}
}
