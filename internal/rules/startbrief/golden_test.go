package startbrief

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

	"google.golang.org/protobuf/encoding/protojson"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/salsa"
)

// recordings are v1's 25 recordings (testdata/make_golden.py's goldens).
var recordings = []string{
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

// epoch is the wall-clock time of each recording's first frame.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// packets is every packet of a recording: salsa's testdata/v1, else
// internal/track's, as internal/race's tests read them.
func packets(t *testing.T, name string) []race.Received {
	t.Helper()
	path := filepath.Join("..", "..", "track", "testdata", "v1", name+".jsonl.gz")
	if _, err := os.Stat(path); err != nil {
		out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}",
			"github.com/those-who-run/salsa").Output()
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(strings.TrimSpace(string(out)), "testdata", "v1", name+".jsonl.gz")
	}
	src, err := salsa.OpenRecording(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	var all []race.Received
	for {
		r, err := src.Next(t.Context())
		if errors.Is(err, io.EOF) {
			return all
		}
		if err != nil {
			t.Fatal(err)
		}
		t0 := r.T
		if len(all) > 0 {
			t0 = all[0].T
		}
		at := epoch.Add(time.Duration(math.Round((r.T - t0) * float64(time.Second))))
		all = append(all, race.Received{Received: r, Time: at})
	}
}

// goldenLine is a callout line of a golden.
type goldenLine struct {
	Event      string          `json:"event"`
	Session    int             `json:"session"`
	Frame      uint32          `json:"frame"`
	Kind       string          `json:"kind"`
	Slots      json.RawMessage `json:"slots"`
	Priority   int32           `json:"priority"`
	HoldSecs   float64         `json:"hold_secs"`
	ExpirySecs float64         `json:"expiry_secs"`
	PitLane    string          `json:"pit_lane"`
	Outcome    string          `json:"outcome"`
}

// show is a callout as the test compares it.
func show(session int, frame uint32, kind string, slots *eventsv1.CalloutSlots, priority int32,
	hold, expiry float64, pitLane, outcome string,
) string {
	s, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(slots)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("session %d frame %d %s %s priority %d hold %.1f expiry %.1f %s %s",
		session, frame, kind, s, priority, hold, expiry, pitLane, outcome)
}

// pitLanes are the golden's pit_lane values, as the contracts' PitLane.
var pitLanes = map[string]eventsv1.PitLane{
	"speak": eventsv1.PitLane_PIT_LANE_SPEAK, "hold": eventsv1.PitLane_PIT_LANE_HOLD, "drop": eventsv1.PitLane_PIT_LANE_DROP,
}

func readGolden(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.Lines(string(data)) {
		var g goldenLine
		if err := json.Unmarshal([]byte(line), &g); err != nil {
			t.Fatal(err)
		}
		if g.Event != "callout" {
			continue
		}
		slots := &eventsv1.CalloutSlots{}
		if err := protojson.Unmarshal(g.Slots, slots); err != nil {
			t.Fatal(err)
		}
		out = append(out, show(g.Session, g.Frame, g.Kind, slots, g.Priority, g.HoldSecs, g.ExpirySecs,
			pitLanes[g.PitLane].String(), g.Outcome))
	}
	return out
}

// replay runs a recording through the machine and an engine of this rule alone,
// with v1's defaults, and shows its callouts.
func replay(t *testing.T, name string) []string {
	t.Helper()
	cfg := rules.DefaultConfig()
	for n, s := range cfg.Rules {
		s.Enabled = n == Name
	}
	e, err := rules.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'})))
	sessions := map[string]int{}
	times := map[uint32]time.Time{}
	var out []string
	for _, r := range packets(t, name) {
		for _, o := range m.Step(r) {
			if o.Started != nil {
				sessions[o.Session] = len(sessions) + 1
			}
		}
		ctx := m.Context()
		times[ctx.Cur.Seq] = ctx.Time
		for _, o := range e.Step(ctx) {
			c := o.Callout
			created, expires := c.GetCreatedAt().AsTime(), c.GetExpiresAt().AsTime()
			hold := 0.0
			if c.HoldUntil != nil {
				hold = c.GetHoldUntil().AsTime().Sub(created).Seconds()
			}
			outcome := "spoken"
			if !created.Equal(times[c.GetAt()]) {
				outcome = "held"
			}
			out = append(out, show(sessions[c.GetSessionId()], c.GetAt(), c.GetKind(), c.GetSlots(), c.GetPriority(),
				hold, expires.Sub(created).Seconds(), c.GetPitLane().String(), outcome))
			if c.GetRule() != Name {
				t.Errorf("a callout of %s", c.GetRule())
			}
		}
	}
	return out
}

// R1 to R6, AC1 to AC5, AC7: on every one of v1's recordings, the rule raises
// v1's callouts, on the same frame, with the kind and slots contracts v0.2.3
// names for v1's phrases, v1's priority, hold, expiry and pit lane, and what
// v1's pit lane did with them. The recordings v1 opens no session on (GT7's
// replays and demo races) are held to v1's engine taking every start for a
// drive (make_golden.py --drive), as ecu does until paddock#74.
func TestTheStartBriefIsV1sOnEveryRecording(t *testing.T) {
	for _, name := range recordings {
		t.Run(name, func(t *testing.T) {
			want := readGolden(t, name)
			if len(want) == 0 {
				t.Fatal("an empty golden")
			}
			if got := replay(t, name); !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}
