package race

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// v1Summary is a line of a summary golden (testdata/summary/make_golden.py): a
// session's summary.json as v1 wrote it, the fields SessionSummary carries.
type v1Summary struct {
	StartedAt         float64   `json:"started_at"`
	EndedAt           float64   `json:"ended_at"`
	DrivingMs         int32     `json:"driving_ms"`
	PausedMs          int32     `json:"paused_ms"`
	EndedPaused       bool      `json:"ended_paused"`
	CarCode           int32     `json:"car_code"`
	CarName           *string   `json:"car_name"`
	TotalLaps         int32     `json:"total_laps"`
	LapsCompleted     int32     `json:"laps_completed"`
	BestLapMs         *int32    `json:"best_lap_ms"`
	FastestLapMs      *int32    `json:"fastest_lap_ms"`
	Finished          bool      `json:"finished"`
	JoinedOnLap       *int32    `json:"joined_on_lap"`
	TotalTimeMs       *int32    `json:"total_time_ms"`
	FinalPosition     *int32    `json:"final_position"`
	TotalCars         *int32    `json:"total_cars"`
	FuelBurned        float64   `json:"fuel_burned"`
	FuelBurnedByStint []float64 `json:"fuel_burned_by_stint"`
	FuelStints        []float64 `json:"fuel_stints"`
	GT7BestLapMs      *int32    `json:"gt7_best_lap_ms"`
	FuelCapacity      float64   `json:"fuel_capacity"`
	FuelAtStart       float64   `json:"fuel_at_start"`
	FuelAtEnd         float64   `json:"fuel_at_end"`
	FuelUsed          float64   `json:"fuel_used"`
	Stints            int32     `json:"stints"`
	PitStops          int       `json:"pit_stops"`
	Gaps              []any     `json:"gaps"`
	Laps              []v1Lap   `json:"laps"`
}

type v1Lap struct {
	Lap       int32   `json:"lap"`
	LapTimeMs *int32  `json:"lap_time_ms"`
	Complete  bool    `json:"complete"`
	FuelStart float64 `json:"fuel_start"`
	FuelEnd   float64 `json:"fuel_end"`
	FuelUsed  float64 `json:"fuel_used"`
	Position  *int32  `json:"position"`
	Pit       *string `json:"pit"`
}

func readSummaries(t *testing.T, name string) []v1Summary {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "summary", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var out []v1Summary
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var s v1Summary
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out = append(out, s)
	}
	return out
}

// v1Roles are v1's names for the laps' pit roles.
var v1Roles = map[eventsv1.PitRole]string{
	eventsv1.PitRole_PIT_ROLE_IN_LAP: "in", eventsv1.PitRole_PIT_ROLE_OUT_LAP: "out",
	eventsv1.PitRole_PIT_ROLE_IN_AND_OUT: "in_out",
}

// asV1 is ecu's summary as v1 wrote it, with ecu's departures from v1 undone
// where the summary says what v1 had:
//   - outside a field (solo laps or solo timed) ecu leaves the positions out, where
//     v1 kept GT7's 1 (contracts v0.1.4, paddock#276): v1's are taken as they are;
//   - a held start (the start check, paddock#74) starts when its first held frame
//     was received (contracts v0.2.3), where v1's clock read the verdict's frame:
//     started_at is compared with session.started's own, which race_test.go and
//     start_test.go hold to the first frame;
//   - with no GT7 best, the best lap is the quickest that counts, where v1's
//     fallback took a lap 1 that was the only one timed (contracts v0.2.3, laps.go):
//     the 24 h race and the first qualifying of 2026-10-14, where ecu has none and
//     v1's is its fastest lap.
//
// Times are compared to the millisecond, fuel levels at float32 precision for a
// recording in v1's rounded format (roundedFixtures).
func asV1(t *testing.T, sum *eventsv1.SessionSummary, want v1Summary, float32s bool) v1Summary {
	t.Helper()
	opt := func(p *int32) *int32 {
		if p == nil {
			return nil
		}
		return ptr(*p)
	}
	var name *string
	if sum.GetCarName() != "" {
		name = ptr(sum.GetCarName())
	}
	fuel := func(x float64) float64 {
		if float32s {
			return float64(float32(x))
		}
		return x
	}
	got := v1Summary{
		StartedAt: msOf(sum.GetStartedAt()), EndedAt: msOf(sum.GetEndedAt()),
		DrivingMs: sum.GetDrivingMs(), PausedMs: sum.GetPausedMs(), EndedPaused: sum.GetEndedPaused(),
		CarCode: sum.GetCarCode(), CarName: name, TotalLaps: sum.GetTotalLaps(),
		LapsCompleted: sum.GetLapsCompleted(), BestLapMs: opt(sum.BestLapTimeMs), FastestLapMs: opt(sum.FastestLapMs),
		Finished: sum.GetFinished(), JoinedOnLap: opt(sum.JoinedOnLap), TotalTimeMs: opt(sum.TotalTimeMs),
		FinalPosition: opt(sum.FinalPosition), TotalCars: opt(sum.TotalCars),
		FuelBurned: sum.GetFuelBurned(), FuelBurnedByStint: append([]float64{}, sum.GetFuelBurnedByStint()...), FuelStints: sum.GetFuelStints(),
		GT7BestLapMs: opt(sum.GameBestLapMs), FuelCapacity: sum.GetFuelCapacity(),
		FuelAtStart: fuel(sum.GetFuelAtStart()), FuelAtEnd: fuel(sum.GetFuelAtEnd()), FuelUsed: sum.GetFuelUsed(),
		Stints: sum.GetStints(), PitStops: len(sum.GetPitStops()), Gaps: []any{},
	}
	if sum.BestLapTimeMs == nil && sum.GameBestLapMs == nil && want.BestLapMs != nil &&
		want.FastestLapMs != nil && *want.BestLapMs == *want.FastestLapMs &&
		len(want.Laps) > 0 && want.Laps[0].Lap == 1 && want.Laps[0].LapTimeMs != nil && *want.Laps[0].LapTimeMs == *want.BestLapMs {
		got.BestLapMs = want.BestLapMs
	}
	soloSession := solo(sum.GetSessionType())
	if soloSession {
		if sum.FinalPosition != nil {
			t.Errorf("a solo session's final position %d, want none", sum.GetFinalPosition())
		}
		got.FinalPosition = want.FinalPosition
	}
	for i, l := range sum.GetLaps() {
		lap := v1Lap{
			Lap: l.GetLap(), Complete: l.GetComplete(), FuelStart: fuel(l.GetFuelAtStart()), FuelEnd: fuel(l.GetFuelAtEnd()),
			FuelUsed: l.GetFuelUsed(), Position: opt(l.Position),
		}
		if ms := l.GetLapTimeMs(); ms > 0 {
			lap.LapTimeMs = &ms
		}
		if r, ok := v1Roles[l.GetPitRole()]; ok {
			lap.Pit = &r
		}
		if soloSession && i < len(want.Laps) {
			if l.Position != nil {
				t.Errorf("a solo session's lap %d has position %d, want none", l.GetLap(), l.GetPosition())
			}
			lap.Position = want.Laps[i].Position
		}
		got.Laps = append(got.Laps, lap)
	}
	return got
}

func secsOf(ms int64) float64 { return float64(ms) / 1000 }

// msOf is ts in epoch seconds, to the nearest millisecond.
func msOf(ts *timestamppb.Timestamp) float64 {
	return secsOf(int64(math.Round(float64(ts.AsTime().UnixNano()) / 1e6)))
}

// v1AsCompared is the golden's line as asV1 compares it: times to the
// millisecond, fuel levels at float32 precision for a rounded recording.
func v1AsCompared(s v1Summary, float32s bool) v1Summary {
	s.StartedAt = secsOf(int64(math.Round(s.StartedAt * 1000)))
	s.EndedAt = secsOf(int64(math.Round(s.EndedAt * 1000)))
	if s.FuelBurnedByStint == nil {
		s.FuelBurnedByStint = []float64{}
	}
	s.Laps = slices.Clone(s.Laps)
	if float32s {
		s.FuelAtStart, s.FuelAtEnd = float64(float32(s.FuelAtStart)), float64(float32(s.FuelAtEnd))
		for i := range s.Laps {
			s.Laps[i].FuelStart, s.Laps[i].FuelEnd = float64(float32(s.Laps[i].FuelStart)), float64(float32(s.Laps[i].FuelEnd))
		}
	}
	return s
}

// diffs is each field of got that is not want's, by its JSON name.
func diffs(got, want v1Summary) []string {
	var g, w map[string]any
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	_ = json.Unmarshal(gb, &g)
	_ = json.Unmarshal(wb, &w)
	var out []string
	for k, wv := range w {
		if fmt.Sprint(g[k]) != fmt.Sprint(wv) {
			out = append(out, fmt.Sprintf("%s: got %v, want %v", k, g[k], wv))
		}
	}
	slices.Sort(out)
	return out
}

// maxHeldSecs bounds how much later v1 dated a held start than ecu: the start
// check's hold, 3 s in the one recording with one (real-race-2laps-2026-10-14).
const maxHeldSecs = 5

// sessionsOf is each session's session.started and session.ended, in order.
func sessionsOf(out []Output) (started []*eventsv1.SessionStarted, ended []*eventsv1.SessionEnded) {
	for _, o := range out {
		switch {
		case o.Started != nil:
			started = append(started, o.Started)
		case o.Ended != nil:
			ended = append(ended, o.Ended)
		}
	}
	return started, ended
}

// paddock#86, R3 and R4: on every one of v1's 25 recordings, each session's
// summary is the summary.json v1 wrote for it, field for field, laps included,
// but for ecu's named departures (asV1).
func TestEverySummaryIsV1s(t *testing.T) {
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			want := readSummaries(t, name)
			all := packets(t, name)
			started, ended := sessionsOf(run(all))
			if len(ended) != len(want) {
				t.Fatalf("%d sessions ended, v1 stored %d", len(ended), len(want))
			}
			for i, e := range ended {
				sum := e.GetSummary()
				if !sum.GetStartedAt().AsTime().Equal(started[i].GetStartedAt().AsTime()) {
					t.Errorf("session %d: the summary started at %v, session.started at %v",
						i+1, sum.GetStartedAt().AsTime(), started[i].GetStartedAt().AsTime())
				}
				w := v1AsCompared(want[i], roundedFixtures[name])
				// A held start, GT7's cars-on-track flag off on its first frame
				// (start.go): v1's clock read the verdict's frame, at most
				// maxHeldSecs later than the first held frame ecu dates it from.
				first := all[started[i].GetFirstSeq()-1]
				if got := msOf(sum.GetStartedAt()); !first.Flags.CarsOnTrack() &&
					w.StartedAt > got && w.StartedAt-got <= maxHeldSecs {
					w.StartedAt = got
				}
				if d := diffs(asV1(t, sum, want[i], roundedFixtures[name]), w); len(d) > 0 {
					t.Errorf("session %d:\n%s", i+1, strings.Join(d, "\n"))
				}
			}
		})
	}
}

// sessionEnds is each session.ended of a recording, in order.
func sessionEnds(t *testing.T, name string) []Output {
	t.Helper()
	var ends []Output
	for _, o := range replay(t, name) {
		if o.Ended != nil {
			ends = append(ends, o)
		}
	}
	return ends
}

// paddock#86, AC2: the first qualifying session ends on its restart, and the
// second on its quit, with the last position seen, as a quit's last frame has
// none.
func TestTheQualifyingEndsOnARestartThenAQuit(t *testing.T) {
	ends := sessionEnds(t, "real-quali-restart-quit-2026-10-14")
	if len(ends) != 2 || ends[0].Why != WhyRestart || ends[1].Why != WhyQuit {
		t.Fatalf("ends %v, want a restart and a quit", ends)
	}
	if p := ends[1].Ended.GetSummary().FinalPosition; p == nil || *p != 1 {
		t.Errorf("the quit's final position %v, want the last seen, 1", p)
	}
}

// paddock#86, AC3: SUGO's three races: the lap-1 restart's has no complete lap,
// and the last ends at the flag in P2.
func TestTheLastSugoRaceEndsAtTheFlagInP2(t *testing.T) {
	ends := sessionEnds(t, "real-nuc-sugo-restarts-2026-10-18")
	if len(ends) != 3 {
		t.Fatalf("%d sessions, want 3", len(ends))
	}
	if n := ends[1].Ended.GetSummary().GetLapsCompleted(); n != 0 {
		t.Errorf("the lap-1 restart's session completed %d laps", n)
	}
	last := ends[2]
	sum := last.Ended.GetSummary()
	if last.Why != WhyFinished || !sum.GetFinished() || sum.FinishingPosition == nil || sum.GetFinishingPosition() != 2 {
		t.Errorf("the last race ended %v, finished %t in P%d; want the flag in P2", last.Why, sum.GetFinished(), sum.GetFinishingPosition())
	}
}

// paddock#86, AC5: Suzuka's pit stop splits the race in two stints, each counted
// in fuel_burned_by_stint and each under a full tank: the refuel does not merge
// them, and fuel_burned is every drop of both.
func TestSuzukasTwoStintsAreBothCounted(t *testing.T) {
	ends := sessionEnds(t, "real-nuc-suzuka-pit-stop-2026-10-15")
	if len(ends) != 1 {
		t.Fatalf("%d sessions, want 1", len(ends))
	}
	sum := ends[0].Ended.GetSummary()
	stints := sum.GetFuelBurnedByStint()
	if sum.GetStints() != 2 || len(stints) != 2 {
		t.Fatalf("%d stints, %v burnt by stint; want 2 of each", sum.GetStints(), stints)
	}
	for i, x := range stints {
		if x <= 0 || x >= sum.GetFuelCapacity() {
			t.Errorf("stint %d burnt %.3f, want some and under the tank's %.0f", i+1, x, sum.GetFuelCapacity())
		}
	}
	if total := round3(stints[0] + stints[1]); math.Abs(total-sum.GetFuelBurned()) > 0.0015 {
		t.Errorf("the stints burnt %.3f, fuel_burned %.3f", total, sum.GetFuelBurned())
	}
}

// The running summary is kept all session, not built at the end: after each lap,
// it holds the laps published so far, the fuel burnt and the time driven as of
// that frame, with no ended_at yet, and session.ended carries that same summary.
func TestTheSummaryIsKeptAllSession(t *testing.T) {
	m := New(testIDs())
	var running, ended *eventsv1.SessionSummary
	var laps int
	var driven int32
	for _, r := range packets(t, "real-nuc-suzuka-pit-stop-2026-10-15") {
		out := m.Step(r)
		if m.session != nil {
			running = m.session.summary
		}
		endsHere := slices.ContainsFunc(out, func(o Output) bool { return o.Ended != nil })
		for _, o := range out {
			switch {
			case o.Ended != nil:
				ended = o.Ended.GetSummary()
			case o.Lap != nil && o.Lap.GetComplete():
				laps++
				if len(running.GetLaps()) != laps || running.GetLaps()[laps-1].GetLap() != o.Lap.GetLap() {
					t.Fatalf("after lap %d: the running summary has %d laps", o.Lap.GetLap(), len(running.GetLaps()))
				}
				if running.GetDrivingMs() <= driven || running.GetFuelBurned() <= 0 || running.GetCarName() == "" {
					t.Errorf("after lap %d: driving %d ms (was %d), burnt %.3f, car %q",
						o.Lap.GetLap(), running.GetDrivingMs(), driven, running.GetFuelBurned(), running.GetCarName())
				}
				driven = running.GetDrivingMs()
				if !endsHere && running.GetEndedAt() != nil {
					t.Errorf("after lap %d: ended_at set while open", o.Lap.GetLap())
				}
			}
		}
	}
	if laps != 6 || ended == nil || ended != running {
		t.Errorf("%d laps; session.ended's summary is the running one: %t", laps, ended == running)
	}
}

// paddock#86, R4: the car's name is v1's, from the table; a car missing from it
// is "car <code>", and no car has none.
func TestTheCarsName(t *testing.T) {
	for code, want := range map[int32]string{
		683:  "Honda Civic Si Group A version '87",
		3265: "Toyota 86 Gr.B Rally Car",
		9999: "car 9999",
		0:    "",
		-1:   "",
	} {
		if got := carName(code); got != want {
			t.Errorf("car %d: %q, want %q", code, got, want)
		}
	}
	if n := len(parseCars(carsCSV)); n != 590 {
		t.Errorf("%d cars, want v1's 590", n)
	}
}

// v1Cars is the sha256 of v1's src/those_who_run/data/cars.csv at v1.0.0.
const v1Cars = "a8bd34d02bf14647298bebfd51304abb56df515e851e07c7435f3efb649fd650"

// R4: the car table is v1's cars.csv at v1.0.0, byte for byte.
func TestTheCarTableIsV1s(t *testing.T) {
	if sum := sha256.Sum256([]byte(carsCSV)); hex.EncodeToString(sum[:]) != v1Cars {
		t.Errorf("cars.csv sha256 %x, v1's is %s", sum, v1Cars)
	}
}

// session.started carries the car's name and the tank's size.
func TestSessionStartedNamesTheCar(t *testing.T) {
	for _, o := range replay(t, "real-nuc-willow-springs-pit-stop-2026-10-17") {
		if s := o.Started; s != nil && (s.GetCarName() != "Toyota 86 Gr.B Rally Car" || s.GetFuelCapacity() <= 0) {
			t.Errorf("car %q, tank %.0f; want the Toyota 86 Gr.B Rally Car and its tank", s.GetCarName(), s.GetFuelCapacity())
		}
	}
}

// R3: a session that ends in a pause ended when the pause began, and its time
// driven leaves the pause out (v1's end_session); one that ends driving ended
// when the packet that ended it came.
func TestASessionEndedPausedEndedWhenThePauseBegan(t *testing.T) {
	// The session opens on frame 2 (t 1 s), the pause begins on frame 12 (t 11 s),
	// and the pause limit passes on frame 72 (t 71 s).
	m := New(testIDs())
	m.SetPauseLimit(60)
	var out []Output
	for _, r := range paused(61).all {
		out = append(out, m.Step(r)...)
	}
	sum := endedPaused(t, out).GetSummary()
	if !sum.GetStartedAt().AsTime().Equal(epoch.Add(time.Second)) || !sum.GetEndedAt().AsTime().Equal(epoch.Add(11*time.Second)) ||
		sum.GetDrivingMs() != 10000 || sum.GetPausedMs() != 60000 {
		t.Errorf("started %v, ended %v, %d ms driven, %d ms paused; want 1 s, 11 s, 10000 and 60000",
			sum.GetStartedAt().AsTime().Sub(epoch), sum.GetEndedAt().AsTime().Sub(epoch), sum.GetDrivingMs(), sum.GetPausedMs())
	}
	m = New(testIDs())
	for _, r := range paused(0).all {
		m.Step(r)
	}
	sum = endedPaused(t, m.EndOfInput()).GetSummary()
	if !sum.GetEndedAt().AsTime().Equal(epoch.Add(10*time.Second)) || sum.GetDrivingMs() != 9000 {
		t.Errorf("ended %v, %d ms driven; want 10 s and 9000", sum.GetEndedAt().AsTime().Sub(epoch), sum.GetDrivingMs())
	}
}

// A session continued after an ecu restart (paddock#76) keeps its first run's
// started_at, so one that then ends paused ended when the pause began, dated
// from this run's first frame (v1's start_wall), not from started_at.
func TestEndedPausedIsDatedFromThisRunsStart(t *testing.T) {
	m := New(testIDs())
	m.SetPauseLimit(60)
	var out []Output
	for i, r := range paused(61).all {
		out = append(out, m.Step(r)...)
		if i == 1 { // the session's first frame: it began 30 min before, in another run
			m.session.summary.StartedAt = timestamppb.New(epoch.Add(-30 * time.Minute))
		}
	}
	sum := endedPaused(t, out).GetSummary()
	if !sum.GetEndedAt().AsTime().Equal(epoch.Add(11 * time.Second)) {
		t.Errorf("ended %v, want 11 s: when the pause began", sum.GetEndedAt().AsTime().Sub(epoch))
	}
}
