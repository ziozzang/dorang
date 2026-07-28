# vLLM as a Day-Zero Backend

> Integration specification, derived from source at `vllm-project/vllm` commit `7aea73d`
> (main, 2026-07-28). Every behavioral claim below was read out of that tree, not inferred.
> Cite the commit rather than a release: the checkout is shallow and untagged, so the version
> number is genuinely unknown.
>
> 한국어: [VLLM.ko.md](VLLM.ko.md)

vLLM is not "an OpenAI-compatible backend plus a priority field". It diverges in ways that
are silent — accepted, ignored, and indistinguishable from working. This document exists
because most of the integration risk is in things that return `200`.

---

## 1. The three findings that change dorang's design

### 1.1 The undeclared context window is solvable with one unauthenticated GET

```
GET {base_url}/v1/models  →  data[i].max_model_len
```

The field is a vLLM extension on the model card and carries the **effective, post-override**
value — `--max-model-len 8192` on a 128K model reports `8192`, and even the memory-based
auto-fit result is synced back before serving. No flag is required; the route is registered
unconditionally.

This closes, for every vLLM deployment, the gap §4.3 describes: dorang no longer has to
choose between guessing a context window and leaving it undeclared. It reads the truth.

Two rules that must be coded, not assumed:

- **`null` means undeclared, never zero.** LoRA adapter cards omit the field entirely. They
  carry a `parent` pointing at the base model, whose window they inherit.
- **There is no max-output value to read, and dorang must not synthesize one.** vLLM has no
  such concept — it computes `max_model_len − input_length` per request. Leave it undeclared.
  The exception is `/v1/completions`, where `max_tokens` defaults to **16** and an explicit
  `null` is coerced back to 16, so dorang must send a real value there.

`POST /tokenize` returns `max_model_len` too, as a required field, and is a useful
cross-check — but it costs a tokenization pass and is unnecessary at startup.

### 1.2 `priority` is silently ignored by default, and the field documents itself falsely

Every `priority` field's description says a non-zero value "will raise an error if the served
model does not use priority scheduling." **No such validation exists.** That enforcement
lived in the engine generation that has since been deleted.

The default scheduling policy is `fcfs`, under which the value travels all the way into the
engine's request object and simply has no consumer. A gateway sending `priority: 5` to a
default-configured server gets **200 OK, no warning, and no effect**.

> dorang cannot detect this from the response. The only HTTP surface exposing the policy is
> a development-mode endpoint that must never be enabled in production. So the honest answer
> is **operator declaration**: the vLLM provider config records whether
> `--scheduling-policy priority` was set, and a deployment that declares priority without it
> is surfaced as an **unverified capability** — the same pattern §4.3 uses for reasoning.

**Direction confirmed: lower value is scheduled first.** Three independent confirmations in
source — the queue docstring, the comparison operator feeding a min-heap, and the config
documentation. dorang's existing class map (`realtime: 0, interactive: 2, batch: 10`) is
already correct and needs no change.

Three further constraints:

- **`service_tier` is accepted and has zero consumers.** It must never be used as a priority
  fallback for this backend.
- **`/v1/messages` has no `priority` field at all**, and its request model silently drops
  unknown keys. Anthropic-shaped traffic needing priority must be translated to
  `/v1/chat/completions`.
- **`/v1/responses` mutates priority**: after each built-in-tool round trip it decrements by
  one, so turn 2+ of a tool loop runs a level more urgent than requested. Priority bands must
  be spaced ≥ 2 apart if that endpoint is used with tools.

Priority also does **not** mean preemption. A waiting request cannot evict a running one;
it goes to the head of the queue and waits for blocks to free. Priority governs admission
order and eviction choice, never per-step allocation among running requests.

### 1.3 There is no batch job API, and the earlier ambiguity is now settled

`/v1/batches` **does not exist** — the string appears nowhere in the source tree.

`/v1/chat/completions/batch` **does** exist as a genuinely registered route, so the older
claim was half right. But it is a **synchronous, single-request, N-conversation fan-out**:
`messages` is a list of *conversations*, each becomes an independent engine request, and the
result is one ordinary response whose `choices` are indexed `0..N−1`. No job id, no polling,
no cancellation, no persistence. It rejects `stream`, `tools`, `n > 1`, and beam search.

`run_batch` is an **offline CLI** that builds its own engine and registers no routes. The
only HTTP it can serve is a bare Prometheus scrape port. It is not reachable as a backend.

> **§11.1's decision that the batch scheduler is dorang's own is confirmed correct and should
> not be revisited.** If the fan-out route is later adopted as the optional accelerator §11.1
> allows, note that it collapses N rows into one all-or-nothing HTTP request — which
> conflicts with §11.1's "partial failure is expected", and is the reason it is an
> accelerator rather than the foundation.

---

## 2. Silent divergences that change gateway code

Ordered by how quietly they fail.

| # | Divergence | Consequence |
|---|---|---|
| 2.1 | **`tool_choice: null` alongside `tools` passes validation and then disables tool parsing.** An explicit null skips the auto-injection that would set `"auto"`, then takes a branch that never invokes the parser. | The client receives plain content with no tool calls and **no error**. dorang must normalize a literal `null` to `"auto"`, or omit the key. Never forward it. |
| 2.2 | **Error `code` is an integer HTTP status**, and `type` is a Python exception name (`BadRequestError`, `NotFoundError`). A third shape exists for validation failures. | OpenAI SDKs branch on a string code and vendor-style type. Unmapped, they mis-branch. dorang must normalize — see COMPATIBILITY 7.1. |
| 2.3 | **`finish_reason` can be `abort` or `repetition`.** | Values OpenAI never emits. COMPATIBILITY §4 needs rows for both. |
| 2.4 | **Unknown request fields are never rejected** — they are debug-logged and dropped. | dorang cannot rely on vLLM to validate anything it passes through. Also a live migration hazard: the older `guided_json` family was replaced by a nested object, so a gateway still sending the old names has them silently ignored. |
| 2.5 | **The response reasoning field is `reasoning`, not `reasoning_content`** — though the old name is still accepted on requests. | §10.2's reverse mapping must read the new name. Requires `--reasoning-parser`, or the field is always null. |
| 2.6 | **Non-streaming responses serialize without omitting nulls**, emitting a dozen vLLM-only fields as explicit `null`; streaming uses a different regime that omits them. | Two serialization regimes on one endpoint. dorang must strip vLLM-only fields on the compat egress path, separately for each. |
| 2.7 | **`user` is declared and explicitly ignored**; `suffix` is declared and rejected with a 400; `best_of` is silently dropped. | Three different behaviors for three unsupported fields. |
| 2.8 | **`parallel_tool_calls: false` post-filters the response** rather than constraining generation. | The model still generates multiple calls; vLLM discards all but the first. |
| 2.9 | **`logprob` defaults to `-9999.0`**, a sentinel standing in for `-inf`. | Not valid JSON `-Infinity`, and not OpenAI-equivalent. Do not round-trip it as an ordinary float. |
| 2.10 | **A model mismatch is 404, not 400.** | For dorang this is a *routing* failure signature, not a client error to relay verbatim. |
| 2.11 | **Endpoint plugins are attached last and may shadow core routes**, including `/v1/chat/completions`. | A third-party plugin can legally replace the endpoint dorang is calling. |

### Context-window overflow signature

§7.6's `context_window` fallback needs a 400 signature. There are **three** distinct
wordings, two structurally identical to OpenAI's and one that is not. Match the stable
substring **`"maximum context length"`**, never a full sentence — and prefer the
pre-computed path from §1.1, using the signature only as a backstop.

---

## 3. Load signals

### 3.1 Metrics worth scraping

All names carry `{model_name, engine}`. The `engine` label is the data-parallel rank and is
**missing from vLLM's own documentation** — the doc generator never emits labels.

| Signal | Metric |
|---|---|
| running requests | `vllm:num_requests_running` |
| waiting requests | `vllm:num_requests_waiting_by_reason{reason="capacity"}` |
| KV cache utilization | `vllm:kv_cache_usage_perc` |
| time to first token | `vllm:time_to_first_token_seconds_*` |
| prefix cache | `vllm:prefix_cache_hits_total` / `vllm:prefix_cache_queries_total` |
| preemptions | `vllm:num_preemptions_total` |

Four traps, each of which produces a wrong routing decision rather than an error:

- **`kv_cache_usage_perc` is a fraction 0–1, not a percentage.** The name is wrong; the
  documentation string says "1 means 100 percent usage."
- **Plain `vllm:num_requests_waiting` includes deferred requests**, not just queued ones.
  For queue-depth routing use the `reason="capacity"` series.
- **There is no prefix-cache hit-rate metric.** Compute it from the two counters. Units are
  **tokens**, not requests.
- **`--disable-log-stats` leaves `/metrics` returning 200 with zero `vllm:` series.** dorang
  must distinguish "endpoint reachable, no series" from "idle", because they look identical
  and mean opposite things.

Under Ray, `:` is sanitized to `_` in every name. With multiple API server processes, gauges
report one process's value rather than a sum.

### 3.2 Per-response load headers beat polling

Sending the request header `endpoint-load-metrics-format: JSON` makes vLLM return an
`endpoint-load-metrics` response header carrying KV-cache usage and waiting count. **No
server flag required.** This is strictly better than scraping for `least_busy`, because it is
per-request and current rather than a possibly-stale gauge.

Limits: non-streaming responses only, and only on `/v1/chat/completions` and
`/v1/completions`. Fall back to scraping for streaming traffic. It also logs at INFO on every
request, which is noisy at dorang's throughput targets.

### 3.3 `/load` is a trap — do not use it

It returns `{"server_load": N}` and looks like exactly the signal a least-busy router wants.
Without `--enable-server-load-tracking` it returns **`0` forever**, and an unconfigured `0`
is indistinguishable from a genuinely idle server — while being the *most attractive*
possible value to that router. The failure mode is actively harmful, not merely
uninformative. Use it only after independently confirming the flag.

---

## 4. Prefix caching

Automatic prefix caching is **on by default** for standard dense generative models, and off
for hybrid (Mamba-class) ones. dorang may assume it is on but must not depend on it.

**dorang's byte-boundary prefix chain (§7.4b) should stay exactly as designed.** vLLM hashes
*token ids* at a 16-token default granularity, but:

1. Aligning to that boundary would force tokenization on the hot path — the precise defect
   §7.4b already rejected. That rejection is now positively confirmed rather than merely
   argued.
2. The effective block size can be silently rewritten by the attention backend and is only
   readable through a development-mode endpoint.
3. Alignment would buy nothing anyway. vLLM's own cache lookup is prefix-chained, so any
   byte-identical prefix yields an identical token prefix and therefore identical block
   hashes — regardless of where dorang cut its chunks.

### The tenancy trade-off dorang has not yet decided

`cache_salt` is a per-request field that partitions vLLM's KV cache. It would give dorang's
`(tenant, group, session)` isolation (§7.4a) real force *inside the engine* — but it also
**destroys cross-tenant reuse of shared system prompts**, which is a direct throughput cost.

This should be an explicit per-provider option, **off by default**, not something dorang
decides silently in either direction.

There is no API to query, pin, or prefetch a prefix. The only related endpoint is
destructive, development-gated, and can preempt every running request.

---

## 5. Operator profile

Flags dorang should recommend, and what breaks without each:

| Flag | Without it |
|---|---|
| `--scheduling-policy priority` | `priority` is accepted and ignored (§1.2) |
| `--enable-prompt-tokens-details` | `cached_tokens` is null, so **§8's cost engine prices cached prompt tokens at full rate** |
| `--enable-auto-tool-choice` + `--tool-call-parser` | `tools` yields a 400 |
| `--reasoning-parser` | `reasoning` is always null |
| not `--disable-log-stats` | `/metrics` returns 200 with no series |
| `--enable-server-load-tracking` | only if `/load` is used at all — see §3.3 |
| `--enable-request-id-headers` | `X-Request-Id` is still honored inbound, just not echoed |

`--enable-chunked-prefill` defaults on; disabling it causes head-of-line blocking in the
waiting queue. `--max-num-partial-prefills` **does not exist** — it belonged to the deleted
engine generation and must not appear in dorang's config.

**Development-mode endpoints must never be called against a production backend.** They
include a prefix-cache reset that can preempt every running request. If dorang ever grows a
vLLM admin surface, it belongs behind a separately-enabled operator capability.

---

## 6. Other levers

- **`X-Request-Id` is honored inbound unconditionally** and adopted as the engine request id.
  Always send it — free correlation into vLLM's own logs and traces.
- **`X-data-parallel-rank`** pins a request to a specific data-parallel engine, letting
  dorang's cache affinity target one rather than trusting internal balancing. Malformed
  values are silently ignored, and it is not accepted on `/v1/responses` or pooling endpoints.
- **LoRA admission outranks priority.** If a batch already holds the maximum number of
  distinct adapters, a request for a cold adapter is set aside **regardless of priority** — a
  high-priority request on a cold adapter loses to a low-priority one on a hot adapter. For
  LoRA-serving deployments, adapter affinity must rank *ahead* of priority in §7.3's strategy
  chain.

---

## 7. Unverified

Recorded rather than assumed:

- The release number for this commit — shallow clone, no tags.
- Concrete per-backend block sizes; that subtree was not in the checkout.
- Whether the load header degrades under multiple API server processes. The two code paths
  demonstrably read different metric registries, but the runtime effect was not observed.
- Whether the async scheduler alters priority behavior.

## 8. Where vLLM's own documentation is wrong

Source was preferred throughout; these are the disagreements found.

| Claim | Reality |
|---|---|
| `priority != 0` errors under non-priority scheduling (stated on five identical field descriptions) | No such validation exists |
| `run_batch` supports only chat completions | Its dispatch table covers six endpoint families |
| Metrics carry only `model_name` | An `engine` label is also present |
| `kv_cache_usage_perc` is a percentage | It is a fraction 0–1 |
| Swap-based preemption modes | Recompute only; the swap flag was removed |
