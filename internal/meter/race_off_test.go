//go:build !race

package meter

// raceEnabled reports whether this build has the race detector on, so the
// overhead gate can say which mode produced its numbers instead of quietly
// reporting instrumented timings as if they were real ones.
const raceEnabled = false
