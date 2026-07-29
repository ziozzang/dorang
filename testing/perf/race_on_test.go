//go:build race

package perf

// raceEnabled reports whether this build has the race detector on.
//
// Every bound in this package is a wall-clock bound, and the race detector
// multiplies wall clock by three to five. A gate that asserted anyway would
// fail `go test -race ./...` on a correct gateway, and the usual repair —
// widening the bound until both modes pass — makes it useless in the mode that
// matters. So the numbers are still measured and still printed under -race, and
// only the assertions are withheld, with the mode named in the output so nobody
// quotes an instrumented figure as a real one.
const raceEnabled = true
