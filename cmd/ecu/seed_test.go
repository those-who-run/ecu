package main

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/those-who-run/contracts/ce"
	"github.com/those-who-run/ecu/internal/bus/bustest"
)

// stored is what a replay left in the streams: each message's subject, headers
// and payload, in order, with no stream sequence numbers.
type stored struct {
	events, callouts []bustest.Msg
	frames           []string // each frame's id
}

func storedBy(t *testing.T, srv *bustest.Server) stored {
	t.Helper()
	var s stored
	s.events = srv.Read("EVENTS", 0)
	s.callouts = srv.Read("CALLOUTS", 0)
	for _, m := range srv.Read("TELEMETRY", 0) {
		s.frames = append(s.frames, ce.Header(m.Header).Get(ce.IDHeader))
	}
	return s
}

// sameMsgs says where two streams' contents differ, or "" when they do not.
func sameMsgs(a, b []bustest.Msg) string {
	if len(a) != len(b) {
		return fmt.Sprintf("%d messages / %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Subject != b[i].Subject || !bytes.Equal(a[i].Data, b[i].Data) {
			return a[i].Subject + " / " + b[i].Subject
		}
		for k, v := range a[i].Header {
			if strings.Join(v, ",") != strings.Join(b[i].Header[k], ",") {
				return a[i].Subject + ": " + k + " " + strings.Join(v, ",") + " / " + strings.Join(b[i].Header[k], ",")
			}
		}
	}
	return ""
}

// seededReplay replays path once onto a fresh server with args and returns what
// the streams hold.
func seededReplay(t *testing.T, stdin []byte, args ...string) (stored, replayed) {
	t.Helper()
	srv := bustest.WithStreams(t)
	got := runReplay(t, stdin, append([]string{"--nats", srv.URL(), "--speed", "max"}, args...)...)
	if got.code != 0 {
		t.Fatalf("%v: exit %d, stderr %q, log %s", args, got.code, got.stderr, got.stdout)
	}
	return storedBy(t, srv), got
}

// AC1 of paddock#35, with --seed (the decision of 2026-10-24) and paddock#309: the
// same recording replayed twice with the same --seed, --src and --clock gives
// identical EVENTS and CALLOUTS contents, apart from stream sequence numbers, and
// the same frame ids. A different seed, or none, gives other ids.
func TestASeededReplayIsTheSameTwice(t *testing.T) {
	path := recordingPath(t, suzuka)
	args := []string{"--src", "parity", "--clock", "2026-01-01T00:00:00Z", "--seed", "7", path}
	first, _ := seededReplay(t, nil, args...)
	second, _ := seededReplay(t, nil, args...)
	if len(first.events) < 8 || len(first.frames) == 0 {
		t.Fatalf("the replay published %d events and %d frames", len(first.events), len(first.frames))
	}
	if d := sameMsgs(first.events, second.events); d != "" {
		t.Errorf("EVENTS differs between two seeded runs: %s", d)
	}
	if d := sameMsgs(first.callouts, second.callouts); d != "" {
		t.Errorf("CALLOUTS differs between two seeded runs: %s", d)
	}
	if strings.Join(first.frames, " ") != strings.Join(second.frames, " ") {
		t.Errorf("the frame ids differ between two seeded runs: %s… / %s…", first.frames[0], second.frames[0])
	}

	other, _ := seededReplay(t, nil, "--src", "parity", "--seed", "8", path)
	random, _ := seededReplay(t, nil, "--src", "parity", path)
	for name, s := range map[string]stored{"--seed 8": other, "no --seed": random} {
		if s.frames[0] == first.frames[0] || sameMsgs(s.events, first.events) == "" {
			t.Errorf("%s gives the ids --seed 7 gives: %s", name, s.frames[0])
		}
	}
}

// paddock#309: under --seed the ids depend on the recording and the source too, so
// two recordings, or two sources, never share a run id: a frame's id has no
// <src>, and TELEMETRY would drop the second's frames as duplicates. The default
// source's suffix comes from the seed, so a seeded replay with no --src is
// reproducible too, and still matches AC4's form. Standard input replays as the
// file does.
func TestTheSeedKeysOnTheRecordingAndTheSource(t *testing.T) {
	path := synthetic(t, 40, nil)
	data, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	shorter := synthetic(t, 39, nil)
	_, a := seededReplay(t, nil, "--seed", "7", path)
	fromFile, _ := seededReplay(t, nil, "--seed", "7", "--src", "parity", path)
	fromStdin, _ := seededReplay(t, data, "--seed", "7", "--src", "parity", "-")
	otherSrc, _ := seededReplay(t, nil, "--seed", "7", "--src", "replay-other", path)
	otherRec, _ := seededReplay(t, nil, "--seed", "7", "--src", "parity", shorter)
	_, b := seededReplay(t, nil, "--seed", "7", path)

	srcOf := regexp.MustCompile(`"src":"(replay-synthetic-[a-z0-9]+)"`)
	sa, sb := srcOf.FindStringSubmatch(a.stdout), srcOf.FindStringSubmatch(b.stdout)
	if sa == nil || sb == nil || sa[1] != sb[1] {
		t.Errorf("the seeded default src is not the same twice: %v, %v", sa, sb)
	}
	if fromStdin.frames[0] != fromFile.frames[0] {
		t.Errorf("standard input gives %s, the file %s", fromStdin.frames[0], fromFile.frames[0])
	}
	run := func(id string) string { r, _, _ := strings.Cut(id, ":"); return r }
	for name, s := range map[string]stored{"another src": otherSrc, "another recording": otherRec} {
		if run(s.frames[0]) == run(fromFile.frames[0]) {
			t.Errorf("%s shares the run id %s", name, run(s.frames[0]))
		}
	}
}

// paddock#309's dedupe: a second seeded replay of one recording under the same
// --src, onto the same stream within its duplicate window, publishes the same
// Nats-Msg-Ids, so the stream keeps the first run's messages and no more. The
// replay is idempotent, and still exits 0.
func TestASecondSeededReplayIsDeduplicated(t *testing.T) {
	srv := bustest.WithStreams(t)
	path := synthetic(t, 200, nil)
	args := []string{"--nats", srv.URL(), "--speed", "max", "--src", "parity", "--seed", "7", path}
	if got := runReplay(t, nil, args...); got.code != 0 {
		t.Fatalf("the first replay: exit %d, stderr %q", got.code, got.stderr)
	}
	frames, events := srv.Count("TELEMETRY"), srv.Count("EVENTS")
	if got := runReplay(t, nil, args...); got.code != 0 {
		t.Fatalf("the second replay: exit %d, stderr %q, log %s", got.code, got.stderr, got.stdout)
	}
	if f, e := srv.Count("TELEMETRY"), srv.Count("EVENTS"); f != frames || e != events {
		t.Errorf("after the second replay the streams hold %d frames and %d events, want the first's %d and %d",
			f, e, frames, events)
	}
}
