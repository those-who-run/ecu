package race

import (
	"testing"
	"time"
)

// posted is the Context's posted lap for a time posted on s against seen.
func posted(seen, s state) *PostedLap {
	m := New(testIDs())
	m.ruleContext(&seen, &seen, &s, time.Time{}, false, unknownKind)
	return m.Context().Posted
}

func driving(lap, total, lastMs, bestMs int32) state {
	return state{phase: phaseInRace, lap: lap, totalLaps: total, lastLapMs: lastMs, bestLapMs: bestMs}
}

// v1's posted_lap is max(counter - 1, 1): a time posted while the counter still
// reads 1 is lap 1's, not lap 0's, so a rule that keeps the first post of each
// lap counts it.
func TestATimePostedOnLap1IsLap1(t *testing.T) {
	p := posted(driving(1, 0, -1, -1), driving(1, 0, 61000, -1))
	if p == nil || p.Lap != 1 || p.TimeMs != 61000 || !p.StartLap {
		t.Errorf("posted on lap 1: %+v, want lap 1, a start lap (GT7's best blank)", p)
	}
	if p := posted(driving(3, 0, 60000, 59000), driving(4, 0, 61000, 59000)); p == nil || p.Lap != 3 || p.StartLap {
		t.Errorf("posted on the bump to 4: %+v, want lap 3, not a start lap", p)
	}
}

// v1's race_summary _start_lap reads the frame the time was posted against: a
// counter jumping from 1 to 3 posts lap 2, which v1 left out of the best, in a
// lap race or with GT7's best blank; with GT7's best set in a timed session, it
// counts.
func TestAStartLapIsReadFromTheFrameBefore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seen, s state
		lap     int32
		start   bool
	}{
		{"a jump from 1 to 3 in a lap race", driving(1, 5, -1, -1), driving(3, 5, 70000, 65000), 2, true},
		{"a jump from 1 to 3, GT7's best blank", driving(1, 0, -1, -1), driving(3, 0, 70000, -1), 2, true},
		{"a jump from 1 to 3, timed, GT7's best set", driving(1, 0, -1, -1), driving(3, 0, 70000, 65000), 2, false},
		{"lap 2 of a lap race", driving(2, 5, 60000, 59000), driving(3, 5, 61000, 59000), 2, false},
	} {
		p := posted(tc.seen, tc.s)
		if p == nil || p.Lap != tc.lap || p.StartLap != tc.start {
			t.Errorf("%s: %+v, want lap %d, start lap %v", tc.name, p, tc.lap, tc.start)
		}
	}
}
