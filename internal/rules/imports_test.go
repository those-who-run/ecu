package rules

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// denied are the imports no file of the engine or a rule may have (R9, AC6):
// NATS, the network, the file system and system calls, as for race.
// depguard in .golangci.yml denies the same list at lint time. rulestest, which
// only tests import, reads the recordings and is left out.
var denied = []string{"github.com/nats-io", "net", "os", "io/fs", "io/ioutil", "path/filepath", "syscall"}

func deniedImport(path string) bool {
	for _, d := range denied {
		if path == d || strings.HasPrefix(path, d+"/") {
			return true
		}
	}
	return false
}

// AC6 and R9: the engine and every rule are pure: no NATS, no network, no files,
// and no clock (time.Now), so their tests run with no NATS server and a replay
// gives the same callouts every time.
func TestTheRulesDoNoIO(t *testing.T) {
	var checked int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "rulestest" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		checked++
		src, err := os.ReadFile(path) //nolint:gosec // a file of this package
		if err != nil {
			return err
		}
		for _, problem := range impure(t, path, src) {
			t.Errorf("%s: %s", path, problem)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 4 {
		t.Fatalf("only %d source files found", checked)
	}
}

func impure(t *testing.T, name string, src []byte) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if deniedImport(path) {
			found = append(found, "imports "+path)
		}
	}
	if strings.Contains(string(src), "time.Now") {
		found = append(found, "reads the clock")
	}
	return found
}

func TestTheIOCheckFindsIt(t *testing.T) {
	src := "package x\n\nimport (\n\t\"os\"\n\t\"github.com/nats-io/nats.go\"\n\t\"oslo\"\n)\n\nvar _ = time.Now\n"
	got := strings.Join(impure(t, "planted.go", []byte(src)), "; ")
	if got != "imports os; imports github.com/nats-io/nats.go; reads the clock" {
		t.Errorf("got %q", got)
	}
}
