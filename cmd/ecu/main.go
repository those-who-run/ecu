// Command ecu listens to the PS5 live: it keeps GT7 sending with its heartbeat,
// reads each packet, runs it through the race state and publishes the frames and
// events on NATS, and, with TWR_ECU_RECORDINGS_DIR set, records the drive in v1's
// format (paddock#106, paddock#266). TWR_ECU_PS5_IP is required: without it ecu
// stops at start. It reads its configuration from the
// environment, logs JSON to standard output and serves GET /healthz until it gets
// SIGINT or SIGTERM. `ecu -version` prints its version and exits.
//
// It connects to NATS at start, with the server and the source token from --nats
// and --src (TWR_NATS_URL and TWR_SRC), and refuses a source that is not one
// subject token. Live frames go out under the source live only (paddock#106, R8),
// so any other source stops ecu at start.
//
// The rule engine (paddock#89) runs on every frame and its callouts go out on
// twr.v1.<src>.callout.p0, .p1 or .p2. TWR_ECU_RULES_FILE names the rules file,
// v1's rules YAML (rules.example.yaml's shape); unset, the rules take v1's
// defaults. A file that cannot be read, or a setting in it that is wrong, stops
// ecu at start.
//
// A pause never ends a session on its own (paddock#77): TWR_ECU_PAUSE_LIMIT_SECS,
// a number of seconds, ends one paused once a pause has lasted that long. Unset or
// 0, the default, there is no limit.
//
// A stop ends no session (paddock#76): ecu keeps the open session in the `ecu` KV
// bucket, sessions.open.live, and the next run continues it when it joins the
// same race within TWR_ECU_CONTINUE_WINDOW_SECS of the last frame (600 s unless
// set; 0 never continues), or ends it, END_REASON_ENGINEER_STOPPED: keep.go.
//
// ecu says what it is doing in the `live` KV bucket (paddock#88): ecu.status.live,
// and while a session is open ecu.session.live, ecu.pit.live and ecu.fuel.live,
// written from a goroutine of their own (internal/status), never from the frame
// path. GET /healthz carries the same idle flag and telemetry state, with
// whether each task is alive and NATS is connected; it answers 503 only when a
// task has died or the telemetry listener cannot hold its port.
//
// `ecu replay PATH` replays a v1 recording instead, under a source of its own, and
// exits at its end (paddock#35): replay.go. `ecu tracks detect PATH` and `ecu
// tracks learn PATH` run v1's track tools over one, offline (paddock#300):
// tracks.go.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/live"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/record"
	"github.com/those-who-run/ecu/internal/rules"
	_ "github.com/those-who-run/ecu/internal/rules/all" // every rule
	"github.com/those-who-run/ecu/internal/service"
	"github.com/those-who-run/ecu/internal/sessions"
	"github.com/those-who-run/ecu/internal/status"
	"github.com/those-who-run/salsa"
)

// closeGrace is how long ecu waits at shutdown to send the events still queued, and
// closeAfter how much longer it waits for the publisher's final totals.
const (
	closeGrace = 5 * time.Second
	closeAfter = 3 * time.Second
)

// version is what -version prints and GET /healthz reports: "dev" unless the build
// sets it with -ldflags "-X main.version=X.Y.Z". The Dockerfile does that from its
// VERSION build argument, which is "dev" unless the build passes one
// (docker build --build-arg VERSION=X.Y.Z). image.yml in those-who-run/.github
// passes VERSION=X.Y.Z when it builds on a release tag vX.Y.Z, so a released image
// reports its version; a pull request's image build passes none, and reports "dev".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "tracks" {
		os.Exit(tracks(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "replay" {
		os.Exit(replay(context.Background(), os.Args[2:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is main without the process around it, so the tests can call it. It serves
// until ctx is done or the process gets SIGINT or SIGTERM, and returns the exit
// code: 0 after a clean shutdown or for -h, 1 when the service fails, 2 for a bad
// flag or configuration.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	flags := flag.NewFlagSet("ecu", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print the version and exit")
	var busCfg bus.Config
	busCfg.Flags(flags, getenv)
	if err := flags.Parse(args); err != nil {
		// -h and -help print the usage, which is what was asked for.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	cfg, err := service.ConfigFromEnv(getenv)
	if err == nil {
		err = busCfg.Validate()
	}
	if err == nil && busCfg.Src != bus.DefaultSrc {
		err = fmt.Errorf("src %q: live frames go out under the source %q only", busCfg.Src, bus.DefaultSrc)
	}
	var liveCfg live.Config
	if err == nil {
		liveCfg, err = live.ConfigFromEnv(getenv)
		liveCfg.Listen = or(listenAddr, liveCfg.Listen)
	}
	var limit float64
	if err == nil {
		limit, err = pauseLimit(getenv)
	}
	var window time.Duration
	if err == nil {
		window, err = continueWindow(getenv)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	logger := service.NewLogger(stdout, cfg.LogLevel)
	eng, err := rulesEngine(getenv, logger)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	var rec *record.Writer
	if dir := getenv("TWR_ECU_RECORDINGS_DIR"); dir != "" {
		if rec, err = record.Open(dir, logger, time.Now); err != nil {
			_, _ = fmt.Fprintln(stderr, "TWR_ECU_RECORDINGS_DIR:", err)
			return 2
		}
	}
	// The recorder closes before the publisher, so a NATS server that cannot be
	// reached never uses up the recording's grace and cuts its gzip short.
	closeRec := func() {
		if rec == nil {
			return
		}
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeGrace)
		defer cancel()
		rec.Close(grace)
	}
	pub, err := bus.Connect(busCfg, logger, bus.Options{})
	if err != nil {
		closeRec()
		logger.ErrorContext(ctx, "stopped", "err", err)
		return 1
	}
	defer func() {
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeGrace)
		defer cancel()
		pub.Close(grace)
		// A write that held the publisher past the grace gives up at once, as Close
		// cuts the connection; its final totals are logged then.
		select {
		case <-pub.Closed():
		case <-time.After(closeAfter):
		}
	}()
	kv, err := pub.KV(statusBucket)
	if err != nil {
		closeRec()
		logger.ErrorContext(ctx, "stopped", "err", err)
		return 1
	}
	// The session the last run left open (paddock#76) is read before the drive
	// takes its first packet, so the first frame can continue it.
	ecuKV, err := pub.KV(sessions.Bucket)
	if err != nil {
		closeRec()
		logger.ErrorContext(ctx, "stopped", "err", err)
		return 1
	}
	keeper := sessions.New(ecuKV, busCfg.Src, 0, pub, logger)
	src := live.Open(liveCfg, logger)
	tracker := newTracker(busCfg.Src, liveCfg.TelemetryTimeout, eng, func() bool { return src.Stats().Listening })
	var driving, writing, keepingAlive atomic.Bool
	driving.Store(true)
	writing.Store(true)
	keepingAlive.Store(true)
	driveCtx, stopDrive := context.WithCancel(ctx)
	driven := make(chan struct{})
	go func() {
		defer close(driven)
		defer driving.Store(false)
		start := src.Start()
		at := func(r salsa.Received) time.Time { return start.Add(time.Duration(r.T * float64(time.Second))) }
		m := race.New(ulid.Make)
		m.SetPauseLimit(limit)
		m.WatchTelemetry(liveCfg.TelemetryTimeout) // live only (paddock#87)
		// Read here, not before serving: with NATS still out of reach, /healthz
		// answers meanwhile, and the packets wait in the listener.
		open, rev := loadKept(driveCtx, ecuKV, busCfg.Src, logger)
		keeper.Loaded(rev)
		m.Restore(open, window)
		_ = drive(driveCtx, src, at, m, eng, pub, rec, tracker, &keeping{k: keeper, now: time.Now})
	}()
	writeCtx, stopWriting := context.WithCancel(context.WithoutCancel(ctx))
	written := make(chan struct{})
	go func() {
		defer close(written)
		defer writing.Store(false)
		status.NewWriter(tracker, kv, logger).Run(writeCtx, statusGrace)
	}()
	// The record's last write goes after the status's, each with its own grace.
	keepCtx, stopKeeping := context.WithCancel(context.WithoutCancel(ctx))
	keptDone := make(chan struct{})
	go func() {
		defer close(keptDone)
		defer keepingAlive.Store(false)
		keeper.Run(keepCtx, statusGrace)
	}()
	// The drive ends, keeping the session it holds (no end: paddock#76), and the
	// status says so; the source stops, so nothing writes to the recorder once it
	// closes; then the recorder closes, then the publisher (deferred above).
	defer func() {
		stopDrive()
		<-driven
		tracker.Stop()
		stopWriting()
		<-written
		stopKeeping()
		<-keptDone
		_ = src.Close()
		closeRec()
	}()
	health := healthOf(tracker, map[string]*atomic.Bool{"drive": &driving, "status": &writing, "sessions": &keepingAlive}, pub.Connected)
	if err := service.Run(ctx, cfg, logger, version, health); err != nil {
		logger.ErrorContext(ctx, "stopped", "err", err)
		return 1
	}
	return 0
}

// newTracker is the status tracker for a drive under src (paddock#88), live or a
// replay: what it holds comes from the race state, the run's version and src, and
// nothing of the configuration, so no value carries a secret, the PS5's address
// or the NATS server's (R5).
func newTracker(src string, timeout time.Duration, eng *rules.Engine, listening func() bool) *status.Tracker {
	cfg := status.Config{Src: src, Version: version, TelemetryTimeout: timeout, Listening: listening}
	if eng != nil {
		cfg.Gauge = eng.Gauge
	}
	return status.New(cfg)
}

// healthOf is /healthz's health (paddock#88, R4): the status's idle flag and
// telemetry, whether each task is alive, and whether NATS is connected. ok says
// all of it is well; the endpoint answers 503 only for a task that has died or a
// telemetry listener that cannot hold its port, as a restart might help those,
// while NATS the client reconnects to by itself and a PS5 that is off is normal.
func healthOf(t *status.Tracker, tasks map[string]*atomic.Bool, connected func() bool) service.Health {
	return func() (bool, map[string]any) {
		h := t.Health()
		alive := make(map[string]bool, len(tasks))
		healthy := h["telemetry_listening"] == true
		for name, task := range tasks {
			alive[name] = task.Load()
			healthy = healthy && alive[name]
		}
		nats := connected()
		h["tasks"], h["nats_connected"], h["ok"] = alive, nats, healthy && nats
		return healthy, h
	}
}

// statusGrace is how long the status's last write may take, from when the drive
// has ended: a var, so the tests can shorten it.
var statusGrace = closeGrace

// statusBucket is the KV bucket ecu's status goes in (contracts' subjects.yaml).
const statusBucket = "live"

// listenAddr, when set, is where the live source listens instead of :33740: the
// tests set it, so they never bind the PS5's port.
var listenAddr string

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// publisher is the part of bus.Publisher the drive uses.
type publisher interface {
	Publish(outs []race.Output)
}

// observed is a publisher that hands the tracker the race state's view after
// each step or tick it publishes (paddock#88): a copy, which never waits.
type observed struct {
	publisher
	m  *race.Machine
	st *status.Tracker
}

func (o observed) Publish(outs []race.Output) {
	o.publisher.Publish(outs)
	o.st.Observe(o.m.Live(), outs)
}

// drive runs each packet src gives through the race state and publishes what it
// gives, recording the packet first when rec is set, until src ends or ctx is done.
// at gives a packet's wall-clock time. It returns what stopped src, nil at its
// end: a replay tells a bad recording from an interrupted one by it. While m
// watches the feed (live, paddock#87), it waits for a packet no later than when
// the feed counts as lost, and then publishes the loss (next). With st set, st
// sees the race state after each step and tick (paddock#88).
//
// With kp set (paddock#76), the open session's record is kept through kp's
// Keeper, and a stop ends no session: it keeps the record as of the last frame,
// for the next run to continue or end. While m waits on a record an earlier run
// left, kp's clock, when set, checks the continue window once a second with no
// frames coming. At the end of the input the open session ends as the input
// ended (EndOfInput), unless kp keeps it open. Without kp, a stop ends the open
// session as ecu stopping (Stop).
func drive(ctx context.Context, src salsa.Source, at func(salsa.Received) time.Time, m *race.Machine,
	eng *rules.Engine, pub publisher, rec *record.Writer, st *status.Tracker, kp *keeping,
) error {
	var now func() time.Time
	if kp != nil {
		pub, now = kept{pub, m, kp.k}, kp.now
	}
	if st != nil {
		pub = observed{pub, m, st}
	}
	if eng != nil {
		// The rules read each frame as the machine leaves it (paddock#89).
		m.Rules(eng.Step)
	}
	for {
		r, err := next(ctx, src, m, pub, now)
		if errors.Is(err, io.EOF) && (kp == nil || !kp.keepOpen) {
			pub.Publish(m.EndOfInput())
			return nil
		}
		if err != nil {
			if kp == nil {
				pub.Publish(m.Stop())
			} else {
				// R2: a graceful stop publishes no session.ended.
				kp.k.Put(m.Open())
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if rec != nil {
			rec.Write(r)
		}
		pub.Publish(m.Step(race.Received{Received: r, Time: at(r)}))
	}
}

// pauseLimitEnv is the pause limit, in seconds.
const pauseLimitEnv = "TWR_ECU_PAUSE_LIMIT_SECS"

// pauseLimit is TWR_ECU_PAUSE_LIMIT_SECS: 0, no limit, when unset; a value that is
// not a number of seconds, 0 or more, is an error.
func pauseLimit(getenv func(string) string) (float64, error) {
	v := getenv(pauseLimitEnv)
	if v == "" {
		return 0, nil
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(secs) || math.IsInf(secs, 0) || secs < 0 {
		return 0, fmt.Errorf("%s: %q is not a number of seconds, 0 or more", pauseLimitEnv, v)
	}
	return secs, nil
}

// lossGrace is the last wait for a packet before the feed is called lost.
const lossGrace = time.Millisecond

// next is src's next packet. While m watches the feed, it waits no later than
// m.LostAt, on the wall clock, which live's packet times are on; when none came by
// then, it publishes what m.Tick and the rules that watch the feed make of the
// loss, and waits on. While m waits on a recorded session and now is set, it
// also wakes once a second to publish what m.Expire makes of the continue window
// (paddock#76). A source's Next keeps a packet that comes as its context ends
// for the next call, as live's does.
func next(ctx context.Context, src salsa.Source, m *race.Machine, pub publisher, now func() time.Time) (salsa.Received, error) {
	for {
		due, watching := m.LostAt()
		expiring := now != nil && m.Waiting()
		if !watching && !expiring {
			return src.Next(ctx)
		}
		deadline := due
		if expiring {
			if tick := now().Add(expireEvery); !watching || tick.Before(due) {
				deadline = tick
			}
		}
		wait, cancel := context.WithDeadline(ctx, deadline)
		r, err := src.Next(wait)
		cancel()
		if err == nil || ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			return r, err
		}
		if expiring {
			pub.Publish(m.Expire(now()))
		}
		if !watching || time.Now().Before(due) {
			continue
		}
		// A packet that came as the deadline passed can still be waiting (live's
		// Next picks at random between it and the deadline), or the drive was
		// held up past it: one more try, with a moment's grace and a context not
		// yet done, gives it first, so it is never called a loss.
		grace, cancel := context.WithTimeout(ctx, lossGrace)
		r, err = src.Next(grace)
		cancel()
		if err == nil || ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			return r, err
		}
		// The rules that watch the feed run inside Tick (race.Machine.Rules).
		pub.Publish(m.Tick(time.Now()))
	}
}

// expireEvery is how often the continue window is checked with no frames
// coming (paddock#76, R5; v1's watchdog let go of held cards once a second).
const expireEvery = time.Second

// rulesEnv names the rules file.
const rulesEnv = "TWR_ECU_RULES_FILE"

// rulesEngine is the rule engine with every rule, from the rules file
// TWR_ECU_RULES_FILE names, or v1's defaults when it is unset.
func rulesEngine(getenv func(string) string, logger *slog.Logger) (*rules.Engine, error) {
	cfg := rules.DefaultConfig()
	if path := getenv(rulesEnv); path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // the file the operator named
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rulesEnv, err)
		}
		if cfg, err = rules.ParseConfig(data); err != nil {
			return nil, fmt.Errorf("%s: %w", rulesEnv, err)
		}
		if len(cfg.Unknown) > 0 {
			logger.Warn("the rules file names rules ecu doesn't know: ignored", "file", path, "rules", cfg.Unknown)
		}
	}
	eng, err := rules.New(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rulesEnv, err)
	}
	logger.Info("rules", "enabled", eng.Rules())
	return eng, nil
}
