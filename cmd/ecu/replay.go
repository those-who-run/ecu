package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/service"
	"github.com/those-who-run/salsa"
)

// The replay's exit codes (paddock#35, R1): a recording that cannot be read is 1,
// naming the file and the line; a NATS server that cannot be reached within
// connectTimeout, or that did not take everything the replay published, is 2, as
// is a bad flag. A replay stopped by SIGINT or SIGTERM is 130.
const (
	exitRecording   = 1
	exitNATS        = 2
	exitInterrupted = 130
)

// connectTimeout is how long a replay waits for the NATS server before it gives up
// with exit 2: a var, so the tests can shorten it.
var connectTimeout = 10 * time.Second

// replayGrace is how long a replay waits at the end for the server to take and
// acknowledge what is still queued. Longer than live's: a replay that gives up
// loses what it was run for. interruptGrace is what is left of it once SIGINT or
// SIGTERM comes. Vars, so the tests can shorten them.
var (
	replayGrace    = 30 * time.Second
	interruptGrace = time.Second
)

// replayOptions are the publisher's limits for a replay: the defaults, which the
// tests shrink so the replay waits on the hand-over often.
var replayOptions = bus.Options{}

// defaultClock is the wall-clock time of a replay's first frame without --clock:
// v1's t is the recording machine's uptime, with no date, so a replay dates itself
// from this fixed time and is the same on every run (paddock#35, decision of
// 2026-10-26).
var defaultClock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// replay is `ecu replay`: it reads a v1 recording through salsa's recording Source,
// drives the race state with it and publishes what it gives, exactly as live does
// from the packet on, then ends the open session, flushes and exits. It returns the
// exit code. It never records and needs no PS5.
func replay(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	flags := flag.NewFlagSet("ecu replay", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: ecu replay [flags] PATH\n\n"+
			"Replays a v1 recording (.jsonl, .jsonl.gz, or - for standard input) onto NATS.\n\nflags:")
		flags.PrintDefaults()
	}
	url := flags.String("nats", or(getenv("TWR_NATS_URL"), bus.DefaultURL), "the NATS server to publish to (TWR_NATS_URL)")
	src := flags.String("src", "", "the source token: parity or replay-<run> (default replay-<file stem>-<random suffix>)")
	run := flags.String("run", "", "publish under replay-<run>, with no random suffix")
	speed := flags.String("speed", "1", "1 for the recorded pacing, any factor above 0, or max for no waiting")
	clock := flags.String("clock", "", "the first frame's wall-clock time: ISO 8601 (UTC unless it says) or epoch seconds "+
		"(default 2026-01-01T00:00:00Z)")
	path, err := parseInterspersed(flags, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if !errors.Is(err, errFlag) {
			_, _ = fmt.Fprintln(stderr, "ecu replay:", err)
		}
		return exitNATS
	}
	cfg, err := service.ConfigFromEnv(getenv)
	var (
		factor float64
		start  = defaultClock
	)
	if err == nil {
		factor, err = parseSpeed(*speed)
	}
	if err == nil && *clock != "" {
		start, err = parseClock(*clock)
	}
	busCfg := bus.Config{URL: *url}
	if err == nil {
		busCfg.Src, err = replaySrc(*src, *run, path)
	}
	if err == nil {
		err = busCfg.Validate()
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ecu replay:", err)
		return exitNATS
	}

	rec, err := openReplay(path, stdin)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ecu replay:", err)
		return exitRecording
	}
	defer func() { _ = rec.Close() }()

	logger := service.NewLogger(stdout, cfg.LogLevel)
	pub, err := bus.Connect(busCfg, logger, replayOptions)
	if err == nil {
		wait, cancel := context.WithTimeout(ctx, connectTimeout)
		err = pub.WaitConnected(wait)
		cancel()
		if err != nil {
			err = fmt.Errorf("NATS at %s not reached within %v", *url, connectTimeout)
			closeQuickly(ctx, pub)
		}
	}
	if err != nil {
		logger.ErrorContext(ctx, "stopped", "err", err)
		_, _ = fmt.Fprintln(stderr, "ecu replay:", err)
		return exitNATS
	}
	logger.InfoContext(ctx, "replaying", "recording", path, "src", busCfg.Src, "speed", *speed, "clock", start.Format(time.RFC3339Nano))

	paced := &pacer{src: rec, speed: factor, clock: start}
	failed := drive(ctx, paced, paced.at, race.New(ulid.Make), waiting{ctx, pub}, nil)

	logger.InfoContext(ctx, "replayed: flushing", "packets", paced.n)

	// The flush gets replayGrace, cut to interruptGrace once a signal comes:
	// NotifyContext takes every later signal, so Ctrl-C must not wait 30 s.
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), replayGrace)
	defer cancel()
	cut := interruptGrace // read here: the func below may run after replay returns
	stopCut := context.AfterFunc(ctx, func() { time.AfterFunc(cut, cancel) })
	defer stopCut()
	pub.Close(grace)
	// The totals are final only once Closed is: the publisher counts what it
	// drops when the grace runs out after Close has returned.
	final := false
	select {
	case <-pub.Closed():
		final = true
	case <-time.After(closeAfter):
	}
	s := pub.Stats()
	waitGone(pub)
	var re *salsa.RecordingError
	switch {
	case errors.As(failed, &re):
		_, _ = fmt.Fprintln(stderr, "ecu replay:", failed)
		return exitRecording
	case failed != nil || ctx.Err() != nil:
		_, _ = fmt.Fprintln(stderr, "ecu replay: stopped:", context.Cause(ctx))
		return exitInterrupted
	case !final:
		_, _ = fmt.Fprintln(stderr, "ecu replay: NATS still held the publisher at the end: what it took is not known")
		return exitNATS
	case s.DroppedFrames > 0 || s.DroppedEvents > 0:
		_, _ = fmt.Fprintf(stderr, "ecu replay: NATS did not take everything: %d frames and %d events dropped\n",
			s.DroppedFrames, s.DroppedEvents)
		return exitNATS
	case s.Unflushed:
		_, _ = fmt.Fprintln(stderr, "ecu replay: the last frames may not have reached NATS: the final flush failed")
		return exitNATS
	}
	return 0
}

// closeQuickly closes a publisher that never connected.
func closeQuickly(ctx context.Context, pub *bus.Publisher) {
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	pub.Close(grace)
	waitGone(pub)
}

// goneAfter bounds the wait for the client's last callback: closing a connection
// to a server that stopped reading can take a write timeout (2 s).
const goneAfter = 3 * time.Second

// waitGone waits, a little, for the connection to close and the client's last
// callback to run, so nothing logs once replay returns and the last log lines come
// before the process exits.
func waitGone(pub *bus.Publisher) {
	select {
	case <-pub.Gone():
	case <-time.After(goneAfter):
	}
}

// waiting publishes with PublishWait: a replay waits for room in the publisher
// rather than dropping a frame (paddock#35).
type waiting struct {
	ctx context.Context // the drive's publisher interface has none of its own
	pub *bus.Publisher
}

func (w waiting) Publish(outs []race.Output) { w.pub.PublishWait(w.ctx, outs) }

// errFlag is a flag error the flag package has already printed with the usage.
var errFlag = errors.New("bad flag")

// parseInterspersed parses flags before and after the one argument, the
// recording's path, so `ecu replay x.jsonl.gz --speed max` works as well as
// `ecu replay --speed max x.jsonl.gz`.
func parseInterspersed(flags *flag.FlagSet, args []string) (string, error) {
	var paths []string
	for {
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return "", err
			}
			return "", errFlag
		}
		args = flags.Args()
		if len(args) == 0 {
			break
		}
		paths = append(paths, args[0])
		args = args[1:]
	}
	if len(paths) != 1 {
		return "", fmt.Errorf("one recording, a path or -, is needed; got %d", len(paths))
	}
	return paths[0], nil
}

// parseSpeed is --speed as a factor: 0 for max, which never waits.
func parseSpeed(s string) (float64, error) {
	if s == "max" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || !(f > 0) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("--speed %q: a factor above 0, or max", s)
	}
	return f, nil
}

// clockLayouts are the ISO 8601 forms --clock takes; one with no zone is UTC.
var clockLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04",
	"2006-01-02",
}

// parseClock is --clock: ISO 8601, UTC unless it names a zone, or seconds since
// the Unix epoch.
func parseClock(s string) (time.Time, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
		sec, frac := math.Modf(f)
		return time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC(), nil
	}
	for _, layout := range clockLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--clock %q: an ISO 8601 time, such as 2026-01-01T00:00:00Z, or epoch seconds", s)
}

// replaySrc is the <src> a replay publishes under (paddock#35, R3): --src as
// given, but never live, which is the PS5's alone; replay-<run> for --run; or
// replay-<stem>-<suffix>, the recording's file stem made into a subject token and a
// random suffix, so two replays of one file do not share a source.
func replaySrc(src, run, path string) (string, error) {
	switch {
	case src != "" && run != "":
		return "", errors.New("--src and --run: give one or the other")
	case src == bus.DefaultSrc:
		return "", fmt.Errorf("src %q is the PS5's: a replay publishes under parity or replay-<run>", src)
	case src != "":
		return src, nil
	case run != "":
		return "replay-" + run, nil
	}
	suffix := strings.ToLower(rand.Text()[:6]) // 30 random bits, [a-z2-7]
	stem := stemOf(path)
	// replay-<run>: a run of 64 at most.
	if room := 64 - len(suffix) - 1; len(stem) > room {
		stem = strings.TrimRight(stem[:room], "-")
	}
	return "replay-" + stem + "-" + suffix, nil
}

// stemOf is the recording's file name without .gz and its extension, lower-cased,
// with anything outside [a-z0-9-] replaced by -: real-race-2laps for
// Real Race 2laps.jsonl.gz. Standard input is stdin.
func stemOf(path string) string {
	if path == "-" {
		return "stdin"
	}
	name := strings.TrimSuffix(filepath.Base(path), ".gz")
	name = strings.TrimSuffix(name, filepath.Ext(name))
	b := []byte(strings.ToLower(name))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			b[i] = '-'
		}
	}
	// A run starts with a letter or a digit.
	if stem := strings.Trim(string(b), "-"); stem != "" {
		return stem
	}
	return "recording"
}

// openReplay opens the recording at path, or standard input for -.
func openReplay(path string, stdin io.Reader) (salsa.Source, error) {
	if path == "-" {
		return salsa.NewRecording(stdin, "stdin")
	}
	return salsa.OpenRecording(path)
}

// pacer gives a recording's packets at the recording's own pace times speed, or
// as fast as they come when speed is 0 (max), and dates them: the first at clock,
// each later one clock plus its time since the first, whatever the speed.
type pacer struct {
	src   salsa.Source
	speed float64
	clock time.Time

	n       int // packets read
	started bool
	t0      float64   // the first packet's t
	wall    time.Time // when the first packet was given, for the pacing
}

func (p *pacer) Next(ctx context.Context) (salsa.Received, error) {
	r, err := p.src.Next(ctx)
	if err != nil {
		return r, err
	}
	p.n++
	if !p.started {
		p.started, p.t0, p.wall = true, r.T, time.Now()
		return r, nil
	}
	if p.speed == 0 {
		return r, nil
	}
	// Each packet waits for its own time from the first, not from the one before,
	// so the waits' small overruns do not add up over a long recording.
	due := p.wall.Add(seconds((r.T - p.t0) / p.speed))
	if wait := time.Until(due); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return salsa.Received{}, ctx.Err()
		}
	}
	return r, nil
}

func (p *pacer) Close() error { return p.src.Close() }

// at is a packet's wall-clock time: clock plus its time since the first packet.
func (p *pacer) at(r salsa.Received) time.Time { return p.clock.Add(seconds(r.T - p.t0)) }

func seconds(s float64) time.Duration { return time.Duration(math.Round(s * float64(time.Second))) }
