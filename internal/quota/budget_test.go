package quota

import (
	"errors"
	"strings"
	"testing"
)

// What is left of this file is what is left of budget.go: the subject a ceiling
// attaches to, and the typed refusal internal/router classifies.
//
// The reserve/settle tests that used to live here went with the in-memory
// Budget they described. They were not deleted because they were failing —
// every one of them passed — but because they were the only thing exercising a
// second implementation of a mechanism the request path reaches through
// internal/cluster's durable ledger. The properties they asserted are asserted
// there, against the path a request actually takes:
//
//	never exceeded under concurrency  cluster.TestBudgetIsNeverExceededUnderConcurrency
//	                                  scenario.TestScenario05_BudgetNeverExceededUnderConcurrency
//	refunded in full before dispatch  cluster.TestReleaseRefundsInFull
//	an unsettled hold does not lock   cluster.TestLeaseOutlivingItsNodeIsReclaimed
//	  the budget (R1-5)               scenario §14.5's third subtest
//	the estimate is an upper bound    app.TestBudgetEstimateIsPessimistic (the
//	                                  estimate is priced where a price exists,
//	                                  which is after routing)

var subject = Subject{Kind: "key", ID: "key-1"}

func TestBudgetExceededIsTerminalNotAFallbackCondition(t *testing.T) {
	// DESIGN §6.4: exceeding a budget is terminal and must be distinguishable
	// from a rate limit WITHOUT string matching, because the two send a request
	// to opposite places — one down the fallback chain, one nowhere.
	err := error(&BudgetError{
		Subject: subject, Limit: NanoUSD(1), Spent: NanoUSD(1), Requested: NanoUSD(2),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if !IsTerminal(err) {
		t.Fatal("budget exceeded must be terminal: failing is the correct outcome")
	}
	var be *BudgetError
	if !errors.As(err, &be) || be.Subject != subject || be.Requested != NanoUSD(2) {
		t.Fatalf("error carries no usable detail: %+v", be)
	}
	if msg := err.Error(); !strings.Contains(msg, "budget exceeded") {
		t.Fatalf("message = %q", msg)
	}
	// A rate-limit-shaped error is not terminal, so the two cannot be confused.
	if IsTerminal(errors.New("some other failure")) {
		t.Fatal("an unrelated error was reported as terminal")
	}
}

func TestSubjectRendering(t *testing.T) {
	// The rendering reaches an operator: it is what a refusal's message and a
	// notification's `subject` field carry, so "global" must not become "" and a
	// kind must not silently swallow its id.
	for _, tc := range []struct {
		s    Subject
		want string
	}{
		{Subject{}, "global"},
		{Global, "global"},
		{Subject{Kind: "team"}, "team"},
		{Subject{ID: "t1"}, "t1"},
		{subject, "key:key-1"},
	} {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("Subject%+v.String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestBudgetErrorRendersMoneyAndNoCredential(t *testing.T) {
	// The message is built from the subject and four amounts, and the subject is
	// a kind and an id. There is no field on this type that could hold a token,
	// which is what keeps "an error never carries a credential" a property of
	// the type rather than of its callers.
	msg := (&BudgetError{
		Subject: Subject{Kind: "key", ID: "key-1"},
		Limit:   NanoUSD(2.5), Spent: NanoUSD(2), Reserved: NanoUSD(0.25), Requested: NanoUSD(1),
	}).Error()
	for _, want := range []string{"key:key-1", "2.500000", "2.000000", "0.250000", "1.000000"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}
