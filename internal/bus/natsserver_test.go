package bus

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The integration tests run nats-server itself, the release paddock pins
// (scripts/nats_it.sh, nats:2.15.1), as a process of its own: nothing of NATS is
// mocked (R9). The release archive is downloaded once into the user's cache and
// checked against these sums, from the release's SHA256SUMS, as logbook's tests
// do. A server built into the tests instead would bring nats-server's module graph
// into go.mod. TWR_TEST_NATS_SERVER names a binary to use instead, which must be
// this release.
const natsVersion = "v2.15.1"

var natsSHA256 = map[string]string{
	"darwin-amd64": "3cba5976a4fbd022669d3401cab4147faeb2b4c079712c882edb9ae4e52feb06",
	"darwin-arm64": "ed0fe39222ec2e317c4d0e1a9cdc16c63ec523d94f9355d24a902a4db2655ffd",
	"linux-amd64":  "bf5edc7b1ce98e885c368cb43a886eb148dc5d0551d99f3cf6141fb5e1954c70",
	"linux-arm64":  "e2fbab4cad7d3c76e3052eb959570393c9d73ba756b70bf8e9eea61e9ae6d49c",
}

// natsBinary is the nats-server the tests run, set by TestMain.
var natsBinary string

func TestMain(m *testing.M) {
	bin, err := findNATSServer()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "nats-server:", err)
		os.Exit(1)
	}
	natsBinary = bin
	os.Exit(m.Run())
}

func findNATSServer() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if bin := os.Getenv("TWR_TEST_NATS_SERVER"); bin != "" {
		out, err := exec.CommandContext(ctx, bin, "--version").Output() //nolint:gosec // the binary the person running the tests named
		if err != nil {
			return "", fmt.Errorf("TWR_TEST_NATS_SERVER=%s: %w", bin, err)
		}
		if !strings.Contains(string(out), natsVersion) {
			return "", fmt.Errorf("TWR_TEST_NATS_SERVER=%s is %q, not %s", bin, strings.TrimSpace(string(out)), natsVersion)
		}
		return bin, nil
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	sum, ok := natsSHA256[platform]
	if !ok {
		return "", fmt.Errorf("no %s release for %s pinned here; set TWR_TEST_NATS_SERVER", natsVersion, platform)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	name := "nats-server-" + natsVersion + "-" + platform
	dir := filepath.Join(cache, "those-who-run", name)
	bin := filepath.Join(dir, "nats-server")
	archivePath := filepath.Join(dir, "nats-server.tar.gz")
	// The archive is kept beside the binary and checked on every use, and the
	// binary taken from it again: a cache someone else can write to is not trusted
	// as it stands.
	if archive, err := os.ReadFile(archivePath); err == nil { //nolint:gosec // the tests' own cache
		if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) == sum {
			return bin, extract(archive, name+"/nats-server", bin)
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	url := "https://github.com/nats-io/nats-server/releases/download/" + natsVersion + "/" + name + ".tar.gz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != sum {
		return "", fmt.Errorf("%s has sha256 %x, want %s", url, got, sum)
	}
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		return "", err
	}
	return bin, extract(archive, name+"/nats-server", bin)
}

// extract writes the archive's file member to path, through a temporary file
// renamed into place, so a run cut short leaves no half binary for the next.
func extract(archive []byte, member, path string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s is not in the archive", member)
		}
		if err != nil {
			return err
		}
		if h.Name != member {
			continue
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".nats-server-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(tmp.Name()) }()
		if _, err := io.Copy(tmp, io.LimitReader(tr, 128<<20)); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := errors.Join(tmp.Chmod(0o755), tmp.Close()); err != nil { //nolint:gosec // an executable
			return err
		}
		return os.Rename(tmp.Name(), path)
	}
}

// natsServer is a nats-server -js the test started, with its store in a directory
// of the test's, so a server started again on it keeps its streams.
type natsServer struct {
	t     *testing.T
	port  int
	store string
	cmd   *exec.Cmd
	done  chan struct{}
}

// URL is the server's address for a client.
func (s *natsServer) URL() string { return "nats://127.0.0.1:" + strconv.Itoa(s.port) }

// startNATS starts nats-server with JetStream on a free port, stopped when the test
// ends.
func startNATS(t *testing.T) *natsServer {
	t.Helper()
	s := &natsServer{t: t, port: freePort(t), store: t.TempDir()}
	s.start()
	t.Cleanup(s.stop)
	return s
}

// start starts the server, on its port and store, and waits until it answers.
func (s *natsServer) start() {
	s.t.Helper()
	cmd := exec.Command(natsBinary, "-a", "127.0.0.1", "-p", strconv.Itoa(s.port), //nolint:gosec // the pinned nats-server
		"-js", "-sd", s.store)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.cmd, s.done = cmd, make(chan struct{})
	done := s.done
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		nc, err := nats.Connect(s.URL())
		if err == nil {
			nc.Close()
			return
		}
		select {
		case <-done:
			s.t.Fatalf("nats-server stopped: %s", out.String())
		default:
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("nats-server not ready after 10 s: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop kills the server and waits for it to go.
func (s *natsServer) stop() {
	if s.cmd == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	<-s.done
	s.cmd = nil
}

func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// addStreams creates TELEMETRY and EVENTS from testdata/streams, copies of
// paddock's deploy/nats/streams at c0fe666 (paddock#36), the definitions the
// bootstrap applies in compose and the cluster.
func addStreams(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	addStream(t, js, "telemetry")
	addStream(t, js, "events")
}

// addStream creates the stream testdata/streams/<name>.json defines.
func addStream(t *testing.T, js jetstream.JetStream, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "streams", name+".json")) //nolint:gosec // a fixture
	if err != nil {
		t.Fatal(err)
	}
	var cfg jetstream.StreamConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if _, err := js.CreateStream(t.Context(), cfg); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}
