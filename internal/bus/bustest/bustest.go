// Package bustest runs a real nats-server for the tests, with paddock's TELEMETRY
// and EVENTS streams, and reads back what was published, for internal/bus's tests
// and for the tests of the commands that publish through it (ecu replay,
// paddock#35). Nothing of NATS is mocked (paddock#34, R9). Only tests import it;
// depguard lets it import nats.go beside internal/bus.
//
// It runs the nats-server release paddock pins (scripts/nats_it.sh, nats:2.15.1)
// as a process of its own. The release archive is downloaded once into the user's
// cache and checked against these sums, from the release's SHA256SUMS, as
// logbook's tests do. A server built into the tests instead would bring
// nats-server's module graph into go.mod. TWR_TEST_NATS_SERVER names a binary to
// use instead, which must be this release.
package bustest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
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
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsVersion is the nats-server release the tests run.
const natsVersion = "v2.15.1"

var natsSHA256 = map[string]string{
	"darwin-amd64": "3cba5976a4fbd022669d3401cab4147faeb2b4c079712c882edb9ae4e52feb06",
	"darwin-arm64": "ed0fe39222ec2e317c4d0e1a9cdc16c63ec523d94f9355d24a902a4db2655ffd",
	"linux-amd64":  "bf5edc7b1ce98e885c368cb43a886eb148dc5d0551d99f3cf6141fb5e1954c70",
	"linux-arm64":  "e2fbab4cad7d3c76e3052eb959570393c9d73ba756b70bf8e9eea61e9ae6d49c",
}

var (
	binaryOnce sync.Once
	binary     string
	binaryErr  error
)

// Binary is the nats-server the tests run, found or downloaded on the first call.
func Binary() (string, error) {
	binaryOnce.Do(func() { binary, binaryErr = findNATSServer() })
	return binary, binaryErr
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

// Server is a nats-server -js the test started, with its store in a directory of
// the test's, so a server started again on it keeps its streams.
type Server struct {
	t     testing.TB
	port  int
	store string
	cmd   *exec.Cmd
	done  chan struct{}
}

// URL is the server's address for a client.
func (s *Server) URL() string { return "nats://127.0.0.1:" + strconv.Itoa(s.port) }

// NewServer starts nats-server with JetStream on a free port, stopped when the
// test ends.
func NewServer(t testing.TB) *Server {
	t.Helper()
	s := &Server{t: t, port: freePort(t), store: t.TempDir()}
	s.Start()
	t.Cleanup(s.Stop)
	return s
}

// Start starts the server, on its port and store, and waits until it answers.
func (s *Server) Start() {
	s.t.Helper()
	bin, err := Binary()
	if err != nil {
		s.t.Fatalf("nats-server: %v", err)
	}
	cmd := exec.Command(bin, "-a", "127.0.0.1", "-p", strconv.Itoa(s.port), //nolint:gosec // the pinned nats-server
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

// Stop kills the server and waits for it to go.
func (s *Server) Stop() {
	if s.cmd == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	<-s.done
	s.cmd = nil
}

func freePort(t testing.TB) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// streams are paddock's deploy/nats/streams at c0fe666 (paddock#36), and
// callouts.json at 08c6580 (paddock#73), the definitions the bootstrap applies in
// compose and the cluster.
//
//go:embed streams/*.json
var streams embed.FS

// AddStreams creates TELEMETRY, EVENTS and CALLOUTS.
func AddStreams(t testing.TB, js jetstream.JetStream) {
	t.Helper()
	AddStream(t, js, "telemetry")
	AddStream(t, js, "events")
	AddStream(t, js, "callouts")
}

// AddStream creates the stream streams/<name>.json defines.
func AddStream(t testing.TB, js jetstream.JetStream, name string) {
	t.Helper()
	data, err := streams.ReadFile("streams/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg jetstream.StreamConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if _, err := js.CreateStream(context.Background(), cfg); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// buckets are paddock's deploy/nats/kv at 4b9ff66, the buckets the bootstrap
// creates (paddock#36, paddock#88).
//
//go:embed kv/*.json
var buckets embed.FS

// AddKV creates the bucket kv/<name>.json defines, "live" or "ecu", and returns it.
func AddKV(t testing.TB, js jetstream.JetStream, name string) jetstream.KeyValue {
	t.Helper()
	data, err := buckets.ReadFile("kv/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var def struct {
		Bucket        string `json:"bucket"`
		Description   string `json:"description"`
		History       uint8  `json:"history"`
		Storage       string `json:"storage"`
		Replicas      int    `json:"replicas"`
		MaxValueSize  int32  `json:"max_value_size"`
		MaxBucketSize int64  `json:"max_bucket_size"`
	}
	if err := json.Unmarshal(data, &def); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	storage := jetstream.FileStorage
	if def.Storage == "memory" {
		storage = jetstream.MemoryStorage
	}
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket: def.Bucket, Description: def.Description, History: def.History, Storage: storage,
		Replicas: def.Replicas, MaxValueSize: def.MaxValueSize, MaxBytes: def.MaxBucketSize,
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return kv
}

// JetStream is a connection to the server and its JetStream context, closed when
// the test ends.
func (s *Server) JetStream() jetstream.JetStream {
	s.t.Helper()
	nc, js := s.connect()
	s.t.Cleanup(nc.Close)
	return js
}

// WithStreams starts a server with the streams named, "telemetry", "events" and
// "callouts", or all three when none is named, for a test that publishes through internal/bus and
// reads back with Read and Count.
func WithStreams(t testing.TB, names ...string) *Server {
	t.Helper()
	s := NewServer(t)
	nc, js := s.connect()
	defer nc.Close()
	if len(names) == 0 {
		names = []string{"telemetry", "events", "callouts"}
	}
	for _, name := range names {
		AddStream(t, js, name)
	}
	return s
}

func (s *Server) connect() (*nats.Conn, jetstream.JetStream) {
	s.t.Helper()
	nc, err := nats.Connect(s.URL())
	if err != nil {
		s.t.Fatal(err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		s.t.Fatal(err)
	}
	return nc, js
}

// Msg is a message a stream holds.
type Msg struct {
	Subject string
	Header  map[string][]string
	Data    []byte
}

// Count is how many messages the stream holds.
func (s *Server) Count(stream string) uint64 {
	s.t.Helper()
	nc, js := s.connect()
	defer nc.Close()
	st, err := js.Stream(context.Background(), stream)
	if err != nil {
		s.t.Fatal(err)
	}
	info, err := st.Info(context.Background())
	if err != nil {
		s.t.Fatal(err)
	}
	return info.State.Msgs
}

// Settled is how many messages the stream holds once it has taken what was sent:
// it waits, 10 s at most, until the count reaches want, or, with want 0, until it
// has not changed for 250 ms. Frames go out with a plain publish, which a
// publisher's Close only flushes to the server, so the stream may store the last
// ones just after a replay returns.
func (s *Server) Settled(stream string, want uint64) uint64 {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	n, since := s.Count(stream), time.Now()
	for time.Now().Before(deadline) {
		if want > 0 && n >= want || want == 0 && time.Since(since) >= 250*time.Millisecond {
			break
		}
		time.Sleep(10 * time.Millisecond)
		if m := s.Count(stream); m != n {
			n, since = m, time.Now()
		}
	}
	return n
}

// Read is the first n messages the stream holds, in order, or every one when n is
// 0.
func (s *Server) Read(stream string, n uint64) []Msg {
	s.t.Helper()
	nc, js := s.connect()
	defer nc.Close()
	st, err := js.Stream(context.Background(), stream)
	if err != nil {
		s.t.Fatal(err)
	}
	info, err := st.Info(context.Background())
	if err != nil {
		s.t.Fatal(err)
	}
	if n == 0 || n > info.State.Msgs {
		n = info.State.Msgs
	}
	all := make([]Msg, 0, n)
	for seq := info.State.FirstSeq; uint64(len(all)) < n && seq <= info.State.LastSeq; seq++ {
		m, err := st.GetMsg(context.Background(), seq)
		if err != nil {
			s.t.Fatalf("%s %d: %v", stream, seq, err)
		}
		all = append(all, Msg{Subject: m.Subject, Header: m.Header, Data: m.Data})
	}
	return all
}

// Stalled is a TCP listener that plays a NATS server gone wrong: each connection
// it accepts is held open, and either never answered (silent), or answered up to
// the client's first PING and then never read again, so the client's writes fill
// the socket and wait.
func Stalled(t testing.TB, silent bool) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			if silent {
				continue
			}
			_ = c.(*net.TCPConn).SetReadBuffer(4096)
			go handshake(c)
		}
	}()
	return "nats://" + ln.Addr().String()
}

// handshake answers a client's CONNECT and first PING as a server would, then reads
// nothing more.
func handshake(c net.Conn) {
	_, _ = c.Write([]byte(`INFO {"server_id":"stalled","version":"2.15.1","proto":1,"headers":true,` +
		`"max_payload":1048576}` + "\r\n"))
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if strings.HasPrefix(line, "PING") {
			_, _ = c.Write([]byte("PONG\r\n"))
			return
		}
	}
}
