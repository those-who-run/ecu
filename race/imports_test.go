package race

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// denied are the imports the race package may not have (paddock#33, R11): NATS, the
// network and the file system. depguard in .golangci.yml denies the same list at
// lint time; this test holds it without golangci-lint. A path is denied when it is
// one of these or below one.
var denied = []string{
	"github.com/nats-io",
	"net",
	"os",
	"io/fs",
	"io/ioutil",
	"path/filepath",
	"syscall",
}

func deniedImport(path string) bool {
	for _, d := range denied {
		if path == d || strings.HasPrefix(path, d+"/") {
			return true
		}
	}
	return false
}

// importsOf is the denied imports of a Go source file.
func importsOf(t *testing.T, name string, src any) []string {
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
			found = append(found, path)
		}
	}
	return found
}

// AC6, without the linter: no file of the package imports NATS, the network or the
// file system. The tests may: they read the recordings.
func TestThePackageImportsNoNATSNetworkOrFiles(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		if found := importsOf(t, name, nil); found != nil {
			t.Errorf("%s imports %v", name, found)
		}
	}
	if checked == 0 {
		t.Fatal("no source files found")
	}
}

func TestTheImportCheckFindsNATS(t *testing.T) {
	src := `package race

import (
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"net/http"
	"os"
	"oslo"
)
`
	got := importsOf(t, "planted.go", src)
	want := []string{"github.com/nats-io/nats.go", "github.com/nats-io/nats.go/jetstream", "net/http", "os"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, want %v", got, want)
	}
}
