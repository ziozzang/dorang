package pricing

import (
	"testing"
	"time"
)

// The feedback loop between routing and utilization pricing, driven long enough to show
// whether it is there.
//
// The hazard is real and worth stating precisely. `least_busy` routes AWAY from occupancy;
// this feature charges MORE for it. Put a cost-based router on top and the two become one
// loop: traffic moves to the cheap backend, the cheap backend fills, its price rises past
// its neighbour's, traffic swings back, and the cycle repeats with a delay equal to
// however long occupancy takes to respond. internal/quota's ranker has damping and jitter
// written for exactly this shape.
//
// Neither is used here, because the loop is OPEN. A routing quote is priced before the
// request runs, and the occupancy it will run at is not knowable then — the same argument
// [NoPriceComputeNotMeasured] already makes about the same instant for wall time. So a
// quote carries no observation, prices at 1.0x, and the factor contributes exactly nothing
// to any routing decision.
//
// [TestUtilizationPricingChangesNoRoutingDecision] asserts that as an identity: the same
// simulation, run with and without the utilization block, produces the same decisions tick
// for tick. [TestTheClosedLoopWouldOscillate] closes the loop deliberately and measures
// what the design avoids, so the claim is a measurement rather than an assertion.

const (
	simTicks = 400
	// simDecay is how much of a backend's occupancy survives a tick with no traffic,
	// and simBump how much a tick of traffic adds. The pair sets the loop's delay: an
	// occupancy that responded instantly could not oscillate, and one that never
	// responded could not either.
	simDecay = 0.80
	simBump  = 0.45
)

// simBackends are three deployments of one model, one of which is materially cheaper. The
// price difference is what makes a cost router's decision interesting: with equal rates
// every router in this file reduces to least_busy.
//
// The ratio is chosen so the crossing is REACHABLE. At a ceiling of 2.0x the cheap backend
// tops out at $0.001000/s against the others' $0.000800/s, so a saturated cheap backend is
// genuinely dearer than an idle expensive one and a cost router has a reason to move. Had
// the cheap rate been exactly half, the two would only ever tie and the loop would never
// close — which is worth saying because a simulation that cannot oscillate proves nothing
// about a design that avoids oscillation.
var simBackends = []struct {
	deployment string
	rate       string
}{
	{"cheap", "0.000500"},
	{"dear-a", "0.000800"},
	{"dear-b", "0.000800"},
}

func simCatalog(t *testing.T, withUtilization bool) *Catalog {
	t.Helper()
	y := "currency: USD\nrules:\n"
	for _, b := range simBackends {
		y += "  - id: " + b.deployment + "\n" +
			"    class: marginal_usage\n" +
			"    match: { deployment: " + b.deployment + " }\n" +
			"    unit: per_compute_second\n" +
			"    compute_seconds: \"" + b.rate + "\"\n"
		if withUtilization {
			y += "    utilization:\n      slope: \"1.0\"\n      max_multiplier: \"2.0\"\n"
		}
	}
	return mustCatalog(t, y)
}

// simRun drives the fleet for simTicks and returns the deployment chosen on each tick and
// the amount settled for it.
//
// closedLoop is the whole experiment. When false the router quotes each candidate the way
// internal/app does — with no observation, because a request that has not run has no
// occupancy — which is the design. When true the router is handed the CURRENT occupancy of
// each candidate as though it were a measurement of the request it is about to send, which
// is the loop the design avoids.
func simRun(t *testing.T, c *Catalog, closedLoop bool) (picks []string, charged []int64) {
	t.Helper()
	stamp := at(t, "2026-08-03T12:00:00Z")
	occ := make([]float64, len(simBackends))
	picks = make([]string, 0, simTicks)
	charged = make([]int64, 0, simTicks)

	for tick := 0; tick < simTicks; tick++ {
		// Route: the cheapest quote wins, ties to the lowest index. This is dorang's
		// cost strategy, which reads Cost.MarginalNano and only that (§8.1).
		best, bestCost := 0, int64(-1)
		for i := range simBackends {
			req := Request{
				Deployment: simBackends[i].deployment, Seconds: 1, At: stamp,
			}
			if closedLoop {
				ppm, err := UtilizationFromFraction(occ[i])
				if err != nil {
					t.Fatal(err)
				}
				req.UtilizationPPM, req.UtilizationObserved = ppm, true
			}
			cost, err := c.Price(req)
			if err != nil {
				t.Fatal(err)
			}
			if bestCost < 0 || cost.MarginalNano < bestCost {
				best, bestCost = i, cost.MarginalNano
			}
		}

		// The fleet responds: every backend drains, the chosen one fills.
		for i := range occ {
			occ[i] *= simDecay
		}
		occ[best] += simBump
		if occ[best] > 1 {
			occ[best] = 1
		}

		// Settle at the occupancy the request actually ran at. This is where the
		// factor applies, and it is the only place it applies.
		ppm, err := UtilizationFromFraction(occ[best])
		if err != nil {
			t.Fatal(err)
		}
		settled, err := c.Price(Request{
			Deployment: simBackends[best].deployment, Seconds: 1, At: stamp,
			UtilizationPPM: ppm, UtilizationObserved: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		picks = append(picks, simBackends[best].deployment)
		charged = append(charged, settled.MarginalNano)
	}
	return picks, charged
}

// switches counts how often the chosen deployment changed over the tail of a run. It is
// the direct measure of flapping: a stable fleet changes rarely, an oscillating one
// changes on a fixed period.
func switches(picks []string) int {
	tail := picks[len(picks)/2:]
	n := 0
	for i := 1; i < len(tail); i++ {
		if tail[i] != tail[i-1] {
			n++
		}
	}
	return n
}

// TestUtilizationPricingChangesNoRoutingDecision is the loop, closed by identity.
//
// Two runs of the same fleet under the same router: one on a catalog that prices on
// utilization, one on a catalog that does not. If the factor had any gain in the routing
// loop the two trajectories would separate, and they cannot separate a little — a single
// different pick at tick k changes every occupancy after it.
func TestUtilizationPricingChangesNoRoutingDecision(t *testing.T) {
	withFactor, chargedWith := simRun(t, simCatalog(t, true), false)
	without, chargedWithout := simRun(t, simCatalog(t, false), false)

	for i := range withFactor {
		if withFactor[i] != without[i] {
			t.Fatalf("tick %d: routing chose %q with utilization pricing on and %q with it "+
				"off. The factor has gain in the routing loop, which is the oscillation "+
				"damping would then be needed for", i, withFactor[i], without[i])
		}
	}

	// And the feature is not inert: the same decisions, different money. Without this
	// half the test above would also pass on a factor that never applied.
	var moved int
	for i := range chargedWith {
		if chargedWith[i] != chargedWithout[i] {
			moved++
		}
		if chargedWith[i] < chargedWithout[i] {
			t.Fatalf("tick %d: the factor made a request CHEAPER (%d < %d); its range is "+
				"[1.0, max_multiplier] and cannot point downwards",
				i, chargedWith[i], chargedWithout[i])
		}
	}
	if moved < simTicks/2 {
		t.Fatalf("only %d of %d settled charges moved; the factor is barely applying and "+
			"the identity above proves nothing", moved, simTicks)
	}
}

// TestTheClosedLoopWouldOscillate measures the hazard the design avoids, by building it.
//
// The router is handed each candidate's current occupancy as though it were an observation
// of the request it is about to send. That is the only change; the fleet, the rates and the
// dynamics are identical. The cheap backend is now cheap only while it is idle, so traffic
// piles onto it, its price passes its neighbours', traffic leaves, it drains, and the cycle
// runs for as long as the simulation does.
func TestTheClosedLoopWouldOscillate(t *testing.T) {
	c := simCatalog(t, true)
	open, _ := simRun(t, c, false)
	closed, _ := simRun(t, c, true)

	openSwitches, closedSwitches := switches(open), switches(closed)
	if closedSwitches <= openSwitches {
		t.Fatalf("the closed loop switched backends %d times and the open loop %d: the "+
			"simulation is not exercising the loop, so the comparison says nothing",
			closedSwitches, openSwitches)
	}
	if openSwitches != 0 {
		t.Errorf("the open loop switched %d times in its second half. With no observation "+
			"in the quote every tick's ranking is identical, so it should settle on one "+
			"deployment and stay there", openSwitches)
	}
	t.Logf("flapping over the last %d ticks: open loop %d switches, closed loop %d",
		simTicks/2, openSwitches, closedSwitches)

	// The other half of what the closed loop costs: it moves traffic onto the EXPENSIVE
	// backends, because their price is the one that looks better once the cheap one is
	// loaded. A router built to minimise cost ends up maximising it.
	countDear := func(picks []string) int {
		n := 0
		for _, p := range picks[len(picks)/2:] {
			if p != "cheap" {
				n++
			}
		}
		return n
	}
	if countDear(closed) <= countDear(open) {
		t.Fatalf("the closed loop sent %d requests to a double-priced backend and the open "+
			"loop %d; the loop is not doing what it is here to demonstrate",
			countDear(closed), countDear(open))
	}
}

// TestThePublishedCeilingHoldsAcrossTheWholeRun. A ceiling asserted at one occupancy is a
// ceiling asserted where it is easy. This drives the fleet to saturation and checks every
// charge against the number DESIGN §5.6 requires be published:
//
//	most chargeable  =  base rate x max_multiplier  =  base rate x 2.000
func TestThePublishedCeilingHoldsAcrossTheWholeRun(t *testing.T) {
	c := simCatalog(t, true)
	for _, closedLoop := range []bool{false, true} {
		picks, charged := simRun(t, c, closedLoop)
		for i := range charged {
			// The base charge for one compute-second at this deployment's rate.
			var base int64
			for _, b := range simBackends {
				if b.deployment == picks[i] {
					cost, err := c.Price(Request{Deployment: b.deployment, Seconds: 1,
						At: at(t, "2026-08-03T12:00:00Z")})
					if err != nil {
						t.Fatal(err)
					}
					base = cost.MarginalNano
				}
			}
			if charged[i] < base {
				t.Fatalf("closedLoop=%v tick %d: charged %d below the base rate %d",
					closedLoop, i, charged[i], base)
			}
			if ceiling := 2 * base; charged[i] > ceiling {
				t.Fatalf("closedLoop=%v tick %d on %s: charged %d nano ($%s), above the "+
					"published ceiling of %d ($%s) = base %d x 2.000",
					closedLoop, i, picks[i], charged[i], usd9(charged[i]),
					ceiling, usd9(ceiling), base)
			}
		}
	}
}

// TestSettlementIsNotSensitiveToWhenTheCatalogIsAsked. A price that moves is one a caller
// cannot predict, so the one thing it must not also be is a price that moves for reasons
// the row does not record. The same request settled twice from the same observation is the
// same number, whatever the wall clock did in between.
func TestSettlementIsNotSensitiveToWhenTheCatalogIsAsked(t *testing.T) {
	c := simCatalog(t, true)
	req := Request{
		Deployment: "cheap", Seconds: 1, UtilizationPPM: 613_247, UtilizationObserved: true,
		At: at(t, "2026-08-03T12:00:00Z"),
	}
	first := mustPrice(t, c, req)
	for i := 0; i < 8; i++ {
		req.At = req.At.Add(time.Duration(i) * time.Hour)
		again := mustPrice(t, c, req)
		if again.MarginalNano != first.MarginalNano ||
			again.UtilizationMultiplierPPM != first.UtilizationMultiplierPPM {
			t.Fatalf("repricing the same observation gave %d nano at %sx, first gave %d at %sx: "+
				"an invoice line has to be re-derivable from the row that recorded it",
				again.MarginalNano, formatPPM(again.UtilizationMultiplierPPM),
				first.MarginalNano, formatPPM(first.UtilizationMultiplierPPM))
		}
	}
}
