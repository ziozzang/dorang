package capacity

// Test-only accessors into broker internals. They exist so tests can assert
// that cancellation and grants leave no residue in the wait queues, which
// Snapshot alone cannot prove (Snapshot reports waiters, not queue nodes, and
// one waiter may hold several nodes).

// queueLen is the number of queue nodes on one axis key.
func (b *Broker) queueLen(k axisKey) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bk := b.buckets[k]; bk != nil {
		return bk.q.Len()
	}
	return 0
}

// inUseOf is the committed count on one axis key, excluding any unit held idle
// by a soft reservation.
func (b *Broker) inUseOf(k axisKey) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bk := b.buckets[k]; bk != nil {
		return bk.committed()
	}
	return 0
}

// totalQueueNodes is the number of queue nodes across every axis. A waiter that
// queued on several axes contributes one node per axis, so this returning zero
// is a stronger statement than Snapshot().Waiting == 0.
func (b *Broker) totalQueueNodes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, bk := range b.buckets {
		n += bk.q.Len()
	}
	return n
}

// liveWaiters is the number of blocked Acquire calls the broker still tracks.
func (b *Broker) liveWaiters() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.waiters)
}

// wakeupCount is the cumulative number of head-of-queue probes.
func (b *Broker) wakeupCount() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wakeups
}

// softReservedOn reports whether one unit of an axis key is being held idle for
// a waiter. Reading it straight after a Release is deterministic: the grant, or
// the soft reservation that absorbed the freed unit, happens inside Release
// under the broker lock.
func (b *Broker) softReservedOn(k axisKey) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	bk := b.buckets[k]
	return bk != nil && bk.claimant != nil
}

// totalClaims is the number of outstanding soft reservations. A drained broker
// must report zero: a claim is capacity kept idle, and leaking one leaks
// capacity just as surely as leaking a reservation would.
func (b *Broker) totalClaims() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.softReservedLocked()
}
