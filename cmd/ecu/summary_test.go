package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/those-who-run/ecu/internal/service"
	"github.com/those-who-run/ecu/race"
)

// summaryLines is the "replayed session" lines in a replay's log, each as
// "<session> <kind> <laps> <ended>".
func summaryLines(t *testing.T, log string) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var l struct {
			Msg, Session, Kind, Ended string
			Laps                      int
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if l.Msg == "replayed session" {
			lines = append(lines, fmt.Sprintf("%s %s %d %s", l.Session, l.Kind, l.Laps, l.Ended))
		}
	}
	return lines
}

// R5 of paddock#35: at the end, one line per session, in the order they started,
// with its id, kind, laps completed and why it ended: the same facts as its
// session.ended. The qualifying recording has two sessions, the first ended by a
// restart and the second by a quit.
func TestTheSummaryHasOneLinePerSession(t *testing.T) {
	in := readAll(t, openRecording(t, recordingPath(t, "real-quali-restart-quit-2026-10-14")))
	pub, sums := &fakePublisher{}, &sessionSums{}
	m := machine()
	_ = drive(t.Context(), &slice{all: in}, (&pacer{clock: defaultClock, t0: in[0].T}).at, m, nil, summing{pub, sums},
		nil, nil, nil)
	sums.keptOpen(m.Open())
	var buf bytes.Buffer
	sums.log(t.Context(), service.NewLogger(&buf, 0))

	var want []string
	for _, o := range pub.outs {
		if e := o.Ended; e != nil {
			want = append(want, fmt.Sprintf("%s %s %d %s", e.GetSessionId(),
				strings.TrimPrefix(e.GetSummary().GetKind().GetSessionType().String(), "SESSION_TYPE_"),
				e.GetSummary().GetLapsCompleted(), strings.TrimPrefix(e.GetReason().String(), "END_REASON_")))
		}
	}
	got := summaryLines(t, buf.String())
	if len(want) != 2 || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the summary is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(got) == 2 && (!strings.HasSuffix(got[0], " 1 RESTARTED") || !strings.HasSuffix(got[1], " 2 QUIT")) {
		t.Errorf("the summary is %q, want 1 lap restarted, then 2 laps quit", got)
	}
}

// A session --keep-open leaves open (or a stop keeps) is in the summary too, as
// kept open, with the laps it had.
func TestTheSummaryHasTheSessionKeptOpen(t *testing.T) {
	in := readAll(t, openRecording(t, recordingPath(t, suzuka)))
	sums := &sessionSums{}
	m := machine()
	at := (&pacer{clock: defaultClock, t0: in[0].T}).at
	for _, r := range in[:len(in)/2] {
		sums.see(m.Step(race.Received{Received: r, Time: at(r)}))
	}
	open := m.Open()
	sums.keptOpen(open)
	var buf bytes.Buffer
	sums.log(t.Context(), service.NewLogger(&buf, 0))
	got := summaryLines(t, buf.String())
	want := fmt.Sprintf("%s LAP_RACE %d KEPT_OPEN", open.GetSessionId(), open.GetSummary().GetLapsCompleted())
	if len(got) != 1 || got[0] != want || open.GetSummary().GetLapsCompleted() == 0 {
		t.Errorf("the summary is %q, want %q, with a lap or more", got, want)
	}
}
