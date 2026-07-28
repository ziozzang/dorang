package quota

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Credential identifies one authenticating identity at a provider — the unit
// quotas attach to (DESIGN §3). It carries the secret a prober needs and
// redacts it under every fmt verb.
type Credential struct {
	id       string
	provider string
	secret   string
}

// NewCredential builds a credential.
func NewCredential(id, provider, secret string) Credential {
	return Credential{id: id, provider: provider, secret: secret}
}

// ID is the credential id.
func (c Credential) ID() string { return c.id }

// ProviderID names the provider this credential belongs to.
func (c Credential) ProviderID() string { return c.provider }

// Secret returns the credential's secret, for a prober to authenticate with.
func (c Credential) Secret() string { return c.secret }

// String redacts.
func (c Credential) String() string {
	return fmt.Sprintf("quota.Credential{id:%s provider:%s secret:(redacted)}", c.id, c.provider)
}

// GoString redacts %#v.
func (c Credential) GoString() string { return c.String() }

// Format redacts every other verb.
func (c Credential) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		fmt.Fprintf(f, "%q", c.String())
		return
	}
	_, _ = f.Write([]byte(c.String()))
}

// ProviderWindow is one window of a provider's own accounting, normalized into
// this package's vocabulary.
type ProviderWindow struct {
	// Window and Metric say what is being counted.
	Window Window
	Metric Metric
	// Used is the provider's figure. When it is zero and UsedPercent and Limit
	// are both set, Used is derived from them.
	Used int64
	// Limit is the provider's ceiling, zero when it does not publish one.
	Limit int64
	// UsedPercent is the provider's own percentage, when that is all it gives.
	UsedPercent float64
	// ResetAt is when the provider says the window resets, zero when unknown.
	ResetAt time.Time
}

// normalize fills Used from UsedPercent × Limit when the provider only
// publishes a percentage.
func (w ProviderWindow) normalize() ProviderWindow {
	if w.Used == 0 && w.Limit > 0 && w.UsedPercent > 0 {
		w.Used = int64(float64(w.Limit) * w.UsedPercent / 100)
	}
	if w.Used > 0 && w.Limit > 0 && w.UsedPercent == 0 {
		w.UsedPercent = 100 * float64(w.Used) / float64(w.Limit)
	}
	return w
}

// ProbeResult is one successful read of a provider's accounting.
type ProbeResult struct {
	ProviderID   string
	CredentialID string
	// FetchedAt is when the figures were read. It is the baseline instant for
	// the local delta, so it must be the read time, not the store time.
	FetchedAt time.Time
	Windows   []ProviderWindow
}

// Prober reads one provider's reported quota for a credential.
//
// Implementations live with their providers. This package ships only
// [StaticProber]; there are no HTTP clients here.
type Prober interface {
	// ProviderID is the provider kind this prober serves.
	ProviderID() string
	// Probe reads the provider's accounting for one credential. It runs off
	// the request path and must honor the context deadline.
	Probe(ctx context.Context, cred Credential) (ProbeResult, error)
}

// Tracker holds the provider's figures for one credential and combines them
// with local metering (DESIGN §6.2).
//
// A Tracker is safe for concurrent use.
type Tracker struct {
	mu    sync.Mutex
	local LocalCounter
	base  map[wmKey]baseline

	lastOK   time.Time
	lastTry  time.Time
	lastErr  error
	failures int
	polls    int
}

// LocalCounter is the local side of the combination: a monotone
// process-lifetime total per metric. *Meter implements it.
type LocalCounter interface {
	Cumulative(m Metric) int64
}

type wmKey struct {
	w Window
	m Metric
}

// baseline is the provider's figure as of the last poll, plus the local
// cumulative at that same instant, which is what makes the delta computable.
type baseline struct {
	used    int64
	limit   int64
	at      time.Time
	localAt int64
	resetAt time.Time
	pct     float64
}

// NewTracker builds a tracker over a local counter. local may be nil, in which
// case the provider's figure is used as-is and no delta is added.
func NewTracker(local LocalCounter) *Tracker {
	return &Tracker{local: local, base: map[wmKey]baseline{}}
}

// Adopt takes a successful probe result.
//
// This is where the correction lives. The provider's figure re-baselines the
// counter, but never downward past what local metering has already seen since
// the previous baseline:
//
//	effective_used = max( provider_reported_used,
//	                      reported_at_last_poll + local_delta_since_that_poll )
//
// A provider figure that lags a burst therefore cannot erase the burst.
func (t *Tracker) Adopt(r ProbeResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastOK, t.lastTry = r.FetchedAt, r.FetchedAt
	t.lastErr, t.failures = nil, 0
	t.polls++
	for _, w := range r.Windows {
		w = w.normalize()
		k := wmKey{w.Window, w.Metric}
		cum := t.cumulative(w.Metric)
		used := w.Used
		if prev, ok := t.base[k]; ok {
			if d := cum - prev.localAt; d > 0 {
				if est := prev.used + d; est > used {
					used = est
				}
			}
		}
		t.base[k] = baseline{
			used:    used,
			limit:   w.Limit,
			at:      r.FetchedAt,
			localAt: cum,
			resetAt: w.ResetAt,
			pct:     w.UsedPercent,
		}
	}
}

// Fail records a failed fetch.
//
// It deliberately changes nothing else: a failed read is not an exhausted
// quota (DESIGN §6.2). The last good snapshot stays, and staleness grows so
// that a caller can see the figure is aging.
func (t *Tracker) Fail(err error, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastTry = now
	t.lastErr = err
	t.failures++
}

// Effective returns the combined used figure for one window and metric.
func (t *Tracker) Effective(w Window, m Metric, now time.Time) (used, limit int64, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.base[wmKey{w, m}]
	if !ok {
		return 0, 0, false
	}
	// Both terms of the design's formula, written out. The second is the
	// fresher one during a burst, which is the whole point of combining them.
	reported := b.used
	withLocal := b.used
	if d := t.cumulative(m) - b.localAt; d > 0 {
		withLocal += d
	}
	if withLocal > reported {
		return withLocal, b.limit, true
	}
	return reported, b.limit, true
}

// ResetAt returns the provider's reset instant for a window and metric.
func (t *Tracker) ResetAt(w Window, m Metric) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.base[wmKey{w, m}]
	if !ok || b.resetAt.IsZero() {
		return time.Time{}, false
	}
	return b.resetAt, true
}

// Staleness is how long ago the last successful poll was. ok is false when the
// provider has never been read successfully, which is not the same as fresh.
func (t *Tracker) Staleness(now time.Time) (d time.Duration, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastOK.IsZero() {
		return 0, false
	}
	return now.Sub(t.lastOK), true
}

// LastError returns the most recent fetch error, or nil.
func (t *Tracker) LastError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastErr
}

// Failures counts consecutive failed fetches since the last success.
func (t *Tracker) Failures() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failures
}

// cumulative reads the local counter. The caller holds t.mu; LocalCounter is
// documented not to take any lock this tracker could be holding — *Meter
// satisfies that with an atomic.
func (t *Tracker) cumulative(m Metric) int64 {
	if t.local == nil {
		return 0
	}
	return t.local.Cumulative(m)
}

// ErrNoProber is returned when a credential is registered for a provider that
// has no prober.
var ErrNoProber = errors.New("quota: no prober registered for that provider")

// DefaultProbeTimeout bounds one probe when a prober has no explicit timeout.
const DefaultProbeTimeout = 10 * time.Second

// Registry polls the registered probers, concurrently and off the request
// path, with a per-provider timeout (DESIGN §6.2).
type Registry struct {
	mu      sync.Mutex
	probers map[string]Prober
	timeout map[string]time.Duration
	targets map[string]*target
	now     func() time.Time
}

type target struct {
	cred    Credential
	tracker *Tracker
}

// NewRegistry builds a registry. now may be nil.
func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		probers: map[string]Prober{},
		timeout: map[string]time.Duration{},
		targets: map[string]*target{},
		now:     now,
	}
}

// Register adds a prober. timeout may be zero for DefaultProbeTimeout.
func (r *Registry) Register(p Prober, timeout time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probers[p.ProviderID()] = p
	if timeout > 0 {
		r.timeout[p.ProviderID()] = timeout
	}
}

// Track registers a credential to be polled into a tracker.
func (r *Registry) Track(cred Credential, t *Tracker) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.probers[cred.ProviderID()]; !ok {
		return fmt.Errorf("%w: %s", ErrNoProber, cred.ProviderID())
	}
	r.targets[cred.ID()] = &target{cred: cred, tracker: t}
	return nil
}

// Untrack stops polling a credential.
func (r *Registry) Untrack(credID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.targets, credID)
}

// PollStats summarizes one polling round.
type PollStats struct {
	Polled    int
	Succeeded int
	Failed    int
}

// PollOnce probes every tracked credential concurrently, each under its
// provider's timeout, and waits for the round to finish.
//
// A failure updates only that credential's staleness and last error. It never
// disables anything.
func (r *Registry) PollOnce(ctx context.Context) PollStats {
	r.mu.Lock()
	type job struct {
		p       Prober
		cred    Credential
		tracker *Tracker
		timeout time.Duration
	}
	jobs := make([]job, 0, len(r.targets))
	for _, t := range r.targets {
		p, ok := r.probers[t.cred.ProviderID()]
		if !ok {
			continue
		}
		to := r.timeout[t.cred.ProviderID()]
		if to <= 0 {
			to = DefaultProbeTimeout
		}
		jobs = append(jobs, job{p: p, cred: t.cred, tracker: t.tracker, timeout: to})
	}
	now := r.now
	r.mu.Unlock()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		stat = PollStats{Polled: len(jobs)}
	)
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, j.timeout)
			defer cancel()
			res, err := j.p.Probe(c, j.cred)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				j.tracker.Fail(err, now())
				stat.Failed++
				return
			}
			if res.FetchedAt.IsZero() {
				res.FetchedAt = now()
			}
			j.tracker.Adopt(res)
			stat.Succeeded++
		}()
	}
	wg.Wait()
	return stat
}

// Run polls every interval until the context is cancelled. It is the
// off-the-request-path loop; nothing on the hot path waits for it.
func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.PollOnce(ctx)
		}
	}
}

// StaticProber is a Prober that returns whatever it is told to. It ships so
// that quota behavior can be tested — including the failure behavior — without
// a provider, and so that other packages can exercise the same paths.
type StaticProber struct {
	mu       sync.Mutex
	provider string
	results  map[string]ProbeResult
	errs     map[string]error
	delay    time.Duration
	calls    int
}

// NewStaticProber builds a static prober for a provider id.
func NewStaticProber(provider string) *StaticProber {
	return &StaticProber{
		provider: provider,
		results:  map[string]ProbeResult{},
		errs:     map[string]error{},
	}
}

// ProviderID implements Prober.
func (s *StaticProber) ProviderID() string { return s.provider }

// Set makes the next probes for a credential return these windows.
func (s *StaticProber) Set(credID string, fetchedAt time.Time, windows ...ProviderWindow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[credID] = ProbeResult{
		ProviderID:   s.provider,
		CredentialID: credID,
		FetchedAt:    fetchedAt,
		Windows:      windows,
	}
	delete(s.errs, credID)
}

// SetError makes the next probes for a credential fail.
func (s *StaticProber) SetError(credID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[credID] = err
}

// SetDelay makes every probe take d, so a timeout can be exercised.
func (s *StaticProber) SetDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// Calls counts probes served.
func (s *StaticProber) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// Probe implements Prober.
func (s *StaticProber) Probe(ctx context.Context, cred Credential) (ProbeResult, error) {
	s.mu.Lock()
	s.calls++
	delay, err, res := s.delay, s.errs[cred.ID()], s.results[cred.ID()]
	_, has := s.results[cred.ID()]
	s.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ProbeResult{}, ctx.Err()
		}
	}
	if err != nil {
		return ProbeResult{}, err
	}
	if !has {
		return ProbeResult{}, fmt.Errorf("quota: static prober has nothing for %s", cred.ID())
	}
	return res, nil
}
