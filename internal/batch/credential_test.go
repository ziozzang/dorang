package batch

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
)

// The credential a row is DISPATCHED to is the credential its reservation was
// TAKEN against.
//
// Acquire admits a row against the provider's whole candidate set, and which
// candidate has room is the broker's answer, not the caller's. Until that answer
// left the Reservation, the executor had nothing to go on and the wiring picked
// the provider's first credential: a row would hold a slot on account A's axes
// while authenticating as account B, so A's concurrency limit counted requests
// it was not serving and B's counted none of the ones it was. It is the same
// class of defect the router found in its own candidate slices — the reservation
// lands on one deployment's axes while the work goes to another — and it is
// invisible until an account starts returning 429s for traffic it never saw.
func TestReservedCredentialIsTheDispatchedCredential(t *testing.T) {
	const rows = 40

	h := newHarness(t, func(c *Config) { c.RowConcurrency = 8 })

	// A provider with three accounts. The broker's choice rotates, so a wiring
	// that quietly used "the provider's first credential" cannot coincide with
	// it by luck.
	creds := []string{"cred-a", "cred-b", "cred-c"}
	var pickN int
	h.cap.pick = func(CapacityRequest) string {
		c := creds[pickN%len(creds)]
		pickN++
		return c
	}

	var mu sync.Mutex
	dispatched := map[string]string{} // custom id → credential the row was sent with
	h.exec.fn = func(ctx context.Context, req *ExecRequest, attempt int) (*ExecResult, error) {
		mu.Lock()
		dispatched[req.CustomID] = req.Credential
		mu.Unlock()
		if req.Credential == "" {
			return nil, fmt.Errorf("row %s was dispatched with no credential; the "+
				"reservation's choice never reached the executor", req.CustomID)
		}
		return okResult(req), nil
	}

	b := h.create(h.upload(jsonlFile(rows, "m1")))
	final := h.await(b.ID, StatusCompleted)
	if final.RequestCounts.Completed != rows {
		t.Fatalf("counts = %+v, want %d completed", final.RequestCounts, rows)
	}

	// Every reservation the broker granted, and every credential a row was
	// dispatched with. One row, one reservation, so the two multisets are equal
	// or the two halves disagree about the account.
	h.cap.mu.Lock()
	granted := append([]string(nil), h.cap.granted...)
	h.cap.mu.Unlock()

	mu.Lock()
	sent := make([]string, 0, len(dispatched))
	for _, c := range dispatched {
		sent = append(sent, c)
	}
	mu.Unlock()

	if len(granted) != rows || len(sent) != rows {
		t.Fatalf("%d reservations and %d dispatches for %d rows", len(granted), len(sent), rows)
	}
	sort.Strings(granted)
	sort.Strings(sent)
	for i := range granted {
		if granted[i] != sent[i] {
			t.Fatalf("the reserved credentials and the dispatched credentials differ:\n"+
				"reserved:   %v\ndispatched: %v", granted, sent)
		}
	}

	// And the choice really did vary, so the assertion above is not satisfied by
	// a single credential trivially matching itself.
	distinct := map[string]bool{}
	for _, c := range sent {
		distinct[c] = true
	}
	if len(distinct) != len(creds) {
		t.Errorf("rows went to %d distinct credentials, want %d — the broker's "+
			"rotation is not reaching the executor", len(distinct), len(creds))
	}
}

// A reservation released before the executor runs still names its credential:
// the id is read from the reservation while it is held, not after.
func TestExecRequestCarriesTheCredentialOnEveryAttempt(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RowConcurrency = 1; c.MaxAttempts = 3 })
	h.cap.pick = func(CapacityRequest) string { return "cred-only" }

	var seen []string
	var mu sync.Mutex
	h.exec.fn = func(ctx context.Context, req *ExecRequest, attempt int) (*ExecResult, error) {
		mu.Lock()
		seen = append(seen, req.Credential)
		mu.Unlock()
		if attempt < 3 {
			return errResult(503, "warming up"), nil
		}
		return okResult(req), nil
	}

	b := h.create(h.upload(jsonlFile(1, "m1")))
	h.await(b.ID, StatusCompleted)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("executor saw %d attempts, want 3", len(seen))
	}
	for i, c := range seen {
		if c != "cred-only" {
			t.Errorf("attempt %d dispatched with credential %q, want cred-only", i+1, c)
		}
	}
}
