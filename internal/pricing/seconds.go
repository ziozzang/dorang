package pricing

// This file is the one place in the package that is allowed to name float64, and it
// contains no arithmetic on one. Request.Seconds is a measured quantity handed to us by
// the metering layer; converting it to an exact decimal here keeps binary floating point
// out of the price path entirely (§8.3). TestNoFloatInPricePath enforces the boundary.

import (
	"errors"
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
