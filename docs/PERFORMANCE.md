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

### 6b. dorang exposes no Go runtime metrics

`/metrics` publishes **131 metric families and zero `go_*` or `process_*`
series.** No GC pause, no goroutine count, no heap, no file descriptors. The
Prometheus Go and process collectors are not registered.

So the tail in 6a cannot be diagnosed from outside the process, and neither
could a goroutine leak, a heap climb, or FD exhaustion in production. For a
gateway that documents an allocation-free hot path as a central property, the
runtime evidence for that property is not observable in a running deployment.

These are recorded here rather than in `docs/SECURITY-REVIEW.md` because neither
is a defect in what dorang does — one is an unexplained shape and the other is a
missing view. Both should be closed before any claim in §2 is repeated without
this document attached.
