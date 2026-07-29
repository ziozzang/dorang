package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/probe"
	"github.com/ziozzang/dorang/internal/quota"
)

// The provider usage probes of DESIGN §6.2, wired.
//
// internal/probe — fetchers for three providers, an OAuth-aware token
// resolution, self-rate-limiting, a scrubber and a normalizer that refuses to
// be confidently wrong — had **no importer at all**, and
// `providers[].usage_probe` was on internal/config's `knownUnwired` ledger.
//
// The consequence is the one §6.2 is written to prevent, and it is not
// cosmetic. Local metering counts what went through dorang. A key shared with
// another tool, a subscription window consumed by somebody's IDE, a plan
// spent by a batch job outside the gateway: none of it is visible, so a
// credential reads as having quota left and traffic is routed into an account
// that is already exhausted. §6.2's whole argument is that the two figures
// COMBINE, and without this half there was only ever one of them.
//
// # What is wired, and what it costs when it is off
//
// A prober per enabled provider; a [quota.Tracker] per credential that has a
// quota rule to gate; one poll round per tick, off the request path. Nothing on
// the hot path waits for any of it — [quota.Meter.Check] reads the tracker's
// last adopted figures and nothing else.
//
// With no `usage_probe` block anywhere, no prober is built, no tracker is
// attached, and the meters behave exactly as they did.
//
// # Two things that are refused rather than ignored
//
//  1. **A fetcher no prober exists for.** `fetcher:` is free text in the
//     schema, and internal/config cannot check it without importing a sibling
//     package (§1). So it is checked here, at the one place where both are
//     named, and a wrong one stops the gateway with the supported list in the
//     message. The alternative is a provider that silently never reports —
//     which is the state this whole file exists to end, reintroduced one
//     typo at a time. Note that CONFIG's own worked example said
//     `fetcher: openai`, for which there is deliberately no prober: OpenAI
//     publishes org-level historical spend behind an Admin key, which is a
//     different subject from "what is left of this key".
//
//  2. **Nothing.** Specifically, a probe enabled for a credential with no
//     quota rule is NOT refused — a rule can arrive from a deployment this
//     configuration does not name yet — but it is logged, because a probe whose
//     figures gate nothing is the same silence in a different place. The
//     tracker is keyed by (window, metric) and a rule is what supplies that
//     key; §6.2's combination has nowhere to land without one.
//
// # Percent-only providers
//
// z.ai reports "37% of your 5-hour allowance" and publishes no ceiling. §6.2's
// max() is unit-homogeneous arithmetic and a percentage is not in the metric's
// units, so [quota.Tracker] keeps such a window for its RESET INSTANT only and
// reports no usage. That is the honest reading, and the reset instant is most
// of the value: §7.5a(c)'s expiring-quota score is zero for a rolling window
// until a provider reports when it resets, so this is what makes that strategy
// score anything at all for a subscription credential.
//
// probe.Allowance closes the remaining gap by declaring the window's absolute
// size, and there is no configuration surface for it. That is a real
// limitation and it is stated rather than papered over: it is recorded in
// CONFIG §6.1 rather than left for an operator to discover from a gauge that
// never moves.
type probeSet struct {
	reg *quota.Registry
	// probers is kept for reporting and for the lifetime of the trackers; the
	// registry holds them too.
	probers []*probe.Prober
	// interval is the poll cadence for the whole set: the shortest configured
	// one. Per-provider spacing is enforced by each prober's own MinInterval,
	// which replays its last snapshot rather than re-reading, so a common tick
	// cannot poll any provider faster than that provider was configured for.
	interval time.Duration
}

// DefaultUsageProbeInterval is the poll cadence for a probe that names none.
const DefaultUsageProbeInterval = time.Minute

// buildProbes assembles the probe registry for a configuration, or returns nil
// when no provider enables one.
//
// It is called from both [New] and [App.Reload], and the trackers are rebuilt
// each time on purpose. A tracker holds the meter it computes the local delta
// against; carrying one across a reload would leave it differencing a meter
// nothing records into, whose cumulative is frozen — and a frozen local delta
// makes the provider figure authoritative, which is exactly the revision-1
// behaviour §6.2 was corrected away from.
func (a *App) buildProbes(cfg *config.Config, qs *quotaSet, client *http.Client) (*probeSet, error) {
	var (
		reg     *quota.Registry
		probers []*probe.Prober
		every   time.Duration
	)
	byProvider := map[string]*probe.Prober{}

	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.UsageProbe.Enabled {
			continue
		}
		interval := p.UsageProbe.Interval.Duration()
		if interval <= 0 {
			interval = DefaultUsageProbeInterval
		}
		allowances, err := probeAllowances(p)
		if err != nil {
			return nil, err
		}
		pr, err := probe.New(p.UsageProbe.Fetcher, probe.Config{
			Client:     client,
			Allowances: allowances,
			// No BaseURL override. `providers[].base_url` is the INFERENCE
			// host, and on every provider with a prober the quota endpoint is a
			// different service — pointing a quota read at an inference host
			// produces a 404 that the prober's backoff then treats as a
			// provider fault and spaces out. probe.Config.BaseURL exists for
			// the case the probe package documents, a gateway fronting a
			// provider, and there is no configuration key for that host; a
			// deployment in that position is the limitation recorded in
			// CONFIG §6.1 rather than a guess made here.
			//
			// The prober's own floor, which is what actually spaces reads of one
			// credential. See probeSet.interval.
			MinInterval: interval,
			Now:         a.now,
		})
		if err != nil {
			return nil, probeFetcherError(p, err)
		}
		if reg == nil {
			reg = quota.NewRegistry(a.now)
		}
		reg.Register(pr, 0)
		byProvider[p.Name] = pr
		probers = append(probers, pr)
		if every == 0 || interval < every {
			every = interval
		}
	}
	if reg == nil {
		return nil, nil
	}

	var ungated []string
	for i := range cfg.Credentials {
		cr := &cfg.Credentials[i]
		pr, ok := byProvider[cr.Provider]
		if !ok {
			continue
		}
		// The secret is resolved at PROBE time and not captured here: an OAuth
		// credential's access token is replaced by its own refresher (§11.2b),
		// and a prober holding the token it saw at startup would answer every
		// poll with a 401 — which its own backoff would then read as a provider
		// problem and space out further.
		pr.SetAuth(cr.ID, a.probeAuth(cr.ID))

		m := qs.meter(cr.ID)
		if m == nil {
			ungated = append(ungated, cr.ID)
			continue
		}
		tr := quota.NewTracker(m)
		m.AttachTracker(tr)
		// The prober's own id, not the configured provider name: quota.Registry
		// routes a credential to a prober by this string, and probe.New
		// canonicalizes aliases (z.ai, glm, zhipu all become "zai").
		cred := quota.NewCredential(cr.ID, pr.ProviderID(), "")
		if err := reg.Track(cred, tr); err != nil {
			return nil, fmt.Errorf("app: usage probe for credential %q: %w", cr.ID, err)
		}
	}
	if len(ungated) > 0 {
		sort.Strings(ungated)
		// Logged rather than refused: the rule may arrive with a deployment
		// this configuration does not name yet. Logged rather than passed over
		// in silence, because a probe whose figures gate nothing is the defect
		// this file was written to remove, one layer further in.
		a.logf("app: usage probe enabled for credential(s) %s with no quota rule to gate: "+
			"the provider's figures will be read and will constrain nothing. A rule comes "+
			"from models[].deployments[].limits[] with metric rpm or tpm (§6.2, §10.2)",
			strings.Join(ungated, ", "))
	}
	if every <= 0 {
		every = DefaultUsageProbeInterval
	}
	return &probeSet{reg: reg, probers: probers, interval: every}, nil
}

// probeAllowances converts §6.2's configured window mappings.
//
// internal/config checks the vocabulary against a copy of quota's, because it
// imports no sibling package (§1). This is where the real parser runs, so a
// string that passed there and fails here stops the gateway naming the field
// rather than dropping the allowance — and an allowance dropped in silence is
// the failure this whole mechanism exists to remove, one layer in.
func probeAllowances(p *config.Provider) ([]probe.Allowance, error) {
	if len(p.UsageProbe.Allowances) == 0 {
		return nil, nil
	}
	out := make([]probe.Allowance, 0, len(p.UsageProbe.Allowances))
	for i, a := range p.UsageProbe.Allowances {
		where := fmt.Sprintf("app: provider %q: usage_probe.allowances[%d]", p.Name, i)
		w, err := quota.ParseWindow(a.Window)
		if err != nil {
			return nil, fmt.Errorf("%s.window: %w", where, err)
		}
		m, err := quota.ParseMetric(a.Metric)
		if err != nil {
			return nil, fmt.Errorf("%s.metric: %w", where, err)
		}
		pa := probe.Allowance{Label: a.Label, Window: w, Metric: m, Limit: a.Limit}
		if err := pa.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		out = append(out, pa)
	}
	return out, nil
}

// probeAuth resolves a credential's secret at probe time.
//
// It reaches through the dispatcher rather than capturing the upstream table,
// for the reason [dispatcher.Credential] gives: a reload swaps the table by
// pointer and a rotated key has to reach the next read.
func (a *App) probeAuth(credID string) probe.Auth {
	return probe.AuthFunc(func(context.Context) (string, error) {
		if a.OAuth != nil {
			if c, ok := a.OAuth.Credential(credID); ok {
				// AccessToken reports the refresher's own failure rather than
				// returning an empty string, so a probe against a credential
				// whose refresh is failing is recorded as a failed READ. That
				// is the correct classification: §6.2 says a failed read is not
				// an exhausted quota, and the alternative — an empty bearer
				// token — is a 401 the prober would back off from as though the
				// provider had rejected a good credential.
				t, err := c.AccessToken()
				if err != nil {
					return "", fmt.Errorf("app: oauth credential %q: %w", credID, err)
				}
				return t, nil
			}
		}
		if a.dispatch != nil {
			if s, _ := a.dispatch.Credential(credID); s != "" {
				return s, nil
			}
		}
		return "", fmt.Errorf("app: credential %q has no secret to probe with", credID)
	})
}

// probeFetcherError turns internal/probe's two refusals into a message an
// operator can act on, which means naming the fetchers that do exist.
//
// The two are different statements and both are worth keeping: ErrNoEndpoint
// means the provider was examined and publishes nothing a serving credential
// can read; ErrUnknownProvider means the name is not one this package has
// looked at.
func probeFetcherError(p *config.Provider, err error) error {
	name := p.UsageProbe.Fetcher
	switch {
	case errors.Is(err, probe.ErrNoEndpoint):
		return fmt.Errorf("app: provider %q: usage_probe.fetcher %q has no quota endpoint a "+
			"serving credential can read: %w. The fetchers that do: %s",
			p.Name, name, err, strings.Join(probe.Probed(), ", "))
	case errors.Is(err, probe.ErrUnknownProvider):
		return fmt.Errorf("app: provider %q: usage_probe.fetcher %q is not a fetcher this "+
			"build has: %w. The ones it has: %s",
			p.Name, name, err, strings.Join(probe.Probed(), ", "))
	}
	return fmt.Errorf("app: provider %q: usage_probe: %w", p.Name, err)
}

// meter returns one credential's meter, or nil.
func (q *quotaSet) meter(credential string) *quota.Meter {
	if q == nil {
		return nil
	}
	return q.meters[credential]
}

// pollProbes runs one round. It is called from the background loop and never
// from a request.
func (a *App) pollProbes(ctx context.Context) {
	ps := a.probes.Load()
	if ps == nil || ps.reg == nil {
		return
	}
	ps.reg.PollOnce(ctx)
}

// probeInterval is the background ticker's cadence for the probe round, or zero
// when nothing is probed.
func (a *App) probeInterval() time.Duration {
	if ps := a.probes.Load(); ps != nil {
		return ps.interval
	}
	return 0
}

// ProbeHealth is one credential's probe state, for reporting.
type ProbeHealth struct {
	Provider  string
	Endpoint  string
	Supported []string
}

// ProbeStatus reports which providers are probed. It exists so that "§6.2 is on
// for this provider" is answerable without reading the configuration back.
func (a *App) ProbeStatus() []ProbeHealth {
	ps := a.probes.Load()
	if ps == nil {
		return nil
	}
	out := make([]ProbeHealth, 0, len(ps.probers))
	for _, p := range ps.probers {
		out = append(out, ProbeHealth{Provider: p.ProviderID(), Endpoint: p.Endpoint()})
	}
	return out
}

// compile-time assertion that the OAuth credential is what probeAuth expects.
var _ interface{ AccessToken() (string, error) } = (*auth.OAuthCredential)(nil)
