package fuel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/ecu/internal/rules/rulestest"
)

// replayLines replays a recording through the machine and an engine running the
// fuel rule alone (only this package registers a rule here), with v1's
// defaults, and writes each session start and each callout as the golden does.
func replayLines(t *testing.T, name string) []string {
	t.Helper()
	engine, err := rules.New(rules.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.Rules(); !slices.Equal(got, []string{Name}) {
		t.Fatalf("the engine runs %v, want the fuel rule alone", got)
	}
	sessions := map[string]int{}
	var lines []string
	for _, o := range rulestest.Run(rulestest.Packets(t, name), engine) {
		if o.Started != nil {
			sessions[o.Session] = len(sessions) + 1
			lines = append(lines, fmt.Sprintf("session %d started", sessions[o.Session]))
		}
		if c := o.Callout; c != nil {
			laps := int32(0)
			if c.GetSlots() != nil {
				laps = c.GetSlots().GetLaps()
			}
			hold := 0.0
			if c.GetHoldUntil() != nil {
				hold = c.GetHoldUntil().AsTime().Sub(c.GetCreatedAt().AsTime()).Seconds()
			}
			expiry := c.GetExpiresAt().AsTime().Sub(c.GetCreatedAt().AsTime()).Seconds()
			lines = append(lines, callout(sessions[c.GetSessionId()], int(c.GetAt()), c.GetKind(), laps,
				c.GetPriority(), hold, expiry, c.GetPitLane().String()))
		}
	}
	return lines
}

func callout(session, frame int, kind string, laps, priority int32, hold, expiry float64, lane string) string {
	return fmt.Sprintf("session %d frame %d: %s laps=%d p%d hold=%gs expiry=%gs %s",
		session, frame, kind, laps, priority, hold, expiry, lane)
}

// goldenLines is testdata/<name>.jsonl (make_golden.py there) in replayLines'
// form. v1 spoke every fuel callout, in the pit lane too (pit_lane speak).
func goldenLines(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".jsonl")) //nolint:gosec // the test's own golden
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	lanes := map[string]string{"spoken": "PIT_LANE_SPEAK"}
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var g struct {
			Event    string
			Session  int
			Frame    int
			Kind     string
			Slots    struct{ Laps int32 }
			Priority int32
			HoldSecs float64 `json:"hold_secs"`
			Expiry   float64 `json:"expiry_secs"`
			PitLane  string  `json:"pit_lane"`
		}
		if err := json.Unmarshal(sc.Bytes(), &g); err != nil {
			t.Fatal(err)
		}
		if g.Event == "session.started" {
			lines = append(lines, fmt.Sprintf("session %d started", g.Session))
			continue
		}
		lane, ok := lanes[g.PitLane]
		if !ok {
			lane = "v1 " + g.PitLane
		}
		lines = append(lines, callout(g.Session, g.Frame, g.Kind, g.Slots.Laps, g.Priority, g.HoldSecs, g.Expiry, lane))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// AC1–AC7: on every one of v1's 25 recordings, the fuel rule raises the callouts
// v1.0.0's FuelRule raised, frame for frame: the same kind, laps, priority, hold,
// expiry and pit lane, in each session v1 opened.
func TestTheFuelCalloutsAreV1sOnEveryRecording(t *testing.T) {
	for _, name := range rulestest.Recordings {
		t.Run(name, func(t *testing.T) {
			got, want := replayLines(t, name), goldenLines(t, name)
			if len(want) == 0 {
				t.Fatal("an empty golden")
			}
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}
