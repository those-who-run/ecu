package race

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Suzuka's restart pair (paddock#76): the old pod's view of a timed race, laps 1
// to 6 and 20 s of lap 7, with a 4 min pause on lap 3 and a stop on lap 4, then
// the new pod's from 8.7 s later.
const (
	beforeRestart = "real-nuc-suzuka-before-restart-2026-10-18"
	joinedMidRace = "real-nuc-suzuka-joined-mid-race-2026-10-18"
	restartGap    = 8700 * time.Millisecond
	window        = 600 * time.Second
)

// pairGolden is a line of testdata/continue/restart-pair.jsonl (its
// make_golden.py): what v1 let go of in one scenario, in order.
type pairGolden struct {
	Scenario string      `json:"scenario"`
	Ended    []v1Summary `json:"ended"`
	Raw      []struct {
		Gaps             []v1Gap `json:"gaps"`
		EngineerShutdown bool    `json:"engineer_shutdown"`
		Track            string  `json:"track"`
	} `json:"-"`
}

type v1Gap struct {
	From float64 `json:"from"`
	To   float64 `json:"to"`
	Lap  int32   `json:"lap"`
}

func readPairGolden(t *testing.T, scenario string) pairGolden {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "continue", "restart-pair.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var g pairGolden
		if err := json.Unmarshal(sc.Bytes(), &g); err != nil {
			t.Fatal(err)
		}
		var raw struct {
			Ended []struct {
				Gaps             []v1Gap `json:"gaps"`
				EngineerShutdown bool    `json:"engineer_shutdown"`
				Track            string  `json:"track"`
			} `json:"ended"`
		}
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		g.Raw = raw.Ended
		if g.Scenario == scenario {
			return g
		}
	}
	t.Fatalf("no scenario %q in the golden", scenario)
	return pairGolden{}
}

// pair is what the two runs of a restart pair gave.
type pair struct {
	first  []Output             // the first run's, up to its graceful stop
	record *statev1.OpenSession // what it left in the KV
	second []Output             // the second run's, its timer's included
}

// restartPair runs the old pod's recording through a machine, stops it as a
// graceful shutdown does (its record, no end), then a new machine, given the
// record as the KV holds it, through the new pod's from gap after the old one's
// last frame, its timer firing each second until the first frame, and to the
// end of the input. car, when not 0, is the new pod's car code.
func restartPair(t *testing.T, gap, window time.Duration, car int32) pair {
	t.Helper()
	var p pair
	a := New(testIDs())
	for _, r := range packets(t, beforeRestart) {
		p.first = append(p.first, a.Step(r)...)
	}
	open := a.Open()
	if open == nil {
		t.Fatal("no session open at the stop")
	}
	// Through JSON, as the KV holds it.
	data, err := ce.Marshal(open)
	if err != nil {
		t.Fatal(err)
	}
	p.record = &statev1.OpenSession{}
	if err := ce.Unmarshal(data, p.record); err != nil {
		t.Fatal(err)
	}
	b := New(NewIDs(func() time.Time { return epoch.Add(time.Hour) }, rand.NewChaCha8([32]byte{'t', 'w', 'o'})))
	b.Restore(p.record, window)
	from := p.record.GetLastSeenAt().AsTime().Add(gap)
	joined := packets(t, joinedMidRace)
	for tick := p.record.GetLastSeenAt().AsTime().Add(time.Second); tick.Before(from); tick = tick.Add(time.Second) {
		p.second = append(p.second, b.Expire(tick)...)
	}
	for _, r := range joined {
		r.Time = from.Add(r.Time.Sub(epoch))
		if car != 0 {
			r.CarCode = car
		}
		p.second = append(p.second, b.Step(r)...)
	}
	p.second = append(p.second, b.EndOfInput()...)
	return p
}

// compareWithV1 checks each session.ended of outs against what v1 let go of.
func compareWithV1(t *testing.T, outs []Output, want pairGolden) []*eventsv1.SessionEnded {
	t.Helper()
	var ended []*eventsv1.SessionEnded
	for _, o := range outs {
		if o.Ended != nil {
			ended = append(ended, o.Ended)
		}
	}
	if len(ended) != len(want.Ended) {
		t.Fatalf("%d sessions ended, v1 let go of %d", len(ended), len(want.Ended))
	}
	rounded := roundedFixtures[beforeRestart]
	for i, e := range ended {
		sum := e.GetSummary()
		w := v1AsCompared(want.Ended[i], rounded)
		w.Gaps = []any{} // asV1 leaves them out: compared below
		if d := diffs(asV1(t, sum, want.Ended[i], rounded), w); len(d) > 0 {
			t.Errorf("session %d:\n%s", i+1, strings.Join(d, "\n"))
		}
		gaps := sum.GetGaps()
		if len(gaps) != len(want.Raw[i].Gaps) {
			t.Errorf("session %d: %d gaps, v1 %d", i+1, len(gaps), len(want.Raw[i].Gaps))
			continue
		}
		for j, g := range gaps {
			wg := want.Raw[i].Gaps[j]
			if g.GetLap() != wg.Lap || msOf(g.GetStartedAt()) != secsOf(int64(wg.From*1000+0.5)) ||
				msOf(g.GetEndedAt()) != secsOf(int64(wg.To*1000+0.5)) ||
				g.GetReason() != eventsv1.GapReason_GAP_REASON_ENGINEER_RESTART {
				t.Errorf("session %d: gap %v, v1's %+v", i+1, g, wg)
			}
		}
		if name := sum.GetTrack().GetName(); name != want.Raw[i].Track {
			t.Errorf("session %d: track %q, v1 %q", i+1, name, want.Raw[i].Track)
		}
	}
	return ended
}

// AC1 (and paddock#81 AC4): the pair, a graceful stop between, gives one session:
// one session.continued, with the gap on lap 7 and Suzuka Circuit carried, no
// session.ended at the stop, and laps 1 to 8 at the end, lap 7 not complete with
// GT7's time for it; the summary is v1's.
func TestTheRestartPairIsOneSession(t *testing.T) {
	p := restartPair(t, restartGap, window, 0)
	started, _ := sessionsOf(p.first)
	if len(started) != 1 {
		t.Fatalf("the first run started %d sessions, want 1", len(started))
	}
	sid := started[0].GetSessionId()
	for _, o := range p.first {
		if o.Ended != nil {
			t.Fatalf("the first run ended its session (%v): a graceful stop ends none", o.Why)
		}
	}
	var continued []*eventsv1.SessionContinued
	var tracks []*eventsv1.TrackVerdict
	for _, o := range p.second {
		switch {
		case o.Started != nil:
			t.Errorf("the second run started a session %s", o.Started.GetSessionId())
		case o.Continued != nil:
			continued = append(continued, o.Continued)
		case o.Track != nil:
			tracks = append(tracks, o.Track.GetTrack())
		}
		if o.Session != "" && o.Session != sid {
			t.Errorf("an event of session %s, want only %s", o.Session, sid)
		}
	}
	if len(continued) != 1 {
		t.Fatalf("%d session.continued, want 1", len(continued))
	}
	c := continued[0]
	if c.GetSessionId() != sid || c.GetGap().GetLap() != 7 || c.GetTrack().GetName() != "Suzuka Circuit" || !c.GetTrack().GetCarried() {
		t.Errorf("continued %v, want %s, the gap on lap 7, Suzuka Circuit carried", c, sid)
	}
	if len(tracks) == 0 || tracks[0].GetName() != "Suzuka Circuit" || !tracks[0].GetCarried() {
		t.Errorf("the first session.track %v, want Suzuka Circuit, carried", tracks)
	}
	ended := compareWithV1(t, p.second, readPairGolden(t, "continue"))
	sum := ended[0].GetSummary()
	var laps []string
	for _, l := range sum.GetLaps() {
		laps = append(laps, fmt.Sprintf("%d complete %t", l.GetLap(), l.GetComplete()))
	}
	if len(sum.GetLaps()) != 8 || sum.GetLaps()[6].GetComplete() || sum.GetLaps()[6].GetLapTimeMs() <= 0 {
		t.Errorf("laps %v, want 1 to 8, lap 7 not complete and timed", laps)
	}
	if ended[0].GetSessionId() != sid {
		t.Errorf("the session that ended is %s, want %s", ended[0].GetSessionId(), sid)
	}
}

// The record the first run leaves carries what the ids and the radio need on: the
// pause and the entry counts, its stop, and the last lap.
func TestTheRecordCarriesTheCounts(t *testing.T) {
	p := restartPair(t, restartGap, window, 0)
	var pauses, entries int32
	for _, o := range p.first {
		if o.Paused != nil {
			pauses = o.Paused.GetPause()
		}
		if o.PitEntered != nil {
			entries = o.PitEntered.GetEntry()
		}
	}
	r := p.record
	if r.GetPauses() != pauses || pauses == 0 || r.GetEntries() != entries || entries == 0 {
		t.Errorf("the record's pauses %d and entries %d, published %d and %d", r.GetPauses(), r.GetEntries(), pauses, entries)
	}
	if r.GetLap().GetLap() != 7 || len(r.GetSummary().GetLaps()) != 6 || len(r.GetSummary().GetPitStops()) != 1 ||
		r.GetPause() != nil || r.GetPitEntry() != nil || r.GetSummary().GetEndedAt() != nil {
		t.Errorf("the record %v: want lap 7 open, 6 laps, 1 stop, no pause, no pit entry, no end", r)
	}
}

// keptEnd is the one session.ended that ends the recorded session, and what
// came before it.
func keptEnd(t *testing.T, outs []Output) (int, Output) {
	t.Helper()
	at, found := -1, Output{}
	for i, o := range outs {
		if o.Kept {
			if at >= 0 {
				t.Fatalf("the recorded session ended twice")
			}
			at, found = i, o
		}
	}
	if at < 0 {
		t.Fatal("the recorded session never ended")
	}
	if found.Ended.GetReason() != eventsv1.EndReason_END_REASON_ENGINEER_STOPPED || found.NotContinued == "" {
		t.Errorf("ended %v (%q), want engineer_stopped with why", found.Ended.GetReason(), found.NotContinued)
	}
	return at, found
}

// AC2: another car is a new session: the recorded one ends, engineer_stopped,
// from its record, then a new one starts. Both are v1's.
func TestAnotherCarEndsTheRecordedSession(t *testing.T) {
	p := restartPair(t, restartGap, window, 1690)
	at, end := keptEnd(t, p.second)
	if end.Ended.GetSessionId() != p.record.GetSessionId() {
		t.Errorf("ended %s, want the recorded %s", end.Ended.GetSessionId(), p.record.GetSessionId())
	}
	startedAt := -1
	for i, o := range p.second {
		if o.Started != nil {
			startedAt = i
		}
		if o.Continued != nil {
			t.Error("another car continued the session")
		}
	}
	if startedAt < at {
		t.Errorf("session.started at %d, the recorded end at %d: want the end first", startedAt, at)
	}
	compareWithV1(t, p.second, readPairGolden(t, "another-car"))
}

// AC3: 11 min between the pair, the timer ends the recorded session before the
// second session starts, with no frame.
func TestTheWindowPassingEndsItFromTheTimer(t *testing.T) {
	p := restartPair(t, 11*time.Minute, window, 0)
	at, _ := keptEnd(t, p.second)
	for _, o := range p.second[:at] {
		if o.Frame != nil {
			t.Fatal("a frame came before the recorded session ended: not the timer")
		}
	}
	compareWithV1(t, p.second, readPairGolden(t, "late"))
}

// AC4: a window of 0 never continues.
func TestAWindowOfZeroNeverContinues(t *testing.T) {
	p := restartPair(t, restartGap, 0, 0)
	keptEnd(t, p.second)
	for _, o := range p.second {
		if o.Continued != nil {
			t.Fatal("continued with a window of 0")
		}
	}
	compareWithV1(t, p.second, readPairGolden(t, "window-0"))
}

// split is a restart cut into one recording: a run through its packets up to and
// including the one at, its record, and a second run, given it, from the packet
// after on, received gap later than recorded, to the end of the input.
func split(t *testing.T, name string, at int, gap time.Duration, edit func(*statev1.OpenSession)) pair {
	t.Helper()
	all := packets(t, name)
	var p pair
	a := New(testIDs())
	for _, r := range all[:at+1] {
		p.first = append(p.first, a.Step(r)...)
	}
	p.record = a.Open()
	if p.record == nil {
		t.Fatalf("no session open at packet %d", at)
	}
	if edit != nil {
		edit(p.record)
	}
	b := New(NewIDs(func() time.Time { return epoch.Add(time.Hour) }, rand.NewChaCha8([32]byte{'t', 'w', 'o'})))
	b.Restore(p.record, window)
	for _, r := range all[at+1:] {
		r.Time = r.Time.Add(gap)
		p.second = append(p.second, b.Step(r)...)
	}
	p.second = append(p.second, b.EndOfInput()...)
	return p
}

// packetOf is the index of the packet whose step first gave an output that
// matches, in a run of the whole recording.
func packetOf(t *testing.T, name string, match func(Output) bool) int {
	t.Helper()
	m := New(testIDs())
	for i, r := range packets(t, name) {
		for _, o := range m.Step(r) {
			if match(o) {
				return i
			}
		}
	}
	t.Fatalf("%s: nothing matched", name)
	return 0
}

// A restart in the pit lane: the entry the first run published is the stop the
// second one completes, under the same <entry>, so radio's dedupe (radio#23)
// never takes a later entry for a resend, and the stops and stints count on to
// what the race gives without the restart.
func TestARestartInThePitLaneCountsTheEntriesOn(t *testing.T) {
	const name = "real-nuc-sugo-two-stops-2026-10-18"
	whole := replay(t, name)
	at := packetOf(t, name, func(o Output) bool { return o.PitEntered != nil }) + 30
	p := split(t, name, at, 2*time.Second, nil)
	if e := p.record.GetPitEntry(); e.GetEntry() != 1 || p.record.GetEntries() != 1 {
		t.Fatalf("the record's pit entry %v, %d entries; want entry 1 open", e, p.record.GetEntries())
	}
	var exits, entries []int32
	for _, o := range p.second {
		if o.PitExited != nil {
			exits = append(exits, o.PitExited.GetEntry(), o.PitExited.GetStop().GetStop())
		}
		if o.PitEntered != nil {
			entries = append(entries, o.PitEntered.GetEntry(), o.PitEntered.GetStop())
		}
	}
	if fmt.Sprint(exits) != "[1 1 2 2]" || fmt.Sprint(entries) != "[2 2]" {
		t.Errorf("exits (entry, stop) %v and entries (entry, stop) %v; want [1 1 2 2] and [2 2]", exits, entries)
	}
	want := lastSummary(whole)
	got := lastSummary(p.second)
	if got.GetStints() != want.GetStints() || len(got.GetPitStops()) != len(want.GetPitStops()) {
		t.Errorf("%d stints and %d stops, without the restart %d and %d",
			got.GetStints(), len(got.GetPitStops()), want.GetStints(), len(want.GetPitStops()))
	}
	for i, s := range got.GetPitStops() {
		if s.GetStop() != int32(i+1) || s.GetInLap() != want.GetPitStops()[i].GetInLap() || s.GetOutLap() != want.GetPitStops()[i].GetOutLap() {
			t.Errorf("stop %d: %v, without the restart %v", i+1, s, want.GetPitStops()[i])
		}
	}
}

// A restart in a pause: the record holds the pause, the next pause takes the
// next number, and the recorded session ended from the record ends paused, when
// the pause began (R2).
func TestARestartInAPause(t *testing.T) {
	const name = beforeRestart
	at := packetOf(t, name, func(o Output) bool { return o.Paused != nil }) + 60
	p := split(t, name, at, 2*time.Second, nil)
	pause := p.record.GetPause()
	if pause.GetPause() != 1 || p.record.GetPauses() != 1 {
		t.Fatalf("the record's pause %v, %d pauses; want pause 1 open", pause, p.record.GetPauses())
	}
	for _, o := range p.second {
		if o.Paused != nil && o.Paused.GetPause() < 2 {
			t.Errorf("a pause numbered %d after the restart", o.Paused.GetPause())
		}
		if o.Resumed != nil && o.Resumed.GetPause() < 2 {
			t.Errorf("a resume numbered %d after the restart", o.Resumed.GetPause())
		}
	}
	m := New(testIDs())
	m.Restore(p.record, 0)
	_, end := keptEnd(t, m.Expire(p.record.GetLastSeenAt().AsTime()))
	sum := end.Ended.GetSummary()
	if !sum.GetEndedPaused() || !sum.GetEndedAt().AsTime().Equal(pause.GetSince().AsTime()) {
		t.Errorf("ended paused %t at %v, want paused at the pause's start %v",
			sum.GetEndedPaused(), sum.GetEndedAt().AsTime(), pause.GetSince().AsTime())
	}
}

// The kind's start, seen before the restart, is kept by the continued session's
// verdict of the same kind (v1's set_kind).
func TestTheKindsStartIsCarried(t *testing.T) {
	p := split(t, beforeRestart, lap5(t), 2*time.Second, nil)
	before := p.record.GetSummary().GetKind()
	if before.GetStart() == eventsv1.StartType_START_TYPE_UNKNOWN {
		t.Fatalf("the first run saw no start: %v", before)
	}
	var kinds []*eventsv1.SessionKind
	for _, o := range p.second {
		if o.Kind != nil {
			kinds = append(kinds, o.Kind.GetKind())
		}
	}
	if len(kinds) == 0 {
		t.Fatal("no session.kind after the restart")
	}
	for _, k := range kinds {
		if k.GetSessionType() == before.GetSessionType() && k.GetStart() != before.GetStart() {
			t.Errorf("kind %v, want the start %v carried", k, before.GetStart())
		}
	}
}

// R6 (paddock#81 R4): a carried track is the session's at once, and the detector
// can still rule it out: carried as Suzuka Circuit East Course, which the car
// leaves the box of, the session ends at Suzuka Circuit.
func TestACarriedTrackCanBeRuledOut(t *testing.T) {
	const east = 442
	p := split(t, beforeRestart, lap5(t), 2*time.Second, func(r *statev1.OpenSession) {
		v := r.GetSummary().GetTrack()
		if v.GetName() != "Suzuka Circuit" {
			t.Fatalf("the first run's track %v", v)
		}
		id := int32(east)
		v.TrackId, v.Name = &id, "Suzuka Circuit East Course"
	})
	var tracks []*eventsv1.TrackVerdict
	for _, o := range p.second {
		if o.Track != nil {
			tracks = append(tracks, o.Track.GetTrack())
		}
	}
	if len(tracks) < 2 || tracks[0].GetTrackId() != east || !tracks[0].GetCarried() {
		t.Fatalf("session.track %v: want East carried first, then another", tracks)
	}
	last := tracks[len(tracks)-1]
	if last.GetName() != "Suzuka Circuit" || last.GetCarried() {
		t.Errorf("the last session.track %v, want Suzuka Circuit, found", last)
	}
	if sum := lastSummary(p.second); sum.GetTrack().GetName() != "Suzuka Circuit" {
		t.Errorf("the summary's track %v", sum.GetTrack())
	}
}

// A start seen from its start (not joined) is another session: the recorded one
// ends first, from its record.
func TestAStartFromTheGridEndsTheRecordedSession(t *testing.T) {
	p := restartPair(t, restartGap, window, 0)
	m := New(testIDs())
	m.Restore(p.record, window)
	var out []Output
	for _, r := range packets(t, "real-race-2laps-2026-10-14") {
		r.Time = p.record.GetLastSeenAt().AsTime().Add(time.Minute).Add(r.Time.Sub(epoch))
		out = append(out, m.Step(r)...)
	}
	at, end := keptEnd(t, out)
	if end.Ended.GetSessionId() != p.record.GetSessionId() {
		t.Errorf("ended %s, want %s", end.Ended.GetSessionId(), p.record.GetSessionId())
	}
	for _, o := range out[:at] {
		if o.Started != nil {
			t.Error("a session started before the recorded one ended")
		}
	}
	if m.Waiting() {
		t.Error("still waiting after the first start")
	}
}

// The record round-trips as JSON with the fields its OpenSession carries, and a
// record with no session is none.
func TestNoRecordIsNoneWaiting(t *testing.T) {
	m := New(testIDs())
	m.Restore(nil, window)
	if m.Waiting() || len(m.Expire(epoch)) != 0 {
		t.Error("waiting on no record")
	}
	m.Restore(&statev1.OpenSession{}, window)
	if m.Waiting() {
		t.Error("waiting on an empty record")
	}
	if m.Open() != nil {
		t.Error("a record with no session open")
	}
}

// lap5 is a packet part way through lap 6 of the old pod's recording, after the
// stop.
func lap5(t *testing.T) int {
	t.Helper()
	return packetOf(t, beforeRestart, func(o Output) bool { return o.Lap != nil && o.Lap.GetLap() == 5 }) + 20
}

// A restart in the pit lane of a later stop: the record the continuing run keeps
// still holds the stops completed before it, not only those since (the open
// stop alone is left out).
func TestARecordInThePitLaneKeepsTheStopsDone(t *testing.T) {
	const name = "real-nuc-sugo-two-stops-2026-10-18"
	all := packets(t, name)
	at := packetOf(t, name, func(o Output) bool { return o.PitEntered != nil && o.PitEntered.GetEntry() == 2 }) + 30
	a := New(testIDs())
	for _, r := range all[:at+1] {
		a.Step(r)
	}
	rec := a.Open()
	if n := len(rec.GetSummary().GetPitStops()); n != 1 || rec.GetPitEntry().GetEntry() != 2 {
		t.Fatalf("the first run's record: %d stops, entry %v; want 1 done and entry 2 open", n, rec.GetPitEntry())
	}
	b := New(NewIDs(func() time.Time { return epoch.Add(time.Hour) }, rand.NewChaCha8([32]byte{'t', 'w', 'o'})))
	b.Restore(rec, window)
	for _, r := range all[at+1 : at+4] {
		r.Time = r.Time.Add(2 * time.Second)
		b.Step(r)
	}
	again := b.Open()
	if n := len(again.GetSummary().GetPitStops()); n != 1 || again.GetPitEntry().GetEntry() != 2 {
		t.Errorf("the continuing run's record: %d stops, entry %v; want 1 done and entry 2 open", n, again.GetPitEntry())
	}
}

// A line crossed in the gap: the lap the record was on is a partial lap of the
// summary, as v1's shutdown flushed it, and the lap the run joins on is the gap's.
func TestALineCrossedInTheGapKeepsThatLap(t *testing.T) {
	all := packets(t, beforeRestart)
	at := packetOf(t, beforeRestart, func(o Output) bool { return o.Lap != nil && o.Lap.GetLap() == 4 }) + 20
	a := New(testIDs())
	for _, r := range all[:at+1] {
		a.Step(r)
	}
	rec := a.Open()
	if rec.GetLap().GetLap() != 5 {
		t.Fatalf("the record is on lap %d, want 5", rec.GetLap().GetLap())
	}
	join := at + 1
	for all[join].CurrentLap < 6 {
		join++
	}
	join += 5
	b := New(NewIDs(func() time.Time { return epoch.Add(time.Hour) }, rand.NewChaCha8([32]byte{'t', 'w', 'o'})))
	b.Restore(rec, window)
	var out []Output
	for _, r := range all[join:] {
		r.Time = r.Time.Add(2 * time.Second)
		out = append(out, b.Step(r)...)
	}
	out = append(out, b.EndOfInput()...)
	sum := lastSummary(out)
	var laps []string
	var used float64
	for _, l := range sum.GetLaps() {
		laps = append(laps, fmt.Sprintf("%d:%t", l.GetLap(), l.GetComplete()))
		used += l.GetFuelUsed()
	}
	if got := strings.Join(laps, " "); got != "1:true 2:true 3:true 4:true 5:false 6:false 7:false" {
		t.Errorf("laps %s, want 1 to 4 complete, then 5, 6 (the gap's) and 7 partial", got)
	}
	if l6 := sum.GetLaps()[5]; l6.GetLapTimeMs() <= 0 {
		t.Errorf("the gap's lap 6 has no time: %v", l6)
	}
	if round3(used) != sum.GetFuelUsed() {
		t.Errorf("fuel used %.3f, the laps' %.3f", sum.GetFuelUsed(), round3(used))
	}
	if !b.opened || b.Restore(rec, window) {
		t.Error("a record taken after the run opened a session")
	}
}

// The lap 1 restart at SUGO on 2026-10-31 (testdata/continue): ecu 0.1.11 came
// up 40 s into lap 1 with the session before it kept, SUGO found. A join on lap
// 1 can't be told from a flying start, so a fresh session opens, as v1's.
const (
	sugoLap1 = "real-nuc-sugo-restart-lap1-2026-10-31"
	sugoID   = 100001
	sugoCar  = 837
)

// sugoRecord is the kept record of the run before the restart, with the facts
// the fixture's first frame has (a 3-lap race of 20 cars, GT7's cars-on-track
// flag on, on lap 1): its session, in car, over laps, on SUGO as probable, last
// seen a minute before the run's first frame.
func sugoRecord(car, laps int32) *statev1.OpenSession {
	id := int32(sugoID)
	cars := int32(20)
	return &statev1.OpenSession{
		SessionId:  "01KDVDKEPT0000000000000000",
		LastSeenAt: timestamppb.New(epoch.Add(-time.Minute)),
		Lap:        &statev1.OpenLap{Lap: 1, FuelAtStart: 100},
		Summary: &eventsv1.SessionSummary{
			CarCode: car, TotalLaps: &laps, TotalCars: &cars,
			Kind: &eventsv1.SessionKind{
				SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, TotalLaps: &laps, Field: true, CarsOnTrack: true,
			},
			Track: &eventsv1.TrackVerdict{
				TrackId: &id, Name: "Sportsland SUGO", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE,
				Evidence: "finish line crossed in the right direction, no full lap yet",
			},
		},
	}
}

// sugoTracks runs the lap 1 restart's run, given record (nil for none): the
// session.track verdicts of the session it opens, and its summary.
func sugoTracks(t *testing.T, record *statev1.OpenSession) ([]*eventsv1.TrackVerdict, *eventsv1.SessionSummary) {
	t.Helper()
	m := New(testIDs())
	m.Restore(record, window)
	var out []Output
	for _, r := range packets(t, sugoLap1) {
		out = append(out, m.Step(r)...)
	}
	out = append(out, m.EndOfInput()...)
	var opened string
	var tracks []*eventsv1.TrackVerdict
	for _, o := range out {
		switch {
		case o.Started != nil:
			if o.Started.GetJoinedPartWay() {
				t.Error("the lap 1 run opened a session joined part way")
			}
			opened = o.Session
		case o.Track != nil && o.Session == opened:
			tracks = append(tracks, o.Track.GetTrack())
		}
	}
	if opened == "" {
		t.Fatal("no session opened")
	}
	var sum *eventsv1.SessionSummary
	for _, o := range out {
		if o.Ended != nil && o.Session == opened {
			sum = o.Ended.GetSummary()
		}
	}
	return tracks, sum
}

func trackNames(vs []*eventsv1.TrackVerdict) []string {
	names := make([]string, len(vs))
	for i, v := range vs {
		names[i] = v.GetConfidence().String() + " " + v.GetName()
	}
	return names
}

// With no record, the lap 1 run is v1.0.0's: the detector takes it for a run
// seen from its start, counts the path across Willow Springs Reverse's line,
// then Fuji's, and at the lap 2 tick judges the partial lap whole and rules SUGO
// out: none.
func TestALap1RunWithNoRecordIsV1s(t *testing.T) {
	tracks, sum := sugoTracks(t, nil)
	want := []string{
		"TRACK_CONFIDENCE_AMBIGUOUS ",
		"TRACK_CONFIDENCE_PROBABLE Willow Springs International Raceway: Streets of Willow Springs Reverse",
		"TRACK_CONFIDENCE_AMBIGUOUS ",
		"TRACK_CONFIDENCE_PROBABLE Fuji International Speedway",
		"TRACK_CONFIDENCE_NONE ",
	}
	if got := trackNames(tracks); !slices.Equal(got, want) {
		t.Errorf("session.track %v, want v1's %v", got, want)
	}
	if c := sum.GetTrack().GetConfidence(); c != eventsv1.TrackConfidence_TRACK_CONFIDENCE_NONE {
		t.Errorf("the summary's track %v, want none", sum.GetTrack())
	}
}

// A departure from v1 (resume): a lap 1 run with a kept record of the same car
// on SUGO opens a fresh session that starts with SUGO, carried, on its first
// frame, and never names Willow Springs or Fuji.
func TestALap1RestartCarriesTheKeptTrack(t *testing.T) {
	tracks, sum := sugoTracks(t, sugoRecord(sugoCar, 3))
	if len(tracks) == 0 || tracks[0].GetTrackId() != sugoID || !tracks[0].GetCarried() {
		t.Fatalf("session.track %v: want SUGO carried first", trackNames(tracks))
	}
	for _, v := range tracks {
		if v.GetTrackId() != sugoID {
			t.Errorf("session.track %q after SUGO carried", v.GetName())
		}
	}
	if sum.GetTrack().GetTrackId() != sugoID {
		t.Errorf("the summary's track %v, want SUGO", sum.GetTrack())
	}
}

// Another car's record carries no track, but the detector still knows it joined
// part way: no line crossing from a mid-lap anchor, and no partial lap judged
// whole, so neither Willow Springs nor Fuji, and SUGO by the lap 2 tick.
func TestALap1RestartInAnotherCarStillJoinsTheDetector(t *testing.T) {
	joinedNotCarried(t, sugoRecord(sugoCar+1, 3))
}

// A record that would not be continued on more than the lap 1 rule is another
// race: a record of a 5-lap race carries nothing into this 3-lap one, and the
// detector still joins.
func TestALap1RestartOfAnotherRaceCarriesNothing(t *testing.T) {
	joinedNotCarried(t, sugoRecord(sugoCar, 5))
}

// joinedNotCarried checks the lap 1 run, given record: no track carried, and the
// detector joined, so neither Willow Springs nor Fuji, and SUGO at the end.
func joinedNotCarried(t *testing.T, record *statev1.OpenSession) {
	t.Helper()
	tracks, sum := sugoTracks(t, record)
	for _, v := range tracks {
		if v.GetCarried() {
			t.Errorf("a track carried: %v", v)
		}
		if strings.Contains(v.GetName(), "Willow") || strings.Contains(v.GetName(), "Fuji") {
			t.Errorf("session.track %q", v.GetName())
		}
	}
	if sum.GetTrack().GetTrackId() != sugoID {
		t.Errorf("the summary's track %v (session.track %v), want SUGO", sum.GetTrack(), trackNames(tracks))
	}
}
