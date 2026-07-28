# Wire Compatibility Contract

> What a client actually observes, and therefore what dorang must reproduce byte-for-byte.
> Every row here is a **golden test**, not a note.
>
> Sources: the OpenAI and Anthropic public wire formats, and the observed behavior of the
> widely-deployed OpenAI-compatible Python proxy that most existing clients were built
> against. Where the two disagree, the row says which one dorang follows and why.
>
> 한국어: [COMPATIBILITY.ko.md](COMPATIBILITY.ko.md)

---

## 0. Surface sizing

An audit of a live 505-path proxy deployment against its actually-attached clients found:

| Tier | Paths | Operations | Meaning |
|---|---:|---:|---|
| T0 | 11 | 14 | an attached client breaks immediately |
| T1 | 88 | 123 | a generic SDK call fails |
| T2 | 343 | 419 | control-plane and enterprise features |
| T3 | 63 | 139 | deprecated, UI-internal, or vendor passthrough |

**Reaching "nothing breaks" is 2.2% of the surface. Reaching a defensible full inference
protocol (T0+T1) is 19.6%.** The remaining 80% is control plane. This is why §0.3 of the
design implements inference protocols in full and fills administrative surface over time —
the ratio, not a preference.

### T0 — implement first

`POST /v1/chat/completions` · `POST /chat/completions` · `POST /v1/embeddings` ·
`POST /embeddings` · `GET /v1/models` · `GET /models` ·
`GET /health/liveliness` · `/health/liveness` · `/health/readiness` (GET **and** OPTIONS) ·
`POST /v1/messages` · `POST /v1/messages/count_tokens`

---

## 1. Chat completions — SSE framing

| # | Contract |
|---|---|
| 1.1 | Each chunk is exactly `data: <json>\n\n`. No `event:` line, no `id:` line. |
| 1.2 | The stream terminates with `data: [DONE]\n\n`. |
| 1.3 | A mid-stream error is delivered **in band**: `data: {"error":{…}}\n\n` followed by `data: [DONE]\n\n`. If the first chunk already went out, the HTTP status is already 200 and cannot change — this is exactly the boundary §7.6 of the design refuses to cross with a fallback. |
| 1.4 | An empty upstream stream yields an empty body with **no** `[DONE]`. |
| 1.5 | Raw upstream SSE lines (`data:`, `event:`, `:`) pass through verbatim; only a frame missing its delimiter is re-framed. |

## 2. Chat completions — chunk JSON shape

| # | Contract | Why it bites |
|---|---|---|
| 2.1 | **Absent fields are omitted, never emitted as `null`.** | ⚠️ Corrected framing: this is **not** merely avoiding a Go mistake. Real OpenAI *does* emit `"logprobs":null` and `"finish_reason":null` on chunks; the reference proxy omits them. The two disagree, and dorang follows **the reference proxy**, because that is what the clients in front of us were built against. Choosing the other way is a one-line option, but it must be a choice, not an accident. |
| 2.1a | **The serializer itself is normative.** | A "byte-for-byte" claim is meaningless without saying which serializer. Compact separators (no space after `:` or `,`), raw UTF-8 rather than `\uXXXX` escaping, and **Go's HTML escaping disabled** — left on, Go turns `&&` into `&&`, which no other server does. |
| 2.2 | A plain text chunk is exactly `{id, object, created, model, choices:[{index, delta:{role?,content?}, finish_reason?}]}` — nothing more. | Extra keys are a divergence. |
| 2.3 | `object` is always `"chat.completion.chunk"`. | |
| 2.4 | `id` and `created` are **pinned across every chunk** of one stream. | Regenerating `created` per chunk breaks clients that use it as a stream identity. |
| 2.5 | `model` is **restamped on every chunk** to the client-facing name. | This independently confirms design §7.2: per-frame rewriting is the required behavior, not an optimization to avoid. Revision 1's "patch the first frame by offset" was wrong on this ground alone. |

## 3. Usage

| # | Contract |
|---|---|
| 3.1 | A usage chunk is emitted **only** when `stream_options.include_usage` is exactly `true`. Truthy is not enough. |
| 3.2 | Without `stream_options`, usage is computed but **not** put on the wire. |
| 3.3 | ⚠️ **Divergence from OpenAI.** The reference proxy's usage chunk carries `"choices":[{"index":0,"delta":{}}]`; OpenAI sends `"choices": []`. dorang follows **the reference proxy**, because existing clients were built against it, and exposes `compat.usage_chunk_choices: empty` for callers who want strict OpenAI shape. |
| 3.4 | Some backends send usage *after* `finish_reason`. The accumulator must accept late usage rather than closing on the finish. |
| 3.5 | Usage may carry non-OpenAI extensions — cost, cached-token detail, server tool use. dorang emits its own under `x-dorang-*` headers and mirrors the widely-read usage extension fields for compatibility. |

## 4. `finish_reason`

| # | Contract |
|---|---|
| 4.1 | Backend-native values are normalized through a mapping table covering at least: `end_turn`, `stop_sequence`, `max_tokens`, `tool_use`, `refusal`, `COMPLETE`, `ERROR`, `ERROR_TOXIC`, `eos_token`, `eos`, `STOP`, `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII`, `IMAGE_SAFETY`, `network_error`, `sensitive`, `guardrail_intervened`. |
| 4.2 | An unmapped value becomes `"stop"` **and logs a warning**. It is never passed through raw. |
| 4.2a | ⚠️ **Error-shaped native reasons map to `"stop"`, and that is lossy in a way worth stating.** `ERROR`, `network_error` and friends have no OpenAI equivalent, so the client is told a failed turn ended normally — the same harm 6.4 flags for a filtered turn reported as a normal one. dorang keeps the wire mapping for compatibility and **must** surface the truth out of band: `x-dorang-native-stop-reason` on the response, and the native value in the ledger. A caller that wants to distinguish them has a way; a caller that does not is unaffected. |
| 4.3 | The original value is preserved out-of-band on the **choice** object, adjacent to `finish_reason`, in both streaming and non-streaming form. Note this is the one documented exception to 2.2's "nothing more" — which is scoped to *plain text* chunks. |
| 4.4 | ⚠️ **A terminal chunk is synthesized when the backend never sent one**, defaulting to `"stop"` — **and upgraded to `"tool_calls"` if any tool call was seen in the stream.** A gateway that merely forwards will emit `"stop"` on a tool-call turn and break every agentic client. This is the highest-value single line in this document. |

## 5. Tool-call streaming

| # | Contract |
|---|---|
| 5.1 | `delta.tool_calls[].index` is **required**, non-optional. `id`, `type`, `function` are optional. |
| 5.2 | Inbound assistant messages have `index` **stripped** from `tool_calls` before forwarding, so a client echoing a full assistant message is not rejected. |
| 5.3 | Tool names are limited to 64 characters on the OpenAI side. Truncation must be recorded in a mapping and **round-tripped**, or the model's tool call cannot be matched back. **Plain truncation is not sufficient**: qualified tool names routinely share a long common prefix, so cutting at 64 collides and two different tools become one. The shortened form is `prefix + "_" + 8 hex of a hash of the full name`, with the mapping authoritative for restoring it. |
| 5.5 | ⚠️ **`max_tokens` and `max_completion_tokens` are not interchangeable, and picking wrong breaks a T0 path.** Several widely deployed OpenAI-compatible servers accept only the former; current reasoning models on the vendor surface reject it in favour of the latter. There is no value that works everywhere, so the field is **per-deployment configuration**, defaulting to `max_tokens` for the compatible-server majority. See DESIGN §10.7, where this is one of the three named cross-protocol traps. |
| 5.4 | Cross-protocol tool-use ids must be normalized consistently in both directions. |

## 6. `/v1/messages` — the highest-risk surface

Anthropic-shaped requests very often target OpenAI-shaped backends, so this is an adapter,
not a passthrough.

| # | Contract |
|---|---|
| 6.1 | SSE framing is `event: <type>\ndata: <json>\n\n` — **both lines**, unlike chat completions. |
| 6.2 | Event types: `message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`. **No `ping`. No `[DONE]`.** `message_stop` exactly once. |
| 6.3 | Content block types on the adapter path: `text`, `tool_use`, `thinking`. |
| 6.4 | ⚠️ `stop_reason` collapses to three values: `stop→end_turn`, `length→max_tokens`, `tool_calls→tool_use`, **everything else → `end_turn`**. So `content_filter` is silently reported as a normal turn end. dorang reproduces this for compatibility **and** reports the true reason in `x-dorang-native-stop-reason`, so the information exists without breaking clients. |
| 6.5 | `stop_sequence` is `null` on the adapter path. |
| 6.6 | Content-block indexing is **stateful**: a held `message_delta`, a chunk queue, and synthesized `content_block_stop` → `content_block_start` pairs on block transitions. A `content_block_stop` must always precede the terminal `message_delta`. This is the most intricate state machine in the whole gateway and gets its own fuzz target. |
| 6.7 | Prompt caching: `message_start` seeds cache counters at 0; the final `message_delta` fills real values; cache fields appear **only when greater than zero**; `input_tokens = prompt − cache_read − cache_creation`, clamped at zero. Cache-read falls back to the OpenAI-style cached-token field when the backend uses that convention. |
| 6.8 | ⚠️ Non-streaming responses include a **non-spec `usage.total_tokens`** while streaming responses omit it. The two shapes differ by one field. dorang reproduces this asymmetry by default under `compat.anthropic_total_tokens: true`. |
| 6.9 | `/v1/messages/count_tokens` returns exactly `{"input_tokens": <number>}` and accepts `?beta=true`. |

## 7. Cross-cutting

| # | Contract |
|---|---|
| 7.1 | Error envelope: `{"error":{"message":str,"type":str,"param":str\|null,"code":"<string>"}}`. **`code` is a string, not a number.** |
| 7.2 | `"no healthy deployment"` conditions map to **429**, not 503. Tag-routing misses map to **401**. |
| 7.3 | **Six authentication header names** are accepted and any one authenticates: `Authorization: Bearer`, `API-Key`, `x-api-key`, `x-goog-api-key`, `Ocp-Apim-Subscription-Key`, and a proxy-specific header. dorang accepts all six and **strips every one of them before forwarding upstream.** |
| 7.4 | `GET /v1/models` items are `{"id","object":"model","created":<constant>,"owned_by"}`. `created` is a **fixed constant**, not the current time — clients cache on it. The list is filtered by the calling key's model allow-list. |
| 7.5 | ⚠️ **Route matching is specificity-ordered.** `/openai/deployments/{model}/chat/completions` must match before `/openai/{endpoint...}`. A naive prefix router silently swallows the specific route into the catch-all. |
| 7.6 | Unsupported parameters are **dropped silently by default**, not rejected. A gateway that forwards everything verbatim surfaces upstream 400s that clients have never seen. dorang defaults `drop_unsupported: true` for this reason. |
| 7.7 | Response headers are read by tooling — a call id and a model/deployment id in particular. dorang emits `x-dorang-request-id` and `x-dorang-deployment`, and mirrors the widely-read legacy header names when `compat.legacy_headers` is on. |
| 7.8 | An inbound call-id header, if the client sets one, is honored as the request id. |

## 8. Routing behavior is part of compatibility

Reliability regressions are indistinguishable from protocol breakage to a user. The defaults
a comparable deployment runs with, and which dorang must be able to express:

```
strategy: least_busy
num_retries: 2
cooldown: 5s          after 3 consecutive failures
timeout: 6000s
```

Cooldown is the load-bearing part: with several deployments per model name, a backend that
starts failing must stop being selected. A round-robin without cooldown keeps sending it
half the traffic and looks like a gateway bug.

## 9. Deliberately not implemented

| Area | Reason |
|---|---|
| Vendor passthrough catch-alls | In the audited deployment, **no** provider credential existed for any of them and passthrough was disabled on every deployment. dorang ships the generic engine (design §10.6) so they can be enabled by configuration, but nothing is enabled by default and none is on the critical path. |
| Assistants / Threads | Superseded by the Responses API. |
| Admin-UI internals, branding, static assets | Not a protocol. |

Anything unimplemented answers `501` with a machine-readable reason. Never a silent `404`.

## 10. Verification status

Contracts above were read from source and schema, not inferred. Two items remain
**unverified** and are marked as such in the test suite rather than assumed:

- `n > 1` streaming semantics — no explicit handling was found in the reference
  implementation; treat multi-choice streaming as unspecified and do not over-invest.
- Per-backend `logprobs` fidelity.
