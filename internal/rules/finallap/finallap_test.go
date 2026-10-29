package finallap_test

import (
	"testing"

	"github.com/those-who-run/contracts/callout"
	_ "github.com/those-who-run/ecu/internal/rules/finallap"
	"github.com/those-who-run/ecu/internal/rules/rulestest"
)

// paddock#93: on the two-lap race, "Final lap." once, on the frame the lap
// counter reaches 2 (v1's frame 198), as the script's template with no slots, on
// .p1 (priority 10), held in the pit lane. The parity with v1 on every recording
// is internal/rules' TestEveryCalloutIsV1sOnEveryRecording.
func TestTheTwoLapRaceGetsOneFinalLap(t *testing.T) {
	var n int
	for _, o := range rulestest.Replay(t, "real-race-2laps-2026-10-14") {
		c := o.Callout
		if c == nil || c.GetRule() != "final_lap" {
			continue
		}
		n++
		if c.GetAt() != 198 || c.GetKind() != "template" || c.GetSlots() != nil ||
			callout.LevelOf(c.GetPriority()) != callout.P1 || c.GetPitLane().String() != "PIT_LANE_HOLD" {
			t.Errorf("final lap: %v", c)
		}
	}
	if n != 1 {
		t.Errorf("%d final laps, want 1", n)
	}
}

// A timed race has no lap total, so no final lap.
func TestATimedRaceHasNoFinalLap(t *testing.T) {
	for _, o := range rulestest.Replay(t, "real-nuc-suzuka-timed-race-2026-10-16") {
		if o.Callout != nil && o.Callout.GetRule() == "final_lap" {
			t.Errorf("final lap in a timed race: %v", o.Callout)
		}
	}
}
