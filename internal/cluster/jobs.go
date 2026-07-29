package cluster

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

// Job is one piece of periodic work the leader owns.
//
// DESIGN 13 lists them: rollup compaction, partition maintenance, expiry sweeps
// for capacity reservations and budget reservations, batch assignment, and
// lease rebalancing. Two of those live in packages this one must not import, so
// they arrive as jobs rather than as dependencies -- which also means a
// deployment can add its own without this package growing a case for it.
type Job struct {
	// Name identifies the job in logs and in [Node.JobStats].
	Name string
	// Every is how often it runs. Zero means every tick.
	Every time.Duration
	// Run does the work. It must honour ctx: the context is cancelled when this
	// node stops leading, and a job that ignores it is a job that will run
	// concurrently with its successor's copy.
	//
	// ctx also carries this term's fencing token ([FenceFrom]). A job whose
	// writes go through [Ledger] or [LeaseStore] is fenced by that without
	// asking. A job that opens its own transaction somewhere else is checked
	// once, before it is dispatched, and not again -- so a handover landing
	// after that check and before its commit is not caught, and closing that
	// gap means putting [Fence.In] in its own transaction.
	//
	// One thing ctx does NOT do is expire on its own. Demotion by the passage
	// of time is observed when something asks ([Election.IsLeader],
	// [Election.Leader], [Election.Campaign]), and a single job that runs for
	// longer than the safe window blocks the tick that would have asked. A job
	// that can run that long should take its own deadline, or check the fence
	// itself as it goes.
	Run func(ctx context.Context) error
}

// JobStat is one job's history.
type JobStat struct {
	Runs     int
	Failures int
	LastRun  time.Time
	LastErr  error
}

type jobState struct {
	job Job

	mu    sync.Mutex
	last  time.Time
	stats JobStat
}

func (j *jobState) due(now time.Time) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.last.IsZero() || !now.Before(j.last.Add(j.job.Every))
}

func (j *jobState) run(ctx context.Context, now time.Time) error {
	j.mu.Lock()
	j.last = now
	j.mu.Unlock()

	err := j.job.Run(ctx)

	j.mu.Lock()
	j.stats.Runs++
	j.stats.LastRun = now
	j.stats.LastErr = err
	if err != nil {
		j.stats.Failures++
	}
	j.mu.Unlock()
	return err
}

func (j *jobState) stat() JobStat {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stats
}

// MaintenanceJob pre-creates ledger partitions and enforces retention.
//
// DESIGN 9.5 is emphatic that this ships with the writer and not with
// clustering: scheduling them six milestones apart would have failed every
// insert at the first midnight after metering shipped. Clustering restricts the
// job to the leader; it does not introduce it, and this constructor exists to
// restrict it, not to own it.
func MaintenanceJob(s *store.Store, ret store.RetentionPolicy, every time.Duration, now func() time.Time) Job {
	if s == nil {
		return Job{}
	}
	if now == nil {
		now = time.Now
	}
	return Job{
		Name:  "partitions-and-retention",
		Every: every,
		Run: func(ctx context.Context) error {
			_, err := s.Maintain(ctx, now(), ret)
			return err
		},
	}
}

// CapacitySweepJob reclaims capacity reservations that outlived their deadline.
//
// DESIGN 5.3 gives every capacity reservation a deadline so that a panic or a
// leaked goroutine cannot permanently consume capacity. Reaching that deadline
// is the only thing that returns the unit, because the broker's hot path is
// deliberately unable to notice time passing.
//
// # The budget half that used to be here
//
// This was ReservationSweepJob and it swept two kinds, because DESIGN 6.4 R1-19
// gave `budget_state` a `reserved_until` for the same reason capacity has a
// deadline, and a sweep that does one of two identical patterns silently leaks
// the other. That argument was sound and its second half had nothing to sweep:
// `store.ReserveBudget` was the only writer of `reserved_nano` and had no
// non-test caller anywhere in the tree, so `WHERE reserved_nano > 0` was a
// store round trip per tick against a predicate that could not match.
//
// The budget path that runs is the lease block ([Ledger]), and its expiry net
// is [LeaseReclaimJob] — which returns the unspent part of every block lease
// whose TTL has passed, including one held by a node that died mid-request.
// That is the same pattern again, and it is where the budget half of this job
// went rather than being dropped: the two jobs run on the same tick, from the
// same leader, and between them every held-and-abandoned unit in the system has
// something that returns it.
//
// sweepCapacity may be nil on a node with no broker, which leaves no work.
func CapacitySweepJob(sweepCapacity func() int, every time.Duration) Job {
	if sweepCapacity == nil {
		return Job{}
	}
	return Job{
		Name:  "capacity-reservation-sweep",
		Every: every,
		Run: func(context.Context) error {
			sweepCapacity()
			return nil
		},
	}
}

// LeaseReclaimJob is DESIGN 13's lease rebalancing.
//
// It reclaims three things, in an order that matters:
//
//  1. The expired leases of nodes whose heartbeat has lapsed, targeted at the
//     node so a failure is attributable to it.
//  2. Block leases whose TTL has passed, returning their unspent part to the
//     durable counter. Without this a budget shrinks by one block every time a
//     node dies.
//  3. Shared counter leases whose TTL has passed, returning their units.
//
// Every step requires expiry, including the first, which is the correction
// described at [Ledger.ReclaimNode]: a lapsed heartbeat says a node is probably
// gone, and a lapsed lease is the only thing that says it has stopped spending.
// This job used to trust the first, and a two-node cluster with a limit of 100
// then admitted 190 with nobody dead.
//
// Pruning the dead node rows happens last, so that a pass which fails part way
// through has not yet forgotten which node it was reclaiming from.
func LeaseReclaimJob(reg *Registry, ledger *Ledger, shared *LeaseStore, every time.Duration, now func() time.Time) Job {
	if reg == nil && ledger == nil && shared == nil {
		return Job{}
	}
	if now == nil {
		now = time.Now
	}
	return Job{
		Name:  "lease-reclaim",
		Every: every,
		Run: func(ctx context.Context) error {
			var errs []error
			t := now()

			var dead []NodeInfo
			if reg != nil {
				var err error
				dead, err = reg.Dead(ctx)
				if err != nil {
					errs = append(errs, err)
				}
			}
			for _, d := range dead {
				if ledger != nil {
					if _, err := ledger.ReclaimNode(ctx, d.ID, t); err != nil {
						errs = append(errs, err)
					}
				}
				if shared != nil {
					if _, err := shared.ReclaimNode(ctx, d.ID, t); err != nil {
						errs = append(errs, err)
					}
				}
			}
			if ledger != nil {
				if _, err := ledger.ReclaimExpired(ctx, t); err != nil {
					errs = append(errs, err)
				}
			}
			if shared != nil {
				if _, err := shared.ReclaimExpired(ctx, t); err != nil {
					errs = append(errs, err)
				}
			}
			if reg != nil {
				// A generous grace: the row is kept well past the point the
				// leases were reclaimed, because an operator looking at a
				// failure wants to see which node it was.
				if _, err := reg.Prune(ctx, 10*reg.NodeTTL()); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
	}
}

// RollupCompactionJob wraps a caller-supplied compaction pass (DESIGN 9.4).
//
// The compaction itself belongs to the metering path, which owns the in-memory
// pre-aggregation; this only says who runs it. Each node pre-aggregates and
// merges once per flush, so the number of writes touching a given row is
// bounded by node count rather than request count -- and the leader-only pass
// is what keeps a shared bucket from being rewritten by every node at once.
func RollupCompactionJob(every time.Duration, run func(ctx context.Context) error) Job {
	if run == nil {
		return Job{}
	}
	return Job{Name: "rollup-compaction", Every: every, Run: run}
}

// BatchAssignmentJob wraps a caller-supplied batch assignment pass.
//
// DESIGN 9.2 leases a batch job to one node. Assignment is leader work so that
// two nodes cannot pick up the same batch, which would pay for every already
// finished row a second time (DESIGN 9.2, on batch_requests.response_body).
func BatchAssignmentJob(every time.Duration, run func(ctx context.Context) error) Job {
	if run == nil {
		return Job{}
	}
	return Job{Name: "batch-assignment", Every: every, Run: run}
}
