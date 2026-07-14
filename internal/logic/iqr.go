// Package logic holds the IQR percentile math and status classification —
// a direct port of the original update_iqr_status() Postgres trigger.
package logic

import "math"

// minIQRSamples is the minimum number of historical points needed before
// an IQR bound means anything. With very few points (2-3), Q1/Q3 land
// almost on top of each other, so the IQR band is razor-thin and any
// reading that isn't nearly identical to the first couple gets flagged
// "Outlier" — a false positive caused by lack of data, not an actual
// anomaly. Below this count we report "No Data"/"Building Data" instead
// and let the write through, rather than judging against a statistically
// meaningless band.
const minIQRSamples = 10

// percentileContinuous replicates PostgreSQL's percentile_cont(p) WITHIN
// GROUP (ORDER BY ...) — linear interpolation between order statistics.
// sorted must already be sorted ascending. Returns NaN for an empty slice,
// matching Postgres returning NULL for percentile_cont() over zero rows —
// callers must treat NaN as "no bound", not zero, or every outlier check
// silently becomes "greater than 0" for any device with no history yet.
func percentileContinuous(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return math.NaN()
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

// classifyStatus ports update_iqr_status(), with two additions on top of
// the original trigger to handle a device that doesn't have enough
// history yet:
//
//   - "Over 1000 Pa/sec" if any leave-time exceeds 1000 (hard fault,
//     checked first, independent of history)
//   - "Initial Failed" if vacuumStart > 20 (also a hard fault, independent
//     of history — checked before the data-volume checks below so a real
//     fault still blocks the write even while history is thin)
//   - "No Data" if there's no history at all for this device yet
//     (sampleCount == 0) — within=true, write proceeds
//   - "Building Data" if there's some history but not enough to trust the
//     IQR bounds yet (sampleCount < minIQRSamples) — within=true, write
//     proceeds. With only 2-3 points, Q1/Q3 sit almost on top of each
//     other, so the "outlier" band is too thin to mean anything.
//   - "Outlier" if the value falls outside [q1-1.5*IQR, q3+1.5*IQR], once
//     there's enough history to compute that meaningfully
//   - "Within IQR" otherwise
func classifyStatus(value *float64, vacuumStart int, v1, v2, v3, q1, q3 float64, sampleCount int) (status string, within bool) {
	over1000 := v1 > 1000 || v2 > 1000 || v3 > 1000
	iqr := q3 - q1

	switch {
	case over1000:
		return "Over 1000 Pa/sec", false
	case vacuumStart > 20:
		return "Initial Failed", false
	case sampleCount == 0:
		return "No Data", true
	case sampleCount < minIQRSamples:
		return "Building Data", true
	case value != nil && (*value < q1-1.5*iqr || *value > q3+1.5*iqr):
		return "Outlier", false
	default:
		return "Within IQR", true
	}
}

// ComputeVacuumStatus is the full port of the trigger function body.
// xStatus/yStatus remain descriptive strings ("Within IQR", "Outlier",
// "No Data", "Building Data", ...) — kept for the row that goes to
// EMQX/the DB, where a human might read them later. xWithin/yWithin are
// the same classification as plain booleans, for the local reply to
// gopub-edge that drives PLC write-back (patch.VacuumData.XStatus/YStatus
// are bool, not string). historicalX/historicalY always have matching
// lengths (db.FetchHistoricalXY only appends a pair when both x and y are
// present on a row), so either length works as the shared sample count.
func ComputeVacuumStatus(vacuumStart int, v1, v2, v3 float64, x, y *float64, historicalX, historicalY []float64) (xStatus, yStatus string, xWithin, yWithin, vacuumStatus bool) {
	q1x, q3x := iqrBounds(historicalX)
	q1y, q3y := iqrBounds(historicalY)
	sampleCount := len(historicalX)

	xStatus, xWithin = classifyStatus(x, vacuumStart, v1, v2, v3, q1x, q3x, sampleCount)
	yStatus, yWithin = classifyStatus(y, vacuumStart, v1, v2, v3, q1y, q3y, sampleCount)

	return xStatus, yStatus, xWithin, yWithin, xWithin && yWithin
}
