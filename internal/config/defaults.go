package config

import "time"

// Documented defaults. Every value here is the design's own (§4.2 unless a
// section is named); nothing is invented at load time that the file cannot
// override.
const (
	defaultListen         = ":4100"
	defaultEnv            = EnvProduction
	defaultMasterKeyEnv   = "DORANG_MASTER_KEY"
	defaultKeyPepperEnv   = "DORANG_KEY_PEPPER"
	defaultRequestTimeout = 600 * time.Second
	defaultShutdownGrace  = 30 * time.Second
	// defaultPreStopDelay covers the readiness-probe detection window of the
	// manifests this repository ships (deploy/kubernetes.yaml): two failures at
	// a two-second period plus a one-second probe timeout is five seconds of
	// detection, and the remaining five are for the balancer to stop routing.
	// An operator whose probes are slower must raise this; §13 states the
	// arithmetic.
	defaultPreStopDelay = 10 * time.Second

	// defaultMaxBodyBytes is server.DefaultMaxBodyBytes. It is spelled out here
	// rather than imported because internal/config imports nothing of the
	// gateway — it is the schema, and a schema that depends on the HTTP surface
	// cannot be loaded by a tool that does not build one.
	//
	// The duplication is pinned by TestConfigBodyCapDefaultMatchesTheServers in
	// internal/app, which is the one package that imports both.
	defaultMaxBodyBytes = 32 << 20

	// The three connection deadlines, spelled out here for the same reason and
	// pinned the same way: they are server.DefaultReadHeaderTimeout,
	// DefaultReadTimeout and DefaultIdleTimeout, and
	// TestConfigDeadlineDefaultsMatchTheServers in internal/app fails if either
	// side moves without the other.
	//
	// The read timeout is the UPLOAD budget and not the generation budget:
	// thirty seconds of it can go on headers and the remaining ninety carry a
	// body up to defaultMaxBodyBytes, which is about 2.9 Mbit/s for a maximal
	// one. The idle timeout is deliberately LONGER than what sits in front of a
	// typical deployment (60 s on an ALB, 75 s for nginx), so the side that
	// closes an idle connection is the side that knows it is idle.
	defaultReadHeaderTimeout = 30 * time.Second
	defaultReadTimeout       = 2 * time.Minute
	defaultIdleTimeout       = 2 * time.Minute

	// The unknown-key lookup budget, matching internal/auth's DefaultMissRate
	// and DefaultMissBurst. Pinned by TestConfigMissBudgetDefaultsMatchTheAuths.
	defaultAuthMissRate  = 100.0
	defaultAuthMissBurst = 500

	defaultStorageDriver = "sqlite"
	defaultSQLitePath    = "~/.dorang/dorang.db"
	defaultPostgresEnv   = "DORANG_DATABASE_URL"
	defaultPostgresConns = 32

	defaultRedisURLEnv  = "DORANG_REDIS_URL"
	defaultCapacityMode = CapacityModeLocal
	defaultMinLeasable  = 16 // §5.6

	defaultRetryAttempts = 2
	defaultRetryBackoff  = "exponential"
	defaultRetryBase     = 500 * time.Millisecond

	defaultInteractiveReserve  = 0.3
	defaultMaxQueueWait        = 30 * time.Second
	defaultPrincipalConcurrent = 32

	defaultKeyRotationStrategy = "least_used"

	defaultStickyTTL     = time.Hour
	defaultStickyPurge   = 5 * time.Minute
	defaultPrefixChunk   = 4096
	defaultCheckpoints   = "logarithmic"
	defaultPrefixMaxByte = 64 << 20 // 64MiB
	defaultPrefixTTL     = time.Hour

	defaultMaxHops  = 3
	defaultBudgetMS = 120000
	defaultCurrency = "USD"

	defaultFlushInterval   = 250 * time.Millisecond
	defaultStoreMessages   = StoreMessagesTruncated
	defaultTruncateChars   = 512
	defaultTraceSampleRate = 1.0
	defaultDailyByteBudget = 8 << 30 // 8GiB
	defaultSpoolDir        = "~/.dorang/spool"
	defaultSpoolMaxBytes   = 2 << 30 // 2GiB

	defaultLogLevel  = "info"
	defaultLogFormat = "json"

	defaultLuaDir          = "/etc/dorang/lua"
	defaultLuaInstructions = 5000000
	defaultLuaMemoryMB     = 32
	defaultLuaTimeout      = 200 * time.Millisecond

	// §10.5b. A filter that was supposed to remove an identity number and did
	// not must stop the request, so failure is closed unless the operator says
	// otherwise; and one placeholder holds for a conversation, which is the
	// scope at which a masked answer still reads as one conversation.
	defaultFilterFail  = FilterFailClosed
	defaultFilterScope = FilterScopeConversation

	defaultPassthroughAuth    = PassthroughAuthDorang
	defaultPassthroughTimeout = 600 * time.Second

	defaultPriorityHeader = "X-Request-Priority"

	// §14.1 leaves everything but the five documented keys open. These are the
	// numbers the shadow implementation runs with, each chosen as a bound:
	//
	//   - the reference timeout is shorter than the request timeout because a
	//     shadow call that outlives the request it copies is holding a worker
	//     for a comparison nobody will read;
	//   - queue × (head + tail) is the memory shadowing may hold, and at these
	//     values that is 256 × 272 KiB ≈ 68 MiB worst case;
	//   - the head is large enough that an ordinary chat response is captured
	//     whole, which is what keeps a comparison conclusive.
	defaultShadowRefTimeout     = 60 * time.Second
	defaultShadowQueueSize      = 256
	defaultShadowWorkers        = 4
	defaultShadowCaptureHead    = 256 << 10
	defaultShadowCaptureTail    = 16 << 10
	defaultShadowReportPath     = "~/.dorang/shadow.jsonl"
	defaultShadowReportMaxBytes = 256 << 20
	// defaultShadowUnpricedEstimate is one cent. It is deliberately not zero:
	// see Shadow.UnpricedEstimateUSD.
	defaultShadowUnpricedEstimate = "0.01"

	// §11.5 names the events and the drivers and leaves the pipeline open.
	// These are the numbers internal/notify runs with, each chosen as a bound
	// rather than as a preference:
	//
	//   - one worker, because mail is not a throughput problem and N workers
	//     hammering a server that is already failing is the behaviour the
	//     breaker exists to prevent;
	//   - a queue of 256, which at a few hundred bytes a notification is
	//     bounded memory and far more than a healthy deployment ever holds;
	//   - an hour of deduplication, because that is the interval at which an
	//     operator wants to be reminded that a budget is still at 80%, not the
	//     interval at which requests arrive.
	defaultNotifyQueueSize       = 256
	defaultNotifyWorkers         = 1
	defaultNotifyDedupPeriod     = time.Hour
	defaultNotifyMaxAttempts     = 3
	defaultNotifyInitialBackoff  = time.Second
	defaultNotifyMaxBackoff      = 30 * time.Second
	defaultNotifyBreakerFailures = 5
	defaultNotifyBreakerCooldown = time.Minute
	defaultNotifySMTPTimeout     = 10 * time.Second
	defaultNotifyHTTPTimeout     = 10 * time.Second
)

// defaultStickyKey is the composite stickiness key of §4.2.
var defaultStickyKey = []string{"api_key", "session_id"}

// defaultFilterOn is the phase pair of §10.5b: a mask is applied on the way out
// and undone on the way back, so both halves run unless the file narrows it.
var defaultFilterOn = []string{FilterOnRequest, FilterOnResponse}

// defaultPriorityClasses are the classes of §7.5.
var defaultPriorityClasses = map[string]int{"realtime": 0, "interactive": 2, "batch": 10}

// Rotation, revocation and token-guard defaults (§11.2c, §11.6).
//
// The rotation and trigger numbers are §11.6's and §11.2c's own example blocks.
// The ones the design leaves unstated are marked as such where they are
// defined, because a default nobody argued for is a default nobody can check.
const (
	defaultRotationGrace  = 24 * time.Hour
	defaultMaxSecrets     = 2
	defaultRotationMaxAge = 90 * 24 * time.Hour

	// defaultAuthEntryTTL and defaultAuthNegativeTTL match internal/auth's own
	// defaults. The negative one is deliberately an order of magnitude shorter:
	// §11.2c rule 3 is that a refused key is cheap to re-check and a serving one
	// is not, and treating them alike is what made the revocation window wide.
	defaultAuthEntryTTL    = 60 * time.Second
	defaultAuthNegativeTTL = 5 * time.Second

	// defaultRevocationPoll is the dominant term in the published clustered
	// revocation bound, so it is set by what an operator is owed after a
	// compromise rather than by what is cheapest: it is one indexed range scan
	// returning nothing in the normal case.
	defaultRevocationPoll   = time.Second
	defaultRevocationRetain = time.Hour
	// defaultRevocationStoreLatency is deliberately generous. A bound computed
	// from an optimistic figure is wrong exactly when the database is slow,
	// which is when an operator is most likely to be revoking something.
	defaultRevocationStoreLatency = 250 * time.Millisecond

	defaultGuardBaselineWindow = 7 * 24 * time.Hour
	// defaultGuardWindow is not in §11.6's block. Without an observation window
	// "factor: 10" has no unit.
	defaultGuardWindow      = time.Hour
	defaultGuardFactor      = 10.0
	defaultGuardMinAbsolute = int64(100_000)
	// defaultGuardMinHistory is not in §11.6's block either; §11.6 requires "a
	// stated minimum of history" and does not state it. A day is the shortest
	// span containing a whole daily cycle, and a baseline without one would call
	// every key's morning anomalous.
	defaultGuardMinHistory = 24 * time.Hour
	defaultGuardCooldown   = time.Hour
	// defaultGuardAction is pend, never revoke: an automated revocation is an
	// outage the operator did not choose and is not reversible in the same
	// sense.
	defaultGuardAction = "pend"
)

// defaultFallbackChains is the "Default chain" column of §7.6. budget_exceeded
// and auth have no chain: failing is the correct outcome.
var defaultFallbackChains = map[string][]string{
	CauseRateLimit:      {TargetSameGroup, TargetSameClass},
	CauseQuotaExhausted: {TargetSameGroup, TargetSameClass},
	CauseContextWindow:  {TargetSameClassLarger},
	CauseContentPolicy:  {TargetSameClass},
	CauseUpstream5xx:    {TargetSameGroup, TargetSameClass},
	CauseTimeout:        {TargetSameGroup},
	CauseBudgetExceeded: {},
	CauseAuth:           {},
}

func boolPtr(b bool) *bool        { return &b }
func floatPtr(f float64) *float64 { return &f }

func durPtr(d time.Duration) *Duration { v := Duration(d); return &v }

// ApplyDefaults fills every unset field with its documented default. It is
// idempotent, and it never overwrites a value the file set — fields where an
// explicit zero differs from "unset" are pointers for exactly that reason.
func (c *Config) ApplyDefaults() {
	if c.Version == 0 {
		c.Version = Version
	}

	// server
	setStr(&c.Server.Listen, defaultListen)
	setStr(&c.Server.Env, defaultEnv)
	setStr(&c.Server.MasterKeyEnv, defaultMasterKeyEnv)
	setStr(&c.Server.KeyPepperEnv, defaultKeyPepperEnv)
	setDur(&c.Server.RequestTimeout, defaultRequestTimeout)
	setDur(&c.Server.ShutdownGrace, defaultShutdownGrace)
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.Server.PreStopDelay == nil {
		c.Server.PreStopDelay = durPtr(defaultPreStopDelay)
	}
	// Only an ABSENT deadline is defaulted. `none` is stored as a negative and
	// survives, which is the whole reason it is a [Deadline] and not a Duration.
	setDeadline(&c.Server.ReadHeaderTimeout, defaultReadHeaderTimeout)
	setDeadline(&c.Server.ReadTimeout, defaultReadTimeout)
	setDeadline(&c.Server.IdleTimeout, defaultIdleTimeout)

	// storage
	setStr(&c.Storage.Driver, defaultStorageDriver)
	setStr(&c.Storage.SQLite.Path, defaultSQLitePath)
	setStr(&c.Storage.Postgres.URLEnv, defaultPostgresEnv)
	setInt(&c.Storage.Postgres.MaxConns, defaultPostgresConns)

	// auth: rotation (§11.2c) and revocation latency (§11.2c, risk W11)
	setDur(&c.Auth.Rotation.Grace, defaultRotationGrace)
	setInt(&c.Auth.Rotation.MaxSecrets, defaultMaxSecrets)
	setDur(&c.Auth.Rotation.MaxAge, defaultRotationMaxAge)
	setDur(&c.Auth.Revocation.EntryTTL, defaultAuthEntryTTL)
	setDur(&c.Auth.Revocation.NegativeTTL, defaultAuthNegativeTTL)
	setDur(&c.Auth.Revocation.Poll, defaultRevocationPoll)
	setDur(&c.Auth.Revocation.Retain, defaultRevocationRetain)
	setDur(&c.Auth.Revocation.StoreLatency, defaultRevocationStoreLatency)
	// Rate zero is absence and takes the default; a NEGATIVE rate is the explicit
	// "no bound" and survives, exactly as internal/auth reads it.
	if c.Auth.MissBudget.Rate == 0 {
		c.Auth.MissBudget.Rate = defaultAuthMissRate
	}
	setInt(&c.Auth.MissBudget.Burst, defaultAuthMissBurst)

	// token guard (§11.6). Filled whether or not it is enabled, so that a
	// rendered configuration shows what turning it on would do.
	setDur(&c.TokenGuard.BaselineWindow, defaultGuardBaselineWindow)
	setDur(&c.TokenGuard.Window, defaultGuardWindow)
	setDur(&c.TokenGuard.MinHistory, defaultGuardMinHistory)
	setDur(&c.TokenGuard.Cooldown, defaultGuardCooldown)
	setStr(&c.TokenGuard.Action, defaultGuardAction)
	if c.TokenGuard.Trigger.Factor == 0 {
		c.TokenGuard.Trigger.Factor = defaultGuardFactor
	}
	if c.TokenGuard.Trigger.MinAbsolute == 0 {
		c.TokenGuard.Trigger.MinAbsolute = defaultGuardMinAbsolute
	}

	// compat (COMPATIBILITY §3.3, §6.8, §7.7). Filled so that a rendered
	// configuration shows what this build actually does, which for two of the
	// three is the only value it can do.
	setStr(&c.Compat.UsageChunkChoices, UsageChunkChoicesStub)
	if c.Compat.AnthropicTotalTokens == nil {
		c.Compat.AnthropicTotalTokens = boolPtr(true)
	}

	// cluster
	setStr(&c.Cluster.RedisURLEnv, defaultRedisURLEnv)
	setStr(&c.Cluster.CapacityMode, defaultCapacityMode)
	setInt(&c.Cluster.MinLeasable, defaultMinLeasable)

	// auth
	if c.Auth.RehashOnUse == nil {
		c.Auth.RehashOnUse = boolPtr(true)
	}

	// providers
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Params.DropUnsupported == nil {
			p.Params.DropUnsupported = boolPtr(true)
		}
		setInt(&p.Retry.MaxAttempts, defaultRetryAttempts)
		setStr(&p.Retry.Backoff, defaultRetryBackoff)
		setDur(&p.Retry.Base, defaultRetryBase)
	}

	// capacity
	if c.Capacity.InteractiveReserve == nil {
		c.Capacity.InteractiveReserve = floatPtr(defaultInteractiveReserve)
	}
	if c.Capacity.Principals == nil {
		c.Capacity.Principals = map[string]PrincipalLimits{}
	}
	def, ok := c.Capacity.Principals["default"]
	if !ok {
		def = PrincipalLimits{MaxConcurrent: defaultPrincipalConcurrent}
	}
	setDur(&def.MaxQueueWait, defaultMaxQueueWait)
	c.Capacity.Principals["default"] = def
	for name, pl := range c.Capacity.Principals {
		setDur(&pl.MaxQueueWait, defaultMaxQueueWait)
		c.Capacity.Principals[name] = pl
	}

	// key rotation
	if len(c.KeyRotation.Providers) > 0 || c.KeyRotation.Strategy != "" {
		setStr(&c.KeyRotation.Strategy, defaultKeyRotationStrategy)
	}

	// routing — §4.2 shows both affinity tables on; either can be turned off.
	if c.Routing.Sticky.Enabled == nil {
		c.Routing.Sticky.Enabled = boolPtr(true)
	}
	if c.Routing.Prefix.Enabled == nil {
		c.Routing.Prefix.Enabled = boolPtr(true)
	}
	setDur(&c.Routing.Sticky.TTL, defaultStickyTTL)
	setDur(&c.Routing.Sticky.PurgeInterval, defaultStickyPurge)
	if len(c.Routing.Sticky.Key) == 0 {
		c.Routing.Sticky.Key = append([]string(nil), defaultStickyKey...)
	}
	if c.Routing.Prefix.ChunkBytes == 0 {
		c.Routing.Prefix.ChunkBytes = defaultPrefixChunk
	}
	setStr(&c.Routing.Prefix.Checkpoints, defaultCheckpoints)
	if c.Routing.Prefix.MaxBytes == 0 {
		c.Routing.Prefix.MaxBytes = defaultPrefixMaxByte
	}
	if c.Routing.Prefix.TTL.IsZero() {
		c.Routing.Prefix.TTL = TTL(defaultPrefixTTL)
	}

	// fallbacks
	if c.Fallbacks.On == nil {
		c.Fallbacks.On = map[string][]string{}
	}
	for cause, chain := range defaultFallbackChains {
		if _, ok := c.Fallbacks.On[cause]; !ok {
			// Copied rather than appended so an empty default chain stays an
			// empty chain rather than becoming a nil one.
			cp := make([]string, len(chain))
			copy(cp, chain)
			c.Fallbacks.On[cause] = cp
		}
	}
	setInt(&c.Fallbacks.MaxHops, defaultMaxHops)
	setInt(&c.Fallbacks.BudgetMS, defaultBudgetMS)

	// pricing
	setStr(&c.Pricing.Currency, defaultCurrency)
	for i := range c.Pricing.Rules {
		setStr(&c.Pricing.Rules[i].Class, PricingMarginalUsage)
	}

	// metering
	if c.Metering.Numeric.Enabled == nil {
		c.Metering.Numeric.Enabled = boolPtr(true)
	}
	setStr(&c.Metering.Trace.StoreMessages, defaultStoreMessages)
	setInt(&c.Metering.Trace.TruncateChars, defaultTruncateChars)
	if c.Metering.Trace.SampleRate == nil {
		c.Metering.Trace.SampleRate = floatPtr(defaultTraceSampleRate)
	}
	if c.Metering.Trace.DailyByteBudget == 0 {
		c.Metering.Trace.DailyByteBudget = defaultDailyByteBudget
	}
	setStr(&c.Metering.Spool.Dir, defaultSpoolDir)
	if c.Metering.Spool.MaxBytes == 0 {
		c.Metering.Spool.MaxBytes = defaultSpoolMaxBytes
	}
	setDur(&c.Metering.FlushInterval, defaultFlushInterval)

	// observability
	if c.Observability.Prometheus == nil {
		c.Observability.Prometheus = boolPtr(true)
	}
	setStr(&c.Observability.LogLevel, defaultLogLevel)
	setStr(&c.Observability.LogFormat, defaultLogFormat)

	// extensions
	setStr(&c.Extensions.Lua.Dir, defaultLuaDir)
	if c.Extensions.Lua.Limits.Instructions == 0 {
		c.Extensions.Lua.Limits.Instructions = defaultLuaInstructions
	}
	setInt(&c.Extensions.Lua.Limits.MemoryMB, defaultLuaMemoryMB)
	setDur(&c.Extensions.Lua.Limits.Timeout, defaultLuaTimeout)

	// filters (§10.5b)
	for i := range c.Filters.Plugins {
		setStr(&c.Filters.Plugins[i].Fail, defaultFilterFail)
	}
	for i := range c.Models {
		for j := range c.Models[i].Filters {
			f := &c.Models[i].Filters[j]
			if len(f.On) == 0 {
				f.On = append([]string(nil), defaultFilterOn...)
			}
			setStr(&f.Scope, defaultFilterScope)
		}
	}

	// passthrough
	setStr(&c.Passthrough.Default.Auth, defaultPassthroughAuth)
	if c.Passthrough.Default.Meter == nil {
		c.Passthrough.Default.Meter = boolPtr(true)
	}
	setDur(&c.Passthrough.Default.Timeout, defaultPassthroughTimeout)
	for i := range c.Passthrough.Routes {
		r := &c.Passthrough.Routes[i]
		setStr(&r.Auth, c.Passthrough.Default.Auth)
		if r.Meter == nil {
			r.Meter = boolPtr(*c.Passthrough.Default.Meter)
		}
		setDur(&r.Timeout, c.Passthrough.Default.Timeout.Duration())
	}

	// shadow
	setStr(&c.Shadow.Mode, ShadowModeOff)
	if c.Shadow.Compare.Structural == nil {
		c.Shadow.Compare.Structural = boolPtr(true)
	}
	setDur(&c.Shadow.Reference.Timeout, defaultShadowRefTimeout)
	setInt(&c.Shadow.QueueSize, defaultShadowQueueSize)
	setInt(&c.Shadow.Workers, defaultShadowWorkers)
	if c.Shadow.Capture.HeadBytes == 0 {
		c.Shadow.Capture.HeadBytes = defaultShadowCaptureHead
	}
	if c.Shadow.Capture.TailBytes == 0 {
		c.Shadow.Capture.TailBytes = defaultShadowCaptureTail
	}
	setStr(&c.Shadow.Report.Path, defaultShadowReportPath)
	if c.Shadow.Report.MaxBytes == 0 {
		c.Shadow.Report.MaxBytes = defaultShadowReportMaxBytes
	}
	if c.Shadow.UnpricedEstimateUSD == "" {
		c.Shadow.UnpricedEstimateUSD = defaultShadowUnpricedEstimate
	}

	// notifications
	setStr(&c.Notifications.Email.Driver, "none")
	setInt(&c.Notifications.QueueSize, defaultNotifyQueueSize)
	setInt(&c.Notifications.Workers, defaultNotifyWorkers)
	setDur(&c.Notifications.DedupPeriod, defaultNotifyDedupPeriod)
	setInt(&c.Notifications.Retry.MaxAttempts, defaultNotifyMaxAttempts)
	setDur(&c.Notifications.Retry.InitialBackoff, defaultNotifyInitialBackoff)
	setDur(&c.Notifications.Retry.MaxBackoff, defaultNotifyMaxBackoff)
	setInt(&c.Notifications.Retry.BreakerThreshold, defaultNotifyBreakerFailures)
	setDur(&c.Notifications.Retry.BreakerCooldown, defaultNotifyBreakerCooldown)
	setDur(&c.Notifications.Email.SMTP.Timeout, defaultNotifySMTPTimeout)
	setDur(&c.Notifications.Email.HTTP.Timeout, defaultNotifyHTTPTimeout)

	// priority mapping
	if len(c.PriorityMapping.Classes) == 0 {
		c.PriorityMapping.Classes = map[string]int{}
		for k, v := range defaultPriorityClasses {
			c.PriorityMapping.Classes[k] = v
		}
	}
	setStr(&c.PriorityMapping.Emit.Header, defaultPriorityHeader)
}

func setStr(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

func setInt(dst *int, v int) {
	if *dst == 0 {
		*dst = v
	}
}

func setDur(dst *Duration, v time.Duration) {
	if *dst == 0 {
		*dst = Duration(v)
	}
}

// setDeadline fills an ABSENT deadline. A negative one is `none` — a value the
// operator wrote — and is left alone.
func setDeadline(dst *Deadline, v time.Duration) {
	if *dst == 0 {
		*dst = Deadline(v)
	}
}
