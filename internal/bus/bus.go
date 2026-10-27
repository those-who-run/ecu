// Package bus is ecu's NATS adapter (paddock#34): it puts the race state's
// outputs on the bus. Each frame is a core publish on twr.v1.<src>.frame.gt7, which
// the TELEMETRY stream captures, and at most 20 a second of frame time go on
// twr.v1.<src>.frame.gt7.lo too; session.started, lap.completed and session.ended
// are core publishes on twr.v1.<src>.evt.<sid>.<area>.<what>, which the EVENTS
// stream captures and deduplicates on Nats-Msg-Id. Every message carries the full
// set of CloudEvents headers, from contracts' ce package. It is the only package
// that imports nats.go.
//
// Nothing on the frame path waits on the network: a publish goes into the client's
// buffer and returns. While the connection is down, frames are dropped and counted,
// and events wait in a bounded queue, sent in order once it is back.
package bus

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/those-who-run/contracts/ce"
	"github.com/those-who-run/ecu/internal/race"
)

// Options are the publisher's limits. The zero value takes the defaults.
type Options struct {
	// QueueSize is how many events wait while the connection is down: 1,024 by
	// default, a few hours of laps. An event beyond it is dropped and counted.
	QueueSize int
	// ReconnectWait is the pause between attempts to reach the server again: 1 s
	// by default.
	ReconnectWait time.Duration
}

// loGap is the least frame time between two frames on the .lo subject: 50 ms,
// 20 Hz at most (R2).
const loGap = 0.05

// resendWindow is how far before a lost connection the events already published
// are sent again once it is back. A publish only reaches the client's buffer, so the
// events of the last moments before the client saw the connection go may never
// have reached the server; EVENTS drops the copies that did, by Nats-Msg-Id, within
// its 2 min duplicate window.
const resendWindow = 10 * time.Second

// Stats is what was published and dropped.
type Stats struct {
	Frames, LoFrames, Events                      uint64
	DroppedFrames, DroppedLoFrames, DroppedEvents uint64
}

// Publisher publishes the race state's outputs under one <src>. Publish is called
// from one goroutine, the step loop's.
type Publisher struct {
	nc     *nats.Conn
	src    string
	logger *slog.Logger
	limit  int

	// The step loop's own: the frame time of the last frame put on .lo.
	lastLo  float64
	sentLo  bool
	reached chan struct{} // a (re)connect happened

	mu       sync.Mutex
	queue    []*nats.Msg // events waiting for the connection, oldest first
	recent   []sent      // events published lately, for resendWindow
	downAt   time.Time   // when the connection went, zero while it is up
	closed   bool
	counters struct {
		frames, lo, events                      atomic.Uint64
		droppedFrames, droppedLo, droppedEvents atomic.Uint64
	}
}

type sent struct {
	msg *nats.Msg
	at  time.Time
}

// Connect is a publisher on the server cfg names, under cfg.Src, which Validate
// must have passed. It doesn't wait for the server: until the first connection,
// and after any loss of it, it keeps trying, every ReconnectWait.
func Connect(cfg Config, logger *slog.Logger, opts Options) (*Publisher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1024
	}
	if opts.ReconnectWait <= 0 {
		opts.ReconnectWait = time.Second
	}
	p := &Publisher{src: cfg.Src, logger: logger, limit: opts.QueueSize, reached: make(chan struct{}, 1)}
	nc, err := nats.Connect(cfg.URL,
		nats.Name("ecu"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(opts.ReconnectWait),
		// No buffer of the client's while it reconnects: a publish then fails at
		// once, so a frame is counted as dropped and an event queued here, in order.
		nats.ReconnectBufSize(-1),
		nats.ConnectHandler(p.connected),
		nats.ReconnectHandler(p.connected),
		nats.DisconnectErrHandler(p.disconnected),
	)
	if err != nil {
		return nil, err
	}
	p.nc = nc
	logger.Info("publishing", "nats", redacted(cfg.URL), "src", cfg.Src)
	return p, nil
}

func (p *Publisher) disconnected(_ *nats.Conn, err error) {
	p.mu.Lock()
	if p.downAt.IsZero() {
		p.downAt = time.Now()
	}
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		p.logger.Warn("NATS connection lost", "err", err)
	}
}

// connected sends the queued events, after those published just before the
// connection went, which may not have reached the server.
func (p *Publisher) connected(nc *nats.Conn) {
	p.mu.Lock()
	if !p.downAt.IsZero() {
		var again []*nats.Msg
		for _, s := range p.recent {
			if !s.at.Before(p.downAt.Add(-resendWindow)) {
				again = append(again, s.msg)
			}
		}
		p.queue = append(again, p.queue...)
		p.recent, p.downAt = nil, time.Time{}
	}
	p.flushQueue(nc)
	p.mu.Unlock()
	p.logger.Info("NATS connected", "server", nc.ConnectedUrlRedacted())
	select {
	case p.reached <- struct{}{}:
	default:
	}
}

// Publish publishes a step's outputs: each frame on the frame subject, and on .lo
// when it is 50 ms of frame time or more after the last one there, and each event
// on its own subject. SessionStarted's source is set to the publisher's <src>
// here, as only the publisher knows it. It never waits on the network.
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
		p.counters.droppedFrames.Add(1)
		return
	}
	subject := "twr.v1." + p.src + ".frame.gt7"
	if !p.nc.IsConnected() || p.nc.PublishMsg(&nats.Msg{Subject: subject, Header: nats.Header(h), Data: data}) != nil {
		p.counters.droppedFrames.Add(1)
		if lo {
			p.counters.droppedLo.Add(1)
		}
		return
	}
	p.counters.frames.Add(1)
	if !lo {
		return
	}
	if p.nc.PublishMsg(&nats.Msg{Subject: subject + ".lo", Header: nats.Header(h), Data: data}) != nil {
		p.counters.droppedLo.Add(1)
		return
	}
	p.counters.lo.Add(1)
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) >= p.limit {
		p.counters.droppedEvents.Add(1)
		p.logger.Warn("event dropped: the queue is full", "id", e.ID, "queue", p.limit)
		return
	}
	p.queue = append(p.queue, msg)
	p.flushQueue(p.nc)
}

// flushQueue publishes the queued events in order while the connection takes them.
// The caller holds mu.
func (p *Publisher) flushQueue(nc *nats.Conn) {
	for len(p.queue) > 0 && nc.IsConnected() {
		if nc.PublishMsg(p.queue[0]) != nil {
			return
		}
		now := time.Now()
		p.recent = append(p.recent, sent{p.queue[0], now})
		p.queue[0] = nil
		p.queue = p.queue[1:]
		p.counters.events.Add(1)
		for len(p.recent) > 0 && now.Sub(p.recent[0].at) > resendWindow {
			p.recent = p.recent[1:]
		}
	}
}

// sendQueued sends the queued events the connection takes, and is how many are
// left.
func (p *Publisher) sendQueued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushQueue(p.nc)
	return len(p.queue)
}

// Stats is what has been published and dropped so far. An event sent again after
// a lost connection counts again.
func (p *Publisher) Stats() Stats {
	c := &p.counters
	return Stats{
		Frames: c.frames.Load(), LoFrames: c.lo.Load(), Events: c.events.Load(),
		DroppedFrames: c.droppedFrames.Load(), DroppedLoFrames: c.droppedLo.Load(), DroppedEvents: c.droppedEvents.Load(),
	}
}

// Close sends the events still queued, waiting for the connection until ctx is
// done, then flushes what the client holds and closes the connection. The events
// it could not send are counted as dropped. It logs and returns the totals (R10).
func (p *Publisher) Close(ctx context.Context) Stats {
wait:
	for p.sendQueued() > 0 {
		select {
		case <-p.reached:
		case <-ctx.Done():
			break wait
		}
	}
	var err error
	if p.nc.IsConnected() {
		err = p.nc.FlushWithContext(ctx)
	}
	p.mu.Lock()
	p.closed = true
	p.counters.droppedEvents.Add(uint64(len(p.queue)))
	p.queue = nil
	p.mu.Unlock()
	p.nc.Close()
	s := p.Stats()
	p.logger.InfoContext(ctx, "published",
		"frames", s.Frames, "lo_frames", s.LoFrames, "events", s.Events,
		"dropped_frames", s.DroppedFrames, "dropped_lo_frames", s.DroppedLoFrames, "dropped_events", s.DroppedEvents)
	if err != nil && !errors.Is(err, context.Canceled) {
		p.logger.WarnContext(ctx, "the last messages may not have reached the server", "err", err)
	}
	return s
}
