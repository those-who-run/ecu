// Package record writes the packets ecu drives on to a recording in v1's format
// (paddock#266): JSON lines, gzip-compressed, one v1 RaceState per line, byte for
// byte as v1's RaceState.to_dict and RecordingWriter write them
// (src/those_who_run/state.py and replay.py at v1.0.0). So a drive raced on v2
// replays through v1's `replay` as well as through salsa's OpenRecording, and the
// two can be compared on it.
package record

import (
	"bytes"
	"math"
	"strconv"

	"github.com/those-who-run/salsa"
)

// v1Version is v1's STATE_FORMAT_VERSION, the "v" every line ends with.
const v1Version = 2

// AppendV1 appends p, received at t seconds on the source's clock, as one line of a
// v1 recording, with its line ending, and reports whether it could. The keys and
// their order are RaceState.to_dict's, numbers are written as Python's json.dumps
// writes them, and the values are what RaceState.from_telemetry took from
// gt-telem's reading of the packet. It cannot write a value that is not finite: v1
// would write NaN or Infinity, which is not JSON, and salsa refuses it, so the
// line is not written and AppendV1 returns dst and false.
func AppendV1(dst []byte, t float64, p *salsa.Packet) ([]byte, bool) {
	start := len(dst)
	w := v1Line{b: dst}
	suggested, _ := p.Gears.Suggested() // 15 when GT7 suggests none, as v1 stored it
	w.b = append(w.b, '{')
	w.float("t", t)
	w.int("packet_id", int64(p.PacketID))
	w.str("phase", v1Phase(p))
	w.int("current_lap", int64(p.CurrentLap))
	w.int("total_laps", int64(p.TotalLaps))
	w.int("best_lap_ms", int64(p.BestLapMs))
	w.int("last_lap_ms", int64(p.LastLapMs))
	w.f32("fuel_level", p.FuelLevel)
	w.f32("fuel_capacity", p.FuelCapacity)
	w.float("speed_kph", p.SpeedKPH())
	w.f32("engine_rpm", p.EngineRPM)
	w.wheels("tire_temp", p.TyreTemp)
	w.f32("water_temp", p.WaterTemp)
	w.f32("oil_temp", p.OilTemp)
	// v1 worked the track out itself, later; from_telemetry left it None.
	w.key("track_id")
	w.b = append(w.b, "null"...)
	w.int("race_start_pos", int64(p.RacePosition))
	w.int("total_cars", int64(p.TotalCars))
	w.key("cars_on_track")
	w.b = strconv.AppendBool(w.b, p.Flags.CarsOnTrack())
	w.f32("pos_x", p.Position.X)
	w.f32("pos_y", p.Position.Y)
	w.f32("pos_z", p.Position.Z)
	w.f32("vel_x", p.Velocity.X)
	w.f32("vel_y", p.Velocity.Y)
	w.f32("vel_z", p.Velocity.Z)
	w.f32("rot_x", p.Rotation.X)
	w.f32("rot_y", p.Rotation.Y)
	w.f32("rot_z", p.Rotation.Z)
	w.f32("orientation", p.Orientation)
	w.f32("ang_vel_x", p.AngularVelocity.X)
	w.f32("ang_vel_y", p.AngularVelocity.Y)
	w.f32("ang_vel_z", p.AngularVelocity.Z)
	w.int("throttle", int64(p.Throttle))
	w.int("brake", int64(p.Brake))
	w.int("current_gear", int64(p.Gears.Current()))
	w.int("suggested_gear", int64(suggested))
	w.wheels("tire_radius", p.TyreRadius)
	w.wheels("suspension_height", p.SuspensionHeight)
	w.wheels("wheel_rps", p.WheelSpeed)
	w.f32("boost_pressure", p.Boost)
	w.f32("oil_pressure", p.OilPressure)
	w.f32("body_height", p.BodyHeight)
	w.int("car_code", int64(p.CarCode))
	w.int("time_of_day_ms", int64(p.TimeOfDayMs))
	w.int("v", v1Version)
	w.b = append(w.b, "}\n"...)
	if !w.ok {
		return dst[:start], false
	}
	return w.b, true
}

// v1Phase is v1's derive_phase (state.py), from the lap counters and flags bits 1
// (paused) and 2 (loading), as gt-telem read them.
func v1Phase(p *salsa.Packet) string {
	switch {
	case p.Flags.Loading():
		return "loading"
	case p.CurrentLap < 0:
		return "menu"
	case p.Flags.Paused():
		return "paused"
	case p.TotalLaps > 0 && p.CurrentLap > p.TotalLaps:
		return "finished"
	case p.CurrentLap >= 1:
		return "in_race"
	default:
		return "at_track"
	}
}

// v1Line is a line being written: json.dumps with separators (",", ":").
type v1Line struct {
	b  []byte
	n  int  // keys written
	ok bool // false once a value could not be written
}

func (w *v1Line) key(k string) {
	if w.n == 0 {
		w.ok = true
	} else {
		w.b = append(w.b, ',')
	}
	w.n++
	w.b = append(w.b, '"')
	w.b = append(w.b, k...)
	w.b = append(w.b, '"', ':')
}

func (w *v1Line) str(k, v string) {
	w.key(k)
	w.b = append(w.b, '"')
	w.b = append(w.b, v...)
	w.b = append(w.b, '"')
}

func (w *v1Line) int(k string, v int64) {
	w.key(k)
	w.b = strconv.AppendInt(w.b, v, 10)
}

// f32 writes a packet's float32 as gt-telem gave it to v1: unpacked to a Python
// float, so widened to float64 exactly.
func (w *v1Line) f32(k string, v float32) { w.float(k, float64(v)) }

func (w *v1Line) float(k string, v float64) {
	w.key(k)
	w.number(v)
}

func (w *v1Line) wheels(k string, v salsa.Wheels) {
	w.key(k)
	w.b = append(w.b, '[')
	for i, f := range []float32{v.FL, v.FR, v.RL, v.RR} {
		if i > 0 {
			w.b = append(w.b, ',')
		}
		w.number(float64(f))
	}
	w.b = append(w.b, ']')
}

// number writes v as Python's float repr, which json.dumps uses: the shortest
// digits that read back as v, in fixed notation with at least one decimal
// ("100.0", "-0.0") when the decimal exponent is from -4 to 15, and in exponent
// notation with a signed exponent of two digits or more otherwise ("1e-05",
// "1.5e+16").
func (w *v1Line) number(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		w.ok = false
		w.b = append(w.b, '0')
		return
	}
	// The exponent of the shortest digits decides, as in Python's repr.
	start := len(w.b)
	w.b = strconv.AppendFloat(w.b, v, 'e', -1, 64)
	e := w.b[start:]
	exp, _ := strconv.Atoi(string(e[bytes.IndexByte(e, 'e')+1:])) // AppendFloat writes a valid exponent
	if exp < -4 || exp >= 16 {
		return
	}
	w.b = strconv.AppendFloat(w.b[:start], v, 'f', -1, 64)
	if bytes.IndexByte(w.b[start:], '.') < 0 {
		w.b = append(w.b, '.', '0')
	}
}
