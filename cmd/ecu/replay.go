package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	"github.com/those-who-run/ecu/internal/bus"
	"github.com/those-who-run/ecu/internal/live"
	"github.com/those-who-run/ecu/internal/service"
	"github.com/those-who-run/ecu/internal/sessions"
	"github.com/those-who-run/ecu/internal/status"
	"github.com/those-who-run/ecu/race"
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
// with exit 2, and how long live's status writer waits for it before its first
// write: a var, so the tests can shorten it.
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
//
// Like live, it starts from the session a run before it under the same source
// left open in the `ecu` bucket (paddock#76): with --keep-open, a replay ends
// with its session kept there, as a stopped ecu leaves it, rather than ended, so
// a second replay under the same --src, its --clock set after the first's last
// frame, continues it or ends it. That is a restart pair.
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
	seed := flags.String("seed", "", "derive the run, session and event ids, and the default --src's suffix, from this seed "+
		"and the recording, so a second replay gives the same ids (default: random ids)")
	keepOpen := flags.Bool("keep-open", false, "at the end, keep the open session in the ecu bucket for the next replay "+
		"under the same --src, as a stopped ecu does, instead of ending it")
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
	var window time.Duration
	if err == nil {
		window, err = continueWindow(getenv)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ecu replay:", err)
		return exitNATS
	}
	// --seed's key needs the recording's sha256 (paddock#309), and so a read of it
	// before the replay's own: a recording that cannot be read is exit 1.
	var key *[32]byte
	if *seed != "" {
		digest, in, err := digestOf(path, stdin)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "ecu replay:", err)
			return exitRecording
		}
		stdin = in
		k := seedKey(*seed, digest)
		key = &k
	}
	busCfg := bus.Config{URL: *url}
	busCfg.Src, err = replaySrc(*src, *run, path, seededSuffix(key))
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
	eng, err := rulesEngine(getenv, logger)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ecu replay:", err)
		return exitNATS
	}
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
	logger.InfoContext(ctx, "replaying", "recording", path, "src", busCfg.Src, "speed", *speed, "clock", start.Format(time.RFC3339Nano),
		"seeded", key != nil)

	// The replay's status goes under its own source, never live's (paddock#88,
	// R6), from a goroutine of its own, as live's does.
	kv, err := pub.KV(statusBucket)
	if err != nil {
		closeQuickly(ctx, pub)
		_, _ = fmt.Fprintln(stderr, "ecu replay:", err)
		return exitNATS
	}
	tracker := newTracker(busCfg.Src, live.DefaultTelemetryTimeout, eng, nil)
	writeCtx, stopWriting := context.WithCancel(context.WithoutCancel(ctx))
	written := make(chan struct{})
	go func() {
		defer close(written)
		status.NewWriter(tracker, kv, logger).Run(writeCtx, statusGrace)
	}()

	// The session a replay before it under the same source left open
	// (--keep-open), to continue or end, as live's (paddock#76).
	ecuKV, err := pub.KV(sessions.Bucket)
	if err != nil {
		stopWriting()
		<-written
		closeQuickly(ctx, pub)
		_, _ = fmt.Fprintln(stderr, "ecu replay:", err)
		return exitNATS
	}
	open, rev, read := loadKept(ctx, ecuKV, nil, busCfg.Src, logger) // connected already
	keeper := sessions.New(ecuKV, busCfg.Src, rev, waiting{ctx, pub}, logger)
	keeper.Replay()
	if read {
		keeper.Loaded(rev)
	}
	// The record's last write goes after the status's, each with its own grace.
	keepCtx, stopKeeping := context.WithCancel(context.WithoutCancel(ctx))
	keptDone := make(chan struct{})
	go func() {
		defer close(keptDone)
		keeper.Run(keepCtx, statusGrace)
	}()

	paced := &pacer{src: rec, speed: factor, clock: start, last: start}
	m := race.New(replayIDs(key, busCfg.Src, paced.now))
	m.Restore(open, window)
	// A replay's frames carry their own clock, so the window is checked on each
	// step, not by a timer.
	sums := &sessionSums{}
	failed := drive(ctx, paced, paced.at, m, eng, summing{waiting{ctx, pub}, sums}, nil, tracker,
		&keeping{k: keeper, keepOpen: *keepOpen})
	sums.keptOpen(m.Open())
	// The status's last values, the session ended and the replay idle, and the
	// open session's record go before the publisher closes.
	tracker.Stop()
	stopWriting()
	<-written
	stopKeeping()
	<-keptDone

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
	sums.log(ctx, logger)
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
	case s.DroppedFrames > 0 || s.DroppedEvents > 0 || s.DroppedCallouts > 0:
		_, _ = fmt.Fprintf(stderr, "ecu replay: NATS did not take everything: %d frames, %d events and %d callouts dropped\n",
			s.DroppedFrames, s.DroppedEvents, s.DroppedCallouts)
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
// replay-<stem>-<suffix>, the recording's file stem made into a subject token and
// suffix, or a random one when suffix is "", so two replays of one file do not
// share a source unless they share a --seed.
func replaySrc(src, run, path, suffix string) (string, error) {
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
	if suffix == "" {
		suffix = strings.ToLower(rand.Text()[:6]) // 30 random bits, [a-z2-7]
	}
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
	last    time.Time // the last packet's time (at), clock before the first
}

func (p *pacer) Next(ctx context.Context) (salsa.Received, error) {
	r, err := p.src.Next(ctx)
	if err != nil {
		return r, err
	}
	p.n++
	if !p.started {
		p.started, p.t0, p.wall = true, r.T, time.Now()
		p.last = p.at(r)
		return r, nil
	}
	p.last = p.at(r)
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

// now is the replay's clock: the time of the packet last read, or clock before
// the first. A seeded replay's ULIDs take their time from it.
func (p *pacer) now() time.Time { return p.last }

func seconds(s float64) time.Duration { return time.Duration(math.Round(s * float64(time.Second))) }

// digestOf is the recording's sha256, over its bytes as stored (still gzipped
// for a .gz), and the standard input to replay from: standard input is read
// whole for the digest and replayed from memory.
func digestOf(path string, stdin io.Reader) ([]byte, io.Reader, error) {
	h := sha256.New()
	if path == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, nil, fmt.Errorf("stdin: %w", err)
		}
		h.Write(data)
		return h.Sum(nil), bytes.NewReader(data), nil
	}
	f, err := os.Open(path) //nolint:gosec // the recording the caller named
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(h, f); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return h.Sum(nil), stdin, nil
}

// seedKey is what a seeded replay's ids come from (paddock#35, paddock#309): the
// seed and the recording's sha256, so one seed replays two recordings to
// different ids, and the same recording and seed to the same ones.
func seedKey(seed string, digest []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("ecu replay --seed\x00"))
	h.Write([]byte(seed))
	h.Write([]byte{0})
	h.Write(digest)
	return [32]byte(h.Sum(nil))
}

// seededSuffix is the default --src's suffix under --seed, from the key, so a
// seeded replay with the default source is reproducible too; "" (random) with no
// seed.
func seededSuffix(key *[32]byte) string {
	if key == nil {
		return ""
	}
	return strings.ToLower(base32.StdEncoding.EncodeToString(key[:]))[:6]
}

// replayIDs is the replay's ULID generator. Without a seed it is live's,
// ulid.Make: random ids on each run. With one (paddock#309), the ULIDs' time is
// the replay's own clock and their entropy a ChaCha8 stream keyed by the seed's
// key and the source, so the same recording, seed, --src and --clock give the
// same run id and session ids, and so the same frame, event and callout ids. The
// source is in the key because a frame's id, <run>:frame:<seq>, has no <src>:
// two seeded replays of one recording under two sources must not share a run id,
// or TELEMETRY would drop the second's frames as duplicates.
func replayIDs(key *[32]byte, src string, now func() time.Time) race.IDs {
	if key == nil {
		return ulid.Make
	}
	stream := sha256.Sum256(append(append(key[:], 0), src...))
	return race.NewIDs(now, mrand.NewChaCha8(stream))
}

// sessionSum is one session of a replay, as its summary line says it
// (paddock#35, R5): its kind, laps completed and why it ended.
type sessionSum struct {
	id    string
	kind  eventsv1.SessionType
	laps  int32
	ended string
}

// sessionSums are the replay's sessions, in the order they first appear: one
// started, continued or ended in the replay.
type sessionSums struct {
	all []*sessionSum
}

func (s *sessionSums) get(id string) *sessionSum {
	for _, sum := range s.all {
		if sum.id == id {
			return sum
		}
	}
	sum := &sessionSum{id: id}
	s.all = append(s.all, sum)
	return sum
}

// see reads the sessions' facts from the events, as a subscriber would.
func (s *sessionSums) see(outs []race.Output) {
	for _, o := range outs {
		switch {
		case o.Started != nil:
			s.get(o.Started.GetSessionId()).kind = o.Started.GetSessionType()
		case o.Continued != nil:
			s.get(o.Continued.GetSessionId())
		case o.Kind != nil:
			if t := o.Kind.GetKind().GetSessionType(); t != eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED {
				s.get(o.Kind.GetSessionId()).kind = t
			}
		case o.Ended != nil:
			sum := s.get(o.Ended.GetSessionId())
			s.summed(sum, o.Ended.GetSummary())
			sum.ended = strings.TrimPrefix(o.Ended.GetReason().String(), "END_REASON_")
		}
	}
}

func (s *sessionSums) summed(sum *sessionSum, summary *eventsv1.SessionSummary) {
	sum.laps = summary.GetLapsCompleted()
	if t := summary.GetKind().GetSessionType(); t != eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED {
		sum.kind = t
	} else if t := summary.GetSessionType(); t != eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED {
		sum.kind = t
	}
}

// keptOpen is the session --keep-open left open at the end, if any: not ended.
func (s *sessionSums) keptOpen(open *statev1.OpenSession) {
	if open.GetSessionId() == "" {
		return
	}
	sum := s.get(open.GetSessionId())
	if sum.ended == "" {
		s.summed(sum, open.GetSummary())
		sum.ended = "KEPT_OPEN"
	}
}

// log logs one line per session, the replay's summary at its end (R5).
func (s *sessionSums) log(ctx context.Context, logger *slog.Logger) {
	for _, sum := range s.all {
		logger.InfoContext(ctx, "replayed session", "session", sum.id,
			"kind", strings.TrimPrefix(sum.kind.String(), "SESSION_TYPE_"), "laps", sum.laps, "ended", sum.ended)
	}
}

// summing is a publisher that notes each session's facts for the summary before
// it publishes.
type summing struct {
	publisher
	sums *sessionSums
}

func (s summing) Publish(outs []race.Output) {
	s.sums.see(outs)
	s.publisher.Publish(outs)
}
