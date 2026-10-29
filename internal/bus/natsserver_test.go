package bus

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/those-who-run/ecu/internal/bus/bustest"
)

// The integration tests run nats-server itself, through bustest: nothing of NATS
// is mocked (R9).

// startNATS starts nats-server with JetStream on a free port, stopped when the test
// ends.
func startNATS(t *testing.T) *bustest.Server { return bustest.NewServer(t) }

// addStreams creates TELEMETRY and EVENTS, the definitions the bootstrap applies
// in compose and the cluster.
func addStreams(t *testing.T, js jetstream.JetStream) { bustest.AddStreams(t, js) }

// addStream creates the stream bustest/streams/<name>.json defines.
func addStream(t *testing.T, js jetstream.JetStream, name string) { bustest.AddStream(t, js, name) }
