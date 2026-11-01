package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/ecu/internal/live"
	"github.com/those-who-run/ecu/internal/record"
	"github.com/those-who-run/salsa"
	"github.com/those-who-run/salsa/salsatest"
)

// runRecordings runs `ecu recordings` with the args, and gives what it printed on
// each stream and its exit code.
func runRecordings(t *testing.T, env map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = recordings(t.Context(), args, func(k string) string { return env[k] }, &out, &errOut)
	return out.String(), errOut.String(), code
}

// AC1 and AC3 (paddock#354): a fake PS5 sends packets, goes quiet for longer
// than the recordings' idle setting (set low here), and sends more: the drive
// gives two recordings, the first a whole gzip once the silence passed, and `ecu
// recordings list` shows both, and the one archived as archived, with its note.
func TestASilenceSplitsTheDriveAndTheListShowsBoth(t *testing.T) {
	in := readAll(t, openRecording(t, recordingPath(t, suzuka)))[:600]
	ps5, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps5.Close() }()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := c.LocalAddr().(*net.UDPAddr)
	_ = c.Close()
	logger := discard()
	idle := 200 * time.Millisecond
	src := live.Open(live.Config{PS5: "127.0.0.1", Heartbeat: time.Hour, HeartbeatPort: ps5.LocalAddr().(*net.UDPAddr).Port,
		Listen: listen.String()}, logger)
	dir := t.TempDir()
	rec, err := record.Open(dir, record.Options{Idle: idle, Keep: 30 * 24 * time.Hour}, logger, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	got := &fakePublisher{}
	ctx, cancel := context.WithCancel(t.Context())
	driven := make(chan struct{})
	go func() {
		defer close(driven)
		start := src.Start()
		_ = drive(ctx, src, func(r salsa.Received) time.Time { return start.Add(time.Duration(r.T * float64(time.Second))) },
			machine(), nil, got, rec, nil, nil)
	}()
	buf := make([]byte, 8)
	_ = ps5.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := ps5.ReadFromUDP(buf); err != nil {
		t.Fatalf("no heartbeat: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); !src.Stats().Listening; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("not listening after 5 s")
		}
	}
	send := func(batch []salsa.Received, frames int) {
		for i, r := range batch {
			if _, err := ps5.WriteToUDP(salsatest.Encode(r.Packet), listen); err != nil {
				t.Fatal(err)
			}
			if i%50 == 49 {
				waitFrames(t, got, frames-len(batch)+i+1)
			}
		}
		waitFrames(t, got, frames)
	}
	send(in[:300], 300)
	// The silence: the first outing ends, its file whole while ecu runs.
	first := rec.Stats().Path
	for deadline := time.Now().Add(10 * time.Second); !readsWhole(first, 300); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first outing is not a whole recording of 300 packets 10 s after its last")
		}
	}
	send(in[300:], 600)
	cancel()
	<-driven
	_ = src.Close()
	s := rec.Close(t.Context())
	if s.Outings != 2 || s.Frames != 600 || s.Path == first {
		t.Fatalf("stats %+v; want 2 outings of 600 frames", s)
	}

	id := strings.TrimSuffix(filepath.Base(first), record.Ext)
	env := map[string]string{"TWR_ECU_RECORDINGS_DIR": dir, "TWR_ECU_RECORDINGS_IDLE_SECS": "0.001"}
	out, errOut, code := runRecordings(t, env, "archive", id, "--note", "the first half", "--issue", "354")
	if code != 0 || !strings.HasPrefix(out, "archived "+id+"  300 frames") || !strings.Contains(out, "#354") {
		t.Fatalf("archive: exit %d\n%s%s", code, out, errOut)
	}
	out, errOut, code = runRecordings(t, env, "list")
	if code != 0 {
		t.Fatalf("list: exit %d: %s", code, errOut)
	}
	var heads []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "   ") {
			heads = append(heads, line)
		}
	}
	second := strings.TrimSuffix(filepath.Base(s.Path), record.Ext)
	if len(heads) != 2 || !strings.HasPrefix(heads[0], id+"  ") || !strings.HasPrefix(heads[1], second+"  ") {
		t.Fatalf("list:\n%s", out)
	}
	if !strings.Contains(heads[0], "300 frames") || !strings.Contains(heads[0], "archived #354") ||
		strings.Contains(heads[1], "archived") || !strings.Contains(heads[1], "300 frames") {
		t.Errorf("list:\n%s", out)
	}
	if !strings.Contains(out, "   note: the first half\n") || strings.Contains(out, "incomplete") {
		t.Errorf("list:\n%s", out)
	}
}

// readsWhole says whether the recording at path reads to its end, n packets.
func readsWhole(path string, n int) bool {
	src, err := salsa.OpenRecording(path)
	if err != nil {
		return false
	}
	defer func() { _ = src.Close() }()
	for i := 0; ; i++ {
		_, err := src.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return i == n
		}
		if err != nil {
			return false
		}
	}
}

func waitFrames(t *testing.T, got *fakePublisher, frames int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for got.count() < frames {
		if time.Now().After(deadline) {
			t.Fatalf("%d frames published, want %d", got.count(), frames)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The list says which recording is cut short, as one still being written or
// cut off by a crash is, an empty one or one with a gzip header only included,
// and reads what it can of it; which one is damaged otherwise; and an empty
// directory. A note whose recording is gone is warned of.
func TestTheListSaysARecordingIsIncomplete(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"TWR_ECU_RECORDINGS_DIR": dir}
	if out, _, code := runRecordings(t, env, "list"); code != 0 || !strings.HasPrefix(out, "no recordings in ") {
		t.Errorf("empty: exit %d: %s", code, out)
	}
	b, err := os.ReadFile(recordingPath(t, suzuka))
	if err != nil {
		t.Fatal(err)
	}
	var header bytes.Buffer
	gz := gzip.NewWriter(&header)
	_ = gz.Flush()
	corrupt := slices.Clone(b)
	for i := len(b) / 2; i < len(b)/2+64; i++ {
		corrupt[i] ^= 0xff
	}
	for name, data := range map[string][]byte{
		"20261031-200509.jsonl.gz": b[:len(b)/2],
		"20261031-200510.jsonl.gz": []byte("not a recording\n"),
		"20261031-200511.jsonl.gz": nil,
		"20261031-200512.jsonl.gz": header.Bytes(),
		"20261031-200513.jsonl.gz": corrupt,
		"20261030-000000.json":     []byte("{}"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil { //nolint:gosec // the test's own directory
			t.Fatal(err)
		}
	}
	out, errOut, code := runRecordings(t, env, "list")
	if code != 0 || !strings.Contains(errOut, "20261030-000000.json is an archive note whose recording is gone") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	lines := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if id, _, ok := strings.Cut(line, "  "); ok && !strings.HasPrefix(line, " ") {
			lines[id] = line
		}
	}
	for id, want := range map[string]string{
		"20261031-200509": "frames  0.1 MB  (incomplete: still being written or cut off)",
		"20261031-200510": "0 frames  0.0 MB  (damaged at line 1: ",
		"20261031-200511": "0 frames  0.0 MB  (incomplete: still being written or cut off)",
		"20261031-200512": "0 frames  0.0 MB  (incomplete: still being written or cut off)",
		"20261031-200513": "(damaged at line ",
	} {
		if !strings.Contains(lines[id], want) {
			t.Errorf("%s: %q, want %q in it", id, lines[id], want)
		}
	}
	if !strings.Contains(out, "   LAP_RACE  ") {
		t.Errorf("no session read from the half there is:\n%s", out)
	}
}

// `ecu recordings` refuses what it cannot do: no directory, no or a bad
// subcommand, an id that is not one, an outing that is not there, a bad issue.
func TestRecordingsRefusesBadArguments(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"TWR_ECU_RECORDINGS_DIR": dir}
	for _, c := range []struct {
		env  map[string]string
		args []string
		code int
		says string
	}{
		{env, nil, 2, "usage"},
		{env, []string{"prune"}, 2, "usage"},
		{nil, []string{"list"}, 2, "TWR_ECU_RECORDINGS_DIR"},
		{env, []string{"list", "x"}, 2, "no arguments"},
		{env, []string{"archive"}, 2, "archive takes one outing, its id or its recording's path; got 0"},
		{env, []string{"archive", "20261031-200509", "20261031-200510"}, 2, "got 2"},
		{env, []string{"archive", "../etc"}, 2, "not an outing id"},
		{env, []string{"archive", "20261031-200509"}, 1, "no such file"},
		{env, []string{"archive", "--issue", "x", "20261031-200509"}, 2, "not an issue number"},
		{map[string]string{"TWR_ECU_RECORDINGS_DIR": dir, "TWR_ECU_RECORDINGS_IDLE_SECS": "0"}, []string{"archive", "20261031-200509"}, 2,
			"TWR_ECU_RECORDINGS_IDLE_SECS"},
		{nil, []string{"list", "--dir", filepath.Join(dir, "missing")}, 2, "no such file"},
	} {
		_, errOut, code := runRecordings(t, c.env, c.args...)
		if code != c.code || !strings.Contains(errOut, c.says) {
			t.Errorf("%v: exit %d, %q; want %d saying %q", c.args, code, errOut, c.code, c.says)
		}
	}
	if _, _, code := runRecordings(t, env, "-h"); code != 0 {
		t.Errorf("-h: exit %d", code)
	}
}

// An outing written to within the idle setting may still be recording: archiving
// it is refused, or with --force done with a warning to archive it again. A
// recording's path is taken for its id, as v1 takes one.
func TestArchivingAnOutingStillRecording(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20261031-200509.jsonl.gz")
	b, err := os.ReadFile(recordingPath(t, suzuka))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil { //nolint:gosec // the test's own directory
		t.Fatal(err)
	}
	_, errOut, code := runRecordings(t, nil, "archive", path, "--note", "n")
	if code != 1 || !strings.Contains(errOut, "may still be recording") || !strings.Contains(errOut, "--force") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "20261031-200509.json")); err == nil {
		t.Error("archived")
	}
	out, errOut, code := runRecordings(t, nil, "archive", "--force", path, "--note", "n", "--issue", "#354")
	if code != 0 || !strings.Contains(errOut, "archive it again once the outing has ended") ||
		!strings.Contains(out, "archived 20261031-200509  ") || !strings.Contains(out, "#354") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	note, err := record.ReadNote(dir, "20261031-200509")
	if err != nil || note == nil || len(note.Tracks) == 0 || len(note.Kinds) == 0 || len(note.Cars) == 0 {
		t.Errorf("note %+v, %v; want the tracks, cars and kinds the race state saw", note, err)
	}
	if err := os.Chtimes(path, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runRecordings(t, nil, "archive", path); code != 0 || errOut != "" {
		t.Errorf("an hour on: exit %d: %s", code, errOut)
	}
}

// TWR_ECU_RECORDINGS_IDLE_SECS is 600 s unless set, and a value that is not a
// number of seconds above 0 stops ecu at start.
func TestTheIdleSetting(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 600 * time.Second, "10": 10 * time.Second, "0.5": 500 * time.Millisecond} {
		got, err := recordingsIdle(func(string) string { return v })
		if err != nil || got != want {
			t.Errorf("%q: %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"0", "-1", "NaN", "Inf", "soon", "86401"} {
		if _, err := recordingsIdle(func(string) string { return v }); err == nil || !strings.Contains(err.Error(), idleSecsEnv) {
			t.Errorf("%q: %v", v, err)
		}
	}
}

// TWR_ECU_RECORDINGS_KEEP_DAYS is 30 days unless set, 0 keeps every recording,
// and a value that is not a number of days stops ecu at start.
func TestTheKeepSetting(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 30 * 24 * time.Hour, "0": 0, "7": 7 * 24 * time.Hour, "0.5": 12 * time.Hour} {
		got, err := keepDays(func(string) string { return v })
		if err != nil || got != want {
			t.Errorf("%q: %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"-1", "NaN", "Inf", "a week", "40000"} {
		if _, err := keepDays(func(string) string { return v }); err == nil || !strings.Contains(err.Error(), keepDaysEnv) {
			t.Errorf("%q: %v", v, err)
		}
	}
}
