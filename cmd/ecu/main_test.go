package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/those-who-run/ecu/internal/service"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// liveEnv is vars with the PS5's address live mode needs: loopback, so the
// heartbeat goes nowhere.
func liveEnv(vars map[string]string) func(string) string {
	all := map[string]string{"TWR_ECU_PS5_IP": "127.0.0.1"}
	for k, v := range vars {
		all[k] = v
	}
	return env(all)
}

// The tests never bind the PS5's port.
func TestMain(m *testing.M) {
	listenAddr = "127.0.0.1:0"
	os.Exit(m.Run())
}

// readyLog collects the service's log and closes ready once it logs "serving", so a
// test stops the service when it is listening rather than after a guessed delay.
type readyLog struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	once  sync.Once
	ready chan struct{}
}

func newReadyLog() *readyLog { return &readyLog{ready: make(chan struct{})} }

// Write takes one record per call, as slog's handlers write them.
func (l *readyLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bytes.Contains(p, []byte(`"msg":"serving"`)) {
		l.once.Do(func() { close(l.ready) })
	}
	return l.buf.Write(p)
}

func (l *readyLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// heldAddress returns an address another listener holds for the rest of the test,
// so listening on it fails at once, with no name to resolve.
func heldAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-version"}, env(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != version {
		t.Errorf("printed %q, want %q", got, version)
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	for _, arg := range []string{"-h", "-help"} {
		t.Run(arg, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(t.Context(), []string{arg}, env(nil), &bytes.Buffer{}, &stderr); code != 0 {
				t.Errorf("exit %d, want 0", code)
			}
			if !strings.Contains(stderr.String(), "-version") {
				t.Errorf("stderr %q has no usage", stderr.String())
			}
		})
	}
}

func TestAnUnknownFlagIsAUsageError(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"-nope"}, env(nil), &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-nope") {
		t.Errorf("stderr %q does not name the flag", stderr.String())
	}
}

func TestABadConfigurationIsAUsageError(t *testing.T) {
	var stderr bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{"LOG_LEVEL": "loud"}), &bytes.Buffer{}, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "LOG_LEVEL") {
		t.Errorf("exit %d, stderr %q; want 2 and LOG_LEVEL named", code, stderr.String())
	}
}

func TestAServiceThatCannotStartExitsOne(t *testing.T) {
	var stdout bytes.Buffer
	code := run(t.Context(), nil, liveEnv(map[string]string{"ADDR": heldAddress(t)}), &stdout, &bytes.Buffer{})
	if code != 1 || !strings.Contains(stdout.String(), `"msg":"stopped"`) {
		t.Errorf("exit %d, log %q; want 1 and the failure logged", code, stdout.String())
	}
}

func TestServesUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout := newReadyLog()
	exit := make(chan int, 1)
	go func() {
		exit <- run(ctx, nil, liveEnv(map[string]string{"ADDR": "127.0.0.1:0"}), stdout, &bytes.Buffer{})
	}()
	// The timeouts only stop a hung test; the service is cancelled once it serves.
	select {
	case <-stdout.ready:
	case code := <-exit:
		t.Fatalf("exit %d before serving, log %q", code, stdout.String())
	case <-time.After(10 * time.Second):
		t.Fatal("not serving after 10 s")
	}
	cancel()
	select {
	case code := <-exit:
		if code != 0 || !strings.Contains(stdout.String(), `"msg":"shutting down"`) {
			t.Errorf("exit %d, log %q; want 0 and the shutdown logged", code, stdout.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still running 10 s after cancel")
	}
}

// AC5 of paddock#34: a src that is not one subject token stops ecu at start, from
// the flag or the environment, with a message naming the rule.
func TestASrcOfReplayDot1IsRefusedAtStart(t *testing.T) {
	for _, c := range []struct {
		args []string
		env  map[string]string
	}{
		{[]string{"-src", "replay.1"}, nil},
		{nil, map[string]string{"TWR_SRC": "replay.1"}},
	} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), c.args, env(c.env), &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), `src "replay.1": a src is one subject token`) {
			t.Errorf("%v %v: exit %d, stderr %q; want 2 and the rule", c.args, c.env, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%v %v: logged %q before refusing", c.args, c.env, stdout.String())
		}
	}
}

// R10 of paddock#34: ecu logs the NATS server and src it publishes to at start,
// and what it published at the end; and, live, where it listens and records, and
// what it heard and recorded.
func TestLogsWhereItPublishes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	stdout := newReadyLog()
	exit := make(chan int, 1)
	dir := t.TempDir()
	go func() {
		exit <- run(ctx, []string{"-nats", "nats://127.0.0.1:1", "-src", "live"},
			liveEnv(map[string]string{"ADDR": "127.0.0.1:0", "TWR_ECU_RECORDINGS_DIR": dir}), stdout, &bytes.Buffer{})
	}()
	select {
	case <-stdout.ready:
	case code := <-exit:
		t.Fatalf("exit %d before serving, log %q", code, stdout.String())
	case <-time.After(10 * time.Second):
		t.Fatal("not serving after 10 s")
	}
	cancel()
	if code := <-exit; code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		`"msg":"publishing","nats":"nats://127.0.0.1:1","src":"live"`,
		`"msg":"published","frames":0,`,
		`"dropped_events":0,"dropped_callouts":0,"final":true}`,
		`"msg":"listening to the PS5","ps5":"127.0.0.1","heartbeat_port":33739,"listen":"127.0.0.1:0","heartbeat_secs":2,"heartbeat_kind":"A","capture":true}`,
		`"msg":"capture","on":true}`,
		`"msg":"recording","dir":"` + dir + `"`,
		`"msg":"stopped listening to the PS5","packets":0,`,
		`"msg":"recorded","path":"","outings":0,"frames":0,"dropped":0,"captured":0,"capture_dropped":0}`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("log has no %s: %s", want, stdout.String())
		}
	}
	// The source stops, then the recorder closes, then the publisher, so a server
	// that cannot be reached never cuts a recording short.
	log := stdout.String()
	stopped, recorded, published := strings.Index(log, `"msg":"stopped listening to the PS5"`),
		strings.Index(log, `"msg":"recorded"`), strings.Index(log, `"msg":"published"`)
	if stopped >= recorded || recorded >= published {
		t.Errorf("shut down out of order (source %d, recorder %d, publisher %d): %s", stopped, recorded, published, log)
	}
}

// TWR_ECU_CAPTURE=off records with no capture, and TWR_ECU_HEARTBEAT_KIND picks
// the heartbeat (paddock#343, R4 and R5).
func TestTheCaptureCanBeTurnedOff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	stdout := newReadyLog()
	exit := make(chan int, 1)
	go func() {
		exit <- run(ctx, []string{"-nats", "nats://127.0.0.1:1"}, liveEnv(map[string]string{"ADDR": "127.0.0.1:0",
			"TWR_ECU_RECORDINGS_DIR": t.TempDir(), "TWR_ECU_CAPTURE": "off", "TWR_ECU_HEARTBEAT_KIND": "~"}), stdout, &bytes.Buffer{})
	}()
	select {
	case <-stdout.ready:
	case code := <-exit:
		t.Fatalf("exit %d before serving, log %q", code, stdout.String())
	case <-time.After(10 * time.Second):
		t.Fatal("not serving after 10 s")
	}
	cancel()
	if code := <-exit; code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{`"heartbeat_kind":"~","capture":false}`, `"msg":"capture","on":false}`} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("log has no %s: %s", want, stdout.String())
		}
	}
}

// No PS5 address (the maintainer's decision, 2026-10-28: a typo in a deployment
// fails loudly rather than leaving a healthy pod that hears nothing), one that is
// not an IP address, a bad heartbeat or heartbeat kind, a capture neither on nor
// off, a src other than live, or a recordings
// directory ecu cannot write to stops ecu at start, before it logs anything.
func TestLiveModeIsConfiguredAtStart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		env  func(string) string
		want string
	}{
		{nil, env(nil), "TWR_ECU_PS5_IP: the PS5's IP address is required for live mode"},
		{nil, env(map[string]string{"TWR_ECU_PS5_IP": " "}), "TWR_ECU_PS5_IP: the PS5's IP address is required for live mode"},
		{nil, env(map[string]string{"TWR_ECU_PS5_IP": "ps5"}), `TWR_ECU_PS5_IP: "ps5" is not an IP address`},
		{nil, liveEnv(map[string]string{"TWR_ECU_HEARTBEAT_SECS": "-1"}), "TWR_ECU_HEARTBEAT_SECS"},
		{[]string{"-src", "parity"}, liveEnv(nil), `src "parity": live frames go out under the source "live" only`},
		{nil, liveEnv(map[string]string{"TWR_SRC": "replay-x"}), `src "replay-x": live frames`},
		{nil, liveEnv(map[string]string{"TWR_ECU_RECORDINGS_DIR": file}), "TWR_ECU_RECORDINGS_DIR:"},
		{nil, liveEnv(map[string]string{"TWR_ECU_PAUSE_LIMIT_SECS": "-1"}), "TWR_ECU_PAUSE_LIMIT_SECS"},
		{nil, liveEnv(map[string]string{"TWR_ECU_PAUSE_LIMIT_SECS": "an hour"}), "TWR_ECU_PAUSE_LIMIT_SECS"},
		{nil, liveEnv(map[string]string{"TWR_ECU_RECORDINGS_KEEP_DAYS": "a week"}), "TWR_ECU_RECORDINGS_KEEP_DAYS"},
		{nil, liveEnv(map[string]string{"TWR_ECU_HEARTBEAT_KIND": "C"}), `TWR_ECU_HEARTBEAT_KIND: "C" is not a heartbeat kind`},
		{nil, liveEnv(map[string]string{"TWR_ECU_CAPTURE": "no"}), `TWR_ECU_CAPTURE: "no" is not on or off`},
		{nil, liveEnv(map[string]string{"TWR_ECU_CAPTURE_KEEP_DAYS": "-1"}), "TWR_ECU_CAPTURE_KEEP_DAYS"},
		{nil, liveEnv(map[string]string{"TWR_ECU_RECORDINGS_IDLE_SECS": "0"}), "TWR_ECU_RECORDINGS_IDLE_SECS"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), c.args, c.env, &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q; want 2 and %q", c.args, code, stderr.String(), c.want)
		}
		if strings.Contains(stdout.String(), "publishing") {
			t.Errorf("%v: connected before refusing: %q", c.args, stdout.String())
		}
	}
}

// AC3 of paddock#89: TWR_ECU_RULES_FILE names the rules file, and a pit_lane
// other than speak, hold or drop, or a file that can't be read, stops ecu at
// start; v1's rules.example.yaml loads.
func TestTheRulesFileIsReadAtStart(t *testing.T) {
	dir := t.TempDir()
	whisper := filepath.Join(dir, "whisper.yaml")
	if err := os.WriteFile(whisper, []byte("rules:\n  position:\n    pit_lane: whisper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{whisper, filepath.Join(dir, "missing.yaml")} {
		var stderr bytes.Buffer
		code := run(t.Context(), nil, liveEnv(map[string]string{"TWR_ECU_RULES_FILE": path}), &bytes.Buffer{}, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "TWR_ECU_RULES_FILE") {
			t.Errorf("%s: exit %d, stderr %q; want 2 naming TWR_ECU_RULES_FILE", path, code, stderr.String())
		}
	}
	typo := filepath.Join(dir, "typo.yaml")
	if err := os.WriteFile(typo, []byte("rules:\n  final_lapp: {enabled: false}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned bytes.Buffer
	if _, err := rulesEngine(env(map[string]string{"TWR_ECU_RULES_FILE": typo}), service.NewLogger(&warned, slog.LevelInfo)); err != nil ||
		!strings.Contains(warned.String(), `"rules":["final_lapp"]`) {
		t.Errorf("a misspelled rule: %v, %s", err, warned.String())
	}
	var logs bytes.Buffer
	eng, err := rulesEngine(env(map[string]string{"TWR_ECU_RULES_FILE": filepath.Join("..", "..", "internal", "rules", "testdata", "rules.example.yaml")}),
		service.NewLogger(&logs, slog.LevelInfo))
	if err != nil || len(eng.Rules()) == 0 || !strings.Contains(logs.String(), `"msg":"rules"`) {
		t.Errorf("v1's example: %v, %v, %s", eng, err, logs.String())
	}
}

// R6 of paddock#77: the pause limit is off unless TWR_ECU_PAUSE_LIMIT_SECS sets it.
func TestThePauseLimitIsOffByDefault(t *testing.T) {
	for v, want := range map[string]float64{"": 0, "0": 0, "600": 600, "90.5": 90.5} {
		if got, err := pauseLimit(env(map[string]string{"TWR_ECU_PAUSE_LIMIT_SECS": v})); err != nil || got != want {
			t.Errorf("%q: %v, %v; want %v", v, got, err, want)
		}
	}
}
