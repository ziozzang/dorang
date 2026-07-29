package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"net/http"

	"gopkg.in/yaml.v3"

	"github.com/ziozzang/dorang/internal/backend"
	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/store"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// storeConfig renders storage.* into the store's own configuration.
//
// SQLite is the default and needs nothing installed; PostgreSQL takes its URL
// from the environment, because a connection string with a password in it is a
// secret and secrets never appear in the configuration file (DESIGN §4.1).
func storeConfig(cfg *config.Config, pepper string) (store.Config, error) {
	sc := store.Config{Pepper: []byte(pepper)}
	if cfg.Auth.Legacy.Enabled {
		until, err := parseUntil(cfg.Auth.Legacy.Until)
		if err != nil {
			return sc, fmt.Errorf("app: auth.legacy.until: %w", err)
		}
		sc.Legacy = store.LegacyAuth{Enabled: true, Until: until}
	}
	switch cfg.Storage.Driver {
	case "postgres":
		name := cfg.Storage.Postgres.URLEnv
		dsn := os.Getenv(name)
		if dsn == "" {
			return sc, fmt.Errorf("app: storage.driver is postgres but %s is unset or empty", name)
		}
		sc.Driver = store.DialectPostgres
		sc.DSN = dsn
		sc.MaxOpenConns = cfg.Storage.Postgres.MaxConns
	default:
		path := config.ExpandPath(cfg.Storage.SQLite.Path)
		if path == "" {
			return sc, errors.New("app: storage.sqlite.path is empty")
		}
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return sc, fmt.Errorf("app: storage directory: %w", err)
			}
		}
		sc.Driver = store.DialectSQLite
		sc.DSN = path
	}
	return sc, nil
}

// brokerConfig renders capacity.* onto the broker's axes (DESIGN §5.1).
//
// The `route` axis is keyed by provider name and configured on the provider,
// not under capacity:, which is why providers[] is read here too.
func brokerConfig(cfg *config.Config, now func() time.Time) capacity.Config {
	c := capacity.Config{
		ProviderGroups:     map[string]int{},
		CredentialGroups:   map[string]int{},
		Routes:             map[string]int{},
		Principals:         map[string]int{},
		InteractiveReserve: cfg.Capacity.Reserve(),
		Now:                now,
	}
	if cfg.Capacity.Global != nil {
		c.Global = cfg.Capacity.Global.MaxConcurrency
	}
	c.Queues = capacity.QueueConfig{
		ProviderGroups:   map[string]int{},
		CredentialGroups: map[string]int{},
		Routes:           map[string]int{},
		Principals:       map[string]int{},
		MaxWait:          map[string]time.Duration{},
	}
	if cfg.Capacity.Global != nil {
		c.Queues.Global = cfg.Capacity.Global.MaxQueue
	}
	for name, l := range cfg.Capacity.ProviderGroups {
		c.ProviderGroups[name] = l.MaxConcurrency
		c.Queues.ProviderGroups[name] = l.MaxQueue
	}
	for name, l := range cfg.Capacity.CredentialGroups {
		c.CredentialGroups[name] = l.MaxConcurrency
		c.Queues.CredentialGroups[name] = l.MaxQueue
	}
	for name, l := range cfg.Capacity.Principals {
		c.Principals[name] = l.MaxConcurrent
		c.Queues.Principals[name] = l.MaxQueue
		c.Queues.MaxWait[name] = l.MaxQueueWait.Duration()
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.MaxConcurrency > 0 {
			c.Routes[p.Name] = p.MaxConcurrency
		}
	}
	for _, m := range cfg.Capacity.Models {
		c.Models = append(c.Models, capacity.ModelLimit{
			Provider: m.Provider, Model: m.Model, Max: m.Limits.MaxConcurrency,
		})
		if m.Limits.MaxQueue > 0 {
			c.Queues.Models = append(c.Queues.Models, capacity.ModelLimit{
				Provider: m.Provider, Model: m.Model, Max: m.Limits.MaxQueue,
			})
		}
	}
	return c
}

// credential is the resolved form of one authenticating identity: the parts the
// router needs (id, group, ceiling) and the part only the upstream call needs
// (the secret), kept apart so no key material reaches internal/router.
type credential struct {
	id            string
	provider      string
	capacityGroup string
	maxConcurrent int
	secret        string
}

// collectCredentials merges credentials[] with key_rotation…keys[].
//
// Both declare the same identity: the example configuration lists acct-1 in
// credentials[] for its provider binding and again under key_rotation for its
// own concurrency ceiling. Merging rather than picking one is what makes both
// spellings mean what they say.
func collectCredentials(cfg *config.Config) map[string]*credential {
	out := make(map[string]*credential, len(cfg.Credentials))
	for i := range cfg.Credentials {
		c := &cfg.Credentials[i]
		secret, _ := c.Key.Value()
		out[c.ID] = &credential{
			id:            c.ID,
			provider:      c.Provider,
			capacityGroup: c.CapacityGroup,
			secret:        secret,
		}
	}
	for provider, kr := range cfg.KeyRotation.Providers {
		for i := range kr.Keys {
			k := &kr.Keys[i]
			c := out[k.ID]
			if c == nil {
				c = &credential{id: k.ID, provider: provider}
				out[k.ID] = c
			}
			if c.provider == "" {
				c.provider = provider
			}
			if k.CapacityGroup != "" {
				c.capacityGroup = k.CapacityGroup
			}
			if k.MaxConcurrency > 0 {
				c.maxConcurrent = k.MaxConcurrency
			}
			if c.secret == "" {
				if s, ok := k.Key.Value(); ok {
					c.secret = s
				}
			}
		}
	}
	return out
}

// routerDeps carries the already-built subsystems into buildRouter.
type routerDeps struct {
	broker   *capacity.Broker
	health   *health.Tracker
	prefix   *prefix.Table
	interner *prefix.Interner
	pricing  *pricing.Catalog
	catalog  *catalog.Catalog
	quota    *quotaSet
	now      func() time.Time
}

// buildRouter compiles models[], aliases and classes into a router.
//
// Every cross-reference is resolved here or by router.New: a deployment naming
// an unknown provider or an unknown credential is a configuration defect, and
// finding it at start-up rather than on the first request is the difference
// between a failed start and an outage.
func buildRouter(cfg *config.Config, cat *catalog.Catalog, deps routerDeps) (*router.Router, error) {
	creds := collectCredentials(cfg)

	groups := make([]router.Group, 0, len(cfg.Models))
	seen := make(map[string]int)
	for i := range cfg.Models {
		m := &cfg.Models[i]
		g := router.Group{Name: m.Name, Class: m.Class}
		for _, s := range m.Strategy {
			st, ok := router.ParseStrategy(s)
			if !ok {
				return nil, fmt.Errorf("app: model %s: unknown strategy %q", m.Name, s)
			}
			g.Strategy = append(g.Strategy, st)
		}
		for j := range m.Deployments {
			d := &m.Deployments[j]
			p, ok := cfg.Provider(d.Provider)
			if !ok {
				return nil, fmt.Errorf("app: model %s: no provider named %q", m.Name, d.Provider)
			}
			id := deploymentID(m.Name, d.Provider, d.UpstreamModel)
			if n := seen[id]; n > 0 {
				id = fmt.Sprintf("%s#%d", id, n)
			}
			seen[deploymentID(m.Name, d.Provider, d.UpstreamModel)]++

			rd := router.Deployment{
				ID:            id,
				Provider:      d.Provider,
				ProviderGroup: p.CapacityGroup,
				Kind:          p.Kind,
				UpstreamModel: d.UpstreamModel,
				Weight:        d.Weight,
				Priority:      d.Priority,
				Timeout:       d.Timeout.Duration(),
				Capabilities:  capabilitiesFor(cat, p.Kind),
				Suppressed:    suppressedFor(p.Kind),
				PrefixTTL:     cfg.PrefixTTLFor(d.Provider, d).TableTTL(),
			}
			if rd.Timeout == 0 {
				rd.Timeout = p.Timeout.Duration()
			}
			for _, cid := range d.Credentials {
				c, ok := creds[cid]
				if !ok {
					return nil, fmt.Errorf("app: model %s deployment %s: no credential named %q",
						m.Name, d.Provider, cid)
				}
				rd.Credentials = append(rd.Credentials, router.Credential{
					ID:            c.id,
					CapacityGroup: c.capacityGroup,
					MaxConcurrent: c.maxConcurrent,
				})
			}
			g.Deployments = append(g.Deployments, rd)
		}
		groups = append(groups, g)
	}

	rc := router.Config{
		Groups:   groups,
		Aliases:  cfg.Aliases,
		Classes:  cfg.Classes,
		Sticky:   router.StickyConfig{Enabled: cfg.Routing.Sticky.IsEnabled(), TTL: cfg.Routing.Sticky.TTL.Duration()},
		Prefix:   router.PrefixConfig{Enabled: cfg.Routing.Prefix.IsEnabled()},
		Fallback: fallbackConfig(cfg),
		Priority: priorityConfig(cfg),
		Now:      deps.now,
	}
	rc.OnCapacity = onCapacity(cfg)
	if rot, ok := router.ParseRotation(cfg.KeyRotation.Strategy); ok {
		rc.Rotation = rot
	} else if cfg.KeyRotation.Strategy != "" {
		return nil, fmt.Errorf("app: key_rotation.strategy: unknown strategy %q",
			cfg.KeyRotation.Strategy)
	}

	return router.New(rc, router.Deps{
		Capacity: deps.broker,
		Health:   deps.health,
		Prefix:   deps.prefix,
		Interner: deps.interner,
		Pricing:  deps.pricing,
		Catalog:  deps.catalog,
		Quota:    deps.quota,
		Urgency:  deps.quota.ranker(),
	})
}

// deploymentID names one routing candidate.
//
// It is built by JOINING three configured values, never by splitting anything:
// a model name is opaque (DESIGN §2.1) and nothing here or downstream ever cuts
// it apart again. The id keys health, prefix affinity and circuit state, so it
// must be stable across a hot reload — which it is, because it is derived only
// from configuration.
// The separator is printable on purpose. The id is stamped into
// x-dorang-deployment, logged and used as a metric label, and internal/server
// does not sanitize the values a dispatcher puts on Result — a control
// character here becomes a malformed response header, which is how the first
// version of this function was found.
func deploymentID(group, provider, upstream string) string {
	return group + "|" + provider + "|" + upstream
}

// capabilitiesFor is what a deployment of this kind can express (DESIGN §10.1).
// It comes from the kind's wire adapter, because that adapter is what will do
// the encoding and therefore what decides what survives.
//
// # It is the only answer, and it used to be one of two
//
// internal/backend had its own computation of this, from the same two constants
// and the same wire shape, because the encoder needed the number and nothing
// handed it one. The two agreed, but by coincidence: both spelled the
// same switch over the same catalog-resolved `api`, and nothing made them. That
// is the arrangement DESIGN §10.1's last paragraph names as the failure — routing
// answers "does any deployment express this" on one set and the encoder converts
// against another — and the codebase has been bitten by two implementations of
// one fact three times.
//
// So this value now travels: onto [router.Deployment.Capabilities], out on
// [router.Decision.Capabilities], and into [backend.Target.Capabilities], which
// the encoder reads in preference to its own default. The deployment that is
// REFUSED for a capability and the deployment that is ENCODED for it are decided
// by the same bits.
func capabilitiesFor(cat *catalog.Catalog, kind string) canonical.Capability {
	return capabilitiesForAPI(apiFor(cat, kind))
}

// capabilitiesForAPI is [capabilitiesFor] for a wire shape that is already
// resolved.
//
// The batch executor is the second caller: it holds a [backend.Provider], which
// carries the api it was built with, and going back to the catalog to derive the
// same value from the kind would be the second derivation this whole change
// removes. It is also the only path where the catalog can be absent, and a
// capability set guessed from a nil lookup is not a set anyone should encode
// against.
//
// The switch itself is [backend.CapabilitiesForAPI] and is deliberately not
// restated here. Unifying the two computations at this seam was only half the
// job: both halves still read the same two wire-package constants, so both were
// wrong together for the one wire shape whose encoder lives in internal/backend
// and had no constant at all. A Gemini deployment was routed and encoded against
// the OpenAI wire shape's set while its encoder wrote no cache_control, no
// logprobs and no thinking block — agreement is not correctness when both
// answers come from the same missing case.
func capabilitiesForAPI(api catalog.API) canonical.Capability {
	return backend.CapabilitiesForAPI(api)
}

// suppressedFor is what dorang declines to send to a deployment of this kind
// even though [capabilitiesFor] says the wire shape accepts it.
//
// A self-hosted engine gets no `service_tier`: internal/backend clears it before
// encoding, in either direction of DESIGN §4.4's argument — as a priority signal
// it is a lie, because vLLM accepts the field with no consumers for it and SGLang
// does not have it on chat/completions at all, and as a relayed caller field it
// is the tell §4.4's single-surface commitment exists to remove.
//
// This is why the answer is not simply to clear the bit in [capabilitiesFor].
// `service_tier` is [canonical.Material] since the capability pass, so a missing
// bit is a `400` in both readers of it — and the reason it is material is the
// PRICE BAND, which does not exist on hardware the operator owns. Refusing there
// would refuse for a reason that is not true of the deployment. The loss is real
// and costs nothing, which is the exact definition of something that belongs in
// x-dorang-dropped-params; see [router.Deployment.Suppressed].
//
// The engine profile is internal/backend's, read through its own exported
// mapping rather than re-spelled here — a second table of which kinds are
// self-hosted is how this function would come to disagree with the code that
// actually clears the field.
func suppressedFor(kind string) canonical.Capability {
	if backend.EngineForKind(kind).SelfHosted() {
		return canonical.CapServiceTier
	}
	return 0
}

// apiFor is the wire adapter a provider kind speaks (DESIGN §4.3). A kind the
// catalog has never heard of is treated as OpenAI-compatible, which is what an
// unlisted OpenAI-compatible server almost always is, and which is visible in
// `dorangctl catalog explain` rather than guessed silently at request time.
func apiFor(cat *catalog.Catalog, kind string) catalog.API {
	if kd, ok := cat.Kind(kind); ok && kd.API != "" {
		return kd.API
	}
	return catalog.APIOpenAIChat
}

func fallbackConfig(cfg *config.Config) router.FallbackConfig {
	fc := router.FallbackConfig{
		MaxHops: cfg.Fallbacks.MaxHops,
		Budget:  time.Duration(cfg.Fallbacks.BudgetMS) * time.Millisecond,
	}
	if len(cfg.Fallbacks.On) > 0 {
		fc.On = make(map[router.Cause][]router.Target, len(cfg.Fallbacks.On))
		for cause, targets := range cfg.Fallbacks.On {
			c, ok := router.ParseCause(cause)
			if !ok {
				continue
			}
			list := make([]router.Target, 0, len(targets))
			for _, t := range targets {
				if tt, ok := router.ParseTarget(t); ok {
					list = append(list, tt)
				}
			}
			fc.On[c] = list
		}
	}
	return fc
}

// priorityConfig renders priority_mapping onto the router's emit rules.
//
// The direction of an engine's priority field is NOT in the configuration file
// and must not be guessed: vLLM and SGLang share a field name and disagree about
// what the number means (§7.5). The shipped defaults carry the direction, so a
// configured backend that the defaults already know keeps its direction and only
// the field name and class fold come from the file.
func priorityConfig(cfg *config.Config) router.PriorityConfig {
	pc := router.DefaultPriority()
	if len(cfg.PriorityMapping.Classes) > 0 {
		pc.Classes = cfg.PriorityMapping.Classes
		if _, ok := pc.Classes[pc.Default]; !ok {
			pc.Default = ""
			for name := range pc.Classes {
				if pc.Default == "" || pc.Classes[name] < pc.Classes[pc.Default] {
					pc.Default = name
				}
			}
		}
	}
	if cfg.PriorityMapping.Emit.Header != "" {
		pc.Header = cfg.PriorityMapping.Emit.Header
	}
	// §10.5's grant, per principal. Without this the ignore half was implemented
	// twice over — DefaultPriority ships Min == Max == 0, and nothing set them —
	// so the allow half could not be selected from configuration at all.
	for name, p := range cfg.Capacity.Principals {
		if !p.GrantsClientPriority() || len(p.Range) != 2 {
			continue
		}
		lo, ok1 := pc.Classes[p.Range[0]]
		hi, ok2 := pc.Classes[p.Range[1]]
		if !ok1 || !ok2 {
			continue // refused at validation; skipped here rather than guessed
		}
		// The pair is a closed interval on the canonical scale and the file may
		// write it in either order — §10.5's own examples do both, because an
		// operator thinks in "batch to interactive", not in "low to high".
		if lo > hi {
			lo, hi = hi, lo
		}
		if pc.Grants == nil {
			pc.Grants = map[string]router.PriorityGrant{}
		}
		pc.Grants[name] = router.PriorityGrant{Min: lo, Max: hi}
	}
	for kind, b := range cfg.PriorityMapping.Emit.Backends {
		rule := pc.Emit[kind]
		rule.Field = b.Field
		if len(b.Map) > 0 {
			rule.Map = b.Map
			rule.Field = ""
		}
		if pc.Emit == nil {
			pc.Emit = map[string]router.EmitRule{}
		}
		pc.Emit[kind] = rule
	}
	return pc
}

// onCapacity is the spill policy for unpinned requests (§5.3, §7.4a2). The file
// states it per provider under key_rotation; the router takes one policy, so a
// single configured "spill" anywhere selects spill.
func onCapacity(cfg *config.Config) capacity.OnCapacity {
	for _, kr := range cfg.KeyRotation.Providers {
		if kr.Stickiness.OnCapacity == "spill" {
			return capacity.Spill
		}
	}
	return capacity.Wait
}

// newQuotaSet builds one quota meter per credential from the rate ceilings the
// deployments declare.
//
// DESIGN §3 attaches quotas to the credential, but the file states rpm and tpm
// on the deployment, so a credential serving two deployments takes the
// STRICTEST of the two. Being wrong in the other direction would let a limit
// quietly stop existing, which is the failure mode §6 exists to prevent.
func newQuotaSet(cfg *config.Config, now func() time.Time) (*quotaSet, error) {
	type limits struct{ rpm, tpm int64 }
	byCred := map[string]*limits{}

	for i := range cfg.Models {
		for j := range cfg.Models[i].Deployments {
			d := &cfg.Models[i].Deployments[j]
			var l limits
			for _, lim := range d.Limits {
				switch lim.Metric {
				case config.MetricRPM:
					l.rpm = lim.Value
				case config.MetricTPM:
					l.tpm = lim.Value
				}
			}
			if l.rpm == 0 && l.tpm == 0 {
				continue
			}
			for _, cid := range d.Credentials {
				cur := byCred[cid]
				if cur == nil {
					cur = &limits{}
					byCred[cid] = cur
				}
				cur.rpm = strictest(cur.rpm, l.rpm)
				cur.tpm = strictest(cur.tpm, l.tpm)
			}
		}
	}

	meters := router.Meters{}
	for cid, l := range byCred {
		var rules []quota.Rule
		if l.rpm > 0 {
			rules = append(rules, quota.Rule{
				Window: quota.Rolling(time.Minute), Metric: quota.MetricRequests,
				Limit: l.rpm, OnExhaust: quota.Cooldown,
			})
		}
		if l.tpm > 0 {
			rules = append(rules, quota.Rule{
				Window: quota.Rolling(time.Minute), Metric: quota.MetricTokensTotal,
				Limit: l.tpm, OnExhaust: quota.Cooldown,
			})
		}
		m, err := quota.NewMeter(quota.MeterConfig{Rules: rules, Now: now})
		if err != nil {
			return nil, fmt.Errorf("app: quota for credential %s: %w", cid, err)
		}
		meters[cid] = m
	}
	qs := &quotaSet{meters: meters}
	// The §7.5a(c) scorer. It is built only when something can actually score:
	// a Ranker over meters with no resetting window answers zero for every
	// credential, and a strategy that always answers zero is silence with a
	// per-request cost.
	if len(meters) > 0 {
		rank := quota.NewRanker(quota.RankerConfig{NodeID: nodeID(cfg)})
		tracked := 0
		for cid, m := range meters {
			if m == nil {
				continue
			}
			rank.Track(cid, m)
			tracked++
		}
		if tracked > 0 {
			qs.rank = rank
		}
	}
	return qs, nil
}

func strictest(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case b < a:
		return b
	}
	return a
}

// meterConfig renders metering.* onto the meter, with the store as the sink.
func meterConfig(cfg *config.Config, st *store.Store, now func() time.Time) (meter.Config, error) {
	// The two packages spell the same three policies differently: the
	// configuration file says none | hash | truncated (DESIGN §12.2) and
	// internal/meter says none | hash | excerpt. Translating here keeps both
	// spellings correct in their own file.
	var mode meter.ExcerptMode
	switch cfg.Metering.Trace.StoreMessages {
	case config.StoreMessagesNone:
		mode = meter.ExcerptNone
	case config.StoreMessagesHash:
		mode = meter.ExcerptHash
	case config.StoreMessagesTruncated, "":
		mode = meter.ExcerptText
	default:
		return meter.Config{}, fmt.Errorf(
			"app: metering.trace.store_messages: unknown mode %q", cfg.Metering.Trace.StoreMessages)
	}
	dir := config.ExpandPath(cfg.Metering.Spool.Dir)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return meter.Config{}, fmt.Errorf("app: metering spool directory: %w", err)
		}
	}
	return meter.Config{
		Sink:            &storeSink{st: st},
		Now:             now,
		FlushInterval:   cfg.Metering.FlushInterval.Duration(),
		SampleRate:      cfg.Metering.Trace.Rate(),
		DailyByteBudget: cfg.Metering.Trace.DailyByteBudget.Bytes(),
		ExcerptMode:     mode,
		ExcerptChars:    cfg.Metering.Trace.TruncateChars,
		SpoolDir:        dir,
		SpoolMaxBytes:   cfg.Metering.Spool.MaxBytes.Bytes(),
	}, nil
}

// buildPricing compiles the price catalog file and the rules kept in the main
// configuration into one catalog (DESIGN §8).
//
// internal/pricing parses one YAML document, so the two sources are spliced into
// one before it is handed over. The file's rules come first and the inline rules
// after, which matters only for the deterministic tie-break between two rules of
// identical specificity and priority.
func buildPricing(cfg *config.Config) (*pricing.Catalog, error) {
	type doc struct {
		Currency string      `yaml:"currency,omitempty"`
		TZ       string      `yaml:"tz,omitempty"`
		Rules    []yaml.Node `yaml:"rules,omitempty"`
	}
	var d doc
	if p := config.ExpandPath(cfg.Pricing.Catalog); p != "" {
		b, err := os.ReadFile(p)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(b, &d); err != nil {
				return nil, fmt.Errorf("app: pricing catalog %s: %w", p, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// A named-but-absent catalog is not fatal: the file's own rules may
			// be the whole price list, and DESIGN §8.3 already makes an unpriced
			// model visible through Cost.Missing rather than through a refusal
			// to start.
		default:
			return nil, fmt.Errorf("app: pricing catalog %s: %w", p, err)
		}
	}
	if cfg.Pricing.Currency != "" {
		d.Currency = cfg.Pricing.Currency
	}
	for i := range cfg.Pricing.Rules {
		n, err := encodeRule(&cfg.Pricing.Rules[i])
		if err != nil {
			return nil, err
		}
		d.Rules = append(d.Rules, *n)
	}
	out, err := yaml.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("app: pricing: %w", err)
	}
	return pricing.ParseCatalog(out)
}

// encodeRule renders one pricing.rules[] entry in the price catalog's own
// spelling. The two schemas differ in three places and each difference is a
// deliberate one in its own file, so the translation is written out rather than
// papered over with a shared struct.
func encodeRule(r *config.PricingRule) (*yaml.Node, error) {
	type match struct {
		Credential  string `yaml:"credential,omitempty"`
		Deployment  string `yaml:"deployment,omitempty"`
		Provider    string `yaml:"provider,omitempty"`
		Model       string `yaml:"model,omitempty"`
		ModelPrefix string `yaml:"model_prefix,omitempty"`
	}
	type rule struct {
		ID       string `yaml:"id,omitempty"`
		Class    string `yaml:"class,omitempty"`
		Match    match  `yaml:"match,omitempty"`
		Priority int    `yaml:"priority,omitempty"`
		// Unit is DERIVED from the components below, because the main file has no
		// `unit:` key and the catalog file requires one. Leaving it empty was the
		// third instance of the `rates.images` defect and the widest: internal/
		// pricing defaults an absent unit to per_1m_tokens, so every non-token
		// rate the schema advertised — `request`, `characters`, `seconds` —
		// passed `dorangctl config lint` and then refused to assemble with "rate
		// does not belong to unit per_1m_tokens". internal/config refuses a rule
		// that mixes two units, which is what makes one derived value correct.
		Unit string `yaml:"unit,omitempty"`

		Input          string `yaml:"input,omitempty"`
		Output         string `yaml:"output,omitempty"`
		CacheRead      string `yaml:"cache_read,omitempty"`
		CacheWrite     string `yaml:"cache_write,omitempty"`
		Reasoning      string `yaml:"reasoning,omitempty"`
		Request        string `yaml:"request,omitempty"`
		Characters     string `yaml:"characters,omitempty"`
		ComputeSeconds string `yaml:"compute_seconds,omitempty"`
		AudioSeconds   string `yaml:"audio_seconds,omitempty"`

		AmountPerPeriod string `yaml:"amount_per_period,omitempty"`
		Period          string `yaml:"period,omitempty"`

		Op     string `yaml:"op,omitempty"`
		Amount string `yaml:"amount,omitempty"`

		Source string `yaml:"source,omitempty"`
		AsOf   string `yaml:"as_of,omitempty"`
	}
	out := rule{
		ID:       r.ID,
		Class:    r.Class,
		Priority: r.Priority,
		Match: match{
			Credential:  r.Match.Credential,
			Deployment:  r.Match.Deployment,
			Provider:    r.Match.Provider,
			Model:       r.Match.Model,
			ModelPrefix: r.Match.ModelPrefix,
		},
	}
	names := make([]string, 0, len(r.Rates))
	for name, v := range r.Rates {
		names = append(names, name)
		// The main file has always spelled the cached-input component
		// "cached_read" and the price catalog "cache_read". Both spellings are
		// now accepted in both files and resolved to the catalog's here, so a
		// rule copied from one file to the other keeps pricing instead of
		// failing to load on an underscore.
		switch config.CanonicalComponent(name) {
		case "input":
			out.Input = v.String()
		case "output":
			out.Output = v.String()
		case "cache_read":
			out.CacheRead = v.String()
		case "cache_write":
			out.CacheWrite = v.String()
		case "reasoning":
			out.Reasoning = v.String()
		case "request":
			out.Request = v.String()
		case "characters":
			out.Characters = v.String()
		case "compute_seconds":
			out.ComputeSeconds = v.String()
		case "audio_seconds":
			out.AudioSeconds = v.String()
		default:
			// Unreachable from a loaded configuration: internal/config refuses
			// an unknown component, and `images` with it. It stays as a guard
			// for a Config built in code rather than parsed.
			return nil, fmt.Errorf("app: pricing rule %s: unknown rate component %q", r.ID, name)
		}
	}
	if len(names) > 0 {
		unit, ok := config.PricingUnit(names)
		if !ok {
			// Unreachable from a loaded configuration for the same reason as the
			// branch above: internal/config refuses a rule whose components are
			// quoted in two different units, because a rule carries one.
			return nil, fmt.Errorf("app: pricing rule %s: its rate components are quoted "+
				"in different units and a rule has one unit", r.ID)
		}
		out.Unit = unit
	}
	switch r.Class {
	case config.PricingFixedSubscription:
		out.AmountPerPeriod = r.Amount.String()
		out.Period = r.Period
	case config.PricingAdjustment:
		out.Op = "percent"
		out.Amount = r.Percent.String()
	case config.PricingNotionalRate:
		// §8.5's provenance. internal/pricing refuses a notional rule without
		// both, so dropping them here would have made the class unusable inline
		// even once the validator accepted it — which is why adding the class
		// name alone would not have been a fix.
		out.Source = r.Source
		out.AsOf = r.AsOf
	}
	var n yaml.Node
	if err := n.Encode(out); err != nil {
		return nil, fmt.Errorf("app: pricing rule %s: %w", r.ID, err)
	}
	return &n, nil
}

// passthroughRoutes renders passthrough.* onto the server's route set (§10.6).
// An unmapped prefix is not served, and a route whose provider has no base URL
// is dropped rather than pointed at nothing.
//
// It takes the resolved provider table rather than the catalog because the one
// thing it has to get right — how dorang's own credential is spelled for this
// provider — is the backend adapter's answer, and asking a second implementation
// would produce a relay that authenticates differently from every other request
// to the same host.
func passthroughRoutes(cfg *config.Config, table *upstreamTable) []server.PassthroughRoute {
	if !cfg.Passthrough.Enabled {
		return nil
	}
	creds := collectCredentials(cfg)
	byProvider := map[string]*credential{}
	for _, c := range creds {
		if _, ok := byProvider[c.provider]; !ok {
			byProvider[c.provider] = c
		}
	}
	out := make([]server.PassthroughRoute, 0, len(cfg.Passthrough.Routes))
	for _, r := range cfg.Passthrough.Routes {
		p, ok := cfg.Provider(r.Provider)
		if !ok || p.BaseURL == "" {
			continue
		}
		mode := r.Auth
		if mode == "" {
			mode = cfg.Passthrough.Default.Auth
		}
		meterIt := cfg.Passthrough.Default.Meter == nil || *cfg.Passthrough.Default.Meter
		if r.Meter != nil {
			meterIt = *r.Meter
		}
		timeout := r.Timeout.Duration()
		if timeout == 0 {
			timeout = cfg.Passthrough.Default.Timeout.Duration()
		}
		route := server.PassthroughRoute{
			Prefix:   r.Prefix,
			Provider: r.Provider,
			BaseURL:  p.BaseURL,
			Auth:     mode,
			Meter:    meterIt,
			Timeout:  timeout,
		}
		if mode == config.PassthroughAuthDorang {
			prov, known := table.provider(r.Provider)
			if c := byProvider[r.Provider]; known && c != nil && c.secret != "" {
				secret := c.secret
				route.Credential = func(h http.Header) { _ = prov.ApplyCredential(secret, nil, h) }
			}
		}
		out = append(out, route)
	}
	return out
}

// modelList is the client-facing model list of GET /v1/models.
//
// Aliases appear alongside groups because a client that reads the list and then
// asks for a name from it must not be refused (COMPATIBILITY §7.4); the server
// filters the result by the calling key's allow-list.
type modelList struct {
	models atomic.Pointer[[]server.Model]
}

func newModelList(cfg *config.Config) *modelList {
	m := &modelList{}
	m.swap(cfg)
	return m
}

func (m *modelList) swap(cfg *config.Config) {
	names := make([]string, 0, len(cfg.Models)+len(cfg.Aliases))
	names = append(names, cfg.ModelNames()...)
	for alias := range cfg.Aliases {
		names = append(names, alias)
	}
	sort.Strings(names)
	out := make([]server.Model, 0, len(names))
	for _, n := range names {
		out = append(out, server.Model{ID: n})
	}
	m.models.Store(&out)
}

// Models implements server.ModelLister.
func (m *modelList) Models() []server.Model {
	if p := m.models.Load(); p != nil {
		return *p
	}
	return nil
}
