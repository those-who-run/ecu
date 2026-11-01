package record

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Ext is a recording's extension: <id>.jsonl.gz.
const Ext = ".jsonl.gz"

// NoteExt is an archived outing's note, <id>.json beside its recording: v1's
// sidecar (ADR 0002), so an archived outing and its note can go to v1's outing
// archive as they are.
const NoteExt = ".json"

// stampLayout is an outing id's time, UTC, as v1 names its outings.
const stampLayout = "20060102-150405"

// outingID is an outing's id: its start, and -N for the Nth outing of that
// second.
var outingID = regexp.MustCompile(`^(\d{8}-\d{6})(?:-([2-9]|[1-9]\d+))?$`)

// ParseID is the start of the outing id names, and the n of its -N suffix, 1
// without one; ok is false when id is not an outing id.
func ParseID(id string) (start time.Time, n int, ok bool) {
	m := outingID.FindStringSubmatch(id)
	if m == nil {
		return time.Time{}, 0, false
	}
	start, err := time.Parse(stampLayout, m[1])
	if err != nil {
		return time.Time{}, 0, false
	}
	n = 1
	if m[2] != "" {
		n, _ = strconv.Atoi(m[2]) // the pattern's digits
	}
	return start, n, true
}

// idOf is the outing id a file of an outing is named for: its name up to the first
// dot, when that is an outing id.
func idOf(name string) (string, bool) {
	id, _, _ := strings.Cut(name, ".")
	_, _, ok := ParseID(id)
	return id, ok
}

// Recording is one outing's recording in the directory.
type Recording struct {
	ID    string
	Path  string
	Start time.Time
	// Size is the recording's size, and Total every file's of the outing,
	// <id>.*: its capture (paddock#343) and its note too.
	Size, Total int64
	// Note is the outing's archive note, nil when it is not archived.
	Note *Note
}

// Recordings are the outings recorded in dir, oldest first: by their start, then
// their -N, as v1's outing_order sorts them. Files not named as ecu names its
// recordings are left out.
func Recordings(dir string) ([]Recording, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("recordings: %w", err)
	}
	var all []Recording
	order := map[string]int{}
	total := map[string]int64{}
	for _, e := range entries {
		if id, ok := idOf(e.Name()); ok && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				total[id] += info.Size()
			}
		}
	}
	for _, e := range entries {
		id, found := strings.CutSuffix(e.Name(), Ext)
		if !found || !e.Type().IsRegular() {
			continue
		}
		start, n, ok := ParseID(id)
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // pruned meanwhile
		}
		note, err := ReadNote(dir, id)
		if err != nil {
			return nil, err
		}
		all = append(all, Recording{ID: id, Path: filepath.Join(dir, e.Name()), Start: start, Size: info.Size(),
			Total: max(total[id], info.Size()), Note: note})
		order[id] = n
	}
	slices.SortFunc(all, func(a, b Recording) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return order[a.ID] - order[b.ID]
	})
	return all, nil
}

// Note is an archived outing's note, <id>.json: v1's sidecar (ADR 0002,
// archive.py's ArchivedOuting), every key of it, so v1's archive loads it. Its
// tracks, cars, kinds and sessions are what ecu's race state saw in the
// recording; its sessions are ecu's lines for them, not v1's.
type Note struct {
	ID         string   `json:"id"`
	SHA256     string   `json:"sha256"`
	Bytes      int64    `json:"bytes"`
	Frames     int      `json:"frames"`
	Date       string   `json:"date"`
	Tracks     []string `json:"tracks"`
	Cars       []string `json:"cars"`
	Kinds      []string `json:"kinds"`
	Sessions   []string `json:"sessions"`
	Note       string   `json:"note"`
	Issues     []int    `json:"issues"`
	ArchivedAt string   `json:"archived_at"`
}

// ReadNote is the outing id's archive note, nil when it has none.
func ReadNote(dir, id string) (*Note, error) {
	b, err := os.ReadFile(filepath.Join(dir, id+NoteExt)) //nolint:gosec // the operator's directory, an outing id
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // not archived
	}
	if err != nil {
		return nil, fmt.Errorf("recordings: %w", err)
	}
	var n Note
	if err := json.Unmarshal(b, &n); err != nil {
		return nil, fmt.Errorf("recordings: %s%s: %w", id, NoteExt, err)
	}
	return &n, nil
}

// Archive marks the outing id as kept, as v1's `recordings archive` does: its
// note, <id>.json, takes the recording's sha256, size and date, and n's frames,
// sessions, note and issues. Archived again, the outing keeps its note unless n
// has one, adds n's issues to its own and keeps when it was first archived. An
// archived outing is never pruned. The note is written whole or not at all.
func Archive(dir, id string, n Note, now time.Time) (Note, error) {
	start, _, ok := ParseID(id)
	if !ok {
		return Note{}, fmt.Errorf("%q is not an outing id (YYYYMMDD-HHMMSS)", id)
	}
	path := filepath.Join(dir, id+Ext)
	sum, size, err := digest(path)
	if err != nil {
		return Note{}, err
	}
	prev, err := ReadNote(dir, id)
	if err != nil {
		return Note{}, err
	}
	n.ID, n.SHA256, n.Bytes, n.Date = id, sum, size, start.Format(time.DateOnly)
	n.ArchivedAt = now.UTC().Format(time.RFC3339)
	if prev != nil {
		if n.Note == "" {
			n.Note = prev.Note
		}
		n.Issues = append(n.Issues, prev.Issues...)
		n.ArchivedAt = prev.ArchivedAt
	}
	slices.Sort(n.Issues)
	n.Issues = slices.Compact(n.Issues)
	if n.Issues == nil {
		n.Issues = []int{}
	}
	for _, list := range []*[]string{&n.Tracks, &n.Cars, &n.Kinds, &n.Sessions} {
		if *list == nil {
			*list = []string{}
		}
	}
	b, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return Note{}, err
	}
	return n, writeWhole(filepath.Join(dir, id+NoteExt), append(b, '\n'))
}

// digest is the file's sha256 and size.
func digest(path string) (string, int64, error) {
	f, err := os.Open(path) //nolint:gosec // the operator's directory, an outing id
	if err != nil {
		return "", 0, fmt.Errorf("recordings: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("recordings: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// writeWhole writes path through a file beside it, renamed over it once synced.
// The file's name starts with a dot, so nothing takes it for an outing's.
func writeWhole(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".ecu-note-*")
	if err != nil {
		return fmt.Errorf("recordings: %w", err)
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o640) //nolint:gosec // as the recordings: the group may read them
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("recordings: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("recordings: %w", err)
	}
	return nil
}

// syncDir syncs the directory, so a file made or renamed in it is there after a
// crash.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // the operator's directory
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// OrphanNotes are the archive notes in dir whose recording is gone.
func OrphanNotes(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("recordings: %w", err)
	}
	var orphans []string
	for _, e := range entries {
		id, found := strings.CutSuffix(e.Name(), NoteExt)
		if _, _, ok := ParseID(id); !found || !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, id+Ext)); errors.Is(err, fs.ErrNotExist) {
			orphans = append(orphans, e.Name())
		}
	}
	return orphans, nil
}

// Prune removes the outings in dir last written more than keep before now that are not
// archived (no <id>.json), every file of each: its recording and whatever is
// recorded beside it under its id. An outing's last write is the latest of its
// start and its files' modification times, so a long recording, such as one of
// the files ecu wrote one per run, is dated by its end. It returns the ids
// pruned. Files not named <id>.* for an outing id are never touched.
func Prune(dir string, keep time.Duration, now time.Time, logger *slog.Logger) ([]string, error) {
	return prune(dir, keep, now, logger, "pruned an old recording", func(string) bool { return true })
}

// PruneCaptures removes the captures, <id>.twrcap, of the outings in dir last
// written more than keep before now that are not archived, as Prune dates them,
// and leaves the rest of each outing (paddock#343): a capture is about six times
// its recording's size, so it is kept for less time. It returns the ids whose
// capture it removed.
func PruneCaptures(dir string, keep time.Duration, now time.Time, logger *slog.Logger) ([]string, error) {
	return prune(dir, keep, now, logger, "pruned an old capture", func(name string) bool {
		return strings.HasSuffix(name, CaptureExt)
	})
}

// prune removes, of each outing Prune would prune, the files take takes, logging
// each with msg.
func prune(dir string, keep time.Duration, now time.Time, logger *slog.Logger, msg string, take func(name string) bool) ([]string, error) {
	before := now.Add(-keep)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("recordings: %w", err)
	}
	files := map[string][]string{}
	last := map[string]time.Time{}
	var ids []string
	for _, e := range entries {
		id, ok := idOf(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if _, seen := files[id]; !seen {
			ids = append(ids, id)
			last[id], _, _ = ParseID(id)
		}
		files[id] = append(files[id], e.Name())
		if info, err := e.Info(); err == nil && info.ModTime().After(last[id]) {
			last[id] = info.ModTime()
		}
	}
	var pruned []string
	var errs []error
	for _, id := range ids {
		if !last[id].Before(before) || slices.Contains(files[id], id+NoteExt) {
			continue
		}
		age := now.Sub(last[id])
		removed := false
		for _, name := range files[id] {
			if !take(name) {
				continue
			}
			// Archived meanwhile: kept.
			if _, err := os.Stat(filepath.Join(dir, id+NoteExt)); err == nil {
				break
			}
			removed = true
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
				continue
			}
			logger.Info(msg, "id", id, "file", name, "last_written", last[id].UTC().Format(time.RFC3339),
				"age", age.Round(time.Second).String())
		}
		if removed {
			pruned = append(pruned, id)
		}
	}
	return pruned, errors.Join(errs...)
}
