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
	src := live.Open(liveCfg, logger)
	driveCtx, stopDrive := context.WithCancel(ctx)
	driven := make(chan struct{})
	go func() {
		defer close(driven)
		start := src.Start()
		at := func(r salsa.Received) time.Time { return start.Add(time.Duration(r.T * float64(time.Second))) }
		m := race.New(ulid.Make)
		m.SetPauseLimit(limit)
		m.WatchTelemetry(liveCfg.TelemetryTimeout) // live only (paddock#87)
		_ = drive(driveCtx, src, at, m, eng, pub, rec)
	}()
	// The drive ends, with the session it holds, and the source stops, so
	// nothing writes to the recorder once it closes; then the recorder closes,
	// then the publisher (deferred above).
	defer func() {
		stopDrive()
		<-driven
		_ = src.Close()
		closeRec()
	}()
	if err := service.Run(ctx, cfg, logger, version); err != nil {
		logger.ErrorContext(ctx, "stopped", "err", err)
		return 1
	}
	return 0
}

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

// drive runs each packet src gives through the race state and publishes what it
// gives, recording the packet first when rec is set, until src ends or ctx is done.
// at gives a packet's wall-clock time. At the end the open session ends: as the
// input ended (EndOfInput) at the end of a recording, as ecu stopping (Stop)
// otherwise. It returns what stopped src, nil at its end: a replay tells a bad
// recording from an interrupted one by it. While m watches the feed (live,
// paddock#87), it waits for a packet no later than when the feed counts as lost,
// and then publishes the loss (next).
func drive(ctx context.Context, src salsa.Source, at func(salsa.Received) time.Time, m *race.Machine,
	eng *rules.Engine, pub publisher, rec *record.Writer,
) error {
	if eng != nil {
		// The rules read each frame as the machine leaves it (paddock#89).
		m.Rules(eng.Step)
	}
	for {
		r, err := next(ctx, src, m, pub)
		if errors.Is(err, io.EOF) {
			pub.Publish(m.EndOfInput())
			return nil
		}
		if err != nil {
			pub.Publish(m.Stop())
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
// loss, and waits on. A source's Next keeps a packet that comes as its context
// ends for the next call, as live's does.
func next(ctx context.Context, src salsa.Source, m *race.Machine, pub publisher) (salsa.Received, error) {
	for {
		due, ok := m.LostAt()
		if !ok {
			return src.Next(ctx)
		}
		wait, cancel := context.WithDeadline(ctx, due)
		r, err := src.Next(wait)
		cancel()
		if err == nil || ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			return r, err
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
