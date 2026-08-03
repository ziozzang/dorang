# Performance, measured against the gateway it replaces

This file records what was measured, on what, and — the part that decides whether
any of it is worth reading — **which comparisons are controlled and which are
not.** A number without its method is a claim, and this document is meant to be
checkable rather than quotable.

Measured 2026-08-03 on the machine that runs both gateways: 16 cores, both
processes in Docker on the same host, client on the same host. dorang at
`bd18205`/`77b9dc3`, LiteLLM v1.93.0.

---

## 1. The method, and where it stops being fair

Three axes were measured. Only the first two are controlled comparisons.

**Controlled — the gateway path with no upstream.** `GET /v1/models` and a
rejected credential both begin and end inside the gateway: parse, authenticate,
authorize, render. No provider is contacted, so nothing outside either process
enters the number. This isolates exactly the cost the gateway adds, which is the
only cost either gateway controls.

**Controlled — resource use at rest.** Both idle, same host, same instant.

**NOT controlled — end-to-end with a real model.** Both gateways serve a model
called `deepseek-v4-flash`, but LiteLLM's routing topology is encrypted in its
database and was not decrypted, so **there is no evidence the two names resolve
to the same upstream.** Upstream latency is three to four orders of magnitude
larger than gateway overhead, so an end-to-end comparison would mostly report
which provider each gateway happened to pick. No such figure is published here.
The honest end-to-end statement is in §5.

### The client was the first thing measured, by accident

An initial pass with a Python client reported dorang at **0.47 ms p50**. The same
request from a Go client with connection reuse reports **0.07 ms**. The
difference is not the gateway; it is the client's own per-request cost, and at
this scale it dominated the thing being measured by 6×.

That is worth stating rather than quietly discarding, because it is the failure
mode of every gateway benchmark: below a millisecond, an ordinary HTTP client is
not a measuring instrument. Everything below uses the Go generator
(`MaxIdleConnsPerHost` 512, warmed, 3,000–5,000 requests per point).

---

## 2. Gateway-only path — `GET /v1/models`

| concurrency | dorang p50 | LiteLLM p50 | dorang p99 | LiteLLM p99 | dorang req/s | LiteLLM req/s |
|---|---|---|---|---|---|---|
| 1  | **0.07 ms** | 3.10 ms | 0.13 ms | 4.37 ms | **14,201** | 313 |
| 4  | **0.07 ms** | 3.41 ms | 0.21 ms | 5.80 ms | **49,272** | 1,115 |
| 16 | **0.20 ms** | 13.37 ms | 0.69 ms | 20.62 ms | **71,897** | 1,158 |
| 64 | **0.89 ms** | 51.87 ms | 36.30 ms | 62.48 ms | **36,314** | 1,210 |

LiteLLM plateaus near 1,200 req/s from concurrency 4 onward: added concurrency
buys latency, not throughput. dorang peaks at concurrency 16 and **loses
throughput at 64** — see §4, which is the one result here that is not flattering
and is not explained.

## 3. Rejected credential — the path an attacker drives

| | p50 | p90 | p99 | req/s |
|---|---|---|---|---|
| dorang | **0.20 ms** | 0.33 ms | 0.66 ms | **69,670** |
| LiteLLM | 39.49 ms | 50.22 ms | 61.40 ms | 399 |

175× throughput, and the shape matters more than the ratio. **LiteLLM is three
times slower rejecting a credential than serving one** at the same concurrency
(39.49 ms against 13.37 ms), so an unauthenticated caller costs it more than a
paying one. dorang answers a rejection in the same 0.20 ms it answers a
successful listing: refusing is not a more expensive path than serving.

## 4. Resource use at rest

| | CPU | Memory |
|---|---|---|
| dorang | 2.77% | **48.27 MiB** |
| LiteLLM | 4.01% | 4.405 GiB |

91× on resident memory. This is a static-binary-versus-Python-runtime difference
before it is an architectural one, and it is reported as measured rather than as
a design claim.

---

## 5. dorang's own overhead, isolated

From `testing/perf`, which drives a local fake upstream and subtracts every
nanosecond spent waiting on it — the boundary is handler entry to handler
return, so what remains is dorang and nothing else.

```
overhead   n=2000  p50=267.86µs  p90=355.91µs  p95=390.85µs  p99=543.83µs  max=1.117ms
overhead   n=2000  p50=260.17µs  p90=344.88µs  p95=375.84µs  p99=513.23µs  max=943.55µs
```

Against the published DESIGN §15.1 target of p50 280 µs / p99 2 ms: inside both.

Overhead is linear in request body size, which is the honest shape — the request
is parsed, canonicalised and re-encoded:

| body | p50 | p99 |
|---|---|---|
| 256 B | 129 µs | 318 µs |
| 1 KiB | 159 µs | 287 µs |
| 4 KiB | 274 µs | 462 µs |
| 16 KiB | 688 µs | 1.23 ms |
| 256 KiB | 5.08 ms | 6.47 ms |

**The practical statement about real traffic.** A live LLM request spends 1–5
seconds upstream. At a 4 KiB body dorang adds ~274 µs, which is under 0.03% of a
one-second request. The gateway is not where the time goes, and no realistic
change to it would be visible to a caller.

---

## 6. Two things this measurement found that are not fixed

### 6a. A reproducible tail at high concurrency

At concurrency 64, three consecutive runs of 5,000 requests:

```
run1  p50=1.00 ms  p90=2.31 ms  p99=40.60 ms   36,932 req/s
run2  p50=1.18 ms  p90=2.41 ms  p99=42.24 ms   34,100 req/s
run3  p50=1.00 ms  p90=2.10 ms  p99=30.22 ms   41,817 req/s
```

p90 is 2.3 ms and p99 is 30–42 ms — a 15–20× step in the last decile,
reproducible, and throughput is *lower* than at concurrency 16. Sixteen cores
against 64 in-flight requests is 4× oversubscription, so garbage collection or
scheduler latency are the obvious candidates.

**Obvious, and unverified**, because of 6b.

### 6b. The scrape could not explain a tail — corrected, then fixed

> ⚠️ **This section first claimed dorang published no runtime metrics at all,
> and that was wrong.** It was written from a grep for `go_*` and `process_*`,
> which returned nothing because dorang has no `client_golang` dependency and
> spells its own runtime series with the project prefix. `dorang_goroutines`,
> `dorang_memory_heap_bytes`, `dorang_memory_bytes` and `dorang_gc_cycles_total`
> were all being published. The error is left visible rather than edited away,
> because it is the same failure this document warns about in §1: a measurement
> taken with the wrong instrument, believed because it produced a number.

The narrower finding survives, and it is the one that mattered. **Nothing
published could distinguish a tail from a mean.** A count of GC cycles cannot
tell a hundred short pauses from one long one, and time spent *runnable but not
running* — the other candidate at 4× oversubscription — had no series at all.

Now added, in `BuildCollector`:

| series | answers |
|---|---|
| `dorang_gc_pause_seconds` | stop-the-world pause **durations**, not just cycles |
| `dorang_sched_latency_seconds` | time goroutines were runnable but not running |
| `dorang_maxprocs` | what the concurrency is being compared against |
| `dorang_os_threads` | threads created — far above GOMAXPROCS means blocking calls |
| `dorang_open_file_descriptors` | absent where `/proc` is not readable, never zero |

Both histograms deliberately emit **no `_sum`**. The runtime does not publish
one, and it could be approximated from bucket midpoints — which is exactly the
move COMPATIBILITY §11.4 refused for a guessed `Retry-After`: nothing downstream
can tell an approximation from a measurement. `histogram_quantile()` needs only
the buckets and is unaffected; `rate(_sum)/rate(_count)` is unavailable because
the data for it does not exist.

The diagnosis this enabled is in §6c.

---

## 7. Rolling replacement under load, on two nodes

Availability rather than speed, but the same rule applies: it is a measurement
or it is a claim. Configured and verified 2026-08-03 on the live deployment.

**Topology.** Two dorang processes, one PostgreSQL, one config file. Kong in
front as the balancer, with active health checks on `/health` every 5s and an
unhealthy threshold of 2. `cluster.capacity_mode: shared-pg` — dorang refuses to
start on `local` with `cluster.enabled`, because node-local counting on N nodes
counts every ceiling N times over.

**The test.** Continuous `POST /v1/chat/completions` through Kong against a
local model — a real 3.3-second upstream call, not a listing — at concurrency 4,
while each node in turn is replaced with a newly built image.

```
TOTAL 208 requests, 0 failed (0.0000%)   p50=3.318s p99=3.724s max=3.725s
seconds containing a failure: 0
```

Leadership moved each time the leader was replaced, within about a second, and
no request observed it.

**The ledger shows the replacement happening**, which is the point of §7c below:

```
node       n    first      last
(null)   215   18:31:26   18:36:42     <- the old image
dorang-1 120   18:35:42   18:38:02     <- replaced first
dorang-2  59   18:36:47   18:38:02     <- replaced second
```

### 7a. `stop_grace_period` was 10s and the drain needed 45

The first run passed with zero failures and proved nothing, because the drain
never ran. `docker stop` took **10,422 ms and exited 137 — SIGKILL** — against a
configuration asking for `pre_stop_delay: 15s` + `shutdown_grace: 30s`.

Docker's default stop timeout is 10s and neither compose file set one, so every
node was killed mid-drain. That made `shutdown_grace` **inert**: it exists so a
20-second stream can finish, and the container was killed at 10 seconds
regardless. The zero-failure result was luck — Kong had already stopped routing
by then, and nothing long-running was in flight.

Fixed with `stop_grace_period: 60s`. The rule is that it must exceed
`pre_stop_delay + shutdown_grace`, and nothing in dorang can check it, because
it lives in the orchestrator rather than in the config dorang reads.

### 7b. `pre_stop_delay: 0s` is right for one node and wrong behind a balancer

A balancer learns a node is unready by **polling**. Between readiness flipping
false and the poll that notices, it is still routing there, and closing the
listener inside that window is connection-refused at the client — the failure
the whole drain sequence exists to prevent. The delay must exceed
(health-check interval × unhealthy threshold): 5s × 2, so 15s.

### 7c. `request_logs.node_id` existed and nothing ever wrote it

The column was in the schema, `InsertRequestLogs` wrote it and `scanRequestLog`
read it back — **52,766 rows of NULL**. On one node that is a column nobody
misses. On two it is the first question asked when one of a pair misbehaves, and
neither the ledger nor any response header could answer it.

Fixed, and the `(null)` rows in the table above are the ones served before the
fix rolled out — the transition is visible in the data.

### 7d. A shared config file cannot name two nodes

`cluster.node_id` is a literal. Written in a file both nodes read, it gives them
the same id and the election refuses the second as a duplicate; left unset, each
process mints a **random** id. Random is unique — all the election needs — and
useless for "this node has been the slow one all week", because it changes at
every restart.

`cluster.node_id_env` was added, following the `key_env` / `url_env` /
`master_key_env` convention already in the schema, and compose sets
`DORANG_NODE_ID` per service. The variable wins over the literal: the file is
what the fleet shares, the variable is what one node says about itself.

### What this does not yet show

The 3.3-second requests here are non-streaming. A drain that cuts a **stream**
mid-flight is the case `shutdown_grace` was really written for, and it has not
been driven under a rolling replacement. Until it is, §7a is fixed by argument
rather than by observation.
