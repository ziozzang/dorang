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
	Filters       Filters             `yaml:"filters"`

	Passthrough     Passthrough     `yaml:"passthrough"`
	Shadow          Shadow          `yaml:"shadow"`
	Notifications   Notifications   `yaml:"notifications"`
	PriorityMapping PriorityMapping `yaml:"priority_mapping"`
	// TokenGuard is §11.6's per-key anomaly guard. Off by default: an automated
	// refusal is a thing an operator opts into.
	TokenGuard TokenGuard `yaml:"token_guard,omitempty"`
	// Compat is COMPATIBILITY's three operator-settable divergence switches.
	Compat Compat `yaml:"compat,omitempty"`

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

	// PreStopDelay is how long the process keeps serving AFTER readiness has
	// gone false and BEFORE the listener closes (§13).
	//
	// It exists because every load balancer discovers unreadiness by polling.
	// Closing the listener in the same instant readiness flips means the
	// balancer is still routing to a socket that no longer accepts, which is
	// connection-refused on every rolling restart — the exact failure a
	// graceful drain exists to prevent. The delay must therefore cover the
	// balancer's own detection window:
	//
	//	pre_stop_delay >= probe period x failure threshold
	//	                + probe timeout
	//	                + however long the balancer takes to stop routing
	//
	// which is a property of the deployment, not of dorang — so it is
	// configuration, and deploy/kubernetes.yaml ships probe settings whose
	// arithmetic the default satisfies.
	//
	// It is a pointer because zero is a MEANING here and not an absence: a
	// single node, a notebook, or anything not behind a balancer wants the
	// listener closed at once, and `pre_stop_delay: 0` must say that rather
	// than silently re-acquiring the default.
	PreStopDelay *Duration `yaml:"pre_stop_delay,omitempty"`
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

// Auth configures credential verification (§2.4), key rotation (§11.2c) and
// the tier set a key may belong to (§11.6).
type Auth struct {
	Legacy      LegacyAuth `yaml:"legacy"`
	RehashOnUse *bool      `yaml:"rehash_on_use,omitempty"`
	Rotation    Rotation   `yaml:"rotation,omitempty"`
	// Tiers is the operator's tier set, most privileged LAST. Empty means the
	// built-in `free < commercial < unlimited`.
	//
	// What is configuration here is the SET and the ORDERING. What is not is
	// that a tier belongs to the key and is assigned by an operator: there is
	// no request field anywhere that writes one (§10.5, §11.6).
	Tiers []Tier `yaml:"tiers,omitempty"`
	// DefaultTier names the tier a key whose row names none belongs to. Empty
	// means the LEAST privileged tier, because defaulting upward is how an
	// unassigned key silently becomes unlimited.
	DefaultTier string `yaml:"default_tier,omitempty"`
	// Revocation configures how fast a revocation, a pend or an early grace cut
	// actually takes effect across a cluster (§11.2c, risk W11).
	Revocation Revocation `yaml:"revocation,omitempty"`
}

// Rotation is the `auth.rotation` block of §11.2c.
//
// The identity is durable and the secret is not. A rotation mints a new secret
// and leaves everything else — tier, budget, spend, allow-list, rate limits,
// team, ledger history — attached to the key id, because a rotation that also
// reset the limits would be a re-provisioning, and an operator facing that puts
// it off.
type Rotation struct {
	// Grace is how long the old secret stays valid after a rotation.
	Grace Duration `yaml:"grace,omitempty"`
	// MaxSecrets is how many secrets a key may have in flight. Two means one
	// overlap.
	MaxSecrets int `yaml:"max_secrets,omitempty"`
	// MaxAge is a POLICY: dorang warns and reports. It does not silently break
	// a working integration on a timer, and there is deliberately no setting
	// that makes it do so.
	MaxAge Duration `yaml:"max_age,omitempty"`
}

// Revocation is the `auth.revocation` block of §11.2c.
//
// The auth hot path answers from a snapshot with a TTL, so a revoked, pended or
// rotation-cut key keeps serving until the snapshot refreshes — on every node
// independently. These are the numbers that bound that.
type Revocation struct {
	// EntryTTL is how long a SERVING row stays cached. It is the FALLBACK bound
	// for a node that missed the published invalidation, not the mechanism.
	EntryTTL Duration `yaml:"entry_ttl,omitempty"`
	// NegativeTTL is how long a REFUSAL stays cached. It must not exceed
	// entry_ttl: a refused key is cheap to re-check and a serving one is not,
	// and holding both the same is what made the revocation window wide.
	NegativeTTL Duration `yaml:"negative_ttl,omitempty"`
	// Poll is how often a node reads the invalidation table. It is the dominant
	// term in the published clustered bound.
	Poll Duration `yaml:"poll,omitempty"`
	// Retain is how long invalidation messages are kept for a node catching up.
	Retain Duration `yaml:"retain,omitempty"`
	// StoreLatency is the deployment's measured worst-case store round trip,
	// the second term of the published bound. It is configuration because it is
	// a property of the database, not of dorang.
	StoreLatency Duration `yaml:"store_latency,omitempty"`
}

// Tier is one entry of `auth.tiers` (§11.6).
//
// Every limit is a CEILING. A per-key setting may narrow it and may never widen
// it, and batch work of any tier ranks below interactive work of every tier.
type Tier struct {
	Name string `yaml:"name"`
	// PriorityClass is the class a key of this tier is scheduled in. A key may
	// name a LESS urgent class of its own; it may not name a more urgent one.
	PriorityClass string `yaml:"priority_class,omitempty"`
	// MaxBudget is the spend ceiling in USD. Absent means the tier imposes
	// none, which is what `unlimited` looks like.
	MaxBudget *Decimal `yaml:"max_budget,omitempty"`
	RPMLimit  *int64   `yaml:"rpm_limit,omitempty"`
	TPMLimit  *int64   `yaml:"tpm_limit,omitempty"`
	// MaxParallel is the concurrency ceiling.
	MaxParallel *int64 `yaml:"max_parallel_requests,omitempty"`
	// Models is the model set the tier grants. Empty grants every model.
	Models []string `yaml:"models,omitempty"`
}

// TokenGuard is the `token_guard` block of §11.6.
//
// It watches a key against its OWN baseline rather than a fixed threshold,
// because a fixed threshold is wrong for every key except the one it was set
// for. It is not a budget: a budget is a stated ceiling the caller agreed to,
// and this is a statistical judgement that might be wrong — which is why its
// default action is the reversible one.
type TokenGuard struct {
	Enabled        bool     `yaml:"enabled"`
	BaselineWindow Duration `yaml:"baseline_window,omitempty"`
	// Window is the interval the observed rate is measured over, and the unit
	// both rates are expressed in. §11.6's example block does not name it;
	// without it `factor: 10` compares a number to a number of a different
	// kind.
	Window  Duration          `yaml:"window,omitempty"`
	Trigger TokenGuardTrigger `yaml:"trigger,omitempty"`
	// MinHistory is the stated minimum of history below which the guard only
	// alerts. A new key has no baseline, and without this every key trips on
	// its first busy hour.
	MinHistory Duration `yaml:"min_history,omitempty"`
	// Action is pend, revoke or alert_only. `pend` is the default and `throttle`
	// is refused rather than silently downgraded.
	Action   string   `yaml:"action,omitempty"`
	Cooldown Duration `yaml:"cooldown,omitempty"`
}

// Compat is the `compat` block: the three places COMPATIBILITY names a
// divergence an operator gets to choose.
//
// All three were documented as settable and existed only as Go constants, so a
// configuration that set any of them failed to load with an unknown-key error
// and the document was describing a knob that was not there. Two of the three
// then loaded but could not be honoured — the value had to reach internal/wire
// through internal/backend and nothing carried it — so they were REFUSED at
// their non-default value rather than accepted and ignored, which named the gap
// at the moment an operator made the decision instead of letting them believe
// they had changed something.
//
// All three are served at both values now. The carrier is backend.Call's
// UsageChunkChoices and AnthropicTotalTokens, filled from dispatchState in
// internal/app; the refusals in Validate are gone and so are the CONFIG §23.1
// rows that recorded them.
type Compat struct {
	// LegacyHeaders mirrors the reference proxy's response header names
	// alongside dorang's own (§7.7). Off by default: they are another vendor's
	// names, and a gateway that emits them unasked is claiming to be that
	// vendor. Turn it on for a cutover, turn it off once nothing reads them.
	LegacyHeaders bool `yaml:"legacy_headers,omitempty"`

	// UsageChunkChoices selects the shape of a streaming usage chunk's
	// `choices` array (§3.3): "stub" is the reference proxy's
	// [{"index":0,"delta":{}}] and "empty" is strict OpenAI's [].
	//
	// Default "stub", because the clients that exist were built against it.
	// Selecting "empty" also takes a same-family stream off the byte-relay fast
	// path: on that path the usage chunk is the upstream's own bytes and dorang
	// does not choose its shape, so the guarantee can only be made by decoding.
	UsageChunkChoices string `yaml:"usage_chunk_choices,omitempty"`

	// AnthropicTotalTokens reproduces the reference implementation's non-spec
	// `usage.total_tokens` on non-streaming Anthropic responses (§6.8).
	//
	// Default true. It is a pointer because "unset" and "false" have to be
	// distinguishable: unset means no opinion and gets the compat asymmetry,
	// false means the strict vendor shape was asked for. Streaming responses
	// never carry the field at either setting — that asymmetry IS §6.8, not a
	// gap in the switch.
	AnthropicTotalTokens *bool `yaml:"anthropic_total_tokens,omitempty"`
}

// The values `compat.usage_chunk_choices` accepts.
const (
	UsageChunkChoicesStub  = "stub"
	UsageChunkChoicesEmpty = "empty"
)

// TokenGuardTrigger is the pair of conditions, BOTH of which must hold.
//
// A key that used 10 tokens yesterday and 200 today has grown twentyfold and is
// not a problem. Without the absolute floor the guard fires hardest on the
// quietest keys.
type TokenGuardTrigger struct {
	Factor      float64 `yaml:"factor,omitempty"`
	MinAbsolute int64   `yaml:"min_absolute,omitempty"`
}

// RehashesOnUse reports whether a successful legacy verification schedules an
// upgrade to the current scheme.
func (a Auth) RehashesOnUse() bool { return a.RehashOnUse == nil || *a.RehashOnUse }

// PreStop returns the pre-stop delay, answering for a Config that never went
// through [Config.ApplyDefaults] as well as for one that did. An explicit zero
// is honoured; only an absent key takes the default.
func (s Server) PreStop() Duration {
	if s.PreStopDelay == nil {
		return Duration(defaultPreStopDelay)
	}
	return *s.PreStopDelay
}

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

	// PrefixTTL is how long this provider's prefix affinity stays believable,
	// overriding routing.prefix.ttl (§7.4b).
	//
	// It belongs here because it is a fact about the backend, not about dorang.
	// A hosted service holds a cached prefix for a vendor-set window — around
	// five minutes for OpenAI's automatic caching, five minutes on Anthropic's
	// default tier and an hour on its extended one — while vLLM and SGLang hold
	// blocks until LRU eviction under memory pressure and have no window at all.
	// One global hour is wrong in both directions: too long for the hosted case,
	// where it pins a conversation to a node that no longer has the prefix and
	// costs load balance for nothing, and too short for the self-hosted case,
	// where it discards hits that were still there. Write `until_evicted` for
	// the self-hosted class.
	PrefixTTL CacheTTL `yaml:"prefix_ttl,omitempty"`
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
	Key SecretRef `yaml:",inline"`
	// Auth selects how this credential authenticates: [AuthKey] (the default) or
	// [AuthOAuth]. It is written out rather than inferred from the presence of
	// the oauth block, because §11.2b's example writes it out and because a
	// credential that carries both a key and an oauth block is a mistake worth
	// naming rather than resolving by precedence.
	Auth string `yaml:"auth,omitempty"`
	// OAuth is §11.2b's block. Non-nil only for `auth: oauth`.
	OAuth         *OAuth `yaml:"oauth,omitempty"`
	CapacityGroup string `yaml:"capacity_group,omitempty"`
}

// How a credential authenticates (§11.2b).
const (
	// AuthKey is a static secret: key_env, key_file or key.
	AuthKey = "key"
	// AuthOAuth is a token read from a store and refreshed ahead of expiry.
	AuthOAuth = "oauth"
)

// IsOAuth reports whether this credential authenticates by OAuth.
func (c Credential) IsOAuth() bool { return c.Auth == AuthOAuth }

// OAuth is one credential's `oauth` block (§11.2b).
//
// It points at a token store that already exists — the vendor CLI's own — so
// that an operator who is signed in stays signed in rather than running a second
// authorization flow on a server. dorang reads the token out of it, and refreshes
// it only when a `refresh` endpoint is configured.
type OAuth struct {
	// Source is where the token is read from: file, exec or env.
	Source string `yaml:"source,omitempty"`
	// Path is the store, for `source: file`. A leading ~/ expands.
	//
	// It is a REFERENCE to a store, never the store's content: nothing about the
	// token reaches this file, and a path is all that is written down.
	Path string `yaml:"path,omitempty"`
	// Command is the argv, for `source: exec`.
	Command []string `yaml:"command,omitempty"`
	// EnvVar is the variable, for `source: env`.
	EnvVar string `yaml:"env_var,omitempty"`

	// Format names the store's layout: generic, codex, claude or gemini. A
	// vendor's file is not dorang's to redesign, so its shape is named rather
	// than spelled out by every deployment.
	Format string `yaml:"format,omitempty"`

	// The four field names, where a store departs from its format's. Each may be
	// a dotted path into a nested object.
	AccessTokenField  string `yaml:"access_token_field,omitempty"`
	RefreshTokenField string `yaml:"refresh_token_field,omitempty"`
	ExpiresAtField    string `yaml:"expires_at_field,omitempty"`
	AccountIDField    string `yaml:"account_id_field,omitempty"`

	// AccountHeader is the header the account id travels in, where a provider
	// requires one. Empty means it is not sent.
	AccountHeader string `yaml:"account_header,omitempty"`

	// RefreshMargin is how far ahead of expiry the token is renewed.
	RefreshMargin Duration `yaml:"refresh_margin,omitempty"`
	// PollInterval is how often the background loop checks the clock. It is
	// clamped to a quarter of the margin, so the margin cannot be slept through.
	PollInterval Duration `yaml:"poll_interval,omitempty"`
	// ExecTimeout bounds a `source: exec` command.
	ExecTimeout Duration `yaml:"exec_timeout,omitempty"`

	// Refresh is the token endpoint. Absent means dorang never exchanges
	// anything: it reads the store, adopts what the vendor's CLI put there, and
	// writes nothing back. That is the safe default, and for a machine where the
	// vendor's CLI is running anyway it is also the whole feature.
	Refresh OAuthRefresh `yaml:"refresh,omitempty"`
}

// OAuthRefresh is the RFC 6749 refresh_token exchange (§11.2b).
//
// The endpoint and the client id live in configuration rather than in a table of
// vendors compiled into dorang: they are published values of a third party's
// client registration, and a third party rotating one must not require a new
// dorang build.
type OAuthRefresh struct {
	// TokenURL is the endpoint. Setting it is what turns refresh on.
	TokenURL string `yaml:"token_url,omitempty"`
	// ClientID identifies the OAuth client.
	ClientID string `yaml:"client_id,omitempty"`
	// ClientSecret carries key_env / key_file / key_ref / key as sibling keys,
	// exactly as a credential's own secret does. Empty is the public-client
	// case, which is what a CLI usually registers.
	ClientSecret SecretRef `yaml:",inline"`
	// Scope is sent when non-empty.
	Scope string `yaml:"scope,omitempty"`
	// Encoding is `form` (the default, RFC 6749's own) or `json`.
	Encoding string `yaml:"encoding,omitempty"`
	// Timeout bounds one exchange.
	Timeout Duration `yaml:"timeout,omitempty"`
}

// IsZero reports whether no refresh endpoint is configured.
func (r OAuthRefresh) IsZero() bool {
	return r.TokenURL == "" && r.ClientID == "" && r.Scope == "" &&
		r.Encoding == "" && r.Timeout == 0 && r.ClientSecret.IsZero()
}

// oauthRefreshYAML is the marshalled shape of an OAuthRefresh: the client secret
// spelled as a reference, with no field an inline literal can land in.
type oauthRefreshYAML struct {
	TokenURL string   `yaml:"token_url,omitempty"`
	ClientID string   `yaml:"client_id,omitempty"`
	KeyEnv   string   `yaml:"key_env,omitempty"`
	KeyFile  string   `yaml:"key_file,omitempty"`
	KeyRef   string   `yaml:"key_ref,omitempty"`
	Scope    string   `yaml:"scope,omitempty"`
	Encoding string   `yaml:"encoding,omitempty"`
	Timeout  Duration `yaml:"timeout,omitempty"`
}

// MarshalYAML writes the refresh block without its client secret, for the reason
// [Credential.MarshalYAML] exists: `yaml:",inline"` flattens the embedded
// SecretRef field by field and never calls its own MarshalYAML, so a literal
// would otherwise be written straight back out.
func (r OAuthRefresh) MarshalYAML() (any, error) {
	env, file, ref := r.ClientSecret.Reference3()
	return oauthRefreshYAML{
		TokenURL: r.TokenURL, ClientID: r.ClientID,
		KeyEnv: env, KeyFile: file, KeyRef: ref,
		Scope: r.Scope, Encoding: r.Encoding, Timeout: r.Timeout,
	}, nil
}

// credentialYAML is the marshalled shape of a Credential: every field, with the
// secret reference spelled out and the inline literal structurally absent.
type credentialYAML struct {
	ID            string `yaml:"id"`
	Provider      string `yaml:"provider"`
	KeyEnv        string `yaml:"key_env,omitempty"`
	KeyFile       string `yaml:"key_file,omitempty"`
	KeyRef        string `yaml:"key_ref,omitempty"`
	Auth          string `yaml:"auth,omitempty"`
	OAuth         *OAuth `yaml:"oauth,omitempty"`
	CapacityGroup string `yaml:"capacity_group,omitempty"`
}

// MarshalYAML writes a credential without its secret.
//
// It exists because `yaml:",inline"` bypasses [SecretRef.MarshalYAML]: the
// encoder flattens the embedded struct's exported fields, Inline among them, so
// the type's own redaction was never consulted and a literal key was written to
// whatever file the caller was generating. DESIGN §4.1 — "secrets never appear
// in configuration" — was true only of the code path that resolves them first.
//
// The shape here has no field a literal can land in, so the guarantee does not
// depend on this function remembering to omit one.
func (c Credential) MarshalYAML() (any, error) {
	env, file, ref := c.Key.Reference3()
	return credentialYAML{
		ID: c.ID, Provider: c.Provider,
		KeyEnv: env, KeyFile: file, KeyRef: ref,
		Auth: c.Auth, OAuth: c.OAuth,
		CapacityGroup: c.CapacityGroup,
	}, nil
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
//
// RPM and TPM are declared here because §5.2 writes them here, and they are
// REFUSED at validation. A rate ceiling and a concurrency ceiling are different
// mechanisms — the first needs a time window, the second a counted gauge — and
// internal/capacity implements only the second. The rate half has a working
// home in internal/quota, reached from `models[].deployments[].limits[]`, and
// accepting a second spelling that does nothing is the defect this whole
// section exists to prevent. See [Config.validateCapacity].
type CapacityLimits struct {
	MaxConcurrency int   `yaml:"max_concurrency,omitempty"`
	RPM            int   `yaml:"rpm,omitempty"`
	TPM            int64 `yaml:"tpm,omitempty"`
	// MaxQueue bounds how many requests may wait on this axis. Past it,
	// admission fails immediately rather than joining an unbounded queue.
	MaxQueue int `yaml:"max_queue,omitempty"`
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
	MaxConcurrent int `yaml:"max_concurrent,omitempty"`
	// MaxQueueWait bounds how long one of this principal's requests may wait
	// for capacity before it is refused. Zero means it never waits.
	MaxQueueWait Duration `yaml:"max_queue_wait,omitempty"`
	// RPM and TPM are refused, for the reason given on [CapacityLimits]: a
	// per-caller rate ceiling is carried by the api key's own rpm_limit and
	// tpm_limit, which internal/quota enforces.
	RPM int   `yaml:"rpm,omitempty"`
	TPM int64 `yaml:"tpm,omitempty"`
	// MaxQueue bounds how many of this principal's requests may wait at once.
	MaxQueue int `yaml:"max_queue,omitempty"`

	// ClientPriority is the §10.5 grant: "ignore" (the default) drops a
	// client-supplied priority hint and reports the drop; "allow" honours it,
	// clamped to Range.
	//
	// An operator can grant urgency, a caller cannot claim it. That asymmetry
	// is the whole point: priority is a claim on shared capacity, so a caller
	// permitted to set it eventually sets the most urgent value.
	ClientPriority string `yaml:"client_priority,omitempty"`
	// Range is the two priority CLASS NAMES a granted hint is clamped between,
	// written as §10.5 writes it: [batch, interactive]. Both must name a key of
	// priority_mapping.classes. Order does not matter — the pair is a closed
	// interval on the canonical scale, not a direction.
	Range []string `yaml:"range,omitempty"`
}

// Client-priority grants (§10.5).
const (
	ClientPriorityIgnore = "ignore"
	ClientPriorityAllow  = "allow"
)

// GrantsClientPriority reports whether this principal's callers may set their
// own priority. The zero value does not: the hint is ignored by default.
func (p PrincipalLimits) GrantsClientPriority() bool {
	return p.ClientPriority == ClientPriorityAllow
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

// rotationKeyYAML is the marshalled shape of a RotationKey. See
// [Credential.MarshalYAML] for why the inline literal cannot be represented.
type rotationKeyYAML struct {
	ID             string `yaml:"id"`
	KeyEnv         string `yaml:"key_env,omitempty"`
	KeyFile        string `yaml:"key_file,omitempty"`
	KeyRef         string `yaml:"key_ref,omitempty"`
	MaxConcurrency int    `yaml:"max_concurrency,omitempty"`
	CapacityGroup  string `yaml:"capacity_group,omitempty"`
}

// MarshalYAML writes a rotation key without its secret. This is the second of
// the two `yaml:",inline"` uses of SecretRef, and it had the same hole.
func (r RotationKey) MarshalYAML() (any, error) {
	env, file, ref := r.Key.Reference3()
	return rotationKeyYAML{
		ID: r.ID, KeyEnv: env, KeyFile: file, KeyRef: ref,
		MaxConcurrency: r.MaxConcurrency, CapacityGroup: r.CapacityGroup,
	}, nil
}

// Model is one client-facing name and the deployments behind it (§3).
// The name is opaque: nothing splits it (§2.1).
type Model struct {
	Name        string       `yaml:"name"`
	Class       string       `yaml:"class,omitempty"`
	Strategy    []string     `yaml:"strategy,omitempty"`
	Deployments []Deployment `yaml:"deployments,omitempty"`
	// Filters attaches transform filters to this model (§10.5b). They run
	// between the canonical request and the backend, in the order written.
	Filters []ModelFilter `yaml:"filters,omitempty"`
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
	// PrefixTTL overrides the provider's affinity lifetime for this deployment.
	// It is the most specific of the three levels and wins over both.
	//
	// A deployment is the level at which a per-request cache tier is visible:
	// two deployments can point at the same vendor with different caching
	// arrangements, and only the deployment knows which.
	PrefixTTL CacheTTL `yaml:"prefix_ttl,omitempty"`
}

// PrefixTTLFor resolves the affinity lifetime of one deployment: the
// deployment's own value, else its provider's, else the global default.
func (c *Config) PrefixTTLFor(provider string, d *Deployment) CacheTTL {
	out := CacheTTL{}
	if d != nil {
		out = d.PrefixTTL
	}
	if p, ok := c.Provider(provider); ok {
		out = out.Or(p.PrefixTTL)
	}
	return out.Or(c.Routing.Prefix.TTL)
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
	// TTL is the DEFAULT affinity lifetime. It is overridden per provider and
	// per deployment, because the thing it models — how long the backend still
	// holds the KV blocks for this prefix — is a property of the backend and not
	// of the gateway. See [Provider.PrefixTTL].
	TTL CacheTTL `yaml:"ttl,omitempty"`
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
	// Source and AsOf are the provenance a notional_rate rule must carry
	// (§8.5). They are load errors rather than warnings on that class, and they
	// are refused on every other class: a rate with no date cannot be judged
	// stale, and an estimate with no source is a guess wearing a currency
	// symbol.
	Source string `yaml:"source,omitempty"`
	AsOf   string `yaml:"as_of,omitempty"`
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

// Pricing rule classes (§8.1, §8.5).
const (
	PricingMarginalUsage     = "marginal_usage"
	PricingFixedSubscription = "fixed_subscription"
	PricingAdjustment        = "adjustment"
	PricingNotionalRate      = "notional_rate"
)

// pricingComponents are the priced quantities of §8.3.
//
// Both spellings of the cached-input component are accepted here. The main file
// has always written `cached_read` and the price catalog has always written
// `cache_read`; a rule moved between the two files should not stop pricing
// because of the underscore, so the two are synonyms and [CanonicalComponent]
// resolves them.
//
// `images` is NOT here. §8.3 lists it among the priced quantities and
// internal/pricing has no per-image unit, so a rule that priced images passed
// validation and then failed to assemble — a load error one layer too late.
var pricingComponents = []string{
	"input", "output", "cached_read", "cache_read", "cache_write", "reasoning",
	"request", "characters", "seconds",
}

// CanonicalComponent maps a priced-component name onto the price catalog's
// spelling, which is the one internal/pricing understands.
func CanonicalComponent(name string) string {
	if name == "cached_read" {
		return "cache_read"
	}
	return name
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
	// Prometheus serves GET /metrics. False removes the route entirely: a
	// disabled scrape endpoint answers 501 like any other route this build does
	// not serve, rather than 200 with an empty body.
	Prometheus   *bool          `yaml:"prometheus,omitempty"`
	Metrics      MetricsSurface `yaml:"metrics,omitempty"`
	OTLPEndpoint string         `yaml:"otlp_endpoint,omitempty"`
	LogLevel     string         `yaml:"log_level,omitempty"`
	LogFormat    string         `yaml:"log_format,omitempty"`
	// AlwaysFullHeaders attaches the full extension header set to every
	// response instead of only on request (§10.4).
	AlwaysFullHeaders bool `yaml:"always_full_headers,omitempty"`
}

// MetricsSurface configures who may read GET /metrics.
//
// The scrape carries per-key spend, per-credential quota state and every
// configured model name. That is a description of a deployment's commercial
// arrangements, so it authenticates by default and is opened deliberately.
type MetricsSurface struct {
	// Public serves /metrics to anyone who can reach the listener. It is a
	// deliberate downgrade for a deployment whose listener is already private —
	// a scrape sidecar on a pod network, or a bound loopback address — and it is
	// spelled out rather than being the default, because "the network is
	// private" is a claim the gateway cannot check.
	Public bool `yaml:"public,omitempty"`
}

// PrometheusEnabled reports whether /metrics is served.
func (o Observability) PrometheusEnabled() bool { return o.Prometheus == nil || *o.Prometheus }

// MetricsPublic reports whether /metrics skips authentication.
func (o Observability) MetricsPublic() bool { return o.Metrics.Public }

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

// Filter failure modes (§10.5b). A filter that cannot enrich a request may be
// skipped; a filter that was supposed to remove an identity number and did not
// must stop the request. `closed` is the safe declaration and the default.
const (
	FilterFailClosed = "closed"
	FilterFailOpen   = "open"
)

// Filter phases (§10.5b). A mask is applied on the way out and undone on the
// way back, so the two halves are named separately.
const (
	FilterOnRequest  = "request"
	FilterOnResponse = "response"
)

// Filter placeholder scopes (§10.5b). The scope is how widely one original
// value keeps the same placeholder: within one conversation, across everything
// a principal or a tenant sends, or not at all beyond a single request.
const (
	FilterScopeConversation = "conversation"
	FilterScopePrincipal    = "principal"
	FilterScopeTenant       = "tenant"
	FilterScopeRequest      = "request"
)

// builtinFilterPatterns are the pattern names a filter may name without writing
// a regexp. Anything else needs one: an operator's disclosure rules are theirs,
// and a gateway cannot ship the right pattern set for every jurisdiction
// (§10.5b).
var builtinFilterPatterns = []string{"krrn", "email"}

// BuiltinFilterPatterns returns the pattern names that need no regexp. It
// returns a copy: the list is a contract between this package and whatever
// compiles the patterns, not a variable to edit.
func BuiltinFilterPatterns() []string {
	return append([]string(nil), builtinFilterPatterns...)
}

// Filters is the transform-filter plugin surface (§10.5b): filters that sit
// between the canonical request and the backend, may rewrite the request, and
// may rewrite the response on the way back. The motivating case is reversible
// PII masking.
type Filters struct {
	// Secret is the cluster-wide seed a placeholder is derived from.
	//
	// It is a secret rather than a generated value because it must be the SAME
	// on every node and across every restart. A per-process seed makes the same
	// original text mask to a different placeholder on each node, so the bodies
	// that reach a backend differ byte for byte and every backend's prefix cache
	// cold-starts (§7.4b). It is a [SecretRef] like every other secret, so it is
	// spelled key_env or key_file and never written in the file (§4.1).
	Secret SecretRef `yaml:"secret,omitempty"`
	// Plugins are the loadable filters. Loading is explicit configuration and
	// never a directory scan (§11.5).
	Plugins []FilterPlugin `yaml:"plugins,omitempty"`
}

// FilterPlugin declares one loadable plugin.
type FilterPlugin struct {
	// Name is what a model's filter list refers to.
	Name string `yaml:"name"`
	// Path is the plugin file. Nothing is loaded that is not named here.
	Path string `yaml:"path"`
	// Fail is "closed" or "open"; see [FilterFailClosed]. Defaults to closed.
	Fail string `yaml:"fail,omitempty"`
	// Config is handed to the plugin verbatim. It holds no secret: a plugin
	// reaches no credential (§11.5).
	Config map[string]string `yaml:"config,omitempty"`
}

// ModelFilter attaches a declared plugin to one model (§10.5b).
type ModelFilter struct {
	// Plugin names an entry of filters.plugins.
	Plugin string `yaml:"plugin"`
	// On lists the phases the filter runs in, a subset of "request" and
	// "response". It defaults to both, and a response-only filter is refused:
	// the mask → original table is built when the request is rewritten, so
	// there is nothing to unmask if nothing was masked.
	On []string `yaml:"on,omitempty"`
	// Scope is how widely one original value keeps one placeholder. It defaults
	// to "conversation"; see [FilterScopeConversation].
	Scope string `yaml:"scope,omitempty"`
	// Patterns are what to look for. Each is either the name of a built-in (see
	// [BuiltinFilterPatterns]) or a name and a regexp.
	Patterns []FilterPattern `yaml:"patterns,omitempty"`
	// Retain bounds how long a scope's placeholder assignment stays usable.
	// Zero, the default, is off: nothing outlives the request.
	Retain Duration `yaml:"retain,omitempty"`
}

// FilterPattern is one pattern. It reads both spellings the design writes:
//
//	patterns: [krrn, email]
//	patterns: [{name: employee_id, regexp: 'EMP-\d{6}'}]
//
// A bare scalar is a name; a mapping carries a name and a regexp. The regexp is
// not compiled here — this package performs no I/O and holds no matcher; it
// only checks that a pattern is nameable.
type FilterPattern struct {
	Name   string `yaml:"name"`
	Regexp string `yaml:"regexp,omitempty"`
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *FilterPattern) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return fmt.Errorf("line %d: a filter pattern must not be empty", n.Line)
		}
		*p = FilterPattern{Name: n.Value}
		return nil
	case yaml.MappingNode:
		// A custom unmarshaler is not reached by the decoder's KnownFields
		// setting, so the keys are checked here: a typo in a pattern is
		// otherwise a pattern that silently never matches.
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch n.Content[i].Value {
			case "name", "regexp":
			default:
				return fmt.Errorf("line %d: field %s not known in a filter pattern",
					n.Content[i].Line, n.Content[i].Value)
			}
		}
		type plain FilterPattern
		var v plain
		if err := n.Decode(&v); err != nil {
			return err
		}
		*p = FilterPattern(v)
		return nil
	}
	return fmt.Errorf("line %d: a filter pattern must be a built-in name such as \"krrn\", "+
		"or a mapping with a \"name\" and a \"regexp\"", n.Line)
}

// MarshalYAML implements yaml.Marshaler. A pattern with no regexp writes back
// as the bare name it was read as.
func (p FilterPattern) MarshalYAML() (any, error) {
	if p.Regexp == "" {
		return p.Name, nil
	}
	type plain FilterPattern
	return plain(p), nil
}

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
//
// Everything below `email` is delivery; everything beside it is the pipeline.
// The pipeline exists because a notification is a deferred write and §9.6
// requires a deferred write to be bounded, visible when it drops, and never on
// the request path.
type Notifications struct {
	Email  EmailNotifications `yaml:"email,omitempty"`
	Events []string           `yaml:"events,omitempty"`

	// QueueSize bounds the queue between the request path and the sender.
	// Reaching it drops, and the drop is counted.
	QueueSize int `yaml:"queue_size,omitempty"`
	// Workers deliver from the queue.
	Workers int `yaml:"workers,omitempty"`
	// DedupPeriod is how often one subject may raise the same event. Without
	// it, budget_80pct fires on every request past the threshold.
	DedupPeriod Duration `yaml:"dedup_period,omitempty"`
	// DedupPeriods overrides the window per event name. Zero disables
	// deduplication for that event.
	DedupPeriods map[string]Duration `yaml:"dedup_periods,omitempty"`
	// Retry bounds how hard a failing transport is retried.
	Retry NotifyRetry `yaml:"retry,omitempty"`
}

// EmailNotifications selects the delivery driver and configures it.
type EmailNotifications struct {
	Driver string `yaml:"driver,omitempty"`
	// From is the envelope sender, required by the smtp driver.
	From string `yaml:"from,omitempty"`
	// To are the default recipients.
	To []string `yaml:"to,omitempty"`

	SMTP SMTPSettings    `yaml:"smtp,omitempty"`
	HTTP WebhookSettings `yaml:"http,omitempty"`
}

// SMTPSettings configures the smtp driver. The password is a [SecretRef] like
// every other secret: it is never written in the configuration file (§4.1).
type SMTPSettings struct {
	Addr     string    `yaml:"addr,omitempty"`
	Username string    `yaml:"username,omitempty"`
	Password SecretRef `yaml:",inline"`
	// StartTLS upgrades the connection when the server offers it. Leaving it
	// off while setting a username means the password crosses the wire in the
	// clear, which net/smtp refuses for anything but a loopback server.
	StartTLS bool `yaml:"starttls,omitempty"`
	// TLSSkipVerify accepts an unverifiable certificate. It exists for a
	// private relay and is a deliberate downgrade.
	TLSSkipVerify bool     `yaml:"tls_skip_verify,omitempty"`
	Timeout       Duration `yaml:"timeout,omitempty"`
	// HELO is the name dorang announces itself as.
	HELO string `yaml:"helo,omitempty"`
}

// WebhookSettings configures the http driver.
//
// Secret is required, not optional. A delivery carrying budget and quota state
// is an information leak the moment the URL is reachable by anything else, and
// an unsigned receiver has no way to tell a real delivery from a forged one
// (§11.5 rule 1). It is a [SecretRef] like every other secret, so it is spelled
// `key_env`, `key_file` or `key_ref`.
type WebhookSettings struct {
	URL     string            `yaml:"url,omitempty"`
	Secret  SecretRef         `yaml:",inline"`
	Timeout Duration          `yaml:"timeout,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

// NotifyRetry bounds delivery retries. A failing mail server must not be
// retried hard: past MaxAttempts the notification is dropped and counted, and
// past BreakerThreshold consecutive failures delivery is not attempted at all
// until BreakerCooldown elapses.
type NotifyRetry struct {
	MaxAttempts      int      `yaml:"max_attempts,omitempty"`
	InitialBackoff   Duration `yaml:"initial_backoff,omitempty"`
	MaxBackoff       Duration `yaml:"max_backoff,omitempty"`
	BreakerThreshold int      `yaml:"breaker_threshold,omitempty"`
	BreakerCooldown  Duration `yaml:"breaker_cooldown,omitempty"`
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
