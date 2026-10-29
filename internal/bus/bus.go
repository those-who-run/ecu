// Package bus is ecu's NATS adapter (paddock#34): it puts the race state's
// outputs on the bus. Each frame is a core publish on twr.v1.<src>.frame.gt7, which
// the TELEMETRY stream captures, and at most 20 a second of frame time go on
// twr.v1.<src>.frame.gt7.lo too. session.started, session.track, session.kind,
// pit.entered, pit.exited, lap.completed and session.ended go on
// twr.v1.<src>.evt.<sid>.<area>.<what>, which the EVENTS stream captures and
// deduplicates on Nats-Msg-Id; each is published through
// JetStream and sent again until EVENTS acknowledges it. Every message carries the
// full set of CloudEvents headers, from contracts' ce package. It is the only
// package that imports nats.go.
//
// Nothing on the live frame path waits on the network: Publish encodes the outputs
// and hands them to one goroutine, which owns the connection, without waiting. That
// goroutine may wait on the network (a server that stops reading holds a write for
// up to its timeout); live's step loop never does. A replay publishes with
// PublishWait instead, which waits for room rather than dropping (paddock#35).
package bus

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/those-who-run/contracts/ce"
	"github.com/those-who-run/ecu/internal/race"
)

// Options are the publisher's limits. The zero value takes the defaults.
type Options struct {
	// QueueSize bounds the events waiting to be sent and acknowledged: QueueSize in
	// the publishing goroutine's queue and QueueSize more in the hand-over to it,
	// 2 × 1,024 = 2,048 by default, hours of laps. An event beyond them is dropped,
	// counted and logged as an error, never silently, and never waited for.
	QueueSize int
	// FrameBuffer is how many frames wait for the publishing goroutine: 1,024 by
	// default, about 17 s at 60 Hz. A frame beyond it is dropped and counted.
	FrameBuffer int
	// ReconnectWait is the pause between attempts to reach the server again, and
	// before an event that was not acknowledged is sent again: 1 s by default.
	ReconnectWait time.Duration

	// ackTimeout overrides ackTimeout, for the tests.
	ackTimeout time.Duration
}

// loGap is the least frame time between two frames on the .lo subject: 50 ms,
// 20 Hz at most (R2).
const loGap = 0.05

// dialer is the client's dialer: it keeps the connection it made last, so Close can
// cut a connection whose writes hold the publishing goroutine.
type dialer struct {
	d    net.Dialer
	mu   sync.Mutex
	last net.Conn
}

func (d *dialer) Dial(network, address string) (net.Conn, error) {
	c, err := d.d.Dial(network, address)
	if err == nil {
		d.mu.Lock()
		d.last = c
		d.mu.Unlock()
	}
	return c, err
}

// cut closes the connection made last, if there is one.
func (d *dialer) cut() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last != nil {
		_ = d.last.Close()
	}
}

// The client's timeouts. writeTimeout bounds one write to a server that stops
// reading; the client keeps the connection after it, so the next write can wait as
// long again, and Close cuts the connection when one holds it at the end. ackTimeout
// is how long an event waits for EVENTS' acknowledgement before it is sent again,
// and connectTimeout how long one attempt to reach the server takes at most, the
// client's own default.
const (
	writeTimeout   = 2 * time.Second
	ackTimeout     = 5 * time.Second
	connectTimeout = nats.DefaultTimeout
)

// Stats is what was published and dropped. Events counts the acknowledgements
// from EVENTS, a duplicate's included. Unflushed is set by Close when the frames
// the client held at the end may not have reached the server: the final flush
// failed, the connection was down, or the time ran out.
type Stats struct {
	Frames, LoFrames, Events                      uint64
	DroppedFrames, DroppedLoFrames, DroppedEvents uint64
	Unflushed                                     bool
}

// frameMsg is a frame for the publishing goroutine, with whether it goes on .lo
// too.
type frameMsg struct {
	msg *nats.Msg
	lo  bool
}

// Publisher publishes the race state's outputs under one <src>. Publish is called
// from one goroutine, the step loop's; everything that touches the connection
// runs on the publisher's own goroutine.
type Publisher struct {
	nc     *nats.Conn
	conns  *dialer
	js     jetstream.JetStream
	src    string
	logger *slog.Logger
	limit  int
	wait   time.Duration

	frames chan frameMsg
	events chan *nats.Msg
	quit   chan struct{}   // closed by Close
	until  context.Context // Close's deadline, set before quit is closed
	done   chan struct{}   // the publishing goroutine has ended
	closed chan struct{}   // the connection is closed and the final totals logged
	gone   chan struct{}   // the connection is closed and the client's last callback has run

	// The step loop's own: the frame time of the last frame put on .lo.
	lastLo float64
	sentLo bool

	counters struct {
		frames, lo, events                      atomic.Uint64
		droppedFrames, droppedLo, droppedEvents atomic.Uint64
		unflushed                               atomic.Bool
	}
}

// Connect is a publisher on the server cfg names, under cfg.Src, which Validate
// must pass. Its first attempt to reach the server can take the client's connect
// timeout (2 s); after that, and after any loss of the connection, the client
// keeps trying in the background, every ReconnectWait.
func Connect(cfg Config, logger *slog.Logger, opts Options) (*Publisher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1024
	}
	if opts.FrameBuffer <= 0 {
		opts.FrameBuffer = 1024
	}
	if opts.ackTimeout <= 0 {
		opts.ackTimeout = ackTimeout
	}
	if opts.ReconnectWait <= 0 {
		opts.ReconnectWait = time.Second
	}
	d := &dialer{d: net.Dialer{Timeout: connectTimeout}}
	gone := make(chan struct{})
	nc, err := nats.Connect(cfg.URL,
		nats.Name("ecu"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(opts.ReconnectWait),
		// No buffer of the client's while it reconnects: a frame published then is
		// dropped at once, and an event waits in the publisher's queue.
		nats.ReconnectBufSize(-1),
		nats.FlusherTimeout(writeTimeout),
		nats.SetCustomDialer(d),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("NATS connected again", "server", nc.ConnectedUrlRedacted())
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.Warn("NATS connection lost", "err", err)
			}
		}),
		// The client runs its callbacks in order on a goroutine of its own, the
		// closed one last: after it, nothing the client does logs.
		nats.ClosedHandler(func(*nats.Conn) { close(gone) }),
	)
	if err != nil {
		return nil, err
	}
	// One event in flight at a time keeps EVENTS in the order the events happened.
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncTimeout(opts.ackTimeout), jetstream.WithPublishAsyncMaxPending(1))
	if err != nil {
		nc.Close()
		return nil, err
	}
	p := &Publisher{
		nc: nc, conns: d, js: js, src: cfg.Src, logger: logger, limit: opts.QueueSize, wait: opts.ReconnectWait,
		frames: make(chan frameMsg, opts.FrameBuffer),
		events: make(chan *nats.Msg, opts.QueueSize),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
		gone:   gone,
	}
	go p.run()
	logger.Info("publishing", "nats", redacted(cfg.URL), "src", cfg.Src)
	return p, nil
}

// Publish publishes a step's outputs: each frame on the frame subject, and on .lo
// when it is 50 ms of frame time or more after the last one there, and each event
// on its own subject. SessionStarted's source is set to the publisher's <src>
// here, as only the publisher knows it. It encodes the outputs and hands them on
// without waiting: a frame that finds the hand-over full is dropped and counted,
// and so is an event that finds the queue full, with an error logged.
func (p *Publisher) Publish(outs []race.Output) { p.publish(nil, outs) }

// PublishWait is Publish for a replay (paddock#35): a frame or an event that finds
// the hand-over or the queue full waits for room, until ctx is done, instead of
// being dropped, so a replay at --speed max goes no faster than the server takes
// its frames and loses none. Once ctx is done it drops as Publish does. Live uses
// Publish, so its step loop never waits on the network.
func (p *Publisher) PublishWait(ctx context.Context, outs []race.Output) { p.publish(ctx.Done(), outs) }

// publish publishes outs, waiting for room until wait is closed; a nil wait never
// waits.
func (p *Publisher) publish(wait <-chan struct{}, outs []race.Output) {
	for _, o := range outs {
		if o.Frame != nil {
			p.frame(o, wait)
		} else {
			p.event(o, wait)
		}
	}
}

// handOver sends m on ch, waiting for room until wait is closed, and says whether
// it was sent.
func handOver[T any](ch chan<- T, m T, wait <-chan struct{}) bool {
	select {
	case ch <- m:
		return true
	default:
	}
	if wait == nil {
		return false
	}
	select {
	case ch <- m:
		return true
	case <-wait:
		return false
	}
}

func (p *Publisher) frame(o race.Output, wait <-chan struct{}) {
	t := o.Frame.GetT()
	// A frame time that goes back is a new clock (another recording, or GT7 started
	// over): the .lo subject starts again from it.
	lo := !p.sentLo || t-p.lastLo >= loGap || t < p.lastLo
	if lo {
		p.lastLo, p.sentLo = t, true
	}
	h, data, err := ce.Encode(o.Frame, o.Event)
	if err != nil {
		p.logger.Error("frame not encoded", "seq", o.Frame.GetSeq(), "err", err)
		p.dropFrame(lo)
		return
	}
	msg := &nats.Msg{Subject: "twr.v1." + p.src + ".frame.gt7", Header: nats.Header(h), Data: data}
	if !handOver(p.frames, frameMsg{msg, lo}, wait) {
		p.dropFrame(lo)
	}
}

func (p *Publisher) dropFrame(lo bool) {
	p.counters.droppedFrames.Add(1)
	if lo {
		p.counters.droppedLo.Add(1)
	}
}

// event publishes a session event. Its id (R4, as amended by paddock#279) is the
// race state's, which follows the catalogue's <sid>:<area>.<what>:<part>, with the
// source in front, <src>:, because EVENTS deduplicates by Nats-Msg-Id across the
// whole stream and two sources can carry the same session id. Its subject's
// <area>.<what> comes from its ce-type, and an event with no session open goes on
// evt._, its id then starting <src>:<run>: as the race state gives it.
func (p *Publisher) event(o race.Output, wait <-chan struct{}) {
	if o.Started == nil && o.Lap == nil && o.Track == nil && o.Kind == nil && o.Ended == nil &&
		o.PitEntered == nil && o.PitExited == nil && o.PitTeleport == nil {
		return
	}
	if o.Started != nil {
		o.Started.Source = p.src
	}
	e := o.Event
	e.ID = p.src + ":" + e.ID
	h, data, err := ce.Encode(o.Message(), e)
	if err != nil {
		p.logger.Error("event not encoded", "id", e.ID, "err", err)
		p.counters.droppedEvents.Add(1)
		return
	}
	sid := o.Session
	if sid == "" {
		// No session open: unreachable until events can come with no session
		// (Prost); TestAnEventWithNoSessionGoesOnEvtUnderscore holds the rule.
		sid = "_"
	}
	msg := &nats.Msg{Subject: "twr.v1." + p.src + ".evt." + sid + "." + areaWhat(h.Get(ce.TypeHeader)), Header: nats.Header(h), Data: data}
	if !handOver(p.events, msg, wait) {
		p.counters.droppedEvents.Add(1)
		p.logger.Error("event dropped: the queue is full", "id", e.ID, "queue", p.limit)
	}
}

// areaWhat is a ce-type's <area>.<what>: lap.completed for
// run.thosewho.lap.completed.v1.
func areaWhat(ceType string) string {
	t := strings.TrimPrefix(ceType, "run.thosewho.")
	if i := strings.LastIndex(t, ".v"); i >= 0 {
		t = t[:i]
	}
	return t
}

// run is the publishing goroutine: it owns the connection. Frames go out as they
// come, or are dropped while the connection is down. Events go out one at a time,
// in order: the next is sent only once EVENTS has acknowledged the one before, and
// one that is not acknowledged (the connection went, or the ack timed out) is sent
// again after ReconnectWait. A copy that EVENTS did store is dropped there as a
// duplicate within its 2 min window.
func (p *Publisher) run() {
	defer close(p.done)
	var (
		queue    []*nats.Msg // events taken, oldest first; queue[0] is in flight or next
		inflight jetstream.PubAckFuture
		retry    *time.Timer
		retryC   <-chan time.Time
		closing  context.Context
		quit     = p.quit
		lastErr  string
		warned   bool // the missing acknowledgement was logged
	)
	for {
		if closing != nil && closing.Err() != nil {
			// Out of time: what is left is dropped, before anything more waits on
			// the network.
			for range len(p.frames) {
				p.dropFrame((<-p.frames).lo)
			}
			p.counters.droppedEvents.Add(uint64(len(queue) + len(p.events)))
			return
		}
		if inflight == nil && retryC == nil && len(queue) > 0 {
			f, err := p.js.PublishMsgAsync(queue[0])
			if err == nil {
				inflight = f
			} else {
				lastErr = p.notSent(queue[0], err, lastErr)
				retry = time.NewTimer(p.wait)
				retryC = retry.C
			}
		}
		if closing != nil && len(queue) == 0 && len(p.events) == 0 && len(p.frames) == 0 {
			return
		}
		var ok <-chan *jetstream.PubAck
		var failed <-chan error
		if inflight != nil {
			ok, failed = inflight.Ok(), inflight.Err()
		}
		events := p.events
		if len(queue) >= p.limit {
			events = nil // the queue is full: the hand-over fills, then the step drops
		}
		var deadline <-chan struct{}
		if closing != nil {
			deadline = closing.Done()
		}
		select {
		case f := <-p.frames:
			p.sendFrame(f)
		case m := <-events:
			queue = append(queue, m)
		case <-ok:
			p.counters.events.Add(1)
			queue[0] = nil
			queue, inflight, lastErr = queue[1:], nil, ""
		case err := <-failed:
			if !warned && (errors.Is(err, jetstream.ErrAsyncPublishTimeout) || errors.Is(err, jetstream.ErrNoStreamResponse)) {
				warned = true
				p.logger.Warn("EVENTS did not acknowledge an event: is the stream there? "+
					"Each event is sent again until it is, and core subscribers see the copies",
					"id", queue[0].Header.Get(ce.IDHeader), "err", err)
			}
			lastErr = p.notSent(queue[0], err, lastErr)
			inflight = nil
			retry = time.NewTimer(p.wait)
			retryC = retry.C
		case <-retryC:
			retryC = nil
		case <-quit:
			closing, quit = p.until, nil
		case <-deadline:
			// The check at the top drops what is left.
		}
	}
}

// notSent logs an event that was not sent or acknowledged, once for each new
// reason, and returns the reason.
func (p *Publisher) notSent(m *nats.Msg, err error, last string) string {
	if err.Error() != last {
		p.logger.Warn("event not acknowledged, sending it again", "id", m.Header.Get(ce.IDHeader), "err", err)
	}
	return err.Error()
}

func (p *Publisher) sendFrame(f frameMsg) {
	if !p.nc.IsConnected() || p.nc.PublishMsg(f.msg) != nil {
		p.dropFrame(f.lo)
		return
	}
	p.counters.frames.Add(1)
	if !f.lo {
		return
	}
	lo := &nats.Msg{Subject: f.msg.Subject + ".lo", Header: f.msg.Header, Data: f.msg.Data}
	if p.nc.PublishMsg(lo) != nil {
		p.counters.droppedLo.Add(1)
		return
	}
	p.counters.lo.Add(1)
}

// WaitConnected waits until the publisher is connected to the server, and returns
// ctx's error if it is not before ctx is done. Connect returns before the first
// connection is made, and the client keeps trying in the background, which suits
// live; a replay waits here instead, so a server that cannot be reached stops it
// (paddock#35).
func (p *Publisher) WaitConnected(ctx context.Context) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !p.nc.IsConnected() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return nil
}

// Stats is what has been published and dropped so far.
func (p *Publisher) Stats() Stats {
	c := &p.counters
	return Stats{
		Frames: c.frames.Load(), LoFrames: c.lo.Load(), Events: c.events.Load(),
		DroppedFrames: c.droppedFrames.Load(), DroppedLoFrames: c.droppedLo.Load(), DroppedEvents: c.droppedEvents.Load(),
		Unflushed: c.unflushed.Load(),
	}
}

// Close publishes what Publish has handed over and waits for EVENTS to acknowledge
// every event, until ctx is done; the events still waiting then are counted as
// dropped. It then flushes the frames the client holds, closes the connection, and
// logs and returns the totals (R10), with "final" true.
//
// It returns once ctx is done even if the publishing goroutine is still held by a
// write. The totals it logs and returns then have "final" false, and it cuts the
// connection, so the write gives up at once: the goroutine counts what it drops,
// then logs the final totals itself and closes the connection. Closed says when that is done.
// Publish must not be called after Close, and Close only once.
func (p *Publisher) Close(ctx context.Context) Stats {
	p.until = ctx
	close(p.quit)
	select {
	case <-p.done:
	case <-ctx.Done():
	}
	select {
	case <-p.done: // in time, or just as the time ran out
		var err error
		if ctx.Err() == nil && p.nc.IsConnected() {
			err = p.nc.FlushWithContext(ctx)
		} else {
			p.counters.unflushed.Store(true)
		}
		if err != nil {
			p.counters.unflushed.Store(true)
		}
		s := p.logTotals(ctx, true)
		if err != nil && !errors.Is(err, context.Canceled) {
			p.logger.WarnContext(ctx, "the last frames may not have reached the server", "err", err)
		}
		close(p.closed)
		if ctx.Err() != nil {
			go p.nc.Close() // it flushes, and may wait on a server that stops reading
		} else {
			p.nc.Close()
		}
		return s
	default:
	}
	p.counters.unflushed.Store(true)
	s := p.logTotals(ctx, false)
	p.logger.WarnContext(ctx, "the publisher was still writing when the time ran out: the totals follow once it stops")
	// A write to a server that stopped reading gives up only after writeTimeout, and
	// the client does not drop the connection when it does, so the next write waits
	// as long again, the client's own flusher's and the publisher's in turn, for as
	// long as they keep coming. Closing the socket ends the write that holds the
	// goroutine at once, and every one after it.
	p.conns.cut()
	later := context.WithoutCancel(ctx)
	go func() {
		<-p.done
		// The counters are final once the goroutine has ended: the line goes out
		// before closing the connection, which may wait on the stalled socket.
		p.logTotals(later, true)
		close(p.closed)
		p.nc.Close()
	}()
	return s
}

// Closed is closed once Close has logged the final totals; the connection is
// closed then, or just after.
func (p *Publisher) Closed() <-chan struct{} { return p.closed }

// Gone is closed once the connection is closed and the client has run its last
// callback, after Closed: from then on nothing the publisher or its client does
// logs. A replay waits for it before it returns (paddock#35).
func (p *Publisher) Gone() <-chan struct{} { return p.gone }

func (p *Publisher) logTotals(ctx context.Context, final bool) Stats {
	s := p.Stats()
	p.logger.InfoContext(ctx, "published",
		"frames", s.Frames, "lo_frames", s.LoFrames, "events", s.Events,
		"dropped_frames", s.DroppedFrames, "dropped_lo_frames", s.DroppedLoFrames, "dropped_events", s.DroppedEvents,
		"final", final)
	return s
}
