package admin

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
)

// Which administrative mutations announce, and with which cause.
//
// The observable assertion — "the key stops serving on a second node" — lives in
// internal/cluster, where there are two real authenticators and a real
// invalidation transport to observe it with. What is asserted here is the other
// half an enumeration needs: that EVERY route which changes whether a key serves
// reaches the announcement, and that the one route which deliberately does not
// still does not.

type invMsg struct {
	keyID string
	cause string
}

type fakeInvalidator struct {
	mu   sync.Mutex
	msgs []invMsg
	err  error
}

func (f *fakeInvalidator) InvalidateKey(_ context.Context, keyID, cause string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, invMsg{keyID: keyID, cause: cause})
	return nil
}

func (f *fakeInvalidator) taken() []invMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]invMsg(nil), f.msgs...)
	f.msgs = nil
	return out
}

func newInvalidatingHarness(t *testing.T) (*harness, *fakeInvalidator) {
	t.Helper()
	inv := &fakeInvalidator{}
	h := newHarness(t, func(c *Config) { c.Invalidator = inv })
	return h, inv
}

// TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt is the enumeration.
//
// It is one table rather than nine tests because the finding was that the list
// had a hole in it, and a list is the shape that makes the next hole visible: a
// route added below without a row here, or with an empty one, is a route whose
// change lands on the credential cache TTL.
func TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt(t *testing.T) {
	h, inv := newInvalidatingHarness(t)
	id, _ := h.newKey(nil)

	// Creation is the deliberate exception: nothing is cached for a key that has
	// never been seen, and "used before it existed, then created" is bounded by
	// the negative-entry TTL rather than by a message.
	if got := inv.taken(); len(got) != 0 {
		t.Fatalf("/key/generate announced %v; creation deliberately publishes nothing", got)
	}

	for _, tc := range []struct {
		name  string
		path  string
		body  map[string]any
		cause string
	}{
		{"block is the incident path", "/key/block", map[string]any{"key_id": id}, CauseRevoked},
		{"unblock lets it serve again", "/key/unblock", map[string]any{"key_id": id}, CauseUpdated},
		{"update rewrites the authorization envelope", "/key/update",
			map[string]any{"key_id": id, "rpm_limit": 10}, CauseUpdated},
		{"update can block by another name", "/key/update",
			map[string]any{"key_id": id, "blocked": true}, CauseRevoked},
		{"pend is the reversible refusal", "/key/pend", map[string]any{"key_id": id}, CausePended},
		{"release ends it in one action", "/key/release", map[string]any{"key_id": id}, CauseReleased},
		{"regenerate cuts the old secret outright", "/key/regenerate",
			map[string]any{"key_id": id}, CauseGraceCut},
		{"rotate leaves the old secret inside its grace", "/key/rotate",
			map[string]any{"key_id": id}, CauseRotated},
		{"rotate with no grace is a cut", "/key/rotate",
			map[string]any{"key_id": id, "grace": "0"}, CauseGraceCut},
		{"an early cut is the compromise path", "/key/rotate/cut",
			map[string]any{"key_id": id}, CauseGraceCut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Unblock first where the previous case left the key blocked, so the
			// route under test is not refused for an unrelated reason.
			rec := h.do(http.MethodPost, tc.path, tc.body)
			h.expectStatus(rec, http.StatusOK)
			got := inv.taken()
			if len(got) != 1 {
				t.Fatalf("%s announced %d times, want exactly 1: %v", tc.path, len(got), got)
			}
			if got[0].keyID != id {
				t.Errorf("announced key %q, want %q", got[0].keyID, id)
			}
			if got[0].cause != tc.cause {
				t.Errorf("cause = %q, want %q", got[0].cause, tc.cause)
			}
		})
	}

	// A bulk delete announces every id it removed. Stopping after the first
	// would leave the rest of the list serving on the TTL.
	other, _ := h.newKey(nil)
	inv.taken()
	rec := h.do(http.MethodPost, "/key/delete", map[string]any{"keys": []string{id, other}})
	h.expectStatus(rec, http.StatusOK)
	got := inv.taken()
	if len(got) != 2 {
		t.Fatalf("/key/delete announced %d of 2 deleted keys: %v", len(got), got)
	}
	for _, m := range got {
		if m.cause != CauseRevoked {
			t.Errorf("delete announced cause %q, want %q", m.cause, CauseRevoked)
		}
		if m.keyID != id && m.keyID != other {
			t.Errorf("delete announced an unrelated key %q", m.keyID)
		}
	}
}

// TestAMutationThatCannotBeAnnouncedIsReportedAndStillAudited is the failure
// shape.
//
// The change is durable and this node has honoured it, so the caller must not be
// told the request failed and must not retry it. What they are told is that the
// fleet is converging on the cache TTL instead of within the published bound —
// and the trail is written anyway, because a mutation that took effect and left
// no record is the worse of the two outcomes.
func TestAMutationThatCannotBeAnnouncedIsReportedAndStillAudited(t *testing.T) {
	h, inv := newInvalidatingHarness(t)
	id, _ := h.newKey(nil)
	before := len(h.store.auditLog())

	inv.err = errors.New("the invalidation table is unreachable")
	rec := h.do(http.MethodPost, "/key/block", map[string]any{"key_id": id})
	h.expectFault(rec, http.StatusInternalServerError, CodeInvalidationFailed)

	if got := h.api.Metrics().InvalidationFailures; got != 1 {
		t.Errorf("InvalidationFailures = %d, want 1: a fleet on the TTL fallback has to be visible", got)
	}
	if after := len(h.store.auditLog()); after != before+1 {
		t.Errorf("audit rows went from %d to %d; a mutation that took effect must leave a record "+
			"even when it could not be announced", before, after)
	}
	// And the change really did take effect, which is why retrying is the wrong
	// advice and the message says so.
	k, err := h.store.GetKey(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !k.Blocked {
		t.Error("the block was rolled back; the refusal claims it is durable")
	}
}

// TestWithoutAnInvalidatorTheSurfaceStillMutates keeps the dependency optional.
//
// A deployment with no invalidation path — one process, or one that has accepted
// the entry TTL as its real bound — must not lose the administration surface
// over it. What it loses is the bound, which is what [Invalidator] says.
func TestWithoutAnInvalidatorTheSurfaceStillMutates(t *testing.T) {
	h := newHarness(t)
	id, _ := h.newKey(nil)
	rec := h.do(http.MethodPost, "/key/block", map[string]any{"key_id": id})
	body := h.expectStatus(rec, http.StatusOK)
	key, _ := body["key"].(map[string]any)
	if key == nil || key["blocked"] != true {
		t.Fatalf("the key was not blocked: %s", rec.Body.String())
	}
	if got := h.api.Metrics().Invalidations; got != 0 {
		t.Errorf("Invalidations = %d with no invalidator configured", got)
	}
}
