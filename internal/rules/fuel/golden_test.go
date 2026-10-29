package fuel

import (
	"bufio"
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
	"sync"
	"testing"
	"time"

	"github.com/those-who-run/salsa"

	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
)

// The recordings are v1's 25: salsa's testdata/v1/, read from the salsa module
// the build already uses, and the ones internal/track keeps.
var recordings = []string{
	"real-n24h-race-lap1-positions-2026-10-14",
	"real-nuc-fuji-timed-2026-10-15",
	"real-nuc-gilles-villeneuve-pit-stop-2026-10-17",
	"real-nuc-gilles-villeneuve-race-2026-10-17",
	"real-nuc-laguna-seca-demo-2026-10-18",
	"real-nuc-laguna-seca-race-2026-10-15",
	"real-nuc-nordschleife-rolling-start-2026-10-15",
	"real-nuc-spa-pit-cut-quit-2026-10-18",
	"real-nuc-spa-time-trial-2026-10-18",
	"real-nuc-sugo-replay-2026-10-18",
	"real-nuc-sugo-restarts-2026-10-18",
	"real-nuc-sugo-timed-2026-10-15",
	"real-nuc-sugo-two-stops-2026-10-18",
	"real-nuc-suzuka-before-restart-2026-10-18",
	"real-nuc-suzuka-east-2026-10-15",
	"real-nuc-suzuka-joined-mid-race-2026-10-18",
	"real-nuc-suzuka-no-stop-2026-10-15",
	"real-nuc-suzuka-pit-stop-2026-10-15",
	"real-nuc-suzuka-timed-race-2026-10-16",
	"real-nuc-tokyo-east-race-2026-10-15",
	"real-nuc-tsukuba-brz-drift-2026-10-15",
	"real-nuc-unknown-track-2026-10-15",
	"real-nuc-willow-springs-pit-stop-2026-10-17",
	"real-quali-restart-quit-2026-10-14",
	"real-race-2laps-2026-10-14",
}

var (
	salsaOnce sync.Once
	salsaDir  string
	salsaErr  error
)

func salsaFixtures(tb testing.TB) string {
	tb.Helper()
	salsaOnce.Do(func() {
		// go test puts its own toolchain's go first on PATH.
		out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}",
			"github.com/those-who-run/salsa").Output()
		salsaDir, salsaErr = filepath.Join(strings.TrimSpace(string(out)), "testdata", "v1"), err
	})
	if salsaErr != nil {
		tb.Fatalf("finding salsa's testdata/v1: %v", salsaErr)
	}
	return salsaDir
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// packets is every packet of a recording, dated from epoch at its first frame.
func packets(tb testing.TB, name string) []race.Received {
	tb.Helper()
	path := filepath.Join("..", "..", "track", "testdata", "v1", name+".jsonl.gz")
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join(salsaFixtures(tb), name+".jsonl.gz")
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

// replayLines replays a recording through the machine and an engine running the
// fuel rule alone (only this package registers a rule here), with v1's
// defaults, and writes each session start and each callout as the golden does.
func replayLines(t *testing.T, name string) []string {
	t.Helper()
	engine, err := rules.New(rules.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.Rules(); !slices.Equal(got, []string{Name}) {
		t.Fatalf("the engine runs %v, want the fuel rule alone", got)
	}
	m := race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'})))
	sessions := map[string]int{}
	var lines []string
	for _, r := range packets(t, name) {
		for _, o := range m.Step(r) {
			if o.Started != nil {
				sessions[o.Session] = len(sessions) + 1
				lines = append(lines, fmt.Sprintf("session %d started", sessions[o.Session]))
			}
		}
		for _, o := range engine.Step(m.Context()) {
			c := o.Callout
			laps := int32(0)
			if c.GetSlots() != nil {
				laps = c.GetSlots().GetLaps()
			}
			hold := 0.0
			if c.GetHoldUntil() != nil {
				hold = c.GetHoldUntil().AsTime().Sub(c.GetCreatedAt().AsTime()).Seconds()
			}
			expiry := c.GetExpiresAt().AsTime().Sub(c.GetCreatedAt().AsTime()).Seconds()
			lines = append(lines, callout(sessions[c.GetSessionId()], int(c.GetAt()), c.GetKind(), laps,
				c.GetPriority(), hold, expiry, c.GetPitLane().String()))
		}
	}
	return lines
}

func callout(session, frame int, kind string, laps, priority int32, hold, expiry float64, lane string) string {
	return fmt.Sprintf("session %d frame %d: %s laps=%d p%d hold=%gs expiry=%gs %s",
		session, frame, kind, laps, priority, hold, expiry, lane)
}

// goldenLines is testdata/<name>.jsonl (make_golden.py there) in replayLines'
// form. v1 spoke every fuel callout, in the pit lane too (pit_lane speak).
func goldenLines(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".jsonl")) //nolint:gosec // the test's own golden
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	lanes := map[string]string{"spoken": "PIT_LANE_SPEAK"}
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var g struct {
			Event    string
			Session  int
			Frame    int
			Kind     string
			Slots    struct{ Laps int32 }
			Priority int32
			HoldSecs float64 `json:"hold_secs"`
			Expiry   float64 `json:"expiry_secs"`
			PitLane  string  `json:"pit_lane"`
		}
		if err := json.Unmarshal(sc.Bytes(), &g); err != nil {
			t.Fatal(err)
		}
		if g.Event == "session.started" {
			lines = append(lines, fmt.Sprintf("session %d started", g.Session))
			continue
		}
		lane, ok := lanes[g.PitLane]
		if !ok {
			lane = "v1 " + g.PitLane
		}
		lines = append(lines, callout(g.Session, g.Frame, g.Kind, g.Slots.Laps, g.Priority, g.HoldSecs, g.Expiry, lane))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// AC1–AC7: on every one of v1's 25 recordings, the fuel rule raises the callouts
// v1.0.0's FuelRule raised, frame for frame: the same kind, laps, priority, hold,
// expiry and pit lane, in each session v1 opened.
func TestTheFuelCalloutsAreV1sOnEveryRecording(t *testing.T) {
	for _, name := range recordings {
		t.Run(name, func(t *testing.T) {
			got, want := replayLines(t, name), goldenLines(t, name)
			if len(want) == 0 {
				t.Fatal("an empty golden")
			}
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}
