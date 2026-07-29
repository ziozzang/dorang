// Package meter records what a request cost and what it did, on two
// independent paths, because the two have irreconcilable requirements.
//
// # Why two paths
//
// Revision 1 of the design had one ring buffer and dropped the ledger when it
// filled, while claiming rollups stayed accurate. Both halves were wrong
// (DESIGN 12.1, REVIEW R1-B metering findings 1 and 2). Dropping the ledger
// loses exactly the per-request tracing the requirements ask for, and keeping
// multi-dimensional rollups accurate requires touching a keyed aggregate,
// which is not the free stack-allocated operation that was assumed. A single
// queue cannot be both never-lossy and cheap, so there are two:
//
//	request completes
//	   |-- numeric event  -> per-CPU fixed-cardinality accumulator  (ALWAYS kept)
//	   |                     merged and flushed to rollups
//	   \-- trace payload  -> bounded queue -> durable local spool -> ledger
//	                         (sampled; droppable, and the drop is VISIBLE)
//
// # Numeric path
//
// Counters are sharded across padded per-CPU shards and keyed by the
// fixed-cardinality tuple (api_key_id, team_id, model_group, provider,
// credential_id, endpoint, status_class) plus the UTC hour the event lands in.
// Distinct keys per shard are capped; beyond the cap an event folds into a
// single OverflowSentinel key and the fold is counted, so the loss of
// resolution is visible rather than inferred. Shards are merged on flush.
//
// Nothing on this path is droppable. If the sink refuses a batch of rollups
// the merged buckets are carried over and merged into the next flush, so a
// store outage costs latency in the reporting surface and never a counter.
// Carry-over is itself bounded: past Config.MaxPendingBuckets the tail folds
// into per-hour overflow buckets, again counted.
//
// # Trace path
//
// Trace payloads go through a bounded lock-free ring, then to a durable local
// spool on disk, then to the sink. The spool is what makes a store stall cost
// disk instead of data. It is append-only segment files under a directory with
// a total size cap; on restart un-drained segments are replayed before new
// writes, and a torn trailing record from a crash is truncated rather than
// rejected.
//
// Draining (ring to spool) and shipping (spool to sink) are separate
// goroutines with separate locks precisely so a blocked sink cannot stop the
// ring from emptying onto disk.
//
// When the spool is also full, traces drop -- but never silently. Drops are
// counted in Stats and raise Degraded, which the health and metrics surface
// reports as metering_degraded (DESIGN 12.3). Sampling and byte-budget
// exclusions are counted separately and do NOT raise Degraded: they are the
// configured policy working, not a failure.
//
// The numeric path loses a count in exactly one place, and it is counted there
// too: an event handed to a meter that has already been closed. Close runs the
// last flush there will ever be, so a Record after it has nowhere to land; it is
// refused, counted in Stats.RecordsRefusedClosed, and raises Degraded with
// ReasonClosed. The window is shutdown, where the drain races this meter's own
// close, which is precisely where a silent drop would be hardest to notice.
//
// # Cost on the hot path
//
// Record must not block, must not allocate, and must not take a contended
// lock. It holds to that:
//
//   - Counter storage is preallocated. Each shard owns an arena of counters
//     and a free list; a new key takes a slot from the free list rather than
//     allocating one. The key map is created with the cap as its size hint and
//     entries are evicted only after Config.EvictAfterFlushes idle flushes, so
//     the steady-state working set never grows the map.
//   - Trace payloads are copied into a preallocated ring slot, and the excerpt
//     into a preallocated byte arena. Nothing is allocated and no caller buffer
//     is retained past the copy.
//   - Shard selection uses a goroutine-affinity hint (the address of a stack
//     local: concurrent goroutines have distinct stacks) and then TryLock
//     fan-out. Correctness never depends on the hint -- a degenerate hint
//     costs a fan-out, not a wrong answer -- and the atomic rotation counter is
//     touched only after a collision, so the uncontended path executes no
//     atomic RMW on shared memory at all. The final fallback is a blocking Lock
//     on a shard, reachable only when every shard is simultaneously held inside
//     a critical section of a few tens of nanoseconds.
//
// TestRecordZeroAlloc asserts the allocation claim with testing.AllocsPerRun
// rather than restating it, and BenchmarkRecordOn/BenchmarkRecordOff measure
// the DESIGN 15.1 "metering on vs off < 5%" target in steady state and at a
// full buffer.
//
// # Sink
//
// Sink is deliberately narrow so that internal/store can implement it without
// either package importing the other.
package meter
