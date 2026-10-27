// Command ecu runs the template's example service: it reads its configuration
// from the environment, logs JSON to standard output and serves GET /healthz until
// it gets SIGINT or SIGTERM. `ecu -version` prints its version and exits.
//
// It connects to NATS at start, with the server and the source token from --nats
// and --src (TWR_NATS_URL and TWR_SRC), and refuses a source that is not one
// subject token. Nothing feeds the publisher yet: reading recordings is
// paddock#35, listening to the PS5 paddock#106.
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

	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/service"
)

// closeGrace is how long ecu waits at shutdown to send the events still queued.
const closeGrace = 5 * time.Second

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
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	logger := service.NewLogger(stdout, cfg.LogLevel)
	pub, err := bus.Connect(busCfg, logger, bus.Options{})
	if err != nil {
		logger.ErrorContext(ctx, "stopped", "err", err)
		return 1
	}
	defer func() {
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeGrace)
		defer cancel()
		pub.Close(grace)
	}()
	if err := service.Run(ctx, cfg, logger, version); err != nil {
		logger.ErrorContext(ctx, "stopped", "err", err)
		return 1
	}
	return 0
}
