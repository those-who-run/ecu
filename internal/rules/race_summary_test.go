package rules

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

const (
	soloLaps  = eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS
	soloTimed = eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED
	lapRace   = eventsv1.SessionType_SESSION_TYPE_LAP_RACE
)

func i32(v int32) *int32 { return &v }

func eqInt(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func show(v *int32) any {
	if v == nil {
		return "none"
	}
	return *v
}

// want checks a call's kind and its position, best_lap_ms and laps slots.
func want(t *testing.T, c Call, ok bool, kind string, position, best, laps *int32) {
	t.Helper()
	if !ok {
		t.Fatalf("no call, want %s", kind)
	}
	if c.Rule != "race_summary" || c.Kind != kind {
		t.Errorf("got %s %s, want race_summary %s", c.Rule, c.Kind, kind)
	}
	s := c.Slots
	if !eqInt(s.Position, position) || !eqInt(s.BestLapMs, best) || !eqInt(s.Laps, laps) {
		t.Errorf("slots position %v best %v laps %v, want %v %v %v",
			show(s.Position), show(s.BestLapMs), show(s.Laps), show(position), show(best), show(laps))
	}
}

// R6: v1's defaults: priority 25, 30 s, spoken in the pit lane, a quit summary for
// solo_timed and solo_laps after one lap.
func TestTheRaceSummaryDefaultsAreV1s(t *testing.T) {
	cfg := DefaultRaceSummary()
	if cfg.Priority != 25 || cfg.MaxAge != 30*time.Second || cfg.PitLane != eventsv1.PitLane_PIT_LANE_SPEAK {
		t.Errorf("got priority %d, max age %s, pit lane %s", cfg.Priority, cfg.MaxAge, cfg.PitLane)
	}
	if len(cfg.QuitKinds) != 2 || cfg.QuitKinds[0] != soloTimed || cfg.QuitKinds[1] != soloLaps || cfg.QuitMinLaps != 1 {
		t.Errorf("got quit kinds %v after %d laps", cfg.QuitKinds, cfg.QuitMinLaps)
	}
	if NewRaceSummary(cfg).Config().Priority != 25 {
		t.Error("the rule doesn't keep its config")
	}
}

// R1, R2: the finish is said once, with the position off the finish's frame when
// there is a field, in the pool its place picks.
func TestTheFinishIsSaidOnceWithItsPlace(t *testing.T) {
	for _, tc := range []struct {
		position int32
		kind     string
	}{
		{1, KindTemplatePositionWin},
		{2, KindTemplatePositionPodium},
		{3, KindTemplatePositionPodium},
		{6, KindTemplatePosition},
	} {
		r := NewRaceSummary(DefaultRaceSummary())
		c, ok := r.Finished(Finish{Position: tc.position, Field: true, GT7BestMs: 53108})
		want(t, c, ok, tc.kind, i32(tc.position), i32(53108), nil)
		if _, again := r.Finished(Finish{Position: tc.position, Field: true}); again {
			t.Error("the finish was said twice")
		}
		if _, end := r.Ended(End{Kind: soloLaps, CurrentLap: 5}); end {
			t.Error("a quit summary after the finish")
		}
	}
}

// R2: no field, no position: the template, with the best alone.
func TestTheFinishWithoutAFieldSaysNoPlace(t *testing.T) {
	r := NewRaceSummary(DefaultRaceSummary())
	c, ok := r.Finished(Finish{Position: 1, Field: false, GT7BestMs: 82190})
	want(t, c, ok, KindTemplate, nil, i32(82190), nil)

	r.Reset()
	c, ok = r.Finished(Finish{Field: true})
	want(t, c, ok, KindTemplate, nil, nil, nil) // Suzuka East: "Chequered flag."
}

// R3: GT7's raw best on the frame when it has one, even one better than every
// lap seen; else the best counted lap, leaving out a start lap; else any lap.
// Each lap counts once, on its first post.
func TestTheBestLap(t *testing.T) {
	r := NewRaceSummary(DefaultRaceSummary())
	r.Posted(PostedLap{Lap: 1, Ms: 50000, StartLap: true})
	r.Posted(PostedLap{Lap: 2, Ms: 54000})
	r.Posted(PostedLap{Lap: 2, Ms: 52000}) // a second post for lap 2
	r.Posted(PostedLap{Lap: 3, Ms: 53000})
	r.Posted(PostedLap{Lap: 4, Ms: 0}) // no time
	c, ok := r.Finished(Finish{Field: true, Position: 6, GT7BestMs: 49000})
	want(t, c, ok, KindTemplatePosition, i32(6), i32(49000), nil)

	r.Reset()
	r.Posted(PostedLap{Lap: 1, Ms: 50000, StartLap: true})
	r.Posted(PostedLap{Lap: 2, Ms: 54000})
	r.Posted(PostedLap{Lap: 2, Ms: 52000})
	r.Posted(PostedLap{Lap: 3, Ms: 53000})
	c, ok = r.Finished(Finish{Field: true, Position: 6})
	want(t, c, ok, KindTemplatePosition, i32(6), i32(53000), nil)

	r.Reset()
	r.Posted(PostedLap{Lap: 1, Ms: 50000, StartLap: true})
	c, ok = r.Finished(Finish{})
	want(t, c, ok, KindTemplate, nil, i32(50000), nil)
}

// R5, AC4, AC5: leaving a solo session without a flag gives template_quit, laps
// counted from GT7's counter too; never for a race, nor under quit_min_laps.
func TestLeavingASoloSession(t *testing.T) {
	r := NewRaceSummary(DefaultRaceSummary())
	r.Posted(PostedLap{Lap: 2, Ms: 120314}) // joined too late to see lap 1
	c, ok := r.Ended(End{Kind: soloTimed, CurrentLap: 3})
	want(t, c, ok, KindTemplateQuit, nil, i32(120314), i32(2))
	if _, again := r.Ended(End{Kind: soloTimed, CurrentLap: 3}); again {
		t.Error("the quit summary was said twice")
	}

	r.Reset()
	c, ok = r.Ended(End{Kind: soloLaps, CurrentLap: 8, GT7BestMs: 49677})
	want(t, c, ok, KindTemplateQuit, nil, i32(49677), i32(7))

	r.Reset()
	if _, ok := r.Ended(End{Kind: lapRace, CurrentLap: 5}); ok {
		t.Error("a quit summary for a race")
	}
	if _, ok := r.Ended(End{Kind: soloLaps, CurrentLap: 1}); ok {
		t.Error("a quit summary with no lap done")
	}
	if _, ok := r.Ended(End{Kind: eventsv1.SessionType_SESSION_TYPE_UNKNOWN, CurrentLap: 5}); ok {
		t.Error("a quit summary for a session of no kind")
	}
}

// goldenCall is a callout line of testdata/race_summary/*.jsonl (make_golden.py).
type goldenCall struct {
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

func readGolden(t *testing.T, name string) []goldenCall {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "race_summary", name+".jsonl")) //nolint:gosec // a golden of this package
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var calls []goldenCall
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c goldenCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		if c.Event == "callout" {
			calls = append(calls, c)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return calls
}

// AC1–AC6 as v1 decided them, in the goldens the engine's test holds ecu to
// (paddock#89): each callout's frame, kind and slots, priority 25, 30 s, no hold,
// spoken.
func TestTheGoldensHoldTheAcceptanceCriteria(t *testing.T) {
	type call struct {
		session, at, asOf int
		kind              string
		slots             map[string]int32
	}
	for name, calls := range map[string][]call{
		// AC1: the practice before the race, then the race.
		"real-race-2laps-2026-10-14": {
			{1, 75, 75, KindTemplateQuit, map[string]int32{"laps": 7, "best_lap_ms": 49677}},
			{2, 263, 263, KindTemplatePosition, map[string]int32{"position": 6, "best_lap_ms": 53108}},
		},
		// AC2.
		"real-nuc-suzuka-pit-stop-2026-10-15": {
			{1, 843, 843, KindTemplatePositionWin, map[string]int32{"position": 1, "best_lap_ms": 59541}},
		},
		// AC3: the timed finish, raised at 752 as of the line crossing at 745.
		"real-nuc-suzuka-timed-race-2026-10-16": {
			{1, 752, 745, KindTemplatePositionWin, map[string]int32{"position": 1, "best_lap_ms": 67585}},
		},
		// AC4: leaving through the menu, no position.
		"real-nuc-fuji-timed-2026-10-15": {
			{1, 633, 633, KindTemplateQuit, map[string]int32{"laps": 2, "best_lap_ms": 120314}},
		},
		// AC5: the restarted races get no quit summary; the last one finishes.
		"real-nuc-sugo-restarts-2026-10-18": {
			{3, 1151, 1151, KindTemplatePositionPodium, map[string]int32{"position": 2, "best_lap_ms": 83610}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := readGolden(t, name)
			if len(got) != len(calls) {
				t.Fatalf("got %d callouts, want %d: %+v", len(got), len(calls), got)
			}
			for i, w := range calls {
				g := got[i]
				if g.Session != w.session || g.At != w.at || g.AsOf != w.asOf || g.Kind != w.kind || len(g.Slots) != len(w.slots) {
					t.Errorf("got %+v, want %+v", g, w)
				}
				for k, v := range w.slots {
					if g.Slots[k] != v {
						t.Errorf("slot %s: got %d, want %d", k, g.Slots[k], v)
					}
				}
				// AC6, R6.
				if g.Priority != 25 || g.MaxAge != 30 || g.Hold != 0 || g.PitLane != "speak" || g.Outcome != "spoken" {
					t.Errorf("got priority %d, max age %g, hold %g, pit lane %s, %s", g.Priority, g.MaxAge, g.Hold, g.PitLane, g.Outcome)
				}
			}
		})
	}
}
