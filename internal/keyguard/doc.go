// Package keyguard is DESIGN §11.6's token guard: it watches a key against its
// OWN baseline and acts when the key departs from it.
//
// A fixed threshold is wrong for every key except the one it was set for, so
// the guard compares a key's observed rate against the rate that key has
// historically run at. A key whose usage suddenly departs from its own history
// is either compromised, looping, or newly popular, and the first two are
// expensive.
//
// # Four things this gets right, because an automated revocation is itself a denial of service
//
//  1. [ActionPend] is the default, not [ActionRevoke]. A pended key is refused
//     with a distinct, documented error ([auth.ReasonPended]) and released by an
//     operator in ONE action — [Guard.Release]. Automatic revocation of a key
//     that turns out to be legitimately busy is an outage the operator did not
//     choose, and it is not reversible in the same sense: the caller has to be
//     reissued a credential.
//
//  2. A relative AND an absolute condition must both hold. A key that used 10
//     tokens yesterday and 200 today has grown twentyfold and is not a problem.
//     Without the absolute floor the guard fires hardest on the quietest keys,
//     which is the exact opposite of what it is for. [Finding.Condition] names
//     which conditions held, and [Finding.Tripped] is the conjunction.
//
//  3. A new key has no baseline. Below [Config.MinHistory] the guard only
//     alerts, whatever the action is configured to be, or every key trips on its
//     first busy hour.
//
//  4. Every action announces itself through internal/notify, with the observed
//     rate, the baseline, and which condition tripped — so the first thing the
//     operator learns is not a support ticket from the affected user. An alert
//     is an action for this purpose. So is a release.
//
// # The guard is not a budget
//
// A budget is a STATED ceiling the caller agreed to; the guard is a STATISTICAL
// judgement that might be wrong. They fail differently and they are configured
// separately, and the guard's action is deliberately the reversible one.
//
// # Pending has to actually stop the key
//
// §11.2c: a guard that pends a key but leaves it serving for a cache TTL has
// not stopped anything — it has only started a timer. [Enforcer] is therefore
// two operations and not one: the durable state change, and the invalidation
// that makes every node drop the key now. The tests in this package assert that
// the key STOPS SERVING, not that a flag was set; a test that asserted the flag
// would pass against an implementation with a sixty-second hole in it.
//
// # History
//
// The guard needs a rate now and a rate over a baseline window. [History] is
// that, as an interface, and this package ships [MemHistory] — bucketed,
// bounded, in-process — the way internal/cluster ships MemRedis. A single-node
// deployment is exact with it. In a cluster each node sees its own share of the
// traffic, so a store-backed History is what makes the baseline the fleet's
// rather than the node's; the interface is the seam for that, and [MemHistory]
// says so rather than pretending otherwise.
package keyguard
