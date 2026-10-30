package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/salsa"
)

// paddock#88, AC5: every field of v1's status route and /healthz is in ecu.status
// or ecu.session, with v1's value, or listed here with where it went. The golden
// is contracts' testdata/v1/gilles_villeneuve_status.json (paddock#72, AC2): what
// v1.0.0 answered mid-race, on the middle frame of lap 2 of
// real-nuc-gilles-villeneuve-race-2026-10-17.jsonl.gz, fed through its live app
// as v1's tests/test_status_api.py does; ecu takes the same frames here.

// v1LeftOut are v1's fields ecu.status and ecu.session leave out, each with why:
// contracts' statusLeftOut, and the car's name, which waits on paddock#86.
var v1LeftOut = map[string]string{
	"status.voice_connected":     "radio's, in its own live key (paddock#62)",
	"healthz.voice_connected":    "radio's, in its own live key (paddock#62)",
	"healthz.ok":                 "/healthz's own, from ecu's tasks, listener and NATS (cmd/ecu's healthOf)",
	"healthz.tasks":              "/healthz's own (cmd/ecu's healthOf)",
	"healthz.telemetry_restarts": "/healthz's own: how often v1 rebound its listener",
	"healthz.queue":              "the callouts waiting, now radio's (paddock#62)",
	"healthz.session":            "the session's kind: SessionKindChanged's, and LiveSession's session_type",
	"healthz.track":              "the session's track: SessionTrackChanged's, and LiveSession's track",
	"status.session.id":          "v1's own id format; session_id is the ULID ecu mints",
	"status.session.car":         "TODO(paddock#86): the car's name, from v1's car table",
}

// v1Unfed are v1's fields its test could not show: it fed the app frames with
// no listener, so it says not connected and no packets. ecu's are checked for
// being set, not for v1's value.
var v1Unfed = map[string]bool{
	"status.telemetry.connected":         true,
	"status.telemetry.last_packet_age_s": true,
	"healthz.telemetry_connected":        true,
	"healthz.packets":                    true,
}

func TestEveryV1StatusFieldIsInTheLiveKV(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(moduleDir(t, "github.com/those-who-run/contracts"),
		"testdata", "v1", "gilles_villeneuve_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]any
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	v1 := map[string]any{}
	flatten(v1, "", golden, map[string]bool{"healthz.session": true, "healthz.track": true, "healthz.tasks": true})

	frames := recording(t, gilles)
	mid := midLap2(t, frames)
	r := newRig(Config{Version: "1.0.0"})
	for _, f := range frames[:mid+1] {
		r.step(f)
	}
	v := r.t.Values()
	st, s := v.Status, v.Session
	if s == nil {
		t.Fatal("no session open on lap 2")
	}
	activity := strings.ToLower(strings.TrimPrefix(st.GetActivity().String(), "ACTIVITY_"))
	ecu := map[string]any{
		"status.version":                     st.GetVersion(),
		"status.started_at":                  float64(st.GetStartedAt().AsTime().UnixMilli()) / 1000,
		"status.activity":                    activity,
		"status.idle_secs":                   st.GetIdleSecs(),
		"status.telemetry.connected":         st.GetTelemetryConnected(),
		"status.telemetry.last_packet_age_s": st.LastPacketAgeSecs,
		"status.session.kind":                strings.ToLower(strings.TrimPrefix(s.GetSessionType().String(), "SESSION_TYPE_")),
		"status.session.track.id":            float64(s.GetTrack().GetTrackId()),
		"status.session.track.name":          s.GetTrack().GetName(),
		"status.session.track.sure":          s.GetTrack().GetSure(),
		"status.session.lap":                 float64(s.GetLap()),
		"status.session.laps_total":          float64(s.GetTotalLaps()),
		"status.session.position":            float64(s.GetPosition()),
		"status.session.cars":                float64(s.GetTotalCars()),
		"healthz.idle":                       st.GetIdle(),
		"healthz.idle_secs":                  st.GetIdleSecs(),
		"healthz.telemetry_connected":        st.GetTelemetryConnected(),
		"healthz.telemetry_listening":        st.GetTelemetryListening(),
		"healthz.packets":                    st.GetPackets(),
		"healthz.races_started":              float64(st.GetRacesStarted()),
	}
	for key, want := range v1 {
		if why, ok := v1LeftOut[key]; ok {
			if _, both := ecu[key]; both {
				t.Errorf("v1's %s is both compared and left out (%s)", key, why)
			}
			continue
		}
		got, ok := ecu[key]
		switch {
		case !ok:
			t.Errorf("v1's %s is neither in ecu.status or ecu.session nor in v1LeftOut", key)
		case v1Unfed[key]:
			if got == nil || got == false || got == uint32(0) {
				t.Errorf("%s: %v, want it set by a fed drive", key, got)
			}
		case got != want:
			t.Errorf("%s: ecu says %v, v1 said %v", key, got, want)
		}
	}
	for key := range ecu {
		if _, ok := v1[key]; !ok {
			t.Errorf("%s is compared but v1 has no such field", key)
		}
	}
	if s.GetCarName() != "" {
		t.Error("car_name is set: compare it with v1's car and drop it from v1LeftOut (paddock#86)")
	}
	if st.GetIdle() != (st.GetActivity() == statev1.Activity_ACTIVITY_IDLE) {
		t.Error("idle and the activity disagree")
	}
}

// flatten puts each field of m in flat under its dotted path, prefixed; an object
// is flattened too, unless keep names it.
func flatten(flat map[string]any, prefix string, m map[string]any, keep map[string]bool) {
	for k, v := range m {
		key := prefix + k
		if obj, ok := v.(map[string]any); ok && !keep[key] {
			flatten(flat, key+".", obj, keep)
			continue
		}
		flat[key] = v
	}
}

// midLap2 is v1's test's frame: the middle of the race's frames on lap 2.
func midLap2(t *testing.T, frames []salsa.Received) int {
	t.Helper()
	r := newRig(Config{})
	var lap2 []int
	for i, f := range frames {
		r.step(f)
		if r.m.Live().Phase == telemetryv1.Phase_PHASE_IN_RACE && f.CurrentLap == 2 {
			lap2 = append(lap2, i)
		}
	}
	if len(lap2) == 0 {
		t.Fatal("no frame on lap 2")
	}
	return lap2[len(lap2)/2]
}
