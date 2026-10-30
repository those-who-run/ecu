package bus

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/bus/bustest"
	"github.com/those-who-run/ecu/internal/race"
)

// rig is a nats-server with the TELEMETRY and EVENTS streams, a client of the
// test's own to read them back, and a publisher under src.
type rig struct {
	srv  *bustest.Server
	nc   *nats.Conn
	js   jetstream.JetStream
	pub  *Publisher
	logs *logs
	src  string
}

func newRig(t *testing.T, src string) *rig {
	t.Helper()
	r := &rig{srv: startNATS(t), logs: &logs{}, src: src}
	nc, err := nats.Connect(r.srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	r.nc = nc
	if r.js, err = jetstream.New(nc); err != nil {
		t.Fatal(err)
	}
	addStreams(t, r.js)
	r.pub, err = Connect(Config{URL: r.srv.URL(), Src: src}, r.logs.logger(), Options{ReconnectWait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.pub.nc.Close() })
	waitFor(t, "the publisher's connection", r.pub.nc.IsConnected)
	return r
}

// close closes the publisher, sending what it still holds.
func (r *rig) close(t *testing.T) Stats {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return r.pub.Close(ctx)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s after 10 s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// read is every message in the stream, in order, once it holds want of them.
func (r *rig) read(t *testing.T, stream string, want int) []jetstream.Msg {
	t.Helper()
	s, err := r.js.Stream(t.Context(), stream)
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	waitFor(t, strconv.Itoa(want)+" messages in "+stream, func() bool {
		info, err := s.Info(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		n = info.State.Msgs
		return n >= uint64(want) //nolint:gosec // a count
	})
	// What arrives after want is a fault too: wait a moment for it.
	time.Sleep(100 * time.Millisecond)
	if info, err := s.Info(t.Context()); err != nil || info.State.Msgs != n {
		t.Fatalf("%s holds %d messages, then more: %v", stream, n, err)
	}
	c, err := s.OrderedConsumer(t.Context(), jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var all []jetstream.Msg
	for uint64(len(all)) < n {
		batch, err := c.Fetch(int(n)-len(all), jetstream.FetchMaxWait(5*time.Second)) //nolint:gosec // a count
		if err != nil {
			t.Fatal(err)
		}
		for m := range batch.Messages() {
			all = append(all, m)
		}
		if batch.Error() != nil {
			t.Fatal(batch.Error())
		}
	}
	return all
}

// header is a message's headers as ce reads them.
func header(m jetstream.Msg) ce.Header { return ce.Header(m.Headers()) }

// AC1: replaying the Suzuka pit stop through the adapter puts every frame in
// TELEMETRY, and in EVENTS one session.started, a lap.completed for each lap and one
// session.ended, with the values the race state gave, the source and every
// CloudEvents header.
func TestAReplayLandsInTheStreams(t *testing.T) {
	r := newRig(t, "parity")
	m := newMachine()
	out := step(m, r.pub, packets(t, suzuka))
	end := m.EndOfInput()
	r.pub.Publish(end)
	out = append(out, end...)
	stats := r.close(t)

	var frames, events []race.Output
	for _, o := range out {
		if o.Frame != nil {
			frames = append(frames, o)
		} else {
			events = append(events, o)
		}
	}
	if len(frames) != 874 {
		t.Fatalf("%d frames, want the recording's 874", len(frames))
	}
	got := r.read(t, "TELEMETRY", len(frames))
	if len(got) != len(frames) {
		t.Fatalf("TELEMETRY holds %d frames, want %d", len(got), len(frames))
	}
	byseq := map[uint32]jetstream.Msg{}
	for i, msg := range got {
		want := frames[i]
		var f telemetryv1.Frame
		e, err := ce.Decode(header(msg), msg.Data(), &f)
		if err != nil {
			t.Fatalf("frame %d: %v", i+1, err)
		}
		if msg.Subject() != "twr.v1.parity.frame.gt7" || !proto.Equal(&f, want.Frame) {
			t.Fatalf("frame %d on %s: %v, want %v", i+1, msg.Subject(), &f, want.Frame)
		}
		checkHeaders(t, header(msg), e, want.Event, m.Run()+":frame:"+strconv.Itoa(i+1))
		byseq[f.GetSeq()] = msg
	}

	evs := r.read(t, "EVENTS", len(events))
	if len(evs) != len(events) {
		t.Fatalf("EVENTS holds %d events, want %d", len(evs), len(events))
	}
	var started, laps, bests, tracks, kinds, entered, exited, finalLaps, positions, finished, ended, paused, resumed int
	for i, msg := range evs {
		want := events[i]
		var gotMsg proto.Message
		var what, id string
		switch {
		case want.Started != nil:
			started++
			gotMsg, what, id = &eventsv1.SessionStarted{}, "session.started", "session.started:1"
		case want.Lap != nil:
			laps++
			gotMsg, what, id = &eventsv1.LapCompleted{}, "lap.completed", "lap.completed:"+strconv.Itoa(int(want.Lap.GetLap()))
		case want.Best != nil:
			bests++
			gotMsg, what, id = &eventsv1.LapBestChanged{}, "lap.best", "lap.best:"+strconv.Itoa(int(want.Best.GetEndedLap()))
		case want.Track != nil:
			tracks++
			gotMsg, what, id = &eventsv1.SessionTrackChanged{}, "session.track", "session.track:"+m.Run()+":"+strconv.Itoa(int(want.Track.GetAt()))
		case want.Kind != nil:
			kinds++
			gotMsg, what, id = &eventsv1.SessionKindChanged{}, "session.kind", "session.kind:"+m.Run()+":"+strconv.Itoa(int(want.Kind.GetAt()))
		case want.PitEntered != nil:
			entered++
			gotMsg, what, id = &eventsv1.PitEntered{}, "pit.entered", "pit.entered:"+strconv.Itoa(int(want.PitEntered.GetEntry()))
		case want.PitExited != nil:
			exited++
			gotMsg, what, id = &eventsv1.PitExited{}, "pit.exited", "pit.exited:"+strconv.Itoa(int(want.PitExited.GetStop().GetStop()))
		case want.FinalLap != nil:
			finalLaps++
			gotMsg, what, id = &eventsv1.RaceFinalLapStarted{}, "race.final_lap", "race.final_lap:1"
		case want.Position != nil:
			positions++
			gotMsg, what, id = &eventsv1.RacePositionChanged{}, "race.position", "race.position:"+m.Run()+":"+strconv.Itoa(int(want.Position.GetAt()))
		case want.Finished != nil:
			finished++
			gotMsg, what, id = &eventsv1.RaceFinished{}, "race.finished", "race.finished:1"
		case want.Ended != nil:
			ended++
			gotMsg, what, id = &eventsv1.SessionEnded{}, "session.ended", "session.ended:1"
		case want.Paused != nil:
			paused++
			gotMsg, what, id = &eventsv1.SessionPaused{}, "session.paused", "session.paused:"+strconv.Itoa(int(want.Paused.GetPause()))
		case want.Resumed != nil:
			resumed++
			gotMsg, what, id = &eventsv1.SessionResumed{}, "session.resumed", "session.resumed:"+strconv.Itoa(int(want.Resumed.GetPause()))
		}
		e, err := ce.Decode(header(msg), msg.Data(), gotMsg)
		if err != nil {
			t.Fatalf("event %d: %v", i+1, err)
		}
		if subject := "twr.v1.parity.evt." + want.Session + "." + what; msg.Subject() != subject {
			t.Errorf("event %d on %s, want %s", i+1, msg.Subject(), subject)
		}
		if !proto.Equal(gotMsg, want.Message()) {
			t.Errorf("event %d: %v, want %v", i+1, gotMsg, want.Message())
		}
		checkHeaders(t, header(msg), e, want.Event, "parity:"+want.Session+":"+id)
	}
	if started != 1 || laps != 6 || bests != 2 || tracks == 0 || kinds == 0 || entered != 1 || exited != 1 ||
		finalLaps != 1 || positions != 2 || finished != 1 || ended != 1 || paused != 1 || resumed != 1 {
		t.Errorf("%d starts, %d laps, %d bests, %d tracks, %d kinds, %d pit entries, %d pit exits, %d final laps, %d positions, %d finishes, %d ends, "+
			"%d pauses, %d resumes; want 1, 6 (the race's laps), 2 (laps 2 and 5), some, some, 1, 1, 1, 2, 1, 1, 1 and 1 (2 s on lap 1)",
			started, laps, bests, tracks, kinds, entered, exited, finalLaps, positions, finished, ended, paused, resumed)
	}

	// The start's source is the subject's <src> (ecu#11's review), and its
	// started_at is the first frame's ce-time to the nanosecond (logbook#13).
	var s eventsv1.SessionStarted
	if _, err := ce.Decode(header(evs[0]), evs[0].Data(), &s); err != nil {
		t.Fatal(err)
	}
	if s.GetSource() != "parity" {
		t.Errorf("source %q, want the subject's parity", s.GetSource())
	}
	first, ok := byseq[s.GetFirstSeq()]
	if !ok {
		t.Fatalf("no frame %d in TELEMETRY", s.GetFirstSeq())
	}
	at, err := time.Parse(time.RFC3339Nano, header(first).Get(ce.TimeHeader))
	if err != nil {
		t.Fatal(err)
	}
	if !s.GetStartedAt().AsTime().Equal(at) || at.Nanosecond() == 0 {
		t.Errorf("started_at %v, first frame's ce-time %v", s.GetStartedAt().AsTime(), at)
	}
	if js, _ := protojson.Marshal(s.GetStartedAt()); string(js) != `"`+header(first).Get(ce.TimeHeader)+`"` {
		t.Errorf("started_at %s, first frame's ce-time %q", js, header(first).Get(ce.TimeHeader))
	}

	if stats.Frames != 874 || stats.Events != uint64(len(events)) || stats.DroppedFrames != 0 ||
		stats.DroppedEvents != 0 || stats.LoFrames == 0 {
		t.Errorf("stats %+v", stats)
	}
	// R10: the URL and src at the start, the totals at the end.
	log := r.logs.String()
	for _, want := range []string{
		`"msg":"publishing","nats":"` + r.srv.URL() + `","src":"parity"`,
		`"msg":"published","frames":874,"lo_frames":` + strconv.FormatUint(stats.LoFrames, 10) +
			`,"events":` + strconv.Itoa(len(events)) + `,"callouts":0,"dropped_frames":0,"dropped_lo_frames":0,"dropped_events":0,"dropped_callouts":0`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log has no %s:\n%s", want, log)
		}
	}
}

// checkHeaders checks a message's CloudEvents headers: every one ADR 0002 names,
// with the id, source and time the race state gave and Nats-Msg-Id the same as
// ce-id.
func checkHeaders(t *testing.T, h ce.Header, got, want ce.Event, id string) {
	t.Helper()
	if got.ID != id || h.Get(ce.MsgIDHeader) != id || got.Source != "/twr/ecu/gt7" || !got.Time.Equal(want.Time) ||
		h.Get(ce.SpecVersionHeader) != "1.0" || h.Get(ce.ContentTypeHeader) != "application/json" {
		t.Fatalf("headers %v, want id %s at %v", h, id, want.Time)
	}
}

// AC2: the same events published twice within EVENTS' duplicate window are stored
// once, on their Nats-Msg-Id.
func TestTheSameEventsTwiceAreStoredOnce(t *testing.T) {
	r := newRig(t, "replay-twice")
	m := newMachine()
	// The race ends at the flag, so every event goes out during the replay.
	events := sessionEvents(step(m, r.pub, packets(t, suzuka)))
	if end := m.EndOfInput(); len(end) != 0 || events[len(events)-1].Ended == nil {
		t.Fatalf("the race did not end at the flag: %v", end)
	}
	r.pub.Publish(events) // every event again
	stats := r.close(t)
	if stats.Events != 2*uint64(len(events)) {
		t.Fatalf("published %d events, want %d", stats.Events, 2*len(events))
	}
	if got := r.read(t, "EVENTS", len(events)); len(got) != len(events) {
		t.Errorf("EVENTS holds %d events, want %d", len(got), len(events))
	}
}

// EVENTS keeps every event of a replay: none shares an id with another, which
// EVENTS would drop as a duplicate. SUGO's race (2026-10-18) holds a verdict as
// the session opens and crosses the line on the same frame, a second track
// change there that is published as one session.track (paddock#81).
func TestEveryEventOfAReplayIsInEvents(t *testing.T) {
	r := newRig(t, "parity")
	m := newMachine()
	events := sessionEvents(step(m, r.pub, packets(t, "real-nuc-sugo-two-stops-2026-10-18")))
	end := m.EndOfInput()
	r.pub.Publish(end)
	events = append(events, sessionEvents(end)...)
	r.close(t)
	got := r.read(t, "EVENTS", len(events))
	if len(got) != len(events) {
		t.Fatalf("EVENTS holds %d events, want all %d", len(got), len(events))
	}
	for i, msg := range got {
		if id, want := header(msg).Get(ce.IDHeader), "parity:"+events[i].Event.ID; id != want {
			t.Errorf("event %d is %s, want %s", i+1, id, want)
		}
	}
}

// AC3: the .lo subject carries no two frames less than 50 ms of frame time apart.
func TestTheLoSubjectIsAtMost20Hz(t *testing.T) {
	r := newRig(t, "parity")
	sub, err := r.nc.SubscribeSync("twr.v1.parity.frame.gt7.lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.SetPendingLimits(-1, -1); err != nil {
		t.Fatal(err)
	}
	if err := r.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	m := newMachine()
	out := step(m, r.pub, packets(t, suzuka))
	stats := r.close(t)
	var ts []float64
	for range stats.LoFrames {
		msg, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("after %d of %d: %v", len(ts), stats.LoFrames, err)
		}
		var f telemetryv1.Frame
		if _, err := ce.Decode(ce.Header(msg.Header), msg.Data, &f); err != nil {
			t.Fatal(err)
		}
		ts = append(ts, f.GetT())
	}
	if len(ts) < 2 {
		t.Fatalf("%d frames on .lo", len(ts))
	}
	for i := 1; i < len(ts); i++ {
		if ts[i]-ts[i-1] < 0.05 {
			t.Errorf("frames %d and %d on .lo %.4f s apart", i, i+1, ts[i]-ts[i-1])
		}
	}
	// Every frame 50 ms after the last one sent goes: the .lo frames are as many as
	// that rule picks from all of them.
	var want int
	var last float64
	for _, o := range out {
		if o.Frame != nil && (want == 0 || o.Frame.GetT()-last >= 0.05) {
			want++
			last = o.Frame.GetT()
		}
	}
	if len(ts) != want || uint64(len(ts)) >= stats.Frames {
		t.Errorf("%d frames on .lo of %d, want %d", len(ts), stats.Frames, want)
	}
}

// AC4: with the server stopped mid-replay the step loop goes on, the frames are
// counted as dropped, and the events queued while it was down arrive in order once
// it is back.
func TestAServerStoppedMidReplay(t *testing.T) {
	r := newRig(t, "parity")
	all := packets(t, suzuka)
	m := newMachine()
	// Up for laps 1 and 2, down for laps 3 and 4, up again for the rest.
	var out []race.Output
	var at int
	lapsSeen := func(o []race.Output) (n int) {
		for _, x := range o {
			if x.Lap != nil {
				n++
			}
		}
		return n
	}
	for lapsSeen(out) < 2 {
		out = append(out, step(m, r.pub, all[at:at+1])...)
		at++
	}
	handled(t, r.pub, len(out)-len(sessionEvents(out)))
	r.nc.Close()
	r.srv.Stop()
	waitFor(t, "lost connection", func() bool { return !r.pub.nc.IsConnected() })
	before := r.pub.Stats()
	var down []race.Output
	took := time.Duration(0)
	for lapsSeen(out)+lapsSeen(down) < 4 {
		start := time.Now()
		down = append(down, step(m, r.pub, all[at:at+1])...)
		took = max(took, time.Since(start))
		at++
	}
	var frames, events int
	for _, o := range down {
		if o.Frame != nil {
			frames++
		} else {
			events++
		}
	}
	handled(t, r.pub, len(out)-len(sessionEvents(out))+frames)
	if s := r.pub.Stats(); s.DroppedFrames-before.DroppedFrames != uint64(frames) || s.Events != before.Events { //nolint:gosec // a count
		t.Errorf("down for %d frames and %d events: stats %+v, before %+v", frames, events, s, before)
	}
	if events < 2 || took > 100*time.Millisecond {
		t.Errorf("%d events while down, the slowest step %v", events, took)
	}
	out = append(out, down...)

	r.srv.Start()
	waitFor(t, "the connection again", r.pub.nc.IsConnected)
	out = append(out, step(m, r.pub, all[at:])...)
	end := m.EndOfInput()
	r.pub.Publish(end)
	out = append(out, end...)
	r.close(t)

	nc, err := nats.Connect(r.srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	r.nc = nc
	if r.js, err = jetstream.New(nc); err != nil {
		t.Fatal(err)
	}
	want := sessionEvents(out)
	got := r.read(t, "EVENTS", len(want))
	if len(got) != len(want) {
		t.Fatalf("EVENTS holds %d events, want %d", len(got), len(want))
	}
	for i, msg := range got {
		var id string
		switch o := want[i]; {
		case o.Started != nil:
			id = "session.started:1"
		case o.Lap != nil:
			id = "lap.completed:" + strconv.Itoa(int(o.Lap.GetLap()))
		case o.Best != nil:
			id = "lap.best:" + strconv.Itoa(int(o.Best.GetEndedLap()))
		case o.Track != nil:
			id = "session.track:" + m.Run() + ":" + strconv.Itoa(int(o.Track.GetAt()))
		case o.Kind != nil:
			id = "session.kind:" + m.Run() + ":" + strconv.Itoa(int(o.Kind.GetAt()))
		case o.PitEntered != nil:
			id = "pit.entered:" + strconv.Itoa(int(o.PitEntered.GetEntry()))
		case o.PitExited != nil:
			id = "pit.exited:" + strconv.Itoa(int(o.PitExited.GetStop().GetStop()))
		case o.FinalLap != nil:
			id = "race.final_lap:1"
		case o.Position != nil:
			id = "race.position:" + m.Run() + ":" + strconv.Itoa(int(o.Position.GetAt()))
		case o.Finished != nil:
			id = "race.finished:1"
		default:
			id = "session.ended:1"
		}
		if h := header(msg).Get(ce.IDHeader); h != "parity:"+want[i].Session+":"+id {
			t.Errorf("event %d is %s, want parity:%s:%s: out of order", i+1, h, want[i].Session, id)
		}
	}
}

// handled waits until the publisher's goroutine has sent or dropped n frames.
func handled(t *testing.T, p *Publisher, n int) {
	t.Helper()
	waitFor(t, strconv.Itoa(n)+" frames handled", func() bool {
		s := p.Stats()
		return s.Frames+s.DroppedFrames == uint64(n) //nolint:gosec // a count
	})
}

// sessionEvents is the outputs without the frames.
func sessionEvents(out []race.Output) []race.Output {
	var evs []race.Output
	for _, o := range out {
		if o.Frame == nil {
			evs = append(evs, o)
		}
	}
	return evs
}

// The cost of encoding a frame with its headers on the step loop (paddock#34,
// Approach): go test -run '^$' -bench EncodeFrame ./internal/bus
func BenchmarkEncodeFrame(b *testing.B) {
	all := packets(b, suzuka)
	m := newMachine()
	var frames []race.Output
	for _, r := range all {
		frames = append(frames, m.Step(r)[0])
	}
	b.ResetTimer()
	for i := range b.N {
		o := frames[i%len(frames)]
		if _, _, err := ce.Encode(o.Frame, o.Event); err != nil {
			b.Fatal(err)
		}
	}
}

// R6 and AC5 of paddock#89: a callout goes on its priority's level (5 on .p0, 12
// on .p1, 20 on .p2), into CALLOUTS, with the source in front of its id, in the
// message, the ce-id and Nats-Msg-Id, so the same callout twice is stored once.
func TestCalloutsGoOnTheirLevel(t *testing.T) {
	r := newRig(t, "parity")
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id string, priority int32) race.Output {
		c := &eventsv1.Callout{
			Id: "run:callout:1:" + id + ":1", Rule: id, Kind: "template", Priority: priority, At: 1, RunId: "run",
			CreatedAt: timestamppb.New(created), ExpiresAt: timestamppb.New(created.Add(8 * time.Second)),
		}
		return race.Output{Event: ce.Event{ID: c.GetId(), Source: race.Source, Time: created}, Callout: c}
	}
	r.pub.Publish([]race.Output{mk("a", 5), mk("b", 12), mk("c", 20)})
	r.pub.Publish([]race.Output{mk("a", 5)}) // sent again: the same id
	s := r.close(t)
	got := r.srv.Read("CALLOUTS", 0)
	if len(got) != 3 {
		t.Fatalf("CALLOUTS holds %d, want 3", len(got))
	}
	for i, want := range []string{"p0", "p1", "p2"} {
		m := got[i]
		if m.Subject != "twr.v1.parity.callout."+want {
			t.Errorf("callout %d on %s, want .%s", i, m.Subject, want)
		}
		var c eventsv1.Callout
		if err := ce.Unmarshal(m.Data, &c); err != nil {
			t.Fatal(err)
		}
		id := "parity:run:callout:1:" + c.GetRule() + ":1"
		h := ce.Header(m.Header)
		if c.GetId() != id || h.Get(ce.IDHeader) != id || nats.Header(m.Header).Get("Nats-Msg-Id") != id {
			t.Errorf("callout %d: id %s, ce-id %s; want %s", i, c.GetId(), h.Get(ce.IDHeader), id)
		}
		if h.Get(ce.TypeHeader) != "run.thosewho.callout.raised.v1" {
			t.Errorf("ce-type %s", h.Get(ce.TypeHeader))
		}
	}
	if s.Callouts != 4 || s.DroppedCallouts != 0 {
		t.Errorf("stats %+v", s)
	}
}

// paddock#73's decision: a callout CALLOUTS does not acknowledge is sent again
// only until its life is over, then dropped and counted; an event is sent again
// until it is acknowledged.
func TestAStaleCalloutIsNotSentAgain(t *testing.T) {
	srv := startNATS(t)
	nc, err := nats.Connect(srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.AddStream(t, js, "events") // no CALLOUTS: nothing acknowledges a callout
	logs := &logs{}
	pub, err := Connect(Config{URL: srv.URL(), Src: "parity"}, logs.logger(),
		Options{ReconnectWait: 20 * time.Millisecond, ackTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the publisher's connection", pub.nc.IsConnected)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // long past: the life is what counts
	c := &eventsv1.Callout{
		Id: "run:callout:1:x:1", Rule: "x", Kind: "k", Priority: 5, At: 1,
		CreatedAt: timestamppb.New(created), ExpiresAt: timestamppb.New(created.Add(300 * time.Millisecond)),
	}
	pub.Publish([]race.Output{{Event: ce.Event{ID: c.GetId(), Source: race.Source, Time: created}, Callout: c}})
	waitFor(t, "the stale callout dropped", func() bool { return pub.Stats().DroppedCallouts == 1 })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if s := pub.Close(ctx); s.Callouts != 0 || s.DroppedCallouts != 1 {
		t.Errorf("stats %+v", s)
	}
	if !strings.Contains(logs.String(), "not sent again") {
		t.Errorf("not logged: %s", logs.String())
	}
}
