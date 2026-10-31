package rules_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
	_ "github.com/those-who-run/ecu/internal/rules/all"
	"github.com/those-who-run/ecu/internal/rules/rulestest"
)

// goldenLine is a line of testdata/callouts/make_golden.py's output: a callout v1
// raised.
type goldenLine struct {
	Session  int    `json:"session"`
	Frame    uint32 `json:"frame"`
	Rule     string `json:"rule"`
	Kind     string `json:"kind"`
	Priority int32  `json:"priority"`
	PitLane  string `json:"pit_lane"`
	Outcome  string `json:"outcome"`
	Released uint32 `json:"released"`
}

// show is a callout as the golden test compares it: where, what, and the frame a
// held one was released on.
func (l goldenLine) show() string {
	s := fmt.Sprintf("session %d frame %d %s %s p%d %s", l.Session, l.Frame, l.Rule, l.Kind, l.Priority, l.PitLane)
	if l.Released != 0 {
		s += fmt.Sprintf(" released %d", l.Released)
	}
	return s
}

// accepted are v1's callouts ecu does not raise on purpose, by recording and the
// line's show(), with the decision that says so. Each rule's port adds its own.
var accepted = map[string]map[string]string{
	// No track at the start (noTrackAtStart): a brief that only named the track
	// is not raised, nor is the track named on its own after the brief.
	"real-nuc-fuji-timed-2026-10-15":     {"session 1 frame 97 start_brief template p8 hold": noTrackAtStart},
	"real-nuc-spa-time-trial-2026-10-18": {"session 1 frame 72 start_brief template p8 hold": noTrackAtStart},
	"real-nuc-sugo-timed-2026-10-15":     {"session 1 frame 100 start_brief template p8 hold": noTrackAtStart},
	"real-nuc-unknown-track-2026-10-15":  {"session 1 frame 5 start_brief template p8 hold": noTrackAtStart},
	"real-quali-restart-quit-2026-10-14": {
		"session 1 frame 72 start_brief template_track p8 hold":  noTrackAtStart,
		"session 2 frame 320 start_brief template_track p8 hold": noTrackAtStart,
	},
	"real-nuc-sugo-replay-2026-10-18": {
		// GT7 jumps the lap counter from 4 to 13 on the way to the results: v1 called
		// "lap twelve" with no lap closed behind it, and ecu's lap_time, which calls
		// the closed lap, says nothing (paddock#91).
		"session 1 frame 864 lap_time delta_slower p20 hold": "https://github.com/those-who-run/paddock/issues/83#issuecomment-6504879235",
	},
}

// noTrackAtStart is the maintainer's decision of 2026-10-31, a departure from
// v1: the engineer names no track at the start (start_brief), only in the race
// summary when ecu is sure of it; every wrong track spoken so far was named
// before the detector could be sure.
const noTrackAtStart = "the maintainer's decision of 2026-10-31: no track named at the start"

// asRanked is v1's callout l as ecu raises it in a ranked session
// (rulestest.RankedSessions, rulestest.RankedKind).
func asRanked(name string, l goldenLine) goldenLine {
	if rulestest.Ranked(name, l.Session) {
		l.Kind = rulestest.RankedKind(l.Rule, l.Kind)
	}
	return l
}

// rankedQuits are the summaries ecu raises on leaving a ranked session, which v1,
// reading a timed field, never raised (rulestest.RankedDecision: ranked is in
// race_summary's quit_kinds).
var rankedQuits = map[string][]string{
	"real-quali-restart-quit-2026-10-14":   {"session 2 frame 774 race_summary template_quit p25 speak"},
	"real-nuc-spa-pit-cut-quit-2026-10-18": {"session 1 frame 1192 race_summary template_quit p25 speak"},
}

// pending are callouts ecu raises that v1 did not, by recording and the line's
// show(), each with the open paddock issue that removes it. Each must still be
// raised: the issue's PR deletes its line when its fix lands.
var pending = map[string]map[string]string{}

func readGolden(t *testing.T, name string) []goldenLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "callouts", name+".jsonl")) //nolint:gosec // a golden of the tests
	if err != nil {
		t.Fatal(err)
	}
	var all []goldenLine
	for line := range strings.Lines(string(data)) {
		var l goldenLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		all = append(all, l)
	}
	return all
}

// published are a replay's callouts as golden lines: sessions numbered as they
// start, and a callout created after the frame it was raised on (held in the
// pit lane) released on the frame it was published with. A callout raised on a
// start's held frame comes out with the frame that ends the hold (paddock#74),
// created as of its own frame: that is not a release.
func published(out []race.Output) []goldenLine {
	sessions := map[string]int{}
	times := map[uint32]time.Time{}
	var frame uint32
	var lines []goldenLine
	for _, o := range out {
		switch {
		case o.Frame != nil:
			frame = o.Frame.GetSeq()
			times[frame] = o.Event.Time
		case o.Started != nil:
			sessions[o.Session] = len(sessions) + 1
		case o.Callout != nil:
			c := o.Callout
			l := goldenLine{
				Session: sessions[c.GetSessionId()], Frame: c.GetAt(), Rule: c.GetRule(), Kind: c.GetKind(),
				Priority: c.GetPriority(), PitLane: strings.ToLower(strings.TrimPrefix(c.GetPitLane().String(), "PIT_LANE_")),
			}
			if frame != c.GetAt() && !c.GetCreatedAt().AsTime().Equal(times[c.GetAt()]) {
				l.Released = frame
			}
			lines = append(lines, l)
		}
	}
	return lines
}

// R6 and the parity of every rule ported: on each of v1's 25 recordings, the
// callouts of every rule registered are v1's, on the same frame of the same
// session, with the same kind, priority and pit lane, and a callout v1 held in
// the pit lane is published on the frame v1 released it. A callout v1 raised in
// the lane and dropped is never published. The order within a frame is not
// compared: the parity diff matches by rule and frame.
func TestEveryCalloutIsV1sOnEveryRecording(t *testing.T) {
	e, err := rules.New(rules.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ported := e.Rules()
	if len(ported) == 0 {
		t.Fatal("no rule registered")
	}
	var compared int
	for _, name := range rulestest.Recordings {
		t.Run(name, func(t *testing.T) {
			var want []string
			for _, l := range readGolden(t, name) {
				if !slices.Contains(ported, l.Rule) || l.Outcome == "unpublished" {
					continue
				}
				if why, ok := accepted[name][l.show()]; ok {
					t.Logf("accepted: %s (%s)", l.show(), why)
					continue
				}
				if r := asRanked(name, l); r != l {
					t.Logf("ranked: %s as %q (%s)", l.show(), r.Kind, rulestest.RankedDecision)
					if r.Kind == "" {
						continue // a v1 position call, in place of which ecu raises RankCalls
					}
					l = r
				}
				want = append(want, l.show())
			}
			want = append(want, rankedQuits[name]...)
			for _, c := range rulestest.RankCalls[name] {
				want = append(want, goldenLine{Session: c.Session, Frame: c.Frame, Rule: "position", Kind: c.Kind, Priority: 12, PitLane: "drop"}.show())
			}
			var got []string
			extra := maps.Clone(pending[name])
			for _, l := range published(rulestest.Replay(t, name)) {
				if why, ok := extra[l.show()]; ok {
					t.Logf("pending: %s (%s)", l.show(), why)
					delete(extra, l.show())
					continue
				}
				got = append(got, l.show())
			}
			for line, why := range extra {
				t.Errorf("pending %s (%s) is no longer raised: delete its line", line, why)
			}
			slices.Sort(got)
			slices.Sort(want)
			compared += len(want)
			if !slices.Equal(got, want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
	if compared == 0 {
		t.Error("no callout of a registered rule in any golden")
	}
}

// paddock#74: over the race replay the driver watched on 2026-10-29
// (internal/race's testdata/start, frames 134511 to 144497 of the live
// recording), where ecu spoke a start brief and a race summary, no rule raises
// anything; session 5, the next start (the fixture's frame 824), is briefed on
// frame 944, as v1 briefed it.
func TestNoCalloutOverTheReplayOf20261029(t *testing.T) {
	var first []string
	for _, l := range published(rulestest.Replay(t, "real-nuc-short-shift-replay-2026-10-29")) {
		if l.Frame < 824 || l.Session != 1 {
			t.Errorf("a callout over the replay: %s", l.show())
		}
		if len(first) == 0 {
			first = append(first, l.show())
		}
	}
	if want := []string{"session 1 frame 944 start_brief template p8 hold"}; !slices.Equal(first, want) {
		t.Errorf("first callout %v, want %v", first, want)
	}
}
