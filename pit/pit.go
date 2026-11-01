// Package pit finds pit stops from telemetry: the entry, the stop, the exit, the
// in-laps and out-laps (paddock#78). It is v1's PitDetector, from
// src/those_who_run/pitstop.py at v1.0.0 (those-who-run/those-who-run), ported as
// it is; its comments are v1's, where each records what a recording showed.
//
// GT7 sends no pit flag. What it does is cut the camera: a pit stop is a run of
// one-frame loading screens in the middle of a race, with the lap counter never
// reset. Every stop recorded so far shows the same three cuts (Suzuka East
// 2026-10-15, Willow Springs three times 2026-10-16 and 17, Circuit Gilles
// Villeneuve 2026-10-17):
//
//  1. Entry. The car is moved into the pit lane (120 to 330 m in one frame) and
//     the next frame reads the pit limiter, 60.0 km/h with no pedals: GT7 drives
//     the car to its box. At Willow the picture freezes at the pit entry for five
//     seconds first (0 km/h, position fixed, still IN_RACE).
//  2. The box. One or two cuts with the car at rest. The fuel then climbs about
//     ten per cent a second to what was asked for (20 to 100 at Gilles
//     Villeneuve, 16 to 65 at Willow), and new tyres show as all four tyre
//     temperatures dropping to 60.0 °C in one frame. Tyre radius never changes.
//  3. Exit. After the car has moved off, a cut puts it at the pit exit at
//     80 km/h (79.9 on the next frame).
//
// The finish line can be anywhere in that sequence. Suzuka East crosses it in the
// entry cut, Willow in the exit cut, and Gilles Villeneuve in the pit lane itself,
// after the box, on a driving frame at the limiter. So the detector does not
// reason about the line: the in-lap is the lap the car entered the pits on and the
// out-lap the lap it left them on, and every lap from one to the other is a pit
// lap.
//
// The cuts are what finds a stop. The rest is recorded, not relied on: the limiter
// reading is only there after the entry cut, and cars do 60 km/h on track; the
// fuel only jumps if fuel was asked for; positions fall while the car stands in the
// box (P5 to P11 at Gilles Villeneuve, P3 to P10 at Willow), as they do in a spin.
//
// Out-of-race loading screens (menus, quitting, a restart) never keep the lap
// counter: the loading frame reads lap 0 and the next frame is the track screen. A
// cut whose loading frame keeps a lap of 1 or more, coming from a racing frame, is
// the pits; no race without a stop has one.
//
// A cut is a single packet, so a dropped packet must not hide it. Two fallbacks: a
// teleport (the car moves faster than TeleportKph between two frames, which no car
// does; the pit cuts imply thousands of km/h) counts as a cut, and the car going
// faster than ExitKph (no pit lane allows it; the exit cut lands at 80) ends a
// stop whose exit cut was missed.
//
// Not every cut or teleport is the pits: being put back on track after an off
// moves the car too. A "stop" that ends without the car ever coming to rest, with
// no fuel added and no tyres changed, was not a stop. It is withdrawn (the status
// goes back to what it was before the entry, so no stint starts and no lap is a
// pit lap) and kept as a Teleport instead.
//
// Feed every frame to Observe and read Status, like the track detector. The
// package has no clock, no network and no files.
package pit

import (
	"math"
	"strconv"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

// The detector's thresholds, v1's PitDetector defaults.
const (
	// TeleportKph is the speed between two frames, in km/h, that no car does: the
	// car was moved, not driven.
	TeleportKph = 600.0
	// TeleportM is the least a teleport moves the car, in metres: GT7 nudges a
	// car at rest by a few cm.
	TeleportM = 10.0
	// RestKph is the speed under which the car is at rest, in km/h.
	RestKph = 1.0
	// MovingKph is the speed from which the car is leaving the box, not
	// creeping, in km/h.
	MovingKph = 5.0
	// ExitKph is the speed no pit lane allows, in km/h: a stop whose exit cut was
	// lost ends there.
	ExitKph = 100.0
	// TyreDropC is how far, in °C, all four tyre temperatures drop in a frame
	// when the tyres are changed.
	TyreDropC = 5.0
	// MaxStepSecs caps a step's time at rest, in seconds: a pause or a gap is not
	// time at rest.
	MaxStepSecs = 1.0
	// RefuelMin is the fuel added, in litres, that counts as a service.
	RefuelMin = 0.5
	// PacketsPerSecond is GT7's packet rate, which times a step from the packet
	// counter.
	PacketsPerSecond = 60.0
)

// Stop is one pit stop (v1's PitStop).
type Stop struct {
	// Number is the stop's number, 1 for the first stop of the session.
	Number int32
	InLap  int32
	// OutLap is the lap the car left the pit lane on, when Done.
	OutLap int32
	EntryT float64
	// ExitT is the exit's time, when Done.
	ExitT float64
	// StationarySecs is the time at rest in the pit lane, mostly the box.
	StationarySecs float64
	FuelBefore     float64
	// FuelAfter is the level as the car leaves, or now while in the pits.
	FuelAfter    float64
	TyresChanged bool
	// PitLaneKph is the speed on the first driving frame after the entry cut:
	// the limiter.
	PitLaneKph float64
	// Exited says the car has left the pit lane: v1's out_lap is not None. It is
	// a flag of its own, not OutLap != 0, so a stop left on lap 0 is done too.
	Exited bool
	// PositionBefore and PositionAfter are the race positions, 0 for none.
	PositionBefore int32
	PositionAfter  int32
	Evidence       string
}

// Done says the car has left the pit lane.
func (s Stop) Done() bool { return s.Exited }

// FuelAdded is the fuel added, never below 0.
func (s Stop) FuelAdded() float64 { return max(s.FuelAfter-s.FuelBefore, 0) }

// PitLaneSecs is the time from the entry cut to the exit cut: the limiter, the box
// and the drive out. ok is false while the car is still in the pit lane.
func (s Stop) PitLaneSecs() (secs float64, ok bool) {
	if !s.Done() {
		return 0, false
	}
	return s.ExitT - s.EntryT, true
}

func (s Stop) covers(lap int32) bool {
	return s.InLap <= lap && (!s.Done() || lap <= s.OutLap)
}

// Teleport is a cut or teleport that opened a stop the car never made: no rest, no
// service (v1's Teleport).
type Teleport struct {
	Lap int32
	// T is when it was withdrawn: the frame that would have closed the stop.
	T      float64
	EntryT float64
	// Evidence is the entry's, then why it ended and why it was no stop.
	Evidence string
}

// Role is what a lap had to do with the pits (v1's LapRole).
type Role int

// The roles.
const (
	// RoleNone is a lap that never saw the pit lane.
	RoleNone Role = iota
	// RoleIn is the lap the car entered the pit lane on.
	RoleIn
	// RoleOut is a lap after the in-lap, up to the out-lap.
	RoleOut
	// RoleInOut is one lap that is both the in-lap and the out-lap.
	RoleInOut
)

// Status is the detector's view of the session's stops (v1's PitStatus).
type Status struct {
	InPitLane bool
	// Stops are the stops in order, the one the car is in last.
	Stops []Stop
	// Teleports are the withdrawn stops, oldest first.
	Teleports []Teleport
}

// Stint is 1 until the first stop is done, then one more for every stop.
func (s Status) Stint() int32 {
	n := int32(1)
	for _, stop := range s.Stops {
		if stop.Done() {
			n++
		}
	}
	return n
}

// StintStartLap is the lap the current stint began on: the last stop's out-lap,
// or lap 1, as for an out-lap of 0 (v1's `out_lap or 1`).
func (s Status) StintStartLap() int32 {
	for i := len(s.Stops) - 1; i >= 0; i-- {
		if s.Stops[i].Done() {
			return max(s.Stops[i].OutLap, 1)
		}
	}
	return 1
}

// IsPitLap says any part of lap was spent in the pit lane: an in-lap or out-lap.
func (s Status) IsPitLap(lap int32) bool { return s.LapRole(lap) != RoleNone }

// LapRole is what lap had to do with the pits.
func (s Status) LapRole(lap int32) Role {
	for _, stop := range s.Stops {
		if !stop.covers(lap) {
			continue
		}
		if lap == stop.InLap {
			if stop.Done() && stop.OutLap == lap {
				return RoleInOut
			}
			return RoleIn
		}
		return RoleOut
	}
	return RoleNone
}

// Change is what a frame changed: nothing, or an entry, an exit or an entry
// withdrawn.
type Change int

// The changes Observe returns.
const (
	// Unchanged is a frame that neither entered nor left the pit lane.
	Unchanged Change = iota
	// Entered is a pit entry: the last of Status().Stops is the stop, open.
	Entered
	// Exited is a pit exit: the last of Status().Stops is the stop, done.
	Exited
	// Withdrawn is an entry that was no stop: the last of Status().Teleports is
	// it.
	Withdrawn
)

// Detector finds pit stops frame by frame (v1's PitDetector). The zero value is
// ready to use. A Detector is not safe for concurrent use.
type Detector struct {
	stops     []Stop
	teleports []Teleport
	open      *Stop
	last      *telemetryv1.Frame // the last racing frame
	cut       bool               // the frames since last include a loading screen
	paused    bool               // ... or a pause, which is not time at rest
	rested    bool               // the open stop has seen the car at rest
}

// New is a detector that has seen nothing.
func New() *Detector { return &Detector{} }

// Reset forgets everything: call on a race start or restart.
func (d *Detector) Reset() { *d = Detector{} }

// Resume starts the detector from the stops a session had before an ecu restart
// (paddock#76): done are the stops it completed, numbered from 1, and open the
// stop the car was in, nil when it was not in the pit lane. The next stop takes
// the next number, the stints and the laps' roles count them, and an open stop is
// left as at rest, so the car driving out closes it as a stop, never as an entry
// withdrawn: the earlier run had published the entry. The frame before the
// restart is not known, so the first frame after it changes nothing.
func (d *Detector) Resume(done []Stop, open *Stop) {
	d.Reset()
	d.stops = append([]Stop(nil), done...)
	if open != nil {
		o := *open
		d.open, d.rested = &o, true
	}
}

// InPitLane says the car is in the pit lane: a stop is open (Status().InPitLane,
// without copying the stops).
func (d *Detector) InPitLane() bool { return d.open != nil }

// Status is the stops so far, the open one last, and the withdrawn ones.
func (d *Detector) Status() Status {
	stops := append([]Stop(nil), d.stops...)
	if d.open != nil {
		stops = append(stops, *d.open)
	}
	return Status{InPitLane: d.open != nil, Stops: stops, Teleports: append([]Teleport(nil), d.teleports...)}
}

// Observe takes the next frame and says what it changed.
func (d *Detector) Observe(f *telemetryv1.Frame) Change {
	switch f.GetPhase() {
	case telemetryv1.Phase_PHASE_LOADING:
		if d.last != nil && f.GetLap() >= 1 {
			d.cut = true
		} else {
			d.last, d.cut = nil, false // out of the race
		}
		return Unchanged
	case telemetryv1.Phase_PHASE_PAUSED:
		d.paused = true
		return Unchanged
	case telemetryv1.Phase_PHASE_IN_RACE:
	default:
		d.last, d.cut = nil, false
		return Unchanged
	}
	last, loading, paused := d.last, d.cut, d.paused
	d.last, d.cut, d.paused = f, false, false
	if last == nil {
		return Unchanged
	}
	if f.GetLap() < last.GetLap() {
		d.Reset() // a restart from the pause menu: back on the grid
		d.last = f
		return Unchanged
	}
	var cut string
	switch {
	case loading:
		cut = "loading cut"
	case teleported(last, f):
		cut = "teleport"
	}
	if d.open == nil {
		if cut == "" {
			return Unchanged
		}
		d.enter(last, f, cut)
		return Entered
	}
	return d.inPits(last, f, cut, paused)
}

func teleported(last, f *telemetryv1.Frame) bool {
	moved := math.Hypot(f.GetPosX()-last.GetPosX(), f.GetPosZ()-last.GetPosZ())
	if moved < TeleportM {
		return false
	}
	// Time from the packet counter (60 a second) as well as the clock: packets
	// arrive in bursts, so two frames received 0.4 s apart can be 2 s of driving
	// apart (Suzuka East, 2026-10-15), which the clock alone reads as a car doing
	// 900 km/h.
	packets := float64(int64(f.GetPacketId()) - int64(last.GetPacketId()))
	elapsed := max(f.GetT()-last.GetT(), packets/PacketsPerSecond)
	return elapsed <= 0 || moved/elapsed*3.6 > TeleportKph
}

func (d *Detector) enter(last, f *telemetryv1.Frame, cut string) {
	d.rested = false
	d.open = &Stop{
		Number:         int32(len(d.stops) + 1), //nolint:gosec // a session's stops
		InLap:          last.GetLap(),
		EntryT:         f.GetT(),
		FuelBefore:     last.GetFuelLevel(),
		FuelAfter:      f.GetFuelLevel(),
		PitLaneKph:     f.GetSpeedKph(),
		PositionBefore: last.GetPosition(),
		PositionAfter:  f.GetPosition(),
		Evidence:       "entry " + cut + " at " + strconv.FormatFloat(f.GetSpeedKph(), 'f', 1, 64) + " km/h",
	}
}

func (d *Detector) inPits(last, f *telemetryv1.Frame, cut string, paused bool) Change {
	stop := d.open
	if f.GetSpeedKph() < RestKph {
		d.rested = true
		if !paused {
			stop.StationarySecs += min(max(f.GetT()-last.GetT(), 0), MaxStepSecs)
		}
	}
	stop.TyresChanged = stop.TyresChanged ||
		last.GetTireFlTemp()-f.GetTireFlTemp() > TyreDropC &&
			last.GetTireFrTemp()-f.GetTireFrTemp() > TyreDropC &&
			last.GetTireRlTemp()-f.GetTireRlTemp() > TyreDropC &&
			last.GetTireRrTemp()-f.GetTireRrTemp() > TyreDropC
	stop.FuelAfter = f.GetFuelLevel()
	stop.PositionAfter = f.GetPosition()
	var out string
	switch {
	case cut != "" && d.rested && last.GetSpeedKph() >= MovingKph:
		out = "exit " + cut
	case f.GetSpeedKph() > ExitKph:
		out = "over " + strconv.FormatFloat(ExitKph, 'f', 0, 64) + " km/h"
	default:
		return Unchanged
	}
	d.open = nil
	if !d.rested && !stop.TyresChanged && stop.FuelAdded() < RefuelMin {
		d.teleports = append(d.teleports, Teleport{
			Lap:      stop.InLap,
			T:        f.GetT(),
			EntryT:   stop.EntryT,
			Evidence: stop.Evidence + "; " + out + "; never at rest, no fuel, no tyres",
		})
		return Withdrawn
	}
	stop.OutLap, stop.ExitT, stop.Exited = f.GetLap(), f.GetT(), true
	stop.Evidence += "; " + out
	d.stops = append(d.stops, *stop)
	return Exited
}
