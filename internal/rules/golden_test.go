package rules_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
	_ "github.com/those-who-run/ecu/internal/rules/all"
	"github.com/those-who-run/ecu/internal/rules/rulestest"
)

// goldenLine is a line of testdata/callouts/make_golden.py's output: a callout v1
// raised.
type goldenLine struct {
	Session  int    `json:"session"`
	Frame    uint32 `json:"frame"`
	Rule     string `json:"rule"`
	Kind     string `json:"kind"`
	Priority int32  `json:"priority"`
	PitLane  string `json:"pit_lane"`
	Outcome  string `json:"outcome"`
	Released uint32 `json:"released"`
}

// show is a callout as the golden test compares it: where, what, and the frame a
// held one was released on.
func (l goldenLine) show() string {
	s := fmt.Sprintf("session %d frame %d %s %s p%d %s", l.Session, l.Frame, l.Rule, l.Kind, l.Priority, l.PitLane)
	if l.Released != 0 {
		s += fmt.Sprintf(" released %d", l.Released)
	}
	return s
}

// accepted are v1's callouts ecu does not raise on purpose, by recording and the
// line's show(), with the decision that says so. Each rule's port adds its own.
var accepted = map[string]map[string]string{}

func readGolden(t *testing.T, name string) []goldenLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "callouts", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var all []goldenLine
	for line := range strings.Lines(string(data)) {
		var l goldenLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		all = append(all, l)
	}
	return all
}

// published are a replay's callouts as golden lines: sessions numbered as they
// start, and a callout published on a frame after the one it was raised on
// released on that frame.
func published(out []race.Output) []goldenLine {
	sessions := map[string]int{}
	var frame uint32
	var lines []goldenLine
	for _, o := range out {
		switch {
		case o.Frame != nil:
			frame = o.Frame.GetSeq()
		case o.Started != nil:
			sessions[o.Session] = len(sessions) + 1
		case o.Callout != nil:
			c := o.Callout
			l := goldenLine{
				Session: sessions[c.GetSessionId()], Frame: c.GetAt(), Rule: c.GetRule(), Kind: c.GetKind(),
				Priority: c.GetPriority(), PitLane: strings.ToLower(strings.TrimPrefix(c.GetPitLane().String(), "PIT_LANE_")),
			}
			if frame != c.GetAt() {
				l.Released = frame
			}
			lines = append(lines, l)
		}
	}
	return lines
}

// R6 and the parity of every rule ported: on each of v1's 25 recordings, the
// callouts of every rule registered are v1's, on the same frame of the same
// session, with the same kind, priority and pit lane, and a callout v1 held in
// the pit lane is published on the frame v1 released it. A callout v1 raised in
// the lane and dropped is never published. The order within a frame is not
// compared: the parity diff matches by rule and frame.
func TestEveryCalloutIsV1sOnEveryRecording(t *testing.T) {
	e, err := rules.New(rules.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ported := e.Rules()
	if len(ported) == 0 {
		t.Fatal("no rule registered")
	}
	var compared int
	for _, name := range rulestest.Recordings {
		t.Run(name, func(t *testing.T) {
			var want []string
			for _, l := range readGolden(t, name) {
				if !slices.Contains(ported, l.Rule) || l.Outcome == "unpublished" {
					continue
				}
				if why, ok := accepted[name][l.show()]; ok {
					t.Logf("accepted: %s (%s)", l.show(), why)
					continue
				}
				want = append(want, l.show())
			}
			var got []string
			for _, l := range published(rulestest.Replay(t, name)) {
				got = append(got, l.show())
			}
			slices.Sort(got)
			slices.Sort(want)
			compared += len(want)
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
	if compared == 0 {
		t.Error("no callout of a registered rule in any golden")
	}
}
