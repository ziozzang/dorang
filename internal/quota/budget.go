package quota

import (
	"errors"
	"fmt"
)

// The budget VOCABULARY: who a ceiling belongs to, and what a refusal is.
//
// # What is not here, and where it went
//
// There is no Budget type. There was one — an exact, in-memory reserve/settle
// implementation of DESIGN §6.4 — and it had no caller outside this package's
// own tests: the request path reserves against [cluster.Ledger], the durable
// counter that closed risk W9, and has done since clustering shipped.
//
// (internal/cluster's Ledger; this package cannot name it in a doc link,
// because the dependency runs the other way).
//
// It was deleted rather than kept, and the reason is worth stating because
// keeping it is the tempting answer. This codebase has been bitten three times
// by two implementations of one thing — most expensively by a dispatch layer
// whose fixes never ran, and once by a store-side budget reservation that could
// have counted the same money twice. That store-side reservation was the third
// implementation of THIS mechanism, deleted in migration 0006; the DESIGN §17
// W9 row's "no in-memory path is kept beside it" was written about that sweep,
// and this one survived it only because the scenario test kept it warm.
//
// Nor was it a trade of durability for speed, which is the usual argument for
// keeping an in-memory tier: the deleted Budget serialized every reservation on
// one mutex, where the ledger takes a read lock and an atomic compare-and-swap
// on a block the node already holds and touches the store once per BLOCK rather
// than once per request. The durable path is the cheaper one under concurrency,
// so "keep the in-memory one for the notebook profile" would have been keeping a
// slower implementation for its lack of a feature.
//
// Everything it claimed is served by the surviving path and asserted against it:
// the never-exceeded property under a hundred concurrent requests in
// `testing/scenario` §14.5 and `cluster.TestBudgetIsNeverExceededUnderConcurrency`,
// the full refund of anything that never reached an upstream in
// `cluster.TestReleaseRefundsInFull`, and R1-5's "money nobody spent must not
// lock the budget" in the lease reclaim — which answers it one level up, at the
// node rather than at the reservation, because the ledger charges a whole block
// before a unit of it is spent.
//
// What remains here is what other packages actually import: the subject a
// ceiling attaches to, and the typed refusal internal/router classifies.

// Subject is what a budget attaches to: a credential, a key, a user, a team,
// or the deployment as a whole (DESIGN §6.4).
type Subject struct {
	Kind string
	ID   string
}

// String renders the subject.
func (s Subject) String() string {
	switch {
	case s.Kind == "" && s.ID == "":
		return "global"
	case s.ID == "":
		return s.Kind
	case s.Kind == "":
		return s.ID
	}
	return s.Kind + ":" + s.ID
}

// Global is the deployment-wide subject.
var Global = Subject{Kind: "global"}

// BudgetError reports a refused reservation. It is deliberately its own type:
// exceeding a budget is not a fallback condition, and a caller must be able to
// tell it apart from a rate limit without string matching (DESIGN §6.4,
// fallbacks.on budget_exceeded: []).
type BudgetError struct {
	Subject   Subject
	Limit     int64
	Spent     int64
	Reserved  int64
	Requested int64
}

// Error implements error. It renders money, never a credential.
func (e *BudgetError) Error() string {
	return fmt.Sprintf(
		"quota: budget exceeded for %s: limit %.6f USD, spent %.6f, reserved %.6f, requested %.6f",
		e.Subject, USD(e.Limit), USD(e.Spent), USD(e.Reserved), USD(e.Requested))
}

// Is makes every BudgetError match ErrBudgetExceeded.
func (e *BudgetError) Is(target error) bool {
	_, ok := target.(*BudgetError)
	return ok
}

// Terminal reports that this refusal must not be retried elsewhere. Failing is
// the correct outcome; there is no cheaper deployment that makes the money
// reappear.
func (e *BudgetError) Terminal() bool { return true }

// ErrBudgetExceeded matches any [BudgetError] under errors.Is.
var ErrBudgetExceeded error = &BudgetError{}

// IsTerminal reports whether an error must not be turned into a fallback.
func IsTerminal(err error) bool {
	var t interface{ Terminal() bool }
	return errors.As(err, &t) && t.Terminal()
}
