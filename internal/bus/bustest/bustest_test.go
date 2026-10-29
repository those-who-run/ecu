package bustest

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// A server with the streams holds what is published to them, keeps it when
// started again on its store, and reads it back in order.
func TestAServerWithTheStreams(t *testing.T) {
	s := WithStreams(t)
	nc, err := nats.Connect(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		m := &nats.Msg{Subject: "twr.v1.parity.frame.gt7", Header: nats.Header{"Nats-Msg-Id": {id}}, Data: []byte(id)}
		if err := nc.PublishMsg(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	nc.Close()
	// The stream stores a core publish after the server has taken it: wait for
	// it before killing the server.
	for deadline := time.Now().Add(10 * time.Second); s.Count("TELEMETRY") != 3; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("TELEMETRY holds %d, want 3", s.Count("TELEMETRY"))
		}
	}
	s.Stop()
	s.Start()
	if n := s.Count("TELEMETRY"); n != 3 {
		t.Fatalf("TELEMETRY holds %d after a restart, want 3", n)
	}
	var got []string
	for _, m := range s.Read("TELEMETRY", 2) {
		got = append(got, m.Subject+" "+string(m.Data)+" "+m.Header["Nats-Msg-Id"][0])
	}
	if strings.Join(got, ",") != "twr.v1.parity.frame.gt7 a a,twr.v1.parity.frame.gt7 b b" {
		t.Errorf("read %v", got)
	}
	if n := len(s.Read("EVENTS", 0)); n != 0 {
		t.Errorf("EVENTS holds %d, want none", n)
	}
}

// TWR_TEST_NATS_SERVER must name the pinned release.
func TestFindNATSServerChecksTheVersion(t *testing.T) {
	t.Setenv("TWR_TEST_NATS_SERVER", "/bin/echo")
	if _, err := findNATSServer(); err == nil || !strings.Contains(err.Error(), "not "+natsVersion) {
		t.Errorf("err %v, want the version refused", err)
	}
	t.Setenv("TWR_TEST_NATS_SERVER", "/nonexistent/nats-server")
	if _, err := findNATSServer(); err == nil {
		t.Error("a missing binary was accepted")
	}
}
