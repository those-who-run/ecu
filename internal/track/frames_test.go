package track_test

import (
	"math/rand/v2"
	"time"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/internal/track"
	"github.com/those-who-run/ecu/race"
	"github.com/those-who-run/salsa"
)

// The detector's tests are fed the frames ecu makes, by race, which
// imports internal/track: only this external test package can import it.
func init() {
	track.FramesOf = func(packets []salsa.Received) []*telemetryv1.Frame {
		epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		m := race.New(race.NewIDs(func() time.Time { return epoch }, rand.NewChaCha8([32]byte{'e', 'c', 'u'})))
		all := make([]*telemetryv1.Frame, len(packets))
		for i, r := range packets {
			all[i] = m.Step(race.Received{Received: r, Time: epoch})[0].Frame
		}
		return all
	}
}
