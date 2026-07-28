package openai

import "testing"

// testingAllocsPerRun is testing.AllocsPerRun, named locally so the intent of
// each call site reads as "this must not allocate" rather than as a benchmark.
func testingAllocsPerRun(runs int, f func()) float64 {
	return testing.AllocsPerRun(runs, f)
}
