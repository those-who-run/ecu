package race

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/those-who-run/salsa"
)

// The v1 recordings the tests replay are salsa's testdata/v1/, read from the salsa
// module the build already uses, so ecu keeps no copy of them.
var (
	fixturesOnce sync.Once
	fixturesDir  string
	fixturesErr  error
)

func fixtures(tb testing.TB) string {
	tb.Helper()
	fixturesOnce.Do(func() {
		// go test puts its own toolchain's go first on PATH.
		out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}",
			"github.com/those-who-run/salsa").Output()
		if err != nil {
			fixturesErr = err
			return
		}
		fixturesDir = filepath.Join(strings.TrimSpace(string(out)), "testdata", "v1")
	})
	if fixturesErr != nil {
		tb.Fatalf("finding salsa's testdata/v1: %v", fixturesErr)
	}
	return fixturesDir
}

// epoch is the wall-clock time of each recording's first frame; later frames are
// that plus their t, as v1's replay --clock dates them.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// testIDs is the fixed clock and seeded entropy the tests mint ids with.
func testIDs() IDs {
	return NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'}))
}

// trackFixtures are the v1 recordings salsa doesn't carry, which internal/track
// keeps (its testdata/v1/README.md says where each comes from).
var trackFixtures = filepath.Join("..", "track", "testdata", "v1")

// liveTrackFixtures are internal/track's recordings cut from live outings (its
// testdata/live/make_fixture.py says where each comes from, paddock#81).
var liveTrackFixtures = filepath.Join("..", "track", "testdata", "live")

// pauseFixtures are the recordings cut for the pauses (paddock#77):
// testdata/pause/make_fixture.py says how.
var pauseFixtures = filepath.Join("testdata", "pause")

// startFixtures are the start check's own recordings (testdata/start/make_fixture.py
// says where each comes from, paddock#74).
var startFixtures = filepath.Join("testdata", "start")

// continueFixtures are the recordings cut for continuing after a restart
// (paddock#76): testdata/continue/make_fixture.py says how.
var continueFixtures = filepath.Join("testdata", "continue")

// sessionFixtures are live recordings of one session of each kind (paddock#145):
// testdata/sessions/make_fixture.py says where each comes from.
var sessionFixtures = filepath.Join("testdata", "sessions")

// packets is every packet of a recording, with its wall-clock time: one of
// salsa's, else one of internal/track's (v1's or live), the start check's, the
// pauses', the restarts' or the sessions'.
func packets(tb testing.TB, name string) []Received {
	tb.Helper()
	path := filepath.Join(fixtures(tb), name+".jsonl.gz")
	for _, dir := range []string{trackFixtures, liveTrackFixtures, startFixtures, pauseFixtures, continueFixtures, sessionFixtures} {
		if p := filepath.Join(dir, name+".jsonl.gz"); fileExists(p) {
			path = p
		}
	}
	src, err := salsa.OpenRecording(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	var all []Received
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
		all = append(all, Received{Received: r, Time: at})
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// replay runs a recording through a new machine, then closes it, as ecu does when
// the recording ends.
func replay(tb testing.TB, name string) []Output {
	tb.Helper()
	return run(packets(tb, name))
}

func run(all []Received) []Output {
	m := New(testIDs())
	var out []Output
	for _, r := range all {
		out = append(out, m.Step(r)...)
	}
	return append(out, m.EndOfInput()...)
}

// frozen is the seqs (from 1) of the frames v1's PauseClock (pause.py at v1.0.0)
// reads as GT7's frozen pause stream and turns into paused frames: an IN_RACE frame
// with the car moving, identical in what changes while driving to the frames of the
// last 1 s or more. It is a second reading of pause.go's, kept apart from it, which
// TestTheFrozenPauseIsInLapTwoOfTheFirstSugoRace holds the machine to.
func frozen(all []Received) map[uint32]bool {
	type still struct {
		pos, vel         salsa.Vec3
		speed, rpm, fuel float32
		throttle, brake  uint8
		lap              int16
		rot, angVel      salsa.Vec3
	}
	out := map[uint32]bool{}
	var key still
	var runT float64
	for i, r := range all {
		p := &r.Packet
		k := still{p.Position, p.Velocity, p.Speed, p.EngineRPM, p.FuelLevel, p.Throttle, p.Brake,
			p.CurrentLap, p.Rotation, p.AngularVelocity}
		if i == 0 || k != key {
			key, runT = k, r.T
		}
		phase := derivePhase(int32(p.CurrentLap), int32(p.TotalLaps), p.Flags.Paused(), p.Flags.Loading())
		if phase == phaseInRace && p.SpeedKPH() > 1 && r.T-runT >= 1 {
			out[uint32(i+1)] = true //nolint:gosec // a recording's frame count fits
		}
	}
	return out
}
