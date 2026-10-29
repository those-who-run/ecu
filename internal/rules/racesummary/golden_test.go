package racesummary

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/ecu/internal/rules/rulestest"
)

// summaries replays a recording through the machine and the engine, with v1's
// defaults, and gives each of this rule's callouts as the golden writes it: the
// session (numbered from 1), the frame raised on, the frame it is created as of,
// the kind and slots, the priority, life and hold, and its pit lane.
func summaries(t *testing.T, name string) []string {
	t.Helper()
	all := rulestest.Packets(t, name)
	e, err := rules.New(rules.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	session := 0
	for _, o := range rulestest.Run(all, e) {
		if o.Started != nil {
			session++
		}
		c := o.Callout
		if c.GetRule() != Name {
			continue
		}
		created := c.GetCreatedAt().AsTime()
		asOf := slices.IndexFunc(all, func(r race.Received) bool { return r.Time.Equal(created) }) + 1
		hold := 0.0
		if c.HoldUntil != nil {
			hold = c.GetHoldUntil().AsTime().Sub(created).Seconds()
		}
		lines = append(lines, line(session, int(c.GetAt()), asOf, c.GetKind(), slotsOf(c.GetSlots()),
			c.GetPriority(), c.GetExpiresAt().AsTime().Sub(created).Seconds(), hold, c.GetPitLane()))
	}
	return lines
}

func slotsOf(s *eventsv1.CalloutSlots) map[string]int32 {
	out := map[string]int32{}
	if s.Position != nil {
		out["position"] = s.GetPosition()
	}
	if s.BestLapMs != nil {
		out["best_lap_ms"] = s.GetBestLapMs()
	}
	if s.Laps != nil {
		out["laps"] = s.GetLaps()
	}
	return out
}

var pitLanes = map[string]eventsv1.PitLane{
	"speak": eventsv1.PitLane_PIT_LANE_SPEAK,
	"hold":  eventsv1.PitLane_PIT_LANE_HOLD,
	"drop":  eventsv1.PitLane_PIT_LANE_DROP,
}

func line(session, at, asOf int, kind string, slots map[string]int32, priority int32, life, hold float64, pitLane eventsv1.PitLane) string {
	keys := make([]string, 0, len(slots))
	for k := range slots {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var s []string
	for _, k := range keys {
		s = append(s, fmt.Sprintf("%s=%d", k, slots[k]))
	}
	return fmt.Sprintf("session %d at %d as of %d: %s {%s} p%d life %gs hold %gs %s",
		session, at, asOf, kind, strings.Join(s, " "), priority, life, hold, pitLane)
}

// golden is a callout line of testdata/race_summary/<name>.jsonl (make_golden.py).
type golden struct {
	Event    string           `json:"event"`
	Session  int              `json:"session"`
	At       int              `json:"at"`
	AsOf     int              `json:"as_of"`
	Kind     string           `json:"kind"`
	Slots    map[string]int32 `json:"slots"`
	Priority int32            `json:"priority"`
	MaxAge   float64          `json:"max_age"`
	Hold     float64          `json:"hold"`
	PitLane  string           `json:"pit_lane"`
	Outcome  string           `json:"outcome"`
}

// readGolden is each callout v1 published, in the golden's order: a callout v1
// dropped in the pit lane is not published, and a held one would be published
// at its release (race_summary speaks in the lane, so v1 held and dropped none).
func readGolden(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "race_summary", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sessions := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var g golden
		if err := json.Unmarshal(sc.Bytes(), &g); err != nil {
			t.Fatal(err)
		}
		switch g.Event {
		case "session.started":
			sessions++
		case "callout":
			if g.Outcome != "spoken" {
				t.Fatalf("v1 %s a race_summary callout: %s", g.Outcome, sc.Text())
			}
			lines = append(lines, line(g.Session, g.At, g.AsOf, g.Kind, g.Slots, g.Priority, g.MaxAge, g.Hold, pitLanes[g.PitLane]))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if sessions == 0 {
		t.Fatal("a golden with no session")
	}
	return lines
}

// R1–R6, AC1–AC7: on each of v1's 25 recordings the rule raises v1's
// race_summary callouts, frame for frame: in the same session, on the same frame,
// created as of the same frame (the timed finish's crossing), with the kind
// contracts v0.2.3 names and the slots v1 said, priority 25, a 30 s life, no
// hold, and spoken in the pit lane. The recordings v1 opens no session on are
// held to v1's engine taking every start for a drive (make_golden.py --drive),
// as ecu does until paddock#74.
//
// AC1, the practice then the race, "Seven laps" and "Finished sixth":
// real-race-2laps at frames 75 and 263. AC2, P1 at the flag: suzuka-pit-stop at
// 843. AC3, the timed finish at the line, P1: suzuka-timed-race at 752, as of
// the crossing at 745. AC4, leaving through the menu with no position:
// fuji-timed at 633. AC5, no quit summary for the restarted races:
// sugo-restarts has only the last race's finish.
func TestTheRaceSummaryIsV1sOnEveryRecording(t *testing.T) {
	for _, name := range rulestest.Recordings {
		t.Run(name, func(t *testing.T) {
			got, want := summaries(t, name), readGolden(t, name)
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}
