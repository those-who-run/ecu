package race

import (
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/salsa"
)

// frame is the contracts frame for a packet: the fields of v1's stored frame
// (store.FRAME_COLUMNS at v1.0.0, which store.frame_row fills from a RaceState),
// with the frame's seq, its phase and the packet's raw flags.
func frame(seq uint32, t float64, phase telemetryv1.Phase, p *salsa.Packet) *telemetryv1.Frame {
	suggested, _ := p.Gears.Suggested() // 15 when GT7 suggests none, as v1 stored it
	f := &telemetryv1.Frame{
		Seq: seq,
		T:   t,
		// GT7's packet counter never goes below 0; the contract keeps it unsigned.
		PacketId:      uint32(p.PacketID), //nolint:gosec // see above
		Lap:           int32(p.CurrentLap),
		Phase:         phase,
		PosX:          float64(p.Position.X),
		PosY:          float64(p.Position.Y),
		PosZ:          float64(p.Position.Z),
		VelX:          float64(p.Velocity.X),
		VelY:          float64(p.Velocity.Y),
		VelZ:          float64(p.Velocity.Z),
		SpeedKph:      p.SpeedKPH(),
		EngineRpm:     float64(p.EngineRPM),
		Gear:          int32(p.Gears.Current()),
		SuggestedGear: int32(suggested),
		Throttle:      int32(p.Throttle),
		Brake:         int32(p.Brake),
		FuelLevel:     float64(p.FuelLevel),
		TireFlTemp:    float64(p.TyreTemp.FL),
		TireFrTemp:    float64(p.TyreTemp.FR),
		TireRlTemp:    float64(p.TyreTemp.RL),
		TireRrTemp:    float64(p.TyreTemp.RR),
		TireFlRadius:  float64(p.TyreRadius.FL),
		TireFrRadius:  float64(p.TyreRadius.FR),
		TireRlRadius:  float64(p.TyreRadius.RL),
		TireRrRadius:  float64(p.TyreRadius.RR),
		SusFl:         float64(p.SuspensionHeight.FL),
		SusFr:         float64(p.SuspensionHeight.FR),
		SusRl:         float64(p.SuspensionHeight.RL),
		SusRr:         float64(p.SuspensionHeight.RR),
		WheelFlRps:    float64(p.WheelSpeed.FL),
		WheelFrRps:    float64(p.WheelSpeed.FR),
		WheelRlRps:    float64(p.WheelSpeed.RL),
		WheelRrRps:    float64(p.WheelSpeed.RR),
		WaterTemp:     float64(p.WaterTemp),
		OilTemp:       float64(p.OilTemp),
		OilPressure:   float64(p.OilPressure),
		BoostPressure: float64(p.Boost),
		BodyHeight:    float64(p.BodyHeight),
		TimeOfDayMs:   p.TimeOfDayMs,
		CarCode:       p.CarCode,
		// TrackId stays absent: the packet carries no track, and finding it from
		// the car's position is the track detection that comes later (paddock#69).
		RotX:        float64(p.Rotation.X),
		RotY:        float64(p.Rotation.Y),
		RotZ:        float64(p.Rotation.Z),
		Orientation: float64(p.Orientation),
		AngVelX:     float64(p.AngularVelocity.X),
		AngVelY:     float64(p.AngularVelocity.Y),
		AngVelZ:     float64(p.AngularVelocity.Z),
		Flags:       uint32(p.Flags),
	}
	if p.RacePosition > 0 {
		pos := int32(p.RacePosition)
		f.Position = &pos
	}
	return f
}
