package position

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

// recordings are v1's 25 recordings (internal/race's v1Recordings).
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

// goldenLine is a line of a golden (testdata/make_golden.py).
type goldenLine struct {
	Event            string   `json:"event"`
	Session          int      `json:"session"`
	Frame            uint32   `json:"frame"`
	Kind             string   `json:"kind"`
	Position         int32    `json:"position"`
	PreviousPosition int32    `json:"previous_position"`
	Priority         int32    `json:"priority"`
	HoldSecs         float64  `json:"hold_secs"`
	MaxAgeSecs       *float64 `json:"max_age_secs"`
	PitLane          string   `json:"pit_lane"`
	Outcome          string   `json:"outcome"`
}

func (l goldenLine) String() string {
	return fmt.Sprintf("session %d frame %d %s P%d to P%d", l.Session, l.Frame, l.Kind, l.PreviousPosition, l.Position)
}

// published is a golden's calls ecu publishes, as lines: all but those raised and
// dropped in the pit lane. One still held at a pit entry (dropped) is published,
// with its hold, for radio to drop (contracts' PitLane).
func published(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.Lines(string(data)) {
		var l goldenLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		if l.Event != "callout" || l.Outcome == "unpublished" {
			continue
		}
		if l.Priority != 12 || l.HoldSecs != 1 || l.MaxAgeSecs != nil || l.PitLane != "drop" {
			t.Fatalf("%s: not v1's priority 12, hold 1 s, no max age and pit_lane drop", l)
		}
		if l.Outcome != "spoken" && l.Outcome != "dropped" {
			t.Fatalf("%s: outcome %s", l, l.Outcome)
		}
		lines = append(lines, l.String())
	}
	return lines
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fixture is a recording's path: internal/track's copy, else salsa's testdata/v1,
// as internal/race's tests read them.
func fixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "track", "testdata", "v1", name+".jsonl.gz")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	// go test puts its own toolchain's go first on PATH.
	out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}",
		"github.com/those-who-run/salsa").Output()
	if err != nil {
		t.Fatalf("finding salsa's testdata/v1: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "testdata", "v1", name+".jsonl.gz")
}

// replay runs a recording through the machine and an engine with v1's defaults,
// and returns the rule's callouts as a golden's lines, checking what the engine
// stamped on each.
func replay(t *testing.T, name string) []string {
	t.Helper()
	src, err := salsa.OpenRecording(fixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	m := race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'})))
	e, err := rules.New(rules.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sessions := map[string]int{}
	var lines []string
	t0 := math.NaN()
	for {
		r, err := src.Next(t.Context())
		if errors.Is(err, io.EOF) {
			return lines
		}
		if err != nil {
			t.Fatal(err)
		}
		if math.IsNaN(t0) {
			t0 = r.T
		}
		at := epoch.Add(time.Duration(math.Round((r.T - t0) * float64(time.Second))))
		for _, o := range m.Step(race.Received{Received: r, Time: at}) {
			if o.Started != nil {
				sessions[o.Session] = len(sessions) + 1
			}
		}
		for _, o := range e.Step(m.Context()) {
			c := o.Callout
			if c == nil || c.GetRule() != Name {
				continue
			}
			created := c.GetCreatedAt().AsTime()
			if c.GetPriority() != 12 || c.GetHoldUntil().AsTime().Sub(created) != time.Second ||
				c.GetExpiresAt().AsTime().Sub(created) != 8*time.Second || c.GetPitLane().String() != "PIT_LANE_DROP" {
				t.Errorf("callout %v: not priority 12, a 1 s hold, an 8 s life and pit_lane drop", c)
			}
			s := c.GetSlots()
			lines = append(lines, goldenLine{
				Session: sessions[c.GetSessionId()], Frame: c.GetAt(), Kind: c.GetKind(),
				Position: s.GetPosition(), PreviousPosition: s.GetPreviousPosition(),
			}.String())
		}
	}
}

// R1–R7, AC1–AC5: on every one of v1's recordings, the rule raises v1's
// PositionRule's calls (rules.example.yaml's 15 s grace, 10 s between calls and
// 3 s settle), on the same frame of the same session, with the kind contracts'
// table names and the places as slots, and the engine stamps v1's priority, hold
// and pit lane. v1's one call still held at a pit entry (SUGO's two stops, frame
// 421) is published, for radio to drop. The recordings v1 opens no session on are
// held to v1's engine taking every start for a drive (make_golden.py --drive), as
// ecu does until paddock#74.
func TestTheCallsAreV1sOnEveryRecording(t *testing.T) {
	total := 0
	for _, name := range recordings {
		t.Run(name, func(t *testing.T) {
			want := published(t, name)
			total += len(want)
			if got := replay(t, name); !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
	if total != 109 {
		t.Errorf("%d calls in the goldens, want v1's 109", total)
	}
}
