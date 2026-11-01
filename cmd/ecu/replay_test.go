package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/those-who-run/contracts/ce"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/bus/bustest"
	"github.com/those-who-run/ecu/internal/record"
	"github.com/those-who-run/ecu/race"
	"google.golang.org/protobuf/encoding/protojson"
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
	// Dated as the replay dates them, so the payloads' times match too.
	dated := &pacer{clock: defaultClock, t0: in[0].T}
	_ = drive(t.Context(), &slice{all: in}, dated.at, machine(), nil, want, nil, nil, nil)
	// An event the session can have more than once a frame apart, such as
	// session.track, ends its id in <run>:<seq>: the run is compared as <run>,
	// the direct run's here and the replay's (its frames') in the stream.
	var kinds []string
	var wantRun string
	for _, o := range want.outs {
		if o.Frame != nil && wantRun == "" {
			wantRun, _, _ = strings.Cut(o.Event.ID, ":frame:")
		}
		if o.Frame == nil {
			kind := strings.ReplaceAll(o.Event.ID[strings.Index(o.Event.ID, ":")+1:], wantRun, "<run>")
			why := ""
			if o.Ended != nil {
				why = o.Why.Reason().String()
			}
			kinds = append(kinds, kind+" "+why)
		}
	}

	// Every frame, in order: seq runs 1..N in stream order, under one run id.
	srv.Settled("TELEMETRY", uint64(len(in)))
	frames := srv.Read("TELEMETRY", 0)
	if len(frames) != len(in) {
		t.Errorf("TELEMETRY holds %d frames, want every one of the %d", len(frames), len(in))
	}
	var run string
	for i, m := range frames {
		id := ce.Header(m.Header).Get(ce.IDHeader) // <run>:frame:<seq>
		r, seq, _ := strings.Cut(id, ":frame:")
		if i == 0 {
			run = r
		}
		if r != run || seq != strconv.Itoa(i+1) {
			t.Fatalf("frame %d in the stream is %s, want %s:frame:%d", i, id, run, i+1)
		}
	}
	stored := srv.Read("EVENTS", 0)
	var gotKinds []string
	for _, m := range stored {
		id := ce.Header(m.Header).Get(ce.IDHeader) // parity:<sid>:<area>.<what>:<n>
		kind := strings.ReplaceAll(id[strings.Index(id[len("parity:"):], ":")+len("parity:")+1:], run, "<run>")
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
	// Each payload is the race state's for the same packets, laps and lap times
	// included, apart from the session id, which is random on each run, and the
	// run id session.track and session.kind carry (contracts v0.2.3).
	var wantEvents []race.Output
	for _, o := range want.outs {
		if o.Frame == nil {
			wantEvents = append(wantEvents, o)
		}
	}
	for i, o := range wantEvents {
		sid := strings.Split(stored[i].Subject, ".")[4] // twr.v1.parity.evt.<sid>.<area>.<what>
		if o.Started != nil {
			o.Started.Source = "parity"
		}
		data, err := protojson.Marshal(o.Message())
		if err != nil {
			t.Fatal(err)
		}
		var w, g map[string]any
		if err := json.Unmarshal([]byte(strings.NewReplacer(o.Session, sid, wantRun, run).Replace(string(data))), &w); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(stored[i].Data, &g); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(w, g) {
			t.Errorf("event %d (%s) is\n%s\nwant\n%s", i, kinds[i], stored[i].Data, data)
		}
	}

	first := srv.Read("TELEMETRY", 1)[0]
	if at := ceTime(t, first); !at.Equal(defaultClock) {
		t.Errorf("the first frame is at %v, want %v", at, defaultClock)
	}
	for _, line := range []string{`"msg":"replaying"`, `"dropped_frames":0`, `"dropped_events":0,"dropped_callouts":0,"final":true`} {
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
	if n := srv.Settled("TELEMETRY", 3); n != 3 {
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
	src, err := replaySrc("", "", "/recordings/Real Race 2laps.jsonl.gz", "")
	if err != nil || !regexp.MustCompile(`^replay-real-race-2laps-[a-z0-9]+$`).MatchString(src) {
		t.Errorf("default src %q, %v", src, err)
	}
	again, _ := replaySrc("", "", "/recordings/Real Race 2laps.jsonl.gz", "")
	if again == src {
		t.Errorf("two replays of one file share the src %q", src)
	}
	for _, c := range []struct{ src, run, path, want string }{
		{"", "monza", "x.jsonl", "replay-monza"},
		{"parity", "", "x.jsonl", "parity"},
		{"replay-mine", "", "x.jsonl", "replay-mine"},
	} {
		if got, err := replaySrc(c.src, c.run, c.path, ""); err != nil || got != c.want {
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
	long, err := replaySrc("", "", strings.Repeat("a", 90)+".jsonl", "")
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
	got := runReplay(t, nil, "--nats", srv.URL(), "--speed", "max", "--run", "half", path)
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	// R5: the summary's one line says so too.
	if lines := summaryLines(t, got.stdout); len(lines) != 1 || !strings.HasSuffix(lines[0], " INPUT_ENDED") {
		t.Errorf("the summary is %q, want one session ended as the input ended", lines)
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
	if n := srv.Settled("TELEMETRY", 1); n != 1 {
		t.Errorf("TELEMETRY holds %d frames, want the first only", n)
	}
}

// R1 of paddock#35: events the server never acknowledges (here, with no EVENTS
// stream) are dropped once the grace runs out, and the replay exits 2, not 0.
func TestEventsNotTakenExitTwo(t *testing.T) {
	srv := bustest.WithStreams(t, "telemetry")
	replayGrace = 300 * time.Millisecond
	t.Cleanup(func() { replayGrace = 30 * time.Second })
	got := runReplay(t, nil, "--speed", "max", "--src", "parity", "--nats", srv.URL(), recordingPath(t, suzuka))
	if got.code != 2 || !strings.Contains(got.stderr, "22 events and") {
		t.Errorf("exit %d, stderr %q; want 2 and the events dropped", got.code, got.stderr)
	}
}

// signalOnFlush is a log that cancels the replay once it says it is flushing, as
// a SIGINT then would.
type signalOnFlush struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	cancel context.CancelFunc
	at     time.Time
}

func (l *signalOnFlush) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bytes.Contains(p, []byte(`"msg":"replayed: flushing"`)) {
		l.at = time.Now()
		l.cancel()
	}
	return l.buf.Write(p)
}

// A signal while the replay waits for the server to take what is queued cuts
// the wait to interruptGrace, and the replay exits 130.
func TestASignalDuringTheFlush(t *testing.T) {
	srv := bustest.WithStreams(t, "telemetry") // no EVENTS: the events would wait the full grace
	interruptGrace = 200 * time.Millisecond
	t.Cleanup(func() { interruptGrace = time.Second })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	log := &signalOnFlush{cancel: cancel}
	var stderr bytes.Buffer
	code := replay(ctx, []string{"--speed", "max", "--src", "parity", "--nats", srv.URL(), recordingPath(t, suzuka)},
		env(nil), nil, log, &stderr)
	took := time.Since(log.at)
	if code != 130 || log.at.IsZero() || took > 2*time.Second {
		t.Errorf("exit %d %v after the signal, stderr %q; want 130 within 2 s", code, took, stderr.String())
	}
}

// A final flush that fails (a server that stops reading after the handshake never
// answers it) exits 2: a replay that may have lost its last frames never exits 0.
func TestAFailedFlushExitsTwo(t *testing.T) {
	replayGrace = 300 * time.Millisecond
	t.Cleanup(func() { replayGrace = 30 * time.Second })
	path := synthetic(t, 3, func(i int) float64 { return float64(i) })
	got := runReplay(t, nil, "--speed", "max", "--src", "parity", "--nats", bustest.Stalled(t, false), path)
	if got.code != 2 || !strings.Contains(got.stderr, "the final flush failed") {
		t.Errorf("exit %d, stderr %q; want 2 and the flush named", got.code, got.stderr)
	}
}

// paddock#88, R1 and R6: a replay writes its status under its own source, never
// live's, and leaves it idle with the session's keys gone at its end.
func TestAReplayWritesItsOwnStatus(t *testing.T) {
	srv := bustest.WithStreams(t)
	kv := bustest.AddKV(t, srv.JetStream(), "live")
	path := recordingPath(t, suzuka)
	got := runReplay(t, nil, "--speed", "max", "--run", "status", "--nats", srv.URL(), path)
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q, log %s", got.code, got.stderr, got.stdout)
	}
	keys, err := kv.ListKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for k := range keys.Keys() {
		all = append(all, k)
	}
	if len(all) != 1 || all[0] != "ecu.status.replay-status" {
		t.Fatalf("keys %v, want ecu.status.replay-status alone", all)
	}
	e, err := kv.Get(t.Context(), "ecu.status.replay-status")
	if err != nil {
		t.Fatal(err)
	}
	var st statev1.EcuStatus
	if err := ce.Unmarshal(e.Value(), &st); err != nil {
		t.Fatal(err)
	}
	if st.GetSource() != "replay-status" || !st.GetIdle() || st.GetActivity() != statev1.Activity_ACTIVITY_IDLE ||
		st.GetRacesStarted() != 1 || int(st.GetPackets()) != len(readAll(t, openRecording(t, path))) {
		t.Errorf("the replay's last status: %v", &st)
	}
}

// paddock#88: the status's last write gets its grace from the end of the drive,
// not from the start of the replay, so a replay that lasts longer than the grace
// still leaves its status idle and its session's keys deleted.
func TestALongReplayLeavesItsStatusIdle(t *testing.T) {
	srv := bustest.WithStreams(t)
	kv := bustest.AddKV(t, srv.JetStream(), "live")
	saved := statusGrace
	// Long enough for the last write's four calls on a loaded CI runner, with
	// the race detector on (200 ms ran out there, paddock#76's PR).
	statusGrace = 600 * time.Millisecond
	t.Cleanup(func() { statusGrace = saved })
	in := readAll(t, openRecording(t, recordingPath(t, suzuka)))
	n := len(in) / 2 // mid-race: the session is open when the recording ends
	// Paced at 1, about 3 s: five times the grace.
	path := synthetic(t, n, func(i int) float64 { return 3 * float64(i) / float64(n) })
	got := runReplay(t, nil, "--speed", "1", "--run", "long", "--nats", srv.URL(), path)
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q, log %s", got.code, got.stderr, got.stdout)
	}
	if got.took < 4*statusGrace {
		t.Fatalf("the replay took %v, not longer than the grace", got.took)
	}
	keys, err := kv.ListKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for k := range keys.Keys() {
		all = append(all, k)
	}
	if len(all) != 1 || all[0] != "ecu.status.replay-long" {
		t.Fatalf("keys %v, want ecu.status.replay-long alone", all)
	}
	e, err := kv.Get(t.Context(), "ecu.status.replay-long")
	if err != nil {
		t.Fatal(err)
	}
	var st statev1.EcuStatus
	if err := ce.Unmarshal(e.Value(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.GetIdle() || st.GetRacesStarted() != 1 || int(st.GetPackets()) != n {
		t.Errorf("the replay's last status: %v", &st)
	}
}

// paddock#76, the restart pair as paddock's CI runs it: two replays under one
// --src, the first with --keep-open, the second's --clock 8.7 s after the first's
// last frame, give one session: one session.started, one session.continued, one
// session.ended at the end of the second, and the ecu bucket's key cleared.
func TestARestartPairOfReplays(t *testing.T) {
	srv := bustest.WithStreams(t)
	kv := bustest.AddKV(t, srv.JetStream(), "ecu")
	before, joined := trackRecording(t, beforeRestart), trackRecording(t, joinedMidRace)
	in := readAll(t, openRecording(t, before))
	last := defaultClock.Add(seconds(in[len(in)-1].T - in[0].T))
	if got := runReplay(t, nil, "--nats", srv.URL(), "--speed", "max", "--run", "pair", "--keep-open", before); got.code != 0 {
		t.Fatalf("the first replay: exit %d, stderr %q, log %s", got.code, got.stderr, got.stdout)
	}
	if _, err := kv.Get(t.Context(), "sessions.open.replay-pair"); err != nil {
		t.Fatalf("the first replay kept no session: %v", err)
	}
	clock := last.Add(8700 * time.Millisecond).Format(time.RFC3339Nano)
	if got := runReplay(t, nil, "--nats", srv.URL(), "--speed", "max", "--run", "pair", "--clock", clock, joined); got.code != 0 {
		t.Fatalf("the second replay: exit %d, stderr %q, log %s", got.code, got.stderr, got.stdout)
	}
	count := map[string]int{}
	for _, m := range srv.Read("EVENTS", 0) {
		for _, what := range []string{"session.started", "session.continued", "session.ended"} {
			if strings.HasSuffix(m.Subject, "."+what) {
				count[what]++
			}
		}
	}
	if count["session.started"] != 1 || count["session.continued"] != 1 || count["session.ended"] != 1 {
		t.Errorf("events %v, want one session started, continued and ended", count)
	}
	if _, err := kv.Get(t.Context(), "sessions.open.replay-pair"); err == nil {
		t.Error("the key is still there after the session ended")
	}
}
