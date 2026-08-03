package pricing

// This file is the one place in the package that is allowed to name float64, and it
// contains no arithmetic on one. Request.Seconds is a measured quantity handed to us by
// the metering layer; converting it to an exact decimal here keeps binary floating point
// out of the price path entirely (§8.3). TestNoFloatInPricePath enforces the boundary.
//
// [UtilizationFromFraction] is the same job for the same reason: a backend's reported
// occupancy arrives as a float64 from a JSON decoder and becomes an exact integer here,
// before any rate is multiplied by it. It is also where the trap VLLM.md §3.1 names is
// caught, because this is the one line every observation crosses.

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// microsPerSecond is the fixed quantum Request.Seconds is reduced to.
const microsPerSecond = 1_000_000

// secondsToMicros converts a measured duration to exact micro-seconds without doing
// arithmetic in binary floating point: strconv renders the value as a correctly rounded
// decimal string and the exact decimal parser takes it from there.
func secondsToMicros(s float64) (int64, error) {
	if math.IsNaN(s) || math.IsInf(s, 0) {
		return 0, errors.New("pricing: seconds is not a finite number")
	}
	if s < 0 {
		return 0, errors.New("pricing: seconds is negative")
	}
	if s == 0 {
		return 0, nil
	}
	if s > 1e12 {
		return 0, ErrOverflow
	}
	d, err := parseDecimal(string(strconv.AppendFloat(nil, s, 'f', 6, 64)))
	if err != nil {
		return 0, err
	}
	micros, ok := d.scaled(6)
	if !ok || micros > maxInt64 {
		return 0, ErrOverflow
	}
	return int64(micros), nil
}

// ErrUtilizationNotAFraction is the refusal for an occupancy reading outside [0,1].
//
// # The metric's own name is wrong, and reading it wrong costs 100x
//
// vLLM exports `vllm:kv_cache_usage_perc` and it is **a fraction, not a percentage**
// (VLLM.md §3.1: the documentation string says "1 means 100 percent usage"). A saturated
// cache reads 1.0, not 100. Anything that hands this function a percentage is handing it
// a number a hundred times too large, and since the multiplier is linear in it, the
// invoice is a hundred times too large as well — quietly, because 45.0 is a perfectly
// well-formed float and every arithmetic step downstream succeeds.
//
// So the range is CHECKED and the check REFUSES rather than clamps. A clamp to 1.0 would
// price the request at the ceiling and look plausible forever; the operator would never
// learn that their scale is wrong, and every request would be billed at the maximum. The
// refusal costs the operator the premium on that one request and tells them why.
//
// What the check cannot catch, stated because it is the direction the mistake can still
// fall in: a percentage-scaled source reporting 0.45 (meaning 0.45%) is read as 45% and
// is indistinguishable from a correct fraction. The guard catches the direction that
// OVERCHARGES, which is the direction that has to be caught — an undercharge is the
// operator's own money and shows up in their margin, an overcharge is a caller's money
// and shows up in a dispute.
var ErrUtilizationNotAFraction = errors.New(
	"pricing: utilization is not a fraction in [0,1]; vLLM's kv_cache_usage_perc is a " +
		"FRACTION despite its name (1.0 means 100% full, VLLM.md §3.1), so a value above " +
		"1 is a percentage-scaled reading that would multiply the price by 100x")

// UtilizationFromFraction converts an observed occupancy in [0,1] to the exact
// parts-per-million integer [Request.UtilizationPPM] carries.
//
// It is the only way a measured occupancy becomes a price input, and it is deliberately
// the only exported entry: a caller cannot set the field from a float without passing the
// range guard above.
func UtilizationFromFraction(f float64) (int32, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("pricing: utilization is not a finite number")
	}
	if f < 0 || f > 1 {
		return 0, fmt.Errorf("%w: got %g", ErrUtilizationNotAFraction, f)
	}
	if f == 0 {
		return 0, nil
	}
	d, err := parseDecimal(string(strconv.AppendFloat(nil, f, 'f', 6, 64)))
	if err != nil {
		return 0, err
	}
	ppm, ok := d.scaled(6)
	if !ok || ppm > OnePPM {
		// Reachable only through a rounding of a value just under 1.0; the guard
		// above has already excluded anything genuinely out of range.
		return 0, ErrOverflow
	}
	return int32(ppm), nil
}
