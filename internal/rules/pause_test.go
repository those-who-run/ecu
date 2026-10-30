package rules_test

import (
	"testing"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/rules/rulestest"
)

// R4 of paddock#77: nothing is said in a pause. On every one of v1's recordings,
// no callout is raised on a paused frame, flagged or frozen (AC1: SUGO's 45 s
// frozen stream on lap 2 among them), as v1's rules never evaluated one.
func TestNoCalloutIsRaisedInAPause(t *testing.T) {
	var pausedFrames int
	for _, name := range rulestest.Recordings {
		t.Run(name, func(t *testing.T) {
			phase := map[uint32]telemetryv1.Phase{}
			for _, o := range rulestest.Replay(t, name) {
				switch {
				case o.Frame != nil:
					phase[o.Frame.GetSeq()] = o.Frame.GetPhase()
					if o.Frame.GetPhase() == telemetryv1.Phase_PHASE_PAUSED {
						pausedFrames++
					}
				case o.Callout != nil:
					if p := phase[o.Callout.GetAt()]; p == telemetryv1.Phase_PHASE_PAUSED {
						t.Errorf("%s raised on paused frame %d", o.Callout.GetRule(), o.Callout.GetAt())
					}
				}
			}
		})
	}
	if pausedFrames == 0 {
		t.Error("no paused frame in any recording")
	}
}
