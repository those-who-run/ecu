package bus

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/those-who-run/contracts/ce"
)

// entry is a subject of contracts' subjects.yaml: its pattern and ce-type.
type entry struct{ pattern, ceType string }

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

// uncatalogued are subjects ecu publishes that contracts has no entry for yet, and
// the issue that adds it. The 20 Hz frame is R2 of paddock#34, but its contract,
// what the dash reads, is paddock#137. When the entry comes, this test fails until
// it leaves this list.
var uncatalogued = map[string]string{
	"twr.v1.<src>.frame.gt7.lo": "paddock#137",
}

// AC6: every subject published in a replay (AC1's) is in subjects.yaml with the
// ce-type it was sent with (R8).
func TestEverySubjectIsInTheCatalogue(t *testing.T) {
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

	sent := map[[2]string]bool{} // subject with <sid> for the session id, ce-type
	for n := stats.Frames + stats.LoFrames + stats.Events; n > 0; n-- {
		msg, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("%d messages still to come: %v", n, err)
		}
		sent[[2]string{generic(msg), msg.Header.Get(ce.TypeHeader)}] = true
	}
	if len(sent) != 5 {
		t.Errorf("%d kinds of message sent, want 5: frames, .lo and three events: %v", len(sent), sent)
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
		_, known := uncatalogued[strings.Replace(subject, "parity", "<src>", 1)]
		switch {
		case !found && !known:
			t.Errorf("%s (%s) is not in subjects.yaml", subject, ceType)
		case found && known:
			t.Errorf("%s is in subjects.yaml now: take it out of uncatalogued", subject)
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
