package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/bus/bustest"
	"github.com/those-who-run/ecu/internal/live"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/service"
	"github.com/those-who-run/ecu/internal/status"
	"github.com/those-who-run/salsa"
)

// healthz is GET /healthz's code and body.
func healthz(t *testing.T, h service.Health) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	service.Handler("test", h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return rec.Code, body
}

// paddock#88, AC4 and R4: with a fake packet source, /healthz says busy while
// driving frames arrive and idle a telemetry timeout after the last one, with the
// telemetry state; it answers 503 only for a dead task or a listener that cannot
// bind, and NATS down is ok false and still 200.
func TestHealthzGoesIdleAfterTheLastFrame(t *testing.T) {
	const timeout = 300 * time.Millisecond
	var listening atomic.Bool
	listening.Store(true)
	tracker := newTracker("live", timeout, nil, listening.Load)
	var driving, writing, nats atomic.Bool
	driving.Store(true)
	writing.Store(true)
	nats.Store(true)
	health := healthOf(tracker, map[string]*atomic.Bool{"drive": &driving, "status": &writing}, nats.Load)

	code, body := healthz(t, health)
	if code != http.StatusOK || body["idle"] != true || body["telemetry_connected"] != false ||
		body["last_packet_age_secs"] != nil || body["ok"] != true {
		t.Fatalf("before a packet: %d %v", code, body)
	}

	src := &fed{packets: make(chan salsa.Packet), start: time.Now()}
	at := func(r salsa.Received) time.Time { return src.start.Add(time.Duration(r.T * float64(time.Second))) }
	ctx, cancel := context.WithCancel(t.Context())
	driven := make(chan struct{})
	go func() {
		defer close(driven)
		_ = drive(ctx, src, at, machine(), nil, &fakePublisher{}, nil, tracker, nil)
	}()
	for i := range 10 {
		src.packets <- racePacket(int32(i+1), 1, false)
	}
	// The drive takes a packet after it is sent: wait for the last.
	deadline := time.Now().Add(5 * time.Second)
	for tracker.Values().Status.GetPackets() < 10 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	code, body = healthz(t, health)
	if code != http.StatusOK || body["idle"] != false || body["idle_secs"] != 0.0 || body["activity"] != "driving" ||
		body["telemetry_connected"] != true || body["packets"] != 10.0 || body["races_started"] != 1.0 {
		t.Fatalf("driving: %d %v", code, body)
	}
	time.Sleep(timeout + 50*time.Millisecond)
	code, body = healthz(t, health)
	if code != http.StatusOK || body["idle"] != true || body["activity"] != "idle" || body["telemetry_connected"] != false {
		t.Fatalf("a timeout after the last frame: %d %v", code, body)
	}
	if age, ok := body["last_packet_age_secs"].(float64); !ok || age < timeout.Seconds()-0.05 {
		t.Errorf("last packet age %v", body["last_packet_age_secs"])
	}
	cancel()
	<-driven

	nats.Store(false)
	if code, body := healthz(t, health); code != http.StatusOK || body["ok"] != false || body["nats_connected"] != false {
		t.Errorf("NATS down: %d %v", code, body)
	}
	listening.Store(false)
	if code, _ := healthz(t, health); code != http.StatusServiceUnavailable {
		t.Errorf("no listener: %d", code)
	}
	listening.Store(true)
	writing.Store(false)
	if code, body := healthz(t, health); code != http.StatusServiceUnavailable || body["tasks"].(map[string]any)["status"] != false {
		t.Errorf("a dead task: %d %v", code, body)
	}
}

// memKV is a KV in memory.
type memKV struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (k *memKV) Put(_ context.Context, key string, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.values[key] = append([]byte(nil), value...)
	return nil
}

func (k *memKV) Delete(_ context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.values, key)
	return nil
}

// paddock#88, R5: with every setting set to a known string, no value in the
// live bucket and nothing /healthz says holds one: not the PS5's address, nor
// the NATS server's, its credentials or a token.
func TestNoSecretInTheStatus(t *testing.T) {
	secrets := map[string]string{ //nolint:gosec // known strings to look for, not credentials
		"TWR_ECU_PS5_IP": "192.0.2.123",
		"TWR_NATS_URL":   "nats://twr-user-c0ffee:twr-password-c0ffee@nats.example:4222",
		"TWR_NATS_TOKEN": "nats-token-c0ffee",
		"DISCORD_TOKEN":  "discord-token-c0ffee",
	}
	getenv := func(k string) string { return secrets[k] }
	liveCfg, err := live.ConfigFromEnv(getenv)
	if err != nil {
		t.Fatal(err)
	}
	var busCfg bus.Config
	busCfg.Flags(flag.NewFlagSet("ecu", flag.ContinueOnError), getenv)
	if !strings.Contains(busCfg.URL, "c0ffee") {
		t.Fatalf("the NATS URL %q is not the one set", busCfg.URL)
	}
	eng, err := rulesEngine(getenv, service.NewLogger(io.Discard, 0))
	if err != nil {
		t.Fatal(err)
	}
	tracker := newTracker(busCfg.Src, liveCfg.TelemetryTimeout, eng, nil)
	all := readAll(t, openRecording(t, recordingPath(t, suzuka)))
	// Mid-race, a session open: the drive's step and Observe, without its end.
	m := machine()
	m.Rules(eng.Step)
	for _, r := range all[:len(all)/2] {
		outs := m.Step(race.Received{Received: r, Time: time.Now()})
		tracker.Observe(m.Live(), outs)
	}
	kv := &memKV{values: map[string][]byte{}}
	status.NewWriter(tracker, kv, slog.New(slog.DiscardHandler)).Flush(t.Context())
	if _, ok := kv.values["ecu.session.live"]; !ok {
		t.Fatalf("no session written: %v", kv.values)
	}
	var text strings.Builder
	for k, v := range kv.values {
		text.WriteString(k + " " + string(v) + "\n")
	}
	_, body := healthz(t, healthOf(tracker, nil, func() bool { return true }))
	h, _ := json.Marshal(body)
	text.Write(h)
	for name, v := range secrets {
		for _, part := range []string{v, "c0ffee", "192.0.2"} {
			if strings.Contains(text.String(), part) {
				t.Errorf("%s's %q is in the status:\n%s", name, part, text.String())
			}
		}
	}
}

// Live's status writer waits for NATS's first connection before its first
// write: with the server reached a moment after the start, as on every deploy,
// ecu.status.live is written with no "cannot be written" warning (2026-10-31's
// "outbound buffer limit exceeded" at each start).
func TestLiveWritesItsStatusOnceNATSIsReached(t *testing.T) {
	srv := bustest.NewServer(t)
	theirs := bustest.AddKV(t, srv.JetStream(), "live")
	bustest.AddKV(t, srv.JetStream(), "ecu")
	srv.Stop()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout := newReadyLog()
	exit := make(chan int, 1)
	go func() {
		exit <- run(ctx, []string{"-nats", srv.URL(), "-src", "live"},
			liveEnv(map[string]string{"ADDR": "127.0.0.1:0", "TWR_ECU_RECORDINGS_DIR": t.TempDir()}), stdout, &bytes.Buffer{})
	}()
	select {
	case <-stdout.ready:
	case code := <-exit:
		t.Fatalf("exit %d before serving, log %q", code, stdout.String())
	case <-time.After(10 * time.Second):
		t.Fatal("not serving after 10 s")
	}
	time.Sleep(200 * time.Millisecond) // past the writer's start
	srv.Start()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := theirs.Get(t.Context(), "ecu.status.live"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ecu.status.live 10 s after NATS came up: %s", stdout.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if code := <-exit; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if log := stdout.String(); strings.Contains(log, "cannot be written") {
		t.Errorf("a write failed before NATS was first reached: %s", log)
	}
}
