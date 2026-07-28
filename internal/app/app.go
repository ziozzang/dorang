// Package app assembles dorang's packages into a running gateway.
//
// Every package under internal/ declares the narrow interfaces it consumes and
// imports none of its siblings (DESIGN §1): internal/server does not know what
// a router is, internal/router does not know what a store is, and internal/batch
// knows none of them. That is deliberate, and it leaves exactly one place where
// all of them have to be named at once. This is that place.
//
// What lives here is glue and nothing else: a *config.Config becomes a store, a
// model catalog, a capacity broker, a health tracker, a prefix table, a price
// catalog, quota meters, an authenticator, a meter, a router, an HTTP server and
// a batch scheduler, plus the adapters that make each of those satisfy the
// interfaces the others declared. No policy is decided here that the packages
// themselves do not already decide.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/backend"
	"github.com/ziozzang/dorang/internal/batch"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/keyguard"
	"github.com/ziozzang/dorang/internal/luaext"
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/metrics"
	"github.com/ziozzang/dorang/internal/notify"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/shadow"
	"github.com/ziozzang/dorang/internal/store"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// Options configures [New].
type Options struct {
	// Config is the loaded, validated configuration. Required.
	Config *config.Config

	// Upstream issues provider requests. Nil builds a default client. A test
	// supplies one whose transport answers a fake upstream.
	Upstream *http.Client

	// CatalogPaths are extra model-catalog files or directories, layered over
	// the embedded data in order. The DORANG_CATALOG_PATH layer is applied
	// after them, exactly as a `dorangctl config lint` would.
	CatalogPaths []string

	// Logf receives operational diagnostics. Nil discards them.
	Logf func(format string, args ...any)

	// Now overrides the clock. Nil means time.Now.
	Now func() time.Time

	// SkipMigrate opens the store without applying migrations.
	SkipMigrate bool

	// Extensions are Go-implemented hooks (DESIGN §11.5). They are the
	// documented extension point for anything the policy language cannot say,
	// and registering one is a compile-time act — so they run whether or not
	// extensions.lua.enabled is set, while the policy *directory* is only read
	// when it is. They see the same secret-free views and run under the same
	// wall-clock watchdog and panic guard as a policy program.
	Extensions []luaext.Native

	// BudgetBlockNanoUSD is the lease size of the durable budget (DESIGN §9.6).
	// Zero uses [DefaultBudgetBlockNanoUSD]. It trades store traffic against
	// the maximum overshoot §5.6 requires to be publishable as a number.
	BudgetBlockNanoUSD int64

	// ClusterTick, ClusterNodeTTL and ClusterLeaseTTL override the node loop's
	// timings (§13). Zero uses internal/cluster's defaults, which are sized for
	// a real deployment: a five-second heartbeat, a thirty-second lapse before
	// a node is declared gone, a fifteen-second leadership lease.
	//
	// They are Options rather than configuration for the reason
	// BudgetBlockNanoUSD is: they are one coherent set of numbers whose
	// relationship matters — the TTL must be several heartbeats or a GC pause
	// evicts a live node — and an operator given three independent keys can
	// write a set that does not hold together. A caller that needs them, and
	// every test of the loop, is a Go caller.
	ClusterTick     time.Duration
	ClusterNodeTTL  time.Duration
	ClusterLeaseTTL time.Duration
}

// App is one assembled gateway. Every exported field is the live subsystem, so
// a test or an administrative surface can reach it without a second assembly
// path existing.
type App struct {
	Store    *store.Store
	Catalog  *catalog.Catalog
	Broker   *capacity.Broker
	Health   *health.Tracker
	Prefix   *prefix.Table
	Interner *prefix.Interner
	Auth     *auth.Authenticator
	Meter    *meter.Meter
	Server   *server.Server
	Batch    *batch.Service
	// Node is this process's membership in the cluster (DESIGN §13): the
	// registry row, the heartbeat, the leadership lease, the leader-only jobs,
	// and the durable ledger.
	//
	// It exists whether or not clustering is on, because [App.Ledger] is its
	// ledger and the durable budget is on the request path either way (§9.6,
	// risk W9). What `cluster.enabled` decides is whether it JOINS — see
	// [App.startBackground]. An unclustered gateway builds this, uses its
	// ledger, and never writes a row to `nodes`.
	Node *cluster.Node
	// Ledger is the durable budget and quota counter of DESIGN §9.6. It is on
	// the request path: the gate reserves against it before every upstream call.
	//
	// It is [Node]'s ledger, not a second one. Two ledgers under one node id
	// would be two in-memory block caches over the same rows: each would draw
	// blocks the other did not know about, and each would return on close what
	// the other had already returned.
	Ledger *cluster.Ledger
	// Coordinator admits or refuses quota spend across nodes for the mode
	// `cluster.capacity_mode` selects (§5.6). It is the node's, so the mode,
	// the node id and the shared backend cannot disagree with the registry.
	//
	// It is not yet on the request path: internal/capacity still counts
	// concurrency per node. What it does today is own the leases and publish
	// the accuracy, which is what makes the figure in §5.6 a measurement of
	// something rather than a restatement of the configuration.
	Coordinator quota.Coordinator
	// Invalidator publishes revocations, pends and early grace cuts, and applies
	// the ones other nodes publish (DESIGN §11.2c, risk W11). Without it a
	// revoked key keeps serving until the auth snapshot's TTL expires, on every
	// node independently — which is a timer, not a revocation.
	Invalidator *cluster.KeyInvalidator
	// KeyControl is the operator-facing set of controls that change whether a
	// key serves: rotate, cut a grace period short, pend, release, revoke. Each
	// one does the durable write AND publishes the invalidation.
	KeyControl *cluster.KeyControl
	// KeyLoader reloads the whole credential set, which is what a node does on
	// rejoin rather than trusting a snapshot it knows is stale (§11.2c rule 4).
	KeyLoader *cluster.KeyLoader
	// Guard is DESIGN §11.6's token guard. Nil when token_guard.enabled is
	// false, which is the default: an automated refusal is something an
	// operator opts into.
	Guard *keyguard.Guard
	// RotationPolicy is `auth.rotation` (§11.2c): the grace period a rotated-away
	// secret keeps, how many secrets may be in flight, and the max_age the
	// gateway WARNS about. It is held on the assembled gateway because a
	// rotation is an operator action arriving over the administration surface,
	// and the policy it honours must be the configured one rather than whatever
	// the caller happened to send.
	RotationPolicy store.RotationPolicy
	// Shadow compares a sampled fraction of traffic against a reference gateway
	// (DESIGN §14.1). Nil when shadow.mode is off, which is the default.
	Shadow *shadow.Shadower
	// Hooks are the §11.5 extension points. Nil when extensions.lua.enabled is
	// false, which is the default, and a nil engine costs one branch per hook
	// point on the request path.
	Hooks *luaext.Engine
	// Notify is the §11.5 notification pipeline. Nil for `driver: none`, which
	// is the default.
	Notify *notify.Notifier
	// OAuth is the §11.2b credential set: the accounts that authenticate with a
	// token read from a vendor CLI's own store and renewed ahead of expiry. Nil
	// when no credential declares `auth: oauth`, which is the default — the
	// absence of the subsystem rather than an empty one, so nothing polls, no
	// metric family is published, and the dispatcher's lookup is a nil check.
	OAuth *auth.OAuthManager
	// Metrics is the DESIGN §12.3 Prometheus surface. It pulls from every
	// subsystem above rather than being pushed to by any of them, so nothing
	// here imports internal/metrics except this package.
	Metrics *metrics.Registry
	// Admin is the DESIGN §2.3 administration surface and the §11.3 operator
	// UI. Nil when this process has no store — there is nothing to administer
	// and nothing to audit — in which case the administrative paths keep
	// answering the 501 they answered before. See admin.go.
	Admin *admin.API

	opts Options
	// pepper is the §2.4 HMAC pepper, kept because the administration surface
	// issues credentials and must hash them exactly as the authenticator and
	// the store do. It is never logged and never rendered: every type that
	// holds it redacts under every fmt verb, and this field is unexported so
	// that a %+v of App cannot reach it.
	pepper   string
	cfg      atomic.Pointer[config.Config]
	requests *metrics.Requests
	// nodes is the live registry count the published overshoot of §5.6 is
	// computed against, refreshed on the node's heartbeat rather than read per
	// scrape. Zero — the unclustered case — reads back as one node.
	nodes liveNodes
	// rates is the rolling-minute observation every subject's rpm_limit and
	// tpm_limit are enforced against (DESIGN §11.2). It is not rebuilt by a
	// reload: the window is live state, and rebuilding it would hand every
	// subject a fresh minute on every SIGHUP.
	//
	// Per SUBJECT, not per key: a team ceiling compared against one key's
	// counter is multiplied by the number of keys under the team, which is the
	// defect the security merge closed.
	rates *keyRates
	// guardHistory is the token guard's usage record. It is per node, which
	// makes the guard's absolute condition a per-node figure in a cluster; the
	// History interface is the seam for a store-backed one (DESIGN §11.6).
	guardHistory *keyguard.MemHistory
	dispatch     *dispatcher
	targets      *batchResolver
	models       *modelList
	quota        *quotaSet
	budget       *budgetGate
	// responses is the Responses API's server-side state (DESIGN §9.2
	// [R1-C7]). Nil when no store is configured, which makes `store: true`
	// answer a named 501 rather than silently not storing.
	responses *responsesStore

	logf func(string, ...any)
	now  func() time.Time

	bgCancel context.CancelFunc
	bgDone   chan struct{}

	closeOnce sync.Once
}

// New assembles a gateway from a validated configuration.
//
// The order is the one DESIGN §1 draws top to bottom, because each stage needs
// the one before it: store, catalog, then the routing substrate (capacity,
// health, prefix, pricing, quota), then the gate (auth), then metering, then
// the router, then the HTTP surface, then batch.
func New(ctx context.Context, opts Options) (*App, error) {
	if opts.Config == nil {
		return nil, errors.New("app: Options.Config is required")
	}
	cfg := opts.Config

	a := &App{
		opts: opts,
		logf: opts.Logf,
		now:  opts.Now,
	}
	if a.logf == nil {
		a.logf = func(string, ...any) {}
	}
	if a.now == nil {
		a.now = time.Now
	}
	a.cfg.Store(cfg)

	// 1. Model catalog. It is needed before the router, which reads a
	//    deployment's protocol family and declared context window from it.
	cat, err := catalog.Load(opts.CatalogPaths...)
	if err != nil {
		return nil, fmt.Errorf("app: model catalog: %w", err)
	}
	a.Catalog = cat

	// 2. Store. SQLite is the notebook default and needs nothing installed
	//    (DESIGN §0.2).
	pepper, generated, err := resolvePepper(cfg)
	if err != nil {
		return nil, err
	}
	if generated {
		a.logf("app: %s is unset; using the generated pepper at %s. "+
			"Set it explicitly before running more than one node.",
			cfg.Server.KeyPepperEnv, pepperPath(cfg))
	}
	a.pepper = pepper
	sc, err := storeConfig(cfg, pepper)
	if err != nil {
		return nil, err
	}
	sc.SkipMigrate = opts.SkipMigrate
	sc.Now = a.now
	st, err := store.Open(ctx, sc)
	if err != nil {
		return nil, fmt.Errorf("app: open store: %w", err)
	}
	a.Store = st

	// 3. Routing substrate.
	a.Broker = capacity.New(brokerConfig(cfg, a.now))
	a.Health = health.New(health.Options{Now: a.now})
	a.Interner = prefix.NewInterner()
	if cfg.Routing.Prefix.IsEnabled() {
		a.Prefix = prefix.NewTable(prefix.Options{
			MaxBytes: cfg.Routing.Prefix.MaxBytes.Bytes(),
			// TableTTL, not Duration: `until_evicted` has to survive the trip or
			// the one class of backend that most needs long affinity — a
			// self-hosted engine with no cache clock — silently gets an hour.
			TTL: cfg.Routing.Prefix.TTL.TableTTL(),
			Now: a.now,
		})
	}
	prices, err := buildPricing(cfg)
	if err != nil {
		return nil, err
	}
	qs, err := newQuotaSet(cfg, a.now)
	if err != nil {
		return nil, err
	}
	a.quota = qs

	// 3a. Cluster membership, and with it the durable budget (DESIGN §13, §9.6,
	//     risk W9).
	//
	//     The node is built here rather than later because it OWNS the ledger,
	//     and the ledger has to exist before the dispatcher whose gate reserves
	//     against it. It is built from the store rather than from memory
	//     because a budget that resets on restart is a budget that is not
	//     enforced.
	//
	//     It is built unconditionally. `cluster.enabled: false` does not mean
	//     "no node", it means "a node that does not join": the ledger is on the
	//     request path in every deployment, and making its owner conditional
	//     would put a second construction path in this function for the case
	//     that matters most. Joining is decided in startBackground.
	//
	//     It is built after the broker because the reservation sweep of §5.3 is
	//     one of its leader jobs and the broker is what it sweeps.
	if err := a.buildNode(cfg, st); err != nil {
		return nil, err
	}

	// 3b. Extensions and notifications (DESIGN §11.5). Both are built before
	//     the dispatcher because the dispatcher holds the one and the budget
	//     gate holds the other, and both are nil by default — the disabled
	//     state is the absence of the object, not a flag inside it.
	if a.Hooks, err = buildHooks(cfg, opts.Extensions, a.logf, a.now); err != nil {
		return nil, err
	}
	if a.Notify, err = buildNotifier(cfg, a.Hooks, a.logf, a.now); err != nil {
		return nil, err
	}
	a.budget = &budgetGate{ledger: a.Ledger, now: a.now, notify: a.Notify}
	a.responses = newResponsesStore(a.Store, a.now)

	// 4. Gate.
	a.rates = newKeyRates(a.now)
	master := os.Getenv(cfg.Server.MasterKeyEnv)
	legacy, err := legacyPolicy(cfg)
	if err != nil {
		return nil, err
	}
	if master == "" {
		a.logf("app: %s is unset; no administrative credential is configured",
			cfg.Server.MasterKeyEnv)
	}
	// The tier set is configuration (DESIGN §11.6): an operator defines the set
	// and the ordering. What is fixed is that a tier is a property of the key,
	// and there is exactly one path by which one reaches a principal — the
	// stored row, through authStore. Nothing here reads a request.
	tiers, err := tierSet(cfg)
	if err != nil {
		return nil, fmt.Errorf("app: tiers: %w", err)
	}
	// The invalidator is built before the authenticator and attached after,
	// because the two are mutually referential: the authenticator publishes
	// revocations through it, and it applies the revocations other nodes
	// publish to the authenticator (DESIGN §11.2c, risk W11).
	invalidator, err := cluster.NewKeyInvalidator(cluster.InvalidatorConfig{
		Store:        st,
		NodeID:       nodeID(cfg),
		Poll:         time.Duration(cfg.Auth.Revocation.Poll),
		Retain:       time.Duration(cfg.Auth.Revocation.Retain),
		StoreLatency: time.Duration(cfg.Auth.Revocation.StoreLatency),
		Now:          a.now,
		Logf:         a.logf,
	})
	if err != nil {
		return nil, fmt.Errorf("app: key invalidator: %w", err)
	}
	authn, err := auth.New(auth.Config{
		Pepper:      pepper,
		MasterKey:   master,
		NoMasterKey: master == "",
		Legacy:      legacy,
		RehashOnUse: cfg.Auth.RehashesOnUse(),
		Store:       &authStore{st: st, tiers: tiers},
		Sink:        invalidator,
		Tiers:       tiers,
		EntryTTL:    time.Duration(cfg.Auth.Revocation.EntryTTL),
		NegativeTTL: time.Duration(cfg.Auth.Revocation.NegativeTTL),
		// A row placed by a bulk load does not expire (§9.1), so the entry TTL
		// is not its fallback. The reload interval is, and it is declared here
		// so the published bound reports the interval this process actually
		// runs rather than a number that does not apply.
		ReloadInterval: reloadInterval(cfg),
		Now:            a.now,
	})
	if err != nil {
		return nil, fmt.Errorf("app: authenticator: %w", err)
	}
	a.Auth = authn
	invalidator.Attach(authn)
	a.Invalidator = invalidator
	a.KeyLoader = cluster.NewKeyLoader(st, 0, tiers)
	if a.KeyControl, err = cluster.NewKeyControl(st, authn, a.Notify, a.now); err != nil {
		return nil, fmt.Errorf("app: key controls: %w", err)
	}
	a.RotationPolicy = rotationPolicy(cfg)
	// The token guard (§11.6). Off by default; keyguard.New answers a disabled
	// configuration with a typed nil, so a deployment that has not asked for an
	// automated refusal pays one nil check for the mechanism.
	gc, err := guardConfig(cfg, a.now)
	if err != nil {
		return nil, fmt.Errorf("app: token guard: %w", err)
	}
	if gc.Enabled {
		a.guardHistory = keyguard.NewMemHistory(0, gc.BaselineWindow, 0)
		if a.Guard, err = keyguard.New(gc, a.guardHistory, a.KeyControl, a.Notify); err != nil {
			return nil, fmt.Errorf("app: token guard: %w", err)
		}
	}

	// 5. Metering. The sink is the store; internal/meter declares Sink and
	//    internal/store implements the operations behind it, and neither
	//    imports the other (DESIGN §9.1).
	mc, err := meterConfig(cfg, st, a.now)
	if err != nil {
		return nil, err
	}
	a.Meter = meter.New(mc)

	// 6. Router.
	rt, err := buildRouter(cfg, cat, routerDeps{
		broker:   a.Broker,
		health:   a.Health,
		prefix:   a.Prefix,
		interner: a.Interner,
		pricing:  prices,
		catalog:  cat,
		quota:    qs,
		now:      a.now,
	})
	if err != nil {
		return nil, err
	}

	up, err := newUpstreamTable(cfg, cat)
	if err != nil {
		return nil, err
	}
	client := opts.Upstream
	if client == nil {
		client = backend.NewClient()
	}
	// The OAuth credential set (DESIGN §11.2b). It is built before the
	// dispatcher because the dispatcher resolves through it, and its background
	// loops start with the rest of them in startBackground — nothing on the
	// request path waits for a refresh.
	if a.OAuth, err = buildOAuth(cfg, a.now); err != nil {
		return nil, err
	}
	a.dispatch = newDispatcher(client, a.OAuth, a.logf, a.now)
	filters, err := buildFilters(cfg, a.logf)
	if err != nil {
		return nil, err
	}
	a.dispatch.swap(&dispatchState{
		router:    rt,
		pricing:   prices,
		catalog:   cat,
		upstreams: up,
		quota:     qs,
		budget:    a.budget,
		responses: a.responses,
		hooks:     a.Hooks,
		filters:   filters,
		prefixOn:  cfg.Routing.Prefix.IsEnabled(),
		chunk:     int(cfg.Routing.Prefix.ChunkBytes.Bytes()),

		usageChunkChoices:    usageChunkChoices(cfg),
		anthropicTotalTokens: anthropicTotalTokens(cfg),
	})
	a.models = newModelList(cfg)

	// 7. Batch. Its Store, Blobs, Executor, Reserver and ModelResolver are all
	//    interfaces internal/batch declares; the adapters are in batch.go.
	if err := a.startBatch(cfg, up); err != nil {
		return nil, err
	}

	// 7b. Shadow comparison (DESIGN §14.1). It is built before the HTTP surface
	//     because the surface holds it, and it is nil for the default `off`
	//     mode — a deployment that is not migrating pays nothing for it.
	if a.Shadow, err = buildShadow(cfg, a.logf); err != nil {
		return nil, err
	}

	// 7c. Metrics (DESIGN §12.3). It is built before the HTTP surface because
	//     the surface serves it, and the surface's own collector is registered
	//     immediately afterwards — that one edge is a cycle in construction
	//     order, not in the package graph.
	a.Metrics = a.buildMetrics(cfg)

	// 7d. Administration (DESIGN §2.3, §11.3). It is built before the HTTP
	//     surface because the surface mounts it, and after the store and the
	//     authenticator because it administers the one through the other.
	//
	//     Until this line the package had no importer at all, which meant no
	//     API path could revoke a leaked key.
	if a.Admin, err = a.buildAdmin(); err != nil {
		return nil, fmt.Errorf("app: administration surface: %w", err)
	}

	// 8. HTTP surface.
	srv, err := server.New(a.serverOptions(cfg))
	if err != nil {
		return nil, fmt.Errorf("app: http server: %w", err)
	}
	a.Server = srv
	a.Metrics.Register(metrics.NewServerCollector(srv))

	a.startBackground()
	return a, nil
}

// Config returns the configuration currently in effect.
func (a *App) Config() *config.Config { return a.cfg.Load() }

// serverOptions renders the HTTP surface's configuration from the file.
func (a *App) serverOptions(cfg *config.Config) server.Options {
	return server.Options{
		Auth:              &authAdapter{a: a.Auth, now: a.now, rates: a.rates},
		Dispatcher:        a.dispatch,
		Models:            a.models,
		Meter:             &meterAdapter{m: a.Meter, now: a.now, record: a.recordMetrics, guard: a.Guard},
		RequestTimeout:    cfg.Server.RequestTimeout.Duration(),
		ShutdownGrace:     cfg.Server.ShutdownGrace.Duration(),
		PreStopDelay:      cfg.Server.PreStop().Duration(),
		AlwaysFullHeaders: cfg.Observability.AlwaysFullHeaders,
		LegacyHeaders:     cfg.Compat.LegacyHeaders,
		Passthrough:       passthroughRoutes(cfg, a.dispatch.state().upstreams),
		Routes:            a.extraRoutes(),
		Observer:          shadowObserver(a.Shadow),
		Metrics:           a.Metrics,
		MetricsAccess:     metricsAccess(cfg),
		HealthReporters:   a.healthReporters(),
		CaptureHeadBytes:  int(cfg.Shadow.Capture.HeadBytes.Bytes()),
		CaptureTailBytes:  int(cfg.Shadow.Capture.TailBytes.Bytes()),
		Now:               a.now,
		Logf:              a.logf,
	}
}

// shadowObserver hands the server a nil interface rather than a typed nil when
// shadowing is off. A typed nil in an interface is non-nil, and the server's
// "is an observer configured" check is a nil test on the hot path.
func shadowObserver(s *shadow.Shadower) server.Observer {
	if s == nil {
		return nil
	}
	return s
}

// Reload swaps in a new configuration (DESIGN §4.1, §13).
//
// Everything that is immutable by construction — the router, the price catalog,
// the upstream table — is rebuilt and swapped by pointer; in-flight requests
// keep the snapshot they started with. The store, the authenticator, the meter
// and the capacity broker are NOT replaced: a broker swap would drop live
// reservations, and a store swap would drop the connection pool under an
// in-flight ledger write.
func (a *App) Reload(cfg *config.Config) error {
	cat, err := catalog.Load(a.opts.CatalogPaths...)
	if err != nil {
		return fmt.Errorf("app: model catalog: %w", err)
	}
	prices, err := buildPricing(cfg)
	if err != nil {
		return err
	}
	qs, err := newQuotaSet(cfg, a.now)
	if err != nil {
		return err
	}
	rt, err := buildRouter(cfg, cat, routerDeps{
		broker:   a.Broker,
		health:   a.Health,
		prefix:   a.Prefix,
		interner: a.Interner,
		pricing:  prices,
		catalog:  cat,
		quota:    qs,
		now:      a.now,
	})
	if err != nil {
		return err
	}
	up, err := newUpstreamTable(cfg, cat)
	if err != nil {
		return err
	}
	// §4.1 says every section hot-reloads. The shadow section is the one that
	// cannot: its daily cost ceiling, its sampled set and its report are
	// per-process state, and rebuilding them on every reload would rearm the
	// ceiling every reload — turning "five dollars a day" into five dollars per
	// SIGHUP. A change is refused rather than silently applied wrongly.
	if err := checkShadowUnchanged(a.cfg.Load(), cfg); err != nil {
		return err
	}
	// The OAuth credential set cannot either, for the same class of reason: a
	// credential holds a token, a backoff and a background loop established at
	// start-up (§11.2b). See checkOAuthUnchanged.
	if err := checkOAuthUnchanged(a.cfg.Load(), cfg); err != nil {
		return err
	}
	// §4.1 says every section hot-reloads, and extensions.lua does: a reload
	// recompiles the policy directory and swaps the engine by pointer, so an
	// in-flight request keeps the engine it started with. A syntax error in a
	// policy file fails the reload rather than half-applying it, which is the
	// same rule the rest of this function follows.
	//
	// The notifier is NOT rebuilt, for the reason the shadow section is not:
	// its deduplication table is what makes budget_80pct one alert rather than
	// one per request, and rebuilding it on every reload would rearm every
	// suppressed alert — turning "once an hour" into "once per SIGHUP".
	hooks, err := buildHooks(cfg, a.opts.Extensions, a.logf, a.now)
	if err != nil {
		return err
	}
	a.Hooks = hooks

	filters, err := buildFilters(cfg, a.logf)
	if err != nil {
		return err
	}
	a.dispatch.swap(&dispatchState{
		router:    rt,
		pricing:   prices,
		catalog:   cat,
		upstreams: up,
		quota:     qs,
		budget:    a.budget,
		responses: a.responses,
		hooks:     hooks,
		filters:   filters,
		prefixOn:  cfg.Routing.Prefix.IsEnabled(),
		chunk:     int(cfg.Routing.Prefix.ChunkBytes.Bytes()),

		usageChunkChoices:    usageChunkChoices(cfg),
		anthropicTotalTokens: anthropicTotalTokens(cfg),
	})
	a.models.swap(cfg)
	a.targets.swap(buildTargets(cfg, cat))
	a.quota = qs
	a.Catalog = cat
	a.cfg.Store(cfg)

	if err := a.Server.Reload(a.serverOptions(cfg)); err != nil {
		return fmt.Errorf("app: http reload: %w", err)
	}
	return nil
}

// startBackground runs the periodic sweeps that are nobody's request path:
// expired sticky pins, expired capacity reservations, budget-lease renewal, and
// — on PostgreSQL — tomorrow's ledger partitions.
func (a *App) startBackground() {
	ctx, cancel := context.WithCancel(context.Background())
	a.bgCancel = cancel
	a.bgDone = make(chan struct{})
	purge := a.cfg.Load().Routing.Sticky.PurgeInterval.Duration()
	if purge <= 0 {
		purge = 5 * time.Minute
	}
	// The budget lease is renewed on its own, much shorter, schedule. A lease
	// renewed only as often as a sticky purge would expire between renewals and
	// be reclaimed out from under a node that is still spending it.
	maintain := cluster.DefaultRenewBefore / 2

	// The invalidation poller is what makes a revocation take effect on this
	// node rather than one cache TTL after it happened (DESIGN §11.2c, W11).
	// It starts BEFORE the rejoin reload, so that a message published while the
	// reload is running is not missed.
	if a.Invalidator != nil {
		if err := a.Invalidator.Start(ctx); err != nil {
			a.logf("app: key invalidation poller: %v", err)
		}
		// §11.2c rule 4: on start a node reloads rather than trusting a
		// snapshot it may have inherited. This is one of the few places the
		// request path is deliberately allowed to wait — serving from a
		// snapshot known to be stale is worse than a brief pause at startup —
		// but it is bounded, and a failure leaves the node reading per key from
		// the store rather than serving what it had.
		rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
		if err := a.Invalidator.Rejoin(rctx, a.KeyLoader); err != nil {
			a.logf("app: credential reload on start: %v", err)
		}
		rcancel()
	}

	// The §11.2b refresh loops, one per OAuth credential. They renew a token
	// while the current one is still valid, so a refresh never sits on a
	// request's critical path — which is the whole mechanism, and it only runs
	// because something starts it.
	if a.OAuth != nil {
		a.OAuth.Start(ctx)
	}

	// The periodic credential reload. It is the TTL fallback for a snapshot row:
	// those do not expire, so without this a node that missed an invalidation
	// would serve a revoked key indefinitely rather than for one entry TTL
	// (§9.1, §11.2c).
	reload := reloadInterval(a.cfg.Load())

	// The token guard's sweep and the max_age rotation warning are periodic
	// judgements, not request-path work.
	guardEvery := time.Duration(a.cfg.Load().TokenGuard.Window)
	if guardEvery <= 0 {
		guardEvery = time.Hour
	}
	maxAge := a.RotationPolicy.MaxAge

	// Cluster membership (§13). The node registers and starts its own heartbeat
	// loop; what stays here is re-reading how many peers are alive, because the
	// published overshoot of §5.6 is proportional to that count and the scrape
	// must not pay a store round trip for it.
	//
	// joinCluster reports false for `cluster.enabled: false`, and then the
	// ticker below is never created and `accuracy` stays nil. A nil channel
	// blocks forever in a select, so the notebook tier pays one dead case in a
	// loop it already runs — no goroutine, no ticker, no query.
	clustered := a.joinCluster(ctx)

	go func() {
		defer close(a.bgDone)
		t := time.NewTicker(purge)
		defer t.Stop()
		m := time.NewTicker(maintain)
		defer m.Stop()
		g := time.NewTicker(guardEvery)
		defer g.Stop()
		r := time.NewTicker(reload)
		defer r.Stop()
		var accuracy <-chan time.Time
		if clustered {
			at := time.NewTicker(a.clusterTick())
			defer at.Stop()
			accuracy = at.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-accuracy:
				a.refreshAccuracy(ctx)
			case <-r.C:
				if a.Auth != nil && a.KeyLoader != nil {
					// Refresh, not Rejoin: this must not drop the snapshot
					// first, or every interval would send every key to the
					// store in the gap. A failure leaves the snapshot as it was
					// and is reported.
					rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					if err := a.Auth.Refresh(rctx, a.KeyLoader); err != nil {
						a.logf("app: credential refresh: %v", err)
					}
					cancel()
				}
			case <-g.C:
				if a.Guard != nil {
					gctx, cancel := context.WithTimeout(ctx, time.Minute)
					if _, err := a.Guard.Sweep(gctx); err != nil {
						a.logf("app: token guard sweep: %v", err)
					}
					cancel()
				}
				if a.KeyControl != nil && maxAge > 0 {
					// A policy, not an execution: this warns and reports, and
					// nothing here rotates, pends or blocks anything (§11.2c).
					wctx, cancel := context.WithTimeout(ctx, time.Minute)
					if _, err := a.KeyControl.WarnOverdue(wctx, maxAge); err != nil {
						a.logf("app: rotation max_age warning: %v", err)
					}
					cancel()
				}
				if a.Invalidator != nil {
					pctx, cancel := context.WithTimeout(ctx, time.Minute)
					if _, err := a.Invalidator.Prune(pctx); err != nil {
						a.logf("app: invalidation prune: %v", err)
					}
					cancel()
				}
			case <-m.C:
				if a.Ledger != nil {
					mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					err := a.Ledger.Maintain(mctx)
					cancel()
					if err != nil {
						a.logf("app: budget lease maintenance: %v", err)
					}
				}
			case <-t.C:
				if st := a.dispatch.state(); st != nil {
					st.router.Purge()
				}
				if a.Store != nil {
					pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					_, err := a.Store.EnsurePartitions(pctx, a.now(), 0)
					cancel()
					if err != nil && !errors.Is(err, store.ErrNoPartitioning) {
						a.logf("app: partition maintenance: %v", err)
					}
				}
			}
		}
	}()
}

// nodeID names this process in its lease rows.
//
// A configured id is used verbatim so that a restarted node reclaims its own
// leases rather than waiting for them to expire. Without one, an id is generated
// per process: a single-node deployment never collides, and a multi-node one is
// required to configure cluster.node_id anyway (§13).
// reloadInterval is how often the whole credential set is re-read.
//
// Half the entry TTL, so a snapshot row's fallback is no worse than a
// runtime-learned row's, and one bulk query replaces the N per-key reads that
// expiring the snapshot would cost. Rows placed by a bulk load do not expire
// (DESIGN §9.1) — this interval is what stands in for the TTL they do not have.
func reloadInterval(cfg *config.Config) time.Duration {
	ttl := time.Duration(cfg.Auth.Revocation.EntryTTL)
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return ttl / 2
}

func nodeID(cfg *config.Config) string {
	if cfg.Cluster.NodeID != "" {
		return cfg.Cluster.NodeID
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "node-local"
	}
	return "node-" + hex.EncodeToString(raw[:])
}

// Close releases everything the app owns, in the reverse order of construction.
// It is safe to call twice.
func (a *App) Close(ctx context.Context) error {
	var firstErr error
	a.closeOnce.Do(func() {
		if a.bgCancel != nil {
			a.bgCancel()
			<-a.bgDone
		}
		if a.Invalidator != nil {
			a.Invalidator.Close()
		}
		if a.Batch != nil {
			if err := a.Batch.Close(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if a.Meter != nil {
			// The meter's Close drains to disk and makes one last flush
			// attempt, so it runs before the store it flushes into closes.
			if err := a.Meter.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if a.Notify != nil {
			// Closed before the store and the meter: it holds no handle on
			// either, and a mail server that does not answer must not extend
			// their shutdown. Queued notifications are delivered inside ctx's
			// grace period and dropped — counted — past it.
			if err := a.Notify.Close(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if a.Shadow != nil {
			// Closed before the meter and the store: it holds no handle on
			// either, and its own grace period must not extend theirs.
			if err := a.Shadow.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if a.OAuth != nil {
			// Stopped before the authenticator for no reason but symmetry with
			// construction: it holds nothing either of them owns. What matters
			// is that it is stopped at all — a refresh loop that outlived the
			// process's shutdown would keep writing a shared token store after
			// dorang stopped serving.
			a.OAuth.Close()
		}
		if a.Auth != nil {
			a.Auth.Close()
		}
		if a.Broker != nil {
			a.Broker.Close()
		}
		// The coordinator before the node, because the leases it holds live in
		// the node's lease table and cluster.Node.Close empties that table. A
		// coordinator closed afterwards would be returning units to rows that
		// no longer exist.
		if a.Coordinator != nil {
			if err := a.Coordinator.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if a.Node != nil {
			// This is where the ledger closes — the node owns it, and this
			// slot is the one the ledger used to occupy, for the same reasons.
			// It runs after the broker so that no reservation is taken against
			// capacity this node is about to stop accounting for, and before
			// the store because every step of it is a write.
			//
			// Returning the unspent part of every block makes a planned restart
			// exact: the durable counter ends up holding precisely what was
			// spent, so the next process reads the true figure rather than a
			// conservative one (DESIGN §9.6). A crash skips this, and the
			// difference is the whole of the published overshoot.
			//
			// The rest of the drain — resign, release the leases, deregister
			// LAST — is ordered inside cluster.Node.Close and argued there. The
			// short version of the part that looks wrong: the registry row is
			// the handle the leader reclaims this node's leases by, so it
			// outlives everything this node still holds.
			if err := a.Node.Close(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if a.Store != nil {
			if err := a.Store.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// resolvePepper returns the HMAC pepper for dorang_v1 key hashing (DESIGN §2.4).
//
// The environment variable named by server.key_pepper_env wins. When it is
// unset, a pepper is generated once and kept beside the state directory, so
// that DESIGN §0.2's "a single binary with no required dependencies serves
// requests" holds without silently making every issued key unverifiable after
// the next restart. A multi-node deployment must set the variable, and the
// caller logs that.
func resolvePepper(cfg *config.Config) (pepper string, generated bool, err error) {
	if name := cfg.Server.KeyPepperEnv; name != "" {
		if v := os.Getenv(name); v != "" {
			return v, false, nil
		}
	}
	path := pepperPath(cfg)
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return string(b), true, nil
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", false, fmt.Errorf("app: generate key pepper: %w", err)
	}
	v := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, fmt.Errorf("app: state directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(v), 0o600); err != nil {
		return "", false, fmt.Errorf("app: write key pepper: %w", err)
	}
	return v, true, nil
}

// pepperPath is where a generated pepper lives: beside the SQLite database, so
// that the database and the key material that makes it readable travel together.
func pepperPath(cfg *config.Config) string {
	dir := filepath.Dir(config.ExpandPath(cfg.Storage.SQLite.Path))
	if dir == "" || dir == "." {
		dir = config.ExpandPath("~/.dorang")
	}
	return filepath.Join(dir, "key_pepper")
}

// legacyPolicy converts the file's legacy window into the authenticator's.
// internal/config has already refused an enabled window with no end date
// (DESIGN §2.4); this only has to parse what survived that.
func legacyPolicy(cfg *config.Config) (auth.LegacyPolicy, error) {
	p := auth.LegacyPolicy{Enabled: cfg.Auth.Legacy.Enabled}
	if !p.Enabled {
		return p, nil
	}
	until, err := parseUntil(cfg.Auth.Legacy.Until)
	if err != nil {
		return p, fmt.Errorf("app: auth.legacy.until: %w", err)
	}
	p.Until = until
	return p, nil
}

func parseUntil(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a date or an RFC 3339 timestamp", s)
	}
	return t.Add(24*time.Hour - time.Nanosecond), nil
}

// quotaSet is the per-credential meter set the router consults.
//
// It is a value rather than a bare map so that a hot reload can swap the whole
// set atomically and so that the dispatcher can feed usage back into it after a
// request completes.
type quotaSet struct {
	meters router.Meters
	// rank scores expiring allowances for the quota_urgency strategy
	// (DESIGN §7.5a(c)). It is built with the meter set rather than on demand,
	// because a Ranker holds the meters it scores and the set is immutable once
	// a reload has swapped it.
	rank *quota.Ranker
}

// ranker is the router's view of the urgency scorer, or a nil interface when no
// credential has a resetting quota rule.
//
// A typed nil would be non-nil in an interface and would make the router call
// through it on the hot path for every candidate, which is exactly the shape
// the shadow observer already had to avoid.
func (q *quotaSet) ranker() router.UrgencySource {
	if q == nil || q.rank == nil {
		return nil
	}
	return q.rank
}

func (q *quotaSet) Check(credential string, now time.Time) quota.Decision {
	if q == nil {
		return quota.Decision{Allow: true}
	}
	return q.meters.Check(credential, now)
}

// record feeds one finished request into the credential's meter.
func (q *quotaSet) record(credential string, now time.Time, u quota.Usage) {
	if q == nil {
		return
	}
	if m, ok := q.meters[credential]; ok && m != nil {
		m.Record(now, u)
	}
}

// usageChunkChoices and anthropicTotalTokens resolve `compat:` onto the wire
// packages' own types.
//
// They are two functions and four lines, and they are the entirety of what was
// missing. COMPATIBILITY §3.3 and §6.8 name two shapes each; internal/wire has
// encoded both members of both pairs since it was written; internal/config has
// parsed, defaulted and documented both spellings. What did not exist was any
// expression relating one to the other, so the loader REFUSED the non-default
// value rather than accept a setting it could not honour — CONFIG §23.1 carried
// both rows and the refusal message named this hop. This is that hop.
//
// The direction of the mapping matters. Each wire type's ZERO value is the shape
// this build already served, so an unset or absent `compat:` block resolves to
// the empty string and no behaviour changes anywhere; only an operator who names
// the other value gets a different frame.
func usageChunkChoices(cfg *config.Config) openai.UsageChunkChoices {
	if cfg != nil && cfg.Compat.UsageChunkChoices == config.UsageChunkChoicesEmpty {
		return openai.UsageChunkChoicesEmpty
	}
	return openai.UsageChunkChoicesStub
}

// anthropicTotalTokens reads a *bool, so "unset" and "false" are distinct: unset
// means the operator expressed no opinion and gets §6.8's compat asymmetry,
// false means they asked for the strict vendor shape and gets it.
func anthropicTotalTokens(cfg *config.Config) anthropic.TotalTokensMode {
	if cfg != nil && cfg.Compat.AnthropicTotalTokens != nil && !*cfg.Compat.AnthropicTotalTokens {
		return anthropic.TotalTokensOmit
	}
	return anthropic.TotalTokensCompat
}
