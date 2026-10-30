package position

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/rules/rulestest"
)

// goldenLine is a line of a golden (testdata/make_golden.py).
type goldenLine struct {
	Event            string   `json:"event"`
	Session          int      `json:"session"`
	Frame            uint32   `json:"frame"`
	Kind             string   `json:"kind"`
	Position         int32    `json:"position"`
	PreviousPosition int32    `json:"previous_position"`
	Priority         int32    `json:"priority"`
	HoldSecs         float64  `json:"hold_secs"`
	MaxAgeSecs       *float64 `json:"max_age_secs"`
	PitLane          string   `json:"pit_lane"`
	Outcome          string   `json:"outcome"`
}

func (l goldenLine) String() string {
	return fmt.Sprintf("session %d frame %d %s P%d to P%d", l.Session, l.Frame, l.Kind, l.PreviousPosition, l.Position)
}

// published is a golden's calls ecu publishes, as lines: all but those raised and
// dropped in the pit lane. One still held at a pit entry (dropped) is published,
// with its hold, for radio to drop (contracts' PitLane).
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
		if l.Event != "callout" || l.Outcome == "unpublished" {
			continue
		}
		if l.Priority != 12 || l.HoldSecs != 1 || l.MaxAgeSecs != nil || l.PitLane != "drop" {
			t.Fatalf("%s: not v1's priority 12, hold 1 s, no max age and pit_lane drop", l)
		}
		if l.Outcome != "spoken" && l.Outcome != "dropped" {
			t.Fatalf("%s: outcome %s", l, l.Outcome)
		}
		lines = append(lines, l.String())
	}
	return lines
}

// replay runs a recording through the machine and an engine with v1's defaults
// (rulestest.Replay), and returns the rule's callouts as a golden's lines,
// checking what the engine stamped on each.
func replay(t *testing.T, name string) []string {
	t.Helper()
	sessions := map[string]int{}
	var lines []string
	for _, o := range rulestest.Replay(t, name) {
		if o.Started != nil {
			sessions[o.Session] = len(sessions) + 1
		}
		c := o.Callout
		if c == nil || c.GetRule() != Name {
			continue
		}
		created := c.GetCreatedAt().AsTime()
		if c.GetPriority() != 12 || c.GetHoldUntil().AsTime().Sub(created) != time.Second ||
			c.GetExpiresAt().AsTime().Sub(created) != 8*time.Second || c.GetPitLane() != eventsv1.PitLane_PIT_LANE_DROP {
			t.Errorf("callout %v: not priority 12, a 1 s hold, an 8 s life and pit_lane drop", c)
		}
		s := c.GetSlots()
		lines = append(lines, goldenLine{
			Session: sessions[c.GetSessionId()], Frame: c.GetAt(), Kind: c.GetKind(),
			Position: s.GetPosition(), PreviousPosition: s.GetPreviousPosition(),
		}.String())
	}
	return lines
}

// R1–R7, AC1–AC5: on every one of v1's recordings, the rule raises v1's
// PositionRule's calls (rules.example.yaml's 15 s grace, 10 s between calls and
// 3 s settle), on the same frame of the same session, with the kind contracts'
// table names and the places as slots, and the engine stamps v1's priority, hold
// and pit lane. v1's one call still held at a pit entry (SUGO's two stops, frame
// 421) is published, for radio to drop. The replay and the demos v1's start check
// drops (paddock#74) have none.
func TestTheCallsAreV1sOnEveryRecording(t *testing.T) {
	total := 0
	for _, name := range rulestest.Recordings {
		t.Run(name, func(t *testing.T) {
			want := published(t, name)
			total += len(want)
			if got := replay(t, name); !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
	if total != 109 {
		t.Errorf("%d calls in the goldens, want v1's 109", total)
	}
}
