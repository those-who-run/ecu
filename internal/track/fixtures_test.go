package track

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/salsa"
)

// The v1 recordings the tests replay: the 18 in testdata/v1/, copied from v1 with
// their provenance (testdata/v1/README.md), and the seven salsa carries in its
// testdata/v1/, read from the salsa module the build already uses, as
// internal/race reads them. Together, all 25 of v1's FIXTURES.yaml.
var (
	salsaOnce sync.Once
	salsaDir  string
	salsaErr  error
)

func salsaFixtures(tb testing.TB) string {
	tb.Helper()
	salsaOnce.Do(func() {
		// go test puts its own toolchain's go first on PATH.
		out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}",
			"github.com/those-who-run/salsa").Output()
		if err != nil {
			salsaErr = err
			return
		}
		salsaDir = filepath.Join(strings.TrimSpace(string(out)), "testdata", "v1")
	})
	if salsaErr != nil {
		tb.Fatalf("finding salsa's testdata/v1: %v", salsaErr)
	}
	return salsaDir
}

// recordingPath is where the recording is, here or in salsa.
func recordingPath(tb testing.TB, name string) string {
	tb.Helper()
	path := filepath.Join("testdata", "v1", name+".jsonl.gz")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return filepath.Join(salsaFixtures(tb), name+".jsonl.gz")
}

// framesCache keeps each recording's frames once read: the tests replay most of
// them more than once, and a detector never changes a frame.
var framesCache sync.Map

// frames is every frame of a recording as ecu makes it: the contracts frame
// internal/race gives for each packet.
func frames(tb testing.TB, name string) []*telemetryv1.Frame {
	tb.Helper()
	if all, ok := framesCache.Load(name); ok {
		return all.([]*telemetryv1.Frame)
	}
	all := readFrames(tb, name)
	framesCache.Store(name, all)
	return all
}

func readFrames(tb testing.TB, name string) []*telemetryv1.Frame {
	tb.Helper()
	src, err := salsa.OpenRecording(recordingPath(tb, name))
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'})))
	var all []*telemetryv1.Frame
	for {
		r, err := src.Next(tb.Context())
		if errors.Is(err, io.EOF) {
			return all
		}
		if err != nil {
			tb.Fatal(err)
		}
		all = append(all, m.Step(race.Received{Received: r, Time: epoch})[0].Frame)
	}
}

// fixtureEntry is one entry of testdata/v1/FIXTURES.yaml.
type fixtureEntry map[string]string

// fixtureKeys are the keys every entry has, and no others.
var fixtureKeys = []string{"entry", "file", "frames", "sha256", "v1"}

// v1EntryLink is a link to an entry of v1's fixture index at v1.0.0, by its lines.
var v1EntryLink = regexp.MustCompile(`^https://github\.com/those-who-run/those-who-run/blob/v1\.0\.0/tests/fixtures/FIXTURES\.yaml#L\d+-L\d+$`)

// readFixtureIndex reads FIXTURES.yaml: the subset of YAML it says it is, a list of
// `key: value` entries under `fixtures:`, as salsa's fixtures_test.go reads its own.
// Anything else is an error, so the index can't drift into YAML this reader would
// misread.
func readFixtureIndex(b []byte) ([]fixtureEntry, error) {
	var entries []fixtureEntry
	inList := false
	for n, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		where := fmt.Sprintf("FIXTURES.yaml:%d", n+1)
		switch trimmed := strings.TrimSpace(line); {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			continue
		case line == "fixtures:" && !inList:
			inList = true
			continue
		case !inList:
			return nil, fmt.Errorf("%s: %q before `fixtures:`", where, line)
		}
		var item string
		switch {
		case strings.HasPrefix(line, "  - "):
			entries = append(entries, fixtureEntry{})
			item = strings.TrimPrefix(line, "  - ")
		case strings.HasPrefix(line, "    ") && len(entries) > 0:
			item = strings.TrimPrefix(line, "    ")
		default:
			return nil, fmt.Errorf("%s: %q is not an entry's line", where, line)
		}
		key, value, ok := strings.Cut(item, ": ")
		if !ok || strings.ContainsAny(key, " :") || value == "" {
			return nil, fmt.Errorf("%s: %q is not `key: value`", where, line)
		}
		entry := entries[len(entries)-1]
		if _, dup := entry[key]; dup {
			return nil, fmt.Errorf("%s: %s twice in one entry", where, key)
		}
		entry[key] = value
	}
	return entries, nil
}

// checkFixtures returns every way the files in dir and dir/FIXTURES.yaml disagree.
func checkFixtures(dir string) []error {
	b, err := os.ReadFile(filepath.Join(dir, "FIXTURES.yaml")) //nolint:gosec // a directory under testdata
	if err != nil {
		return []error{err}
	}
	entries, err := readFixtureIndex(b)
	if err != nil {
		return []error{err}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl.gz"))
	if err != nil {
		return []error{err}
	}
	var errs []error
	listed := map[string]bool{}
	for _, e := range entries {
		var keys []string
		for k := range e {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, fixtureKeys) {
			errs = append(errs, fmt.Errorf("%s: keys %v, want %v", e["file"], keys, fixtureKeys))
			continue
		}
		name := e["file"]
		if listed[name] {
			errs = append(errs, fmt.Errorf("%s: listed twice", name))
		}
		listed[name] = true
		if e["v1"] != "tests/fixtures/"+name {
			errs = append(errs, fmt.Errorf("%s: v1 path %s, want the same name under tests/fixtures/", name, e["v1"]))
		}
		if !v1EntryLink.MatchString(e["entry"]) {
			errs = append(errs, fmt.Errorf("%s: entry %s is not a link to v1's FIXTURES.yaml at v1.0.0", name, e["entry"]))
		}
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a file the index names, under testdata
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: listed, but %w", name, err))
			continue
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != e["sha256"] {
			errs = append(errs, fmt.Errorf("%s: sha256 %x, FIXTURES.yaml says %s", name, sum, e["sha256"]))
		}
		lines, err := countLines(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		} else if strconv.Itoa(lines) != e["frames"] {
			errs = append(errs, fmt.Errorf("%s: %d lines, FIXTURES.yaml says %s frames", name, lines, e["frames"]))
		}
	}
	for _, path := range files {
		if !listed[filepath.Base(path)] {
			errs = append(errs, fmt.Errorf("%s: not in FIXTURES.yaml", path))
		}
	}
	return errs
}

// countLines counts the lines of a gzipped file.
func countLines(gz []byte) (int, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return 0, err
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		return 0, err
	}
	return bytes.Count(b, []byte("\n")), nil
}

// The fixtures are the files copied from v1 (testdata/v1/README.md), unchanged, each
// listed in FIXTURES.yaml with its provenance; with salsa's seven they are v1's 25.
func TestV1FixtureSums(t *testing.T) {
	dir := filepath.Join("testdata", "v1")
	for _, err := range checkFixtures(dir) {
		t.Error(err)
	}
	for _, name := range v1Recordings {
		if _, err := os.Stat(recordingPath(t, name)); err != nil {
			t.Error(err)
		}
	}
}

// Changing one byte of a copied fixture fails the check, and so do a file with no
// entry and an entry with no file.
func TestV1FixtureSumsFail(t *testing.T) {
	copyFixtures := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		paths, err := filepath.Glob(filepath.Join("testdata", "v1", "*"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			b, err := os.ReadFile(path) //nolint:gosec // under testdata
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, filepath.Base(path)), b, 0o600); err != nil { //nolint:gosec // a temporary directory
				t.Fatal(err)
			}
		}
		return dir
	}
	t.Run("a byte changed", func(t *testing.T) {
		dir := copyFixtures(t)
		path := filepath.Join(dir, "real-nuc-suzuka-east-2026-10-15.jsonl.gz")
		b, err := os.ReadFile(path) //nolint:gosec // a temporary copy
		if err != nil {
			t.Fatal(err)
		}
		b[len(b)/2] ^= 0x01
		if err := os.WriteFile(path, b, 0o600); err != nil { //nolint:gosec // a temporary copy
			t.Fatal(err)
		}
		if errs := checkFixtures(dir); !strings.Contains(fmt.Sprint(errs), "sha256") {
			t.Errorf("one byte changed: %v, want a sha256 mismatch", errs)
		}
	})
	t.Run("a file not listed", func(t *testing.T) {
		dir := copyFixtures(t)
		if err := os.WriteFile(filepath.Join(dir, "extra.jsonl.gz"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if errs := checkFixtures(dir); len(errs) != 1 || !strings.Contains(errs[0].Error(), "not in FIXTURES.yaml") {
			t.Errorf("an unlisted file: %v", errs)
		}
	})
	t.Run("an entry with no file", func(t *testing.T) {
		dir := copyFixtures(t)
		if err := os.Remove(filepath.Join(dir, "real-nuc-suzuka-east-2026-10-15.jsonl.gz")); err != nil {
			t.Fatal(err)
		}
		if errs := checkFixtures(dir); len(errs) != 1 || !strings.Contains(errs[0].Error(), "listed, but") {
			t.Errorf("a missing file: %v", errs)
		}
	})
	t.Run("not the format", func(t *testing.T) {
		for _, bad := range []string{"other:\n", "fixtures:\n  file: x\n", "fixtures:\n  - file x\n", "fixtures:\n  - file: x\n    file: y\n"} {
			if _, err := readFixtureIndex([]byte(bad)); err == nil {
				t.Errorf("%q: read, want an error", bad)
			}
		}
	})
}
