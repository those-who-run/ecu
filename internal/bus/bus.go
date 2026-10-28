// Package bus is ecu's NATS adapter (paddock#34): it puts the race state's
// outputs on the bus. Each frame is a core publish on twr.v1.<src>.frame.gt7, which
// the TELEMETRY stream captures, and at most 20 a second of frame time go on
// twr.v1.<src>.frame.gt7.lo too. session.started, lap.completed and session.ended
// go on twr.v1.<src>.evt.<sid>.<area>.<what>, which the EVENTS stream captures and
// deduplicates on Nats-Msg-Id; each is published through JetStream and sent again
// until EVENTS acknowledges it. Every message carries the full set of CloudEvents
// headers, from contracts' ce package. It is the only package that imports nats.go.
//
// Nothing on the frame path waits on the network: Publish encodes the outputs and
// hands them to one goroutine, which owns the connection, without waiting. That
// goroutine may wait on the network (a server that stops reading holds a write for
// up to its timeout); the step loop never does.
package bus

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
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
}

// loGap is the least frame time between two frames on the .lo subject: 50 ms,
// 20 Hz at most (R2).
const loGap = 0.05

// The client's timeouts. writeTimeout bounds how long the publishing goroutine
// waits on a server that stops reading before the client drops the connection and
// tries again; ackTimeout how long an event waits for EVENTS' acknowledgement
// before it is sent again.
const (
	writeTimeout = 2 * time.Second
	ackTimeout   = 5 * time.Second
)

// Stats is what was published and dropped. Events counts the acknowledgements
// from EVENTS, a duplicate's included.
type Stats struct {
	Frames, LoFrames, Events                      uint64
	DroppedFrames, DroppedLoFrames, DroppedEvents uint64
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
	js     jetstream.JetStream
	src    string
	logger *slog.Logger
	limit  int
	wait   time.Duration

	frames chan frameMsg
	events chan *nats.Msg
	quit   chan struct{}   // closed by Close
	until  context.Context // Close's deadline, set before quit is closed
	done   chan struct{}

	// The step loop's own: the frame time of the last frame put on .lo.
	lastLo float64
	sentLo bool

	counters struct {
		frames, lo, events                      atomic.Uint64
		droppedFrames, droppedLo, droppedEvents atomic.Uint64
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
	if opts.ReconnectWait <= 0 {
		opts.ReconnectWait = time.Second
	}
	nc, err := nats.Connect(cfg.URL,
		nats.Name("ecu"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(opts.ReconnectWait),
		// No buffer of the client's while it reconnects: a frame published then is
		// dropped at once, and an event waits in the publisher's queue.
		nats.ReconnectBufSize(-1),
		nats.FlusherTimeout(writeTimeout),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("NATS connected again", "server", nc.ConnectedUrlRedacted())
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.Warn("NATS connection lost", "err", err)
			}
		}),
	)
	if err != nil {
		return nil, err
	}
	// One event in flight at a time keeps EVENTS in the order the events happened.
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncTimeout(ackTimeout), jetstream.WithPublishAsyncMaxPending(1))
	if err != nil {
		nc.Close()
		return nil, err
	}
	p := &Publisher{
		nc: nc, js: js, src: cfg.Src, logger: logger, limit: opts.QueueSize, wait: opts.ReconnectWait,
		frames: make(chan frameMsg, opts.FrameBuffer),
		events: make(chan *nats.Msg, opts.QueueSize),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
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
func (p *Publisher) Publish(outs []race.Output) {
	for _, o := range outs {
		if o.Frame != nil {
			p.frame(o)
		} else {
			p.event(o)
		}
	}
}

func (p *Publisher) frame(o race.Output) {
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
	select {
	case p.frames <- frameMsg{msg, lo}:
	default:
		p.dropFrame(lo)
	}
}

func (p *Publisher) dropFrame(lo bool) {
	p.counters.droppedFrames.Add(1)
	if lo {
		p.counters.droppedLo.Add(1)
	}
}

// The event ids (R4): <src> is part of each, so two replays of one recording
// under different sources are not deduplicated against each other.
func (p *Publisher) event(o race.Output) {
	var what, id string
	switch {
	case o.Started != nil:
		o.Started.Source = p.src
		what, id = "session.started", "session:started"
	case o.Lap != nil:
		what, id = "lap.completed", "lap:"+strconv.Itoa(int(o.Lap.GetLap()))+":completed"
	case o.Ended != nil:
		what, id = "session.ended", "session:ended"
	default:
		return
	}
	e := o.Event
	e.ID = p.src + ":" + o.Session + ":" + id
	h, data, err := ce.Encode(o.Message(), e)
	if err != nil {
		p.logger.Error("event not encoded", "id", e.ID, "err", err)
		p.counters.droppedEvents.Add(1)
		return
	}
	msg := &nats.Msg{Subject: "twr.v1." + p.src + ".evt." + o.Session + "." + what, Header: nats.Header(h), Data: data}
	select {
	case p.events <- msg:
	default:
		p.counters.droppedEvents.Add(1)
		p.logger.Error("event dropped: the queue is full", "id", e.ID, "queue", p.limit)
	}
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

// Stats is what has been published and dropped so far.
func (p *Publisher) Stats() Stats {
	c := &p.counters
	return Stats{
		Frames: c.frames.Load(), LoFrames: c.lo.Load(), Events: c.events.Load(),
		DroppedFrames: c.droppedFrames.Load(), DroppedLoFrames: c.droppedLo.Load(), DroppedEvents: c.droppedEvents.Load(),
	}
}

// Close publishes what Publish has handed over and waits for EVENTS to acknowledge
// every event, until ctx is done; the events still waiting then are counted as
// dropped. It then flushes the frames the client holds and closes the connection,
// and logs and returns the totals (R10). It returns once ctx is done even if the
// publishing goroutine is still held by a write, which then closes the connection
// itself when the write gives up. Publish must not be called after it, and Close
// only once.
func (p *Publisher) Close(ctx context.Context) Stats {
	p.until = ctx
	close(p.quit)
	var err error
	select {
	case <-p.done:
		if p.nc.IsConnected() {
			err = p.nc.FlushWithContext(ctx)
		}
		p.nc.Close()
	case <-ctx.Done():
		go func() {
			<-p.done
			p.nc.Close()
		}()
		err = errors.New("the publisher was still writing when the time ran out")
	}
	s := p.Stats()
	p.logger.InfoContext(ctx, "published",
		"frames", s.Frames, "lo_frames", s.LoFrames, "events", s.Events,
		"dropped_frames", s.DroppedFrames, "dropped_lo_frames", s.DroppedLoFrames, "dropped_events", s.DroppedEvents)
	if err != nil && !errors.Is(err, context.Canceled) {
		p.logger.WarnContext(ctx, "the last frames may not have reached the server", "err", err)
	}
	return s
}
