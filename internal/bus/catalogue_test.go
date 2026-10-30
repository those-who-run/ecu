package bus

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"github.com/those-who-run/ecu/internal/race"
)

// entry is a subject of contracts' subjects.yaml: its pattern, ce-type and msg_id
// scheme.
type entry struct{ pattern, ceType, msgID string }

// catalogue reads the subjects of subjects.yaml at the contracts version go.mod
// requires. Only the two keys this test needs are read, by their place in the
// file's fixed layout (a subject's entry starts "  - pattern:" and its keys are
// indented under it; any other entry or top-level key ends it), so the test needs
// no YAML package.
func catalogue(t *testing.T) []entry {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}",
		"github.com/those-who-run/contracts").Output()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(strings.TrimSpace(string(out)), "subjects.yaml")) //nolint:gosec // the module's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var all []entry
	in := false // in a subject's entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "  - pattern: "); ok {
			all, in = append(all, entry{pattern: strings.TrimSpace(v)}), true
		} else if v, ok := strings.CutPrefix(line, "    ce_type: "); ok && in {
			all[len(all)-1].ceType = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "    msg_id: "); ok && in {
			all[len(all)-1].msgID = strings.TrimSpace(v)
		} else if strings.HasPrefix(line, "  - ") || !strings.HasPrefix(line, " ") {
			in = false
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no subjects in subjects.yaml")
	}
	return all
}

// matches says the subject fits the pattern: each <placeholder> one token, every
// other token the same.
func matches(pattern, subject string) bool {
	p, s := strings.Split(pattern, "."), strings.Split(subject, ".")
	if len(p) != len(s) {
		return false
	}
	for i := range p {
		if strings.HasPrefix(p[i], "<") && strings.HasSuffix(p[i], ">") {
			continue
		}
		if p[i] != s[i] {
			return false
		}
	}
	return true
}

// published is every message a replay of the Suzuka recording (AC1's) puts on
// twr.v1.parity.>, and the run it was published under.
func published(t *testing.T) ([]*nats.Msg, string) {
	t.Helper()
	r := newRig(t, "parity")
	sub, err := r.nc.SubscribeSync("twr.v1.parity.>")
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
	step(m, r.pub, packets(t, suzuka))
	r.pub.Publish(m.EndOfInput())
	stats := r.close(t)

	var all []*nats.Msg
	for n := stats.Frames + stats.LoFrames + stats.Events; n > 0; n-- {
		msg, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("%d messages still to come: %v", n, err)
		}
		all = append(all, msg)
	}
	return all, m.Run()
}

// AC6: every subject published in a replay (AC1's) is in subjects.yaml with the
// ce-type it was sent with (R8), the 20 Hz frame's too since contracts v0.2.3
// (paddock#137).
func TestEverySubjectIsInTheCatalogue(t *testing.T) {
	msgs, _ := published(t)
	sent := map[[2]string]bool{} // subject with <sid> for the session id, ce-type
	for _, msg := range msgs {
		sent[[2]string{generic(msg), msg.Header.Get(ce.TypeHeader)}] = true
	}
	// The replay's thirteen events include race.final_lap, race.position,
	// lap.best, race.finished, session.paused and session.resumed; pit.teleport is in no recording, so
	// TestAPitTeleportIsInTheCatalogue checks it.
	if len(sent) != 15 {
		t.Errorf("%d kinds of message sent, want 15: frames, .lo and thirteen events: %v", len(sent), sent)
	}
	entries := catalogue(t)
	for k := range sent {
		subject, ceType := k[0], k[1]
		var found bool
		for _, e := range entries {
			if matches(e.pattern, subject) {
				found = true
				if e.ceType != ceType {
					t.Errorf("%s was sent as %s; subjects.yaml says %s", subject, ceType, e.ceType)
				}
			}
		}
		if !found {
			t.Errorf("%s (%s) is not in subjects.yaml", subject, ceType)
		}
	}
}

// generic is the message's subject with its session id as <sid>, so the events of
// every session count once.
func generic(msg *nats.Msg) string {
	tokens := strings.Split(msg.Subject, ".")
	if len(tokens) > 4 && tokens[3] == "evt" {
		tokens[4] = "<sid>"
	}
	return strings.Join(tokens, ".")
}

func TestMatches(t *testing.T) {
	for _, c := range []struct {
		pattern, subject string
		want             bool
	}{
		{"twr.v1.<src>.frame.gt7", "twr.v1.parity.frame.gt7", true},
		{"twr.v1.<src>.frame.gt7", "twr.v1.parity.frame.gt7.lo", false},
		{"twr.v1.<src>.evt.<sid>.session.started", "twr.v1.live.evt.<sid>.session.started", true},
		{"twr.v1.<src>.evt.<sid>.session.started", "twr.v1.live.evt.<sid>.session.ended", false},
	} {
		if got := matches(c.pattern, c.subject); got != c.want {
			t.Errorf("matches(%q, %q) = %t", c.pattern, c.subject, got)
		}
	}
}

// N2 of the review of ecu#12: every id ecu publishes, ce-id and Nats-Msg-Id alike,
// follows the msg_id scheme subjects.yaml gives for its subject (R4, as amended by
// paddock#279): <src>:<sid>:<area>.<what>:<part> for an event, with the subject's
// own <src> and <sid>, and <run>:frame:<seq> for a frame.
func TestEveryIDFollowsTheCatalogue(t *testing.T) {
	msgs, run := published(t)
	entries := catalogue(t)
	checked := map[string]bool{}
	for _, msg := range msgs {
		id := msg.Header.Get(ce.IDHeader)
		if msg.Header.Get(ce.MsgIDHeader) != id {
			t.Fatalf("%s: Nats-Msg-Id %q, ce-id %q", msg.Subject, msg.Header.Get(ce.MsgIDHeader), id)
		}
		var scheme string
		for _, e := range entries {
			if matches(e.pattern, msg.Subject) {
				scheme = e.msgID
			}
		}
		if scheme == "" {
			continue // TestEverySubjectIsInTheCatalogue names it
		}
		tokens := strings.Split(msg.Subject, ".")
		values := map[string]string{"src": tokens[2], "run": run}
		if tokens[3] == "evt" {
			values["sid"] = tokens[4]
		}
		if !followsScheme(scheme, id, values) {
			t.Fatalf("%s has id %q; subjects.yaml says %s (src %s, run %s)", msg.Subject, id, scheme, tokens[2], run)
		}
		checked[scheme] = true
	}
	// pit.teleport's scheme is TestAPitTeleportIsInTheCatalogue's, as above.
	if len(checked) != 14 {
		t.Errorf("checked ids against %d schemes, want 14: the frames' (both subjects share one) and the thirteen events': %v", len(checked), checked)
	}
}

// followsScheme says id is the msg_id scheme with each <placeholder> filled: from
// values where it names one, else a number from 1.
func followsScheme(scheme, id string, values map[string]string) bool {
	var re strings.Builder
	re.WriteString("^")
	for rest := scheme; rest != ""; {
		open := strings.Index(rest, "<")
		if open < 0 {
			re.WriteString(regexp.QuoteMeta(rest))
			break
		}
		end := strings.Index(rest[open:], ">") + open
		re.WriteString(regexp.QuoteMeta(rest[:open]))
		if v, ok := values[rest[open+1:end]]; ok {
			re.WriteString(regexp.QuoteMeta(v))
		} else {
			re.WriteString("[1-9][0-9]*")
		}
		rest = rest[end+1:]
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String()).MatchString(id)
}

func TestFollowsScheme(t *testing.T) {
	v := map[string]string{"src": "parity", "sid": "S", "run": "R"}
	for _, c := range []struct {
		scheme, id string
		want       bool
	}{
		{"<src>:<sid>:lap.completed:<n>", "parity:S:lap.completed:3", true},
		{"<src>:<sid>:lap.completed:<n>", "S:lap.completed:3", false},
		{"<src>:<sid>:lap.completed:<n>", "live:S:lap.completed:3", false},
		{"<src>:<sid>:session.started:1", "parity:S:session.started:1", true},
		{"<src>:<sid>:session.started:1", "parity:S:session:started", false},
		{"<run>:frame:<seq>", "R:frame:12", true},
		{"<run>:frame:<seq>", "R:frame:0", false},
		{"<src>:<sid>:session.track:<run>:<seq>", "parity:S:session.track:R:42", true},
		{"<src>:<sid>:session.track:<run>:<seq>", "parity:S:session.track:42", false},
		{"<src>:<sid>:session.kind:<run>:<seq>", "parity:S:session.kind:R:42", true},
	} {
		if got := followsScheme(c.scheme, c.id, v); got != c.want {
			t.Errorf("followsScheme(%q, %q) = %t", c.scheme, c.id, got)
		}
	}
}

// An event published with no session open goes on evt._, with the run in the id
// where the session would be (subjects.yaml, paddock#279). The race state sends
// none yet (Prost will), so the event is built by hand.
func TestAnEventWithNoSessionGoesOnEvtUnderscore(t *testing.T) {
	r := newRig(t, "parity")
	sub, err := r.nc.SubscribeSync("twr.v1.parity.evt._.>")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	const run = "01JBUSRUN0000000000000000A"
	r.pub.Publish([]race.Output{{
		Event: ce.Event{ID: run + ":lap.completed:1", Source: race.Source, Time: epoch},
		Lap:   &eventsv1.LapCompleted{Lap: 1},
	}})
	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Subject != "twr.v1.parity.evt._.lap.completed" {
		t.Errorf("subject %s, want twr.v1.parity.evt._.lap.completed", msg.Subject)
	}
	var scheme string
	for _, e := range catalogue(t) {
		if matches(e.pattern, msg.Subject) {
			scheme = e.msgID
		}
	}
	id := msg.Header.Get(ce.IDHeader)
	if scheme == "" || !followsScheme(scheme, id, map[string]string{"src": "parity", "sid": run}) {
		t.Errorf("id %q, want the scheme %q with the run as <sid>", id, scheme)
	}
	if s := r.close(t); s.Events != 1 {
		t.Errorf("stats %+v, want the event acknowledged", s)
	}
}

// paddock#79: no recording has a teleport, so pit.teleport is checked by hand: its
// subject and ce-type are in the catalogue, and its id follows the catalogue's
// <src>:<sid>:pit.teleport:<entry>.
func TestAPitTeleportIsInTheCatalogue(t *testing.T) {
	r := newRig(t, "parity")
	const sid = "01JBUSSESSION0000000000000"
	sub, err := r.nc.SubscribeSync("twr.v1.parity.evt." + sid + ".>")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	r.pub.Publish([]race.Output{{
		Event:       ce.Event{ID: sid + ":pit.teleport:2", Source: race.Source, Time: epoch},
		Session:     sid,
		PitTeleport: &eventsv1.PitTeleport{SessionId: sid, Entry: 2, Lap: 3},
	}})
	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := "twr.v1.parity.evt." + sid + ".pit.teleport"; msg.Subject != want {
		t.Errorf("subject %s, want %s", msg.Subject, want)
	}
	var found bool
	for _, e := range catalogue(t) {
		if !matches(e.pattern, msg.Subject) {
			continue
		}
		found = true
		if got := msg.Header.Get(ce.TypeHeader); got != e.ceType {
			t.Errorf("ce-type %s; subjects.yaml says %s", got, e.ceType)
		}
		if id := msg.Header.Get(ce.IDHeader); !followsScheme(e.msgID, id, map[string]string{"src": "parity", "sid": sid}) ||
			id != "parity:"+sid+":pit.teleport:2" {
			t.Errorf("id %q, want the scheme %q with entry 2", id, e.msgID)
		}
	}
	if !found {
		t.Errorf("%s is not in subjects.yaml", msg.Subject)
	}
	if s := r.close(t); s.Events != 1 {
		t.Errorf("stats %+v, want the event acknowledged", s)
	}
}

// paddock#74 R4: a held start that was a replay or a demo goes on its own
// subject, with the catalogue's ce-type and <src>:<sid>:session.discarded:1, and
// the publisher logs one line saying why.
func TestASessionDiscardedIsInTheCatalogue(t *testing.T) {
	r := newRig(t, "parity")
	const sid = "01JBUSSESSION0000000000000"
	sub, err := r.nc.SubscribeSync("twr.v1.parity.evt." + sid + ".>")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	const why = "fuel is draining (98.3 -> 97.6), which only a replay does"
	r.pub.Publish([]race.Output{{
		Event:   ce.Event{ID: sid + ":session.discarded:1", Source: race.Source, Time: epoch},
		Session: sid,
		Discarded: &eventsv1.SessionDiscarded{
			SessionId: sid, Reason: eventsv1.DiscardReason_DISCARD_REASON_REPLAY, Evidence: why, FirstSeq: 62, At: 114,
		},
	}})
	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := "twr.v1.parity.evt." + sid + ".session.discarded"; msg.Subject != want {
		t.Errorf("subject %s, want %s", msg.Subject, want)
	}
	var found bool
	for _, e := range catalogue(t) {
		if !matches(e.pattern, msg.Subject) {
			continue
		}
		found = true
		if got := msg.Header.Get(ce.TypeHeader); got != e.ceType {
			t.Errorf("ce-type %s; subjects.yaml says %s", got, e.ceType)
		}
		if id := msg.Header.Get(ce.IDHeader); !followsScheme(e.msgID, id, map[string]string{"src": "parity", "sid": sid}) ||
			id != "parity:"+sid+":session.discarded:1" {
			t.Errorf("id %q, want the scheme %q", id, e.msgID)
		}
	}
	if !found {
		t.Errorf("%s is not in subjects.yaml", msg.Subject)
	}
	if s := r.close(t); s.Events != 1 {
		t.Errorf("stats %+v, want the event acknowledged", s)
	}
	if logs := r.logs.String(); strings.Count(logs, "start discarded") != 1 ||
		!strings.Contains(logs, "DISCARD_REASON_REPLAY") || !strings.Contains(logs, why) {
		t.Errorf("logs %s, want one line with the reason and the evidence", logs)
	}
}

// paddock#87: the watchdog's events go on evt.<sid> with a session open and on
// evt._ with none, each with the catalogue's ce-type and id, <run> in the id
// and run_id in the message.
func TestTelemetryLostAndRestoredAreInTheCatalogue(t *testing.T) {
	r := newRig(t, "parity")
	const sid, run = "01JBUSSESSION0000000000000", "01JBUSRUN0000000000000000A"
	sub, err := r.nc.SubscribeSync("twr.v1.parity.evt.>")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	r.pub.Publish([]race.Output{
		{
			Event: ce.Event{ID: sid + ":telemetry.lost:" + run + ":7", Source: race.Source, Time: epoch}, Session: sid,
			TelemetryLost: &eventsv1.TelemetryLost{SessionId: sid, RunId: run, At: 7, SilenceMs: 3000},
		},
		{
			Event:             ce.Event{ID: run + ":telemetry.restored:9", Source: race.Source, Time: epoch},
			TelemetryRestored: &eventsv1.TelemetryRestored{RunId: run, LostAt: 7, At: 9, GapMs: 5000},
		},
	})
	want := []struct{ subject, id string }{
		{"twr.v1.parity.evt." + sid + ".telemetry.lost", "parity:" + sid + ":telemetry.lost:" + run + ":7"},
		{"twr.v1.parity.evt._.telemetry.restored", "parity:" + run + ":telemetry.restored:9"},
	}
	for _, w := range want {
		msg, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if msg.Subject != w.subject || msg.Header.Get(ce.IDHeader) != w.id {
			t.Errorf("%s %s, want %s %s", msg.Subject, msg.Header.Get(ce.IDHeader), w.subject, w.id)
		}
		var found bool
		underscore := strings.Contains(msg.Subject, ".evt._.")
		for _, e := range catalogue(t) {
			// evt._ also matches the <sid> pattern: the entry is the evt._ one.
			if !matches(e.pattern, msg.Subject) || underscore != strings.Contains(e.pattern, ".evt._.") {
				continue
			}
			found = true
			if got := msg.Header.Get(ce.TypeHeader); got != e.ceType {
				t.Errorf("ce-type %s; subjects.yaml says %s", got, e.ceType)
			}
			if id := msg.Header.Get(ce.IDHeader); !followsScheme(e.msgID, id, map[string]string{"src": "parity", "sid": sid, "run": run}) {
				t.Errorf("id %q, want the scheme %q", id, e.msgID)
			}
		}
		if !found {
			t.Errorf("%s is not in subjects.yaml", msg.Subject)
		}
	}
	if s := r.close(t); s.Events != 2 {
		t.Errorf("stats %+v, want both events acknowledged", s)
	}
}

// paddock#97, R5: a car alarm, which only the rule engine raises (the temps
// rule), goes on its subject in subjects.yaml with its ce-type and its id scheme,
// <src>:<sid>:car.alarm:<check>:<run>:<seq>.
func TestACarAlarmIsInTheCatalogue(t *testing.T) {
	r := newRig(t, "parity")
	const sid, run = "01JBUSSESSION0000000000000", "01JBUSRUN00000000000000000"
	sub, err := r.nc.SubscribeSync("twr.v1.parity.evt." + sid + ".>")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	r.pub.Publish([]race.Output{{
		Event:   ce.Event{ID: sid + ":car.alarm:tyre_hot:" + run + ":554", Source: race.Source, Time: epoch},
		Session: sid,
		Alarm: &eventsv1.CarAlarmRaised{
			SessionId: sid, Check: eventsv1.CarCheck_CAR_CHECK_TYRE_HOT, At: 554, RunId: run, ValueC: 178.8, ThresholdC: 120,
			Tyres: []*eventsv1.TyreTemp{{Corner: eventsv1.Corner_CORNER_REAR_LEFT, TempC: 178.8}},
		},
	}})
	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := "twr.v1.parity.evt." + sid + ".car.alarm"; msg.Subject != want {
		t.Errorf("subject %s, want %s", msg.Subject, want)
	}
	var found bool
	for _, e := range catalogue(t) {
		if !matches(e.pattern, msg.Subject) {
			continue
		}
		found = true
		if got := msg.Header.Get(ce.TypeHeader); got != e.ceType {
			t.Errorf("ce-type %s; subjects.yaml says %s", got, e.ceType)
		}
		values := map[string]string{"src": "parity", "sid": sid, "check": "tyre_hot", "run": run}
		if id := msg.Header.Get(ce.IDHeader); !followsScheme(e.msgID, id, values) ||
			id != "parity:"+sid+":car.alarm:tyre_hot:"+run+":554" {
			t.Errorf("id %q, want the scheme %q", id, e.msgID)
		}
	}
	if !found {
		t.Errorf("%s is not in subjects.yaml", msg.Subject)
	}
	if s := r.close(t); s.Events != 1 {
		t.Errorf("stats %+v, want the event acknowledged", s)
	}
}
