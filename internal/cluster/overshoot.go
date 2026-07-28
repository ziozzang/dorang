package cluster

import (
	"fmt"

	"github.com/ziozzang/dorang/internal/quota"
)

// Mode is a capacity and quota coordination mode. It is [quota.Mode] under
// another name rather than a second enumeration, because DESIGN 6.3's whole
// point is that quota and concurrency use one accuracy vocabulary and not two.
type Mode = quota.Mode

// The modes of DESIGN 5.6.
const (
	ModeLocal       = quota.ModeLocal
	ModeSharedRedis = quota.ModeSharedRedis
	ModeSharedPG    = quota.ModeSharedPG
	ModeLeased      = quota.ModeLeased
)

// ParseMode decodes a configured cluster.capacity_mode value.
func ParseMode(s string) (Mode, error) { return quota.ParseMode(s) }

// Modes returns every implemented mode, in configuration order. A test
// iterates it, so a mode added without a published overshoot figure fails to
// compile a claim rather than shipping without one.
func Modes() []Mode { return []Mode{ModeLocal, ModeSharedRedis, ModeSharedPG, ModeLeased} }

// DefaultMinLeasable is the smallest limit the leased mode will divide across
// nodes. It matches cluster.min_leasable's default in internal/config.
const DefaultMinLeasable int64 = 16

// DefaultBlockSize is how many units a node leases at a time under the leased
// mode, and how much budget [Ledger] draws per store write.
const DefaultBlockSize int64 = 16

// Params describes the deployment a published accuracy figure is about.
type Params struct {
	// Limit is the ceiling being coordinated, in the metric's own units.
	Limit int64
	// Nodes is how many nodes share it. Below 1 is treated as 1.
	Nodes int
	// Block is the lease size for the leased mode. Zero means
	// [DefaultBlockSize].
	Block int64
	// MinLeasable is the smallest divisible limit. Zero means
	// [DefaultMinLeasable].
	MinLeasable int64
	// Clustered mirrors cluster.enabled.
	Clustered bool
}

func (p Params) fill() Params {
	if p.Nodes < 1 {
		p.Nodes = 1
	}
	p.Block = orInt64(p.Block, DefaultBlockSize)
	p.MinLeasable = orInt64(p.MinLeasable, DefaultMinLeasable)
	return p
}

// Accuracy is a mode's published accuracy.
//
// DESIGN 5.6: "Every mode publishes its maximum possible overshoot as a number.
// 'Approximately accurate' is not an acceptable specification." This type is
// that number, together with the arithmetic that produced it, so that the
// figure can be recomputed by a reader rather than trusted.
type Accuracy struct {
	// Mode is the mode described.
	Mode Mode
	// MaxOvershoot is the largest number of units that all nodes together can
	// admit beyond Limit. Zero means exact.
	MaxOvershoot int64
	// Formula is the arithmetic, with the parameters substituted.
	Formula string
	// Why states the mechanism the number comes from.
	Why string
	// HotPathCost describes what a request pays for this accuracy.
	HotPathCost string
}

// String renders the figure for a log line or a status page.
func (a Accuracy) String() string {
	return fmt.Sprintf("%s: max overshoot %d (%s); %s", a.Mode, a.MaxOvershoot, a.Formula, a.HotPathCost)
}

// Publish returns a mode's maximum possible overshoot for a deployment.
//
// It returns an error when the mode cannot coordinate the limit at all: local
// under cluster.enabled ([ErrLocalInCluster]), or a limit too small to divide
// under leased ([ErrLimitTooSmall]). Those are refusals, not warnings -- a mode
// that cannot honour a limit must say so at configuration time rather than
// overshoot at run time and let the consequence surface somewhere else.
func Publish(mode Mode, p Params) (Accuracy, error) {
	p = p.fill()
	n := int64(p.Nodes)

	switch mode {
	case ModeLocal:
		if p.Clustered {
			return Accuracy{}, ErrLocalInCluster
		}
		// Every node carries the whole limit, so N nodes admit N x limit.
		return Accuracy{
			Mode:         ModeLocal,
			MaxOvershoot: p.Limit * (n - 1),
			Formula:      fmt.Sprintf("limit x (nodes - 1) = %d x %d", p.Limit, n-1),
			Why: "each node counts only its own traffic, so every ceiling is counted once per node. " +
				"Exact on one node, which is the only configuration in which it is permitted",
			HotPathCost: "none: an in-process counter",
		}, nil

	case ModeSharedRedis:
		return Accuracy{
			Mode:         ModeSharedRedis,
			MaxOvershoot: 0,
			Formula:      "0",
			Why: "the read, the ceiling test and the write are one atomic script, so two nodes " +
				"cannot both see room for the last unit",
			HotPathCost: "one round trip to Redis per acquire",
		}, nil

	case ModeSharedPG:
		return Accuracy{
			Mode:         ModeSharedPG,
			MaxOvershoot: 0,
			Formula:      "0",
			Why: "the read-modify-write happens inside one transaction, serialized against every " +
				"other transaction on the same key by an advisory lock",
			HotPathCost: "one round trip to the store per acquire, costlier than Redis",
		}, nil

	case ModeLeased:
		if p.Limit < p.MinLeasable {
			return Accuracy{}, fmt.Errorf("%w: limit %d is below min_leasable %d",
				ErrLimitTooSmall, p.Limit, p.MinLeasable)
		}
		// Blocks are drawn from the shared authority, so while every lease is
		// live the units handed out cannot exceed the limit. Overshoot comes
		// from expiry: a lease reclaimed while its holder is still spending
		// against it is counted in neither place. At most one such block per
		// other node can be outstanding at a time, because a node holds one
		// lease per key.
		return Accuracy{
			Mode:         ModeLeased,
			MaxOvershoot: p.Block * (n - 1),
			Formula:      fmt.Sprintf("block x (nodes - 1) = %d x %d", p.Block, n-1),
			Why: "blocks come from the shared authority, so live leases never exceed the limit; " +
				"the bound is one reclaimed-but-still-spending block per other node. This " +
				"implementation never spends against an expired lease, so the figure is an " +
				"upper bound and not an estimate",
			HotPathCost: fmt.Sprintf("one round trip per %d units, none in between", p.Block),
		}, nil
	}
	return Accuracy{}, fmt.Errorf("%w: %v", ErrUnknownMode, mode)
}

// PublishAll returns the accuracy of every mode that can serve a deployment,
// and the reason each refused mode refused. It is what a status page renders
// and what an operator reads before choosing.
func PublishAll(p Params) (ok []Accuracy, refused map[Mode]error) {
	refused = map[Mode]error{}
	for _, m := range Modes() {
		a, err := Publish(m, p)
		if err != nil {
			refused[m] = err
			continue
		}
		ok = append(ok, a)
	}
	return ok, refused
}
