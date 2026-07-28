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
)

// defaultStickyKey is the composite stickiness key of §4.2.
var defaultStickyKey = []string{"api_key", "session_id"}

// defaultPriorityClasses are the classes of §7.5.
var defaultPriorityClasses = map[string]int{"realtime": 0, "interactive": 2, "batch": 10}

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

	// storage
	setStr(&c.Storage.Driver, defaultStorageDriver)
	setStr(&c.Storage.SQLite.Path, defaultSQLitePath)
	setStr(&c.Storage.Postgres.URLEnv, defaultPostgresEnv)
	setInt(&c.Storage.Postgres.MaxConns, defaultPostgresConns)

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
	setDur(&c.Routing.Prefix.TTL, defaultPrefixTTL)

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
