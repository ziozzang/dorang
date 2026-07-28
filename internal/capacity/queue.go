package capacity

import (
	"container/heap"
	"time"
)

// waiterState is guarded by Broker.mu.
type waiterState uint8

const (
	stateWaiting waiterState = iota
	stateGranted
	stateCancelled
	stateFailed
)

// grant is what a waiter receives exactly once.
type grant struct {
	res *Reservation
	err error
}

// waiter is one blocked Acquire. It records the full request so a release can
// re-run the whole multi-axis check on its behalf and hand it a finished
// reservation ("targeted wakeup") instead of waking it to re-race.
type waiter struct {
	// seq is the arrival sequence and the sole priority key. It is assigned
	// once, on first enqueue, and preserved across every re-enqueue, so a
	// waiter's effective priority rises monotonically with total wait time.
	seq uint64
	// enqueued is the original arrival time, kept for observability. Ordering
	// uses seq, which is a total order consistent with enqueued.
	enqueued time.Time

	req   Request
	ch    chan grant
	state waiterState

	// at holds this waiter's position in every queue it currently sits in. A
	// Spill waiter may block on a different axis per candidate and must be
	// woken by whichever of them frees first.
	at []*qnode

	// pass marks the release pass that already probed this waiter, so one
	// release never probes the same waiter twice.
	pass uint64
}

// nodeFor returns this waiter's membership in bk's queue, or nil if it is not
// in that queue. The scan is over at most maxBlockAxes entries.
func (w *waiter) nodeFor(bk *bucket) *qnode {
	for _, nd := range w.at {
		if nd.bk == bk {
			return nd
		}
	}
	return nil
}

// qnode is a waiter's membership in one axis queue. It carries its own heap
// index so removal is O(log n) rather than a scan, and a direct pointer to its
// bucket so nothing on the release path needs a map lookup.
type qnode struct {
	w   *waiter
	bk  *bucket
	idx int
}

// waitQueue is a per-axis FIFO ordered by arrival sequence. It is a heap rather
// than a list because a re-enqueued waiter carries its original sequence and so
// may need to be inserted ahead of waiters already queued.
type waitQueue struct {
	n []*qnode
}

func (q *waitQueue) Len() int { return len(q.n) }

func (q *waitQueue) Less(i, j int) bool { return q.n[i].w.seq < q.n[j].w.seq }

func (q *waitQueue) Swap(i, j int) {
	q.n[i], q.n[j] = q.n[j], q.n[i]
	q.n[i].idx = i
	q.n[j].idx = j
}

func (q *waitQueue) Push(x any) {
	nd := x.(*qnode)
	nd.idx = len(q.n)
	q.n = append(q.n, nd)
}

func (q *waitQueue) Pop() any {
	old := q.n
	last := len(old) - 1
	nd := old[last]
	old[last] = nil
	q.n = old[:last]
	nd.idx = -1
	return nd
}

// head returns the oldest waiter without removing it.
func (q *waitQueue) head() *waiter {
	if len(q.n) == 0 {
		return nil
	}
	return q.n[0].w
}

func (q *waitQueue) push(nd *qnode) { heap.Push(q, nd) }

func (q *waitQueue) remove(nd *qnode) {
	if nd.idx < 0 || nd.idx >= len(q.n) || q.n[nd.idx] != nd {
		return
	}
	heap.Remove(q, nd.idx)
}
