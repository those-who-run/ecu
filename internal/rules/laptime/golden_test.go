package laptime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/salsa"

	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
)

// v1Recordings are v1's 25 recordings, as internal/race's tests name them.
var v1Recordings = []string{
	"real-race-2laps-2026-10-14",
	"real-quali-restart-quit-2026-10-14",
	"real-n24h-race-lap1-positions-2026-10-14",
	"real-nuc-unknown-track-2026-10-15",
	"real-nuc-suzuka-east-2026-10-15",
	"real-nuc-suzuka-no-stop-2026-10-15",
	"real-nuc-suzuka-pit-stop-2026-10-15",
	"real-nuc-sugo-timed-2026-10-15",
	"real-nuc-fuji-timed-2026-10-15",
	"real-nuc-tokyo-east-race-2026-10-15",
	"real-nuc-nordschleife-rolling-start-2026-10-15",
	"real-nuc-laguna-seca-race-2026-10-15",
	"real-nuc-tsukuba-brz-drift-2026-10-15",
	"real-nuc-suzuka-timed-race-2026-10-16",
	"real-nuc-willow-springs-pit-stop-2026-10-17",
	"real-nuc-gilles-villeneuve-race-2026-10-17",
	"real-nuc-gilles-villeneuve-pit-stop-2026-10-17",
	"real-nuc-sugo-two-stops-2026-10-18",
	"real-nuc-sugo-replay-2026-10-18",
	"real-nuc-laguna-seca-demo-2026-10-18",
	"real-nuc-sugo-restarts-2026-10-18",
	"real-nuc-suzuka-joined-mid-race-2026-10-18",
	"real-nuc-suzuka-before-restart-2026-10-18",
	"real-nuc-spa-pit-cut-quit-2026-10-18",
	"real-nuc-spa-time-trial-2026-10-18",
}

// accepted are v1's calls ecu doesn't make, by recording and frame, each with the
// decision behind it.
var accepted = map[string][]uint32{
	// GT7's counter jumps from 4 to 13 on the way to the results, and v1 called
	// "lap twelve" with no lap closed behind it: paddock#83's decision (the parity
	// diff maps it).
	"real-nuc-sugo-replay-2026-10-18": {864},
}

// goldenLine is a line of a golden (testdata/make_golden.py).
type goldenLine struct {
	Event    string `json:"event"`
	Session  int    `json:"session"`
	Frame    uint32 `json:"frame"`
	Kind     string `json:"kind"`
	Priority int32  `json:"priority"`
	HoldMs   int64  `json:"hold_ms"`
	ExpiryMs int64  `json:"expiry_ms"`
	PitLane  string `json:"pit_lane"`
	Outcome  string `json:"outcome"`
	Released uint32 `json:"released"`
	Slots    struct {
		Lap       int32  `json:"lap"`
		LapTimeMs int32  `json:"lap_time_ms"`
		DeltaMs   *int32 `json:"delta_ms"`
	} `json:"slots"`
}

// show is a callout as a line to compare: the session, the frame raised on, the
// frame published on when it was held in the pit lane, and the callout.
func show(session int, at, released uint32, kind string, priority int32, lap, ms int32, delta *int32,
	holdMs, expiryMs int64, pitLane string) string {
	d := "-"
	if delta != nil {
		d = fmt.Sprint(*delta)
	}
	held := ""
	if released != 0 {
		held = fmt.Sprintf(" held to %d", released)
	}
	return fmt.Sprintf("session %d frame %d%s: %s p%d lap %d %d delta %s hold %d expiry %d %s",
		session, at, held, kind, priority, lap, ms, d, holdMs, expiryMs, pitLane)
}

func readGolden(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.Lines(string(data)) {
		var l goldenLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		if l.Event != "callout" || slices.Contains(accepted[name], l.Frame) {
			continue
		}
		if l.Outcome != "spoken" && l.Outcome != "held" {
			t.Fatalf("frame %d: an outcome the test doesn't know: %s", l.Frame, l.Outcome)
		}
		out = append(out, show(l.Session, l.Frame, l.Released, l.Kind, l.Priority, l.Slots.Lap,
			l.Slots.LapTimeMs, l.Slots.DeltaMs, l.HoldMs, l.ExpiryMs, l.PitLane))
	}
	return out
}

// pitLanes are the contracts' PitLane as v1 named it.
var pitLanes = map[string]string{"PIT_LANE_SPEAK": "speak", "PIT_LANE_HOLD": "hold", "PIT_LANE_DROP": "drop"}

// replayed is the rule's callouts on a recording, through the race state and the
// engine with v1's defaults, as lines to compare.
func replayed(t *testing.T, name string) []string {
	t.Helper()
	engine, err := rules.New(rules.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.Rules(); !slices.Equal(got, []string{Name}) {
		t.Fatalf("the engine runs %v, want only %s", got, Name)
	}
	m := race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'})))
	sessions := map[string]int{}
	var out []string
	for _, r := range packets(t, name) {
		for _, o := range m.Step(r) {
			if o.Started != nil {
				sessions[o.Session] = len(sessions) + 1
			}
		}
		ctx := m.Context()
		for _, o := range engine.Step(ctx) {
			c := o.Callout
			var released uint32
			if c.GetAt() != ctx.Cur.Seq {
				released = ctx.Cur.Seq
			}
			var hold int64
			if c.GetHoldUntil() != nil {
				hold = c.GetHoldUntil().AsTime().Sub(c.GetCreatedAt().AsTime()).Milliseconds()
			}
			expiry := c.GetExpiresAt().AsTime().Sub(c.GetCreatedAt().AsTime()).Milliseconds()
			s := c.GetSlots()
			out = append(out, show(sessions[c.GetSessionId()], c.GetAt(), released, c.GetKind(), c.GetPriority(),
				s.GetLap(), s.GetLapTimeMs(), s.DeltaMs, hold, expiry, pitLanes[c.GetPitLane().String()]))
		}
	}
	m.EndOfInput()
	return out
}

// R1 to R8, AC1 to AC4, AC6: on every one of v1's recordings the rule raises
// v1's callouts frame for frame, with v1's kind (contracts' table), slots,
// priority, hold, expiry and pit lane, held in the lane to the frame v1 released
// them, but for the accepted differences.
func TestEveryLapCallIsV1sOnEveryRecording(t *testing.T) {
	for _, name := range v1Recordings {
		t.Run(name, func(t *testing.T) {
			got, want := replayed(t, name), readGolden(t, name)
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// AC4: at Willow Springs the in-lap, posted on a loading frame in the pit cut
// (frame 204), is still called, as v1 called it.
func TestTheWillowSpringsInLapIsCalled(t *testing.T) {
	got := replayed(t, "real-nuc-willow-springs-pit-stop-2026-10-17")
	if !slices.ContainsFunc(got, func(l string) bool {
		return strings.HasPrefix(l, "session 1 frame 204: "+KindInLap+" p20 lap 6 96562 ")
	}) {
		t.Errorf("no call of the in-lap, lap 6, on frame 204 in\n%s", strings.Join(got, "\n"))
	}
}

// The recordings, as internal/race's tests read them: salsa's testdata/v1, else
// internal/track's.

// epoch is the wall-clock time of each recording's first frame; later frames are
// that plus their t.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func packets(tb testing.TB, name string) []race.Received {
	tb.Helper()
	path := filepath.Join("..", "..", "track", "testdata", "v1", name+".jsonl.gz")
	if _, err := os.Stat(path); err != nil {
		// go test puts its own toolchain's go first on PATH.
		dir, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}",
			"github.com/those-who-run/salsa").Output()
		if err != nil {
			tb.Fatal(err)
		}
		path = filepath.Join(strings.TrimSpace(string(dir)), "testdata", "v1", name+".jsonl.gz")
	}
	src, err := salsa.OpenRecording(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	var all []race.Received
	for {
		r, err := src.Next(tb.Context())
		if errors.Is(err, io.EOF) {
			return all
		}
		if err != nil {
			tb.Fatal(err)
		}
		t0 := r.T
		if len(all) > 0 {
			t0 = all[0].T
		}
		at := epoch.Add(time.Duration(math.Round((r.T - t0) * float64(time.Second))))
		all = append(all, race.Received{Received: r, Time: at})
	}
}
