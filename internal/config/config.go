package config

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// Version is the only configuration schema version this build understands.
const Version = 1

// Config is the whole configuration file (design §4.2).
//
// A Config is immutable once [Load] returns it. Readers hold a snapshot; a hot
// reload swaps in a new Config rather than mutating this one (§4.1, §15.2).
type Config struct {
	Version int `yaml:"version"`

	Server        Server              `yaml:"server"`
	Storage       Storage             `yaml:"storage"`
	Cluster       Cluster             `yaml:"cluster"`
	Auth          Auth                `yaml:"auth"`
	Providers     []Provider          `yaml:"providers,omitempty"`
	Credentials   []Credential        `yaml:"credentials,omitempty"`
	Capacity      Capacity            `yaml:"capacity"`
	KeyRotation   KeyRotation         `yaml:"key_rotation"`
	Models        []Model             `yaml:"models,omitempty"`
	Aliases       map[string]string   `yaml:"aliases,omitempty"`
	Classes       map[string][]string `yaml:"classes,omitempty"`
	Routing       Routing             `yaml:"routing"`
	Fallbacks     Fallbacks           `yaml:"fallbacks"`
	Pricing       Pricing             `yaml:"pricing"`
	Metering      Metering            `yaml:"metering"`
	Observability Observability       `yaml:"observability"`
	Extensions    Extensions          `yaml:"extensions"`

	Passthrough     Passthrough     `yaml:"passthrough"`
	Shadow          Shadow          `yaml:"shadow"`
	Notifications   Notifications   `yaml:"notifications"`
	PriorityMapping PriorityMapping `yaml:"priority_mapping"`

	// idx is built once, after validation, and never mutated afterwards.
	idx *index

	// source records where the configuration came from, for diagnostics.
	source string
}

// Server is the listening process itself.
type Server struct {
	Listen         string   `yaml:"listen,omitempty"`
	Env            string   `yaml:"env,omitempty"`
	MasterKeyEnv   string   `yaml:"master_key_env,omitempty"`
	KeyPepperEnv   string   `yaml:"key_pepper_env,omitempty"`
	RequestTimeout Duration `yaml:"request_timeout,omitempty"`
	ShutdownGrace  Duration `yaml:"shutdown_grace,omitempty"`
}

// Storage selects the ledger and control-plane store (§9).
type Storage struct {
	Driver   string          `yaml:"driver,omitempty"`
	SQLite   SQLiteStorage   `yaml:"sqlite,omitempty"`
	Postgres PostgresStorage `yaml:"postgres,omitempty"`
}

// SQLiteStorage configures the embedded store.
type SQLiteStorage struct {
	Path string `yaml:"path,omitempty"`
}

// PostgresStorage configures the shared store.
type PostgresStorage struct {
	URLEnv   string `yaml:"url_env,omitempty"`
	MaxConns int    `yaml:"max_conns,omitempty"`
}

// Cluster configures multi-node operation (§13) and, through capacity_mode,
// how accurate capacity accounting is across nodes (§5.6).
type Cluster struct {
	Enabled      bool   `yaml:"enabled"`
	NodeID       string `yaml:"node_id,omitempty"`
	RedisURLEnv  string `yaml:"redis_url_env,omitempty"`
	CapacityMode string `yaml:"capacity_mode,omitempty"`
	// MinLeasable is the smallest limit the "leased" mode will divide across
	// nodes; anything below it requires a shared mode (§5.6).
	MinLeasable int `yaml:"min_leasable,omitempty"`
}

// Capacity modes (§5.6).
const (
	CapacityModeLocal       = "local"
	CapacityModeSharedRedis = "shared-redis"
	CapacityModeSharedPG    = "shared-pg"
	CapacityModeLeased      = "leased"
)

// Auth configures credential verification (§2.4).
type Auth struct {
	Legacy      LegacyAuth `yaml:"legacy"`
	RehashOnUse *bool      `yaml:"rehash_on_use,omitempty"`
}

// RehashesOnUse reports whether a successful legacy verification schedules an
// upgrade to the current scheme.
func (a Auth) RehashesOnUse() bool { return a.RehashOnUse == nil || *a.RehashOnUse }

// LegacyAuth enables the unsalted single-round digest scheme for a migration
// window. It is a window, not an architecture: it requires an end date.
type LegacyAuth struct {
	Enabled bool   `yaml:"enabled"`
	Until   string `yaml:"until,omitempty"`
}

// Provider is one upstream service (§3).
type Provider struct {
	Name           string          `yaml:"name"`
	Kind           string          `yaml:"kind"`
	BaseURL        string          `yaml:"base_url,omitempty"`
	Timeout        Duration        `yaml:"timeout,omitempty"`
	MaxConcurrency int             `yaml:"max_concurrency,omitempty"`
	CapacityGroup  string          `yaml:"capacity_group,omitempty"`
	Params         Params          `yaml:"params,omitempty"`
	Retry          Retry           `yaml:"retry,omitempty"`
	UsageProbe     UsageProbe      `yaml:"usage_probe,omitempty"`
	Metrics        ProviderMetrics `yaml:"metrics,omitempty"`
}

// Params controls parameter conversion for a provider (§10.3).
type Params struct {
	DropUnsupported *bool          `yaml:"drop_unsupported,omitempty"`
	Drop            []string       `yaml:"drop,omitempty"`
	Set             map[string]any `yaml:"set,omitempty"`
	Default         map[string]any `yaml:"default,omitempty"`
}

// DropsUnsupported reports whether parameters the kind does not support are
// filtered out rather than forwarded.
func (p Params) DropsUnsupported() bool { return p.DropUnsupported == nil || *p.DropUnsupported }

// Retry configures in-provider retries, distinct from the fallback chain (§7.6).
type Retry struct {
	MaxAttempts int      `yaml:"max_attempts,omitempty"`
	Backoff     string   `yaml:"backoff,omitempty"`
	Base        Duration `yaml:"base,omitempty"`
}

// UsageProbe polls a provider for its own view of quota (§6.2).
type UsageProbe struct {
	Enabled  bool     `yaml:"enabled"`
	Fetcher  string   `yaml:"fetcher,omitempty"`
	Interval Duration `yaml:"interval,omitempty"`
}

// ProviderMetrics declares a backend metrics endpoint to scrape (§12.4).
type ProviderMetrics struct {
	Enabled  bool     `yaml:"enabled"`
	Endpoint string   `yaml:"endpoint,omitempty"`
	Interval Duration `yaml:"interval,omitempty"`
}

// Credential is one authenticating identity at a provider — the unit quotas and
// concurrency actually attach to (§3).
type Credential struct {
	ID       string `yaml:"id"`
	Provider string `yaml:"provider"`
	// Key carries key_env / key_file / key_ref / key as sibling keys.
	Key           SecretRef `yaml:",inline"`
	CapacityGroup string    `yaml:"capacity_group,omitempty"`
}

// Capacity declares the ceilings of every axis (§5.1).
type Capacity struct {
	ProviderGroups     map[string]CapacityLimits  `yaml:"provider_groups,omitempty"`
	CredentialGroups   map[string]CapacityLimits  `yaml:"credential_groups,omitempty"`
	Models             []ModelCapacity            `yaml:"models,omitempty"`
	Principals         map[string]PrincipalLimits `yaml:"principals,omitempty"`
	Global             *CapacityLimits            `yaml:"global,omitempty"`
	InteractiveReserve *float64                   `yaml:"interactive_reserve,omitempty"`
}

// Reserve returns the fraction of every axis that batch work may not occupy
// (§11.1).
func (c Capacity) Reserve() float64 {
	if c.InteractiveReserve == nil {
		return defaultInteractiveReserve
	}
	return *c.InteractiveReserve
}

// CapacityLimits is one axis ceiling, per metric (§5.2). A zero metric is not a
// ceiling of zero: it means this axis does not constrain that metric.
type CapacityLimits struct {
	MaxConcurrency int   `yaml:"max_concurrency,omitempty"`
	RPM            int   `yaml:"rpm,omitempty"`
	TPM            int64 `yaml:"tpm,omitempty"`
	MaxQueue       int   `yaml:"max_queue,omitempty"`
}

// ModelCapacity is the per-(key, model) axis: a ceiling that counts one
// upstream model at one provider (§5.1).
type ModelCapacity struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	Limits   CapacityLimits
}

// UnmarshalYAML decodes provider/model plus the inline limit metrics.
func (m *ModelCapacity) UnmarshalYAML(n *yaml.Node) error {
	var shape struct {
		Provider string         `yaml:"provider"`
		Model    string         `yaml:"model"`
		Limits   CapacityLimits `yaml:",inline"`
	}
	if err := n.Decode(&shape); err != nil {
		return err
	}
	m.Provider, m.Model, m.Limits = shape.Provider, shape.Model, shape.Limits
	return nil
}

// MarshalYAML encodes provider/model plus the inline limit metrics.
func (m ModelCapacity) MarshalYAML() (any, error) {
	return struct {
		Provider string         `yaml:"provider"`
		Model    string         `yaml:"model"`
		Limits   CapacityLimits `yaml:",inline"`
	}{m.Provider, m.Model, m.Limits}, nil
}

// PrincipalLimits is the per-caller axis (§5.1). As with [CapacityLimits], a
// zero metric means that metric is unconstrained. The design spells the gauge
// metric "max_concurrent" here and "max_concurrency" on the group axes; both
// spellings are kept as written.
type PrincipalLimits struct {
	MaxConcurrent int      `yaml:"max_concurrent,omitempty"`
	MaxQueueWait  Duration `yaml:"max_queue_wait,omitempty"`
	RPM           int      `yaml:"rpm,omitempty"`
	TPM           int64    `yaml:"tpm,omitempty"`
	MaxQueue      int      `yaml:"max_queue,omitempty"`
}

// KeyRotation configures how several keys of one provider are chosen between.
type KeyRotation struct {
	Strategy  string                         `yaml:"strategy,omitempty"`
	Providers map[string]KeyRotationProvider `yaml:"providers,omitempty"`
}

// KeyRotationProvider is the rotation policy for one provider's keys.
type KeyRotationProvider struct {
	AffinityGroup string        `yaml:"affinity_group,omitempty"`
	Stickiness    Stickiness    `yaml:"stickiness,omitempty"`
	Keys          []RotationKey `yaml:"keys,omitempty"`
}

// Stickiness decides whether a request pinned to a key waits for it or spills
// to the next candidate (§5.3).
type Stickiness struct {
	Scope      string `yaml:"scope,omitempty"`
	OnCapacity string `yaml:"on_capacity,omitempty"`
}

// RotationKey is one key in a rotation pool. Its id is the credential id the
// key axis counts over (§5.1).
type RotationKey struct {
	ID             string    `yaml:"id"`
	Key            SecretRef `yaml:",inline"`
	MaxConcurrency int       `yaml:"max_concurrency,omitempty"`
	CapacityGroup  string    `yaml:"capacity_group,omitempty"`
}

// Model is one client-facing name and the deployments behind it (§3).
// The name is opaque: nothing splits it (§2.1).
type Model struct {
	Name        string       `yaml:"name"`
	Class       string       `yaml:"class,omitempty"`
	Strategy    []string     `yaml:"strategy,omitempty"`
	Deployments []Deployment `yaml:"deployments,omitempty"`
}

// Deployment is one routing candidate: a provider, a credential pool and an
// upstream model name (§3).
type Deployment struct {
	Provider      string   `yaml:"provider"`
	UpstreamModel string   `yaml:"upstream_model"`
	Credentials   []string `yaml:"credentials,omitempty"`
	Weight        int      `yaml:"weight,omitempty"`
	Priority      int      `yaml:"priority,omitempty"`
	Timeout       Duration `yaml:"timeout,omitempty"`
	StreamTimeout Duration `yaml:"stream_timeout,omitempty"`
	Limits        []Limit  `yaml:"limits,omitempty"`
}

// Limit is one (metric, ceiling) pair on a deployment (§2.2, §5.2).
type Limit struct {
	Metric string `yaml:"metric"`
	Value  int64  `yaml:"value"`
}

// Capacity metric names (§5.2).
const (
	MetricMaxConcurrent = "max_concurrent"
	MetricRPM           = "rpm"
	MetricTPM           = "tpm"
	MetricMaxQueue      = "max_queue"
)

// Routing configures cache affinity (§7.4).
type Routing struct {
	Sticky StickyRouting `yaml:"sticky,omitempty"`
	Prefix PrefixRouting `yaml:"prefix,omitempty"`
}

// StickyRouting pins a session to a target for the life of the upstream cache.
type StickyRouting struct {
	Enabled       *bool    `yaml:"enabled,omitempty"`
	TTL           Duration `yaml:"ttl,omitempty"`
	PurgeInterval Duration `yaml:"purge_interval,omitempty"`
	Key           []string `yaml:"key,omitempty"`
}

// IsEnabled reports whether session stickiness is on.
func (s StickyRouting) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// PrefixRouting configures the order-exact prefix hash chain (§7.4b). Chunks
// are cut at byte boundaries; there is no tokenizer on the hot path.
type PrefixRouting struct {
	Enabled     *bool    `yaml:"enabled,omitempty"`
	ChunkBytes  ByteSize `yaml:"chunk_bytes,omitempty"`
	Checkpoints string   `yaml:"checkpoints,omitempty"`
	MaxBytes    ByteSize `yaml:"max_bytes,omitempty"`
	TTL         Duration `yaml:"ttl,omitempty"`
}

// IsEnabled reports whether prefix affinity is on.
func (p PrefixRouting) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// Fallbacks is the fail-back chain per cause (§7.6).
type Fallbacks struct {
	On       map[string][]string `yaml:"on,omitempty"`
	MaxHops  int                 `yaml:"max_hops,omitempty"`
	BudgetMS int                 `yaml:"budget_ms,omitempty"`
}

// Fallback causes (§7.6).
const (
	CauseRateLimit      = "rate_limit"
	CauseQuotaExhausted = "quota_exhausted"
	CauseContextWindow  = "context_window"
	CauseContentPolicy  = "content_policy"
	CauseUpstream5xx    = "upstream_5xx"
	CauseTimeout        = "timeout"
	CauseBudgetExceeded = "budget_exceeded"
	CauseAuth           = "auth"
)

// Fallback targets (§7.6).
const (
	TargetSameGroup       = "same_group"
	TargetSameClass       = "same_class"
	TargetSameClassLarger = "same_class_larger"
)

// Pricing points at the price catalog and carries any rules kept in the main
// file (§8).
type Pricing struct {
	Catalog  string        `yaml:"catalog,omitempty"`
	Currency string        `yaml:"currency,omitempty"`
	Rules    []PricingRule `yaml:"rules,omitempty"`
}

// PricingRule is one priced rule. Classes compose rather than compete: each
// class picks its own winner and the results are combined (§8.1).
type PricingRule struct {
	ID       string             `yaml:"id,omitempty"`
	Class    string             `yaml:"class,omitempty"`
	Priority int                `yaml:"priority,omitempty"`
	Match    PricingMatch       `yaml:"match,omitempty"`
	Rates    map[string]Decimal `yaml:"rates,omitempty"`
	// Period and Amount price a fixed_subscription rule.
	Period string  `yaml:"period,omitempty"`
	Amount Decimal `yaml:"amount,omitempty"`
	// Percent adjusts a computed cost, for an adjustment rule.
	Percent Decimal `yaml:"percent,omitempty"`
}

// PricingMatch selects which requests a rule applies to. The dimensions are the
// specificity ladder of §8.2.
type PricingMatch struct {
	Credential  string `yaml:"credential,omitempty"`
	Provider    string `yaml:"provider,omitempty"`
	Model       string `yaml:"model,omitempty"`
	ModelPrefix string `yaml:"model_prefix,omitempty"`
	Deployment  string `yaml:"deployment,omitempty"`
}

// Pricing rule classes (§8.1).
const (
	PricingMarginalUsage     = "marginal_usage"
	PricingFixedSubscription = "fixed_subscription"
	PricingAdjustment        = "adjustment"
)

// pricingComponents are the priced quantities of §8.3.
var pricingComponents = []string{
	"input", "output", "cached_read", "cache_write", "reasoning",
	"request", "characters", "images", "seconds",
}

// Metering configures the two metering queues (§12.1).
type Metering struct {
	Numeric       MeteringNumeric `yaml:"numeric,omitempty"`
	Trace         MeteringTrace   `yaml:"trace,omitempty"`
	Spool         MeteringSpool   `yaml:"spool,omitempty"`
	FlushInterval Duration        `yaml:"flush_interval,omitempty"`
}

// MeteringNumeric is the fixed-cardinality accounting path. It is always on:
// cost, tokens and error counts survive any back-pressure (§12.1).
type MeteringNumeric struct {
	Enabled *bool `yaml:"enabled,omitempty"`
}

// IsEnabled reports whether numeric accounting is on.
func (m MeteringNumeric) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// MeteringTrace is the sampled, droppable trace payload path (§12.1, §12.2).
type MeteringTrace struct {
	StoreMessages   string   `yaml:"store_messages,omitempty"`
	TruncateChars   int      `yaml:"truncate_chars,omitempty"`
	SampleRate      *float64 `yaml:"sample_rate,omitempty"`
	DailyByteBudget ByteSize `yaml:"daily_byte_budget,omitempty"`
}

// Rate returns the trace sampling rate.
func (m MeteringTrace) Rate() float64 {
	if m.SampleRate == nil {
		return defaultTraceSampleRate
	}
	return *m.SampleRate
}

// Message storage modes (§12.2).
const (
	StoreMessagesNone      = "none"
	StoreMessagesHash      = "hash"
	StoreMessagesTruncated = "truncated"
)

// MeteringSpool is the durable local buffer that stands between the trace queue
// and the store, so a store stall costs disk rather than data (§12.1).
type MeteringSpool struct {
	Dir      string   `yaml:"dir,omitempty"`
	MaxBytes ByteSize `yaml:"max_bytes,omitempty"`
}

// Observability configures metrics, tracing export and logs (§12).
type Observability struct {
	Prometheus   *bool  `yaml:"prometheus,omitempty"`
	OTLPEndpoint string `yaml:"otlp_endpoint,omitempty"`
	LogLevel     string `yaml:"log_level,omitempty"`
	LogFormat    string `yaml:"log_format,omitempty"`
	// AlwaysFullHeaders attaches the full extension header set to every
	// response instead of only on request (§10.4).
	AlwaysFullHeaders bool `yaml:"always_full_headers,omitempty"`
}

// PrometheusEnabled reports whether /metrics is served.
func (o Observability) PrometheusEnabled() bool { return o.Prometheus == nil || *o.Prometheus }

// Extensions holds the optional Lua extension points (§11.5).
type Extensions struct {
	Lua LuaExtension `yaml:"lua,omitempty"`
}

// LuaExtension configures the sandboxed hooks. Disabled by default on the hot
// path.
type LuaExtension struct {
	Enabled bool      `yaml:"enabled"`
	Dir     string    `yaml:"dir,omitempty"`
	Hooks   []string  `yaml:"hooks,omitempty"`
	Limits  LuaLimits `yaml:"limits,omitempty"`
}

// LuaLimits bounds a hook's instructions, memory and wall clock.
type LuaLimits struct {
	Instructions int64    `yaml:"instructions,omitempty"`
	MemoryMB     int      `yaml:"memory_mb,omitempty"`
	Timeout      Duration `yaml:"timeout,omitempty"`
}

// luaHooks are the hook points of §11.5.
var luaHooks = []string{"on_request", "on_route", "on_response", "on_email"}

// Passthrough opens provider-native routes by configuration (§10.6).
// Unmapped prefixes are not served: this is not an open proxy.
type Passthrough struct {
	Enabled bool                `yaml:"enabled"`
	Routes  []PassthroughRoute  `yaml:"routes,omitempty"`
	Default PassthroughDefaults `yaml:"default,omitempty"`
}

// PassthroughRoute maps a path prefix onto a provider's base URL.
type PassthroughRoute struct {
	Prefix   string   `yaml:"prefix"`
	Provider string   `yaml:"provider"`
	Auth     string   `yaml:"auth,omitempty"`
	Meter    *bool    `yaml:"meter,omitempty"`
	Timeout  Duration `yaml:"timeout,omitempty"`
}

// PassthroughDefaults applies to any route that does not override it.
type PassthroughDefaults struct {
	Auth    string   `yaml:"auth,omitempty"`
	Meter   *bool    `yaml:"meter,omitempty"`
	Timeout Duration `yaml:"timeout,omitempty"`
}

// Passthrough authentication modes (§10.6).
const (
	PassthroughAuthDorang = "dorang"
	PassthroughAuthClient = "client"
	PassthroughAuthNone   = "none"
)

// Shadow mirrors or compares live traffic against a reference gateway (§14.1).
//
// §14.1 writes five keys. The rest of this struct is what enforcing those five
// safely turned out to require, and every one of them is a bound rather than a
// feature: a queue that cannot grow without limit, a capture that cannot retain
// a whole 200 MiB stream, a report that cannot fill a disk, and a cost estimate
// for the requests dorang could not price. §9.6's first rule — no deferred write
// may be unbounded — applies to shadowing exactly as it applies to metering.
type Shadow struct {
	Mode             string          `yaml:"mode,omitempty"`
	Reference        ShadowReference `yaml:"reference,omitempty"`
	SampleRate       float64         `yaml:"sample_rate,omitempty"`
	Compare          ShadowCompare   `yaml:"compare,omitempty"`
	MaxCostUSDPerDay Decimal         `yaml:"max_cost_usd_per_day,omitempty"`

	// UnpricedEstimateUSD is what one shadow call is charged against the daily
	// ceiling when dorang could not price the request it copies. Charging zero
	// would make the ceiling unenforceable for exactly the traffic whose cost is
	// unknown, which is the traffic most worth capping.
	UnpricedEstimateUSD Decimal `yaml:"unpriced_estimate_usd,omitempty"`

	// QueueSize bounds the shadow work queue. Past it, work is dropped and
	// counted (§9.6 rule 3): a full queue must never push back into the request
	// path.
	QueueSize int `yaml:"queue_size,omitempty"`
	// Workers is how many reference calls may be in flight at once.
	Workers int `yaml:"workers,omitempty"`

	Capture ShadowCapture `yaml:"capture,omitempty"`
	Report  ShadowReport  `yaml:"report,omitempty"`
}

// ShadowReference is the gateway being compared against.
type ShadowReference struct {
	URL string `yaml:"url,omitempty"`
	// APIKeyEnv names the environment variable holding the reference gateway's
	// own credential. The client's credential is never forwarded there.
	//
	// This is the one place in the file that spells a secret reference
	// `api_key_env` rather than the `key_env`/`key_file`/`key_ref` triple of
	// §4.1 — §14.1 writes it that way. Deployments that keep secrets in a file
	// or a vault therefore cannot express a shadow reference credential at all.
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// Timeout bounds one reference call. A reference that hangs must free its
	// worker rather than hold it for the process lifetime.
	Timeout Duration `yaml:"timeout,omitempty"`
}

// ShadowCompare selects what a comparison looks at. Model output is not
// deterministic, so structural comparison is the default.
type ShadowCompare struct {
	Structural *bool `yaml:"structural,omitempty"`
	Semantic   bool  `yaml:"semantic"`
	// IgnoreFields are field paths excluded from the structural diff, on top of
	// the built-in set (ids, timestamps, system_fingerprint, output text, token
	// counts). A bare name matches that name at any depth; a path beginning
	// with "$." matches exactly; a trailing "*" matches a prefix.
	IgnoreFields []string `yaml:"ignore_fields,omitempty"`
}

// IsStructural reports whether status, field set, types and header keys are
// compared.
func (s ShadowCompare) IsStructural() bool { return s.Structural == nil || *s.Structural }

// ShadowCapture bounds what one comparison holds of a response body.
//
// Two windows rather than one, because a stream's terminator is the last thing
// on the wire and a single head buffer loses exactly it — and the terminator is
// one of the things §14.1 requires the comparison to check.
type ShadowCapture struct {
	HeadBytes ByteSize `yaml:"head_bytes,omitempty"`
	TailBytes ByteSize `yaml:"tail_bytes,omitempty"`
}

// ShadowReport is the JSONL diff report. An empty report is the completion
// criterion for taking over traffic (§14.1), which is only meaningful if the
// report is also the place inconclusive comparisons are recorded.
type ShadowReport struct {
	Path     string   `yaml:"path,omitempty"`
	MaxBytes ByteSize `yaml:"max_bytes,omitempty"`
}

// Shadow modes (§14.1).
const (
	ShadowModeOff     = "off"
	ShadowModeMirror  = "mirror"
	ShadowModeCompare = "compare"
)

// Notifications configures outbound email (§11.5).
type Notifications struct {
	Email  EmailNotifications `yaml:"email,omitempty"`
	Events []string           `yaml:"events,omitempty"`
}

// EmailNotifications selects the delivery driver.
type EmailNotifications struct {
	Driver string `yaml:"driver,omitempty"`
}

// notificationEvents are the events of §11.5.
var notificationEvents = []string{
	"key_created", "budget_80pct", "budget_exceeded", "quota_exhausted",
	"credential_unhealthy", "batch_completed", "invite",
}

// emailDrivers are the drivers of §11.5.
var emailDrivers = []string{"smtp", "http", "lua", "none"}

// PriorityMapping maps dorang's priority classes onto what each backend
// understands (§7.5).
type PriorityMapping struct {
	Classes map[string]int `yaml:"classes,omitempty"`
	Emit    PriorityEmit   `yaml:"emit,omitempty"`
}

// PriorityEmit is the "emit" block. The design mixes a scalar "header" key with
// per-backend mappings in one YAML mapping, so it is decoded by hand: "header"
// is the header name, every other key names a backend.
type PriorityEmit struct {
	Header   string
	Backends map[string]PriorityEmitBackend
}

// PriorityEmitBackend is one backend's native priority field.
type PriorityEmitBackend struct {
	Field string            `yaml:"field"`
	Map   map[string]string `yaml:"map,omitempty"`
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *PriorityEmit) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == 0 || (n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "")) {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: priority_mapping.emit must be a mapping", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value == "header" {
			if v.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: priority_mapping.emit.header must be a string", v.Line)
			}
			p.Header = v.Value
			continue
		}
		if v.Kind != yaml.MappingNode {
			return fmt.Errorf("line %d: priority_mapping.emit.%s must be a mapping with a "+
				"\"field\" and an optional \"map\"", v.Line, k.Value)
		}
		for j := 0; j+1 < len(v.Content); j += 2 {
			switch v.Content[j].Value {
			case "field", "map":
			default:
				return fmt.Errorf("line %d: field %s not known in priority_mapping.emit.%s",
					v.Content[j].Line, v.Content[j].Value, k.Value)
			}
		}
		var b PriorityEmitBackend
		if err := v.Decode(&b); err != nil {
			return err
		}
		if p.Backends == nil {
			p.Backends = map[string]PriorityEmitBackend{}
		}
		p.Backends[k.Value] = b
	}
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (p PriorityEmit) MarshalYAML() (any, error) {
	out := map[string]any{}
	if p.Header != "" {
		out["header"] = p.Header
	}
	for k, v := range p.Backends {
		out[k] = v
	}
	return out, nil
}

// index is the lookup table built once after a successful load.
type index struct {
	providers   map[string]*Provider
	credentials map[string]*Credential
	models      map[string]*Model
}

func (c *Config) buildIndex() {
	idx := &index{
		providers:   make(map[string]*Provider, len(c.Providers)),
		credentials: make(map[string]*Credential, len(c.Credentials)),
		models:      make(map[string]*Model, len(c.Models)),
	}
	for i := range c.Providers {
		idx.providers[c.Providers[i].Name] = &c.Providers[i]
	}
	for i := range c.Credentials {
		idx.credentials[c.Credentials[i].ID] = &c.Credentials[i]
	}
	for i := range c.Models {
		idx.models[c.Models[i].Name] = &c.Models[i]
	}
	c.idx = idx
}

// Source reports where the configuration was loaded from.
func (c *Config) Source() string { return c.source }

// Provider returns the provider with the given name.
func (c *Config) Provider(name string) (*Provider, bool) {
	if c.idx != nil {
		p, ok := c.idx.providers[name]
		return p, ok
	}
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

// Credential returns the credential with the given id.
func (c *Config) Credential(id string) (*Credential, bool) {
	if c.idx != nil {
		cr, ok := c.idx.credentials[id]
		return cr, ok
	}
	for i := range c.Credentials {
		if c.Credentials[i].ID == id {
			return &c.Credentials[i], true
		}
	}
	return nil, false
}

// Model returns the model group with the given name. The name is compared
// whole: it is never split (§2.1).
func (c *Config) Model(name string) (*Model, bool) {
	if c.idx != nil {
		m, ok := c.idx.models[name]
		return m, ok
	}
	for i := range c.Models {
		if c.Models[i].Name == name {
			return &c.Models[i], true
		}
	}
	return nil, false
}

// ResolveAlias maps a client-facing name through the alias table. A name that
// is not an alias is returned unchanged. Aliases do not chain: an alias target
// must name a model group.
func (c *Config) ResolveAlias(name string) string {
	if target, ok := c.Aliases[name]; ok {
		return target
	}
	return name
}

// ResolveModel resolves an alias if there is one and returns the model group.
func (c *Config) ResolveModel(name string) (*Model, bool) {
	return c.Model(c.ResolveAlias(name))
}

// ClassMembers returns the model groups of a class, which is the scope
// fail-back may delegate within (§3, §7.6).
func (c *Config) ClassMembers(class string) []string {
	return c.Classes[class]
}

// ModelNames returns every declared model group name, sorted.
func (c *Config) ModelNames() []string {
	names := make([]string, 0, len(c.Models))
	for i := range c.Models {
		names = append(names, c.Models[i].Name)
	}
	sort.Strings(names)
	return names
}

// IsDevelopment reports whether the file runs in the relaxed development mode
// that accepts inline literal secrets.
func (c *Config) IsDevelopment() bool { return c.Server.Env == EnvDevelopment }
