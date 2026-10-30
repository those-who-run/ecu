package race

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/pit"
	"github.com/those-who-run/salsa"
)

// The pit stops on v1's recordings (paddock#78), against testdata/pit/'s goldens:
// v1.0.0's PitDetector and engine on each, from make_golden.py there.

// The recordings with a stop: the line crossed in the entry cut (Suzuka East),
// the exit cut (Willow Springs) and the pit lane (Gilles Villeneuve); two stops
// (SUGO); a stop that drives out (Suzuka before the restart); and a quit from
// the box (Spa).
const (
	suzukaPitStop  = "real-nuc-suzuka-pit-stop-2026-10-15"
	willowPitStop  = "real-nuc-willow-springs-pit-stop-2026-10-17"
	gillesPitStop  = "real-nuc-gilles-villeneuve-pit-stop-2026-10-17"
	sugoTwoStops   = "real-nuc-sugo-two-stops-2026-10-18"
	suzukaDriveOut = "real-nuc-suzuka-before-restart-2026-10-18"
	spaPitCutQuit  = "real-nuc-spa-pit-cut-quit-2026-10-18"
)

// pitLine is a line of a pit golden: a change of v1's detector, with the session
// v1's engine applied it to, or a session's end with the stops it stored.
type pitLine struct {
	Event     string           `json:"event"`
	Frame     uint32           `json:"frame"`
	Stint     int32            `json:"stint"`
	InPitLane bool             `json:"in_pit_lane"`
	Stop      map[string]any   `json:"stop"`
	Teleport  map[string]any   `json:"teleport"`
	Session   *int             `json:"session"`
	Stints    int32            `json:"stints"`
	PitStops  []map[string]any `json:"pit_stops"`
}

// show is a detector's change as the test compares it, in JSON: the frame, the
// change, the stint, the lane and the stop or teleport, v1's way.
func (l pitLine) show() string {
	b, _ := json.Marshal(map[string]any{
		"event": l.Event, "frame": l.Frame, "stint": l.Stint, "in_pit_lane": l.InPitLane,
		"stop": l.Stop, "teleport": l.Teleport,
	})
	return string(b)
}

func readPitGolden(t *testing.T, name string) (changes, ended []pitLine) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "pit", name+".jsonl")) //nolint:gosec // the test's own golden
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l pitLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		if l.Event == "session.ended" {
			ended = append(ended, l)
		} else {
			changes = append(changes, l)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return changes, ended
}

// asJSON is v through JSON, as the golden's maps are read.
func asJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// v1Record is the stop as v1's PitStop.record() writes it.
func v1Record(t *testing.T, s pit.Stop) map[string]any {
	t.Helper()
	opt := func(p int32) any {
		if p == 0 {
			return nil
		}
		return p
	}
	var outLap, lane any
	if secs, ok := s.PitLaneSecs(); ok {
		outLap, lane = s.OutLap, roundN(secs, 2)
	}
	return asJSON(t, map[string]any{
		"stop": s.Number, "in_lap": s.InLap, "out_lap": outLap, "pit_lane_secs": lane,
		"stationary_secs": roundN(s.StationarySecs, 2),
		"fuel_before":     round3(s.FuelBefore), "fuel_after": round3(s.FuelAfter),
		"fuel_added": round3(s.FuelAdded()), "tyres_changed": s.TyresChanged,
		"pit_lane_kph":    roundN(s.PitLaneKph, 1),
		"position_before": opt(s.PositionBefore), "position_after": opt(s.PositionAfter),
		"evidence": s.Evidence,
	})
}

// frameList is the frames of the outputs, in order.
func frameList(out []Output) []*telemetryv1.Frame {
	var all []*telemetryv1.Frame
	for _, o := range out {
		if o.Frame != nil {
			all = append(all, o.Frame)
		}
	}
	return all
}

// detect is each change a new detector makes on the frames, as a golden's line.
func detect(t *testing.T, frames []*telemetryv1.Frame) ([]pitLine, *pit.Detector) {
	t.Helper()
	d := pit.New()
	var lines []pitLine
	for _, f := range frames {
		change := d.Observe(f)
		if change == pit.Unchanged {
			continue
		}
		s := d.Status()
		l := pitLine{Frame: f.GetSeq(), Stint: s.Stint(), InPitLane: s.InPitLane}
		switch change {
		case pit.Entered, pit.Exited:
			l.Event = map[pit.Change]string{pit.Entered: "pit.entered", pit.Exited: "pit.exited"}[change]
			l.Stop = v1Record(t, s.Stops[len(s.Stops)-1])
		case pit.Withdrawn:
			jump := s.Teleports[len(s.Teleports)-1]
			l.Event = "pit.teleport"
			l.Teleport = asJSON(t, map[string]any{"lap": jump.Lap, "t": jump.T, "entry_t": jump.EntryT, "evidence": jump.Evidence})
		}
		lines = append(lines, l)
	}
	return lines, d
}

func shown(lines []pitLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.show()
	}
	return out
}

// The detector, fed every frame of each of v1's recordings as v1's
// tests/test_pitstop.py feeds it, changes on v1's frames with v1's stops, field
// for field.
func TestThePitDetectorIsV1sOnEveryRecording(t *testing.T) {
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			got, _ := detect(t, frameList(replay(t, name)))
			want, _ := readPitGolden(t, name)
			if g, w := shown(got), shown(want); !slices.Equal(g, w) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(g, "\n"), strings.Join(w, "\n"))
			}
		})
	}
}

// pitStopFromV1 is the contracts' PitStop for a stop as v1's PitStop.record()
// wrote it, as contracts' own test converts it. A stop still open has no position
// after (contracts v0.2.3), where v1 kept its last.
func pitStopFromV1(t *testing.T, r map[string]any) *eventsv1.PitStop {
	t.Helper()
	i32 := func(k string) int32 { return int32(r[k].(float64)) }
	opt := func(k string) *int32 {
		if v, ok := r[k].(float64); ok {
			return proto.Int32(int32(v))
		}
		return nil
	}
	s := &eventsv1.PitStop{
		Stop: i32("stop"), InLap: i32("in_lap"), OutLap: opt("out_lap"),
		StationaryMs: int32(math.Round(r["stationary_secs"].(float64) * 1000)),
		FuelBefore:   r["fuel_before"].(float64), FuelAfter: r["fuel_after"].(float64),
		FuelAdded: r["fuel_added"].(float64), TyresChanged: r["tyres_changed"].(bool),
		PitLaneKph:     proto.Float64(r["pit_lane_kph"].(float64)),
		PositionBefore: opt("position_before"), PositionAfter: opt("position_after"),
		Evidence: r["evidence"].(string),
	}
	if v, ok := r["pit_lane_secs"].(float64); ok {
		s.PitLaneMs = proto.Int32(int32(math.Round(v * 1000)))
	}
	if s.OutLap == nil {
		s.PositionAfter = nil
	}
	return s
}

// R1–R3, R5, AC1–AC4: on every one of v1's recordings, ecu publishes pit.entered
// and pit.exited where v1's engine applied an entry or an exit to the session,
// with v1's figures, and each session.ended's summary keeps the stints and stops
// v1 stored for the session.
func TestPitEventsAreV1sOnEveryRecording(t *testing.T) {
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			all := packets(t, name)
			out := run(all)
			changes, ended := readPitGolden(t, name)

			var sessions []string
			var got []proto.Message
			var summaries []*eventsv1.SessionSummary
			for _, o := range out {
				switch {
				case o.Started != nil:
					sessions = append(sessions, o.Session)
				case o.PitEntered != nil:
					got = append(got, o.PitEntered)
				case o.PitExited != nil:
					got = append(got, o.PitExited)
				case o.PitTeleport != nil:
					got = append(got, o.PitTeleport)
				case o.Ended != nil:
					summaries = append(summaries, o.Ended.GetSummary())
				}
			}

			var want []proto.Message
			entries := map[int]int32{}
			var entered *eventsv1.PitEntered
			for _, l := range changes {
				if l.Session == nil {
					continue
				}
				sid := sessions[*l.Session-1]
				r := all[l.Frame-1]
				switch l.Event {
				case "pit.entered":
					stop := pitStopFromV1(t, l.Stop)
					entries[*l.Session]++
					entered = &eventsv1.PitEntered{
						SessionId: sid, Entry: entries[*l.Session], Stop: stop.GetStop(), InLap: stop.GetInLap(),
						EnteredT: r.T, EnteredAt: timestamppb.New(r.Time), FuelBefore: stop.GetFuelBefore(),
						PositionBefore: stop.PositionBefore, Evidence: stop.GetEvidence(), At: l.Frame,
					}
					want = append(want, entered)
				case "pit.exited":
					stop := pitStopFromV1(t, l.Stop)
					want = append(want, &eventsv1.PitExited{
						SessionId: sid, Stop: stop, Stint: l.Stint, Entry: entered.GetEntry(),
						EnteredAt: entered.GetEnteredAt(), ExitedAt: timestamppb.New(r.Time), At: l.Frame,
						ExitedT: r.T, EnteredT: entered.GetEnteredT(),
					})
				case "pit.teleport":
					at := entered.GetAt()
					want = append(want, &eventsv1.PitTeleport{
						SessionId: sid, Entry: entered.GetEntry(), EntryAt: &at,
						Lap: int32(l.Teleport["lap"].(float64)), Evidence: l.Teleport["evidence"].(string),
						At: l.Frame, WithdrawnT: l.Teleport["t"].(float64),
					})
				default:
					t.Fatalf("%s: not a pit change", l.Event)
				}
			}
			if len(got) != len(want) {
				t.Fatalf("%d pit events, want %d:\n%v\nwant\n%v", len(got), len(want), got, want)
			}
			for i := range got {
				if !proto.Equal(got[i], want[i]) {
					t.Errorf("pit event %d:\n%v\nwant\n%v", i+1, got[i], want[i])
				}
			}

			if len(summaries) != len(ended) {
				t.Fatalf("%d sessions ended, v1 stored %d", len(summaries), len(ended))
			}
			for i, l := range ended {
				s := summaries[i]
				if s.GetStints() != l.Stints || len(s.GetPitStops()) != len(l.PitStops) {
					t.Fatalf("session %d: %d stints, %d stops; v1 stored %d, %d",
						i+1, s.GetStints(), len(s.GetPitStops()), l.Stints, len(l.PitStops))
				}
				for j, r := range l.PitStops {
					if w := pitStopFromV1(t, r); !proto.Equal(s.GetPitStops()[j], w) {
						t.Errorf("session %d, stop %d:\n%v\nwant\n%v", i+1, j+1, s.GetPitStops()[j], w)
					}
				}
			}
		})
	}
}

// pitEvents are the pit events of the outputs, in order.
func pitEvents(out []Output) []Output {
	var evs []Output
	for _, o := range out {
		if o.PitEntered != nil || o.PitExited != nil || o.PitTeleport != nil {
			evs = append(evs, o)
		}
	}
	return evs
}

// AC1: each of the three single stops is one pit.entered and one pit.exited, with
// v1's in-lap, out-lap, fuel and tyres.
func TestEachSingleStopIsOneEntryAndOneExit(t *testing.T) {
	for _, c := range []struct {
		name          string
		in, out       int32
		fuelBefore    float64
		fuelAfter     float64
		tyres         bool
		lane          int32
		posBefore     int32
		posAfterOrNil int32
	}{
		{suzukaPitStop, 3, 4, 22.899, 99.999, false, 12160, 1, 1},
		{willowPitStop, 6, 7, 16.014, 65.353, true, 13110, 2, 3},
		{gillesPitStop, 6, 7, 8.685, 100, true, 30220, 5, 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			evs := pitEvents(replay(t, c.name))
			if len(evs) != 2 || evs[0].PitEntered == nil || evs[1].PitExited == nil {
				t.Fatalf("pit events %v", evs)
			}
			in, out := evs[0].PitEntered, evs[1].PitExited
			stop := out.GetStop()
			if in.GetInLap() != c.in || in.GetFuelBefore() != c.fuelBefore || in.GetPositionBefore() != c.posBefore ||
				in.GetEntry() != 1 || in.GetStop() != 1 {
				t.Errorf("entered %v", in)
			}
			if stop.GetInLap() != c.in || stop.GetOutLap() != c.out || stop.GetFuelBefore() != c.fuelBefore ||
				stop.GetFuelAfter() != c.fuelAfter || stop.GetTyresChanged() != c.tyres ||
				stop.GetPitLaneMs() != c.lane || stop.GetPositionAfter() != c.posAfterOrNil ||
				out.GetStint() != 2 || out.GetEntry() != 1 {
				t.Errorf("exited %v", out)
			}
			if lane := out.GetExitedT() - out.GetEnteredT(); math.Abs(lane*1000-float64(c.lane)) > 5 {
				t.Errorf("exited_t less entered_t is %.3f s, the stop's lane %d ms", lane, c.lane)
			}
		})
	}
}

// AC2: SUGO's two stops are three stints.
func TestSugoIsTwoStopsAndThreeStints(t *testing.T) {
	out := replay(t, sugoTwoStops)
	var exits []*eventsv1.PitExited
	for _, o := range pitEvents(out) {
		if o.PitExited != nil {
			exits = append(exits, o.PitExited)
		}
	}
	if len(exits) != 2 || exits[0].GetStint() != 2 || exits[1].GetStint() != 3 ||
		exits[0].GetStop().GetInLap() != 4 || exits[1].GetStop().GetInLap() != 8 {
		t.Fatalf("exits %v", exits)
	}
	if s := lastSummary(out); s.GetStints() != 3 || len(s.GetPitStops()) != 2 {
		t.Errorf("summary: %d stints, %d stops", s.GetStints(), len(s.GetPitStops()))
	}
}

func lastSummary(out []Output) *eventsv1.SessionSummary {
	var s *eventsv1.SessionSummary
	for _, o := range out {
		if o.Ended != nil {
			s = o.Ended.GetSummary()
		}
	}
	return s
}

// AC3: the stop on lap 4 before the restart drives out, with no exit cut, after
// about 3.5 min at rest and no fuel.
func TestTheStopBeforeTheRestartDrivesOut(t *testing.T) {
	evs := pitEvents(replay(t, suzukaDriveOut))
	if len(evs) != 2 || evs[1].PitExited == nil {
		t.Fatalf("pit events %v", evs)
	}
	stop := evs[1].PitExited.GetStop()
	if stop.GetInLap() != 4 || stop.GetOutLap() != 4 || stop.GetFuelAdded() != 0 ||
		!strings.HasSuffix(stop.GetEvidence(), "over 100 km/h") ||
		stop.GetStationaryMs() < 200_000 || stop.GetStationaryMs() > 230_000 {
		t.Errorf("stop %v", stop)
	}
}

// AC4: at Spa the car enters the pits and the session is quit from the box: no
// pit.exited, and the session ends with the stop open and its track kept.
func TestSpaEndsWithTheStopOpen(t *testing.T) {
	out := replay(t, spaPitCutQuit)
	evs := pitEvents(out)
	if len(evs) != 1 || evs[0].PitEntered == nil || evs[0].PitEntered.GetInLap() != 5 {
		t.Fatalf("pit events %v", evs)
	}
	s := lastSummary(out)
	if s.GetStints() != 1 || len(s.GetPitStops()) != 1 || s.GetTrack() == nil {
		t.Fatalf("summary %v", s)
	}
	if stop := s.GetPitStops()[0]; stop.OutLap != nil || stop.PitLaneMs != nil || stop.PositionAfter != nil ||
		stop.GetEvidence() != "entry loading cut at 0.0 km/h" {
		t.Errorf("stop %v", stop)
	}
}

// R1: each pit cut is a single packet. Without the loading frames the car still
// jumps 118 to 330 m between two packets, and the same stop is found (v1's
// test_a_lost_cut_frame_is_seen_as_a_teleport).
func TestALostCutFrameIsSeenAsATeleport(t *testing.T) {
	for _, name := range []string{gillesPitStop, willowPitStop, suzukaPitStop} {
		t.Run(name, func(t *testing.T) {
			frames := frameList(replay(t, name))
			_, withCuts := detect(t, frames)
			var noCuts []*telemetryv1.Frame
			for _, f := range frames {
				if f.GetPhase() != telemetryv1.Phase_PHASE_LOADING || f.GetLap() < 1 {
					noCuts = append(noCuts, f)
				}
			}
			_, d := detect(t, noCuts)
			got, want := d.Status().Stops, withCuts.Status().Stops
			if len(got) != 1 || len(want) != 1 {
				t.Fatalf("stops %v, want %v", got, want)
			}
			if got[0].InLap != want[0].InLap || got[0].OutLap != want[0].OutLap ||
				got[0].FuelAfter != want[0].FuelAfter || got[0].Evidence != "entry teleport at 60.0 km/h; exit teleport" {
				t.Errorf("stop %+v, want %+v", got[0], want[0])
			}
		})
	}
}

// The Gilles Villeneuve stop is contracts' testdata/v1/gilles_villeneuve_pit_stop.json,
// v1's PitDetector at the entry and the exit: the same frames, by their t and
// packet ids, and the same stop, field for field.
func TestGillesVilleneuveIsTheContractsGolden(t *testing.T) {
	dir, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}",
		"github.com/those-who-run/contracts").Output()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(dir)), "testdata", "v1", "gilles_villeneuve_pit_stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	type at struct {
		Frame struct {
			T        float64 `json:"t"`
			PacketID uint32  `json:"packet_id"`
		} `json:"frame"`
		Stint int32          `json:"stint"`
		Stop  map[string]any `json:"stop"`
	}
	var golden struct{ Entry, Exit at }
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	out := replay(t, gillesPitStop)
	byseq := frames(out)
	evs := pitEvents(out)
	if len(evs) != 2 {
		t.Fatalf("pit events %v", evs)
	}
	in, exit := evs[0].PitEntered, evs[1].PitExited
	entry := pitStopFromV1(t, golden.Entry.Stop)
	if f := byseq[in.GetAt()]; f.GetPacketId() != golden.Entry.Frame.PacketID || in.GetEnteredT() != golden.Entry.Frame.T {
		t.Errorf("entered on packet %d at %v, want %d at %v", f.GetPacketId(), in.GetEnteredT(), golden.Entry.Frame.PacketID, golden.Entry.Frame.T)
	}
	if in.GetStop() != entry.GetStop() || in.GetInLap() != entry.GetInLap() || in.GetFuelBefore() != entry.GetFuelBefore() ||
		in.GetPositionBefore() != entry.GetPositionBefore() || in.GetEvidence() != entry.GetEvidence() {
		t.Errorf("entered %v, want %v", in, entry)
	}
	if f := byseq[exit.GetAt()]; f.GetPacketId() != golden.Exit.Frame.PacketID || exit.GetExitedT() != golden.Exit.Frame.T {
		t.Errorf("exited on packet %d at %v, want %d at %v", f.GetPacketId(), exit.GetExitedT(), golden.Exit.Frame.PacketID, golden.Exit.Frame.T)
	}
	if w := pitStopFromV1(t, golden.Exit.Stop); !proto.Equal(exit.GetStop(), w) || exit.GetStint() != golden.Exit.Stint {
		t.Errorf("exited %v, want %v, stint %d", exit, w, golden.Exit.Stint)
	}
}

// pitDrive builds packets by hand, a frame each 1/60 s of packets: a five-lap race
// in a field of 20, driving along x.
type pitDrive struct{ all []Received }

func (d *pitDrive) add(t float64, lap int16, flags salsa.Flags, kph, x, fuel float32) {
	var p salsa.Packet
	p.CurrentLap, p.TotalLaps, p.TotalCars, p.Flags = lap, 5, 20, flags|salsa.FlagCarsOnTrack
	p.Speed, p.Position.X, p.FuelLevel, p.RacePosition = kph/3.6, x, fuel, 4
	p.PacketID = int32(t * 60)
	d.all = append(d.all, Received{
		Received: salsa.Received{T: t, Packet: p},
		Time:     epoch.Add(time.Duration(t * float64(time.Second))),
	})
}

// The ids (contracts v0.2.3): pit.entered:<entry> counts every entry, a withdrawn
// one too, and pit.exited:<stop> counts the stops, so an entry withdrawn (as a
// reset to track is) and the stop after it have ids of their own.
func TestAWithdrawnEntryAndTheNextHaveTheirOwnIDs(t *testing.T) {
	var d pitDrive
	d.add(0, 0, 0, 0, 0, 50) // the grid
	d.add(1, 1, 0, 150, 0, 50)
	d.add(2, 1, 0, 150, 40, 50)
	// Off and put back on track, straight back up to speed: not a stop.
	d.add(2.02, 1, salsa.FlagLoading, 0, 340, 50)
	d.add(2.04, 1, 0, 40, 340, 50)
	d.add(4, 1, 0, 110, 400, 50)
	// The pits: entry, the box with fuel, exit.
	d.add(5, 1, 0, 150, 450, 50)
	d.add(5.02, 1, salsa.FlagLoading, 0, 700, 50)
	d.add(5.04, 1, 0, 60, 700, 50)
	d.add(8, 1, 0, 0, 760, 50)
	d.add(12, 1, 0, 0, 760, 90)
	d.add(14, 1, 0, 60, 800, 90)
	d.add(14.02, 2, salsa.FlagLoading, 80, 1000, 90)
	d.add(14.04, 2, 0, 80, 1000, 90)
	out := run(d.all)
	var ids []string
	sid := ""
	for _, o := range out {
		if o.Started != nil {
			sid = o.Session
		}
		if o.PitEntered != nil || o.PitExited != nil || o.PitTeleport != nil {
			ids = append(ids, strings.TrimPrefix(o.Event.ID, sid+":"))
		}
	}
	want := []string{"pit.entered:1", "pit.teleport:1", "pit.entered:2", "pit.exited:1"}
	if !slices.Equal(ids, want) {
		t.Fatalf("ids %v, want %v", ids, want)
	}
	evs := pitEvents(out)
	if e := evs[0].PitEntered; e.GetEntry() != 1 || e.GetStop() != 1 {
		t.Errorf("first entry %v", e)
	}
	if e := evs[2].PitEntered; e.GetEntry() != 2 || e.GetStop() != 1 {
		t.Errorf("second entry %v", e)
	}
	if x := evs[3].PitExited; x.GetEntry() != 2 || x.GetStop().GetStop() != 1 || x.GetStint() != 2 ||
		x.GetStop().GetFuelAdded() != 40 || x.GetStop().GetOutLap() != 2 {
		t.Errorf("exit %v", x)
	}
}

// A pit change with no session open publishes nothing: the detector sees every
// frame, but its stops are a session's.
func TestNoPitEventWithNoSession(t *testing.T) {
	m := New(testIDs())
	f := func(t float64, phase telemetryv1.Phase, x float64) {
		m.observePits(&telemetryv1.Frame{T: t, PacketId: uint32(t * 60), Lap: 2, Phase: phase, PosX: x, SpeedKph: 60}, epoch)
	}
	f(0, telemetryv1.Phase_PHASE_IN_RACE, 0)
	f(0.02, telemetryv1.Phase_PHASE_LOADING, 300)
	f(0.04, telemetryv1.Phase_PHASE_IN_RACE, 300)
	if len(m.out) != 0 || !m.pits.Status().InPitLane {
		t.Errorf("outputs %v, in the pit lane %t", m.out, m.pits.Status().InPitLane)
	}
}

// AC1 of paddock#79: a 300 m jump at racing speed on lap 3, then driving on at
// over 100 km/h without ever coming to rest, is an entry withdrawn: pit.entered,
// then pit.teleport naming it, no pit.exited, and lap 3 no pit lap. The
// session's stops and stints are as they were before the entry.
func TestAJumpOnLap3IsATeleportNotAStop(t *testing.T) {
	var d pitDrive
	d.add(0, 0, 0, 0, 0, 50)                                // the grid
	x := func(t float64) float32 { return float32(50 * t) } // 180 km/h
	for at := 1.0; at <= 25; at += 0.5 {
		d.add(at, int16(1+at/10), 0, 180, x(at), 50)
	}
	// Put back on track 300 m on, at racing speed, and driving on.
	const jump = 25 + 1.0/60
	for _, at := range []float64{jump, 25.5, 26, 26.5} {
		d.add(at, 3, 0, 180, x(at)+300, 50)
	}
	d.add(30, 4, 0, 180, x(30)+300, 50)

	m := New(testIDs())
	var out []Output
	for _, r := range d.all {
		out = append(out, m.Step(r)...)
	}
	status := m.pits.Status()
	out = append(out, m.EndOfInput()...)

	evs := pitEvents(out)
	if len(evs) != 2 || evs[0].PitEntered == nil || evs[1].PitTeleport == nil {
		t.Fatalf("pit events %v, want an entry and a teleport", evs)
	}
	in, jumped := evs[0].PitEntered, evs[1].PitTeleport
	if in.GetEntry() != 1 || in.GetStop() != 1 || in.GetInLap() != 3 || in.GetEnteredT() != jump ||
		in.GetEvidence() != "entry teleport at 180.0 km/h" {
		t.Errorf("entered %v", in)
	}
	want := &eventsv1.PitTeleport{
		SessionId: in.GetSessionId(), Entry: 1, EntryAt: proto.Uint32(in.GetAt()), Lap: 3,
		Evidence: "entry teleport at 180.0 km/h; over 100 km/h; never at rest, no fuel, no tyres",
		At:       in.GetAt() + 1, WithdrawnT: 25.5,
	}
	if !proto.Equal(jumped, want) {
		t.Errorf("teleport\n%v\nwant\n%v", jumped, want)
	}
	if id := evs[1].Event.ID; id != in.GetSessionId()+":pit.teleport:1" || evs[1].Session != in.GetSessionId() {
		t.Errorf("teleport id %s in session %s", id, evs[1].Session)
	}
	if status.InPitLane || len(status.Stops) != 0 || len(status.Teleports) != 1 || status.Stint() != 1 ||
		status.LapRole(3) != pit.RoleNone {
		t.Errorf("status %+v, lap 3 %v", status, status.LapRole(3))
	}
	if s := lastSummary(out); s.GetStints() != 1 || len(s.GetPitStops()) != 0 {
		t.Errorf("summary: %d stints, stops %v", s.GetStints(), s.GetPitStops())
	}
}
