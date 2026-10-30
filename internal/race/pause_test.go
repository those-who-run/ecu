package race

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
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/salsa"
)

// pauseLine is a line of a pause golden (testdata/pause/make_golden.py): a session
// opened, paused, resumed, or ended with what it stored about its pauses.
type pauseLine struct {
	Event       string `json:"event"`
	Session     int    `json:"session"`
	Frame       uint32 `json:"frame"`
	Kind        string `json:"kind"`
	SinceFrame  uint32 `json:"since_frame"`
	PauseMs     int32  `json:"pause_ms"`
	PausedMs    int32  `json:"paused_ms"`
	EndedPaused bool   `json:"ended_paused"`
}

func (l pauseLine) String() string {
	switch l.Event {
	case "session.started":
		return fmt.Sprintf("session.started %d frame %d", l.Session, l.Frame)
	case "session.paused":
		return fmt.Sprintf("session.paused %d frame %d %s since frame %d, %d ms paused before",
			l.Session, l.Frame, l.Kind, l.SinceFrame, l.PausedMs)
	case "session.resumed":
		return fmt.Sprintf("session.resumed %d frame %d after %d ms, %d ms paused", l.Session, l.Frame, l.PauseMs, l.PausedMs)
	default:
		return fmt.Sprintf("session.ended %d %d ms paused, ended paused %t", l.Session, l.PausedMs, l.EndedPaused)
	}
}

func readPauseGolden(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "pause", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.Lines(string(data)) {
		var l pauseLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l.String())
	}
	return lines
}

var pauseKinds = map[eventsv1.PauseKind]string{
	eventsv1.PauseKind_PAUSE_KIND_FLAGGED: "flagged",
	eventsv1.PauseKind_PAUSE_KIND_FROZEN:  "frozen",
}

// pauseLines are the outputs as the pause golden's lines. A pause's since_frame
// is the frame of its since_t: the first frame at that time, counting back from
// the frame it was read on.
func pauseLines(out []Output) []string {
	all := frames(out)
	sinceFrame := func(at uint32, t float64) uint32 {
		seq := at
		for seq > 1 && all[seq-1].GetT() >= t {
			seq--
		}
		return seq
	}
	sessions := map[string]int{}
	var lines []string
	for _, o := range out {
		var l pauseLine
		switch {
		case o.Started != nil:
			sessions[o.Session] = len(sessions) + 1
			l = pauseLine{Event: "session.started", Frame: o.Started.GetFirstSeq()}
		case o.Paused != nil:
			p := o.Paused
			l = pauseLine{
				Event: "session.paused", Frame: p.GetAt(), Kind: pauseKinds[p.GetKind()],
				SinceFrame: sinceFrame(p.GetAt(), p.GetSinceT()), PausedMs: p.GetPausedMs(),
			}
		case o.Resumed != nil:
			r := o.Resumed
			l = pauseLine{Event: "session.resumed", Frame: r.GetAt(), PauseMs: r.GetPauseMs(), PausedMs: r.GetPausedMs()}
		case o.Ended != nil:
			s := o.Ended.GetSummary()
			l = pauseLine{Event: "session.ended", PausedMs: s.GetPausedMs(), EndedPaused: s.GetEndedPaused()}
		default:
			continue
		}
		l.Session = sessions[o.Session]
		lines = append(lines, l.String())
	}
	return lines
}

// R1–R3, AC6: on every one of v1's recordings, ecu reads the pauses v1's
// PauseClock read in an open session, flagged or frozen, on the same frames,
// dated from the same frame, and publishes session.paused where it began and
// session.resumed on the frame after it, with v1's times; each session's summary
// has v1's paused_ms and ended_paused. As for the finish, the recordings v1 opens
// no session on are held to v1 taking every start for a drive (--drive), as ecu
// does until paddock#74.
func TestThePausesAreV1sOnEveryRecording(t *testing.T) {
	var pausesSeen int
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			want := readPauseGolden(t, name)
			got := pauseLines(replay(t, name))
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			for _, l := range want {
				if strings.HasPrefix(l, "session.paused") {
					pausesSeen++
				}
			}
		})
	}
	if pausesSeen == 0 {
		t.Error("no pause in any golden")
	}
}

// AC2, as the maintainer reworded it (2026-10-30): the live race of 2026-10-29,
// lap 6 of its session 2, cut to testdata/pause's fixture. GT7 flags a pause for
// 1.3 s, then sends a frozen stream for 2.9 s, from the first frame after the
// flagged ones, until the driver quits to the menu: two pauses, flagged then
// frozen, each with session.paused and session.resumed, and v1's 1,325 ms,
// 2,913 ms and 4,238 ms paused, in the events and the summary; nothing else
// against v1 (the golden, from v1's engine on the fixture).
func TestAFlaggedThenFrozenPauseBeforeAQuit(t *testing.T) {
	const name = "real-nuc-short-shift-pauses-2026-10-29"
	out := replay(t, name)
	if got, want := pauseLines(out), readPauseGolden(t, name); !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var kinds []eventsv1.PauseKind
	var lengths []int32
	var ended *eventsv1.SessionEnded
	for _, o := range out {
		switch {
		case o.Paused != nil:
			kinds = append(kinds, o.Paused.GetKind())
		case o.Resumed != nil:
			lengths = append(lengths, o.Resumed.GetPauseMs())
		case o.Ended != nil:
			ended = o.Ended
		}
	}
	if !slices.Equal(kinds, []eventsv1.PauseKind{eventsv1.PauseKind_PAUSE_KIND_FLAGGED, eventsv1.PauseKind_PAUSE_KIND_FROZEN}) ||
		!slices.Equal(lengths, []int32{1325, 2913}) {
		t.Errorf("pauses %v of %v ms, want flagged then frozen, 1325 and 2913", kinds, lengths)
	}
	if s := ended.GetSummary(); ended.GetReason() != eventsv1.EndReason_END_REASON_MENU || s.GetPausedMs() != 4238 || s.GetEndedPaused() {
		t.Errorf("ended %v", ended)
	}
}

// AC1: SUGO's 45 s frozen stream on lap 2 of the first race goes out paused,
// every frame of it, as part of one pause: GT7 flags the first 1.2 s, then
// freezes, and v1's PauseClock reads it all as the one pause, so it is flagged
// (v1 cannot tell it began any other way). The restart through a loading frame
// at lap 0 ends it, and the session; no frame of it is in a lap.
func TestTheFrozenPauseIsInLapTwoOfTheFirstSugoRace(t *testing.T) {
	all := packets(t, "real-nuc-sugo-restarts-2026-10-18")
	skip := frozen(all)
	if len(skip) != 96 {
		t.Errorf("%d frozen frames, want 96", len(skip))
	}
	out := run(all)
	byseq := frames(out)
	var pauses []*eventsv1.SessionPaused
	var resumed *eventsv1.SessionResumed
	var ended *eventsv1.SessionEnded
	for _, o := range out {
		switch {
		case o.Paused != nil && ended == nil:
			pauses = append(pauses, o.Paused)
		case o.Resumed != nil && ended == nil:
			resumed = o.Resumed
		case o.Ended != nil && ended == nil:
			ended = o.Ended
		case o.Lap != nil:
			for seq := o.Lap.GetFirstSeq(); seq <= o.Lap.GetLastSeq(); seq++ {
				if skip[seq] && byseq[seq].GetPhase() != phasePaused {
					t.Errorf("frame %d is in lap %d driving", seq, o.Lap.GetLap())
				}
			}
		}
	}
	if len(pauses) != 2 || resumed == nil || ended == nil {
		t.Fatalf("%d pauses, resumed %v, ended %v: want two pauses, the second resumed, then the end", len(pauses), resumed, ended)
	}
	// One frozen frame ends the first pause, flagged; the other 95 are the second.
	p := pauses[1]
	var inPause int
	for seq := range skip {
		if byseq[seq].GetPhase() != phasePaused {
			t.Errorf("frozen frame %d goes out %v", seq, byseq[seq].GetPhase())
		}
		if seq >= p.GetAt() && seq < resumed.GetAt() {
			inPause++
		}
	}
	if inPause != 95 {
		t.Errorf("%d frozen frames in the pause on lap 2, want 95", inPause)
	}
	if p.GetKind() != eventsv1.PauseKind_PAUSE_KIND_FLAGGED || p.GetPause() != 2 || byseq[p.GetAt()].GetLap() != 2 ||
		resumed.GetPause() != 2 || resumed.GetPauseMs() != 46463 {
		t.Errorf("pause %v, resumed %v", p, resumed)
	}
	if ended.GetReason() != eventsv1.EndReason_END_REASON_QUIT || ended.GetSummary().GetEndedPaused() ||
		ended.GetSummary().GetPausedMs() != resumed.GetPausedMs() || ended.GetLastSeq() != resumed.GetAt() {
		t.Errorf("ended %v", ended)
	}
}

// AC3: qualifying paused three times mid-lap, twice in the first session and once
// in the second: each a pause and a resume, with no start and no end between.
func TestThreePausesMidLapAreNeitherAStartNorAnEnd(t *testing.T) {
	var open bool
	var pairs int
	for _, o := range replay(t, "real-quali-restart-quit-2026-10-14") {
		switch {
		case o.Paused != nil:
			open = true
		case o.Resumed != nil:
			if !open {
				t.Error("resumed with no pause")
			}
			open = false
			pairs++
		case (o.Started != nil || o.Ended != nil) && open:
			t.Errorf("a start or an end in a pause: %v", o.Message())
		}
	}
	if pairs != 3 {
		t.Errorf("%d pauses resumed, want 3", pairs)
	}
}

// paused is a lap race driven 10 s, from the grid at frame 1, then paused (GT7's
// flag) for secs seconds from frame 12: a frame a second, as drive gives them.
func paused(secs int) *drive {
	var d drive
	d.add(0, 0, 50, -1, 3) // 1: the grid
	for range 10 {
		d.add(1, 0, 50, -1, 3) // 2–11: lap 1
	}
	for range secs {
		d.add(1, salsa.FlagPaused, 50, -1, 3)
	}
	return &d
}

// endedPaused is the session.ended of the outputs; it fails unless there is
// exactly one.
func endedPaused(t *testing.T, out []Output) *eventsv1.SessionEnded {
	t.Helper()
	var ends []*eventsv1.SessionEnded
	for _, o := range out {
		if o.Ended != nil {
			ends = append(ends, o.Ended)
		}
	}
	if len(ends) != 1 {
		t.Fatalf("%d session.ended, want 1", len(ends))
	}
	return ends[0]
}

// AC4, R5: the feed lost during a pause, flagged or frozen, ends the session
// paused, on the last frame, as of the pause's start (the watchdog's Tick, live
// only, paddock#87). The frames after it are ignored until the next start: only
// the frame goes out, with telemetry.restored on the first, which the context
// keeps for the rules that watch the feed. Lost while driving, the session stays
// open.
func TestTheFeedLostInAPauseEndsTheSessionPaused(t *testing.T) {
	frozenPause := paused(0)
	for range 5 {
		frozenPause.add(1, 0, 50, -1, 3) // 12–16: the same frame at speed, read paused from 13
		frozenPause.all[len(frozenPause.all)-1].Speed = 30
	}
	for name, c := range map[string]struct {
		d        *drive
		pausedMs int32
	}{"flagged": {paused(5), 4000}, "frozen": {frozenPause, 4000}} {
		m := New(testIDs())
		m.WatchTelemetry(3 * time.Second)
		for _, r := range c.d.all[:11] {
			m.Step(r)
		}
		if out := m.Tick(c.d.all[10].Time.Add(3 * time.Second)); endsOf(out) != 0 {
			t.Errorf("%s: feed lost while driving: %v, want the session open", name, out)
		}
		m.Step(Received{Received: c.d.all[11].Received, Time: c.d.all[10].Time.Add(4 * time.Second)})
		for _, r := range c.d.all[12:] {
			r.Time = r.Time.Add(3 * time.Second)
			m.Step(r)
		}
		last := c.d.all[len(c.d.all)-1].Time.Add(3 * time.Second)
		lost := m.Tick(last.Add(3 * time.Second))
		end := endedPaused(t, lost)
		s := end.GetSummary()
		if end.GetReason() != eventsv1.EndReason_END_REASON_TELEMETRY_LOST || !s.GetEndedPaused() ||
			end.GetLastSeq() != 16 || s.GetPausedMs() != c.pausedMs {
			t.Errorf("%s: ended %v", name, end)
		}
		if lost[len(lost)-1].Why != WhyTelemetryLost {
			t.Errorf("%s: why %v", name, lost[len(lost)-1].Why)
		}
		// The race carries on after it: not watched, until a new start.
		var after drive
		after.add(1, 0, 50, -1, 3)
		after.add(2, 0, 49, 61000, 3)
		after.add(0, 0, 50, -1, 3) // the track screen
		after.add(1, 0, 50, -1, 3) // a new start
		for i, r := range after.all {
			r.T += 30
			r.Time = last.Add(time.Duration(10+i) * time.Second)
			got := m.Step(r)
			ctx := m.Context()
			want := 1
			if i == 0 {
				want = 2 // and telemetry.restored
			}
			if i < 3 && (len(got) != want || got[0].Frame == nil || !ctx.Ignored || (i == 0) != (ctx.Restored != nil)) {
				t.Errorf("%s: frame %d after the end: %d outputs, ignored %t, restored %v; want only the frame, ignored",
					name, i+1, len(got), ctx.Ignored, ctx.Restored)
			}
			if i == 3 && (len(got) < 2 || got[1].Started == nil || ctx.Ignored) {
				t.Errorf("%s: the new start: %v", name, got)
			}
		}
	}
}

func endsOf(out []Output) int {
	var n int
	for _, o := range out {
		if o.Ended != nil {
			n++
		}
	}
	return n
}

// AC5, R6: with the pause limit at 60 s, a pause of 61 s ends the session paused
// on the frame the limit passes, as of the pause's start; a pause of 59 s, or any
// pause with no limit (the default), does not.
func TestAPauseThatLastsTheLimitEndsTheSessionPaused(t *testing.T) {
	limited := func(secs int, limit float64) []Output {
		m := New(testIDs())
		m.SetPauseLimit(limit)
		var out []Output
		for _, r := range paused(secs).all {
			out = append(out, m.Step(r)...)
		}
		return out
	}
	out := limited(61, 60)
	end := endedPaused(t, out)
	// The pause began on frame 12 (t 11 s); 60 s of it is frame 72 (t 71 s).
	if end.GetReason() != eventsv1.EndReason_END_REASON_PAUSE_LIMIT || !end.GetSummary().GetEndedPaused() ||
		end.GetLastSeq() != 72 || end.GetSummary().GetPausedMs() != 60000 {
		t.Errorf("ended %v", end)
	}
	for _, c := range []struct {
		secs  int
		limit float64
	}{{59, 60}, {61, 0}} {
		for _, o := range limited(c.secs, c.limit) {
			if o.Ended != nil {
				t.Errorf("%d s paused, limit %v: %v", c.secs, c.limit, o.Ended)
			}
		}
	}
}

// R5: the frames stopping in a pause, at the end of the input or on ecu's stop,
// end the session paused; out of a pause they end it as before.
func TestTheFramesStoppingInAPauseEndTheSessionPaused(t *testing.T) {
	for _, c := range []struct {
		name   string
		close  func(*Machine) []Output
		reason eventsv1.EndReason
	}{
		{"input ended", (*Machine).EndOfInput, eventsv1.EndReason_END_REASON_INPUT_ENDED},
		{"stopped", (*Machine).Stop, eventsv1.EndReason_END_REASON_ENGINEER_STOPPED},
	} {
		m := New(testIDs())
		for _, r := range paused(3).all {
			m.Step(r)
		}
		end := endedPaused(t, c.close(m))
		if end.GetReason() != c.reason || !end.GetSummary().GetEndedPaused() || end.GetSummary().GetPausedMs() != 2000 {
			t.Errorf("%s: %v", c.name, end)
		}
	}
	m := New(testIDs())
	for _, r := range paused(0).all {
		m.Step(r)
	}
	if end := endedPaused(t, m.EndOfInput()); end.GetSummary().GetEndedPaused() {
		t.Errorf("ended paused out of a pause: %v", end)
	}
}

// R1, R2: identical frames with the car moving are a pause from the first of them,
// read 1 s on, and go out paused; the next frame that moves resumes it, with the
// pause's ids numbered from 1 in the session. At a standstill they are not.
func TestIdenticalFramesAtSpeedAreAFrozenPause(t *testing.T) {
	var d drive
	d.add(0, 0, 50, -1, 3) // 1: the grid
	for i := range 6 {
		d.add(1, 0, 50, -1, 3) // 2–7: lap 1, frames 4 to 6 identical
		p := &d.all[len(d.all)-1].Packet
		p.Speed = 30
		p.Position.X = float32(min(i, 2))
	}
	d.add(1, 0, 50, -1, 3) // 8: moving again
	d.all[7].Speed, d.all[7].Position.X = 30, 10
	out := run(d.all)
	byseq := frames(out)
	for seq, want := range map[uint32]telemetryv1.Phase{3: phaseInRace, 4: phaseInRace, 5: phasePaused, 7: phasePaused, 8: phaseInRace} {
		if got := byseq[seq].GetPhase(); got != want {
			t.Errorf("frame %d: %v, want %v", seq, got, want)
		}
	}
	var p *eventsv1.SessionPaused
	var r *eventsv1.SessionResumed
	var ids []string
	for _, o := range out {
		if o.Paused != nil {
			p = o.Paused
			ids = append(ids, o.Event.ID)
		}
		if o.Resumed != nil {
			r = o.Resumed
			ids = append(ids, o.Event.ID)
		}
	}
	if p == nil || r == nil {
		t.Fatalf("paused %v, resumed %v", p, r)
	}
	if p.GetKind() != eventsv1.PauseKind_PAUSE_KIND_FROZEN || p.GetAt() != 5 || p.GetSinceT() != 3 ||
		!p.GetSince().AsTime().Equal(d.all[3].Time) || p.GetPausedMs() != 0 || p.GetPause() != 1 {
		t.Errorf("paused %v", p)
	}
	if r.GetAt() != 8 || r.GetPauseMs() != 4000 || r.GetPausedMs() != 4000 || r.GetKind() != p.GetKind() {
		t.Errorf("resumed %v", r)
	}
	sid := p.GetSessionId()
	if want := []string{sid + ":session.paused:1", sid + ":session.resumed:1"}; !slices.Equal(ids, want) {
		t.Errorf("ids %v, want %v", ids, want)
	}

	// The same at a standstill: a car on the grid or in its box.
	for i := range d.all {
		d.all[i].Speed = 0
	}
	for _, o := range run(d.all) {
		if o.Paused != nil || (o.Frame != nil && o.Frame.GetPhase() == phasePaused) {
			t.Errorf("a pause at a standstill: %v", o.Message())
		}
	}
}
