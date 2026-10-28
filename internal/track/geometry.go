package track

import "math"

// The geometry the detector reads (v1's trackdetect.py at v1.0.0), ported as it is,
// operation for operation, so the same positions give the same answers.

// boxContains says inner lies inside outer, give or take tolerance.
func boxContains(outer, inner Box, tolerance float64) bool {
	return outer.MinX-tolerance <= inner.MinX &&
		outer.MinZ-tolerance <= inner.MinZ &&
		inner.MaxX <= outer.MaxX+tolerance &&
		inner.MaxZ <= outer.MaxZ+tolerance
}

// boxIoU is the area the two boxes share over the area they cover, 0 to 1.
func boxIoU(a, b Box) float64 {
	ix := math.Max(0, math.Min(a.MaxX, b.MaxX)-math.Max(a.MinX, b.MinX))
	iz := math.Max(0, math.Min(a.MaxZ, b.MaxZ)-math.Max(a.MinZ, b.MinZ))
	inter := ix * iz
	union := (a.MaxX-a.MinX)*(a.MaxZ-a.MinZ) + (b.MaxX-b.MinX)*(b.MaxZ-b.MinZ) - inter
	if union > 0 {
		return inter / union
	}
	return 0
}

// segmentCrosses says segment a-b intersects segment c-d (touching counts).
func segmentCrosses(a, b, c, d Point) bool {
	rx, rz := b.X-a.X, b.Z-a.Z
	sx, sz := d.X-c.X, d.Z-c.Z
	den := rx*sz - rz*sx
	if den == 0 {
		return false // parallel or collinear: the next frame will tell
	}
	qx, qz := c.X-a.X, c.Z-a.Z
	t := (qx*sz - qz*sx) / den
	u := (qx*rz - qz*rx) / den
	return 0 <= t && t <= 1 && 0 <= u && u <= 1
}

// travelDirection is the direction code for a move from a to b, as the table
// encodes it.
//
// The source table (gt7telemetry) keys each line on the sign of the car's x
// movement when it crossed, falling back to z only when x did not change at all.
// It is not the dominant axis: a finish straight running mostly along z with a
// slight westward drift is "NX". Match that exactly.
func travelDirection(a, b Point) Direction {
	dx, dz := b.X-a.X, b.Z-a.Z
	switch {
	case dx > 0:
		return PX
	case dx < 0:
		return NX
	case dz > 0:
		return PZ
	case dz < 0:
		return NZ
	default:
		return DirectionNone
	}
}

// widenSegment is the segment a-b extended along its own direction to
// 2*halfWidth long, with the same midpoint.
func widenSegment(a, b Point, halfWidth float64) [2]Point {
	mx, mz := (a.X+b.X)/2, (a.Z+b.Z)/2
	dx, dz := b.X-a.X, b.Z-a.Z
	length := math.Hypot(dx, dz)
	if length == 0 {
		return [2]Point{a, b}
	}
	ux, uz := dx/length, dz/length
	return [2]Point{{mx - ux*halfWidth, mz - uz*halfWidth}, {mx + ux*halfWidth, mz + uz*halfWidth}}
}

// distanceToSegment is how far p is from the nearest point of segment a-b.
func distanceToSegment(p, a, b Point) float64 {
	abx, abz := b.X-a.X, b.Z-a.Z
	ab2 := abx*abx + abz*abz
	if ab2 == 0 {
		return math.Hypot(p.X-a.X, p.Z-a.Z)
	}
	t := math.Max(0, math.Min(1, ((p.X-a.X)*abx+(p.Z-a.Z)*abz)/ab2))
	qx, qz := a.X+t*abx, a.Z+t*abz
	return math.Hypot(p.X-qx, p.Z-qz)
}
