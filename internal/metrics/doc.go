// Package metrics is dorang's Prometheus surface (DESIGN §12.3).
//
// # It pulls; it is never pushed to
//
// Almost every subsystem already keeps its own numbers and already publishes
// them: [capacity.Broker.Snapshot], [health.Tracker.Stats], [prefix.Table.Stats],
// [meter.Meter.Stats], the quota meters, the cluster ledger, [shadow.Shadower.Stats],
// the store's connection pool, the authenticator's cache counters. None of them
// import a metrics library and none of them should: a counter that lives in two
// places drifts, and a package that has to be told about an exposition format
// cannot be tested without one.
//
// So this package is a set of thin adapters over accessors that already existed,
// plus one thing nobody owned — the per-model, per-provider, per-credential
// request counters of §12.3, which are recorded here from the observation point
// internal/server already has.
//
// # Four rules, each of which a real backend got wrong
//
//  1. **Cardinality is the failure mode.** `credential`, `model`, `provider` and
//     `endpoint` are bounded by configuration; a capacity axis key is not, and a
//     per-request id would be fatal. Every keyed family has a cap, folds the tail
//     into [OverflowSentinel] exactly as internal/meter does, and **counts the
//     folds** — see `dorang_metrics_cardinality_folds_total`. An overflow that is
//     not counted is a lie about resolution rather than a loss of it.
//
//  2. **Units are named honestly.** VLLM.md §3.1 records a gauge named
//     `kv_cache_usage_perc` whose value is a 0–1 fraction; its own documentation
//     string says "1 means 100 percent usage". A ratio here ends `_ratio` and is
//     in [0,1], a percentage ends `_percent` and is in [0,100], seconds end
//     `_seconds`, bytes end `_bytes`, and a counter ends `_total`. [Validate]
//     enforces it and a test runs it over the whole scrape.
//
//  3. **Unconfigured is absent, never zero.** VLLM.md §3.3 describes vLLM's
//     `/load`, which returns `{"server_load": 0}` forever when its flag is unset —
//     indistinguishable from a genuinely idle server, and the single most
//     attractive value to a least-busy router. Every metric here that dorang
//     cannot compute is omitted from the scrape: no OAuth credentials means no
//     `dorang_oauth_*`, no prefix table means no `dorang_prefix_hit_ratio`, an
//     unlimited connection pool means no `dorang_store_pool_saturation_ratio`, a
//     deployment with no latency sample means no `dorang_deployment_ttft_seconds`
//     for it. A family whose samples are all omitted emits no HELP or TYPE line
//     either, which is why [Writer.Metric] defers the header until the first
//     sample.
//
//  4. **/metrics is not a denial-of-service surface.** Collection walks live
//     state, so it takes only locks the request path either does not take at all
//     or takes in shared mode: the request tables are read under an RWMutex read
//     lock, which never conflicts with the read lock [Requests.Observe] takes; the
//     server's configuration mutex is never touched (TestSnapshotReadTakesNoLock
//     is the property that must survive); and no collector performs I/O. The one
//     mutex [Registry.Gather] holds exclusively is its own, and nothing on the
//     request path can reach it.
//
// # Buckets
//
// DESIGN §15.1 puts the warm-local p50 at 249 µs and the p99 at 2 ms. A histogram
// whose lowest bucket is 5 ms cannot answer whether that target is met, which is
// the only question it exists to answer, so [DurationBounds] resolves
// microseconds.
//
// The p50 in that sentence has moved three times — 200 µs published and never
// measured, 480 µs when it first was, then 375 µs and 249 µs as the codec got
// faster twice — and the bounds have not moved with it, deliberately. See
// [DurationBounds] for why they still bracket the figure.
package metrics
