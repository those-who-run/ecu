package laptime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/those-who-run/ecu/internal/rules/rulestest"
)

// accepted are v1's calls ecu doesn't make, by recording and frame, each with the
// decision behind it.
var accepted = map[string][]uint32{
	// GT7's counter jumps from 4 to 13 on the way to the results, and v1 called
	// "lap twelve" with no lap closed behind it: paddock#83's decision (the parity
	// diff maps it).
	"real-nuc-sugo-replay-2026-10-18": {864},
}

// goldenLine is a line of a golden (testdata/make_golden.py).
type goldenLine struct {
	Event    string `json:"event"`
	Session  int    `json:"session"`
	Frame    uint32 `json:"frame"`
	Kind     string `json:"kind"`
	Priority int32  `json:"priority"`
	HoldMs   int64  `json:"hold_ms"`
	ExpiryMs int64  `json:"expiry_ms"`
	PitLane  string `json:"pit_lane"`
	Outcome  string `json:"outcome"`
	Released uint32 `json:"released"`
	Slots    struct {
		Lap       int32  `json:"lap"`
		LapTimeMs int32  `json:"lap_time_ms"`
		DeltaMs   *int32 `json:"delta_ms"`
	} `json:"slots"`
}

// show is a callout as a line to compare: the session, the frame raised on, the
// frame published on when it was held in the pit lane, and the callout.
func show(session int, at, released uint32, kind string, priority int32, lap, ms int32, delta *int32,
	holdMs, expiryMs int64, pitLane string) string {
	d := "-"
	if delta != nil {
		d = fmt.Sprint(*delta)
	}
	held := ""
	if released != 0 {
		held = fmt.Sprintf(" held to %d", released)
	}
	return fmt.Sprintf("session %d frame %d%s: %s p%d lap %d %d delta %s hold %d expiry %d %s",
		session, at, held, kind, priority, lap, ms, d, holdMs, expiryMs, pitLane)
}

func readGolden(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.Lines(string(data)) {
		var l goldenLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		if l.Event != "callout" || slices.Contains(accepted[name], l.Frame) {
			continue
		}
		if l.Outcome != "spoken" && l.Outcome != "held" {
			t.Fatalf("frame %d: an outcome the test doesn't know: %s", l.Frame, l.Outcome)
		}
		out = append(out, show(l.Session, l.Frame, l.Released, l.Kind, l.Priority, l.Slots.Lap,
			l.Slots.LapTimeMs, l.Slots.DeltaMs, l.HoldMs, l.ExpiryMs, l.PitLane))
	}
	return out
}

// pitLanes are the contracts' PitLane as v1 named it.
var pitLanes = map[string]string{"PIT_LANE_SPEAK": "speak", "PIT_LANE_HOLD": "hold", "PIT_LANE_DROP": "drop"}

// replayed is the rule's callouts on a recording, through the race state and the
// engine with v1's defaults (rulestest.Replay), as lines to compare.
func replayed(t *testing.T, name string) []string {
	t.Helper()
	sessions := map[string]int{}
	var frame uint32
	var out []string
	for _, o := range rulestest.Replay(t, name) {
		switch {
		case o.Frame != nil:
			frame = o.Frame.GetSeq()
		case o.Started != nil:
			sessions[o.Session] = len(sessions) + 1
		case o.Callout != nil && o.Callout.GetRule() == Name:
			c := o.Callout
			var released uint32
			if c.GetAt() != frame {
				released = frame
			}
			var hold int64
			if c.GetHoldUntil() != nil {
				hold = c.GetHoldUntil().AsTime().Sub(c.GetCreatedAt().AsTime()).Milliseconds()
			}
			expiry := c.GetExpiresAt().AsTime().Sub(c.GetCreatedAt().AsTime()).Milliseconds()
			s := c.GetSlots()
			out = append(out, show(sessions[c.GetSessionId()], c.GetAt(), released, c.GetKind(), c.GetPriority(),
				s.GetLap(), s.GetLapTimeMs(), s.DeltaMs, hold, expiry, pitLanes[c.GetPitLane().String()]))
		}
	}
	return out
}

// R1 to R8, AC1 to AC4, AC6: on every one of v1's recordings the rule raises
// v1's callouts frame for frame, with v1's kind (contracts' table), slots,
// priority, hold, expiry and pit lane, held in the lane to the frame v1 released
// them, but for the accepted differences.
func TestEveryLapCallIsV1sOnEveryRecording(t *testing.T) {
	for _, name := range rulestest.Recordings {
		t.Run(name, func(t *testing.T) {
			got, want := replayed(t, name), readGolden(t, name)
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// AC4: at Willow Springs the in-lap, posted on a loading frame in the pit cut
// (frame 204), is still called, as v1 called it.
func TestTheWillowSpringsInLapIsCalled(t *testing.T) {
	got := replayed(t, "real-nuc-willow-springs-pit-stop-2026-10-17")
	if !slices.ContainsFunc(got, func(l string) bool {
		return strings.HasPrefix(l, "session 1 frame 204: "+KindInLap+" p20 lap 6 96562 ")
	}) {
		t.Errorf("no call of the in-lap, lap 6, on frame 204 in\n%s", strings.Join(got, "\n"))
	}
}
