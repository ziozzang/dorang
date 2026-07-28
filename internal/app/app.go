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

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/batch"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/health"
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
	// Ledger is the durable budget and quota counter of DESIGN §9.6. It is on
	// the request path: the gate reserves against it before every upstream call.
	Ledger *cluster.Ledger
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
	// Metrics is the DESIGN §12.3 Prometheus surface. It pulls from every
	// subsystem above rather than being pushed to by any of them, so nothing
	// here imports internal/metrics except this package.
	Metrics *metrics.Registry

	opts     Options
	cfg      atomic.Pointer[config.Config]
	requests *metrics.Requests
	dispatch *dispatcher
	targets  *batchResolver
	models   *modelList
	quota    *quotaSet
	budget   *budgetGate
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
			TTL:      cfg.Routing.Prefix.TTL.Duration(),
			Now:      a.now,
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

	// 3a. The durable budget (DESIGN §9.6, risk W9). It is built before the
	//     dispatcher because the dispatcher's gate reserves against it, and it
	//     is built from the store rather than from memory because a budget that
	//     resets on restart is a budget that is not enforced.
	block := opts.BudgetBlockNanoUSD
	if block <= 0 {
		block = DefaultBudgetBlockNanoUSD
	}
	ledger, err := cluster.NewLedger(cluster.LedgerConfig{
		Store:  st,
		NodeID: nodeID(cfg),
		Block:  block,
		Now:    a.now,
	})
	if err != nil {
		return nil, fmt.Errorf("app: budget ledger: %w", err)
	}
	a.Ledger = ledger

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
	a.budget = &budgetGate{ledger: ledger, now: a.now, notify: a.Notify}
	a.responses = newResponsesStore(a.Store, a.now)

	// 4. Gate.
	master := os.Getenv(cfg.Server.MasterKeyEnv)
	legacy, err := legacyPolicy(cfg)
	if err != nil {
		return nil, err
	}
	if master == "" {
		a.logf("app: %s is unset; no administrative credential is configured",
			cfg.Server.MasterKeyEnv)
	}
	authn, err := auth.New(auth.Config{
		Pepper:      pepper,
		MasterKey:   master,
		NoMasterKey: master == "",
		Legacy:      legacy,
		RehashOnUse: cfg.Auth.RehashesOnUse(),
		Store:       &authStore{st: st},
		Now:         a.now,
	})
	if err != nil {
		return nil, fmt.Errorf("app: authenticator: %w", err)
	}
	a.Auth = authn

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
		client = defaultUpstreamClient()
	}
	a.dispatch = newDispatcher(client, a.logf, a.now)
	a.dispatch.swap(&dispatchState{
		router:    rt,
		pricing:   prices,
		catalog:   cat,
		upstreams: up,
		quota:     qs,
		budget:    a.budget,
		responses: a.responses,
		hooks:     a.Hooks,
		prefixOn:  cfg.Routing.Prefix.IsEnabled(),
		chunk:     int(cfg.Routing.Prefix.ChunkBytes.Bytes()),
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
		Auth:              &authAdapter{a: a.Auth, now: a.now},
		Dispatcher:        a.dispatch,
		Models:            a.models,
		Meter:             &meterAdapter{m: a.Meter, now: a.now, record: a.recordMetrics},
		RequestTimeout:    cfg.Server.RequestTimeout.Duration(),
		ShutdownGrace:     cfg.Server.ShutdownGrace.Duration(),
		AlwaysFullHeaders: cfg.Observability.AlwaysFullHeaders,
		Passthrough:       passthroughRoutes(cfg, a.Catalog),
		Routes:            a.extraRoutes(),
		Observer:          shadowObserver(a.Shadow),
		Metrics:           a.Metrics,
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

	a.dispatch.swap(&dispatchState{
		router:    rt,
		pricing:   prices,
		catalog:   cat,
		upstreams: up,
		quota:     qs,
		budget:    a.budget,
		responses: a.responses,
		hooks:     hooks,
		prefixOn:  cfg.Routing.Prefix.IsEnabled(),
		chunk:     int(cfg.Routing.Prefix.ChunkBytes.Bytes()),
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
	go func() {
		defer close(a.bgDone)
		t := time.NewTicker(purge)
		defer t.Stop()
		m := time.NewTicker(maintain)
		defer m.Stop()
		for {
			select {
			case <-ctx.Done():
				return
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
		if a.Auth != nil {
			a.Auth.Close()
		}
		if a.Broker != nil {
			a.Broker.Close()
		}
		if a.Ledger != nil {
			// Returning the unspent part of every block makes a planned restart
			// exact: the durable counter ends up holding precisely what was
			// spent, so the next process reads the true figure rather than a
			// conservative one (DESIGN §9.6). A crash skips this, and the
			// difference is the whole of the published overshoot.
			if err := a.Ledger.Close(ctx); err != nil && firstErr == nil {
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
