package bus

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/salsa"
)

// The recording the acceptance criteria name: a six-lap race at Suzuka East with a
// stop, from salsa's testdata/v1/, read from the salsa module the build already
// uses, as internal/race's tests read it.
const suzuka = "real-nuc-suzuka-pit-stop-2026-10-15"

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

// epoch is the wall-clock time of the recording's first frame, with nanoseconds,
// so a time that lost them on the way would show; later frames are that plus their
// t.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)

// packets is every packet of a recording, with its wall-clock time.
func packets(tb testing.TB, name string) []race.Received {
	tb.Helper()
	src, err := salsa.OpenRecording(filepath.Join(fixtures(tb), name+".jsonl.gz"))
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

// newMachine is a race state with fixed ids, so each run gives the same ids.
func newMachine() *race.Machine {
	return race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'b', 'u', 's'})))
}

// step runs packets through m, publishing each step's outputs as ecu does, and
// returns them.
func step(m *race.Machine, pub *Publisher, all []race.Received) []race.Output {
	var out []race.Output
	for _, r := range all {
		o := m.Step(r)
		pub.Publish(o)
		out = append(out, o...)
	}
	return out
}

// logs is a logger's output, safe to read while the publisher's handlers write.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logs) logger() *slog.Logger { return slog.New(slog.NewJSONHandler(l, nil)) }
