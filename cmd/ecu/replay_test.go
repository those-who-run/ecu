package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/contracts/ce"
	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/bus/bustest"
	"github.com/those-who-run/ecu/internal/record"
	"github.com/those-who-run/salsa"
)

// replayed is one run of ecu replay.
type replayed struct {
	code           int
	stdout, stderr string
	took           time.Duration
}

func runReplay(t *testing.T, stdin []byte, args ...string) replayed {
	t.Helper()
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := replay(t.Context(), args, env(nil), bytes.NewReader(stdin), &stdout, &stderr)
	return replayed{code, stdout.String(), stderr.String(), time.Since(start)}
}

// synthetic writes a v1 recording of the first n packets of the Suzuka recording,
// the i-th at at(i), or at its own time when at is nil, and returns its path.
func synthetic(t *testing.T, n int, at func(i int) float64) string {
	t.Helper()
	in := readAll(t, openRecording(t, recordingPath(t, suzuka)))
	var buf []byte
	for i := range n {
		if at == nil {
			buf, _ = record.AppendV1(buf, in[i].T, &in[i].Packet)
		} else {
			buf, _ = record.AppendV1(buf, at(i), &in[i].Packet)
		}
	}
	path := filepath.Join(t.TempDir(), "synthetic.jsonl")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ceTime is a stored message's ce-time.
func ceTime(t *testing.T, m bustest.Msg) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, ce.Header(m.Header).Get(ce.TimeHeader))
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// R1 and R2 of paddock#35, at --speed max: a v1 recording replayed against a real
// server puts every frame in TELEMETRY, none dropped for the publisher's full
// hand-over (the back-pressure), and in EVENTS the events the race state gives for the same packets,
// in order. The first frame is dated 2026-01-01T00:00:00Z, with no --clock.
func TestAReplayAtMaxPublishesEveryFrameAndEvent(t *testing.T) {
	srv := bustest.WithStreams(t)
	// Room for one frame and one event, so the replay waits on the hand-over
	// often; internal/bus's TestPublishWaitDropsNoFrame pins the waiting itself.
	replayOptions = bus.Options{FrameBuffer: 1, QueueSize: 1}
	t.Cleanup(func() { replayOptions = bus.Options{} })
	path := recordingPath(t, suzuka)
	got := runReplay(t, nil, "--speed", "max", "--src", "parity", "--nats", srv.URL(), path)
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q, log %s", got.code, got.stderr, got.stdout)
	}

	in := readAll(t, openRecording(t, path))
	want := &fakePublisher{}
	_ = drive(t.Context(), &slice{all: in}, func(salsa.Received) time.Time { return time.Unix(0, 0) }, machine(), want, nil)
	var kinds []string
	for _, o := range want.outs {
		if o.Frame == nil {
			kind := o.Event.ID[strings.Index(o.Event.ID, ":")+1:]
			why := ""
			if o.Ended != nil {
				why = o.Why.Reason().String()
			}
			kinds = append(kinds, kind+" "+why)
		}
	}

	if n := srv.Count("TELEMETRY"); n != uint64(len(in)) {
		t.Errorf("TELEMETRY holds %d frames, want every one of the %d", n, len(in))
	}
	stored := srv.Read("EVENTS", 0)
	var gotKinds []string
	for _, m := range stored {
		id := ce.Header(m.Header).Get(ce.IDHeader) // parity:<sid>:<area>.<what>:<n>
		kind := id[strings.Index(id[len("parity:"):], ":")+len("parity:")+1:]
		why := ""
		if strings.HasSuffix(m.Subject, ".session.ended") {
			var ended struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(m.Data, &ended); err != nil {
				t.Fatal(err)
			}
			why = ended.Reason
		}
		gotKinds = append(gotKinds, kind+" "+why)
	}
	if len(gotKinds) != len(kinds) || len(kinds) < 8 {
		t.Fatalf("EVENTS holds %d events, want %d:\n%v\n%v", len(gotKinds), len(kinds), gotKinds, kinds)
	}
	for i := range kinds {
		if gotKinds[i] != kinds[i] {
			t.Errorf("event %d is %q, want %q", i, gotKinds[i], kinds[i])
		}
	}

	first := srv.Read("TELEMETRY", 1)[0]
	if at := ceTime(t, first); !at.Equal(defaultClock) {
		t.Errorf("the first frame is at %v, want %v", at, defaultClock)
	}
	for _, line := range []string{`"msg":"replaying"`, `"dropped_frames":0`, `"dropped_events":0,"final":true`} {
		if !strings.Contains(got.stdout, line) {
			t.Errorf("log has no %s: %s", line, got.stdout)
		}
	}
}

// R4 of paddock#35, as restored on 2026-10-26: --clock dates the first frame, and
// later frames keep their recorded spacing. A recording read from standard input
// replays as a file does, under replay-stdin-<suffix>.
func TestTheClockDatesTheFirstFrame(t *testing.T) {
	srv := bustest.WithStreams(t)
	path := synthetic(t, 20, func(i int) float64 { return 5000 + 0.25*float64(i) })
	data, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	got := runReplay(t, data, "--nats", srv.URL(), "-", "--speed", "max", "--clock", "2026-10-31T20:05:09.5+01:00")
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	frames := srv.Read("TELEMETRY", 0)
	if len(frames) != 20 {
		t.Fatalf("%d frames, want 20", len(frames))
	}
	start := time.Date(2026, 10, 31, 19, 5, 9, 500_000_000, time.UTC)
	for i, m := range frames {
		if at := ceTime(t, m); !at.Equal(start.Add(time.Duration(i) * 250 * time.Millisecond)) {
			t.Errorf("frame %d at %v", i, at)
		}
		if !regexp.MustCompile(`^twr\.v1\.replay-stdin-[a-z2-7]{6}\.frame\.gt7$`).MatchString(m.Subject) {
			t.Errorf("frame %d on %s", i, m.Subject)
		}
	}
}

// R2 of paddock#35: at speed 1 the replay takes the recording's own time, a pause
// included, and at speed 4 a quarter of it; max does not wait.
func TestTheSpeedPacesTheReplay(t *testing.T) {
	// 1.2 s: 20 packets 10 ms apart, a pause, and 1 more at 1.2 s.
	path := synthetic(t, 21, func(i int) float64 {
		if i == 20 {
			return 100 + 1.2
		}
		return 100 + 0.01*float64(i)
	})
	for _, c := range []struct {
		speed float64
		want  time.Duration
	}{{1, 1200 * time.Millisecond}, {4, 300 * time.Millisecond}, {0, 0}} {
		src := openRecording(t, path)
		p := &pacer{src: src, speed: c.speed, clock: defaultClock}
		start := time.Now()
		all := readAll(t, p)
		took := time.Since(start)
		if len(all) != 21 || took < c.want || took > c.want+150*time.Millisecond {
			t.Errorf("speed %v: %d packets in %v, want 21 in %v", c.speed, len(all), took, c.want)
		}
		if at := p.at(all[20]); !at.Equal(defaultClock.Add(1200 * time.Millisecond)) {
			t.Errorf("speed %v: the last packet at %v, want 1.2 s after the first whatever the speed", c.speed, at)
		}
	}
}

// R1 of paddock#35: a bad recording exits 1 with the file and line, a server that
// cannot be reached within the timeout 2, and a bad flag 2, before any publishing.
func TestTheExitCodes(t *testing.T) {
	srv := bustest.WithStreams(t)
	good := synthetic(t, 3, func(i int) float64 { return float64(i) })
	data, err := os.ReadFile(good) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(t.TempDir(), "broken.jsonl")
	if err := os.WriteFile(broken, append(data, "{not a frame\n"...), 0o600); err != nil { //nolint:gosec // the test's own file
		t.Fatal(err)
	}
	got := runReplay(t, nil, "--nats", srv.URL(), "--speed", "max", broken)
	if got.code != 1 || !strings.Contains(got.stderr, broken+":4:") {
		t.Errorf("broken line: exit %d, stderr %q; want 1 naming %s:4", got.code, got.stderr, broken)
	}
	if n := srv.Count("TELEMETRY"); n != 3 {
		t.Errorf("TELEMETRY holds %d frames, want the 3 before the broken line", n)
	}
	missing := filepath.Join(t.TempDir(), "missing.jsonl.gz")
	if got := runReplay(t, nil, "--nats", srv.URL(), missing); got.code != 1 || !strings.Contains(got.stderr, missing) {
		t.Errorf("missing file: exit %d, stderr %q; want 1 naming it", got.code, got.stderr)
	}

	connectTimeout = 200 * time.Millisecond
	t.Cleanup(func() { connectTimeout = 10 * time.Second })
	got = runReplay(t, nil, "--nats", "nats://127.0.0.1:1", good)
	if got.code != 2 || !strings.Contains(got.stderr, "not reached within 200ms") || got.took > 3*time.Second {
		t.Errorf("no server: exit %d after %v, stderr %q; want 2 at once", got.code, got.took, got.stderr)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--speed", "0", good}, `--speed "0"`},
		{[]string{"--speed", "fast", good}, `--speed "fast"`},
		{[]string{"--speed", "+Inf", good}, `--speed "+Inf"`},
		{[]string{"--clock", "yesterday", good}, `--clock "yesterday"`},
		{[]string{"--src", "live", good}, `src "live" is the PS5's`},
		{[]string{"--src", "parity", "--run", "x", good}, "--src and --run"},
		{[]string{"--run", "a.b", good}, "a src is one subject token"},
		{[]string{}, "one recording"},
		{[]string{good, good}, "one recording"},
		{[]string{"--nope", good}, "-nope"},
	} {
		got := runReplay(t, nil, append([]string{"--nats", srv.URL()}, c.args...)...)
		if got.code != 2 || !strings.Contains(got.stderr, c.want) || strings.Contains(got.stdout, "publishing") {
			t.Errorf("%v: exit %d, stderr %q, log %q; want 2 and %q", c.args, got.code, got.stderr, got.stdout, c.want)
		}
	}
	if got := runReplay(t, nil, "-h"); got.code != 0 || !strings.Contains(got.stderr, "usage: ecu replay") {
		t.Errorf("-h: exit %d, stderr %q", got.code, got.stderr)
	}
}

// R3 of paddock#35 (AC4): the default src is replay-<stem>-<suffix>, the stem made
// into a subject token; --run sets the stem with no suffix; parity is accepted.
func TestTheSrc(t *testing.T) {
	src, err := replaySrc("", "", "/recordings/Real Race 2laps.jsonl.gz")
	if err != nil || !regexp.MustCompile(`^replay-real-race-2laps-[a-z0-9]+$`).MatchString(src) {
		t.Errorf("default src %q, %v", src, err)
	}
	again, _ := replaySrc("", "", "/recordings/Real Race 2laps.jsonl.gz")
	if again == src {
		t.Errorf("two replays of one file share the src %q", src)
	}
	for _, c := range []struct{ src, run, path, want string }{
		{"", "monza", "x.jsonl", "replay-monza"},
		{"parity", "", "x.jsonl", "parity"},
		{"replay-mine", "", "x.jsonl", "replay-mine"},
	} {
		if got, err := replaySrc(c.src, c.run, c.path); err != nil || got != c.want {
			t.Errorf("%+v: %q, %v", c, got, err)
		}
	}
	for path, stem := range map[string]string{
		"-":                                "stdin",
		"a/b/x.jsonl":                      "x",
		"Suzuka_East (2).JSONL":            "suzuka-east--2",
		"__.jsonl.gz":                      "recording",
		"20261031-200509.jsonl.gz":         "20261031-200509",
		"real-race-2laps.jsonl.gz":         "real-race-2laps",
		"real.race.2laps.jsonl.gz":         "real-race-2laps",
		strings.Repeat("a", 90) + ".jsonl": strings.Repeat("a", 90),
	} {
		if got := stemOf(path); got != stem {
			t.Errorf("stemOf(%q) = %q, want %q", path, got, stem)
		}
	}
	long, err := replaySrc("", "", strings.Repeat("a", 90)+".jsonl")
	if err == nil {
		err = bus.Config{URL: "x", Src: long}.Validate()
	}
	if err != nil {
		t.Errorf("a long stem: %q, %v", long, err)
	}
}

// --clock takes ISO 8601 times, UTC unless they say, and epoch seconds.
func TestParseClock(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2026-01-01T00:00:00Z":      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		"2026-01-01T02:00:00+02:00": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		"2026-10-31T20:05:09.25":    time.Date(2026, 10, 31, 20, 5, 9, 250_000_000, time.UTC),
		"2026-10-31T20:05":          time.Date(2026, 10, 31, 20, 5, 0, 0, time.UTC),
		"2026-10-31":                time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		"1767225600":                time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		"1767225600.5":              time.Date(2026, 1, 1, 0, 0, 0, 500_000_000, time.UTC),
	} {
		if got, err := parseClock(in); err != nil || !got.Equal(want) {
			t.Errorf("parseClock(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

// R1 of paddock#35: the session a recording leaves open ends at its end, as the
// input ended (END_REASON_INPUT_ENDED, the race state's reason for the end of a
// recording since contracts v0.1.4).
func TestTheOpenSessionEndsWithTheRecording(t *testing.T) {
	srv := bustest.WithStreams(t)
	in := readAll(t, openRecording(t, recordingPath(t, suzuka)))
	path := synthetic(t, len(in)/2, nil) // mid-race
	if got := runReplay(t, nil, "--nats", srv.URL(), "--speed", "max", "--run", "half", path); got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	stored := srv.Read("EVENTS", 0)
	last := stored[len(stored)-1]
	if !strings.HasSuffix(last.Subject, ".session.ended") || !bytes.Contains(last.Data, []byte(`"END_REASON_INPUT_ENDED"`)) ||
		!strings.HasPrefix(last.Subject, "twr.v1.replay-half.evt.") {
		t.Errorf("the last event is %s %s, want the session ended as the input ended", last.Subject, last.Data)
	}
}

// A replay stopped part way publishes what it had and exits 130.
func TestAnInterruptedReplay(t *testing.T) {
	srv := bustest.WithStreams(t)
	path := synthetic(t, 3, func(i int) float64 { return 60 * float64(i) }) // a minute apart
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if code := replay(ctx, []string{"--nats", srv.URL(), path}, env(nil), nil, &stdout, &stderr); code != 130 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if n := srv.Count("TELEMETRY"); n != 1 {
		t.Errorf("TELEMETRY holds %d frames, want the first only", n)
	}
}
