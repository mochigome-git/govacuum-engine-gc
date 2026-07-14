// Package logic holds the IQR percentile math and status classification —
// a direct port of the original update_iqr_status() Postgres trigger.
package logic

import "math"

// percentileContinuous replicates PostgreSQL's percentile_cont(p) WITHIN
// GROUP (ORDER BY ...) — linear interpolation between order statistics.
// sorted must already be sorted ascending.
func percentileContinuous(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	idx := p * float64(n-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower] + frac*(sorted[upper]-sorted[lower])
}

// iqrBounds returns (q1, q3) for a sorted slice.
func iqrBounds(sorted []float64) (q1, q3 float64) {
	return percentileContinuous(sorted, 0.25), percentileContinuous(sorted, 0.75)
}

// CalcXY replicates the generated x/y columns:
//
//	x = ((v1/60) - (v2/120)) / NULLIF(v1/60, 0)
//	y = ((v2/120) - (v3/180)) / NULLIF(v2/120, 0)
//
// Returns nil (not a pointer to zero) when the denominator is zero, exactly
// matching Postgres NULLIF-then-divide-by-null-is-null semantics.
func CalcXY(v1, v2, v3 float64) (x, y *float64) {
	d1 := v1 / 60
	d2 := v2 / 120
	d3 := v3 / 180

	if d1 != 0 {
		val := (d1 - d2) / d1
		x = &val
	}
	if d2 != 0 {
		val := (d2 - d3) / d2
		y = &val
	}
	return x, y
}

// classifyStatus ports update_iqr_status() directly:
//   - "Over 1000 Pa/sec" if any leave-time exceeds 1000
//   - "Outlier" if the value falls outside [q1-1.5*IQR, q3+1.5*IQR]
//     (skipped entirely when the value is nil, matching SQL NULL comparisons
//     always being unknown/false — falls through to the next check)
//   - "Initial Failed" if vacuumStart > 20
//   - "Within IQR" otherwise
func classifyStatus(value *float64, vacuumStart int, v1, v2, v3, q1, q3 float64) (status string, within bool) {
	over1000 := v1 > 1000 || v2 > 1000 || v3 > 1000
	iqr := q3 - q1

	switch {
	case over1000:
		return "Over 1000 Pa/sec", false
	case value != nil && (*value < q1-1.5*iqr || *value > q3+1.5*iqr):
		return "Outlier", false
	case vacuumStart > 20:
		return "Initial Failed", false
	default:
		return "Within IQR", true
	}
}

// ComputeVacuumStatus is the full port of the trigger function body.
func ComputeVacuumStatus(vacuumStart int, v1, v2, v3 float64, x, y *float64, historicalX, historicalY []float64) (xStatus, yStatus string, vacuumStatus bool) {
	q1x, q3x := iqrBounds(historicalX)
	q1y, q3y := iqrBounds(historicalY)

	var xWithin, yWithin bool
	xStatus, xWithin = classifyStatus(x, vacuumStart, v1, v2, v3, q1x, q3x)
	yStatus, yWithin = classifyStatus(y, vacuumStart, v1, v2, v3, q1y, q3y)

	return xStatus, yStatus, xWithin && yWithin
}
