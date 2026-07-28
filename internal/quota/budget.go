package quota

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

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

// ErrNoReservation reports an unknown or already-settled reservation id.
var ErrNoReservation = errors.New("quota: no such reservation")

// IsTerminal reports whether an error must not be turned into a fallback.
func IsTerminal(err error) bool {
	var t interface{ Terminal() bool }
	return errors.As(err, &t) && t.Terminal()
}

// Estimate is the upper bound a reservation is taken against (DESIGN §6.4):
// exact input tokens priced, plus output priced at max_tokens. It is
// deliberately pessimistic — a reservation that turns out too large is
// refunded at settlement, while one that turns out too small has already let
// the budget overshoot.
type Estimate struct {
	InputTokens        int64
	MaxOutputTokens    int64
	InputNanoPerToken  int64
	OutputNanoPerToken int64
	// FixedNanoUSD is any per-request charge that does not scale with tokens.
	FixedNanoUSD int64
}

// UpperBound returns the estimate in nano-USD.
func (e Estimate) UpperBound() int64 {
	return e.InputTokens*e.InputNanoPerToken +
		e.MaxOutputTokens*e.OutputNanoPerToken +
		e.FixedNanoUSD
}

// Hold distinguishes the two stages of a reservation (DESIGN §6.4).
type Hold uint8

const (
	// SoftHold is taken at the gate, before capacity is acquired. A request
	// that never reaches an upstream releases it in full.
	SoftHold Hold = iota
	// HardHold is taken once capacity is acquired and the request is about to
	// be dispatched.
	HardHold
)

// String returns the hold's name.
func (h Hold) String() string {
	if h == HardHold {
		return "hard"
	}
	return "soft"
}

// Reservation is money held but not yet spent.
type Reservation struct {
	// ID identifies the reservation for settlement.
	ID uint64
	// Subject is whose budget is held.
	Subject Subject
	// Amount is the held amount in nano-USD.
	Amount int64
	// Hold is soft at the gate, hard after capacity is acquired.
	Hold Hold
	// ReservedUntil is when the reservation expires and is swept. Without it a
	// process killed between reserve and settle would lock the amount forever
	// (R1-5).
	ReservedUntil time.Time
}

// BudgetSnapshot is a subject's budget state.
type BudgetSnapshot struct {
	Subject     Subject
	Limit       int64
	Spent       int64
	Reserved    int64
	Available   int64
	PeriodStart time.Time
	PeriodEnd   time.Time
	Holds       int
}

// BudgetConfig configures a [Budget].
type BudgetConfig struct {
	// Period is the budget window. Calendar windows are the normal choice.
	Period Window
	// DefaultLimit is the limit applied to a subject with no explicit one.
	// Zero means unlimited, which is what an unbudgeted deployment looks like.
	DefaultLimit int64
	// SoftTTL bounds a soft hold: the gate-to-capacity window. It should be
	// larger than the maximum queue wait.
	SoftTTL time.Duration
	// HardTTL bounds a hard hold: the dispatch-to-settlement window. It should
	// be larger than the request timeout.
	HardTTL time.Duration
	// Now overrides the clock.
	Now func() time.Time
}

// Defaults for a Budget.
const (
	DefaultSoftTTL = 60 * time.Second
	DefaultHardTTL = 15 * time.Minute
)

// Budget holds money before it is spent, so that concurrent requests cannot
// overshoot a limit between checking it and charging it (DESIGN §6.4).
//
// A Budget is safe for concurrent use.
type Budget struct {
	mu     sync.Mutex
	cfg    BudgetConfig
	limits map[Subject]int64
	states map[Subject]*budgetState
	holds  map[uint64]*Reservation
	seq    uint64
}

type budgetState struct {
	periodStart time.Time
	spent       int64
	soft        int64
	hard        int64
	holds       int
}

// NewBudget builds a budget.
func NewBudget(cfg BudgetConfig) (*Budget, error) {
	if !cfg.Period.Valid() {
		return nil, fmt.Errorf("quota: budget period %q is not a valid window", cfg.Period)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.SoftTTL = orDur(cfg.SoftTTL, DefaultSoftTTL)
	cfg.HardTTL = orDur(cfg.HardTTL, DefaultHardTTL)
	return &Budget{
		cfg:    cfg,
		limits: map[Subject]int64{},
		states: map[Subject]*budgetState{},
		holds:  map[uint64]*Reservation{},
	}, nil
}

// SetLimit sets one subject's ceiling in nano-USD. Zero means unlimited.
func (b *Budget) SetLimit(s Subject, limit int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limits[s] = limit
}

// Limit returns a subject's ceiling.
func (b *Budget) Limit(s Subject) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limitOf(s)
}

func (b *Budget) limitOf(s Subject) int64 {
	if l, ok := b.limits[s]; ok {
		return l
	}
	return b.cfg.DefaultLimit
}

// state returns the subject's state, rolling the period over when the boundary
// has been crossed.
//
// Outstanding reservations survive a rollover: they are money the previous
// period committed but has not accounted for yet, and dropping them would let
// the same request be paid for out of two periods.
func (b *Budget) state(s Subject, now time.Time) *budgetState {
	st, ok := b.states[s]
	start := b.cfg.Period.PeriodStart(now)
	if !ok {
		st = &budgetState{periodStart: start}
		b.states[s] = st
		return st
	}
	if start.After(st.periodStart) {
		st.periodStart, st.spent = start, 0
	}
	return st
}

// Reserve takes a soft hold for an estimated amount.
//
// The check is against spent plus everything currently held, which is what
// makes concurrent reservations unable to overshoot: two requests that would
// each fit on their own do not both fit if together they exceed the limit.
func (b *Budget) Reserve(s Subject, amount int64, now time.Time) (*Reservation, error) {
	if amount < 0 {
		return nil, fmt.Errorf("quota: negative reservation")
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	st := b.state(s, now)
	limit := b.limitOf(s)
	if limit > 0 {
		held := st.soft + st.hard
		if st.spent+held+amount > limit {
			return nil, &BudgetError{
				Subject: s, Limit: limit, Spent: st.spent,
				Reserved: held, Requested: amount,
			}
		}
	}
	b.seq++
	r := &Reservation{
		ID:            b.seq,
		Subject:       s,
		Amount:        amount,
		Hold:          SoftHold,
		ReservedUntil: now.Add(b.cfg.SoftTTL),
	}
	st.soft += amount
	st.holds++
	b.holds[r.ID] = r
	return r, nil
}

// Harden promotes a soft hold to a hard one, which is what happens when
// capacity has been acquired and the request is about to be dispatched. It
// also extends the expiry to the hard TTL.
func (b *Budget) Harden(id uint64, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.holds[id]
	if !ok {
		return ErrNoReservation
	}
	if r.Hold == HardHold {
		return nil
	}
	st := b.state(r.Subject, now)
	st.soft -= r.Amount
	st.hard += r.Amount
	r.Hold = HardHold
	r.ReservedUntil = now.Add(b.cfg.HardTTL)
	return nil
}

// Release refunds a reservation in full.
//
// This is the path for everything that fails before dispatch: a request that
// reserved at the gate and was then refused while waiting for capacity never
// reached an upstream and must cost nothing. Revision 1 of the design defined
// settlement only for completion, so that budget leaked (R1-6).
func (b *Budget) Release(id uint64, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.releaseLocked(id, now)
}

func (b *Budget) releaseLocked(id uint64, now time.Time) error {
	r, ok := b.holds[id]
	if !ok {
		return ErrNoReservation
	}
	st := b.state(r.Subject, now)
	b.dropHold(st, r)
	delete(b.holds, id)
	return nil
}

// dropHold removes a reservation's held amount from its state.
func (b *Budget) dropHold(st *budgetState, r *Reservation) {
	if r.Hold == HardHold {
		st.hard -= r.Amount
	} else {
		st.soft -= r.Amount
	}
	if st.soft < 0 {
		st.soft = 0
	}
	if st.hard < 0 {
		st.hard = 0
	}
	st.holds--
	if st.holds < 0 {
		st.holds = 0
	}
}

// Settle records what a request actually cost and releases the reservation.
//
// The estimate is an upper bound, so actual is normally smaller and the
// difference returns to the budget. An actual above the estimate is still
// recorded — the money was spent, and hiding it would make the ledger lie —
// which is why the estimate is deliberately pessimistic.
func (b *Budget) Settle(id uint64, actual int64, now time.Time) error {
	if actual < 0 {
		return fmt.Errorf("quota: negative settlement")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.holds[id]
	if !ok {
		return ErrNoReservation
	}
	st := b.state(r.Subject, now)
	b.dropHold(st, r)
	st.spent += actual
	delete(b.holds, id)
	return nil
}

// SweepExpired reclaims reservations that outlived their expiry and returns how
// many were reclaimed and how much money they held.
//
// This is the safety net for a process that died between reserve and settle
// (R1-5). It is the same pattern capacity uses for reservations, and it exists
// for the same reason: without it, a budget is eventually exhausted by money
// nobody spent.
func (b *Budget) SweepExpired(now time.Time) (n int, reclaimed int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, r := range b.holds {
		if now.Before(r.ReservedUntil) {
			continue
		}
		st := b.state(r.Subject, now)
		b.dropHold(st, r)
		delete(b.holds, id)
		n++
		reclaimed += r.Amount
	}
	return n, reclaimed
}

// Snapshot reports a subject's state.
func (b *Budget) Snapshot(s Subject, now time.Time) BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state(s, now)
	limit := b.limitOf(s)
	held := st.soft + st.hard
	avail := int64(0)
	if limit > 0 {
		avail = max64(limit-st.spent-held, 0)
	}
	return BudgetSnapshot{
		Subject:     s,
		Limit:       limit,
		Spent:       st.spent,
		Reserved:    held,
		Available:   avail,
		PeriodStart: st.periodStart,
		PeriodEnd:   b.cfg.Period.PeriodEnd(now),
		Holds:       st.holds,
	}
}

// Outstanding returns how many reservations are unsettled.
func (b *Budget) Outstanding() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.holds)
}
