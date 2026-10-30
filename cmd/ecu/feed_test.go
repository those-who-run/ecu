package main

import (
	"context"
	"io"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/service"
	"github.com/those-who-run/salsa"
)

// fed is a live-like source: it gives the packets sent on its channel, stamped
// with the time since start as live stamps them, and keeps a packet that comes as
// the context ends for the next call, as live's Next does.
type fed struct {
	packets chan salsa.Packet
	start   time.Time
}

func (f *fed) Next(ctx context.Context) (salsa.Received, error) {
	select {
	case p := <-f.packets:
		return salsa.Received{T: time.Since(f.start).Seconds(), Packet: p}, nil
	case <-ctx.Done():
		return salsa.Received{}, ctx.Err()
	}
}

func (f *fed) Close() error { return nil }

func racePacket(id int32, lap int16, paused bool) salsa.Packet {
	var p salsa.Packet
	p.PacketID, p.CurrentLap, p.TotalLaps, p.TotalCars, p.RacePosition = id, lap, 5, 20, 7
	p.Flags, p.FuelLevel = salsa.FlagCarsOnTrack, 50
	if paused {
		p.Flags |= salsa.FlagPaused
	}
	return p
}

// feedDrive drives the packets with the watchdog's timeout, the feed stopping for
// gap after the last of before and resuming with after, and returns what was
// published.
func feedDrive(t *testing.T, timeout, gap time.Duration, before, after []salsa.Packet) []*fakeOut {
	t.Helper()
	src := &fed{packets: make(chan salsa.Packet), start: time.Now()}
	at := func(r salsa.Received) time.Time { return src.start.Add(time.Duration(r.T * float64(time.Second))) }
	m := machine()
	m.WatchTelemetry(timeout)
	eng, err := rulesEngine(func(string) string { return "" }, service.NewLogger(io.Discard, 0))
	if err != nil {
		t.Fatal(err)
	}
	pub := &fakePublisher{}
	ctx, cancel := context.WithCancel(t.Context())
	driven := make(chan struct{})
	go func() { defer close(driven); _ = drive(ctx, src, at, m, eng, pub, nil, nil, nil) }()
	for _, p := range before {
		src.packets <- p
	}
	time.Sleep(gap)
	for _, p := range after {
		src.packets <- p
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-driven
	pub.mu.Lock()
	defer pub.mu.Unlock()
	var out []*fakeOut
	var lastFrame time.Time
	for _, o := range pub.outs {
		if o.Frame != nil {
			lastFrame = o.Event.Time
			continue
		}
		out = append(out, &fakeOut{o.Event.ID, o.Event.Time, lastFrame, o.TelemetryLost, o.TelemetryRestored, o.Ended})
	}
	return out
}

type fakeOut struct {
	id              string
	time, lastFrame time.Time
	lost            *eventsv1.TelemetryLost
	restored        *eventsv1.TelemetryRestored
	ended           *eventsv1.SessionEnded
}

func only[T any](outs []*fakeOut, get func(*fakeOut) *T) []*fakeOut {
	var all []*fakeOut
	for _, o := range outs {
		if get(o) != nil {
			all = append(all, o)
		}
	}
	return all
}

// AC1, scaled down: with a 300 ms timeout, a feed that stops for 500 ms mid-race
// gives one telemetry.lost about 300 ms after the last packet and one
// telemetry.restored on resuming, with the gap, and the session stays open until
// ecu stops.
func TestALiveDriveLosesAndRestoresTheFeed(t *testing.T) {
	timeout, gap := 300*time.Millisecond, 500*time.Millisecond
	outs := feedDrive(t, timeout, gap,
		[]salsa.Packet{racePacket(1, 0, false), racePacket(2, 1, false), racePacket(3, 1, false)},
		[]salsa.Packet{racePacket(4, 1, false)})
	lost := only(outs, func(o *fakeOut) *eventsv1.TelemetryLost { return o.lost })
	restored := only(outs, func(o *fakeOut) *eventsv1.TelemetryRestored { return o.restored })
	if len(lost) != 1 || len(restored) != 1 {
		t.Fatalf("%d lost and %d restored, want one each: %v", len(lost), len(restored), outs)
	}
	if after := lost[0].time.Sub(lost[0].lastFrame); after < timeout || after > timeout+2*time.Second ||
		lost[0].lost.GetSilenceMs() < 300 || lost[0].lost.GetAt() != 3 {
		t.Errorf("lost %v after the last packet: %v", after, lost[0].lost)
	}
	// The gap is the receive times' on the wall clock: a loaded runner can stamp
	// the packets either side of the sleep a little closer than gap (498 ms of
	// 500 seen in CI), so the floor has 100 ms of slack, still above the timeout.
	floor := (gap - 100*time.Millisecond).Milliseconds()
	if r := restored[0].restored; int64(r.GetGapMs()) < floor || r.GetGapMs() > 3000 || r.GetLostAt() != 3 || r.GetAt() != 4 {
		t.Errorf("restored %v", r)
	}
	ended := only(outs, func(o *fakeOut) *eventsv1.SessionEnded { return o.ended })
	if len(ended) != 1 || ended[0].ended.GetReason() != eventsv1.EndReason_END_REASON_ENGINEER_STOPPED {
		t.Errorf("ended %v, want the session open until ecu stopped", ended)
	}
}

// AC2, scaled down: the same with the last packet paused ends the session paused,
// as the feed was lost.
func TestALiveDriveLostInAPauseEndsTheSessionPaused(t *testing.T) {
	outs := feedDrive(t, 300*time.Millisecond, 500*time.Millisecond,
		[]salsa.Packet{racePacket(1, 0, false), racePacket(2, 1, false), racePacket(3, 1, true)},
		[]salsa.Packet{racePacket(4, 1, true)})
	ended := only(outs, func(o *fakeOut) *eventsv1.SessionEnded { return o.ended })
	if len(ended) != 1 || ended[0].ended.GetReason() != eventsv1.EndReason_END_REASON_TELEMETRY_LOST ||
		!ended[0].ended.GetSummary().GetEndedPaused() || ended[0].ended.GetLastSeq() != 3 {
		t.Errorf("ended %v, want the session ended paused on the paused frame", ended)
	}
}

// racy is live's Next at its worst: with a packet waiting as the deadline passes,
// its first call picks the deadline.
type racy struct {
	calls int
	p     salsa.Packet
}

func (r *racy) Next(ctx context.Context) (salsa.Received, error) {
	r.calls++
	if r.calls == 1 {
		<-ctx.Done()
		return salsa.Received{}, ctx.Err()
	}
	return salsa.Received{T: 1, Packet: r.p}, nil
}

func (*racy) Close() error { return nil }

// A packet waiting as the feed's deadline passes is taken, not called a loss.
func TestAPacketAtTheDeadlineIsNoLoss(t *testing.T) {
	m := machine()
	m.WatchTelemetry(time.Millisecond)
	m.Step(race.Received{Received: salsa.Received{Packet: racePacket(1, 1, true)}, Time: time.Now()})
	src := &racy{p: racePacket(2, 1, true)}
	pub := &fakePublisher{}
	r, err := next(t.Context(), src, m, pub, nil)
	if err != nil || r.PacketID != 2 || src.calls != 2 || len(pub.outs) != 0 {
		t.Errorf("got packet %d, %v after %d calls, published %v; want packet 2 and no loss", r.PacketID, err, src.calls, pub.outs)
	}
}
