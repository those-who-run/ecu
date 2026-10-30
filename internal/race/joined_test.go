package race

import (
	"slices"
	"strings"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// joinedRun is what a session joined part way gives: its start, its kind's
// verdicts, and its track's, with the seq of the first frame on a lap after the
// one it was joined on, the first line crossing.
type joinedRun struct {
	started  *eventsv1.SessionStarted
	kinds    []*eventsv1.SessionKind
	tracks   []*eventsv1.SessionTrackChanged
	crossing uint32
}

func replayJoined(t *testing.T, name string) joinedRun {
	t.Helper()
	var r joinedRun
	for _, o := range replay(t, name) {
		switch {
		case o.Frame != nil:
			if r.started != nil && r.crossing == 0 && o.Frame.GetLap() > r.started.GetJoinedOnLap() {
				r.crossing = o.Frame.GetSeq()
			}
		case o.Started != nil:
			if r.started != nil {
				t.Fatalf("%s: a second session", name)
			}
			r.started = o.Started
		case o.Kind != nil:
			r.kinds = append(r.kinds, o.Kind.GetKind())
		case o.Track != nil:
			r.tracks = append(r.tracks, o.Track)
		}
	}
	if r.started == nil || r.crossing == 0 {
		t.Fatalf("%s: no session, or no line crossed", name)
	}
	return r
}

// noTrackBefore says no verdict published before the first line crossing names a
// track (R3): v1 publishes its ambiguous verdict, which names none.
func (r joinedRun) noTrackBefore(t *testing.T, name string) {
	t.Helper()
	for _, tr := range r.tracks {
		if tr.GetAt() < r.crossing && tr.GetTrack().GetName() != "" {
			t.Errorf("%s: frame %d names %q before the line, crossed on frame %d",
				name, tr.GetAt(), tr.GetTrack().GetName(), r.crossing)
		}
	}
}

// AC1: Suzuka, joined on lap 7 of a timed race: session.started says joined on
// lap 7, every session.kind has the start unknown, and no track is named until
// the line is crossed, when it is Suzuka Circuit: Lago Maggiore is never named as
// the track. AC1 is read as the named track only, as v1 has it: frame 1's
// ambiguous verdict, which names none, lists four Lago Maggiore layouts among its
// candidates (790 East End Reverse, 795 East End, 1231 East Reverse, 1226 East),
// v1's own candidates, pinned below so the gap shows; leaving them out would be a
// departure from v1 for an issue of its own.
func TestSuzukaJoinedOnLap7(t *testing.T) {
	const name = "real-nuc-suzuka-joined-mid-race-2026-10-18"
	r := replayJoined(t, name)
	if !r.started.GetJoinedPartWay() || r.started.JoinedOnLap == nil || r.started.GetJoinedOnLap() != 7 {
		t.Fatalf("joined %t on lap %v, want joined on lap 7", r.started.GetJoinedPartWay(), r.started.JoinedOnLap)
	}
	if len(r.kinds) == 0 {
		t.Fatal("no session.kind")
	}
	for _, k := range r.kinds {
		if k.GetStart() != eventsv1.StartType_START_TYPE_UNKNOWN {
			t.Errorf("session.kind's start %s, want unknown", k.GetStart())
		}
	}
	r.noTrackBefore(t, name)
	if len(r.tracks) == 0 || r.tracks[0].GetAt() != 1 {
		t.Fatal("no verdict on frame 1")
	}
	var cands []int32
	for _, c := range r.tracks[0].GetTrack().GetCandidates() {
		cands = append(cands, c.GetTrackId())
	}
	if want := []int32{790, 846, 442, 795, 1231, 1226, 356, 449}; !slices.Equal(cands, want) {
		t.Errorf("frame 1's candidates %v, want v1's %v", cands, want)
	}
	named := false
	for _, tr := range r.tracks {
		got := tr.GetTrack().GetName()
		if strings.Contains(got, "Lago Maggiore") {
			t.Errorf("frame %d names %q", tr.GetAt(), got)
		}
		if got != "" {
			named = true
			if got != "Suzuka Circuit" {
				t.Errorf("frame %d names %q, want Suzuka Circuit", tr.GetAt(), got)
			}
		}
	}
	if !named {
		t.Error("Suzuka Circuit never named")
	}
}

// AC2: Willow Springs, cut mid-lap from lap 6: session.started says joined on lap
// 6, and no track is named before the line (v1 names Big Willow once the car
// crosses it, testdata/track's golden).
func TestWillowSpringsCutMidLapIsJoined(t *testing.T) {
	const name = "real-nuc-willow-springs-pit-stop-2026-10-17"
	r := replayJoined(t, name)
	if !r.started.GetJoinedPartWay() || r.started.GetJoinedOnLap() != 6 {
		t.Fatalf("joined %t on lap %v, want joined on lap 6", r.started.GetJoinedPartWay(), r.started.JoinedOnLap)
	}
	r.noTrackBefore(t, name)
}

// joined_on_lap is set only for a session joined part way (contracts v0.2.3).
func TestASessionSeenFromItsStartHasNoJoinedOnLap(t *testing.T) {
	for _, o := range replay(t, "real-race-2laps-2026-10-14") {
		if s := o.Started; s != nil && !s.GetJoinedPartWay() && s.JoinedOnLap != nil {
			t.Errorf("session %s: joined_on_lap %d, not joined", s.GetSessionId(), s.GetJoinedOnLap())
		}
	}
}
