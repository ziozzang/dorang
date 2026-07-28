package auth

import (
	"errors"
	"sync"
)

// errFlightAborted is what a follower sees when the leader's call did not
// complete, which can only happen if it panicked. Followers must never be
// handed a zero value with no error: for a lookup that would read as "no such
// key", and for a token refresh it would read as "here is your new token".
var errFlightAborted = errors.New("auth: coalesced call did not complete")

// flight coalesces concurrent calls for the same key into one. It is a minimal
// singleflight: the package takes no dependencies, and the general
// implementation's extra features (Forget, DoChan, panic propagation shaping)
// are not needed here.
//
// Two callers use it, for two different reasons. Credential lookup coalesces to
// save the store a round trip. OAuth refresh coalesces because a stampede is
// dangerous rather than merely wasteful: some providers invalidate the previous
// refresh token when it is used, so two concurrent refreshes of one credential
// can lock the account out (DESIGN §11.2b).
//
// The leader's work must not be bound to the leader's context — see
// Authenticator.fetch, which detaches it — because a follower's result would
// otherwise depend on whether an unrelated caller cancelled first.
//
// The zero flight is ready to use.
type flight[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]*flightCall[V]
}

type flightCall[V any] struct {
	done chan struct{}
	v    V
	err  error
	// ok distinguishes "fn returned a zero value" from "fn never returned".
	// Only the leader touches it, before close(done) publishes the result.
	ok bool
}

// do runs fn once for k, however many callers arrive while it is running.
// It reports whether this caller waited on someone else's call.
func (f *flight[K, V]) do(k K, fn func() (V, error)) (V, error, bool) {
	f.mu.Lock()
	if c, ok := f.m[k]; ok {
		f.mu.Unlock()
		<-c.done
		return c.v, c.err, true
	}
	c := &flightCall[V]{done: make(chan struct{})}
	if f.m == nil {
		f.m = make(map[K]*flightCall[V])
	}
	f.m[k] = c
	f.mu.Unlock()

	defer func() {
		if !c.ok {
			// fn panicked.
			var zero V
			c.v, c.err = zero, errFlightAborted
		}
		f.mu.Lock()
		delete(f.m, k)
		f.mu.Unlock()
		close(c.done)
	}()
	c.v, c.err = fn()
	c.ok = true
	return c.v, c.err, false
}

// inFlight reports how many calls are running. Diagnostics only.
func (f *flight[K, V]) inFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.m)
}
