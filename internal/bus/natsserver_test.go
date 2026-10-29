package bus

import (
	"fmt"
	"os"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/those-who-run/ecu/internal/bus/bustest"
)

// The integration tests run nats-server itself, through bustest: nothing of NATS
// is mocked (R9).

// TestMain finds the nats-server, downloading it once, before any test runs, so a
// missing one fails the run at once rather than test by test.
func TestMain(m *testing.M) {
	if _, err := bustest.Binary(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "nats-server:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// startNATS starts nats-server with JetStream on a free port, stopped when the test
// ends.
func startNATS(t *testing.T) *bustest.Server { return bustest.NewServer(t) }

// addStreams creates TELEMETRY and EVENTS, the definitions the bootstrap applies
// in compose and the cluster.
func addStreams(t *testing.T, js jetstream.JetStream) { bustest.AddStreams(t, js) }

// addStream creates the stream bustest/streams/<name>.json defines.
func addStream(t *testing.T, js jetstream.JetStream, name string) { bustest.AddStream(t, js, name) }
