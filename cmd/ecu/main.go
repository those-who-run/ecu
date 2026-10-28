// Command ecu listens to the PS5 live: it keeps GT7 sending with its heartbeat,
// reads each packet, runs it through the race state and publishes the frames and
// events on NATS, and, with TWR_ECU_RECORDINGS_DIR set, records the drive in v1's
// format (paddock#106, paddock#266). Without TWR_ECU_PS5_IP it logs an error and
// listens to nothing, serving GET /healthz and publishing nothing, as before. It reads its configuration from the
// environment, logs JSON to standard output and serves GET /healthz until it gets
// SIGINT or SIGTERM. `ecu -version` prints its version and exits.
//
// It connects to NATS at start, with the server and the source token from --nats
// and --src (TWR_NATS_URL and TWR_SRC), and refuses a source that is not one
// subject token. Live frames go out under the source live only (paddock#106, R8),
// so any other source stops ecu at start; replays, under their own source, are
// paddock#35.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/live"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/record"
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
	listening := false
	if err == nil {
		liveCfg, err = live.ConfigFromEnv(getenv)
		liveCfg.Listen = or(listenAddr, liveCfg.Listen)
		// Without the PS5's address ecu serves and publishes nothing, as the image's
		// smoke test runs it; the error is logged below.
		listening = err == nil
		if errors.Is(err, live.ErrNoPS5) {
			err = nil
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	logger := service.NewLogger(stdout, cfg.LogLevel)
	if !listening {
		logger.ErrorContext(ctx, "not listening to the PS5", "err", live.ErrNoPS5)
	}
	var rec *record.Writer
	if dir := getenv("TWR_ECU_RECORDINGS_DIR"); dir != "" && listening {
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
	if listening {
		src := live.Open(liveCfg, logger)
		driveCtx, stopDrive := context.WithCancel(ctx)
		driven := make(chan struct{})
		go func() {
			defer close(driven)
			start := src.Start()
			at := func(r salsa.Received) time.Time { return start.Add(time.Duration(r.T * float64(time.Second))) }
			drive(driveCtx, src, at, race.New(ulid.Make), pub, rec)
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
	}
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
// otherwise.
func drive(ctx context.Context, src salsa.Source, at func(salsa.Received) time.Time, m *race.Machine,
	pub publisher, rec *record.Writer,
) {
	for {
		r, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			pub.Publish(m.EndOfInput())
			return
		}
		if err != nil {
			pub.Publish(m.Stop())
			return
		}
		if rec != nil {
			rec.Write(r)
		}
		pub.Publish(m.Step(race.Received{Received: r, Time: at(r)}))
	}
}
