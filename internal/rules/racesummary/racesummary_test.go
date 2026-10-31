package racesummary

import (
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/ecu/internal/track"
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

// field is a race with a field.
var field = race.Kind{Type: lapRace, Field: true}

// post is a racing frame that posts lap's time.
func post(lap, ms int32, startLap bool) *race.Context {
	return &race.Context{Racing: true, Posted: &race.PostedLap{Lap: lap, TimeMs: ms, StartLap: startLap}}
}

// finish is the frame a race finished on, as of at.
func finish(kind race.Kind, at race.View) *race.Context {
	return &race.Context{Racing: true, Kind: kind, Cur: at, Finish: &at, Finished: true}
}

// end is a session of kind ending without a finish, last its last driving frame.
func end(kind eventsv1.SessionType, last race.View) *race.Context {
	return &race.Context{Kind: race.Kind{Type: kind}, EndedEarly: &last}
}

// one checks the rule raised one callout, of kind with these position,
// best_lap_ms and laps slots.
func one(t *testing.T, got []rules.Callout, kind string, position, best, laps *int32) rules.Callout {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("got %d callouts, want one %s", len(got), kind)
	}
	c := got[0]
	if c.Kind != kind || c.Priority != 0 {
		t.Errorf("got %s at priority %d, want %s at the rule's", c.Kind, c.Priority, kind)
	}
	s := c.Slots
	if !eqInt(s.Position, position) || !eqInt(s.BestLapMs, best) || !eqInt(s.Laps, laps) {
		t.Errorf("slots position %v best %v laps %v, want %v %v %v",
			show(s.Position), show(s.BestLapMs), show(s.Laps), show(position), show(best), show(laps))
	}
	return c
}

// R6: the rule's own settings, v1's defaults, and the ones the rules file sets.
func TestTheSettings(t *testing.T) {
	r := New(Default())
	if len(r.quitKinds) != 2 || r.quitKinds[0] != soloTimed || r.quitKinds[1] != soloLaps || r.quitMinLaps != 1 {
		t.Errorf("got quit kinds %v after %d laps", r.quitKinds, r.quitMinLaps)
	}
	cfg, err := rules.ParseConfig([]byte("rules:\n  race_summary:\n    quit_kinds: [lap_race, no_such_kind]\n    quit_min_laps: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := rules.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Rules(); len(got) != 1 || got[0] != Name {
		t.Errorf("the engine runs %v", got)
	}
	r = New(Config{QuitKinds: []string{"lap_race", "no_such_kind"}, QuitMinLaps: 3})
	if len(r.quitKinds) != 1 || r.quitKinds[0] != lapRace || r.quitMinLaps != 3 {
		t.Errorf("got quit kinds %v after %d laps", r.quitKinds, r.quitMinLaps)
	}
	r.Reset()
	if len(r.quitKinds) != 1 || r.quitMinLaps != 3 {
		t.Error("a reset forgot the settings")
	}
}

// R1, R2: the finish is said once, with the position off the frame it finished
// as of, in the pool its place picks, created as of that frame.
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
		r := New(Default())
		at := race.View{Seq: 745, Position: tc.position, BestLapMs: 53108}
		c := one(t, r.Evaluate(finish(field, at)), tc.kind, i32(tc.position), i32(53108), nil)
		if c.AsOf == nil || c.AsOf.Seq != 745 {
			t.Errorf("created as of %v, want frame 745", c.AsOf)
		}
		if got := r.Evaluate(finish(field, at)); len(got) != 0 {
			t.Error("the finish was said twice")
		}
		if got := r.Evaluate(end(soloLaps, race.View{Lap: 5})); len(got) != 0 {
			t.Error("a quit summary after the finish")
		}
	}
}

// R2: no field, or no position: the template, with the best alone.
func TestTheFinishWithoutAFieldSaysNoPlace(t *testing.T) {
	r := New(Default())
	one(t, r.Evaluate(finish(race.Kind{Type: lapRace}, race.View{Position: 1, BestLapMs: 82190})),
		KindTemplate, nil, i32(82190), nil)

	r.Reset()
	// Suzuka East: "Chequered flag." and nothing more.
	one(t, r.Evaluate(finish(field, race.View{Position: -1})), KindTemplate, nil, nil, nil)
}

// R3: GT7's raw best on the frame when it has one, even one quicker than every
// lap seen; else the quickest lap posted that counts, leaving out a start lap;
// else any lap. Each lap counts once, on its first post, and only while racing.
func TestTheBestLap(t *testing.T) {
	r := New(Default())
	laps := func() {
		r.Evaluate(post(1, 50000, true))
		r.Evaluate(post(2, 54000, false))
		r.Evaluate(post(2, 52000, false)) // a second post for lap 2
		r.Evaluate(post(3, 53000, false))
		r.Evaluate(post(4, 0, false)) // no time
		r.Evaluate(&race.Context{Posted: &race.PostedLap{Lap: 5, TimeMs: 40000}})
	}
	laps()
	one(t, r.Evaluate(finish(field, race.View{Position: 6, BestLapMs: 49000})),
		KindTemplatePosition, i32(6), i32(49000), nil)

	r.Reset()
	laps()
	one(t, r.Evaluate(finish(field, race.View{Position: 6})), KindTemplatePosition, i32(6), i32(53000), nil)

	r.Reset()
	r.Evaluate(post(1, 50000, true))
	one(t, r.Evaluate(finish(race.Kind{}, race.View{})), KindTemplate, nil, i32(50000), nil)
}

// R5, AC4, AC5: leaving a solo session without a finish gives template_quit,
// laps counted from GT7's counter too; never for a race, under quit_min_laps, or
// after the finish.
func TestLeavingASoloSession(t *testing.T) {
	r := New(Default())
	r.Evaluate(post(2, 120314, false)) // joined too late to see lap 1
	one(t, r.Evaluate(end(soloTimed, race.View{Lap: 3})), KindTemplateQuit, nil, i32(120314), i32(2))
	if got := r.Evaluate(end(soloTimed, race.View{Lap: 3})); len(got) != 0 {
		t.Error("the quit summary was said twice")
	}

	r.Reset()
	one(t, r.Evaluate(end(soloLaps, race.View{Lap: 8, BestLapMs: 49677})), KindTemplateQuit, nil, i32(49677), i32(7))

	r.Reset()
	for _, ctx := range []*race.Context{
		end(lapRace, race.View{Lap: 5}),
		end(soloLaps, race.View{Lap: 1}),
		end(eventsv1.SessionType_SESSION_TYPE_UNKNOWN, race.View{Lap: 5}),
		{Kind: race.Kind{Type: soloLaps}, EndedEarly: &race.View{Lap: 5}, Finished: true},
		{Racing: true},
	} {
		if got := r.Evaluate(ctx); len(got) != 0 {
			t.Errorf("a quit summary on %+v", ctx)
		}
	}
}

// The decision of 2026-10-31, a departure from v1: the finish and the quit
// summary name the track when the session's verdict is confirmed ("Race over at
// Sportsland SUGO."), and none when it is only probable, a circuit whose layout
// isn't picked, ambiguous, or not there.
func TestTheSummaryNamesASureTrack(t *testing.T) {
	sugo := track.Match{TrackID: 9, Name: "Sportsland SUGO", Confidence: track.Confirmed}
	probable := sugo
	probable.Confidence = track.Probable
	fuji := track.Match{TrackID: 100003, Name: "Fuji International Speedway", Family: "Fuji International Speedway",
		Confidence: track.Probable}
	ambiguous := track.Match{Confidence: track.Ambiguous}
	for _, tc := range []struct {
		name string
		m    *track.Match
		want string
	}{
		{"confirmed", &sugo, "Sportsland SUGO"},
		{"probable", &probable, ""},
		{"a circuit", &fuji, ""},
		{"ambiguous", &ambiguous, ""},
		{"none", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(Default())
			ctx := finish(field, race.View{Position: 1})
			ctx.Track = tc.m
			if got := one(t, r.Evaluate(ctx), KindTemplatePositionWin, i32(1), nil, nil).Slots.GetTrack(); got != tc.want {
				t.Errorf("at the finish, track %q, want %q", got, tc.want)
			}
			r.Reset()
			ctx = end(soloLaps, race.View{Lap: 4})
			ctx.Track = tc.m
			if got := one(t, r.Evaluate(ctx), KindTemplateQuit, nil, nil, i32(3)).Slots.GetTrack(); got != tc.want {
				t.Errorf("leaving, track %q, want %q", got, tc.want)
			}
		})
	}
}
