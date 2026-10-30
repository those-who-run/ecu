package status

import (
	"context"
	"errors"
	"io"
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

// moduleDir is a module's directory, from the build's go.mod, so the tests read
// salsa's recordings and contracts' v1 golden from the modules already used.
func moduleDir(tb testing.TB, module string) string {
	tb.Helper()
	// go test puts its own toolchain's go first on PATH.
	out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}", module).Output() //nolint:gosec // a module the test names
	if err != nil {
		tb.Fatalf("finding %s: %v", module, err)
	}
	return strings.TrimSpace(string(out))
}

// recording is a v1 recording: one of salsa's testdata/v1/, or one of the 18
// internal/track keeps.
func recording(tb testing.TB, name string) []salsa.Received {
	tb.Helper()
	path := filepath.Join("..", "track", "testdata", "v1", name+".jsonl.gz")
	if name == sugoTwoStops {
		path = filepath.Join(moduleDir(tb, "github.com/those-who-run/salsa"), "testdata", "v1", name+".jsonl.gz")
	}
	src, err := salsa.OpenRecording(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	var all []salsa.Received
	for {
		r, err := src.Next(tb.Context())
		if errors.Is(err, io.EOF) {
			return all
		}
		if err != nil {
			tb.Fatal(err)
		}
		all = append(all, r)
	}
}

const (
	gilles       = "real-nuc-gilles-villeneuve-race-2026-10-17"
	sugoTwoStops = "real-nuc-sugo-two-stops-2026-10-18"
	sugoReplay   = "real-nuc-sugo-replay-2026-10-18"
)

// epoch is the wall-clock time of a recording's first frame: later ones are it
// plus their t, and the test clock follows them.
var epoch = time.Date(2026, 10, 29, 13, 52, 4, 125_000_000, time.UTC)

// clock is a wall clock the test moves.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *clock) add(d time.Duration) { c.set(c.Now().Add(d)) }

// rig is a race state and a tracker on a test clock, which each packet moves
// to its own time, as live's are taken.
type rig struct {
	m     *race.Machine
	t     *Tracker
	clock *clock
	t0    float64
}

func newRig(cfg Config) *rig {
	c := &clock{now: epoch}
	cfg.Now = c.Now
	if cfg.TelemetryTimeout == 0 {
		cfg.TelemetryTimeout = 3 * time.Second
	}
	if cfg.Src == "" {
		cfg.Src = "live"
	}
	ids := race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'}))
	return &rig{m: race.New(ids), t: New(cfg), clock: c, t0: -1}
}

// step takes one packet and says what it gave.
func (r *rig) step(p salsa.Received) []race.Output {
	if r.t0 < 0 {
		r.t0 = p.T
	}
	at := epoch.Add(time.Duration((p.T - r.t0) * float64(time.Second)))
	r.clock.set(at)
	outs := r.m.Step(race.Received{Received: p, Time: at})
	r.t.Observe(r.m.Live(), outs)
	return outs
}

// fakeKV keeps what is written to it, and fails while fail is set.
type fakeKV struct {
	mu     sync.Mutex
	values map[string][]byte
	ops    []string // "put key" or "delete key", in order
	fail   error
}

func newFakeKV() *fakeKV { return &fakeKV{values: map[string][]byte{}} }

func (k *fakeKV) Put(_ context.Context, key string, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail != nil {
		return k.fail
	}
	k.values[key] = append([]byte(nil), value...)
	k.ops = append(k.ops, "put "+key)
	return nil
}

func (k *fakeKV) Delete(_ context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail != nil {
		return k.fail
	}
	delete(k.values, key)
	k.ops = append(k.ops, "delete "+key)
	return nil
}

// take is the ops since the last take.
func (k *fakeKV) take() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	ops := k.ops
	k.ops = nil
	return ops
}
