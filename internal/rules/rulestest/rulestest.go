// Package rulestest replays v1's recordings through the race state and the rule
// engine, for the tests of internal/rules and of each rule. Only tests import it.
package rulestest

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
	"github.com/those-who-run/salsa"
)

// Epoch is the wall-clock time of each recording's first frame; later frames are
// that plus their t, as v1's replay --clock dates them.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// IDs are the fixed clock and seeded entropy the tests mint ids with: the same
// run id on every replay.
func IDs() race.IDs {
	return race.NewIDs(func() time.Time { return Epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'}))
}

var (
	salsaOnce sync.Once
	salsaDir  string
	salsaErr  error
)

// Recording is the path of one of v1's recordings: salsa's testdata/v1, else
// internal/track's; or of the start check's own, internal/race's testdata/start
// (paddock#74).
func Recording(tb testing.TB, name string) string {
	tb.Helper()
	_, here, _, _ := runtime.Caller(0)
	internal := filepath.Join(filepath.Dir(here), "..", "..")
	for _, dir := range []string{filepath.Join("track", "testdata", "v1"), filepath.Join("race", "testdata", "start")} {
		path := filepath.Join(internal, dir, name+".jsonl.gz")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	salsaOnce.Do(func() {
		out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}",
			"github.com/those-who-run/salsa").Output()
		salsaDir, salsaErr = filepath.Join(strings.TrimSpace(string(out)), "testdata", "v1"), err
	})
	if salsaErr != nil {
		tb.Fatalf("finding salsa's testdata/v1: %v", salsaErr)
	}
	return filepath.Join(salsaDir, name+".jsonl.gz")
}

// Packets is every packet of a recording, with its wall-clock time.
func Packets(tb testing.TB, name string) []race.Received {
	tb.Helper()
	src, err := salsa.OpenRecording(Recording(tb, name))
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
		at := Epoch.Add(time.Duration(math.Round((r.T - t0) * float64(time.Second))))
		all = append(all, race.Received{Received: r, Time: at})
	}
}

// Run steps each packet through a new machine and e, as ecu does, and returns
// everything published, frames included, with the machine's EndOfInput at the
// end.
func Run(all []race.Received, e *rules.Engine) []race.Output {
	m := race.New(IDs())
	m.Rules(e.Step)
	var out []race.Output
	for _, r := range all {
		out = append(out, m.Step(r)...)
	}
	return append(out, m.EndOfInput()...)
}

// Replay is Run over a recording, with an engine of v1's defaults and every
// registered rule.
func Replay(tb testing.TB, name string) []race.Output {
	tb.Helper()
	e, err := rules.New(rules.DefaultConfig(), nil)
	if err != nil {
		tb.Fatal(err)
	}
	return Run(Packets(tb, name), e)
}

// Discarded are the recordings of a GT7 replay or demo race, which v1's start
// check and ecu's drop (paddock#74): no session, so no callout and an empty
// golden.
var Discarded = map[string]bool{
	"real-nuc-laguna-seca-demo-2026-10-18":           true,
	"real-nuc-laguna-seca-race-2026-10-15":           true,
	"real-nuc-nordschleife-rolling-start-2026-10-15": true,
	"real-nuc-sugo-replay-2026-10-18":                true,
	"real-nuc-tokyo-east-race-2026-10-15":            true,
	"real-nuc-tsukuba-brz-drift-2026-10-15":          true,
}

// Recordings are v1's 25 recordings, the goldens' names.
var Recordings = []string{
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
