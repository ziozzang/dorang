package metrics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/batch"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/shadow"
)

// TestWholeSurfaceValidates drives every collector in the package, including
// the ones the core test cannot build without a store, and runs the naming and
// typing rules over the result.
//
// This is the test that keeps a new family from shipping with a `_total` gauge
// or a `_ratio` above one.
func TestWholeSurfaceValidates(t *testing.T) {
	body := everyCollector(t).Metrics(nil)
	if _, err := Parse(body); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	for _, e := range Validate(body) {
		t.Errorf("validation: %v", e)
	}

	// Spot-check that the §12.3 headline families are all present, by name.
	for _, want := range []string{
		"dorang_requests_total",
		"dorang_request_duration_seconds",
		"dorang_ttft_seconds",
		"dorang_tokens_total",
		"dorang_cost_nano_total",
		"dorang_notional_nano_total",
		"dorang_capacity_inflight",
		"dorang_capacity_limit",
		"dorang_capacity_wait_seconds",
		"dorang_credential_health",
		"dorang_provider_quota_used_percent",
		"dorang_budget_spent_ratio",
		"dorang_prefix_hit_ratio",
		"dorang_fallback_total",
		"dorang_meter_dropped_total",
		"dorang_meter_spool_depth",
		"dorang_quota_urgency",
		"dorang_coordination_max_overshoot",
		"dorang_capacity_overshoot_measured",
		"dorang_oauth_refreshes_total",
		"dorang_shadow_skipped_total",
		"dorang_batch_queue_depth",
		"dorang_batch_rows_in_flight",
		"dorang_auth_cache_hit_ratio",
		"dorang_store_pool_saturation_ratio",
		"dorang_auth_rehash_pending",
		"dorang_build_info",
	} {
		if !hasFamily(t, body, want) {
			t.Errorf("%s is missing from the scrape", want)
		}
	}
}

// TestQuotaPercentIsAPercentage is rule 2 with the specific case DESIGN §12.3
// names. VLLM.md §3.1 records a backend gauge called `kv_cache_usage_perc`
// whose value is a 0–1 fraction and whose own documentation string admits "1
// means 100 percent usage". A dashboard built on that is wrong by a factor of a
// hundred and looks entirely reasonable.
func TestQuotaPercentIsAPercentage(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	m, err := quota.NewMeter(quota.MeterConfig{
		Rules: []quota.Rule{{
			Window: quota.Rolling(time.Hour), Metric: quota.MetricRequests,
			Limit: 200, OnExhaust: quota.Cooldown,
		}},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		m.Record(now, quota.Usage{Requests: 1})
	}

	c := NewQuotaCollector(func() time.Time { return now }, 0)
	c.Track("cred-1", m)
	r := New(nil)
	r.Register(c)

	var got float64
	fams, err := Parse(r.Metrics(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.Name == "dorang_provider_quota_used_percent" {
			got = f.Samples[0].Value
		}
	}
	// 50 of 200 is twenty-five percent, and the name says percent.
	if got != 25 {
		t.Errorf("used_percent = %v for 50 of 200; want 25, not 0.25", got)
	}
}

// TestQuotaUrgencyAbsentWithoutAResetInstant is DESIGN §7.5a(c) correction 1: a
// rolling window has no reset instant, so it scores nothing until a provider
// reports one. A zero would be indistinguishable from a fully consumed
// allowance, which is the opposite reading.
func TestQuotaUrgencyAbsentWithoutAResetInstant(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	rolling, err := quota.NewMeter(quota.MeterConfig{
		Rules: []quota.Rule{{
			Window: quota.Rolling(5 * time.Hour), Metric: quota.MetricRequests,
			Limit: 100, OnExhaust: quota.Cooldown, Resets: true,
		}},
		Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := NewQuotaCollector(clock, 0)
	c.Track("rolling", rolling)
	r := New(nil)
	r.Register(c)
	if hasFamily(t, r.Metrics(nil), "dorang_quota_urgency") {
		t.Error("a rolling window with no provider-reported reset produced an urgency; " +
			"§7.5a(c) correction 1 says it scores nothing until the provider reports one")
	}

	// A calendar window has a boundary dorang itself defines, so it does score.
	daily, err := quota.NewMeter(quota.MeterConfig{
		Rules: []quota.Rule{{
			Window: quota.Daily, Metric: quota.MetricRequests,
			Limit: 100, OnExhaust: quota.Cooldown, Resets: true,
		}},
		Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	c2 := NewQuotaCollector(clock, 0)
	c2.Track("daily", daily)
	r2 := New(nil)
	r2.Register(c2)
	if !hasFamily(t, r2.Metrics(nil), "dorang_quota_urgency") {
		t.Error("a calendar window has a known reset instant and must score")
	}
}

// TestPublishedOvershootIsANumber is DESIGN §5.6: "Every mode publishes its
// maximum possible overshoot as a number. 'Approximately accurate' is not an
// acceptable specification." A figure that lives only in a design document is
// not published.
func TestPublishedOvershootIsANumber(t *testing.T) {
	c := &ClusterCollector{
		Enabled: true,
		Mode:    cluster.ModeLeased,
		Params:  cluster.Params{Limit: 64, Nodes: 4, Block: 16, Clustered: true},
	}
	r := New(nil)
	r.Register(c)
	fams, err := Parse(r.Metrics(nil))
	if err != nil {
		t.Fatal(err)
	}

	published := map[string]float64{}
	refused := map[string]float64{}
	for _, f := range fams {
		switch f.Name {
		case "dorang_coordination_max_overshoot":
			for _, s := range f.Samples {
				published[s.Label("mode")] = s.Value
			}
		case "dorang_coordination_mode_refused":
			for _, s := range f.Samples {
				refused[s.Label("mode")] = s.Value
			}
		}
	}

	// leased: block x (nodes - 1) = 16 x 3.
	if published["leased"] != 48 {
		t.Errorf("leased overshoot = %v, want 48", published["leased"])
	}
	// The shared modes are exact.
	if published["shared-redis"] != 0 || published["shared-pg"] != 0 {
		t.Errorf("a shared mode published a non-zero overshoot: %v", published)
	}
	// `local` under cluster.enabled is a refusal, not a warning: §5.6 says it
	// refuses to start, because silently exceeding a plan limit produces
	// upstream 429s that surface far from their cause.
	if _, ok := published["local"]; ok {
		t.Error("local published an overshoot under cluster.enabled; it must refuse")
	}
	if refused["local"] != 1 {
		t.Errorf("local is not marked refused under cluster.enabled: %v", refused)
	}
}

// TestBudgetRatioAbsentWithoutALimit: there is no ratio to a budget nobody set,
// and 0.0 would say the subject has spent nothing of an unlimited allowance.
func TestBudgetRatioAbsentWithoutALimit(t *testing.T) {
	unlimited := fakeLedger{stats: []cluster.LedgerStat{{
		Key:       cluster.BudgetKey("key", "k1", quota.Rolling(24*time.Hour), time.Now()),
		Limit:     0,
		Committed: 500,
	}}}
	r := New(nil)
	r.Register(NewBudgetCollector(unlimited, nil, 0))
	if hasFamily(t, r.Metrics(nil), "dorang_budget_spent_ratio") {
		t.Error("a spent ratio was published for a subject with no ceiling")
	}
	if !hasFamily(t, r.Metrics(nil), "dorang_budget_spent_nano") {
		t.Error("the spend itself is known and must still be reported")
	}

	limited := fakeLedger{stats: []cluster.LedgerStat{{
		Key:       cluster.BudgetKey("key", "k1", quota.Rolling(24*time.Hour), time.Now()),
		Limit:     1000,
		Committed: 250,
	}}}
	r2 := New(nil)
	r2.Register(NewBudgetCollector(limited, nil, 0))
	fams, err := Parse(r2.Metrics(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.Name == "dorang_budget_spent_ratio" && f.Samples[0].Value != 0.25 {
			t.Errorf("spent ratio = %v, want 0.25", f.Samples[0].Value)
		}
	}
}

// TestStoreSaturationAbsentOnAnUnlimitedPool is rule 3 applied to the pool. An
// unlimited pool has no saturation, and reporting 0.0 would say it is idle at
// the moment it is oversubscribed.
func TestStoreSaturationAbsentOnAnUnlimitedPool(t *testing.T) {
	r := New(nil)
	r.Register(NewStoreCollector(fakeStore{db: sql.OpenDB(deadConnector{})}, "sqlite"))
	body := r.Metrics(nil)
	if hasFamily(t, body, "dorang_store_pool_saturation_ratio") {
		t.Error("a saturation ratio was published against an unlimited pool")
	}
	if hasFamily(t, body, "dorang_store_pool_max_open") {
		t.Error("a ceiling was published where none is configured")
	}
}

// TestRehashMetricsAbsentWhenNotRunning: with rehash-on-use off,
// dorang_auth_rehash_done_total sits at zero forever, which reads as "the
// migration is stuck" rather than "the migration is not running". Those need
// different actions, so they must not look the same.
func TestRehashMetricsAbsentWhenNotRunning(t *testing.T) {
	src := fakeAuth{}
	off := New(nil)
	off.Register(NewAuthCollector(src, false, time.Time{}, nil))
	if hasFamily(t, off.Metrics(nil), "dorang_auth_rehash_pending") {
		t.Error("rehash metrics were published with rehash-on-use off")
	}

	on := New(nil)
	on.Register(NewAuthCollector(src, true, time.Time{}, nil))
	body := on.Metrics(nil)
	if !hasFamily(t, body, "dorang_auth_rehash_pending") {
		t.Error("rehash metrics are missing with rehash-on-use on")
	}
	got := sampleValues(t, body)
	// queued 10, done 6, dropped 1 leaves 3 used keys still on legacy_sha256.
	if got["dorang_auth_rehash_pending"] != 3 {
		t.Errorf("rehash pending = %v, want 3", got["dorang_auth_rehash_pending"])
	}
}

// TestShadowOffEmitsNothingButTheMode: a page of zeroed shadow counters is
// exactly the "clean report" DESIGN §14.1 warns must not be read as evidence.
func TestShadowOffEmitsNothingButTheMode(t *testing.T) {
	r := New(nil)
	r.Register(NewShadowCollector(fakeShadow{st: shadow.Stats{Mode: "off"}}))
	body := string(r.Metrics(nil))
	if !strings.Contains(body, `dorang_shadow_mode{mode="off"} 1`) {
		t.Error("the mode itself must always be reported")
	}
	if strings.Contains(body, "dorang_shadow_clean_total") {
		t.Error("shadow verdict counters were published with shadowing off; zero clean " +
			"comparisons and a stopped gate look identical")
	}
}

// TestMeterDegradedReasonIsAStateSet keeps the degradation reason from
// disappearing when it changes, which would leave a stale series in every
// dashboard that graphed it.
func TestMeterDegradedReasonIsAStateSet(t *testing.T) {
	r := New(nil)
	r.Register(NewMeterCollector(fakeMeter{st: meter.Stats{
		Degraded: true, Reason: meter.ReasonSpoolFull, SpoolDepth: 900,
	}}))
	body := string(r.Metrics(nil))
	if !strings.Contains(body, `dorang_metering_degraded_reason{reason="spool_full"} 1`) {
		t.Errorf("the active reason is not 1:\n%s", body)
	}
	if !strings.Contains(body, `dorang_metering_degraded_reason{reason="trace_queue_full"} 0`) {
		t.Error("the inactive reasons must still be emitted, at 0")
	}
	if !strings.Contains(body, "dorang_meter_spool_depth 900") {
		t.Error("spool depth (DESIGN §12.3) is missing")
	}
}

// TestCapacityFoldSumsRatherThanDrops: an axis whose keys have overflowed still
// has a true total occupancy, and the total is the number that says whether the
// ceiling is being approached.
func TestCapacityFoldSumsRatherThanDrops(t *testing.T) {
	src := newFakeCapacity(10)
	var wantInUse int64
	for _, a := range src.axes {
		if a.Axis == 6 { // AxisKey
			wantInUse += int64(a.InUse)
		}
	}

	r := New(nil)
	r.Register(NewCapacityCollector(src, 2))
	fams, err := Parse(r.Metrics(nil))
	if err != nil {
		t.Fatal(err)
	}
	var got int64
	for _, f := range fams {
		if f.Name != "dorang_capacity_inflight" {
			continue
		}
		for _, s := range f.Samples {
			if s.Label("axis") == "key" {
				got += int64(s.Value)
			}
		}
	}
	if got != wantInUse {
		t.Errorf("folded occupancy sums to %d, want %d; a fold must lose resolution, "+
			"never a unit of occupancy", got, wantInUse)
	}
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeMeter struct{ st meter.Stats }

func (f fakeMeter) Stats() meter.Stats { return f.st }

type fakePrefix struct{}

func (fakePrefix) Stats() (uint64, uint64, uint64, int64) { return 1000, 620, 12, 4096 }
func (fakePrefix) Len() int                               { return 40 }

type fakeLedger struct{ stats []cluster.LedgerStat }

func (f fakeLedger) Stats() []cluster.LedgerStat { return f.stats }
func (fakeLedger) BlockSize() int64              { return 16 }
func (fakeLedger) Draws() int64                  { return 7 }

// fakeStore hands back a real *sql.DB with a real connection limit, opened
// against a connector that never connects. database/sql reports MaxOpenConns
// from its own bookkeeping, so the pool gauges are exercised without a driver.
type fakeStore struct{ db *sql.DB }

func newFakeStore(maxOpen int) fakeStore {
	db := sql.OpenDB(deadConnector{})
	db.SetMaxOpenConns(maxOpen)
	return fakeStore{db: db}
}

func (f fakeStore) DB() *sql.DB          { return f.db }
func (fakeStore) StatementCount() uint64 { return 4242 }

type deadConnector struct{}

func (deadConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("this connector never connects")
}
func (deadConnector) Driver() driver.Driver { return deadDriver{} }

type deadDriver struct{}

func (deadDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("this driver never opens")
}

type fakeAuth struct{}

func (fakeAuth) Stats() auth.Stats {
	return auth.Stats{
		Hits: 900, Misses: 100, StoreCalls: 90, Coalesced: 10, Rejected: 3,
		MasterHits: 1, RehashQueued: 10, RehashDone: 6, RehashDropped: 1,
		SnapshotSize: 120, OverlaySize: 4,
	}
}

type fakeOAuth struct{ now time.Time }

func (f fakeOAuth) Snapshot() []auth.CredentialHealth {
	return []auth.CredentialHealth{
		{
			ID: "oauth-1", Provider: "plan-a", Healthy: true, Refreshes: 12,
			LastRefresh: f.now.Add(-30 * time.Minute),
			ExpiresAt:   f.now.Add(30 * time.Minute),
		},
		{
			ID: "oauth-2", Provider: "plan-a", Healthy: false, Reason: "invalid_grant",
			Failures: 4, NextAttempt: f.now.Add(2 * time.Minute),
			// No ExpiresAt: the store did not record one, so the countdown is
			// absent rather than reported as "already expired".
		},
	}
}

type fakeShadow struct{ st shadow.Stats }

func (f fakeShadow) Stats() shadow.Stats { return f.st }

type fakeBatch struct{}

func (fakeBatch) Stats() batch.Stats {
	return batch.Stats{
		Active: 3, DispatchSlots: 4, SlotsFree: 1, Queued: 2,
		RowsInFlight: 17, RowsStarted: 900, RowsFinished: 883,
	}
}

// everyCollector registers one of each, so Validate covers the whole surface.
func everyCollector(t *testing.T) *Registry {
	t.Helper()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	r := fullRegistry(t)

	qm, err := quota.NewMeter(quota.MeterConfig{
		Rules: []quota.Rule{
			{Window: quota.Rolling(time.Hour), Metric: quota.MetricRequests,
				Limit: 200, OnExhaust: quota.Cooldown},
			{Window: quota.Rolling(24 * time.Hour), Metric: quota.MetricCostUSD,
				Limit: quota.NanoUSD(20), OnExhaust: quota.Cooldown},
			// A resetting calendar window, so the expiring-quota urgency of
			// DESIGN §7.5a(c) has something to score.
			{Window: quota.Daily, Metric: quota.MetricRequests,
				Limit: 5000, OnExhaust: quota.Cooldown, Resets: true},
		},
		Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	qm.Record(now, quota.Usage{Requests: 30, CostNanoUSD: quota.NanoUSD(4)})
	qc := NewQuotaCollector(clock, 0)
	qc.Track("cred-1", qm)

	r.Register(
		NewMeterCollector(fakeMeter{st: meter.Stats{
			Recorded: 1000, TracesRecorded: 100, TracesDropped: 2,
			TracesDroppedQueue: 2, SpoolDepth: 5, SpoolBytes: 900, Shards: 8,
		}}),
		NewPrefixCollector(fakePrefix{}, 1<<20),
		qc,
		NewBudgetCollector(fakeLedger{stats: []cluster.LedgerStat{{
			Key:   cluster.BudgetKey("key", "k1", quota.Rolling(24*time.Hour), now),
			Limit: 1_000_000, Committed: 250_000, Remaining: 40_000,
			ExpiresAt: now.Add(time.Minute),
		}}}, clock, 0),
		&ClusterCollector{
			Enabled: false, NodeID: "node-1", Mode: cluster.ModeLocal,
			Params: cluster.Params{Limit: 64, Nodes: 1},
		},
		NewAuthCollector(fakeAuth{}, true, now.Add(90*24*time.Hour), clock),
		NewOAuthCollector(fakeOAuth{now: now}, clock, 0),
		NewStoreCollector(newFakeStore(10), "sqlite"),
		NewBatchCollector(fakeBatch{}),
		NewShadowCollector(fakeShadow{st: shadow.Stats{
			Mode: "compare", SampleRate: 0.05, Sampled: 50, Queued: 50,
			Sent: 48, Compared: 48, Clean: 47, WithDiffs: 1, Diffs: 2,
			SkippedUnsafe: 3, LimitNanoUSD: 5_000_000_000, SpentNanoUSD: 1_000_000_000,
			QueueCapacity: 256,
		}}),
	)
	return r
}

// TestListCollectorCapCountsItsDrops keeps a capped list from losing series
// silently. A circuit state does not aggregate, so the tail is dropped rather
// than folded into one meaningless series — but the drop is still counted, or
// the operator sees a shrinking fleet instead of an exhausted cap.
func TestListCollectorCapCountsItsDrops(t *testing.T) {
	tr := healthTrackerWith(t, 12)
	c := NewHealthCollector(tr, 4)
	r := New(nil)
	r.Register(c)

	fams, err := Parse(r.Metrics(nil))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var folds float64
	for _, f := range fams {
		switch f.Name {
		case "dorang_deployment_requests_total":
			for _, s := range f.Samples {
				seen[s.Label("deployment")] = true
			}
		case "dorang_metrics_cardinality_folds_total":
			for _, s := range f.Samples {
				if s.Label("family") == "dorang_deployment_health" {
					folds = s.Value
				}
			}
		}
	}
	if len(seen) != 4 {
		t.Errorf("%d deployments past a cap of 4", len(seen))
	}
	if folds != 8 {
		t.Errorf("folds = %v, want 8; a dropped series that is not counted is a "+
			"fleet that appears to have shrunk", folds)
	}
}

func healthTrackerWith(t *testing.T, n int) *health.Tracker {
	t.Helper()
	tr := health.New(health.Options{})
	for i := 0; i < n; i++ {
		tr.Report("dep-"+string(rune('a'+i)), health.Outcome{})
	}
	return tr
}
