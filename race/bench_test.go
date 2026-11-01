package race

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// BenchmarkReplay replays every recording in salsa's testdata/v1/ through a new
// machine each time, and reports the time a Step takes: over every frame, and over
// the frames that start a session, close a lap or end a session (paddock#33, R12).
// Each is the p50, p99 and max in ns. Run it with
//
//	go test -run '^$' -bench Replay ./race
func BenchmarkReplay(b *testing.B) {
	names, err := filepath.Glob(filepath.Join(fixtures(b), "*.jsonl.gz"))
	if err != nil || len(names) == 0 {
		b.Fatalf("no recordings: %v", err)
	}
	var recordings [][]Received
	for _, name := range names {
		recordings = append(recordings, packets(b, strings.TrimSuffix(filepath.Base(name), ".jsonl.gz")))
	}
	var frame, start, lap, end []time.Duration
	for b.Loop() {
		for _, all := range recordings {
			m := New(testIDs())
			for _, r := range all {
				t0 := time.Now()
				out := m.Step(r)
				took := time.Since(t0)
				frame = append(frame, took)
				for _, o := range out[1:] {
					switch {
					case o.Started != nil:
						start = append(start, took)
					case o.Lap != nil:
						lap = append(lap, took)
					case o.Ended != nil:
						end = append(end, took)
					}
				}
			}
			m.EndOfInput()
		}
	}
	b.StopTimer()
	for _, d := range []struct {
		name string
		all  []time.Duration
	}{{"frame", frame}, {"start", start}, {"lap", lap}, {"end", end}} {
		slices.Sort(d.all)
		n := len(d.all)
		b.ReportMetric(float64(d.all[n/2]), "p50-ns/"+d.name)
		b.ReportMetric(float64(d.all[n*99/100]), "p99-ns/"+d.name)
		b.ReportMetric(float64(d.all[n-1]), "max-ns/"+d.name)
	}
}
