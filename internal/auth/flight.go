package auth

import "sync"

// flight coalesces concurrent store lookups for the same index key into one
// call. It is a minimal singleflight: the package takes no dependencies, and
// the general implementation's extra features (Forget, DoChan, panic
// propagation shaping) are not needed here.
//
// The leader's work must not be bound to the leader's context — see
// Authenticator.fetch, which detaches it — because a follower's result would
// otherwise depend on whether an unrelated caller cancelled first.
type flight struct {
	mu sync.Mutex
	m  map[Lookup]*flightCall
}

type flightCall struct {
	done chan struct{}
	e    *entry
	err  error
}

// do runs fn once for k, however many callers arrive while it is running.
// It reports whether this caller waited on someone else's call.
func (f *flight) do(k Lookup, fn func() (*entry, error)) (*entry, error, bool) {
	f.mu.Lock()
	if c, ok := f.m[k]; ok {
		f.mu.Unlock()
		<-c.done
		return c.e, c.err, true
	}
	c := &flightCall{done: make(chan struct{})}
	if f.m == nil {
		f.m = make(map[Lookup]*flightCall)
	}
	f.m[k] = c
	f.mu.Unlock()

	defer func() {
		if c.e == nil && c.err == nil {
			// fn panicked. Followers must not see (nil, nil).
			c.err = refuse(ReasonUnavailable, "", "lookup did not complete")
		}
		f.mu.Lock()
		delete(f.m, k)
		f.mu.Unlock()
		close(c.done)
	}()
	c.e, c.err = fn()
	return c.e, c.err, false
}

// inFlight reports how many lookups are running. Diagnostics only.
func (f *flight) inFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.m)
}
