package race

import (
	"testing"

	"google.golang.org/protobuf/proto"

	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/salsa"
)

// Every field of v1's stored frame comes from its own field of the packet.
func TestAFrameHoldsThePacketsFields(t *testing.T) {
	var p salsa.Packet
	p.Position, p.Velocity = salsa.Vec3{X: 1, Y: 2, Z: 3}, salsa.Vec3{X: 4, Y: 5, Z: 6}
	p.Rotation, p.Orientation = salsa.Vec3{X: 0.25, Y: 0.5, Z: 0.75}, -0.5
	p.AngularVelocity = salsa.Vec3{X: 7, Y: 8, Z: 9}
	p.BodyHeight, p.EngineRPM, p.FuelLevel, p.Speed = 0.125, 7000, 42.5, 50
	p.Boost, p.OilPressure, p.WaterTemp, p.OilTemp = 1.5, 4.5, 85, 95
	p.TyreTemp = salsa.Wheels{FL: 71, FR: 72, RL: 73, RR: 74}
	p.TyreRadius = salsa.Wheels{FL: 0.31, FR: 0.32, RL: 0.33, RR: 0.34}
	p.SuspensionHeight = salsa.Wheels{FL: 0.01, FR: 0.02, RL: 0.03, RR: 0.04}
	p.WheelSpeed = salsa.Wheels{FL: -41, FR: -42, RL: -43, RR: -44}
	p.PacketID, p.CurrentLap, p.TimeOfDayMs, p.RacePosition, p.CarCode = 1234, 3, 45_000_000, 4, 683
	p.Flags = salsa.FlagCarsOnTrack | salsa.FlagHasTurbo | salsa.FlagTCS
	p.Gears = salsa.MakeGears(3, salsa.NoGear)
	p.Throttle, p.Brake = 255, 12

	pos := int32(4)
	want := &telemetryv1.Frame{
		Seq: 9, T: 12.5, PacketId: 1234, Lap: 3, Phase: phaseInRace,
		PosX: 1, PosY: 2, PosZ: 3, VelX: 4, VelY: 5, VelZ: 6,
		SpeedKph: 180, EngineRpm: 7000, Gear: 3, SuggestedGear: 15, Throttle: 255, Brake: 12,
		FuelLevel:  42.5,
		TireFlTemp: 71, TireFrTemp: 72, TireRlTemp: 73, TireRrTemp: 74,
		TireFlRadius: float64(float32(0.31)), TireFrRadius: float64(float32(0.32)),
		TireRlRadius: float64(float32(0.33)), TireRrRadius: float64(float32(0.34)),
		SusFl: float64(float32(0.01)), SusFr: float64(float32(0.02)),
		SusRl: float64(float32(0.03)), SusRr: float64(float32(0.04)),
		WheelFlRps: -41, WheelFrRps: -42, WheelRlRps: -43, WheelRrRps: -44,
		WaterTemp: 85, OilTemp: 95, OilPressure: 4.5, BoostPressure: 1.5, BodyHeight: 0.125,
		Position: &pos, TimeOfDayMs: 45_000_000, CarCode: 683,
		RotX: 0.25, RotY: 0.5, RotZ: 0.75, Orientation: -0.5, AngVelX: 7, AngVelY: 8, AngVelZ: 9,
		Flags: uint32(salsa.FlagCarsOnTrack | salsa.FlagHasTurbo | salsa.FlagTCS),
	}
	if got := frame(9, 12.5, phaseInRace, &p); !proto.Equal(got, want) {
		t.Errorf("got\n%v\nwant\n%v", got, want)
	}

	// Outside a race GT7 sends position -1, which the frame leaves out.
	p.RacePosition = -1
	if got := frame(9, 12.5, phaseInRace, &p); got.Position != nil {
		t.Errorf("position %d, want none", got.GetPosition())
	}
}
