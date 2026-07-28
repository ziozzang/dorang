package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FieldError is one problem, at the YAML path it was found at.
type FieldError struct {
	Path    string
	Message string
}

func (e FieldError) Error() string {
	if e.Path == "" {
		return e.Message
	}
	return e.Path + ": " + e.Message
}

// ValidationError aggregates every problem found in one configuration. Loading
// never stops at the first: one run reports one complete list.
type ValidationError struct {
	Source string
	Errors []FieldError
}

func (v *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("invalid configuration")
	if v.Source != "" {
		b.WriteString(" ")
		b.WriteString(v.Source)
	}
	b.WriteString(": ")
	b.WriteString(strconv.Itoa(len(v.Errors)))
	if len(v.Errors) == 1 {
		b.WriteString(" problem:")
	} else {
		b.WriteString(" problems:")
	}
	for _, e := range v.Errors {
		b.WriteString("\n  - ")
		b.WriteString(e.Error())
	}
	return b.String()
}

// Unwrap exposes the individual problems to errors.Is and errors.As.
func (v *ValidationError) Unwrap() []error {
	out := make([]error, len(v.Errors))
	for i, e := range v.Errors {
		out[i] = e
	}
	return out
}

// Problems returns the individual problems of a validation error, or nil if err
// is not one.
func Problems(err error) []FieldError {
	var v *ValidationError
	if errors.As(err, &v) {
		return v.Errors
	}
	return nil
}

type collector struct {
	errs []FieldError
}

func (c *collector) add(path, format string, args ...any) {
	c.errs = append(c.errs, FieldError{Path: path, Message: fmt.Sprintf(format, args...)})
}

func (c *collector) err(source string) error {
	if len(c.errs) == 0 {
		return nil
	}
	return &ValidationError{Source: source, Errors: c.errs}
}

// timeNow is overridden in tests so "must be in the future" is deterministic.
var timeNow = time.Now

// Validate checks the whole configuration and returns every problem at once as
// a *[ValidationError]. It performs no I/O: secret resolution happens in [Load].
func (c *Config) Validate() error {
	col := &collector{}
	c.validate(col)
	return col.err(c.source)
}

var (
	storageDrivers   = []string{"sqlite", "postgres"}
	capacityModes    = []string{CapacityModeLocal, CapacityModeSharedRedis, CapacityModeSharedPG, CapacityModeLeased}
	serverEnvs       = []string{EnvProduction, EnvDevelopment}
	backoffKinds     = []string{"exponential", "linear", "constant"}
	rotationStrategy = []string{"round_robin", "least_used", "failover", "random"}
	stickyScopes     = []string{"session", "api_key", "user", "team", "tenant", "none"}
	onCapacityModes  = []string{"wait", "spill"}
	strategies       = []string{
		"round_robin", "least_busy", "lowest_cost", "lowest_latency", "highest_tps",
		"sticky", "prefix_sticky", "priority", "weighted_random",
	}
	stickyKeyParts  = []string{"api_key", "session_id", "user", "team", "tenant"}
	checkpointKinds = []string{"logarithmic", "fixed"}
	fallbackCauses  = []string{
		CauseRateLimit, CauseQuotaExhausted, CauseContextWindow, CauseContentPolicy,
		CauseUpstream5xx, CauseTimeout, CauseBudgetExceeded, CauseAuth,
	}
	fallbackTargets = []string{TargetSameGroup, TargetSameClass, TargetSameClassLarger}
	pricingClasses  = []string{PricingMarginalUsage, PricingFixedSubscription, PricingAdjustment}
	storeMessages   = []string{StoreMessagesNone, StoreMessagesHash, StoreMessagesTruncated}
	logLevels       = []string{"debug", "info", "warn", "error"}
	logFormats      = []string{"json", "text"}
	passthroughAuth = []string{PassthroughAuthDorang, PassthroughAuthClient, PassthroughAuthNone}
	shadowModes     = []string{ShadowModeOff, ShadowModeMirror, ShadowModeCompare}
	metricNames     = []string{MetricMaxConcurrent, MetricRPM, MetricTPM, MetricMaxQueue}
)

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func mustBeOneOf(c *collector, path, value string, allowed []string) {
	if !oneOf(value, allowed) {
		c.add(path, "%q is not a known value: want one of %s", value, strings.Join(allowed, ", "))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func nonNegative(c *collector, path string, v int64) {
	if v < 0 {
		c.add(path, "must not be negative (is %d)", v)
	}
}

func (c *Config) validate(col *collector) {
	if c.Version != Version {
		col.add("version", "unsupported configuration version %d: this build understands version %d",
			c.Version, Version)
	}

	env := c.Server.Env
	c.validateServer(col)
	c.validateStorage(col)
	c.validateCluster(col)
	c.validateAuth(col)

	providers := map[string]*Provider{}
	c.validateProviders(col, providers)
	credentials := map[string]*Credential{}
	c.validateCredentials(col, providers, credentials, env)
	c.validateCapacity(col, providers)
	c.validateKeyRotation(col, providers, credentials, env)
	models := c.validateModels(col, providers, credentials)
	c.validateAliasesAndClasses(col, models)
	c.validateRouting(col)
	c.validateFallbacks(col)
	c.validatePricing(col, providers, credentials)
	c.validateMetering(col)
	c.validateObservability(col)
	c.validateExtensions(col)
	c.validatePassthrough(col, providers)
	c.validateShadow(col)
	c.validateNotifications(col)
	c.validatePriorityMapping(col)
	c.validateLeasedLimits(col)
}

func (c *Config) validateServer(col *collector) {
	if c.Server.Listen == "" {
		col.add("server.listen", "must not be empty")
	}
	mustBeOneOf(col, "server.env", c.Server.Env, serverEnvs)
	if c.Server.MasterKeyEnv == "" {
		col.add("server.master_key_env", "must name the environment variable holding the master key")
	}
	if c.Server.KeyPepperEnv == "" {
		col.add("server.key_pepper_env", "must name the environment variable holding the key pepper "+
			"(dorang_v1 hashes keys with it, §2.4)")
	}
	if c.Server.RequestTimeout <= 0 {
		col.add("server.request_timeout", "must be greater than zero")
	}
	nonNegative(col, "server.shutdown_grace", int64(c.Server.ShutdownGrace))
}

func (c *Config) validateStorage(col *collector) {
	mustBeOneOf(col, "storage.driver", c.Storage.Driver, storageDrivers)
	switch c.Storage.Driver {
	case "sqlite":
		if c.Storage.SQLite.Path == "" {
			col.add("storage.sqlite.path", "must not be empty when the driver is sqlite")
		}
	case "postgres":
		if c.Storage.Postgres.URLEnv == "" {
			col.add("storage.postgres.url_env", "must name the environment variable holding the "+
				"connection URL when the driver is postgres")
		}
		if c.Storage.Postgres.MaxConns <= 0 {
			col.add("storage.postgres.max_conns", "must be greater than zero")
		}
	}
}

func (c *Config) validateCluster(col *collector) {
	mustBeOneOf(col, "cluster.capacity_mode", c.Cluster.CapacityMode, capacityModes)
	if !c.Cluster.Enabled {
		return
	}
	// Design §5.6, risk W1: this is a hard guard, not a recommendation.
	if c.Cluster.CapacityMode == CapacityModeLocal {
		col.add("cluster.capacity_mode",
			"cluster.enabled is true with capacity_mode \"local\", which refuses to start: "+
				"node-local counting is exact on one node only, so with N nodes every ceiling is "+
				"counted N times over and the provider sees up to N-fold the configured limit. "+
				"The upstream 429s that follow cascade into the fallback chain (§7.6) and consume "+
				"the capacity of unrelated models in the same class, so the failure surfaces far "+
				"from its cause. Set capacity_mode to %q, %q or %q (§5.6)",
			CapacityModeSharedRedis, CapacityModeSharedPG, CapacityModeLeased)
	}
	if c.Cluster.CapacityMode == CapacityModeSharedRedis && c.Cluster.RedisURLEnv == "" {
		col.add("cluster.redis_url_env", "must name the environment variable holding the Redis URL "+
			"when capacity_mode is %q", CapacityModeSharedRedis)
	}
	if c.Cluster.MinLeasable <= 0 {
		col.add("cluster.min_leasable", "must be greater than zero")
	}
}

func (c *Config) validateAuth(col *collector) {
	if !c.Auth.Legacy.Enabled {
		return
	}
	// Design §2.4: legacy verification is a migration window, so it must have
	// an end date, and the date must still be ahead of us.
	if strings.TrimSpace(c.Auth.Legacy.Until) == "" {
		col.add("auth.legacy.until",
			"auth.legacy.enabled is true, which requires an end date: legacy_sha256 is an unsalted "+
				"single-round digest accepted only for a migration window (§2.4)")
		return
	}
	until, err := parseUntil(c.Auth.Legacy.Until)
	if err != nil {
		col.add("auth.legacy.until", "%v: want a date such as \"2026-12-31\" or an RFC 3339 timestamp", err)
		return
	}
	if !until.After(timeNow()) {
		col.add("auth.legacy.until",
			"the legacy migration window closed at %s: legacy verification is refused after that date (§2.4)",
			until.UTC().Format(time.RFC3339))
	}
}

func parseUntil(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse date %q", s)
}

func (c *Config) validateProviders(col *collector, providers map[string]*Provider) {
	for i := range c.Providers {
		p := &c.Providers[i]
		path := fmt.Sprintf("providers[%d]", i)
		switch {
		case p.Name == "":
			col.add(path+".name", "must not be empty")
		default:
			if _, dup := providers[p.Name]; dup {
				col.add(path+".name", "duplicate provider name %q", p.Name)
			} else {
				providers[p.Name] = p
			}
		}
		if p.Kind == "" {
			col.add(path+".kind", "must name a provider kind (§4.3)")
		}
		nonNegative(col, path+".max_concurrency", int64(p.MaxConcurrency))
		nonNegative(col, path+".retry.max_attempts", int64(p.Retry.MaxAttempts))
		if p.Retry.Backoff != "" {
			mustBeOneOf(col, path+".retry.backoff", p.Retry.Backoff, backoffKinds)
		}
		nonNegative(col, path+".retry.base", int64(p.Retry.Base))
		if p.CapacityGroup != "" {
			if _, ok := c.Capacity.ProviderGroups[p.CapacityGroup]; !ok {
				col.add(path+".capacity_group",
					"capacity group %q is not declared under capacity.provider_groups", p.CapacityGroup)
			}
		}
		if p.UsageProbe.Enabled && p.UsageProbe.Fetcher == "" {
			col.add(path+".usage_probe.fetcher", "must name a fetcher when the usage probe is enabled")
		}
		nonNegative(col, path+".usage_probe.interval", int64(p.UsageProbe.Interval))
		if p.Metrics.Enabled && p.Metrics.Endpoint == "" {
			col.add(path+".metrics.endpoint", "must be set when provider metrics collection is enabled")
		}
		for j, d := range p.Params.Drop {
			if strings.TrimSpace(d) == "" {
				col.add(fmt.Sprintf("%s.params.drop[%d]", path, j), "must not be empty")
			}
		}
	}
}

func (c *Config) validateCredentials(col *collector, providers map[string]*Provider,
	credentials map[string]*Credential, env string) {

	for i := range c.Credentials {
		cr := &c.Credentials[i]
		path := fmt.Sprintf("credentials[%d]", i)
		switch {
		case cr.ID == "":
			col.add(path+".id", "must not be empty")
		default:
			if _, dup := credentials[cr.ID]; dup {
				col.add(path+".id", "duplicate credential id %q", cr.ID)
			} else {
				credentials[cr.ID] = cr
			}
		}
		switch {
		case cr.Provider == "":
			col.add(path+".provider", "must name a provider")
		default:
			if _, ok := providers[cr.Provider]; !ok {
				col.add(path+".provider", "provider %q is not declared under providers", cr.Provider)
			}
		}
		cr.Key.validate(env, path, col)
		if cr.CapacityGroup != "" {
			if _, ok := c.Capacity.CredentialGroups[cr.CapacityGroup]; !ok {
				col.add(path+".capacity_group",
					"capacity group %q is not declared under capacity.credential_groups", cr.CapacityGroup)
			}
		}
	}
}

func (c *Config) validateCapacity(col *collector, providers map[string]*Provider) {
	for _, g := range sortedKeys(c.Capacity.ProviderGroups) {
		validateLimits(col, fmt.Sprintf("capacity.provider_groups[%q]", g), c.Capacity.ProviderGroups[g])
	}
	for _, g := range sortedKeys(c.Capacity.CredentialGroups) {
		validateLimits(col, fmt.Sprintf("capacity.credential_groups[%q]", g), c.Capacity.CredentialGroups[g])
	}
	for i, m := range c.Capacity.Models {
		path := fmt.Sprintf("capacity.models[%d]", i)
		switch {
		case m.Provider == "":
			col.add(path+".provider", "must name a provider")
		default:
			if _, ok := providers[m.Provider]; !ok {
				col.add(path+".provider", "provider %q is not declared under providers", m.Provider)
			}
		}
		if m.Model == "" {
			col.add(path+".model", "must name the upstream model this ceiling counts over")
		}
		validateLimits(col, path, m.Limits)
	}
	for _, name := range sortedKeys(c.Capacity.Principals) {
		p := c.Capacity.Principals[name]
		path := fmt.Sprintf("capacity.principals[%q]", name)
		nonNegative(col, path+".max_concurrent", int64(p.MaxConcurrent))
		nonNegative(col, path+".max_queue_wait", int64(p.MaxQueueWait))
		nonNegative(col, path+".rpm", int64(p.RPM))
		nonNegative(col, path+".tpm", p.TPM)
		nonNegative(col, path+".max_queue", int64(p.MaxQueue))
	}
	if c.Capacity.Global != nil {
		validateLimits(col, "capacity.global", *c.Capacity.Global)
	}
	if r := c.Capacity.Reserve(); r < 0 || r >= 1 {
		col.add("capacity.interactive_reserve",
			"must be in [0,1) (is %v): it is the fraction of every axis batch work may not occupy (§11.1)", r)
	}
}

func validateLimits(col *collector, path string, l CapacityLimits) {
	nonNegative(col, path+".max_concurrency", int64(l.MaxConcurrency))
	nonNegative(col, path+".rpm", int64(l.RPM))
	nonNegative(col, path+".tpm", l.TPM)
	nonNegative(col, path+".max_queue", int64(l.MaxQueue))
}

func (c *Config) validateKeyRotation(col *collector, providers map[string]*Provider,
	credentials map[string]*Credential, env string) {

	if c.KeyRotation.Strategy != "" {
		mustBeOneOf(col, "key_rotation.strategy", c.KeyRotation.Strategy, rotationStrategy)
	}
	for _, name := range sortedKeys(c.KeyRotation.Providers) {
		kp := c.KeyRotation.Providers[name]
		path := fmt.Sprintf("key_rotation.providers[%q]", name)
		if _, ok := providers[name]; !ok {
			col.add(path, "provider %q is not declared under providers", name)
		}
		if kp.Stickiness.Scope != "" {
			mustBeOneOf(col, path+".stickiness.scope", kp.Stickiness.Scope, stickyScopes)
		}
		if kp.Stickiness.OnCapacity != "" {
			mustBeOneOf(col, path+".stickiness.on_capacity", kp.Stickiness.OnCapacity, onCapacityModes)
		}
		seen := map[string]bool{}
		for i, k := range kp.Keys {
			kpath := fmt.Sprintf("%s.keys[%d]", path, i)
			switch {
			case k.ID == "":
				col.add(kpath+".id", "must not be empty")
			case seen[k.ID]:
				col.add(kpath+".id", "duplicate key id %q in this rotation pool", k.ID)
			default:
				seen[k.ID] = true
				if cr, ok := credentials[k.ID]; ok && cr.Provider != name {
					col.add(kpath+".id",
						"key id %q is a credential of provider %q, but this rotation pool is for provider %q",
						k.ID, cr.Provider, name)
				}
			}
			k.Key.validate(env, kpath, col)
			nonNegative(col, kpath+".max_concurrency", int64(k.MaxConcurrency))
			if k.CapacityGroup != "" {
				if _, ok := c.Capacity.CredentialGroups[k.CapacityGroup]; !ok {
					col.add(kpath+".capacity_group",
						"capacity group %q is not declared under capacity.credential_groups", k.CapacityGroup)
				}
			}
		}
	}
}

func (c *Config) validateModels(col *collector, providers map[string]*Provider,
	credentials map[string]*Credential) map[string]*Model {

	models := map[string]*Model{}
	for i := range c.Models {
		m := &c.Models[i]
		path := fmt.Sprintf("models[%d]", i)
		switch {
		case m.Name == "":
			col.add(path+".name", "must not be empty")
		default:
			if _, dup := models[m.Name]; dup {
				col.add(path+".name", "duplicate model name %q", m.Name)
			} else {
				models[m.Name] = m
			}
		}
		if m.Class != "" {
			members, ok := c.Classes[m.Class]
			if !ok {
				col.add(path+".class", "class %q is not declared under classes", m.Class)
			} else if m.Name != "" && !containsString(members, m.Name) {
				col.add(path+".class",
					"model %q declares class %q but is not a member of it under classes[%q]",
					m.Name, m.Class, m.Class)
			}
		}
		for j, s := range m.Strategy {
			mustBeOneOf(col, fmt.Sprintf("%s.strategy[%d]", path, j), s, strategies)
		}
		if len(m.Deployments) == 0 {
			col.add(path+".deployments", "model %q has no deployments", m.Name)
		}
		for j := range m.Deployments {
			d := &m.Deployments[j]
			dpath := fmt.Sprintf("%s.deployments[%d]", path, j)
			switch {
			case d.Provider == "":
				col.add(dpath+".provider", "must name a provider")
			default:
				if _, ok := providers[d.Provider]; !ok {
					col.add(dpath+".provider", "provider %q is not declared under providers", d.Provider)
				}
			}
			if d.UpstreamModel == "" {
				col.add(dpath+".upstream_model",
					"must name the model to send upstream (the name is used verbatim; it is never parsed, §2.1)")
			}
			for k, id := range d.Credentials {
				cpath := fmt.Sprintf("%s.credentials[%d]", dpath, k)
				cr, ok := credentials[id]
				if !ok {
					col.add(cpath, "credential %q is not declared under credentials", id)
					continue
				}
				if d.Provider != "" && cr.Provider != d.Provider {
					col.add(cpath,
						"credential %q belongs to provider %q, but this deployment is on provider %q",
						id, cr.Provider, d.Provider)
				}
			}
			nonNegative(col, dpath+".weight", int64(d.Weight))
			nonNegative(col, dpath+".priority", int64(d.Priority))
			nonNegative(col, dpath+".timeout", int64(d.Timeout))
			nonNegative(col, dpath+".stream_timeout", int64(d.StreamTimeout))
			for k, l := range d.Limits {
				lpath := fmt.Sprintf("%s.limits[%d]", dpath, k)
				mustBeOneOf(col, lpath+".metric", l.Metric, metricNames)
				nonNegative(col, lpath+".value", l.Value)
			}
		}
	}
	return models
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (c *Config) validateAliasesAndClasses(col *collector, models map[string]*Model) {
	for _, alias := range sortedKeys(c.Aliases) {
		target := c.Aliases[alias]
		path := fmt.Sprintf("aliases[%q]", alias)
		if alias == "" {
			col.add("aliases", "an alias name must not be empty")
			continue
		}
		if _, clash := models[alias]; clash {
			col.add(path, "alias %q is also a declared model name: the same name cannot mean two things", alias)
		}
		if target == "" {
			col.add(path, "must name a model")
			continue
		}
		if _, ok := models[target]; !ok {
			if _, isAlias := c.Aliases[target]; isAlias {
				col.add(path, "alias target %q is itself an alias: aliases do not chain, "+
					"an alias must name a model group", target)
			} else {
				col.add(path, "alias target %q is not a declared model", target)
			}
		}
	}
	for _, class := range sortedKeys(c.Classes) {
		members := c.Classes[class]
		path := fmt.Sprintf("classes[%q]", class)
		if class == "" {
			col.add("classes", "a class name must not be empty")
			continue
		}
		if len(members) == 0 {
			col.add(path, "class %q has no members", class)
		}
		seen := map[string]bool{}
		for i, m := range members {
			mpath := fmt.Sprintf("%s[%d]", path, i)
			if seen[m] {
				col.add(mpath, "model %q is listed twice in class %q", m, class)
			}
			seen[m] = true
			if _, ok := models[m]; !ok {
				col.add(mpath, "class member %q is not a declared model", m)
			}
		}
	}
}

func (c *Config) validateRouting(col *collector) {
	if c.Routing.Sticky.IsEnabled() {
		if c.Routing.Sticky.TTL <= 0 {
			col.add("routing.sticky.ttl", "must be greater than zero when session stickiness is enabled")
		}
		if c.Routing.Sticky.PurgeInterval < 0 {
			col.add("routing.sticky.purge_interval", "must not be negative")
		}
		if len(c.Routing.Sticky.Key) == 0 {
			col.add("routing.sticky.key", "must list at least one key component")
		}
		for i, k := range c.Routing.Sticky.Key {
			mustBeOneOf(col, fmt.Sprintf("routing.sticky.key[%d]", i), k, stickyKeyParts)
		}
	}
	if c.Routing.Prefix.IsEnabled() {
		if c.Routing.Prefix.ChunkBytes <= 0 {
			col.add("routing.prefix.chunk_bytes",
				"must be greater than zero: prefix chunks are cut at byte boundaries (§7.4b)")
		}
		mustBeOneOf(col, "routing.prefix.checkpoints", c.Routing.Prefix.Checkpoints, checkpointKinds)
		if c.Routing.Prefix.MaxBytes <= 0 {
			col.add("routing.prefix.max_bytes",
				"must be greater than zero: the prefix table is budgeted by retained bytes (§7.4b)")
		}
		if c.Routing.Prefix.TTL <= 0 {
			col.add("routing.prefix.ttl", "must be greater than zero when prefix affinity is enabled")
		}
	}
}

func (c *Config) validateFallbacks(col *collector) {
	for _, cause := range sortedKeys(c.Fallbacks.On) {
		path := fmt.Sprintf("fallbacks.on[%q]", cause)
		if !oneOf(cause, fallbackCauses) {
			col.add(path, "%q is not a known fallback cause: want one of %s",
				cause, strings.Join(fallbackCauses, ", "))
			continue
		}
		chain := c.Fallbacks.On[cause]
		if (cause == CauseBudgetExceeded || cause == CauseAuth) && len(chain) > 0 {
			col.add(path, "%q must have an empty chain: exceeding a budget or failing authentication "+
				"is not a fallback condition, failing is the correct outcome (§7.6)", cause)
		}
		for i, t := range chain {
			mustBeOneOf(col, fmt.Sprintf("%s[%d]", path, i), t, fallbackTargets)
		}
	}
	nonNegative(col, "fallbacks.max_hops", int64(c.Fallbacks.MaxHops))
	nonNegative(col, "fallbacks.budget_ms", int64(c.Fallbacks.BudgetMS))
}

func (c *Config) validatePricing(col *collector, providers map[string]*Provider,
	credentials map[string]*Credential) {

	if c.Pricing.Currency == "" {
		col.add("pricing.currency", "must not be empty")
	}
	ids := map[string]bool{}
	for i := range c.Pricing.Rules {
		r := &c.Pricing.Rules[i]
		path := fmt.Sprintf("pricing.rules[%d]", i)
		if r.ID != "" {
			if ids[r.ID] {
				col.add(path+".id", "duplicate pricing rule id %q", r.ID)
			}
			ids[r.ID] = true
		}
		mustBeOneOf(col, path+".class", r.Class, pricingClasses)
		nonNegative(col, path+".priority", int64(r.Priority))
		if r.Match.Provider != "" {
			if _, ok := providers[r.Match.Provider]; !ok {
				col.add(path+".match.provider", "provider %q is not declared under providers", r.Match.Provider)
			}
		}
		if r.Match.Credential != "" {
			if _, ok := credentials[r.Match.Credential]; !ok {
				col.add(path+".match.credential", "credential %q is not declared under credentials",
					r.Match.Credential)
			}
		}
		for _, comp := range sortedKeys(r.Rates) {
			cpath := fmt.Sprintf("%s.rates[%q]", path, comp)
			if !oneOf(comp, pricingComponents) {
				col.add(cpath, "%q is not a priced component: want one of %s",
					comp, strings.Join(pricingComponents, ", "))
			}
			if err := checkDecimal(string(r.Rates[comp])); err != nil {
				col.add(cpath, "%v", err)
			}
		}
		switch r.Class {
		case PricingMarginalUsage:
			if len(r.Rates) == 0 {
				col.add(path+".rates", "a marginal_usage rule must price at least one component")
			}
		case PricingFixedSubscription:
			if r.Period == "" {
				col.add(path+".period", "a fixed_subscription rule must state its period")
			}
			if r.Amount.IsZero() {
				col.add(path+".amount", "a fixed_subscription rule must state its period cost")
			} else if err := checkDecimal(string(r.Amount)); err != nil {
				col.add(path+".amount", "%v", err)
			}
		case PricingAdjustment:
			if r.Percent.IsZero() {
				col.add(path+".percent", "an adjustment rule must state a percentage")
			} else if err := checkDecimal(string(r.Percent)); err != nil {
				col.add(path+".percent", "%v", err)
			}
		}
	}
}

func (c *Config) validateMetering(col *collector) {
	mustBeOneOf(col, "metering.trace.store_messages", c.Metering.Trace.StoreMessages, storeMessages)
	nonNegative(col, "metering.trace.truncate_chars", int64(c.Metering.Trace.TruncateChars))
	if r := c.Metering.Trace.Rate(); r < 0 || r > 1 {
		col.add("metering.trace.sample_rate", "must be in [0,1] (is %v)", r)
	}
	nonNegative(col, "metering.trace.daily_byte_budget", int64(c.Metering.Trace.DailyByteBudget))
	nonNegative(col, "metering.spool.max_bytes", int64(c.Metering.Spool.MaxBytes))
	if c.Metering.Spool.Dir == "" {
		col.add("metering.spool.dir", "must not be empty: trace payloads spool to disk before the store (§12.1)")
	}
	if c.Metering.FlushInterval <= 0 {
		col.add("metering.flush_interval", "must be greater than zero")
	}
	if !c.Metering.Numeric.IsEnabled() {
		col.add("metering.numeric.enabled",
			"numeric accounting cannot be turned off: cost, tokens and error counts must survive "+
				"any back-pressure (§12.1). Reduce metering.trace.sample_rate instead")
	}
}

func (c *Config) validateObservability(col *collector) {
	mustBeOneOf(col, "observability.log_level", c.Observability.LogLevel, logLevels)
	mustBeOneOf(col, "observability.log_format", c.Observability.LogFormat, logFormats)
}

func (c *Config) validateExtensions(col *collector) {
	lua := c.Extensions.Lua
	if lua.Enabled && lua.Dir == "" {
		col.add("extensions.lua.dir", "must be set when Lua extensions are enabled")
	}
	for i, h := range lua.Hooks {
		mustBeOneOf(col, fmt.Sprintf("extensions.lua.hooks[%d]", i), h, luaHooks)
	}
	nonNegative(col, "extensions.lua.limits.instructions", lua.Limits.Instructions)
	nonNegative(col, "extensions.lua.limits.memory_mb", int64(lua.Limits.MemoryMB))
	nonNegative(col, "extensions.lua.limits.timeout", int64(lua.Limits.Timeout))
	if lua.Enabled {
		if lua.Limits.Instructions == 0 || lua.Limits.MemoryMB == 0 || lua.Limits.Timeout == 0 {
			col.add("extensions.lua.limits",
				"an enabled hook needs instruction, memory and wall-clock ceilings (§11.5)")
		}
	}
}

func (c *Config) validatePassthrough(col *collector, providers map[string]*Provider) {
	mustBeOneOf(col, "passthrough.default.auth", c.Passthrough.Default.Auth, passthroughAuth)
	nonNegative(col, "passthrough.default.timeout", int64(c.Passthrough.Default.Timeout))
	seen := map[string]bool{}
	for i, r := range c.Passthrough.Routes {
		path := fmt.Sprintf("passthrough.routes[%d]", i)
		switch {
		case r.Prefix == "":
			col.add(path+".prefix", "must not be empty")
		case !strings.HasPrefix(r.Prefix, "/"):
			col.add(path+".prefix", "must start with \"/\" (is %q)", r.Prefix)
		case strings.Contains(r.Prefix, ".."):
			col.add(path+".prefix", "must not contain \"..\": joined paths are normalized and "+
				"traversal is rejected (§10.6)")
		case seen[r.Prefix]:
			col.add(path+".prefix", "duplicate passthrough prefix %q", r.Prefix)
		default:
			seen[r.Prefix] = true
		}
		switch {
		case r.Provider == "":
			col.add(path+".provider", "must name a provider")
		default:
			if _, ok := providers[r.Provider]; !ok {
				col.add(path+".provider", "provider %q is not declared under providers", r.Provider)
			}
		}
		if r.Auth != "" {
			mustBeOneOf(col, path+".auth", r.Auth, passthroughAuth)
		}
		nonNegative(col, path+".timeout", int64(r.Timeout))
	}
}

func (c *Config) validateShadow(col *collector) {
	mustBeOneOf(col, "shadow.mode", c.Shadow.Mode, shadowModes)
	if c.Shadow.Mode == ShadowModeOff {
		return
	}
	if c.Shadow.Reference.URL == "" {
		col.add("shadow.reference.url", "must be set when shadow.mode is %q", c.Shadow.Mode)
	} else if err := checkShadowReferenceURL(c.Shadow.Reference.URL); err != nil {
		col.add("shadow.reference.url", "%v", err)
	}
	if c.Shadow.SampleRate < 0 || c.Shadow.SampleRate > 1 {
		col.add("shadow.sample_rate", "must be in [0,1] (is %v)", c.Shadow.SampleRate)
	}

	// Both modes send every sampled request twice, so both need the ceiling.
	// An earlier revision required it for mirror only, which is backwards:
	// compare is mirror plus a diff, so it costs at least as much.
	if !c.Shadow.MaxCostUSDPerDay.IsZero() {
		if err := checkDecimal(string(c.Shadow.MaxCostUSDPerDay)); err != nil {
			col.add("shadow.max_cost_usd_per_day", "%v", err)
		}
	} else {
		col.add("shadow.max_cost_usd_per_day",
			"%s sends every sampled request twice and therefore costs twice: "+
				"set a daily cost ceiling (§14.1)", c.Shadow.Mode)
	}
	if !c.Shadow.UnpricedEstimateUSD.IsZero() {
		if err := checkDecimal(string(c.Shadow.UnpricedEstimateUSD)); err != nil {
			col.add("shadow.unpriced_estimate_usd", "%v", err)
		}
	}

	// §14.1 names compare.semantic and defines no comparison for it. Accepting
	// it silently would mean an operator who asked for semantic comparison gets
	// structural comparison and an empty diff report, and reads that report as
	// proof of something it never checked — which is the one failure mode a
	// cutover gate cannot have.
	if c.Shadow.Compare.Semantic {
		col.add("shadow.compare.semantic",
			"semantic comparison is not implemented: §14.1 names the knob but "+
				"specifies no comparison for it, and a gate that silently checks "+
				"less than it was asked to is worse than one that refuses")
	}
	if c.Shadow.Mode == ShadowModeCompare && !c.Shadow.Compare.IsStructural() {
		col.add("shadow.compare.structural",
			"mode is %q but no comparison is enabled; use mode: mirror instead",
			ShadowModeCompare)
	}
	for i, f := range c.Shadow.Compare.IgnoreFields {
		if strings.TrimSpace(f) == "" {
			col.add(fmt.Sprintf("shadow.compare.ignore_fields[%d]", i), "must not be empty")
		}
	}

	if c.Shadow.QueueSize <= 0 {
		col.add("shadow.queue_size", "must be greater than zero")
	}
	if c.Shadow.Workers <= 0 {
		col.add("shadow.workers", "must be greater than zero")
	}
	nonNegative(col, "shadow.reference.timeout", int64(c.Shadow.Reference.Timeout))
	if c.Shadow.Capture.HeadBytes <= 0 {
		col.add("shadow.capture.head_bytes",
			"must be greater than zero: a comparison needs the response it compares")
	}
	nonNegative(col, "shadow.capture.tail_bytes", int64(c.Shadow.Capture.TailBytes))
	if c.Shadow.Mode == ShadowModeCompare && c.Shadow.Report.Path == "" {
		col.add("shadow.report.path",
			"must be set when shadow.mode is %q: an empty diff report is the "+
				"completion criterion for taking over traffic (§14.1), and a report "+
				"that is not written is not empty, it is absent", ShadowModeCompare)
	}
	nonNegative(col, "shadow.report.max_bytes", int64(c.Shadow.Report.MaxBytes))
}

// checkWebhookURL rejects a notification endpoint dorang cannot POST to.
//
// Unlike a shadow reference (below) a query string is fine here: the URL is
// used whole rather than joined with a request path, and every hosted webhook
// carries its token in one.
func checkWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("invalid URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid URL %q: no host", raw)
	}
	return nil
}

// checkShadowReferenceURL rejects a reference dorang cannot call, and one it
// must not call.
func checkShadowReferenceURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("invalid URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid URL %q: no host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid URL %q: a query or fragment cannot survive "+
			"being joined with the request path", raw)
	}
	return nil
}

func (c *Config) validateNotifications(col *collector) {
	n := &c.Notifications
	mustBeOneOf(col, "notifications.email.driver", n.Email.Driver, emailDrivers)
	for i, e := range n.Events {
		mustBeOneOf(col, fmt.Sprintf("notifications.events[%d]", i), e, notificationEvents)
	}
	for _, name := range sortedKeys(n.DedupPeriods) {
		mustBeOneOf(col, fmt.Sprintf("notifications.dedup_periods[%q]", name), name, notificationEvents)
		nonNegative(col, fmt.Sprintf("notifications.dedup_periods[%q]", name),
			int64(n.DedupPeriods[name]))
	}
	nonNegative(col, "notifications.queue_size", int64(n.QueueSize))
	nonNegative(col, "notifications.workers", int64(n.Workers))
	nonNegative(col, "notifications.dedup_period", int64(n.DedupPeriod))
	nonNegative(col, "notifications.retry.max_attempts", int64(n.Retry.MaxAttempts))
	nonNegative(col, "notifications.retry.initial_backoff", int64(n.Retry.InitialBackoff))
	nonNegative(col, "notifications.retry.max_backoff", int64(n.Retry.MaxBackoff))
	nonNegative(col, "notifications.retry.breaker_threshold", int64(n.Retry.BreakerThreshold))
	nonNegative(col, "notifications.retry.breaker_cooldown", int64(n.Retry.BreakerCooldown))
	if n.Retry.MaxBackoff > 0 && n.Retry.InitialBackoff > n.Retry.MaxBackoff {
		col.add("notifications.retry.max_backoff",
			"must not be shorter than initial_backoff (%s < %s)",
			n.Retry.MaxBackoff, n.Retry.InitialBackoff)
	}

	// Recipients are checked for every driver that has one, because a `to:`
	// that is not an address fails at delivery time — which is to say, during
	// the incident the notification was meant to report.
	for i, addr := range n.Email.To {
		if !strings.Contains(addr, "@") {
			col.add(fmt.Sprintf("notifications.email.to[%d]", i),
				"%q is not an email address", addr)
		}
	}
	if n.Email.From != "" && !strings.Contains(n.Email.From, "@") {
		col.add("notifications.email.from", "%q is not an email address", n.Email.From)
	}

	switch n.Email.Driver {
	case "smtp":
		if n.Email.SMTP.Addr == "" {
			col.add("notifications.email.smtp.addr",
				"must be set as host:port when the driver is smtp")
		} else if _, _, err := net.SplitHostPort(n.Email.SMTP.Addr); err != nil {
			col.add("notifications.email.smtp.addr", "must be host:port (is %q)", n.Email.SMTP.Addr)
		}
		if n.Email.From == "" {
			col.add("notifications.email.from", "must be set when the driver is smtp")
		}
		if len(n.Email.To) == 0 {
			col.add("notifications.email.to", "must list at least one recipient when the driver is smtp")
		}
		if !n.Email.SMTP.Password.IsZero() {
			n.Email.SMTP.Password.validate(c.Server.Env, "notifications.email.smtp", col)
			if n.Email.SMTP.Username == "" {
				col.add("notifications.email.smtp.username",
					"must be set alongside an SMTP password")
			}
		}
		if n.Email.SMTP.Username != "" && !n.Email.SMTP.StartTLS {
			// net/smtp refuses PLAIN over an unencrypted connection to anything
			// but a loopback server, so this would fail at delivery time rather
			// than here. Saying so at load is the cheaper discovery.
			col.add("notifications.email.smtp.starttls",
				"must be true when a username is set: a password is not sent over an "+
					"unencrypted connection")
		}
		nonNegative(col, "notifications.email.smtp.timeout", int64(n.Email.SMTP.Timeout))
	case "http":
		if n.Email.HTTP.URL == "" {
			col.add("notifications.email.http.url", "must be set when the driver is http")
		} else if err := checkWebhookURL(n.Email.HTTP.URL); err != nil {
			col.add("notifications.email.http.url", "%s", err)
		}
		// §11.5 rule 1: a webhook delivery is signed. The secret is required
		// rather than optional because an unsigned receiver cannot tell a real
		// delivery from a forged one, and the payload carries budget and quota
		// state.
		if n.Email.HTTP.Secret.IsZero() {
			col.add("notifications.email.http.key_env",
				"must be set when the driver is http: a webhook delivery is signed "+
					"(HMAC-SHA256 over the body), and a receiver with no secret cannot "+
					"tell a real delivery from a forged one")
		} else {
			n.Email.HTTP.Secret.validate(c.Server.Env, "notifications.email.http", col)
		}
		nonNegative(col, "notifications.email.http.timeout", int64(n.Email.HTTP.Timeout))
	case "lua":
		// The lua driver delivers through the on_email hook. A configuration
		// that selects it without enabling the hook is a mail system that
		// silently sends nothing, which is exactly the failure this whole
		// section is about.
		if !c.Extensions.Lua.Enabled {
			col.add("notifications.email.driver",
				"the lua driver delivers through the on_email hook, "+
					"but extensions.lua.enabled is false")
		} else if len(c.Extensions.Lua.Hooks) > 0 && !oneOf("on_email", c.Extensions.Lua.Hooks) {
			col.add("notifications.email.driver",
				"the lua driver delivers through the on_email hook, "+
					"but extensions.lua.hooks does not list it")
		}
	}
}

func (c *Config) validatePriorityMapping(col *collector) {
	for _, name := range sortedKeys(c.PriorityMapping.Classes) {
		if c.PriorityMapping.Classes[name] < 0 {
			col.add(fmt.Sprintf("priority_mapping.classes[%q]", name), "must not be negative")
		}
	}
	if c.PriorityMapping.Emit.Header == "" && len(c.PriorityMapping.Emit.Backends) == 0 {
		col.add("priority_mapping.emit.header",
			"must be set: a backend that understands no native priority field still receives the header (§7.5)")
	}
	for _, backend := range sortedKeys(c.PriorityMapping.Emit.Backends) {
		b := c.PriorityMapping.Emit.Backends[backend]
		path := fmt.Sprintf("priority_mapping.emit[%q]", backend)
		if b.Field == "" {
			col.add(path+".field", "must name the backend's native priority field")
		}
		for _, class := range sortedKeys(b.Map) {
			if _, ok := c.PriorityMapping.Classes[class]; !ok {
				col.add(fmt.Sprintf("%s.map[%q]", path, class),
					"%q is not a declared priority class", class)
			}
			if b.Map[class] == "" {
				col.add(fmt.Sprintf("%s.map[%q]", path, class), "must not be empty")
			}
		}
	}
}

// validateLeasedLimits enforces §5.6: leased mode divides a ceiling into blocks
// across nodes, and a single-digit limit cannot be usefully divided.
func (c *Config) validateLeasedLimits(col *collector) {
	if !c.Cluster.Enabled || c.Cluster.CapacityMode != CapacityModeLeased || c.Cluster.MinLeasable <= 0 {
		return
	}
	min := c.Cluster.MinLeasable
	report := func(path string, v int) {
		if v > 0 && v < min {
			col.add(path, "limit %d is below cluster.min_leasable (%d): a limit this small cannot be "+
				"usefully divided across nodes, so it needs a shared capacity mode (§5.6)", v, min)
		}
	}
	for i := range c.Providers {
		report(fmt.Sprintf("providers[%d].max_concurrency", i), c.Providers[i].MaxConcurrency)
	}
	for _, g := range sortedKeys(c.Capacity.ProviderGroups) {
		report(fmt.Sprintf("capacity.provider_groups[%q].max_concurrency", g),
			c.Capacity.ProviderGroups[g].MaxConcurrency)
	}
	for _, g := range sortedKeys(c.Capacity.CredentialGroups) {
		report(fmt.Sprintf("capacity.credential_groups[%q].max_concurrency", g),
			c.Capacity.CredentialGroups[g].MaxConcurrency)
	}
	for i, m := range c.Capacity.Models {
		report(fmt.Sprintf("capacity.models[%d].max_concurrency", i), m.Limits.MaxConcurrency)
	}
	for _, name := range sortedKeys(c.KeyRotation.Providers) {
		kp := c.KeyRotation.Providers[name]
		for i, k := range kp.Keys {
			report(fmt.Sprintf("key_rotation.providers[%q].keys[%d].max_concurrency", name, i),
				k.MaxConcurrency)
		}
	}
}
