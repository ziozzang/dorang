//go:build !race

package perf

// raceEnabled reports whether this build has the race detector on. See
// race_on_test.go for why the gates consult it.
const raceEnabled = false
