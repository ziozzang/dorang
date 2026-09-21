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
| 2.0 | ⚠️ **Field matching is case-SENSITIVE, and this is a security property, not a style choice.** | Go's `encoding/json` matches struct tags case-insensitively, so a struct decode fills a `model` field from a key spelled `Model` or `moDel`. A hand-written scanner — and every Python backend downstream — does not. If the authorization gate scans and the adapter unmarshals, a request carrying `{"Model":"expensive"}` is **authorized as having no model and dispatched as having one**: the allow-list check (§7.4) never sees it. Every decode path must therefore be case-sensitive, and a differential test must assert the gate and the adapter resolve the same field from the same bytes. |
| 2.1a | **The serializer itself is normative.** | A "byte-for-byte" claim is meaningless without saying which serializer. Compact separators (no space after `:` or `,`), raw UTF-8 rather than `\uXXXX` escaping, and **Go's HTML escaping disabled** — left on, Go turns `&&` into `\u0026\u0026`, which no other server does. ⚠️ **It held everywhere except the field a caller actually controls.** `SetEscapeHTML(false)` is set on the package encoder, so every plain struct field was correct — and every test that checked was checking one of those. A **string-or-array** type carries its own `MarshalJSON`, and the three that do — `openai.Content`, `openai.StopSequences`, `anthropic.BlockList` — called `encoding/json.Marshal`, whose escaping is on by default. So a user message reading `a && b <tag>` went upstream as `a \u0026\u0026 b \u003ctag\u003e` while the `name` beside it went as itself, and a client doing the substring match this row exists for did not find its own prompt. The rule applies to **every** serializer a body passes through, not to the outermost one. `TestNoHTMLEscapingOnTheRequestPath` pins the string form, the array form and `stop` together. |
| 2.2 | A plain text chunk is exactly `{id, object, created, model, choices:[{index, delta:{role?,content?}, finish_reason?}]}` — nothing more. | Extra keys are a divergence. |
| 2.3 | `object` is always `"chat.completion.chunk"`. | |
| 2.4 | `id` and `created` are **pinned across every chunk** of one stream. | Regenerating `created` per chunk breaks clients that use it as a stream identity. |
| 2.5 | `model` is **restamped on every chunk** to the client-facing name. | This independently confirms design §7.2: per-frame rewriting is the required behavior, not an optimization to avoid. Revision 1's "patch the first frame by offset" was wrong on this ground alone. |
| 2.6 | **The first delta of each choice carries `role: "assistant"`.** | It is DESIGN §10.7's stream-open event, and it is what `message_start` maps to when a Messages stream is converted. Reproduced against a real provider: the byte relay carried the role because it carries everything, and the CONVERTED stream dropped it — so two clients of one gateway saw structurally different streams depending only on which family their deployment's upstream spoke. It rides on the first delta rather than as a frame of its own, so no stream gains a frame it did not have, and a role the source already stated is never overwritten. |
| 2.7 | **`created` is a real timestamp on every path.** | The stream writer stamped one and the non-streaming converter did not, so a Messages answer rendered as a chat completion carried `"created":0` — 1970 — unless the caller passed `stream:true`. The value is the upstream's own where its family has the member, and the gateway's clock where it does not. See DESIGN §10.7's response-identity rule, which covers `id` in the same breath. |

## 3. Usage

| # | Contract |
|---|---|
| 3.1 | A usage chunk is emitted **only** when `stream_options.include_usage` is exactly `true`. Truthy is not enough. |
| 3.2 | Without `stream_options`, usage is computed but **not** put on the wire. |
| 3.3 | ⚠️ **Divergence from OpenAI.** The reference proxy's usage chunk carries `"choices":[{"index":0,"delta":{}}]`; OpenAI sends `"choices": []`. dorang follows **the reference proxy**, because existing clients were built against it. `compat.usage_chunk_choices` selects between them and **both values are served**: `stub` is the default, `empty` emits strict OpenAI's `[]`. It used to be refused at `empty` — the encoder had emitted both shapes since it was written and no configuration could reach it — and the value now travels on `backend.Call`. Selecting `empty` takes a same-family stream off the byte-relay fast path, because on that path the usage chunk is the upstream's own bytes and dorang does not choose its shape. CONFIG §21a. |
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
| 5.5 | ⚠️ **`max_tokens` and `max_completion_tokens` are not interchangeable, and picking wrong breaks a T0 path.** Several widely deployed OpenAI-compatible servers accept only the former; current reasoning models on the vendor surface reject it in favour of the latter. There is no value that works everywhere, so the field is **per-deployment configuration** — `models[].deployments[].max_tokens_field`, with `providers[].params.max_tokens_field` as the endpoint-wide default — defaulting to `max_tokens` for the compatible-server majority. **Per DEPLOYMENT and not per provider**, because `api.openai.com` is one `base_url` that takes `max_tokens` for a chat model and refuses it for a reasoning one. ⚠️ **Not every mismatch is a `400`.** A measured token-plan endpoint accepted `max_tokens: 16`, discarded it and substituted a far larger ceiling — 116 completion tokens billed against a request for 16, with `finish_reason: length` reporting the caller's own limit as honoured. On a metered plan the silent case is the expensive one, and nothing in the response distinguishes it from the correct one; the only signal is `usage.completion_tokens` exceeding what was asked for. This spelling had both constants and a working consumer in `openai.EncodeOptions` and **no configuration could select it** until the key existed, so the promise in this row was true of the code and false of every deployment — CONFIG §23.1b's direction, and the reason that section exists. See DESIGN §10.7, where this is one of the three named cross-protocol traps. |
| 5.4 | Cross-protocol tool-use ids must be normalized consistently in both directions. |

## 6. `/v1/messages` — the highest-risk surface

Anthropic-shaped requests very often target OpenAI-shaped backends, so this is an adapter,
not a passthrough.

| # | Contract |
|---|---|
| 6.1 | SSE framing is `event: <type>\ndata: <json>\n\n` — **both lines**, unlike chat completions. |
| 6.2 | Event types: `message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`, and **`error`**. **No `ping`. No `[DONE]`.** `message_stop` exactly once — and *not* emitted after an `error`, which terminates the stream by itself. An earlier draft omitted `error`, but 1.3 mandates in-band mid-stream errors and once the status is 200 there is no other channel. |
| 6.3 | Content block types on the adapter path: `text`, `tool_use`, `thinking`. |
| 6.4 | ⚠️ `stop_reason` collapses to three values: `stop→end_turn`, `length→max_tokens`, `tool_calls→tool_use`, **everything else → `end_turn`**. So `content_filter` is silently reported as a normal turn end. dorang reproduces this for compatibility **and** reports the true reason out of band — but **only a non-streaming response can carry it in `x-dorang-native-stop-reason`**. On a stream the true reason is known at the terminal `message_delta`, long after headers are sent, so it goes to the ledger and is retrieved by `x-dorang-request-id` (DESIGN §10.4). An earlier draft prescribed the header for both, which is unimplementable for half this endpoint's traffic. |
| 6.5 | `stop_sequence` is `null` on the adapter path. |
| 6.6 | Content-block indexing is **stateful**: a held `message_delta`, a chunk queue, and synthesized `content_block_stop` → `content_block_start` pairs on block transitions. A `content_block_stop` must always precede the terminal `message_delta`. This is the most intricate state machine in the whole gateway and gets its own fuzz target. |
| 6.7 | Prompt caching: the final `message_delta` carries the real values, and cache fields appear **only when `> 0`** — so `message_start` omits them rather than zero-seeding. An earlier draft said both "seeds at 0" and "only when > 0", which cannot both be literal. The vendor itself emits explicit zeros there, so a golden captured from the vendor will not match one captured from a proxy. `input_tokens = prompt − cache_read − cache_creation`, clamped at zero — which only type-checks if the canonical count is **inclusive** (DESIGN §10.7). |
| 6.8 | ⚠️ **Some** reference-proxy builds add a non-spec `usage.total_tokens` to non-streaming `/v1/messages` answers while streaming answers omit it, so the two shapes differ by one field. `compat.anthropic_total_tokens` selects which shape dorang serves and **both values are served**: `true` is the default and adds the member, `false` is the strict vendor shape with no `total_tokens` anywhere. It used to be refused at `false`. The switch governs the NON-STREAMING half only — a streamed message omits the member at either setting. **This is not universal, and the default is not a claim that it is:** a deployment measured in 2026-07 emitted `{"input_tokens":68,"output_tokens":8}` with no `total_tokens` at all, so byte-parity with *that* incumbent wants `anthropic_total_tokens: false`. Check the incumbent before assuming the default matches it; the member is additive and every SDK in the family tolerates one it does not model, which is why the default errs toward emitting. CONFIG §21a. |
| 6.9 | `/v1/messages/count_tokens` returns exactly `{"input_tokens": <number>}` and accepts `?beta=true`. |

## 6a. `/v1/responses` — the streamed event tree

| # | Contract |
|---|---|
| 6a.1 | SSE framing is `event: <type>\ndata: <json>\n\n` — **both lines**, like `/v1/messages` and unlike chat completions. `sequence_number` is on every event, starts at **0**, and climbs by exactly one, failure frames included. No `[DONE]`. |
| 6a.2 | Emitted set: `response.created`, `output_item.added`, `content_part.added`, `output_text.delta`, `reasoning_summary_part.added`, `reasoning_summary_text.delta`, `refusal.delta`, `function_call_arguments.delta`, the part/item `.done`s, and the terminal `response.completed` / `response.failed`. **Not** emitted: `response.in_progress` and the per-delta `.done` echoes. The part lifecycle is emitted even though dorang's own decoder ignores it: the reference client (codex) keys a text delta on an open part. `response.completed` exactly once, and **never after** a `response.failed`. |
| 6a.3 | Identity is pinned across the stream: the id is dorang's own `resp_…`, `created_at` fixed at stream start, and the **client-facing** model on every response object. The served model and every upstream id never appear. |
| 6a.4 | Item lifecycle: `output_item.added` opens an item at the next `output_index`; deltas name their `item_id`; `output_item.done` carries the **complete** item — assembled text, reasoning summary, or a `function_call` with its settled `call_id`, merged name and whole arguments — exactly once. Item ids (`msg_0`, `rs_1`, `fc_2`) are dorang-minted and stream-scoped; `call_id` is the model's own, never invented. |
| 6a.5 | Usage appears only inside the terminal's response object. The terminal is **held** until the stop and any late usage arrive (the §3.4 rule, this family's spelling), and it is rendered by the same encoder as a buffered answer — a streamed and a non-streamed request for one exchange differ by transport only (DESIGN §10.7), and a test pins the two byte-equal. |
| 6a.6 | A truncated upstream is never dressed as completion: no `response.completed` is synthesized, the terminal object never reaches the store, and what the client sees is the failure frame (§11.1a) or nothing at all when nothing was written. |
| 6a.7 | Server-side state: a streamed exchange is stored from the **terminal event's own response object**, after the last byte and before the handler returns — so `previous_response_id` never observes a gap, but the buffered rule "stored before the client has the id" cannot hold (the `created` event carries it). A store failure is logged, not failed to the client: the answer is served, and the id 404s later in the TTL-expired shape. `store:false` skips the store; a store-keeping stream against a deployment with no store is refused **at decode**, before a byte is written, in the buffered path's named 501. |
| 6a.8 | `event: dorang.usage` (opt-in `x-dorang-usage-events`) lands **after** `response.completed`; this family's terminator is load-bearing, so a dorang-aware client reads the frame after the turn. |

## 7. Cross-cutting

| # | Contract |
|---|---|
| 7.1 | Error envelope: `{"error":{"message":str,"type":str,"param":str\|null,"code":"<string>"}}`. **`code` is a string, not a number.** |
| 7.2 | `"no healthy deployment"` conditions map to **429**, not 503. Tag-routing misses map to **401**. |
| 7.3 | **Six authentication header names** are accepted and any one authenticates: `Authorization: Bearer`, `API-Key`, `x-api-key`, `x-goog-api-key`, `Ocp-Apim-Subscription-Key`, and a proxy-specific header. dorang accepts all six and **strips every one of them before forwarding upstream.** |
| 7.4 | `GET /v1/models` items are `{"id","object":"model","created":<constant>,"owned_by"}`. `created` is a **fixed constant**, not the current time — clients cache on it. The list is filtered by the calling key's model allow-list. |
| 7.5 | ⚠️ **Route matching is specificity-ordered.** `/openai/deployments/{model}/chat/completions` must match before `/openai/{endpoint...}`. A naive prefix router silently swallows the specific route into the catch-all. |
| 7.6 | Unsupported parameters are **dropped silently by default**, not rejected. A gateway that forwards everything verbatim surfaces upstream 400s that clients have never seen. dorang defaults `drop_unsupported: true` for this reason. **The rule is about knobs, and it stops where a parameter states something about the ANSWER** — see §7.9, whose four rows are refused with a `400` naming the construct rather than dropped. Dropping `top_k` costs a caller nothing; dropping their `stop` sequence bills them for text they excluded. |
| 7.7 | Response headers are read by tooling — a call id and a model/deployment id in particular. dorang emits `x-dorang-request-id` and `x-dorang-deployment`, and mirrors the legacy header names when `compat.legacy_headers` is on. The mirrored set and the names dorang deliberately does **not** mirror are §7.7a; the flag is off by default. |
| 7.8 | An inbound call-id header, if the client sets one, is honored as the request id. |
| 7.9 | The **§10.1 construct table** — which losses are refused and which are dropped, and why each one is on the side it is on. Versioned, and every row has a conversion test in both directions. Below. |

### 7.7a The legacy header mirror, name by name

`compat.legacy_headers: true` adds the reference proxy's spellings **alongside** dorang's own;
nothing is renamed and nothing is removed. It is off by default, because these are another
vendor's names and a gateway that emits them unasked is claiming to be that vendor.

The reason it exists is that the failure mode is silent. A cost exporter reading
`x-litellm-response-cost` does not error when the header stops arriving — it reports zero, and
so does the dashboard built on it. §0.3's "run alongside, then take over" is not a migration
anyone can perform if taking over quietly zeroes the numbers.

| Legacy name | dorang header it mirrors | Always on? |
|---|---|---|
| `x-litellm-call-id` | `x-dorang-request-id` | yes |
| `x-dorang-real-model` | `x-dorang-upstream-model` | yes |
| `x-litellm-model-id` | `x-dorang-deployment` | yes |
| `x-litellm-response-cost` | `x-dorang-cost-usd`, **and `0` when nothing is priced** | on every non-streamed answer; **absent on a stream** — see the pair table below |
| `x-litellm-key-spend` | `x-dorang-spend-usd` | detail only (§10.4), and only once the spend has been looked up |
| `x-litellm-key-max-budget` | `x-dorang-budget-usd` | detail only |
| `x-litellm-attempted-retries` | `x-dorang-attempt`, **less one** | detail only |
| `x-litellm-response-duration-ms` | `x-dorang-latency-ms` | detail only |

Each mirror is stamped beside the header it copies, so it inherits the same §10.4 detail gate:
a mirror arriving when its source did not would be a second, disagreeing answer to "what is
always on". `attempted-retries` is deliberately not a copy — dorang counts attempts from 1 and
the reference proxy counts retries from 0, so copying the number across would report one retry
for every request that never retried.

> The cost row read **"yes, always"** while the pair table three paragraphs below said the same
> header is absent on a stream, and the test named there asserts the absence. Both could not be
> true, and the one that was wrong was the one an exporter reads first. The column now says what
> the table says. The `key-spend` row moved for a related reason and is stated once here: the
> spend is read from the budget hold, a request that reserves nothing never hydrates one, and a
> figure nobody looked up is absent rather than `0` — the same rule this whole section is about.

**`x-litellm-response-cost` is the one mirror that is not a straight copy, and it is on
purpose.** `x-dorang-cost-usd` is *absent* when no price rule matched, because dorang's own
vocabulary distinguishes "not priced" from "free". The legacy name has no such distinction —
the proxy it belongs to always sends it — so a mirror that vanished would reproduce the exact
failure this section exists to prevent, silently, for every model without a price rule. That
was the shipped behaviour: a deployment with no `pricing:` block emitted the call id, the model
id, the retry count and the duration, and no cost header at all. **Read the pair:**

| `x-litellm-response-cost` | `x-dorang-cost-usd` | means |
|---|---|---|
| `0` | absent | no price rule matched this model — the number is unknown, not zero |
| `0` | `0` | priced, and this request came to nothing |
| *n* | *n* | priced at *n* |
| absent | absent | **the answer streamed.** The cost is not decidable when the headers are written; read the §10.4 usage event, or the ledger by `x-dorang-request-id` |

`TestUnpricedRequestStillMirrorsTheCostHeaderAsZero`,
`TestPricedRequestMirrorsTheCostHeaderExactly`,
`TestStreamedRequestDoesNotPublishACostOfZero` and
`TestTheDiscriminatorStillDistinguishesUnpricedFromPriced` pin the four rows together, so
that fixing one cannot quietly collapse another into it.

**The fourth row is the correction to the first three.** Headers precede usage on a stream:
the response headers go out before the first frame and the request is settled after the last
one, so nothing has computed a cost when this header is written. Making the mirror
unconditional therefore published `0` on every streamed request while the ledger row for the
same request carried a real figure — and by the first row of the table above, a reader is
entitled to interpret that pair as "this model has no price rule". A cost exporter read zero
for every streamed request, and in an agent deployment every turn streams.

The version before that emitted nothing at all, which was a different failure but a smaller
one: **absent claims nothing, zero claims a measurement.** So the mirror is written whenever
the cost is known and whenever it is knowably absent, and omitted only in the one case where
it is not yet decidable. The number is still available for that case, on the channel §10.4
already has for exactly this — the opt-in `event: dorang.usage` frame carries `cost_usd`, and
it is emitted after settlement — and in the ledger, joined by `x-dorang-request-id`, which is
a response header and always present.

An exporter that must have a cost for every streamed turn should read the ledger, not the
header. There is no arrangement of response headers that can carry a number the response does
not yet know.

**Names dorang does not mirror, and why.** A header emitted with an invented value is worse
than an absent one: the reader cannot tell it apart from a measurement.

The list below is longer than it was, and the reason is worth stating: a live deployment of the
reference proxy was measured emitting **17 distinct `x-litellm-*` names** across 40 request
shapes, against the 8 modelled above. Four of the unmodelled ones are cost-related and appeared
on 17 of 20 successful responses, while `x-litellm-response-cost` itself appeared on 1 of 20 —
so a cost exporter pointed at *that* deployment is more likely to be reading a name dorang does
not model than the one it does. Naming them is not a promise to emit them; it is the difference
between an operator discovering the gap during a cutover and discovering it in a reconciliation
three weeks later.

| Legacy name | Why not |
|---|---|
| `x-litellm-version` | dorang is not that proxy. Any value here is a claim to be a version of something else. |
| `x-litellm-model-api-base` | The upstream's URL is deployment topology, not a tenant's business. It is not on a dorang header either. |
| `x-litellm-model-region` | dorang has no region concept on a deployment. |
| `x-litellm-attempted-fallbacks`, `x-litellm-max-fallbacks` | dorang records which model a fallback came *from* (`x-dorang-fallback-from`), not a count and not a configured ceiling; the attempt counter does not separate retries from fallbacks. |
| `x-litellm-key-rpm-limit`, `x-litellm-key-tpm-limit` | dorang publishes the same facts in the standard-form `x-ratelimit-limit-requests` / `-remaining-requests` / `-reset-requests` and the `-tokens` trio, which more clients already read. ⚠️ **This justification was false when it was first written and is true now.** Nothing filled the value `internal/server` renders those four names from, so for a time dorang published neither the legacy pair nor the standard one, and a reader migrating a rate-limit dashboard would have planned around a header that had never arrived. The producer is `internal/app`'s `fillQuotaResult`, reading the chosen credential's own quota view at selection time. |
| `x-litellm-overhead-duration-ms` | `x-dorang-queue-ms` is a capacity wait, not proxy overhead. They are near enough to be confused and not near enough to be equal. |
| `x-litellm-timeout`, `x-litellm-applied-guardrails` | No dorang equivalent is computed per request. |
| `x-litellm-response-cost-original` | The reference proxy reports a pre-adjustment cost beside the adjusted one. dorang's pricing applies its adjustment rules inside `Settle` and publishes one figure; §8.5's second figure is `x-dorang-notional-usd`, which is the **list-rate** equivalent and a different quantity — a discount is not a list price. Mirroring one onto the other would put a number under a name that does not mean it. |
| `x-litellm-margin-amount`, `x-litellm-margin-percent`, `x-litellm-discount-amount` | dorang has no margin or discount model on a request. Adjustment rules (§8.4) can add or subtract, but they are not classified into margin and discount, so there is no per-request value to put here and any split would be invented. An operator who needs these must derive them from `pricing:` — which is where dorang keeps the rule, rather than restating a derived number on every response. |

The measured deployment emitted a handful of further names that are not enumerated here,
because the observation was of one deployment's responses and not of that proxy's contract:
listing a name dorang saw once would read as a commitment about a surface dorang does not own.
**The claim this section makes is bounded accordingly** — it states what dorang mirrors and why
each named omission is an omission, not that the mirrored set is complete against any
particular build of the reference proxy. An operator whose tooling reads a `x-litellm-*` name
absent from both tables above should expect it not to arrive.

### 7.9 The §10.1 construct table — what is refused, what is dropped

DESIGN §10.1 splits conversion loss in two. A **droppable** loss is a knob the target does
not have: dorang omits it, lists it in `x-dorang-dropped-params`, and the request still means
what it meant. A **refused** loss answers `400` with `code: unsupported_construct` naming the
construct, unless the caller listed that construct in `x-dorang-allow-lossy` — in which case
the request is served and the construct is named in `x-dorang-downgraded`, which reports what
this request lost rather than what the caller once agreed might be lost.

The test is one question, and it is not "can the target represent it":

> **Does the absence change what the caller RECEIVES or is CHARGED, or only which knobs
> dorang applied on the way?**

Refused losses come in two reporting forms. A **located** one has an instance to point at and
the detail carries it (`messages[2].content[1]: application/pdf`). A **material** one is a
request parameter — there is one `service_tier` and it means one thing — so the detail carries
the value the caller wrote, and the `400` body can fill `param` with the field name, which the
located half has no equivalent of.

**Refused — located.** No wire parameter; the construct is a shape.

| Construct | What its absence destroys |
|---|---|
| `multi_block_content` | the array form of a message's content |
| `image_block` | the image |
| `document_block` | the document entirely |
| `cache_breakpoints` | the caching topology, **and therefore the bill** |
| `thinking_block` | the reasoning block, and any integrity material on it |
| `multi_block_tool_result` | the non-text blocks of a tool result |
| `structured_system` | per-block attributes on the system prompt |
| `rich_stop_reason` | *which* terminal condition occurred |
| `tool_calls` | the ability to call a tool at all |
| `json_schema` | enforcement of the response schema |

**Refused — material.** The construct id **is** the wire parameter.

| Construct | Parameter | What its absence changes | Not raised when |
|---|---|---|---|
| `stop` | `stop` / `stop_sequences` | **when generation ends.** The text runs past the terminator the caller stated, the caller is billed for tokens they excluded, and a client that splits on the sequence never fires. Nothing in the response says the terminator was not applied. | the list is empty |
| `n` | `n` | **how many choices come back.** `n: 4` answered with one choice makes `choices[3]` an index error inside the client rather than an error from the gateway. | `n: 1` — the default written down |
| `logprobs` | `logprobs`, `top_logprobs` | **a member of the response that was asked for.** The body comes back without the thing it was requested for, with a `200` to say it went well. | neither field is set |
| `service_tier` | `service_tier` | **the price band.** The request runs in a band the caller did not select and is billed for it — the same reason `cache_breakpoints` has always been refused. | `auto`, case-insensitively: it delegates the band to the provider, which is exactly what omitting the field does |

**Dropped and named.** Each of these was weighed against the rule above and stays droppable;
the reasoning is the load-bearing part, because "it changes the answer" proves too much — every
sampling knob changes the answer — and a caller trained to set `x-dorang-allow-lossy` on
everything has been given a mechanism that reports nothing.

| Construct | Parameter(s) | Why the answer is materially the same |
|---|---|---|
| `logit_bias` | `logit_bias` | A **prior over sampling**. No vendor promises a distribution — the same request returns different text on every call — so there is no postcondition for its absence to violate, and the result is indistinguishable from an ordinary re-draw of the request that carried it. Refusing here would commit dorang to refusing `top_k` and `frequency_penalty` by the identical argument. |
| `top_k` | `top_k` | Same class, and it is the everyday case: `top_k` is Messages-native and absent from chat-completions, so refusing it would refuse a conversion that works today. |
| `penalties` | `frequency_penalty`, `presence_penalty` | Same class. |
| `seed` | `seed` | Reproducibility is **vendor-documented as best effort**; the same seed does not promise the same bytes, so a dropped seed removes a preference and not a guarantee. |
| `parallel_tool_calls` | `parallel_tool_calls` | A target that cannot express it **does not make parallel tool calls**, so `false` is already satisfied there and `true` is a permission rather than a requirement. The constraint holds by default. |
| `reasoning` | `reasoning` | DESIGN §10.2 decides this one normatively: an unverified reasoning capability is **omitted and reported**, never guessed. |
| `priority` | `priority` | DESIGN §10.5 makes ignoring a client hint the **default policy**, not a capability gap. Refusing would refuse the configured behaviour. |
| `metadata` | `metadata` | Labels for the vendor's own dashboards. They do not enter the answer, its shape, or its price. |
| `user` | `user` | The same. |

Two things the table is deliberate about:

- **The no-op values are not refusals.** `n: 1` and `service_tier: auto` mean "do the
  default", so they raise no capability and cost the everyday caller nothing. SDKs fill `n`
  whether or not the application asked for it; a classification that 400'd on that would be a
  worse defect than the one it replaces.
- **The refusal is raised twice, on purpose, against one set.** Routing answers "does any
  deployment of this model express it" (DESIGN §7.1); the backend answers "does the one that
  was chosen", after the self-hosted normalizations of DESIGN §4.4 have run. The two gates
  read the *same* capability value, carried from the routing table onto the decision and into
  the encoder — they used to compute it independently, and two answers to one question fail
  in the invisible direction: routing admits, encoding drops, the caller gets a `200`.
- **A construct can be expressible and still not sent.** A self-hosted engine's wire shape
  accepts `service_tier`; §4.4 clears it anyway, because there are no price bands to select
  between. That is not a capability gap and is not refused — the request is served and
  `service_tier` is listed in `x-dorang-dropped-params`. The same applies where DESIGN §10.5
  puts dorang's own priority class in the field and the caller's value never reaches the
  wire. Modelling either as a missing capability would produce a `400` for a reason that is
  not true of the deployment.

#### 7.9a Loss in the response direction, and the one case it cannot be reported

Everything above is about the request. A conversion loses things in the other direction too,
and those losses are **not** derivable from the request: the caller did not ask for four
choices, the upstream volunteered them; the caller did not ask for `pause_turn`, the model
stopped that way. The capability subtraction that produces the request-side report cannot see
any of it, because it subtracts against what the *request* uses.

**A buffered answer reports them exactly like a request-side loss.** The construct names reach
`x-dorang-downgraded` and `x-dorang-dropped-params` on the same response, under the same rules,
because the answer's bytes are rendered while its header block is still open. The response-side
constructs are the same vocabulary as above — `rich_stop_reason` and `thinking_block` are the
common ones on a crossing, and `n` is raised as a **downgrade** rather than as a dropped
parameter, because collapsing four choices into one message hands the caller a different answer
rather than the same answer with a knob unapplied. It is the same classification the request
half gives `n`, and the two disagreeing was a defect rather than a distinction.

**A streamed answer does not report them, and cannot.** The headers of an SSE response go out
with its first event, and a stream's response-side losses are discovered as later events arrive
— a collapsed stop reason is known at the terminating frame. There is no header block left by
then, and there is no earlier point at which the loss is known: predicting it from the family
pairing alone would mark every crossing stream as downgraded whether or not anything was.

So the guarantee is stated as it is rather than as a rule with an unmentioned exception:

> **`x-dorang-downgraded` and `x-dorang-dropped-params` report the request direction on every
> answer, and the response direction on buffered answers only.**

Nothing carries a stream's response-direction loss today, and that is recorded as a gap rather
than closed by a mechanism that would not be true. The two channels a stream still has are an
in-band frame — a wire-shape change, not a report — and the ledger, which is written after the
exchange and off the response's critical path. Either would be a decision to take on its own
terms; neither is a header.

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

---

## 11. Error taxonomy — vendor vocabulary, not ours

An error is an API surface. Clients branch on it: SDKs decide whether to retry from the
status and the `type`, and application code often matches `code`. A gateway that invents its
own vocabulary is as incompatible as one that changes a field name — it just fails later, in
someone's retry loop, rather than at the first request.

So dorang emits **the vendor's own error vocabulary**, chosen by which family the caller is
speaking, and normalizes every upstream shape into it. Backends are inconsistent enough that
this is unavoidable work: one engine sends an integer `code` and a Python exception name as
`type`, and a single server has been observed producing four different `type` spellings for
one condition across four call paths.

### 11.1 Envelopes

**OpenAI family** — `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`,
`/v1/responses`, and the rest:

```json
{"error":{"message":"…","type":"invalid_request_error","param":"model","code":"model_not_found"}}
```

`code` is a **string or null**, never a number. `param` is the offending field or null.

**Anthropic family** — `/v1/messages`, `/v1/messages/count_tokens`:

```json
{"type":"error","error":{"type":"invalid_request_error","message":"…","param":"model","code":"model_not_found"}}
```

The outer `"type":"error"` is load-bearing: the Anthropic SDK dispatches on it. A body
carrying only the OpenAI object is not a differently-spelled error to such a client — it is
one it cannot classify **at all**, because the member it switches on is absent. Every error
dorang returned on this family was that body until the envelope became a function of the
family: the projection of §11.2's `type` column landed in the other family's object, where
nothing reads it.

⚠️ **`param` and `code` are emitted here too, and that is a deliberate union rather than an
oversight.** The vendor's own object has neither and §7.1 requires both, so emitting only one
of the two breaks a reader: drop the outer type and this family's SDK cannot classify the
failure, drop `param`/`code` and a client written against §7.1 reads a missing key. Every SDK
in this family tolerates unknown members of the error object, so the union satisfies both and
neither sees a field it must reject. It also keeps §11.2's `code` column — which is what
application code matches on, and which is most of the information in that table — from being
folded into prose that nothing can branch on. An earlier revision of this section said there
was no `code` and no `param` here; no build has ever emitted that shape.

The envelope is chosen by the family the **caller** is speaking, on the single path every
error response takes: `(*server.Error).ForFamily` records the family alongside the projected
type, and `server.appendEnvelope` renders that family's object — delegating the Anthropic one
to `internal/wire/anthropic`, which already owns and golden-tests that serializer, rather than
keeping a second renderer of one contract. Two renderers of one envelope is exactly what
produced this defect. Every family that is neither — models, health, metrics, admin,
passthrough, and the zero value a request that matched no route carries — is answered in the
OpenAI object by the whitelist in `server.Family.Anthropic`, deliberately rather than by
omission. `TestCompat11EnvelopeIsDispatchableOnBothFamilies` walks every row of §11.2 through
both families and decodes the outer discriminator from the bytes.

#### 11.1a The mid-stream error frame

Once the first frame is out the status is 200 and cannot change (§1.3), so the error goes in
band — and the two families do not frame it the same way. The envelope above is only half the
answer; the framing is the other half, and getting it wrong is worse than getting the envelope
wrong, because the client does not see the frame at all.

| Family | Frames |
|---|---|
| OpenAI | `data: {"error":{…}}\n\n` then `data: [DONE]\n\n` (§1.1, §1.2) |
| Anthropic | `event: error\ndata: {"type":"error","error":{…}}\n\n`, and **nothing after it** (§6.1, §6.2) |
| Responses | `event: response.failed\ndata: {"type":"response.failed","response":{…"status":"failed","error":{…},…}}\n\n`, and **nothing after it** (§6a.2) |

A client reading an Anthropic stream dispatches on the event **name**. A data-only frame is
therefore not a frame it mis-parses — it is one it silently discards, which leaves a failed
exchange indistinguishable from a truncated one. In the other direction a `[DONE]` in an
Anthropic stream is a frame a conforming parser cannot name, and `message_stop` must not
follow an `error`: the message did not stop, it failed. `TestMidStreamErrorUsesTheFamilysFraming`
pins both, parsed out of the frames rather than substring-matched.

### 11.2 Canonical conditions

One internal condition per row. dorang never emits a `type` outside this table — the eight
strings in its two type columns are the whole vocabulary, and
`TestEveryTypeOnTheWireIsInTheTable` walks every status through both families and fails on a
ninth. `timeout_error`, `not_implemented_error` and `service_unavailable_error` are declared in
`internal/server` and are in **neither vendor's** vocabulary; the first two used to go out on
real 504s and 501s, where this table says `api_error` in both columns. They are folded now.
Nothing is lost: what a client acts on for a 501 is the code — `route_not_implemented` versus
`route_unknown` (DESIGN §0.3) — and the status carries the rest.

The two `type` columns are **not the same function of the status**. A 404 is
`invalid_request_error` to an OpenAI client and `not_found_error` to an Anthropic one; a 413 is
`invalid_request_error` and `request_too_large`. The projection lives in one place —
`server.TypeForFamily`, applied by `(*server.Error).ForFamily` on the single path every error
response takes — so that a new condition cannot pick a family's spelling by accident. The two
429 capacity rows are the only ones whose type is chosen by *condition* rather than by status;
they set the alternate spelling explicitly where they are raised.

| Condition | HTTP | OpenAI `type` | OpenAI `code` | Anthropic `type` |
|---|---:|---|---|---|
| Malformed request body | 400 | `invalid_request_error` | `invalid_request` | `invalid_request_error` |
| Unknown or disallowed parameter | 400 | `invalid_request_error` | `invalid_parameter` | `invalid_request_error` |
| Structural downgrade refused (§10.1) | 400 | `invalid_request_error` | `unsupported_construct` | `invalid_request_error` |
| Context window exceeded | 400 | `invalid_request_error` | `context_length_exceeded` | `invalid_request_error` |
| Budget exhausted (terminal, §6.4) | 400 | `invalid_request_error` | `budget_exceeded` | `invalid_request_error` |
| Missing or malformed credential | 401 | `authentication_error` | `invalid_api_key` | `authentication_error` |
| Expired or revoked credential | 401 | `authentication_error` | `invalid_api_key` | `authentication_error` |
| Model not in the key's allow-list | 403 | `permission_error` | `model_not_allowed` | `permission_error` |
| Route not permitted for this key | 403 | `permission_error` | `route_not_allowed` | `permission_error` |
| Key blocked | 403 | `permission_error` | `key_blocked` | `permission_error` |
| Unknown model | 404 | `invalid_request_error` | `model_not_found` | `not_found_error` |
| Unknown resource (file, batch, response) | 404 | `invalid_request_error` | `not_found` | `not_found_error` |
| Body over the size cap | 413 | `invalid_request_error` | `request_too_large` | `request_too_large` |
| Rate limit (RPM/TPM) | 429 | `rate_limit_error` | `rate_limit_exceeded` | `rate_limit_error` |
| Provider quota exhausted (§6) | 429 | `rate_limit_error` | `insufficient_quota` | `rate_limit_error` |
| No healthy deployment | 429 | `rate_limit_error` | `no_healthy_deployment` | `overloaded_error` |
| Capacity wait timed out (§5.4) | 429 | `rate_limit_error` | `capacity_unavailable` | `overloaded_error` |
| Gateway fault | 500 | `api_error` | `internal_error` | `api_error` |
| Upstream 5xx after fallback | **the upstream's own 5xx**, passed through | `api_error` | the upstream's own string `code` when it sent one, else the status as a string | `api_error` |
| Upstream answered nothing dorang could use | 502 | `api_error` | `upstream_*`, naming which way it failed | `api_error` |
| Route declared but unimplemented (§0.3) | 501 | `api_error` | `route_not_implemented` | `api_error` |
| Upstream timeout | 504 | `api_error` | `timeout` when dorang's own deadline fired, else as the upstream-5xx row | `api_error` |

Three of these deserve their reasoning stated, because a plausible alternative is wrong:

- **Budget exhausted is 400, not 429.** A `429` invites a retry and marks the condition as a
  fallback trigger (§7.6), which would spend a *different* subject's budget on a model the
  caller never asked for. Budget is terminal; the status has to say so.
- **Model not in the allow-list is 403, not 401.** The credential authenticated fine; it is
  not permitted this model. A `401` tells the client to re-authenticate, which cannot help.
  A reference proxy answers `401` here — dorang does not follow it, because the guidance it
  gives the client is actively misleading.
- **No healthy deployment is 429 and not 503.** It is a capacity condition with a
  `Retry-After`, and clients already back off correctly on `429`.

- **An upstream 5xx keeps its own status; it is not folded into 502.** A `500` from the
  provider leaves dorang as a `500` and a `502` as a `502`. This row read "Upstream 5xx
  after fallback → 502" until an audit measured it: dorang has always passed the status
  through (`backend.upstreamError` hands the upstream's status straight to
  `server.Normalize`), and the passthrough is what keeps dorang status-identical to the
  incumbent on every upstream failure — nine of them in the reproduction that found this.
  Folding would be worse as well as untrue: `502` says *dorang* could not reach a working
  backend, and a client that retries on `502` and gives up on `500` would be told to retry
  a model the provider has retired. `502` is reserved for the case where dorang got no
  usable answer at all — no response, an unreadable body, a refused redirect, a failure
  inside a stream already begun — which is a statement about the *hop*, not about the
  provider's opinion of the request. `TestUpstream5xxKeepsItsOwnStatus` pins it.

The `code` column of the upstream-5xx and 504 rows says what it says because §11.3's preservation rule
comes first: a backend that sent a usable **string** code already satisfies §7.1, and that code
is more useful to a client than `upstream_error` would be. It is passed through; a code that
cannot go in the envelope — a number, an object, an array — is replaced by dorang's canonical
one and preserved on `x-dorang-native-error-code`. An earlier revision of this table specified
`upstream_error` unconditionally, which no build has ever emitted and which contradicts §11.3
two subsections later.

#### 11.2a dorang's own refusals

These have no row above because the reference vocabulary has no condition for them. They are
listed so the next audit does not read them as drift, and so that the *reason* each one is
separate is written down rather than re-derived.

Each is a refusal whose **fix is different** from the row it would otherwise collapse into.
That is the whole test for belonging here: a condition a caller cannot act on differently gets
the §11.2 code and folds into the rows above. The `type` columns are the same projection
§11.2 uses, so nothing here escapes the type vocabulary.

This is not an exhaustive code registry — dorang also emits operational codes for conditions no
client branches on (`method_not_allowed`, `admin_required`, `catalog_not_configured`, the
`passthrough_*` family). Those are diagnostic text with a stable spelling; the table is for
codes a client is expected to *match*.

| Condition | HTTP | OpenAI `type` | OpenAI `code` | Anthropic `type` |
|---|---:|---|---|---|
| Rotated secret retired (§11.2c) | 401 | `authentication_error` | `secret_retired` | `authentication_error` |
| Stored credential uses an unsupported hash scheme | 401 | `authentication_error` | `unsupported_hash_scheme` | `authentication_error` |
| Legacy hash scheme disabled | 401 | `authentication_error` | `legacy_scheme_disabled` | `authentication_error` |
| Legacy import window closed | 401 | `authentication_error` | `legacy_window_closed` | `authentication_error` |
| Key pended by the token guard (§11.6) | 403 | `permission_error` | `credential_pended` | `permission_error` |
| Key has no owning user or team | 403 | `permission_error` | `no_principal` | `permission_error` |
| Credential store unreachable | 503 | `api_error` | `auth_unavailable` | `overloaded_error` |
| Route is unknown (not merely unbuilt) | 501 | `api_error` | `route_unknown` | `api_error` |
| Structural pin cannot be routed (§B.2) | **400** | `invalid_request_error` | `state_pin_unroutable` | `invalid_request_error` |
| Credential pin cannot be routed | **400 or 503** | per status | `credential_pin_unroutable` | per status |
| Credential pin exhausted | **429** | `rate_limit_error` | `credential_pin_exhausted` | `rate_limit_error` |
| Credential pin saturated | 503 | `api_error` | `credential_pin_saturated` | `overloaded_error` |
| Every fallback candidate already tried (§7.6) | 503 | `api_error` | `fallback_exhausted` | `overloaded_error` |
| Fallback hop or wall-clock budget spent | varies | per status | `max_hops_exhausted`, `fallback_budget_elapsed` | per status |
| Stream already committed, cannot hop (§7.6) | **500** | `api_error` | `stream_committed` | `api_error` |

> ⚠️ **Four of these rows carried `503` and the code has never returned `503` for three of
> them.** Corrected 2026-08-03 against `internal/router`, which is the only producer of all six
> codes; the `type` columns follow from `server.TypeForStatus`, so they moved with the statuses.
> `state_pin_unroutable` is `400` — the request carries opaque state no deployment can accept,
> which is a property of the request and not of the fleet's health, so a client that retries it
> unchanged will fail identically. `stream_committed` is `500`: the answer was already partly
> delivered and a second attempt would duplicate output, which is a fault and not back-pressure.
> `credential_pin_exhausted` is `429` because it is a quota window recovering, and it is the one
> pin refusal that carries a `Retry-After` (§11.4). `credential_pin_unroutable` is genuinely
> two statuses from two call sites — `400` when no deployment can serve the pinned account at
> all, `503` when every one that could is out of service — and that difference is the point of
> the code, so it is stated rather than flattened.
>
> This is the §11.2a table's own failure mode rather than an isolated typo: a status column is a
> claim about code that reads like a category, and grouping "exhausted / saturated" in one row
> hid a `429` inside a `503`. **A wire-contract row is one condition, one status, one producer.**

`secret_retired` is the one worth arguing about, because `invalid_api_key` is so nearly right.
The fix for a retired secret is "use the secret the last rotation issued", not "get a new key".
Reporting it as `invalid_api_key` sends a caller to re-provisioning, which is precisely the
thing rotation exists to avoid. The same reasoning keeps `credential_pended` apart from
`key_blocked`: one is a statistical judgement an operator can release in a single action, the
other is a decision an operator made, and a support ticket that cannot tell them apart goes to
the wrong screen.

What was collapsed *into* `invalid_api_key`: `missing_credential`, `malformed_credential`,
`invalid_credential` (twice — unknown key and digest mismatch) and `credential_expired`. Five
spellings, one client-visible condition, and five ways for a client's `invalid_api_key` branch
to miss. The distinction survives in the message, which is where §11.1 already says the
Anthropic family folds everything `code` would have carried.

### 11.3 The native error is preserved, never forwarded

The upstream's own `type` and `code` are surfaced in `x-dorang-native-error-type` /
`x-dorang-native-error-code`. They are **not** put in the response body. Forwarding them
reproduces the exact defect this section exists to fix — a client branching on `type`
mis-branches on a vendor-specific string — and it is the same out-of-band pattern §4.2a uses
for stop reasons.

`x-dorang-native-error-code` carries the backend's own code only when that code could not
*be* the envelope's code: a number where §7.1 requires a string, or a JSON object or array
where it requires a scalar. A backend that sent a plain string code sets no header, because
that code is already in the envelope and a header repeating it on every error would signal
nothing. Both headers are clamped to 128 bytes and refused if they contain a control
character; a backend is a less trusted source than the caller, and the caller's path is
already checked.

#### 11.3a The upstream's *message* goes to the operator, not to the client

There is no `x-dorang-native-error-message`, and there deliberately will not be one.

The reason the message is kept out of the body is concrete rather than tidy: several
OpenAI-compatible servers answer 401 with the offending key quoted in the message, so a gateway
that copies the upstream's text into its own envelope hands the operator's provider credential
to whichever tenant happened to be calling while a credential was invalid or mid-rotation. A
hostile backend does not have to wait for that — it can answer any request with the `x-api-key`
header it was just given.

**That reasoning transfers to a response header unchanged.** A header is read by the same
client, over the same connection, by every HTTP library ever written. Moving the text from the
body to a header would relocate the leak, not close it. The `type` and the `code` do go out of
band because they are enumerated tokens, not free text.

So the message goes to the **operator log**, joined to the request by `x-dorang-request-id`,
scrubbed by `internal/redact` with the exact secret that request carried. That is recoverable
without a database round trip, which is what an operator debugging an outage needs. What a
client gets is dorang's own wording for the status, the canonical `code`, and — when the
backend sent a usable one — the backend's own string `code` in the envelope, which is often the
whole answer: a `model_retired` code identifies the condition without the sentence.

This is a **known reduction in fidelity against the incumbent**, which does put the upstream's
sentence in the body. It is deliberate. Restoring it for clients would be a new decision with a
security review attached, not a bug fix.

### 11.4 `Retry-After` goes out on every 429, 503 and 529 that has a number behind it

Not gated behind a detail header (§10.4). A client acts on it; without it every SDK's backoff
degrades to a fixed guess — and that argument does not weaken as the status gets worse. It gets
stronger: a rate-limited client can at least infer a window from its own request rate, and a
client told the far side is overloaded has nothing to guess with at all.

**Where the number comes from. There are two producers and there is no third.**

| Source | Reaches the client on |
|---|---|
| the upstream's own `Retry-After`, parsed by `internal/backend` off the response being relayed | every relayed `429`, `503` and `529` |
| `router.Error.ResetAt` — the instant an exhausted **quota window** recovers, from dorang's own quota source | the two quota refusals, `insufficient_quota` and `credential_pin_exhausted`, both `429` (§11.2a) |

The three statuses are the ones whose whole content is "come back later"; 529 is the Anthropic
family's spelling of 503 and is treated as one throughout §11.2. Other 5xx are excluded on
purpose: a `500` or a `502` says something went wrong, not that the far side will be ready at a
stated time, so an upstream's stray header on one is not forwarded.

Pinned by `TestCompat11RetryAfterOnEveryRetrySignallingStatus` and
`TestCompat11RetryAfterIsNotGatedBehindTheDetailHeader` in `internal/server/compat11_test.go`,
both directions — the three statuses that carry it and the three that must not.

> ⚠️ **This section read "`Retry-After` is mandatory on every 429 and 503" until 2026-08-03, and
> it was wrong in three independent ways — one of them a code defect and two of them promises
> nothing had ever kept.** It is worth listing them, because the shape recurs (DESIGN §17.1):
> a section whose title is an *argument* reads as a *description*, and nothing fails when the
> world stops matching it.
>
> 1. **The 503 half was false in the code.** `internal/server` tested `status == 429` in both
>    places that could emit the header, so an upstream's own `Retry-After` on an overloaded
>    `503` or `529` was parsed, carried the whole way on `Error.RetryAfterSeconds`, and dropped
>    at the response writer. **Fixed** — that is the table above, and the two named tests are
>    what would fail if it regressed. The document was right and the code was wrong, which is
>    why the title was widened rather than narrowed.
> 2. **"dorang supplies its own estimate from … the capacity queue" was never true.** There is
>    no capacity-queue estimate anywhere. `capacity_unavailable` and `no_healthy_deployment` —
>    §11.2's two capacity rows, both `429` — carry no `Retry-After`, and neither does
>    `credential_pin_saturated` (`503`). A queued request has already spent
>    its whole wait budget by the time it is refused; nothing in the broker publishes when a
>    reservation will free, so any number here would be invented. **Narrowed**, with the reason
>    recorded, rather than closed by making one up: a header carrying a guess is worse than an
>    absent one, because the client cannot tell it from a measurement (§7.8's rule).
> 3. **dorang's own `503`s carry no estimate, and mostly cannot.** `fallback_exhausted`,
>    `not_configured`, `budget_unavailable`, batch `draining` and `gateway_shutting_down` are
>    conditions with no recovery instant to publish — the last one is a *node* going away and
>    the answer is another node, not a later second. One exception is worth naming so it is not
>    re-derived: `auth_unavailable` (§11.2a) is raised when the miss-budget bucket is empty
>    ([CONFIG.md](CONFIG.md) §5), and that bucket has a known refill rate, so `1/rate` **is** an
>    honest estimate and it is the one dorang-side `503` where this could be closed. Not taken;
>    recorded here so the next reader starts from the argument rather than from the gap.
>
> The general form, since this is the second §11.4 claim to be wrong the same way: **"mandatory
> on every X" is a completeness claim, and a completeness claim in prose is a test nobody runs.**
> The table above is now pinned by name.

> ✅ **The fourth case is covered since 2026-09-12.** "The upstream supplies one" used to mean a
> literal `Retry-After` header and nothing else, so an upstream answering `429` with only
> `x-ratelimit-reset-requests` — a window reset instant rather than a delay — supplied nothing
> this gateway forwarded, and `router.Outcome.ResetAt` had no producer anywhere in the tree.
> `internal/backend.rateLimitReset` now reads the three spellings on a `429`, `503` or `529` that
> carries no `Retry-After`: OpenAI's `x-ratelimit-reset-*` durations, Anthropic's
> `anthropic-ratelimit-*-reset` instants, and the IETF `RateLimit-Reset` / `X-RateLimit-Reset`
> deltas. The exhausted window (its `remaining` reads zero) is the one taken, the earliest among
> several; the instant becomes the client's `Retry-After`, the deployment's cooldown (in place of
> `fallback.rate_limit_cooldown`, which is a guess made before the provider said anything) and the
> outcome's reset. A stray reset header on a `400` is not read. Pinned by
> `TestAnUpstreamResetHeaderIsAWaitAndACooldown` (backend),
> `TestAProviderNamedResetOutlivesTheConfiguredCooldown` (router) and
> `TestAnUpstreamResetHeaderReachesTheCallerAsRetryAfter` (the assembled gateway).
