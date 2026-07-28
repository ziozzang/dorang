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
		"sticky", "prefix_sticky", "priority", "weighted_random", "quota_urgency",
	}
	clientPriorityModes = []string{ClientPriorityIgnore, ClientPriorityAllow}
	stickyKeyParts      = []string{"api_key", "session_id", "user", "team", "tenant"}
	checkpointKinds     = []string{"logarithmic", "fixed"}
	fallbackCauses      = []string{
		CauseRateLimit, CauseQuotaExhausted, CauseContextWindow, CauseContentPolicy,
		CauseUpstream5xx, CauseTimeout, CauseBudgetExceeded, CauseAuth,
	}
	fallbackTargets = []string{TargetSameGroup, TargetSameClass, TargetSameClassLarger}
	pricingClasses  = []string{
		PricingMarginalUsage, PricingFixedSubscription, PricingAdjustment, PricingNotionalRate,
	}
	storeMessages   = []string{StoreMessagesNone, StoreMessagesHash, StoreMessagesTruncated}
	logLevels       = []string{"debug", "info", "warn", "error"}
	logFormats      = []string{"json", "text"}
	filterFailModes = []string{FilterFailClosed, FilterFailOpen}
	filterPhases    = []string{FilterOnRequest, FilterOnResponse}
	filterScopes    = []string{
		FilterScopeConversation, FilterScopePrincipal, FilterScopeTenant, FilterScopeRequest,
	}
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
	c.validateFilters(col)
	c.validatePassthrough(col, providers)
	c.validateShadow(col)
	c.validateNotifications(col)
	c.validatePriorityMapping(col)
	c.validateLeasedLimits(col)
	c.validateCompat(col)
}

// validateCompat checks the three divergence switches of COMPATIBILITY §3.3,
// §6.8 and §7.7.
//
// All three are now served at both of their values, so the only thing left to
// check is that the spelling is one this schema knows. Two of them used to be
// REFUSED at their non-default value, and the refusal message named the hop that
// was missing — a field on backend.Call, and the two lines in internal/app that
// fill it. That hop exists, so the refusal is gone with it: keeping it would
// have been a validator rejecting a configuration the gateway can honour, which
// is the same class of lie as accepting one it cannot.
//
// The unknown-value branch stays. It is a different check from the one that was
// removed — it catches a typo, not a gap in the build — and it is the reason
// `usage_chunk_choices: stubb` is a load error rather than a silent fallback to
// the default.
func (c *Config) validateCompat(col *collector) {
	switch c.Compat.UsageChunkChoices {
	case "", UsageChunkChoicesStub, UsageChunkChoicesEmpty:
	default:
		col.add("compat.usage_chunk_choices",
			"unknown value %q: expected %q or %q",
			c.Compat.UsageChunkChoices, UsageChunkChoicesStub, UsageChunkChoicesEmpty)
	}
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
	c.validateRotation(col)
	c.validateRevocation(col)
	c.validateTiers(col)
	c.validateTokenGuard(col)
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

// validateRotation checks the §11.2c block.
func (c *Config) validateRotation(col *collector) {
	r := c.Auth.Rotation
	nonNegative(col, "auth.rotation.grace", int64(r.Grace))
	nonNegative(col, "auth.rotation.max_age", int64(r.MaxAge))
	if r.MaxSecrets < 2 {
		// One secret means a rotation has to cut the old one in the same
		// instant it mints the new one, which is a re-provisioning wearing a
		// rotation's name: there is no window in which a client can roll.
		col.add("auth.rotation.max_secrets",
			"must be at least 2: rotation mints a new secret and leaves the old one valid for a "+
				"grace period, and %d leaves no overlap in flight (§11.2c)", r.MaxSecrets)
	}
	if r.MaxAge > 0 && r.Grace > 0 && time.Duration(r.Grace) >= time.Duration(r.MaxAge) {
		col.add("auth.rotation.grace",
			"the grace period (%s) is not shorter than max_age (%s), so a secret would be overdue "+
				"for rotation before the previous rotation's window had closed",
			time.Duration(r.Grace), time.Duration(r.MaxAge))
	}
}

// validateRevocation checks the §11.2c / W11 block.
func (c *Config) validateRevocation(col *collector) {
	r := c.Auth.Revocation
	for path, v := range map[string]Duration{
		"auth.revocation.entry_ttl":     r.EntryTTL,
		"auth.revocation.negative_ttl":  r.NegativeTTL,
		"auth.revocation.poll":          r.Poll,
		"auth.revocation.retain":        r.Retain,
		"auth.revocation.store_latency": r.StoreLatency,
	} {
		nonNegative(col, path, int64(v))
	}
	if r.NegativeTTL > r.EntryTTL {
		// §11.2c rule 3, refused rather than clamped: a key that was refused is
		// cheap to re-check and a key that is serving is not, and holding
		// refusals for longer inverts the rule rather than merely weakening it.
		col.add("auth.revocation.negative_ttl",
			"must not exceed entry_ttl (%s): a refused key is cheap to re-check and a serving one "+
				"is not, and holding refusals longer is what makes a revocation window wide (§11.2c)",
			time.Duration(r.EntryTTL))
	}
	if r.Poll > 0 && r.Retain > 0 && r.Retain <= r.Poll {
		col.add("auth.revocation.retain",
			"must be longer than poll (%s), or a message can be pruned before every node has read it",
			time.Duration(r.Poll))
	}
	if r.EntryTTL > 0 && r.Poll+r.StoreLatency >= r.EntryTTL {
		// The invalidation path exists to be faster than the TTL it falls back
		// to. A configuration where it is not has the mechanism as decoration
		// on top of the thing it was built to replace.
		col.add("auth.revocation.poll",
			"poll + store_latency (%s) is not below entry_ttl (%s): the published revocation bound "+
				"would be no better than the cache TTL it is supposed to replace (§11.2c)",
			time.Duration(r.Poll+r.StoreLatency), time.Duration(r.EntryTTL))
	}
}

// validateTiers checks the §11.6 tier set.
//
// It does NOT check that a tier's limits are ordered against its neighbours'.
// An operator may legitimately grant a lower tier more of one axis and less of
// another, and inventing a monotonicity rule here would refuse configurations
// the design permits. What the ordering fixes is scheduling, and that is
// computed from position rather than declared.
func (c *Config) validateTiers(col *collector) {
	seen := map[string]bool{}
	for i, t := range c.Auth.Tiers {
		path := fmt.Sprintf("auth.tiers[%d]", i)
		name := strings.TrimSpace(t.Name)
		if name == "" {
			col.add(path+".name", "a tier must have a name")
			continue
		}
		if seen[name] {
			col.add(path+".name", "tier %q is defined twice", name)
		}
		seen[name] = true
		if t.RPMLimit != nil {
			nonNegative(col, path+".rpm_limit", *t.RPMLimit)
		}
		if t.TPMLimit != nil {
			nonNegative(col, path+".tpm_limit", *t.TPMLimit)
		}
		if t.MaxParallel != nil {
			nonNegative(col, path+".max_parallel_requests", *t.MaxParallel)
		}
		if t.PriorityClass != "" && len(c.PriorityMapping.Classes) > 0 {
			if _, ok := c.PriorityMapping.Classes[t.PriorityClass]; !ok {
				col.add(path+".priority_class",
					"%q is not one of priority_mapping.classes %v", t.PriorityClass,
					sortedKeys(c.PriorityMapping.Classes))
			}
		}
	}
	if d := strings.TrimSpace(c.Auth.DefaultTier); d != "" && len(c.Auth.Tiers) > 0 && !seen[d] {
		col.add("auth.default_tier",
			"%q is not one of auth.tiers %v; a default that names nothing is how every unassigned "+
				"key silently becomes unrestricted", d, sortedKeys(seen))
	}
}

// tokenGuardActions are the actions of §11.6. `throttle` is designed and not
// implemented, and it is listed so that naming it is refused with an
// explanation rather than silently downgraded to alert_only.
var tokenGuardActions = []string{"pend", "revoke", "alert_only", "throttle"}

// validateTokenGuard checks the §11.6 block.
func (c *Config) validateTokenGuard(col *collector) {
	g := c.TokenGuard
	mustBeOneOf(col, "token_guard.action", g.Action, tokenGuardActions)
	if g.Action == "throttle" {
		col.add("token_guard.action",
			"throttle is designed (§11.6) and not implemented; configure pend, revoke or "+
				"alert_only rather than have the guard do less than you asked")
	}
	nonNegative(col, "token_guard.baseline_window", int64(g.BaselineWindow))
	nonNegative(col, "token_guard.window", int64(g.Window))
	nonNegative(col, "token_guard.min_history", int64(g.MinHistory))
	nonNegative(col, "token_guard.cooldown", int64(g.Cooldown))
	nonNegative(col, "token_guard.trigger.min_absolute", g.Trigger.MinAbsolute)
	if g.Trigger.Factor < 0 {
		col.add("token_guard.trigger.factor", "must not be negative")
	}
	if !g.Enabled {
		return
	}
	// Both conditions must hold, so a guard configured with either one disabled
	// is a guard that fires on one condition — which is precisely the shape
	// §11.6 rules out. Refusing is better than firing hardest on the quietest
	// keys.
	if g.Trigger.Factor <= 1 {
		col.add("token_guard.trigger.factor",
			"must be greater than 1 when the guard is enabled: a factor of %g makes the relative "+
				"condition hold for any key at or above its own baseline (§11.6)", g.Trigger.Factor)
	}
	if g.Trigger.MinAbsolute <= 0 {
		col.add("token_guard.trigger.min_absolute",
			"must be greater than zero when the guard is enabled: without an absolute floor the "+
				"guard fires hardest on the quietest keys (§11.6)")
	}
	if g.BaselineWindow > 0 && g.Window > g.BaselineWindow {
		col.add("token_guard.window",
			"is longer than baseline_window (%s); the baseline would be computed from less traffic "+
				"than it is compared against", time.Duration(g.BaselineWindow))
	}
	if g.MinHistory <= 0 {
		col.add("token_guard.min_history",
			"must be greater than zero when the guard is enabled: a new key has no baseline, and "+
				"without a stated minimum of history every key trips on its first busy hour (§11.6)")
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
		checkCacheTTL(col, path+".prefix_ttl", p.PrefixTTL, false)
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
		validateCredentialAuth(col, cr, path, env)
		if cr.CapacityGroup != "" {
			if _, ok := c.Capacity.CredentialGroups[cr.CapacityGroup]; !ok {
				col.add(path+".capacity_group",
					"capacity group %q is not declared under capacity.credential_groups", cr.CapacityGroup)
			}
		}
	}
}

// oauthSources, oauthFormats and oauthEncodes are what `auth: oauth` accepts.
//
// They are spelled here rather than imported from internal/auth because
// internal/config imports no sibling (DESIGN §1). Two lists that can drift are
// one list that is wrong, so they are held together by an executable check —
// TestConfigAndAuthAgreeOnOAuthSpellings in internal/app, which is the place
// both packages are already imported. It fails when either side moves.
var (
	oauthSources = []string{"file", "exec", "env"}
	oauthFormats = []string{"claude", "codex", "gemini", "generic"}
	oauthEncodes = []string{"form", "json"}
)

// validateCredentialAuth checks that a credential says exactly one thing about
// how it authenticates.
//
// The two spellings are ALTERNATIVES on one object, not two shapes of object: a
// credential that carries both a key and an oauth block has two answers to the
// question the upstream call asks once, and picking one by precedence is how a
// deployment sends the wrong credential without a line in the file to explain it.
func validateCredentialAuth(col *collector, cr *Credential, path, env string) {
	switch cr.Auth {
	case "", AuthKey:
		if cr.OAuth != nil {
			col.add(path+".oauth",
				"an oauth block needs `auth: oauth`; without it this credential authenticates "+
					"with its key and the whole block is read by nothing")
			return
		}
		cr.Key.validate(env, path, col)
		return
	case AuthOAuth:
	default:
		col.add(path+".auth", "unknown value %q: want %q or %q", cr.Auth, AuthKey, AuthOAuth)
		return
	}

	if !cr.Key.IsZero() {
		col.add(path+".auth",
			"`auth: oauth` and a static key are alternatives: this credential sets both "+
				"(%s), and only one of them can authenticate a request", cr.Key.Source())
	}
	if cr.OAuth == nil {
		col.add(path+".oauth", "`auth: oauth` needs an oauth block naming the token store")
		return
	}
	validateOAuth(col, cr.OAuth, path+".oauth", env)
}

func validateOAuth(col *collector, o *OAuth, path, env string) {
	switch o.Source {
	case "", "file":
		if strings.TrimSpace(o.Path) == "" {
			col.add(path+".path", "`source: file` needs the path of the token store")
		}
		if len(o.Command) > 0 {
			col.add(path+".command", "`source: file` reads a file; command is read by nothing")
		}
		if o.EnvVar != "" {
			col.add(path+".env_var", "`source: file` reads a file; env_var is read by nothing")
		}
	case "exec":
		if len(o.Command) == 0 || strings.TrimSpace(o.Command[0]) == "" {
			col.add(path+".command", "`source: exec` needs a command to run")
		}
	case "env":
		if strings.TrimSpace(o.EnvVar) == "" {
			col.add(path+".env_var", "`source: env` needs the name of a variable")
		}
	default:
		col.add(path+".source", "unknown source %q: want one of %s",
			o.Source, strings.Join(oauthSources, ", "))
	}

	if o.Format != "" && !containsString(oauthFormats, o.Format) {
		col.add(path+".format", "unknown token store format %q: want one of %s",
			o.Format, strings.Join(oauthFormats, ", "))
	}
	if o.RefreshMargin < 0 {
		col.add(path+".refresh_margin", "must not be negative")
	}
	if o.PollInterval < 0 {
		col.add(path+".poll_interval", "must not be negative")
	}
	if o.ExecTimeout < 0 {
		col.add(path+".exec_timeout", "must not be negative")
	}

	if o.Refresh.IsZero() {
		// No endpoint is a supported deployment, not an omission: dorang reads
		// the store the vendor's CLI keeps current and never writes to it. It is
		// stated here because the alternative reading — "refresh silently does
		// not work" — is the defect class this file exists to catch.
		if o.Source != "exec" && o.Source != "env" && !o.Refresh.ClientSecret.IsZero() {
			col.add(path+".refresh", "a client secret is set with no token_url to send it to")
		}
		return
	}
	rpath := path + ".refresh"
	switch {
	case strings.TrimSpace(o.Refresh.TokenURL) == "":
		col.add(rpath+".token_url", "must be set when a refresh block is configured")
	case !strings.HasPrefix(o.Refresh.TokenURL, "https://") &&
		!strings.HasPrefix(o.Refresh.TokenURL, "http://127.0.0.1") &&
		!strings.HasPrefix(o.Refresh.TokenURL, "http://localhost") &&
		!strings.HasPrefix(o.Refresh.TokenURL, "http://[::1]"):
		col.add(rpath+".token_url",
			"must be https: the body of a refresh is a refresh token, and posting one in "+
				"clear hands the account to anything on the path")
	}
	if strings.TrimSpace(o.Refresh.ClientID) == "" {
		col.add(rpath+".client_id", "must be set when a refresh block is configured")
	}
	if o.Refresh.Encoding != "" && !containsString(oauthEncodes, o.Refresh.Encoding) {
		col.add(rpath+".encoding", "unknown encoding %q: want one of %s",
			o.Refresh.Encoding, strings.Join(oauthEncodes, ", "))
	}
	if o.Refresh.Timeout < 0 {
		col.add(rpath+".timeout", "must not be negative")
	}
	if !o.Refresh.ClientSecret.IsZero() {
		o.Refresh.ClientSecret.validate(env, rpath, col)
	}
	if o.Source == "exec" || o.Source == "env" {
		// The command or the environment owns the token, so a successor cannot
		// be written back. dorang would spend the refresh token and have nowhere
		// to record what it got — which is precisely §11.2b's
		// token_store_write_failed, arranged in advance by the configuration.
		col.add(rpath,
			"a refresh endpoint needs a writable store: `source: %s` is read-only, so the "+
				"exchanged refresh token could not be recorded anywhere and the next refresh "+
				"would fail with a token the provider has already invalidated", o.Source)
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
		nonNegative(col, path+".max_queue", int64(p.MaxQueue))
		refuseRate(col, path, int64(p.RPM), p.TPM, principalRateHome)
		c.validateClientPriority(col, path, p)
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
	nonNegative(col, path+".max_queue", int64(l.MaxQueue))
	refuseRate(col, path, int64(l.RPM), l.TPM, groupRateHome)
}

// The two homes a rate ceiling actually has. Both are enforced by
// internal/quota, which is the package that owns time windows; internal/capacity
// counts gauges and has no window at all.
const (
	groupRateHome = "put the ceiling on the deployment instead — " +
		"models[].deployments[].limits[] with metric: rpm or tpm, which becomes a " +
		"rolling-minute quota on every credential the deployment names (§10.2)"
	principalRateHome = "a per-caller rate ceiling belongs on the api key itself — " +
		"`dorangctl key create --rpm N --tpm N`, or rpm_limit/tpm_limit through the " +
		"admin API, both of which internal/quota enforces on the request path"
)

// refuseRate rejects a rate ceiling written on a capacity axis.
//
// §5.2 writes rpm and tpm beside max_concurrency, and the two are not the same
// mechanism: a gauge is released when a request finishes, a rate is not, so a
// rate needs a time window that the capacity broker does not have and should not
// grow. Accepting the key and counting nothing is the failure mode this rule
// exists to remove — an operator who believes they capped a rate has capped
// nothing, and finds out from the provider's bill.
func refuseRate(col *collector, path string, rpm, tpm int64, home string) {
	if rpm != 0 {
		col.add(path+".rpm",
			"a rate ceiling is not enforced on a capacity axis: capacity counts concurrent "+
				"reservations, which are released when a request finishes, and has no time "+
				"window to count a rate over. %s", home)
	}
	if tpm != 0 {
		col.add(path+".tpm",
			"a rate ceiling is not enforced on a capacity axis: capacity counts concurrent "+
				"reservations, which are released when a request finishes, and has no time "+
				"window to count a rate over. %s", home)
	}
}

// validateClientPriority checks the §10.5 grant.
//
// The range is written as class names because that is what §10.5 writes, and
// because a caller's grant is expressed in the same vocabulary as the class the
// operator assigned them. A range naming a class that does not exist is refused
// rather than silently clamped to nothing.
func (c *Config) validateClientPriority(col *collector, path string, p PrincipalLimits) {
	if p.ClientPriority != "" {
		mustBeOneOf(col, path+".client_priority", p.ClientPriority, clientPriorityModes)
	}
	if len(p.Range) == 0 {
		if p.GrantsClientPriority() {
			col.add(path+".range",
				"client_priority: allow requires a range of two priority class names "+
					"(§10.5), for example [batch, interactive]: an unbounded grant is the "+
					"self-elevation the default exists to prevent")
		}
		return
	}
	if !p.GrantsClientPriority() {
		col.add(path+".range",
			"a range is only meaningful with client_priority: allow (it is %q here): "+
				"a range that grants nothing is a setting that does nothing",
			cmp(p.ClientPriority, ClientPriorityIgnore))
	}
	if len(p.Range) != 2 {
		col.add(path+".range", "must name exactly two priority classes (has %d)", len(p.Range))
	}
	for i, class := range p.Range {
		if _, ok := c.PriorityMapping.Classes[class]; !ok {
			col.add(fmt.Sprintf("%s.range[%d]", path, i),
				"%q is not a declared priority class: want one of %s",
				class, strings.Join(sortedKeys(c.PriorityMapping.Classes), ", "))
		}
	}
}

// cmp returns v, or fallback when v is empty.
func cmp(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
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
			checkCacheTTL(col, dpath+".prefix_ttl", d.PrefixTTL, false)
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
		checkCacheTTL(col, "routing.prefix.ttl", c.Routing.Prefix.TTL, true)
	}
}

// checkCacheTTL validates one affinity lifetime. required marks the global
// default, which must say something: an unset default would leave every
// deployment with no lifetime at all rather than with an inherited one.
func checkCacheTTL(col *collector, path string, v CacheTTL, required bool) {
	switch {
	case v.IsZero():
		if required {
			col.add(path, "must be a duration such as \"5m\" or %q when prefix affinity "+
				"is enabled (§7.4b)", UntilEvicted)
		}
	case v.IsForever():
	case v.Duration() <= 0:
		col.add(path, "must be greater than zero, or %q for a backend whose prefix cache "+
			"has no clock (vLLM and SGLang evict by memory pressure, not by time)", UntilEvicted)
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
		seenComp := map[string]string{}
		for _, comp := range sortedKeys(r.Rates) {
			cpath := fmt.Sprintf("%s.rates[%q]", path, comp)
			switch {
			case comp == "images":
				// It used to pass here and fail at assembly, which is a load
				// error delivered one layer too late: `dorangctl config lint`
				// said the file was good and the server refused to start.
				col.add(cpath, "the images component is not priced by this build: "+
					"§8.3 lists it, internal/pricing has no per-image unit, and there is no "+
					"rate that would be applied. Price the request instead (rates.request)")
			case !oneOf(comp, pricingComponents):
				col.add(cpath, "%q is not a priced component: want one of %s",
					comp, strings.Join(pricingComponents, ", "))
			default:
				canon := CanonicalComponent(comp)
				if prev, dup := seenComp[canon]; dup {
					col.add(cpath, "%q and %q are the same component (%q) priced twice",
						prev, comp, canon)
				}
				seenComp[canon] = comp
			}
			if err := checkDecimal(string(r.Rates[comp])); err != nil {
				col.add(cpath, "%v", err)
			}
		}
		c.validateProvenance(col, path, r)
		switch r.Class {
		case PricingMarginalUsage, PricingNotionalRate:
			if len(r.Rates) == 0 {
				col.add(path+".rates", "a %s rule must price at least one component", r.Class)
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

// validateProvenance enforces §8.5's two load errors, in the main file rather
// than only in the catalog file.
//
// Both directions are checked. A notional_rate rule without provenance is a
// guess wearing a currency symbol; provenance on any other class is a rule that
// looks audited and is not, since only the notional class is excluded from the
// bill.
func (c *Config) validateProvenance(col *collector, path string, r *PricingRule) {
	if r.Class != PricingNotionalRate {
		if r.Source != "" || r.AsOf != "" {
			col.add(path+".source",
				"source and as_of belong to a notional_rate rule (§8.5); this rule is %q",
				cmp(r.Class, PricingMarginalUsage))
		}
		return
	}
	if strings.TrimSpace(r.Source) == "" {
		col.add(path+".source",
			"a notional_rate rule must name where the list price came from: "+
				"an estimate without provenance is not auditable (§8.5)")
	}
	switch {
	case strings.TrimSpace(r.AsOf) == "":
		col.add(path+".as_of",
			"a notional_rate rule must carry the date the list price was read: "+
				"a rate with no date cannot be judged stale (§8.5)")
	default:
		if _, err := parseUntil(r.AsOf); err != nil {
			col.add(path+".as_of", "%v: want a date such as \"2026-07-28\" or an RFC 3339 timestamp", err)
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

// validateFilters checks the transform-filter surface of §10.5b: the plugins a
// deployment may load, and every model that attaches one.
func (c *Config) validateFilters(col *collector) {
	plugins := map[string]*FilterPlugin{}
	for i := range c.Filters.Plugins {
		p := &c.Filters.Plugins[i]
		path := fmt.Sprintf("filters.plugins[%d]", i)
		switch {
		case p.Name == "":
			col.add(path+".name", "must name the plugin: a model attaches a filter by this name")
		default:
			if _, dup := plugins[p.Name]; dup {
				col.add(path+".name", "duplicate filter plugin name %q", p.Name)
			} else {
				plugins[p.Name] = p
			}
		}
		if p.Path == "" {
			col.add(path+".path",
				"must name the plugin file: a filter is loaded because it is written here, never "+
					"because it was found in a directory (§11.5)")
		}
		mustBeOneOf(col, path+".fail", p.Fail, filterFailModes)
	}

	// The first model filter whose placeholders outlive the request, recorded so
	// the missing-seed message can name a concrete offender rather than the
	// whole file.
	var seedModel, seedPlugin, seedScope string
	for i := range c.Models {
		m := &c.Models[i]
		for j := range m.Filters {
			f := &m.Filters[j]
			path := fmt.Sprintf("models[%d].filters[%d]", i, j)
			switch {
			case f.Plugin == "":
				col.add(path+".plugin", "must name a plugin declared under filters.plugins")
			default:
				if _, ok := plugins[f.Plugin]; !ok {
					col.add(path+".plugin",
						"model %q attaches filter plugin %q, which is not declared under "+
							"filters.plugins", m.Name, f.Plugin)
				}
			}
			for k, phase := range f.On {
				mustBeOneOf(col, fmt.Sprintf("%s.on[%d]", path, k), phase, filterPhases)
			}
			// §10.5b: a mask is not a redaction — it has to come back. The
			// mask → original table is built while the REQUEST is rewritten and
			// never persisted, so a filter that runs only on the way back has an
			// empty table to unmask against.
			if containsString(f.On, FilterOnResponse) && !containsString(f.On, FilterOnRequest) {
				col.add(path+".on",
					"model %q attaches filter %q on the response only, which has nothing to "+
						"unmask: the mask → original table is built while the request is "+
						"rewritten and lives for that one request (§10.5b). Add \"request\", or "+
						"drop the filter", m.Name, f.Plugin)
			}
			mustBeOneOf(col, path+".scope", f.Scope, filterScopes)
			nonNegative(col, path+".retain", int64(f.Retain))
			for k := range f.Patterns {
				pt := f.Patterns[k]
				ppath := fmt.Sprintf("%s.patterns[%d]", path, k)
				if pt.Name == "" {
					col.add(ppath+".name", "must name the pattern")
					continue
				}
				if pt.Regexp == "" && !oneOf(pt.Name, builtinFilterPatterns) {
					col.add(ppath,
						"pattern %q has no regexp and is not built in: the built-in patterns are "+
							"%s. Give it a regexp, or name one of those (§10.5b)",
						pt.Name, strings.Join(builtinFilterPatterns, ", "))
				}
			}
			if seedModel == "" && f.Scope != "" && f.Scope != FilterScopeRequest {
				seedModel, seedPlugin, seedScope = m.Name, f.Plugin, f.Scope
			}
		}
	}

	if !c.Filters.Secret.IsZero() {
		c.Filters.Secret.validate(c.Server.Env, "filters.secret", col)
		return
	}
	if seedModel != "" {
		col.add("filters.secret",
			"must be set: model %q attaches filter %q with scope %q, and a placeholder is derived "+
				"from this seed. With no seed — or with one invented per process — the same text "+
				"masks to a different placeholder on every node and again after every restart, so "+
				"the bodies that reach a backend differ byte for byte and every backend's prefix "+
				"cache cold-starts on each hop and each restart (§7.4b). Set filters.secret "+
				"cluster-wide with key_env or key_file, the same value on every node, or use "+
				"scope: %q, which is per-request by construction and needs no seed",
			seedModel, seedPlugin, seedScope, FilterScopeRequest)
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
