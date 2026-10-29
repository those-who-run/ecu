package rules

import (
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// race feeds the rule one frame every half second from t, with a field, and
// returns each call with the time it was raised at.
type race struct {
	p     *Position
	t     float64
	pit   bool
	field bool
	calls []timedCall
}

type timedCall struct {
	t    float64
	call PositionCall
}

func newRace() *race { return &race{p: NewPosition(DefaultPositionConfig), field: true} }

// hold drives secs at pos.
func (r *race) hold(pos int32, secs float64) {
	for end := r.t + secs; r.t < end; r.t += 0.5 {
		f := PositionFrame{T: r.t, InRace: true, Position: pos, HasField: r.field, InPitLane: r.pit}
		if c, ok := r.p.Evaluate(f); ok {
			r.calls = append(r.calls, timedCall{r.t, c})
		}
	}
}

func (r *race) want(t *testing.T, want ...timedCall) {
	t.Helper()
	if len(r.calls) != len(want) {
		t.Fatalf("calls %+v, want %+v", r.calls, want)
	}
	for i := range want {
		if r.calls[i] != want[i] {
			t.Errorf("call %d: %+v, want %+v", i, r.calls[i], want[i])
		}
	}
}

// R2: the grid slot follows the first 3 s quietly (P20 then P1), nothing is called
// in the first 15 s, and the first call after the grace compares with the grid slot.
func TestPositionGridSlotAndStartGrace(t *testing.T) {
	r := newRace()
	r.hold(20, 0.5)
	r.hold(5, 4.5)  // the grid slot, settled by 3 s
	r.hold(3, 13.5) // from t 5: settles in the grace, called 3 s after it
	r.want(t, timedCall{18, PositionCall{KindGainedPodium, 3, 5}})
}

// R3: a change is called once it has held 3 s, the latest place once things calm
// down, and no sooner than 10 s after the last call.
func TestPositionSettleAndInterval(t *testing.T) {
	r := newRace()
	r.hold(8, 20)
	r.hold(7, 2.5) // under 3 s: never settles
	r.hold(8, 1)
	r.hold(7, 1)
	r.hold(6, 4) // settling since 23.5, called at 26.5
	r.hold(9, 9) // settled at 31.5, waits for 36.5
	r.want(t,
		timedCall{26.5, PositionCall{KindGained, 6, 8}},
		timedCall{36.5, PositionCall{KindLost, 9, 6}},
	)
}

// R4: quiet in the pit lane, settling again from the exit; the last place called is
// kept, so one call gives the net change.
func TestPositionPitLane(t *testing.T) {
	r := newRace()
	r.hold(5, 20)
	r.pit = true
	for pos := int32(6); pos <= 11; pos++ {
		r.hold(pos, 5)
	}
	r.pit = false
	r.hold(10, 4)
	r.want(t, timedCall{53, PositionCall{KindLost, 10, 5}})
}

// R1: nothing without a field (a time trial's ghost), nor without a position or
// off a driving frame.
func TestPositionNeedsAField(t *testing.T) {
	r := newRace()
	r.field = false
	r.hold(2, 20)
	r.hold(1, 20)
	r.want(t)
	p := NewPosition(DefaultPositionConfig)
	for i := range 80 {
		ts := float64(i) / 2
		for _, f := range []PositionFrame{
			{T: ts, InRace: true, Position: 0, HasField: true},
			{T: ts, InRace: false, Position: int32(1 + i%5), HasField: true},
		} {
			if c, ok := p.Evaluate(f); ok {
				t.Errorf("called %+v on %+v", c, f)
			}
		}
	}
}

// R6: the situational pools.
func TestPositionKinds(t *testing.T) {
	for _, c := range []struct {
		was, pos int32
		want     string
	}{
		{2, 1, KindGainedLead},
		{5, 3, KindGainedPodium},
		{3, 2, KindGainedPodium},
		{9, 4, KindGained},
		{1, 2, KindLostLead},
		{1, 7, KindLostLead},
		{2, 3, KindLost},
	} {
		if got := positionKind(c.was, c.pos); got != c.want {
			t.Errorf("P%d to P%d: %s, want %s", c.was, c.pos, got, c.want)
		}
	}
}

// R5, R7: v1's defaults; the slots are the place and the one before it.
func TestPositionDefaultsAndSlots(t *testing.T) {
	if PositionPriority != 12 || PositionHoldSecs != 1 || PositionPitLane != eventsv1.PitLane_PIT_LANE_DROP {
		t.Error("not v1's priority 12, hold_secs 1 and pit_lane drop")
	}
	s := PositionCall{KindGained, 6, 8}.Slots()
	if s.GetPosition() != 6 || s.GetPreviousPosition() != 8 {
		t.Errorf("slots %v", s)
	}
}

// Reset forgets the race but keeps the settings.
func TestPositionReset(t *testing.T) {
	r := newRace()
	r.hold(8, 20)
	r.hold(7, 4)
	r.p.Reset()
	r.hold(3, 20) // a new grid slot: quiet again
	r.want(t, timedCall{23, PositionCall{KindGained, 7, 8}})
}
