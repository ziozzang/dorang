# Vendor Extension APIs and Context-Window Handling

> Two things a gateway must handle that no OpenAI-compatible specification covers.
>
> Every behavioral claim below carries a `file:line` citation read out of source. Where a
> claim could not be checked against source it says **UNVERIFIED** rather than guessing.
> Where source and documentation disagree, source wins and the disagreement is named.
>
> Read alongside DESIGN §10.1 (two kinds of loss), §10.5a (dorang does not compact),
> §10.6 (generic passthrough engine), §10.7 (metadata equivalence), and COMPATIBILITY.
>
> 한국어: [EXTENSIONS.ko.md](EXTENSIONS.ko.md)

---

## 0. Reading this document

### 0.1 Citation roots

Paths are given relative to a root, so lines stay readable:

| Prefix | Root |
|---|---|
| *(none)* | this repository, `` |
| `codex/` | `codex` — the Codex CLI Rust workspace |
| `grok/` | `grok` — the grok CLI Rust workspace |
| `hermes/` | `hermes-agent` |
| `jikji/` | `jikji` |
| `openclaw/` | `openclaw` |
| `opencode/` | `opencode` |
| `openharness/` | `openharness` |
| `claude-code/` | `claude-code` — a leaked TypeScript snapshot, see §G |
| `vllm/` | `vllm-project/vllm` at commit `7aea73d`, the same tree VLLM.md was derived from |

Three things the brief that commissioned this document asserted turned out not to hold: a vLLM
route that does not exist (§0.2), four jikji source files that exist only as tests (§D.4), and
`internal/press` described as compaction machinery when it is an ingress-transport package
(§D.4). Each is corrected where it arises rather than quietly worked around, because a
specification that silently absorbs a wrong premise carries it forward.

### 0.2 One premise in the brief was wrong, and the correction matters

The task that produced this document said VLLM.md "mentions a `/v1/responses/compact` route
family". **It does not, and no such route exists in vLLM.** The string `compact` appears
nowhere in VLLM.md, and vLLM's Responses router registers exactly three routes:

- `vllm/entrypoints/openai/responses/api_router.py:49` — `POST /v1/responses`
- `vllm/entrypoints/openai/responses/api_router.py:80` — `GET /v1/responses/{response_id}`
- `vllm/entrypoints/openai/responses/api_router.py:110` — `POST /v1/responses/{response_id}/cancel`

A repository-wide grep for `compact` across the vLLM checkout returns only KV-connector,
spec-decode, and JSON-serialization uses — nothing route-shaped.

`/responses/compact` is real, but it is an **OpenAI/Codex backend route**, not a vLLM one:

```rust
// codex/codex-rs/core/src/client.rs:159-165
const REALTIME_CALLS_ENDPOINT: &str = "/realtime/calls";
const RESPONSES_ENDPOINT: &str = "/responses";
const RESPONSES_COMPACT_ENDPOINT: &str = "/responses/compact";
// `/responses/compact` is unary, so the timeout covers the full response rather than one idle
// period between stream events.
const COMPACT_REQUEST_TIMEOUT_IDLE_MULTIPLIER: u32 = 4;
const MEMORIES_SUMMARIZE_ENDPOINT: &str = "/memories/trace_summarize";
```

The correction is load-bearing, not pedantic. If compaction were a vLLM route, dorang would
meet it on a self-hosted backend it fully controls and could reason about. It is not. It is a
route on a **proprietary, credential-gated backend whose request and response bodies dorang
cannot construct** — which puts it squarely in the passthrough category and nowhere else.

---

## A. Inventory of vendor extension APIs

The classification column is the one that drives design. Three values:

- **neutral** — expressible in `CanonicalRequest`/`CanonicalResponse`, or droppable per §10.1.
- **opaque** — must be relayed byte-identically; see §B.
- **gateway-owned** — dorang must strip, rewrite, or refuse it, because relaying it verbatim
  is itself the defect.

### A.1 OpenAI, reached through Codex

Codex is the most extension-heavy client examined and the only one that calls a **server-side
compaction endpoint**. Its base URL depends on the credential:

```rust
// codex/codex-rs/model-provider-info/src/lib.rs:244-262
let default_base_url = if matches!(auth_mode, Some(AuthMode::Chatgpt | …))
{ CHATGPT_CODEX_BASE_URL } else { "https://api.openai.com/v1" };
```

with `CHATGPT_CODEX_BASE_URL = "https://chatgpt.com/backend-api/codex"`
(`codex/codex-rs/model-provider-info/src/lib.rs:38`). So the same client speaks two different
path spaces depending on which credential is loaded — the first reason a per-provider route
map cannot be a single global table.

#### Endpoints

| Path | What it does | Stateful | Class |
|---|---|---|---|
| `POST {base}/responses/compact` | Server-side conversation compaction. Returns `{output: [ResponseItem]}` — a replacement history. `codex/codex-rs/codex-api/src/endpoint/compact.rs:35-37`, response shape `:85-88` | yes (consumes and returns conversation) | **opaque** |
| `POST {base}/memories/trace_summarize` | Cross-session memory summarization. `codex/codex-rs/codex-api/src/endpoint/memories.rs:32-34` | yes | **opaque** |
| `POST {base}/alpha/search` | Backend search. `codex/codex-rs/codex-api/src/endpoint/search.rs:31-33` | no | **opaque** |
| `POST {base}/responses` over **WebSocket** | Same body family, `ws`/`wss` scheme. `codex/codex-rs/codex-api/src/endpoint/responses_websocket.rs:389`; scheme upgrade at `codex/codex-rs/codex-api/src/provider.rs:92-103` | yes (`previous_response_id`) | **opaque** |
| `POST {base}/realtime/calls`, `{base}/live` | Realtime session creation; path varies by whether the base URL contains `/backend-api`. `codex/codex-rs/codex-api/src/endpoint/realtime_call.rs:62-79` | yes | **opaque** |
| `GET {base}/models?client_version=…` | Model catalog with ETag. `codex/codex-rs/codex-api/src/endpoint/models.rs:31-44` | no | partly neutral — see A.1 fields |
| `POST {base}/images/generations`, `…/edits` | `codex/codex-rs/codex-api/src/endpoint/images.rs:39`, `:52` | no | neutral |
| `/api/codex/*` **or** `/wham/*` (accounts, profiles, tasks, config bundle, settings) | Control plane, two path styles selected by config. `codex/codex-rs/backend-client/src/client.rs:318-320`, `:336-337`, `:376-377`, `:412-413`, `:449-450`, `:495-496`, `:632-633` | yes | **opaque** |
| `wss://…/wham/remote/control/server{,/enroll,/refresh,/pair,/pair/status}` | Remote control. `codex/codex-rs/app-server-transport/src/transport/remote_control/protocol.rs:224-236` | yes | **opaque** |

Note the path-join rule: `codex/codex-rs/codex-api/src/provider.rs:53-75` trims one slash from
each side and concatenates. It never inserts `/v1`. A gateway prefix-mapping these routes must
reproduce exactly that, and DESIGN §10.6 step 2 already says "strip the prefix, join onto the
provider's base URL" — which is the same rule.

#### Non-standard request fields on `/responses`

The full serialized body is `codex/codex-rs/codex-api/src/common.rs:251-275`, populated at
`codex/codex-rs/core/src/client.rs:907-923`:

| Field | Behavior | Class |
|---|---|---|
| `client_metadata` | Non-standard object carrying `session_id`, `thread_id`, `turn_id`, `window_id`, `installation_id`, `compaction`, W3C traceparent. Keys at `codex/codex-rs/core/src/responses_metadata.rs:27-42`, construction at `:215-246` | **opaque** |
| `include: ["reasoning.encrypted_content"]` | Always exactly this one element. `codex/codex-rs/core/src/client.rs:888` | **opaque** (see §B) |
| `store` | **`false` except on Azure**: `store: provider.is_azure_responses_endpoint()`, `codex/codex-rs/core/src/client.rs:915`. Azure detection is a substring match over six markers, `codex/codex-rs/codex-api/src/provider.rs:116-127` | neutral (`Store` in §10.7) |
| `prompt_cache_key` | Defaults to the **session id**. `codex/codex-rs/core/src/client.rs:483-487` | **opaque** |
| `reasoning.context: auto\|current_turn\|all_turns` | Non-standard sub-field. `codex/codex-rs/codex-api/src/common.rs:140-156` | **opaque** |
| `stream_options.reasoning_summary_delivery: sequential_cutoff` | Non-standard. `codex/codex-rs/codex-api/src/common.rs:158-167` | **opaque** |
| `text.verbosity` | `codex/codex-rs/codex-api/src/common.rs:190-205` | droppable |
| `previous_response_id`, `generate` | **WebSocket body only**, not the HTTP body. `codex/codex-rs/codex-api/src/common.rs:301-329` | **opaque** |
| `context_management: [{type: "compaction", compact_threshold: N}]` | **Server-side compaction as a request field.** See A.1a | **opaque** |
| `prompt_cache_retention: "24h"` | Sent only against `api.openai.com`. `openclaw/packages/ai/src/transports/openai-responses-transport.ts:2131-2132`, `:1950`, type at `:2396`; chat-completions equivalent at `openclaw/packages/ai/src/transports/openai-completions-transport.ts:1748` | **opaque** |

Two absences are worth recording because a gateway might assume them present:
`previous_response_id` is **not** on the HTTP body, and `safety_identifier` appears nowhere in
the workspace.

#### A.1a `context_management` — server-side compaction as a request field

`/responses/compact` is not the only server-side compaction mechanism on this backend, and it
is not even the one most likely to reach dorang. A second client asks for compaction by
setting a **field on an ordinary `/responses` request**:

```ts
// openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:409-416
if (policy.useServerCompaction && payloadObj.context_management === undefined) {
  payloadObj.context_management = [
    {
      type: "compaction",
      compact_threshold: policy.compactThreshold,
    },
  ];
}
```

The threshold is **70% of the context window**, floored at 1,000 and defaulting to 80,000 when
the window is unknown (`openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:288-294`),
and it is enabled only when `store` is explicitly `true` and the provider is OpenAI
(`:296-312`).

This is the single most important row in §A for dorang's purposes:

- It has **no route to key on**. A gateway that decides "compaction is passthrough traffic" by
  path will forward this to the ordinary `/v1/responses` adapter.
- It is a **nested array of objects** in the request body. Adapters that decode to a known-field
  struct and re-encode drop it silently, and the caller then gets no compaction, no error, and
  a request that overflows later.
- It **couples to `store`**. dorang's `Store` field (§10.7) and `responses_store` (§9.2) now
  have a third interaction: a caller can only get server compaction if `store: true` survives
  the crossing.

So OpenAI, alone among the vendors surveyed, offers server-side compaction in **three
mutually incompatible shapes**: a dedicated route, a sentinel input item, and a request field.
No two are detectable by the same mechanism. This is the concrete reason §E3.1 argues for
allow-lists and §E3.3 argues for preserving unknown body structure — a single "compaction
route" special case would catch one of the three.

#### A.1b Fields Codex-route servers reject

A client talking to `chatgpt.com/backend-api/codex` must **remove** parameters that the same
vendor accepts on `api.openai.com`:

```ts
// openclaw/packages/ai/src/transports/openai-responses-transport.ts:2000-2007
const OPENAI_CODEX_RESPONSES_UNSUPPORTED_PARAMS = [
  "max_output_tokens",
  "metadata",
  "prompt_cache_retention",
  "service_tier",
  "temperature",
  "top_p",
];
```

and `store: true` is rejected outright — *"ChatGPT Codex Responses rejects `store: true`
('Store must be set to false'). WebSocket continuation still works via connection-scoped
previous_response_id state."*
(`openclaw/packages/ai/src/providers/openai-chatgpt-responses.ts:1471-1472`).
Replayed `input[].status` must also be stripped, because strict Responses-compatible endpoints
reject it (`openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:333-343`).

This directly supports DESIGN §4.3's decision to key capability on `(kind, model)` and
COMPATIBILITY 5.5's decision to make the `max_tokens` field name per-deployment configuration:
here the **same vendor, same wire family, same model** accepts a different parameter set
depending on which credential opened the connection. dorang's separate `codex-responses` kind
(`pkg/catalog/provider_defaults.yaml:88-95`) is the right shape; what it lacks is this
rejected-parameter list.

#### Headers

Request headers are enumerated at `codex/codex-rs/core/src/client.rs:142-158`:
`x-codex-installation-id`, `x-codex-turn-state`, `x-codex-turn-metadata`,
`x-codex-parent-thread-id`, `x-codex-window-id`, `x-openai-memgen-request`,
`x-openai-subagent`, `x-responsesapi-include-timing-metrics`, plus `OpenAI-Beta`.
Also `session-id` and `thread-id` (`codex/codex-rs/codex-api/src/requests/headers.rs:5-14`),
`x-client-request-id` (`codex/codex-rs/codex-api/src/endpoint/responses.rs:88-90`),
`ChatGPT-Account-ID` and `X-OpenAI-Fedramp`
(`codex/codex-rs/model-provider/src/bearer_auth_provider.rs:41-44`).

`x-openai-subagent` takes the value `"compact"` when the request *is* a compaction
(`codex/codex-rs/codex-api/src/requests/headers.rs:16-31`) — the one header that tells a
gateway a request is compaction traffic without parsing the body.

Response headers Codex reads: `x-reasoning-included`, `x-codex-turn-state`, `openai-model`,
`x-request-id` (`codex/codex-rs/codex-api/src/sse/responses.rs:28-32`), `X-Models-Etag`
(`:41-45`), and a large `x-codex-*` rate-limit family
(`codex/codex-rs/codex-api/src/rate_limits.rs:180-220`, `:282-356`).

**Requests may be zstd-compressed**: `codex/codex-rs/codex-api/src/requests/responses.rs:41-46`
defines `enum Compression { None, Zstd }`, wired at
`codex/codex-rs/codex-api/src/endpoint/responses.rs:135-152`. A passthrough that assumes a JSON
body it can sniff is wrong here.

### A.2 xAI, through the grok CLI

grok speaks **three** inference wire formats against one base URL family:

- `POST {base}/chat/completions` — `grok/crates/codegen/xai-grok-sampler/src/client.rs:796`
- `POST {base}/responses` — `grok/crates/codegen/xai-grok-sampler/src/client.rs:1067`
- `POST {base}/messages` — Anthropic Messages shape, `grok/crates/codegen/xai-grok-sampler/src/client.rs:1406`

Base URLs are compiled in: `grok/crates/codegen/xai-grok-env/src/lib.rs:22-28` gives
`https://cli-chat-proxy.grok.com/v1` for chat, `wss://code.grok.com/ws/code-agent` and
`wss://grok.com/ws/gw/` for websockets. The direct API path `https://api.x.ai/v1` is also
supported (`grok/crates/codegen/xai-grok-shell/src/agent/models.rs:2073`). Join rule is the
same trim-and-concatenate as Codex: `grok/crates/codegen/xai-grok-sampler/src/client.rs:703-707`.

**Explicitly absent, checked**: no `/v1/chat/deferred-completion/{id}`, no `/v1/tokenize-text`,
no `/v1/api-key`. The `deferred` hits in this tree are local tool-completion queues.

| Field | Behavior | Class |
|---|---|---|
| `search_parameters` | `{mode, sources[], from_date, to_date, return_citations, max_search_results}`. `grok/crates/codegen/xai-grok-sampling-types/src/types.rs:84-85`, struct `:654-670`, sources `:674-694` | **opaque** |
| `reasoning_effort` incl. `xhigh` | `grok/crates/codegen/xai-grok-sampling-types/src/types.rs:88-89`; `xhigh` used at `grok/crates/codegen/xai-grok-shell/src/agent/models.rs:2244` | neutral — DESIGN §10.2 already lists `xhigh` |
| `x_search` server-side tool | Injected as **raw JSON after serialization** because it cannot be expressed in the typed tool schema. `grok/crates/codegen/xai-grok-sampler/src/client.rs:1192`, `:1733`; shape at `grok/crates/codegen/xai-grok-sampling-types/src/tool_overrides.rs:146-148` | **opaque** |
| `x-grok-conv-id`, `-req-id`, `-model-override`, `-session-id`, `-agent-id`, `-turn-idx`, `-deployment-id`, `-user-id` | Headers, not body fields (`#[serde(skip)]` at `grok/crates/codegen/xai-grok-sampling-types/src/types.rs:91-105`), applied at `grok/crates/codegen/xai-grok-sampler/src/client.rs:56-71` | **opaque**, except `x-grok-model-override` — see below |
| `usage.cost_in_usd_ticks` | xAI extension, 1 USD = 1e10 ticks. `grok/crates/codegen/xai-grok-sampling-types/src/types.rs:535-549` | neutral, and dorang should read it |
| `usage.context_details.{input_tokens,output_tokens}` | See A.2a — the single most consequential usage extension found | **opaque** |
| `message.citations[]`, `message.reasoning_content` | `grok/crates/codegen/xai-grok-sampling-types/src/types.rs:490-503` | `reasoning_content` neutral; `citations` structural (no OpenAI equivalent) |

`x-grok-model-override` is **gateway-owned**, not opaque. It carries the model id in a header
while the body also carries `model`. DESIGN §7.2 makes the body's model the real upstream id
and rewrites the response's `model` back to the alias. Two mechanisms claiming to select the
model is exactly the ambiguity §7.2 exists to remove. dorang must set this header itself from
the resolved deployment, or not send it — never relay the client's value unexamined.

#### A.2a `context_details` — a usage extension that changes when the client compacts

```
// grok/crates/codegen/xai-grok-sampler/src/client.rs:89-98
/// On `response.completed` / `response.incomplete`, this also rewrites
/// `response.usage.total_tokens` in place to the live context length
/// (`context_details.input_tokens + context_details.output_tokens`)
/// when the API emits the xAI-specific `context_details` field.
```

and the reason, verbatim at `grok/crates/codegen/xai-grok-sampler/src/client.rs:137-145`:

> `total_tokens` drives the CLI's `/context` bar, the auto-compact threshold, and
> `meta.totalTokens` on persisted sessions. Under server-side multi-turn loops (e.g.
> `web_search`, `x_search`) the wire's cumulative total inflates as the loop runs;
> `context_details` reports the final turn's prompt + output tokens — the real live context
> the model is sitting in.

Extraction is at `grok/crates/codegen/xai-grok-sampler/src/client.rs:189-194`.

This is the cleanest demonstration in the whole survey that **a usage extension is not
cosmetic**. DESIGN §10.7 normalizes usage into `InputTokens`/`OutputTokens`/`TotalTokens` and
is emphatic that "token accounting is the part that must be exactly right" because it feeds
cost. It is also, here, what decides when the caller compacts. A gateway that recomputes
`total_tokens` from the typed fields hands back the cumulative number the client deliberately
replaced, and the client's compaction fires at the wrong time — early on a search loop, and
with no error anywhere. §10.7's normalization must therefore preserve, not overwrite, a
`context_details` the backend supplied.

### A.3 vLLM

vLLM has **no compaction endpoint** (§0.2) but it does have the only *server-side context
management* mechanism found anywhere in the survey, plus a wide non-standard route surface.
Full route inventory read from `vllm/entrypoints/**/api_router.py`:

| Route | Class | Note |
|---|---|---|
| `POST /tokenize`, `POST /detokenize` | **opaque** or gateway-owned | `vllm/entrypoints/serve/tokenize/api_router.py:37`, `:63`. Returns `max_model_len`; see §D.1 |
| `GET /tokenizer_info` | **opaque** | `:98`, registered only under `--enable-tokenizer-info-endpoint` |
| `POST /v1/load_lora_adapter`, `/v1/unload_lora_adapter` | **opaque**, operator-gated | `vllm/entrypoints/serve/lora/api_router.py:43`, `:60` |
| `POST /reset_prefix_cache`, `/reset_mm_cache`, `/reset_encoder_cache` | **must not be exposed** | `vllm/entrypoints/serve/dev/cache/api_router.py:20`, `:47`, `:58`. VLLM.md §5 already says development-mode endpoints must never be called against a production backend; this is the cache-management family the brief asked about, and the answer is that it is destructive |
| `POST /sleep`, `/wake_up`, `GET /is_sleeping`; `/pause`, `/resume`, `/abort_requests`, weight-update family; `/collective_rpc`; `/server_info` | **must not be exposed** | `vllm/entrypoints/serve/dev/**` |
| `POST /v1/messages`, `/v1/messages/count_tokens` | neutral | `vllm/entrypoints/anthropic/api_router.py:50`, `:96` |
| `POST /v1/responses/{id}/cancel` | **opaque** | `vllm/entrypoints/openai/responses/api_router.py:110` — **dorang's §2.1 route table does not list it** |
| `GET /v1/responses/{id}?starting_after=&stream=` | **opaque** | `:80-87`. The two query parameters are extensions |
| `/classify`, `/score`, `/rerank`, `/pooling`, `/embed`, `/invocations` (SageMaker), `/ping`, `/version`, `/load`, `/start_profile`, `/stop_profile`, elastic-EP and fault-tolerance families | mixed | see the router files |

Also: dorang's §2.1 table lists `DELETE /v1/responses/{id}`, which **vLLM does not implement**.
The route table and the backend disagree in both directions.

#### A.3a `truncation` and `truncate_prompt_tokens` — server-side context management

This is the closest thing to server-side compaction that actually exists on an
OpenAI-compatible server, and it is not compaction — it is truncation.

```python
# vllm/entrypoints/openai/responses/protocol.py:183
truncation: Literal["auto", "disabled"] | None = "disabled"
```

which becomes, at `vllm/entrypoints/openai/responses/protocol.py:346`:

```python
truncate_prompt_tokens=-1 if self.truncation != "disabled" else None,
```

and `-1` means "the full window" (`vllm/renderers/params.py:153-158`), with a side that
defaults to the tokenizer's (`vllm/renderers/params.py:160-166`). The effect is at
`vllm/entrypoints/serve/utils/api_utils.py:178-188`: when `truncate_prompt_tokens` is set, the
input length is **clamped instead of raising**, so the `ValueError("Input length … exceeds
model's maximum context length")` never fires.

Chat completions and completions carry the raw knob as a vLLM extension:
`vllm/entrypoints/openai/chat_completion/protocol.py:260-268` — `truncate_prompt_tokens` plus
`truncation_side: "left" | "right"`.

Three consequences for dorang:

1. **`truncation: "auto"` is a caller's explicit request to have their conversation silently
   shortened.** It is theirs to make. It is not dorang's, and dorang must never set it.
2. It **suppresses the very 400 that §7.6's `context_window` fallback keys on**. A request that
   would have been routed to a larger model is instead answered from a truncated prompt, with
   a `200`. If dorang injected `truncation: "auto"` through `params.set{}` or a kind default,
   it would disable its own fallback path across a whole deployment.
3. It is the concrete instance of DESIGN §10.5a's warning: a plausible answer to a question
   the caller did not ask, with nothing in the response to say so.

#### A.3b vLLM Responses-family fields

`vllm/entrypoints/openai/responses/protocol.py`: `background` (`:139`, requires `store: true`
per the validator at `:459-465`), `store` defaulting to **`true`** (`:174`),
`previous_response_id` (`:161`), `prompt_cache_key` — *accepted and ignored*, and the field
says so itself (`:205-212`: "This field has not been implemented yet and vLLM will ignore
it"), `cache_salt` (`:244-254`), `enable_response_messages` (`:256-262`),
`previous_input_messages` in Harmony format, mutually exclusive with `previous_response_id`
(`:263-267`), `request_id` (`:215-222`), `priority` (`:234-243`, whose description is the false
one VLLM.md §1.2 documents), `include_reasoning` (`:164-172`).

`prompt_cache_key` is a new entry for VLLM.md §8's table of places vLLM's own documentation
is wrong — except here the field's own description is honest and it is the *OpenAI* contract
that is not met. A caller who sets it gets no error and no caching.

### A.4 GLM/Z.AI, MiniMax, and Anthropic

From openclaw, which carries production adapters for all three.

#### GLM / Z.AI

Four base URLs, two of them a separate *coding* plan path:
`openclaw/extensions/zai/model-definitions.ts:6-9` —
`https://api.z.ai/api/coding/paas/v4`, `https://open.bigmodel.cn/api/coding/paas/v4`,
`https://api.z.ai/api/paas/v4`, `https://open.bigmodel.cn/api/paas/v4`.

| Field or route | Behavior | Class |
|---|---|---|
| `thinking: {type: "enabled"\|"disabled", clear_thinking: false}` | The GLM reasoning shape. `openclaw/packages/ai/src/providers/openai-completions.ts:830-833`; type at `:737-740` | neutral — `glm_thinking` in DESIGN §4.3 |
| `reasoning_effort: "max"` | A level OpenAI does not define. `openclaw/extensions/zai/index.ts:142-156` | neutral — DESIGN §10.2 clamps into the declared level set, so `max` must be *in* the catalog's level list or it is dropped |
| `tool_stream: true` | Top-level flag for streaming tool-call deltas. `openclaw/packages/ai/src/providers/openai-completions.ts:796-798`; documented at `openclaw/packages/llm-core/src/types.ts:465` | **opaque** |
| `GET https://api.z.ai/api/monitor/usage/quota/limit` | Non-standard quota endpoint. `openclaw/src/infra/provider-usage.fetch.zai.ts:70` | **opaque** — but see §6.2, dorang could consume it |
| `finish_reason: model_context_window_exceeded` | Non-standard terminal value, surfaced as error text. `openclaw/packages/ai/src/utils/overflow.ts:65` | COMPATIBILITY §4 needs a row |

**`do_sample` is not used anywhere in openclaw** — a grep returns nothing. It is documented
in Zhipu's public API; no client in this survey sends it. Recorded so the absence is not
mistaken for an oversight.

#### MiniMax

Text chat is **Anthropic-shaped**: `https://api.minimax.io/anthropic`,
`https://api.minimaxi.com/anthropic` (`openclaw/extensions/minimax/model-definitions.ts:5-7`),
which confirms DESIGN §4.3's `minimax: { api: anthropic-messages }` mapping.

| Field or route | Behavior | Class |
|---|---|---|
| `base_resp: {status_code, status_msg}` | **An error envelope returned with HTTP 200.** `openclaw/extensions/minimax/tts.ts:112-122`; the shared assertion at `openclaw/extensions/minimax/music-generation-provider.ts:79-85` throws when `status_code != 0` | **gateway-owned** — see below |
| `POST /v1/coding_plan/search` | Vendor search, body `{q}` not `{query}`. `openclaw/extensions/minimax/src/minimax-web-search-provider.runtime.ts:30-31`, `:145` | **opaque** |
| `GET /v1/token_plan/remains` | Quota. `openclaw/src/infra/provider-usage.fetch.minimax.ts:31-32` | **opaque** |
| `POST /v1/t2a_v2`, `/v1/image_generation`, `/v1/video_generation`, `/v1/query/video_generation`, `/v1/files/retrieve`, `/v1/music_generation` | Media. `openclaw/extensions/minimax/{tts,image-generation-provider,video-generation-provider,music-generation-provider}.ts` | **opaque** |
| Mixed-protocol SSE | *"Legacy MiniMax (M2.x) Anthropic-compatible streaming endpoint returns reasoning_content in OpenAI-style delta chunks … rather than the native Anthropic thinking block format"* — `openclaw/src/llm/providers/stream-wrappers/minimax.ts:90-96` | structural hazard |

The `base_resp` envelope deserves the **gateway-owned** classification rather than opaque,
because relaying it verbatim is itself the defect. A caller receives `200 OK` with a body that
means "quota exceeded". openclaw's own comment records what happens without the check:
*"Without this check, quota/billing errors with placeholder audio are silently accepted"*
(`openclaw/extensions/minimax/tts.ts:115-117`). For dorang this is a **metering and fallback**
problem, not just a client problem: §7.6 classifies failures by HTTP status, and a
quota-exhausted response that arrives as `200` is neither retried nor recorded as an outage,
while §8 prices it as a successful request.

Also note `openclaw/src/infra/provider-usage.fetch.minimax.ts:152-153`: *"MiniMax usage
endpoints misname these: values are remaining quota, not consumed."* DESIGN §6.2 combines
provider-reported quota with local metering; combining a *remaining* value as if it were a
*used* value inverts the sign.

The mixed-protocol SSE row is a direct threat to the state machine COMPATIBILITY 6.6 calls
"the most intricate in the whole gateway": a stream that is Anthropic-framed but carries
OpenAI-shaped deltas satisfies neither adapter's assumptions.

#### Anthropic

| Field | Behavior | Class |
|---|---|---|
| `cache_control: {type: "ephemeral", ttl: "1h"}` | A TTL on a cache breakpoint. `openclaw/packages/ai/src/transports/anthropic-payload-policy.ts:18`, `:74-78` | **opaque** — `CapCacheBreakpoints` models the breakpoint, not its TTL |
| `redacted_thinking: {data}` | The opaque payload replayed for a withheld reasoning block. `openclaw/packages/ai/src/providers/anthropic.ts:1410-1417` | **opaque**; `canonical.Thinking.Redacted` exists (`internal/canonical/message.go:133-134`) but the *data* has nowhere to live |
| `usage` → derived **`contextUsage`** | Not a wire field: openclaw computes a per-iteration context reading from Anthropic usage and stores it as `contextUsage {promptTokens, totalTokens}` (`openclaw/packages/ai/src/providers/anthropic.ts:769-791`) | see §B.2 B12 |
| `context_management: {edits: [...]}` + `anthropic-beta: context-management-2025-06-27` | **Server-side context editing on `/v1/messages`.** See A.4a | **opaque** |
| `cache_edits` | A companion body field for cache editing, added under its own beta header (`claude-code/src/services/api/claude.ts:1673-1687`, `:1700-1708`) | **opaque** |

#### A.4a `context_management` on `/v1/messages` — the highest-risk instance

This is the same field name as OpenAI's (§A.1a) with a completely different schema, on a
different vendor, on the surface COMPATIBILITY §6 calls "the highest-risk". Two strategies:

```ts
// claude-code/src/services/compact/apiMicrocompact.ts:34-61
export type ContextEditStrategy =
  | {
      type: 'clear_tool_uses_20250919'
      trigger?: { type: 'input_tokens'; value: number }
      keep?: { type: 'tool_uses'; value: number }
      clear_tool_inputs?: boolean | string[]
      exclude_tools?: string[]
      clear_at_least?: { type: 'input_tokens'; value: number }
    }
  | {
      type: 'clear_thinking_20251015'
      keep: { type: 'thinking_turns'; value: number } | 'all'
    }
export type ContextManagementConfig = { edits: ContextEditStrategy[] }
```

Wire-up, at `claude-code/src/services/api/claude.ts:1715-1722`, puts it on the request body
**only when the beta header is in the request's beta list**:

```ts
...(contextManagement &&
  useBetas &&
  betasParams.includes(CONTEXT_MANAGEMENT_BETA_HEADER) && {
    context_management: contextManagement,
  }),
```

with `CONTEXT_MANAGEMENT_BETA_HEADER = 'context-management-2025-06-27'`
(`claude-code/src/constants/betas.ts:7`). And it **round-trips on the response**:
`claude-code/src/utils/messages.ts:400` and `:763` both carry
`context_management` on the assistant message, defaulting to `null`.

Four things make this the single hardest row in §A for dorang:

1. **It is a field/header pair.** Forwarding the body field while dropping
   `anthropic-beta: context-management-2025-06-27` produces a request the server will reject or
   ignore. Forwarding the header without the field is harmless but pointless. They are one unit.
2. **It sits on `/v1/messages`, which COMPATIBILITY §6 says is "an adapter, not a passthrough"**
   because Anthropic-shaped requests very often target OpenAI-shaped backends. So this field
   *will* be decoded, and there is no OpenAI-family equivalent to fold it onto. Its absence
   from a converted request is a structural loss with no construct id.
3. **It appears on the response too**, so the streaming adapter's terminal-message assembly
   (COMPATIBILITY 6.6) has a field to carry that no other family has.
4. **The name collides with OpenAI's.** `context_management` is an array of `{type, ...}` on
   Responses (§A.1a) and an object `{edits: [...]}` on Messages. A neutral representation that
   models it once, by name, would be wrong for one of them. It must stay in `Extra`, keyed by
   family.

The default values also record a real operating point:
`DEFAULT_MAX_INPUT_TOKENS = 180_000` and `DEFAULT_TARGET_INPUT_TOKENS = 40_000`
(`claude-code/src/services/compact/apiMicrocompact.ts:15-16`), with the comment
*"Keep last 40k tokens like client-side"*. Tool clearing is gated to internal users
(`:88-92`), so a public deployment may not see it — which is a reason to treat it as opaque
rather than to model it.

`contextUsage` is the second independent confirmation of §A.2a's finding. openclaw's compaction
trigger prefers it over `totalTokens`:

```ts
// openclaw/packages/agent-core/src/harness/compaction/compaction.ts:156-162
export function calculateContextTokens(usage: Usage): number {
  if (usage.contextUsage?.state === "available") {
    return usage.contextUsage.totalTokens;
  }
  return usage.totalTokens || usage.input + usage.output + usage.cacheRead + usage.cacheWrite;
}
```

Two vendors, two clients, same conclusion: **`total_tokens` is not the number that decides when
to compact, and the number that does is vendor-specific.**

**`/v1/messages/count_tokens` is not used anywhere in openclaw.** A grep returns no
provider-code hits; all pre-flight accounting is a character heuristic plus the last turn's
`usage`. dorang serves that route as T0 (COMPATIBILITY §0), which is correct for SDK
compatibility — but no surveyed client calls it, which is evidence about how much §D.2's
estimator has to carry.

### A.5 Cross-vendor patterns

Independent of vendor, six shapes recur, and they are what a passthrough engine has to be
built for:

1. **A server-side conversation handle the client echoes.** `previous_response_id` (Codex WS,
   vLLM, openclaw's ChatGPT path at `openclaw/packages/ai/src/providers/openai-chatgpt-responses.ts:1429`),
   `store`. dorang already has `responses_store` for this (§9.2).
2. **An encrypted blob the client echoes.** `reasoning.encrypted_content` (OpenAI, xAI —
   `openclaw/extensions/xai/stream.ts:39-41` requests it for *every* reasoning-capable xAI
   model), `Compaction { encrypted_content }`, Anthropic `thinking.signature` and
   `redacted_thinking.data`.
3. **A sticky routing token in a header.** `x-codex-turn-state`, the `x-grok-*` family,
   `chatgpt-account-id`, `session_id`
   (`openclaw/packages/ai/src/providers/openai-chatgpt-responses.ts:1660-1682`).
4. **A cache-partitioning key or policy.** `prompt_cache_key`, `prompt_cache_retention`,
   `cache_salt`, `cache_control.ttl`.
5. **A usage extension the client's own control loop reads.** `context_details`,
   `contextUsage`, `cost_in_usd_ticks`, `cache_creation_input_tokens`,
   `prompt_cache_hit_tokens` (`openclaw/packages/ai/src/providers/openai-completions.ts:1364-1373`),
   `usage.cost` (`:1395`).
6. **An envelope that reports failure with a success status.** MiniMax `base_resp`;
   z.ai accepting an overflow silently and reporting it only in `usage`
   (`openclaw/packages/ai/src/utils/overflow.ts:152-182`, case 2).

None of the six is in any OpenAI-compatible specification. All six break silently, and the
sixth breaks dorang's own failure classification rather than the caller's.

---

## B. What must pass through opaquely, and why

### B.1 A third category, beside droppable and structural

`internal/canonical/capability.go:84-91` splits capability into `Structural` and `Droppable`,
and `:77-83` states the rule: a missing droppable capability means a knob was not applied and
dorang says so; a missing structural capability means dorang would return `200` having thrown
information away.

**Opaque state is a third category, and the split is on a different axis.** Structural loss is
about what a protocol *can express*. Opaque state is about what dorang is *entitled to
touch*. A blob can be perfectly expressible — it is a JSON string — and still be something
dorang must not normalize, reformat, re-key, or synthesize, because its meaning lives on the
server that minted it.

The operative test, stated so it can be applied to a field dorang has never seen:

> A field is **opaque state** if the value's correctness is established somewhere other than
> in dorang, and dorang cannot re-derive it. Integrity-protected content, server-side handles,
> and cache partition keys all qualify. The consequence is that the only correct handling is
> byte-identical relay, and the only correct failure is refusal.

DESIGN §10.2 already applies exactly this rule to one instance — "reasoning blocks that carry
integrity material are stored as opaque handles keyed by response id, and replayed
byte-identically … dorang never fabricates or re-signs one" (DESIGN.md:1180-1182). §B
generalizes it.

### B.2 The list

| # | Item | Why opaque | What breaks if dorang normalizes it |
|---|---|---|---|
| B1 | `reasoning.encrypted_content` — `codex/codex-rs/protocol/src/models.rs:835-847`; requested by `include` at `codex/codex-rs/core/src/client.rs:888` | Server-encrypted; no client-side validation exists anywhere in the Codex tree | Dropped: turn 2 of a tool loop loses reasoning continuity, and the failure lands on the second turn, which DESIGN.md:1173-1174 already names as the worst place to find it. **Forwarded to a different model family: a hard 400.** grok has the exact error text — `"Could not decrypt the provided encrypted_content. Ensure the value is the unmodified encrypted_content from a previous response."` (`grok/crates/codegen/xai-grok-sampling-types/src/error.rs:748`) — classified non-retryable at `:756`, with the cause documented at `:211-214`: *"contains `encrypted_content` from a different model family that the current model cannot decrypt. Never retryable — the user must start a new session."* |
| B2 | `Compaction { id, encrypted_content: String }` — `codex/codex-rs/protocol/src/models.rs:1003-1012`. **`encrypted_content` is not optional.** Also `ContextCompaction { encrypted_content: Option<String> }` at `:1015-1025` | It *is* the compacted transcript. Everything before the compaction boundary exists only inside it | Dropped or rewritten: the conversation loses its entire history before the boundary and the model answers from a fragment. dorang cannot regenerate it — it is produced by `/responses/compact` on a credential-gated backend |
| B3 | `CompactionTrigger {}` — `codex/codex-rs/protocol/src/models.rs:1013-1014`, pushed into `input` at `codex/codex-rs/core/src/compact_remote_v2_attempt.rs:71` | The source comment is explicit: *"Compaction triggers are request controls, not durable response items."* | A gateway that validates `input[]` against a known item-type enum rejects it, or a gateway that persists items into `responses_store` stores a control as history. Both break remote compaction v2, which rides **in-band on ordinary `/responses`** — there is no route to key off |
| B4 | `x-codex-turn-state` — set from the response at `codex/codex-rs/codex-api/src/sse/responses.rs:62-69`, echoed on the next request at `codex/codex-rs/core/src/client.rs:1897-1903`, documented at `:1886` as *"sticky routing token captured earlier in the turn"*; also returned by `/responses/compact` (`codex/codex-rs/codex-api/src/endpoint/compact.rs:58-65`) | A backend-internal affinity token. Stored in a `OnceLock` — set once per turn, never modified | Stripped: backend affinity is lost mid-turn. Forwarded to a *different* deployment than the one that minted it: at best meaningless, at worst routed somewhere stale. Note COMPATIBILITY 7.3 requires stripping six authentication headers before forwarding — this one must survive, so "strip inbound client headers" cannot be a blanket rule |
| B5 | `previous_response_id` — `codex/codex-rs/codex-api/src/common.rs:306-307` (WebSocket body); `vllm/entrypoints/openai/responses/protocol.py:161` | A server-side handle into conversation state dorang does not hold | Already handled: `responses_store` exists precisely for this (§9.2, DESIGN.md:1001-1007). The new finding is the **field-stability requirement**: Codex only sends the input *delta* when the surrounding request fields match, compared field-by-field at `codex/codex-rs/core/src/client.rs:307-360`, with an explicit warning at `:304-306` that the comparison is deliberately not `PartialEq`. dorang injecting a parameter through §10.3's `params.set{}`/`params.default{}` changes the request the server sees between turns, against a response id minted under the old shape |
| B6 | `previous_input_messages` (Harmony format) — `vllm/entrypoints/openai/responses/protocol.py:263-267`, *"this cannot be used in conjunction with previous_response_id"* | A vendor-private message encoding | Cross-protocol conversion mangles it. It is also a mutual-exclusion constraint dorang must not violate by adding the other field |
| B7 | `prompt_cache_key` — `codex/codex-rs/core/src/client.rs:483-487` (defaults to the session id); `vllm/entrypoints/openai/responses/protocol.py:205-212` (accepted and ignored) | A cache partition identity chosen by the caller | Rewritten or dropped: cache hit rate collapses with **no error and no signal**, and DESIGN §8's cost engine then prices full-rate tokens that should have been cache reads. Worse, dorang *generating* one — say from its own session key — silently repartitions a cache the caller was managing |
| B8 | `cache_salt` — `vllm/entrypoints/openai/responses/protocol.py:244-254`, described as an anti-guessing measure for multi-user environments | A **security** parameter, not a performance one | Dropped: the caller loses prompt isolation they believe they have. Injected by dorang: cross-tenant reuse of shared system prompts is destroyed, a direct throughput cost. VLLM.md §4 already says this must be an explicit per-provider option, off by default; §B is the reason it must also never be *removed* |
| B9 | `client_metadata` — `codex/codex-rs/codex-api/src/common.rs:251-275`, keys at `codex/codex-rs/core/src/responses_metadata.rs:27-42` | Carries `compaction`, `turn_id`, thread lineage, and W3C `traceparent`/`tracestate` (`codex/codex-rs/codex-api/src/common.rs:23-24`) | A gateway that decodes to a known-field struct and re-encodes drops it. Trace context is lost, and so is the client's own record of whether this turn was a compaction |
| B10 | Response item `id`, and the prefixed-id rule — `codex/codex-rs/core/src/client.rs:927-933` strips any id that is not `<nonempty>_<nonempty>` (`codex/codex-rs/protocol/src/response_item_id.rs:36-39`) | The prefix distinguishes server-minted ids from client-local ones | dorang rewriting ids — for example while normalizing tool-call ids per COMPATIBILITY 5.4 — can make a client-local id look server-minted, or destroy a server one. Tool-name shortening (COMPATIBILITY 5.3) already has a round-trip mapping; ids need the same discipline or none at all |
| B11 | `x_search` and peers, injected as raw JSON *after* serialization — `grok/crates/codegen/xai-grok-sampler/src/client.rs:1192`, `:1733` | Deliberately outside the typed tool schema | A gateway that decodes `tools[]` into `canonical.Tool` and re-encodes drops it. grok itself has to defend against the mirror problem — the API echoes `tools` back and its own deserializer fails, so it strips unknown tools and retries (`grok/crates/codegen/xai-grok-sampler/src/client.rs:84-87`) |
| B12 | `usage.context_details` — `grok/crates/codegen/xai-grok-sampler/src/client.rs:189-194` | A live-context reading that intentionally disagrees with cumulative `total_tokens` | See §A.2a. Overwritten: **the caller's compaction fires at the wrong time**, and nothing in the response says so. This is the one item where normalizing a usage field changes control flow rather than a number on an invoice |
| B13 | `usage.cost_in_usd_ticks` — `grok/crates/codegen/xai-grok-sampling-types/src/types.rs:535-549` | Vendor-authoritative price for a request | Dropped: DESIGN §8 falls back to rule-based pricing when the backend already told the truth. Note the tick unit (1 USD = 1e10) and that `0` means unbilled, not free |
| B14 | `comp_hash` — `codex/codex-rs/protocol/src/openai_models.rs:418-420`, *"Opaque identifier for compaction-compatible model configurations"*; a change forces compaction at `codex/codex-rs/core/src/session/turn.rs:953-955` | An opaque server-side identity for "would a compaction made under configuration A still be valid under configuration B" | dorang's alias layer (§7.2) can map one client-facing name onto several real deployments. If `GET /v1/models` reports one `comp_hash` for an alias that fronts two models with different ones, the client either compacts needlessly or fails to compact when it must |
| B15 | Models `ETag` / `X-Models-Etag` — `codex/codex-rs/codex-api/src/endpoint/models.rs:64-68`; **pushed on the inference stream** at `codex/codex-rs/codex-api/src/sse/responses.rs:41-45` | A cache validator for the model catalog | dorang's `GET /v1/models` rewrites the list (COMPATIBILITY 7.4 filters by the calling key's allow-list), so forwarding the upstream ETag would validate a body dorang did not send. The stream-pushed one is a different problem: it arrives inside SSE frames the §7.2 scanner passes through, which is correct, but it invites a client to invalidate a catalog dorang controls |
| B16 | Anthropic `thinking.signature` — already modelled at `internal/canonical/message.go:129-132` | Integrity material | Already correct. Recorded here so the list is complete, and because grok's Messages path produces it (`grok/crates/codegen/xai-grok-sampler/src/stream/messages.rs:335-344`) |
| B17 | `context_management: [{type: "compaction", compact_threshold}]` — `openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:409-416` | A nested request-body structure that instructs the *server* to compact | Dropped by a decode/re-encode adapter: the caller gets no compaction and no error, and their conversation overflows on a later turn — the failure appears far from the cause. Also couples to `store: true` (`:296-312`), so dropping `store` disables it as a side effect |
| B18 | `redacted_thinking.data` — `openclaw/packages/ai/src/providers/anthropic.ts:1410-1417` | The opaque payload of a reasoning block whose text the provider withheld | `canonical.Thinking` models `Redacted bool` (`internal/canonical/message.go:133-134`) but has no field for the data. A round trip through `CanonicalRequest` therefore reconstructs the block **without** its payload, which is the same class of defect §10.2 already fixed for signatures |
| B19 | `usage.contextUsage` (Anthropic, derived) and `usage.context_details` (xAI, native) | The live context reading, distinct from cumulative totals | See B12. openclaw prefers it over `totalTokens` when computing the compaction trigger (`openclaw/packages/agent-core/src/harness/compaction/compaction.ts:156-162`). Two independent clients, two vendors, same behavior |
| B20 | `prompt_cache_retention: "24h"` — `openclaw/packages/ai/src/transports/openai-responses-transport.ts:2131-2132` | A cache **policy**, not just a key | Dropped: cache entries expire on the default schedule and the caller's cost model is wrong. Injected: dorang extends retention the caller did not ask for, on a surface where retention may be billed |
| B21 | `cache_control.ttl: "1h"` — `openclaw/packages/ai/src/transports/anthropic-payload-policy.ts:18`, `:74-78` | Same, on the Anthropic breakpoint | `CapCacheBreakpoints` (`internal/canonical/capability.go:27-28`) models *that* a breakpoint exists, not its TTL. Crossing a breakpoint into a protocol without one is already a structural downgrade; crossing one *with* breakpoints but no TTL concept silently changes the cache economics |
| B22 | Anthropic `context_management: {edits: [...]}` **plus** `anthropic-beta: context-management-2025-06-27`, echoed on the response — `claude-code/src/services/compact/apiMicrocompact.ts:34-61`; `claude-code/src/services/api/claude.ts:1715-1722`; `claude-code/src/constants/betas.ts:7`; response at `claude-code/src/utils/messages.ts:400`, `:763` | A server-side context-editing program, versioned by strategy name (`clear_tool_uses_20250919`, `clear_thinking_20251015`) | Field dropped: the server does not edit, and the caller's conversation grows past a limit they arranged not to hit. **Header dropped but field kept: the request is rejected or the field ignored** — they are one unit (§A.4a). Response field dropped: the caller cannot tell whether an edit happened, on the very turn it happened. And because this is on `/v1/messages`, COMPATIBILITY §6's adapter path decodes it — so it needs a construct id and a `400` under §10.1, not silent omission |
| B23 | `cache_edits` — `claude-code/src/services/api/claude.ts:1673-1687`, `:1700-1708`, under its own beta header | Cache-editing program | Same field/header coupling as B22 |

### B.3 The routing consequence nobody states

Items B1 and B14 together produce a constraint that is not in DESIGN today:

**Once a conversation contains opaque state minted by a specific deployment, that conversation
is pinned to a deployment that can accept it.** Not "prefers", per §7.4a's cache affinity —
*pinned*. The evidence is grok's classification of the cross-family case as never retryable
(`grok/crates/codegen/xai-grok-sampling-types/src/error.rs:211-214`), which means fail-back
under §7.6 does not help: retrying on a sibling deployment produces the identical 400.

DESIGN §10.2 already reaches the right answer for the one case it considers — "if a caller
echoes a block dorang cannot produce for the selected backend, capability routing prefers a
backend that can. If none exists, dorang fails with `400`" (DESIGN.md:1183-1185). §B says the
same rule must apply to **every** item in B.2 that is model-family-scoped, and that the
consequence for §7.6 is a new non-fallback cause, not merely a routing preference.

---

## C. Compaction as a first-class concern

DESIGN §10.5a settles the gateway question: **dorang does not compact.** This section supplies
the evidence behind that, which is not the argument one might expect. The clients that compact
are not sloppy about it. They are extremely careful — and the care is exactly what a gateway
cannot replicate, because all of it depends on knowing things only the agent loop knows.

### C.1 When clients compact

Every threshold found, with its citation:

| Client | Trigger | Citation |
|---|---|---|
| Codex | **90% of the resolved context window**, derived when the server does not send a limit, and used as a *clamp* when it does | `codex/codex-rs/protocol/src/openai_models.rs:459-470`; the doc comment at `:413-415` — "core derives it from `context_window` (90%). When provided, core clamps it to 90%" |
| Codex | **95% effective window** applied first, reserving headroom for system prompt, tool overhead, and output | `codex/codex-rs/protocol/src/openai_models.rs:355-357`; applied at `codex/codex-rs/core/src/session/turn_context.rs:220-227` |
| grok | **85%**, shared by two harnesses | `grok/crates/common/xai-grok-compaction/src/code_compaction/config.rs:13`; session default at `grok/crates/codegen/xai-grok-agent/src/compaction.rs:39` |
| grok | **speculative pass starts 10 points earlier** (75%) | `grok/crates/codegen/xai-grok-shell/src/session/compaction.rs:38`, arithmetic at `:220-222` |
| hermes | **50% default**, raised to **75% for windows under 512K**, raise-only | `hermes/agent/context_compressor.py:1528`; `:346-347`; `:1480-1482` |
| hermes | **85%** for Codex gpt-5.4/5.5/5.6, **70%** for gpt-5.3-codex-spark, **75%** for one Arcee model | `hermes/agent/auxiliary_client.py:357`, `:366`, `:464-465` |
| jikji | **90%** default soft trigger; band `(0.8, 0.95]` for cache-aware deferral | `jikji/internal/foundry/config_jikjicode_runtime.go:6`; `jikji/internal/compositor/conversation/context_policy.go:9`, `:12` |
| jikji | **80%** for the observation-budget contraction and the model-facing usage directive | `jikji/internal/forme/context_observation_budget.go:88`; `jikji/cmd/jikjicode/runner_02_reduceProjectedOuterConversation.go:706` |
| openclaw | **absolute reserve, not a percentage**: compact when `contextTokens > contextWindow − 16384` | `openclaw/packages/agent-core/src/harness/compaction/compaction.ts:148-153`, `:249-260` |
| openclaw | reserve floor raised to **20,000** at the agent layer, capped so the prompt keeps at least `max(8_000, 0.5 × window)` | `openclaw/src/agents/agent-settings.ts:9`; `openclaw/src/agents/agent-compaction-constants.ts:6`, `:12`; cap at `openclaw/src/agents/agent-settings.ts:48-61` |
| openclaw | server-side `compact_threshold` = **70% of the window**, or 80,000 when unknown | `openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:288-294` |
| openclaw | a second, **byte-based** trigger on transcript size, independent of tokens | `openclaw/src/auto-reply/reply/agent-runner-memory.ts:860-863`, labelled `"transcript_bytes"` at `:923` |
| Claude Code | `tokens ≥ (window − min(modelMaxOutput, 20_000)) − 13_000`. Its own comment computes this as **~93% of effective** | `claude-code/src/services/compact/autoCompact.ts:30`, `:33-48`, `:62`, `:72-76`; the 93% figure at `:200-206` |
| Claude Code | proactive precompute at **80% of usable**, in the shipped binary only | `~/.local/share/claude/versions/2.1.220` (minified, one line) — `"Pds=0.2"` and `"function Mds(e,t){return Math.min(e-Math.round(e*t.precomputeBufferFraction),yfo(e,t))}"` |
| opencode v1 | `count ≥ limit.input − min(20_000, maxOutput)` — **100% of usable**, no headroom | `opencode/packages/opencode/src/session/overflow.ts:8`, `:10-20`, `:31-33` |
| opencode v2 | `estimate > context − max(output, 20_000)` | `opencode/packages/core/src/session/compaction.ts:12`, `:230-234` |
| openharness | `tokens ≥ (window − 20_000) − 13_000` — a deliberate port of Claude Code's formula | `openharness/src/openharness/services/compact/__init__.py:55-56`, `:1080-1092`; the port is stated at `:1-7` |

Two observations about this table that matter more than the numbers.

**First, no two agree, and the disagreement is principled.** hermes' comment for its
small-window rule (`hermes/agent/context_compressor.py:344-347`) explains that on a small
window a 50% trigger leaves so little reclaimed headroom that "compaction re-fires every 1-2
turns and the session spends most of its wall-clock summarizing". Codex layers a 95% *usable*
window under a 90% *trigger* because it must reserve for tool overhead it can measure and a
gateway cannot. These are tuned against each harness's own prompt structure. A gateway picking
one number would be wrong for every caller.

The absolute-reserve rules are worth separating out, because they are a *different kind* of
rule from the percentages. Claude Code, openharness, openclaw, and both opencode generations
all subtract a fixed token count rather than taking a fraction. A fixed 13,000+20,000 reserve
is ~83% of a 200K window and ~97% of a 1M one — the *effective percentage moves with the model*,
which is the opposite of what a percentage rule does. Claude Code's own comment names the
resulting figure as "~93% of effective"
(`claude-code/src/services/compact/autoCompact.ts:200-206`), which is a derived number, not a
configured one.

That two incompatible *shapes* of rule exist, not merely two numbers, is the point. A gateway
cannot express one policy that satisfies both, and the one number it would need — the reserve —
is a property of the caller's summarization step, not of the model.

Claude Code's reserve is also empirically derived in a way nothing outside the client could
reproduce: `MAX_OUTPUT_TOKENS_FOR_SUMMARY = 20_000` carries the comment *"Based on p99.99 of
compact summary output being 17,387 tokens"*
(`claude-code/src/services/compact/autoCompact.ts:28-30`). That is a measurement of *its own
summarization prompt's* output distribution. dorang has no such prompt and therefore no such
distribution.

**Second, the trigger is only ever a soft signal.** jikji states this as policy:
`jikji/docs/design/compaction-strategy.md:56-57` — *"Treat the model context window as an
independent hard limit. Cache policy, cooldowns, and soft-trigger failures must never disable
hard overflow recovery."* Soft trigger and hard limit are different mechanisms with different
owners. dorang owns the hard limit. It does not own the soft trigger.

### C.2 What they actually do

| Client | Mechanism |
|---|---|
| Codex — local | An extra `/responses` call with a summarization prompt, then a **client-side history rewrite**: `codex/codex-rs/core/src/compact.rs:347-383` builds a new history from the summary plus retained user messages. Prompt at `codex/codex-rs/prompts/templates/compact/prompt.md:1` — *"You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task."* Retained-user-message budget `20_000` tokens (`codex/codex-rs/core/src/compact.rs:56`) |
| Codex — remote v1 | `POST /responses/compact` returns a replacement history, which is then **filtered client-side** — developer messages, non-user-content user messages, all reasoning and tool items, and `CompactionTrigger` are dropped (`codex/codex-rs/core/src/compact_remote.rs:336-363`) |
| Codex — remote v2 | In-band: a `CompactionTrigger {}` item is pushed into an ordinary `/responses` request (`codex/codex-rs/core/src/compact_remote_v2_attempt.rs:71`), retained-message budget `64_000` (`codex/codex-rs/core/src/compact_remote_v2.rs:54-59`) |
| Codex — token-budget mode | **No summarization at all** — installs a fresh context window: `codex/codex-rs/core/src/compact_token_budget.rs:21-25`, `:79-85` |
| grok | Summarize a prefix, splice onto a preserved prefix; the system message is a hard precondition — compaction *fails* without one (`grok/crates/codegen/xai-grok-shell/src/session/compaction.rs:986`, `:1009`). Splice logic at `:473-476` also de-duplicates a re-injected AGENTS.md |
| hermes | Five phases, verbatim at `hermes/agent/context_compressor.py:4020-4025`: prune old tool results (no LLM call) → protect head → find tail by token budget → summarize the middle → iteratively update the previous summary. Tool-result pruning replaces content with `"[Old tool output cleared to save context space]"` (`:307`) and de-duplicates repeated reads (`:1906-1915`) |
| jikji | A cascade, least-lossy first: elide stale **tool observations** in place, preserving the two most recent rounds (`jikji/internal/forme/context_compactor_cascade.go:17-19`, `:212-213`); only then evict whole provider rounds; system messages are score-immune (`jikji/internal/forme/context_guard.go:743-748`) |
| openclaw | LLM summarization into six fixed sections — Goal, Constraints & Preferences, Progress, Key Decisions, Next Steps, Critical Context (`openclaw/packages/agent-core/src/harness/compaction/compaction.ts:490-521`), iteratively updated against `<previous-summary>` on re-compaction (`:523-560`). Cut point chosen by walking back until `keepRecentTokens` accumulates (`:415-484`), and **a toolResult message is never a valid cut point** (`:330-341`) |
| openclaw | A separate **route decision** before compacting: `compact_only`, `truncate_tool_results_only`, or `compact_then_truncate`, chosen from how far over budget the request is (`openclaw/src/agents/embedded-agent-runner/run/preemptive-compaction.ts:365-381`) |

The common shape across all six: **tool results are sacrificed first, the system prompt never,
and the recent tail is protected.** Three of them enforce structural invariants a gateway could
not even check — hermes never cuts inside a tool_call/result group and cleans up orphaned pairs
so the API never sees mismatched ids (`hermes/agent/context_compressor.py:3893-3894`, `:4034-4035`);
jikji refuses to retain a subset of one native provider tool batch
(`jikji/internal/forme/context_guard.go:573-576`); openclaw preserves the last N user turns
*together with their tool-call ids* (`openclaw/src/agents/agent-hooks/compaction-safeguard.ts:823-890`).

One more invariant is worth quoting because it is a security property, not a correctness one:

```ts
// openclaw/src/agents/compaction-planning.ts:56
// SECURITY: toolResult.details and runtime-context transcript entries must never enter LLM-facing compaction.
```

Compaction re-feeds a conversation to a model. Which parts of a transcript are safe to re-feed
is a judgement about the *contents*, made by the harness that produced them. A gateway holds
the bytes and none of the judgement.

### C.3 Server-side versus client-side — the precise answer

Both exist. There are **four distinct server-side mechanisms across two vendors**, and no two
are detected the same way:

| # | Vendor | Shape | Detectable by | Citation |
|---|---|---|---|---|
| 1 | OpenAI | a dedicated route, `POST /responses/compact` | path | `codex/codex-rs/codex-api/src/endpoint/compact.rs:35-37` |
| 2 | OpenAI | a sentinel item `CompactionTrigger {}` inside `input[]` on an ordinary `/responses` call | **nothing on the request line** — only `x-openai-subagent: compact`, out of band | `codex/codex-rs/core/src/compact_remote_v2_attempt.rs:71`; header at `codex/codex-rs/codex-api/src/requests/headers.rs:16-31` |
| 3 | OpenAI | `context_management: [{type: "compaction", compact_threshold}]` on an ordinary `/responses` call | body inspection | `openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:409-416` |
| 4 | Anthropic | `context_management: {edits: [...]}` on `/v1/messages`, plus the `context-management-2025-06-27` beta header, echoed on the response | body **and** header inspection | `claude-code/src/services/compact/apiMicrocompact.ts:34-61`; `claude-code/src/services/api/claude.ts:1715-1722`; `claude-code/src/constants/betas.ts:7` |

Mechanisms 3 and 4 **share a field name and share nothing else** — one is an array, the other
an object with an `edits` key, on different wire families with different semantics. That is the
strongest possible argument against modelling extension fields by name in a neutral
representation.

There is also a fifth thing that looks like a server-side compaction endpoint and is not:
**opencode exposes `POST /api/session/:sessionID/compact`**
(`opencode/packages/protocol/src/groups/session.ts:226-236`; v1 equivalent
`POST /session/:sessionID/summarize` at
`opencode/packages/opencode/src/server/routes/instance/httpapi/groups/session.ts:303-314`).
That is a **harness API, not a provider API** — behind it opencode does an ordinary LLM call
and rewrites its own history
(`opencode/packages/opencode/src/session/compaction.ts:388-402`), persisting the boundary as a
row in its own database (`opencode/packages/core/src/session/history.ts:17`, `:36-38`).

The distinction matters to dorang in one specific way: **a harness like that may sit in front
of dorang.** When it does, its compaction call arrives at dorang as an ordinary chat request
that happens to contain a summarization prompt. dorang cannot tell, must not try, and must
price and meter it like any other request. It is also a second compactor in the path, which is
§10.5a's argument again — dorang would be the third.

Everything else:

- **Server-side compaction is gated by provider identity even on OpenAI.**
  `supports_remote_compaction()` returns true only for OpenAI and Azure Responses providers
  (`codex/codex-rs/model-provider-info/src/lib.rs:422-424`); openclaw's equivalent requires
  `store: true` and `provider === "openai"`
  (`openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:296-312`).
- **Even server-side, the client rewrites the result.** The returned history is filtered
  client-side before use (`codex/codex-rs/core/src/compact_remote.rs:336-363`). Server-side
  compaction is not "the server owns the conversation"; it is "the server produces a
  candidate and the client decides".
- **grok, hermes, jikji, and vLLM are entirely client-side.** grok's Responses client sets
  `previous_response_id: None` and `store: None` unconditionally
  (`grok/crates/codegen/xai-grok-sampling-types/src/conversation.rs:2236`, `:2246`) — it is
  stateless and resends full history.
- **openclaw is both**, and it raises its own client threshold to at least the server's so the
  two do not fight (`openclaw/src/auto-reply/reply/memory-flush.ts:92-98`). That is a
  coordination problem between a client and a server that both compact. A gateway inserting a
  *third* compactor into that relationship is the argument for §10.5a in one sentence.
- **vLLM offers truncation, not compaction** (§A.3a). The distinction is not academic:
  truncation drops tokens the model then cannot see, with no summary standing in for them.

### C.4 Why the non-goal is right

DESIGN §10.5a gives the reason: a gateway rewriting a conversation changes what the model was
asked, and the caller cannot see it in the response. Three findings from this survey make that
concrete, and each is stronger than the general argument.

**1. The clients' own safety machinery is invisible to a gateway.**

jikji will not compact when an inquiry is awaiting a durable answer
(`jikji/internal/cli/code/session_rewind.go:216`), when an unsettled steering fallback is
pending (`:221`), or when session context consistency is already unsafe (`:213`). Those are
states of an agent loop. A gateway sees an HTTP request. It cannot know that discarding turn 7
strands a tool call the caller is still waiting on.

The strongest single statement of this is jikji's own server-side chat path, which faces
exactly dorang's situation and refuses:

```go
// jikji/internal/compositor/chat_context_admission.go:22-24
// admitCompleteRequest validates the exact provider-facing request copy. Press
// messages are client-owned, so this boundary deliberately rejects an oversized
// request instead of silently deleting or summarizing caller-supplied history.
```

A sibling project, reaching the same conclusion, for the same reason, at the same boundary.

**2. Compaction is not a pure function of the messages — it needs a model call and a budget.**

Codex, grok, hermes, and jikji's semantic path all issue a *second inference request* to
produce the summary. grok budgets 32,768 tokens of reserve and 300 seconds of wall clock for
it (`grok/crates/codegen/xai-grok-shell/src/session/compaction.rs:956`;
`grok/crates/codegen/xai-grok-agent/src/compaction.rs:24-26`). For dorang, that would mean
spending the caller's money, on a model the caller did not choose, inside a request the caller
thinks is one request — and then billing them for it. There is no honest way to render that on
an invoice.

**3. Compaction fails, and the failure handling is stateful across turns.**

jikji carries a persisted circuit breaker: three consecutive failed *or ineffective* attempts
open a five-minute cooldown (`jikji/internal/forme/compaction_guard_state.go:17-18`), and the
state is validated on restore so a restart cannot bypass it (`:79-82`). hermes blocks
automatic compression after two ineffective attempts
(`hermes/agent/context_compressor.py:1882-1885`). grok discards a compaction that did not
shrink by at least 20% (`grok/crates/common/xai-grok-compaction/src/intra_compaction/config.rs:121-124`).

A gateway would have to hold per-conversation compaction state, durably, across nodes, keyed
by something it does not have — because a conversation has no id in the chat-completions
protocol. DESIGN §0.2 requires that nothing be structurally single-node. Compaction state is
structurally per-conversation, and the protocol dorang mostly serves has no conversation.

**4. And the one place a gateway *could* compact is the one place it must not.**

vLLM's `truncation: "auto"` is available on every vLLM deployment and would let dorang make
oversized requests succeed with one field. It succeeds by clamping the input length so the
overflow error never fires (`vllm/entrypoints/serve/utils/api_utils.py:178-188`). That is the
precise failure §10.5a describes, available as a one-line configuration change, which is why
it is worth naming as a prohibition rather than leaving to judgement.

### C.5 What dorang does instead

Per §10.5a, unchanged, with the extension-API row given force by §A and §B:

| Situation | dorang does |
|---|---|
| Fits the target's window | dispatch |
| Does not fit; a same-class deployment with a larger window exists | route there (§7.6 `context_window`) |
| Does not fit anywhere in the class | fail, naming the real limit |
| Caller invokes a vendor's compaction API | pass it through (§10.6), do not interpret it |

The third row is where dorang adds something. Clients spend real effort trying to recover the
real limit from error text: hermes carries seven regular expressions to parse a number out of
an error message (`hermes/agent/model_metadata.py:1256-1264`), clamps the result to a sane
range (`:1270`), and — importantly — **only ever accepts a smaller number**
(`:1290-1292`), because *"Context-overflow recovery must not invent a new model window
size"* (`:1281-1285`). dorang reads `max_model_len` from `GET /v1/models` (VLLM.md §1.1) and
already knows. Putting the number in the error body replaces all of that guesswork.

---

## D. Context-window handling mechanism

If dorang never compacts, then the whole mechanism reduces to one question asked cheaply and
answered correctly: **will this request fit?** Everything else follows from the answer.

The asymmetry from DESIGN §4.3 governs the design: a window that is **too large** means
requests between the real limit and the declared one fail outright and context-window fallback
*never fires*, because dorang believes they fit. A window that is **too small** means
unnecessary routing to a bigger model — a cost, not a failure. The same asymmetry applies to
the *estimate*: **an estimate that is too low produces the failure mode fallback cannot see.**
Estimation must therefore be conservative in the same direction the catalog is.

### D.1 Where the number comes from

Ordered by authority, best first:

1. **The deployment's own declaration.** `GET {base}/v1/models → data[i].max_model_len` for
   vLLM (VLLM.md §1.1). `null` means undeclared, never zero; LoRA cards inherit from `parent`.
2. **A dated catalog entry.** `pkg/catalog/model_catalog.yaml` — `context_window` and
   `max_output_tokens`, where absent or `0` means undeclared and callers must treat that as
   unknown rather than as a bound (`pkg/catalog/model_catalog.yaml:34-35`).
3. **A prefix rule**, capability defaults only (`pkg/catalog/provider_defaults.yaml:908-920`).
4. **Nothing.** Undeclared stays undeclared; `pkg/catalog/catalog_test.go:405-407` freezes this —
   *"undeclared must stay 0, never a guess"*.

Codex demonstrates the same pattern from the client side: the window comes from the server's
`/models` table (`codex/codex-rs/protocol/src/openai_models.rs:408-424`), resolved as
`context_window.or(max_context_window)` (`:455-457`), cached against an ETag, and refreshable
mid-stream through `X-Models-Etag` (`codex/codex-rs/codex-api/src/sse/responses.rs:41-45`).
That last one is a mechanism dorang currently lacks and could adopt: a backend can tell you
its catalog changed without you polling.

`POST /tokenize` also returns `max_model_len` as a required field
(`vllm/entrypoints/serve/tokenize/api_router.py:37`; VLLM.md §1.1), and it is a valid
cross-check — but it costs a tokenization pass, so it belongs in a probe, never on the request
path.

### D.2 Pre-flight estimation without tokenizing

DESIGN §15.5 prohibits reflection, regular expressions, and formatted string construction on
the hot path, and §7.4b already rejected tokenization there on latency grounds — a rejection
VLLM.md §4 independently confirms. So the estimator must be a byte-level pass, and the only
question is how wrong it is allowed to be.

**What the field actually does.** Every client surveyed estimates rather than tokenizes:

| Client | Estimator | Citation |
|---|---|---|
| Codex | `bytes/4`, rounding up | `codex/codex-rs/utils/string/src/truncate.rs:4`, `:71-74` |
| grok | `bytes/4`, **truncating** — `estimate_tokens("abc") == 0` | `grok/crates/codegen/xai-token-estimation/src/lib.rs:9`, `:17-19` |
| hermes | `chars/4` with an ASCII fast path and a CJK correction counting dense codepoints as ~1 token each | `hermes/agent/model_metadata.py:2714-2723` |
| jikji | rune-classified: 4 ASCII word runes, 2 punctuation runes, 8 space runes per token, wide symbols at 2 | `jikji/pkg/tokenestimate/estimate.go:12-16`, `:69-72` |
| openclaw | `chars/4` in the harness (`openclaw/packages/agent-core/src/harness/compaction/compaction.ts:279-293`), images at 4,800 chars (`:262`); but the **pre-flight** path is differentiated — see below |
| openharness | ports Claude Code's constants wholesale, including a hardcoded 200,000 window for known model families and a conservative default otherwise (`openharness/src/openharness/services/compact/__init__.py:1065-1078`) |

jikji's package comment states the trap the other three walk into:

```go
// jikji/pkg/tokenestimate/estimate.go:18-20
// Text returns a conservative, language-aware estimate for model-visible text.
// It deliberately avoids treating UTF-8 bytes as tokens: that undercounts CJK
// text while making transport byte limits look like context limits.
```

This is decisive for dorang. `bytes/4` **undercounts CJK**, and undercounting is exactly the
optimistic direction that disables fallback. dorang serves a Korean-language deployment; a
byte-only estimator would systematically under-report the workload most likely to overflow.
grok's truncating division makes it worse — it rounds every fragment toward zero.

**openclaw's pre-flight estimator is the one worth copying**, because it is the only surveyed
implementation built specifically for the admission decision rather than for a compaction
trigger, and it independently arrives at the design below:

```ts
// openclaw/src/agents/embedded-agent-runner/run/preemptive-compaction.ts:26-32
const ESTIMATED_CHARS_PER_TOKEN = 4;
const TOOL_RESULT_CHARS_PER_TOKEN = 2;
const JSON_PAYLOAD_CHARS_PER_TOKEN = 3;
const MESSAGE_BOUNDARY_OVERHEAD_TOKENS = 12;
const CONTENT_BLOCK_OVERHEAD_TOKENS = 6;
const IMAGE_BLOCK_TOKENS = 2_000;
const TRUNCATION_ROUTE_BUFFER_TOKENS = 512;
```

Three things it does that a naive `chars/4` does not:

- **Different densities for different content.** Tool results at 2 chars/token and JSON at 3
  reflect that structured text tokenizes far denser than prose. dorang's traffic is agentic, so
  most bytes are tool results and JSON — the two cases a flat divisor gets most wrong, in the
  undercounting direction.
- **Explicit framing overhead**, 12 tokens per message boundary and 6 per content block, which
  is the same correction jikji applies (`jikji/internal/forme/context_guard.go:781-790`,
  `jikji/internal/compositor/conversation/history.go:138`) arrived at independently.
- **A blanket safety margin applied last**: `SAFETY_MARGIN = 1.2`, commented *"Buffer for
  estimateTokens() inaccuracy"* (`openclaw/src/agents/compaction-planning.ts:19-21`), applied at
  `openclaw/src/agents/embedded-agent-runner/run/preemptive-compaction.ts:273`.

That 20% margin against jikji's 5% is a real disagreement, and the direction is informative:
jikji's 5% sits **on top of a real tokenizer** (`jikji/internal/typebackbone/token_estimate.go:29-47`),
openclaw's 20% on top of a character heuristic. dorang, having no tokenizer on the hot path,
belongs nearer the 20% end — but the margin is a **configuration value with a measured
default**, not a constant, because §D.2's closing point is that the error becomes measurable as
soon as the first response's usage arrives.

**What is actually feasible, and what it costs.** dorang already streams the request body and
hashes it incrementally at 4 KiB byte boundaries for prefix routing (§7.4b, DESIGN.md:826-830).
The estimator rides that same pass:

- Classify bytes, not runes, in the common case: ASCII bytes are already separated into word,
  punctuation, and whitespace classes by a 256-entry lookup table — no branching on rune
  decode, no allocation, no regex.
- Count non-ASCII bytes separately. Every continuation byte (`0b10xxxxxx`) is skipped, so the
  count is of *codepoints*, and each non-ASCII codepoint is charged at least one token, per
  jikji's rule. This costs one comparison per byte.
- Add a per-message framing surcharge. jikji charges one token per serialized message boundary
  (`jikji/internal/forme/context_guard.go:781-790`) and 16 tokens per tool exchange
  (`jikji/internal/compositor/conversation/history.go:138`). dorang can count message
  boundaries during the same scan it already does.
- Add a margin. jikji applies 5% even on top of a real tokenizer
  (`jikji/internal/typebackbone/token_estimate.go:29-47`) and still reports `Exact: false`,
  with the reason stated at `:41-43`: *"Even an exact text tokenizer cannot exactly model every
  provider's private chat/tool framing."*

The result is one pass over bytes dorang is already reading, with a table lookup and two
counters. That is well inside §15.5.

**The estimate is never authoritative, and the design must say which reading wins.** jikji's
ordering is the right one: provider-reported usage beats a provider-native tokenizer beats the
fallback estimate (`jikji/pkg/tokenestimate/estimate.go:1-3`;
`jikji/cmd/jikjicode/live_context_usage.go:85-91` marks provider-reported readings
`Authoritative = true`). For dorang that means the estimator is used **only** for the admission
decision on a request that has not been sent yet. Once a response arrives, the usage numbers
of §10.7 are the truth, and the estimator's error is measurable — which makes it tunable
against real traffic instead of guessed at.

**A conformance obligation.** Because the failure direction is asymmetric, the estimator needs
a test that asserts it does not *under*-estimate on a corpus that includes CJK, long tool-result
JSON, and base64 image payloads. hermes charges a flat 1,600 tokens per image
(`hermes/agent/context_compressor.py:316`) while grok charges 765
(`grok/crates/codegen/xai-token-estimation/src/lib.rs:13`) — a 2× disagreement between two
production clients, and hermes' own docstring says 1,500 where its constant says 1,600
(`hermes/agent/model_metadata.py:2729-2730`). Nobody knows this number. dorang should take the
larger value it can justify and mark the choice **UNVERIFIED** in the catalog rather than
picking a middle.

### D.3 Relationship to §7.6's `context_window` fallback

§7.6 lists the cause as detected by "pre-computed, or a 400 signature". With §D.1 and §D.2 the
decision becomes explicit:

| Condition | Action |
|---|---|
| `estimate + max_tokens ≤ declared window` | dispatch |
| `estimate + max_tokens > declared window`, a same-class deployment has a larger declared window | route there — **before dispatching**, not after a 400 |
| Window undeclared for every candidate | dispatch and rely on the 400 signature; record the deployment on `UnverifiedModels()` |
| Over the limit everywhere in the class | **fail with `400`**, naming the real limit, the estimate, and the deployment it was measured against |
| Conversation carries opaque state pinned to one family (§B.3) | **no fallback**; fail if that family cannot take it |

Two rules the table implies and that are easy to get wrong:

- **`max_tokens` counts against the window.** vLLM computes `max_model_len − input_length` per
  request (`vllm/entrypoints/serve/utils/api_utils.py:189`) and VLLM.md §1.1 says dorang must
  not synthesize a max-output value. So the admission test uses the caller's requested output,
  and when there isn't one, dorang must not invent a large default just to have a number.
- **The backstop signature stays.** VLLM.md §2 fixes it: match the substring
  `"maximum context length"`, never a full sentence. hermes' 28-pattern list
  (`hermes/agent/error_classifier.py:261-300`) shows what happens without a pre-computed path —
  and its own comment at `:272-273` flags that bare `"max_tokens"` in that list is load-bearing
  for a *different* recovery path, which is the kind of coupling substring matching creates.

#### D.3a Some backends do not report overflow at all

The backstop signature has a hole, and it is large enough to change the design. openclaw's
overflow detector has **three cases, and only the first is an error**:

```ts
// openclaw/packages/ai/src/utils/overflow.ts:152-182
// Case 1: Check error message patterns
// Case 2: Silent overflow (z.ai style) - successful but usage exceeds context
if (contextWindow && message.stopReason === "stop") {
  const inputTokens = resolveContextInputTokens(message);
  if (inputTokens !== undefined && inputTokens > contextWindow) { return true; }
}
// Case 3: Length-stop overflow (Xiaomi MiMo style) - server truncates oversized input
// to fit the context window, leaving no room for output.
if (contextWindow && message.stopReason === "length" && message.usage.output === 0) {
  … inputTokens >= contextWindow * 0.99 …
}
```

The documented provider behavior behind those cases, at
`openclaw/packages/ai/src/utils/overflow.ts:30-42` and `:113-125`:

- **z.ai** — *"May return … (code 1210), … (code 1261), or accept overflow silently"*, and
  *"Sometimes accepts overflow silently (detectable via usage.input > contextWindow), sometimes
  returns rate limit errors instead"*. A **rate-limit error that is actually an overflow** would
  send §7.6 down the `rate_limit` chain, retrying on a sibling deployment that fails identically.
- **Xiaomi MiMo** — *"Truncates input to fill contextWindow exactly, then returns finish_reason
  'length' with output=0"*.
- **Ollama** — *"Some deployments truncate silently, others return errors"*.

And this is the same behavior class as vLLM's `truncation: "auto"` (§A.3a): the server silently
shortens the conversation and returns `200`. What §A.3a establishes is that dorang must never
*request* it. What §D.3a establishes is that **dorang cannot assume it is not happening
anyway**, on backends that do it unconditionally.

Three consequences:

1. **A `200` is not proof the request fit.** dorang already reads usage for metering (§12.1).
   The check `reported_input_tokens > declared_context_window` costs one comparison on a number
   dorang has in hand, and it is the only signal available for this class of backend.
2. **`finish_reason: "length"` with zero output tokens is an overflow, not a completion.**
   COMPATIBILITY §4 maps terminal reasons; this is a case where the mapping is correct on the
   wire and wrong in meaning. It belongs in the ledger and in `x-dorang-native-stop-reason`,
   the same out-of-band channel COMPATIBILITY 4.2a already uses for exactly this shape of
   problem.
3. **It is evidence about the declared window, not just about the request.** If reported input
   exceeds the declared window, the *declaration* is what is wrong — which is DESIGN §4.3's
   "too small" direction, the benign one. dorang should record it against the deployment so
   `UnverifiedModels()` has something to point at, and must **not** silently widen the declared
   window on that evidence: §4.3 is explicit that a larger value can only be adopted from a
   probe.

#### D.3b Input overflow and output-cap overflow look alike, and must not share a chain

**An input-overflow 400 and an output-cap-too-large 400 are hard to tell apart from the error
text.** hermes separates them with two signal sets — `hermes/agent/model_metadata.py:1443-1463`
for the output-cap case (`"range of max_tokens should be"`, `"available_tokens"`,
`"in the output"` alongside `"maximum context length"`, `"requested … output tokens"`) and
`:1470-1478` for the input case (`"prompt is too long"`, `"input is too long"`,
`"reduce the length"`) — and then fails fast on the output-cap case, for a reason it states
plainly at `hermes/agent/conversation_loop.py:3912-3914`:

> Routing it into compression re-sends the same max_tokens, gets the identical 400, and
> death-loops.

For dorang the equivalent hazard is routing an output-cap error into §7.6's `context_window`
fallback: the larger-window model receives the same oversized `max_tokens` and returns the same
400, burning every hop in `max_hops` and then failing anyway, slower. The two causes need
separate rows in §7.6, and the output-cap one has **no** fallback chain — the correct response
is a 400 naming the model's real max output, which dorang has in the catalog
(`pkg/catalog/model_catalog.yaml:149-152`).

Note also that the two live in the same substring space: hermes keeps bare `"max_tokens"` in
its context-overflow pattern list and flags at `hermes/agent/error_classifier.py:272-273` that
it is load-bearing for the *output-cap* recovery path. Substring matching over error prose
couples unrelated recovery paths to each other. It is a backstop, and VLLM.md §2 is right that
the pre-computed path must be preferred.

### D.4 Guard rails — what jikji's guards protect against

The file names in the brief were partly hypotheses; here is what is actually there. Of the four
named, only `jikji/cmd/jikjicode/compaction_override.go` exists as production code. There is no
production `compaction_guard.go`, `compact_runner.go`, or `context_safeguard.go` in
`jikji/cmd/jikjicode/` — those names exist **only as test files**, and the implementations live
elsewhere: the breakers in `jikji/internal/forme/compaction_guard_state.go` and
`jikji/cmd/jikjicode/runner_02_reduceProjectedOuterConversation.go:735-915`, the compactor in
the same file at `:234-295`, and the tool-result cap in `jikji/cmd/jikjicode/config.go:46-67`.
`jikji/internal/press` is **not** a compression package: `jikji/internal/press/doc.go:1` —
*"Package press owns external ingress transports such as HTTP APIs."* Its role here is
propagating child-agent compaction policy (`jikji/internal/press/run_child_policy.go:12-15`)
and a server-side session compactor (`jikji/internal/press/orchestrator.go:713`, `:1171`).
The normative statement of jikji's policy is a design document,
`jikji/docs/design/compaction-strategy.md`, cited below.

Five lessons, each with a dorang consequence.

**1. A breaker's state must be bounded, and version-checked, or a restart weaponizes it.**

```go
// jikji/internal/forme/compaction_guard_state.go:79-82
// V1 had no persisted cooldown start, so RetryAfter had no structural
// upper bound. Discard the old proactive breaker rather than trusting an
// unbounded timestamp. Overflow recovery never consults this state.
```

Validation rejects an out-of-band cooldown outright
(`jikji/internal/forme/compaction_guard_state.go:62-64`). **dorang consequence**: the same
applies to every piece of durable state dorang restores across restarts — `credential_state`,
`quota_leases`, `budget_state` (§9.2). A persisted `unavailable_until` with no structural bound
is a stored outage. Restore should validate, and reject rather than adopt, exactly as jikji's
`RestoreCompactionGuardState` resets when the identity does not match
(`jikji/cmd/jikjicode/runner_02_reduceProjectedOuterConversation.go:908-911`).

**2. A guard on the soft path must never disable the hard path.**

`jikji/internal/forme/compaction_guard_state.go:21-23` — *"It governs proactive attempts only;
overflow recovery never consults it."* Restated as policy at
`jikji/docs/design/compaction-strategy.md:56-57`. **dorang consequence**: a circuit breaker
that removes a deployment from candidacy (§7.6) must not also suppress the capacity check that
would have refused the request. Optimization state and correctness state are separate, and the
separation has to be structural rather than remembered.

**3. Manual action bypasses the automatic breaker without mutating it.**

`jikji/cmd/jikjicode/compaction_guard_persistence_test.go:54` names the invariant:
`TestManualCompactBypassesOpenOuterGuardWithoutMutatingIt`, failing with *"manual compact
mutated automatic breaker"* (`:79`). **dorang consequence**: an operator forcing a request at a
deployment in cooldown must not thereby clear the cooldown — nor extend it. §7.6's half-open
probe already has this shape; the test is worth copying.

**4. A per-item budget and a cross-item budget are different mechanisms and you need both.**

`jikji/cmd/jikjicode/config.go:46-59` caps a *single* tool result at ~1/16 of the window,
clamped to `[8 KiB, 64 KiB]`, and the comment records the failure that motivated it:
*"Previously 100 KB for a 200k window, which let a handful of results dominate the context."*
Cross-item accumulation is handled separately by the context guard, and the observation budget
contracts both limits to a quarter of remaining bytes above 80% occupancy
(`jikji/internal/forme/context_observation_budget.go:22-25`, trigger at `:88`).
**dorang consequence**: this is the same shape as §5's multi-axis reservation and §15.4's
process-wide replay budget — a per-request cap is not a bound on the aggregate. It is direct
support for the design decision already taken in §15.4, from an independent codebase.

**5. Refuse rather than proceed, and say what to do instead.**

`jikji/cmd/jikjicode/runner_01_ConfigItems.go:1022`:

```
admit provider request before call: projected input %d tokens exceeds the %d-token model
capacity and outer history could not be reduced; place large data in a file and reference
or page it instead
```

Note what that error contains: the projection, the capacity, and an action. **dorang
consequence**: this is the template for §C.5's third row. dorang's version should carry the
estimate, the real limit, the deployment the limit came from, and — because dorang has it and
the caller does not — the largest same-class window it *did* find, so the caller knows whether
a different model would have worked.

**6. A temporary override auto-reverts, is never persisted, and is rejected rather than
clamped.**

`jikji/cmd/jikjicode/compaction_override.go` is the one file from the brief that exists as
production code, and it is entirely about not letting a temporary knob become permanent:

- `:18-21` — `compactionOverrideTurns = 100`, `compactionOverrideDuration = 8 * time.Hour`, with
  the reason at `:13-17`: *"a bounded, auto-reverting knob must never become unbounded, and a
  fresh process always comes back up at the configured proactive-compaction trigger."*
- `:23-25` — *"the runner-local, NEVER-persisted record"*.
- `:198-200` — an out-of-range percentage is **rejected with the valid band in the message**,
  not silently clamped.
- `:154-161` — teardown is idempotent; `:108-109`, `:135-152` — it is consumed at a turn
  boundary under a lock, so a mid-turn change cannot split a request.

**dorang consequence**, and it cuts against an existing decision worth re-examining: §10.5
clamps a client's priority hint into the principal's permitted range, and §10.2 clamps
reasoning effort into the declared level set. Clamping is right when the caller is expressing a
*preference* — a priority hint means "as urgent as you'll allow". It is wrong when the caller is
expressing a *requirement*, because a clamped requirement is silently not met. The distinction
is worth stating in §10.3 rather than deciding per-parameter, and jikji's rule — reject with the
valid band named — is the right default for anything that is not obviously a preference.

A seventh, structural: **compaction must never loop.**
`jikji/internal/compositor/conversation/admission.go:246-248` — *"RecoverOverflowOnce compacts
an outer projection and retries exactly once. It never loops and never retries a failed
retry."* Claude Code has the same guard for a measured reason:
`MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES = 3`, commented *"BQ 2026-03-10: 1,279 sessions had 50+
consecutive failures (up to 3,272) in a single session, wasting ~250K API calls/day globally"*
(`claude-code/src/services/compact/autoCompact.ts:67-70`). dorang has the same obligation for
fallback, and §7.6 already bounds it by `max_hops` and a wall-clock budget. The addition §D.3b
makes is that an output-cap 400 must not consume those hops at all.

### D.5 What dorang must not do

The obvious one is §10.5a: silently mutating a caller's conversation. Six more, each grounded:

1. **Never set `truncation: "auto"` or `truncate_prompt_tokens`** on a request the caller did
   not set them on (§A.3a). It converts an error the caller would see into a `200` they cannot
   audit, and it disables §7.6's `context_window` detection for that deployment.
2. **Never remove them either.** They are the caller's explicit choice. Removing one turns a
   working request into a 400.
3. **Never synthesize `max_tokens` to make the arithmetic work.** DESIGN §10.7 already requires
   supplying the model's max output when crossing into the Anthropic family, where the field is
   mandatory — that is a protocol requirement, and it is the *only* case. Everywhere else,
   VLLM.md §1.1 is explicit that dorang must not synthesize a max-output value, and §10.2 is
   explicit that dorang never raises a caller's `max_tokens`.
4. **Never generate a `prompt_cache_key`, `prompt_cache_retention`, `cache_salt`, or
   `cache_control.ttl` the caller did not send** (B7, B8, B20, B21). All four change semantics
   the caller owns — one repartitions their cache, one is a security boundary, and two change
   what they are billed.
5. **Never issue a second inference request the caller did not ask for.** No summarization, no
   token-counting round trip on the hot path. A `POST /tokenize` cross-check is a probe, not a
   request-path step.
6. **Never let an estimate become a claim.** If dorang refuses a request on an estimate, the
   error must say it was an estimate and say what the estimate was. jikji's error does exactly
   this (`jikji/cmd/jikjicode/runner_01_ConfigItems.go:1022`) — "projected input %d tokens".
   A caller who disagrees can then check.
7. **Never inject or remove `context_management`, in either vendor's shape** (B17, B22). It is
   a context-editing instruction to the *server*, and both directions are §10.5a violations:
   injecting it makes dorang the party that decided to compact, and removing it silently
   disables an edit the caller arranged. Removing its beta header has the same effect as
   removing the field (§A.4a).
8. **Never treat a `200` as proof the request fit** (§D.3a). Where reported input tokens exceed
   the declared window, record it — but do not widen the declared window on that evidence,
   because §4.3 permits raising a window only from a probe.
9. **Never trust an HTTP status alone for failure classification.** MiniMax returns errors
   inside a `200` body (`openclaw/extensions/minimax/music-generation-provider.ts:79-85`), and
   z.ai sometimes reports an overflow as a rate limit
   (`openclaw/packages/ai/src/utils/overflow.ts:118-121`). §7.6's classifier needs a
   per-kind hook, or it will retry the unretryable and price the failed.

---

## E. Passthrough design implications

### E.1 Current state

`internal/passthrough` does not exist yet; DESIGN §16 lists it in the planned layout
(DESIGN.md:1635). §10.6 specifies six behaviors and a security boundary. §9.2 already provides
`responses_store(response_id, …, items, reasoning_blobs)` and states why
(DESIGN.md:1001-1007). `internal/canonical` already carries `Extra map[string]json.RawMessage`
on `Request` (`internal/canonical/request.go:60-62`), `Message`, `Block`, and `Tool`
(`internal/canonical/message.go:69-71`, `:205`), round-tripped by the chat-completions adapter
through `marshalWithExtra`/`splitExtra` (`internal/wire/openai/types.go:182-206`).

So dorang is in better shape than the brief assumed. The gaps are specific.

### E.2 What §10.6 covers, and it covers more than expected

Checked against §A's inventory, the generic engine as specified handles:

- **Every unary vendor route**, including `/responses/compact`, `/memories/trace_summarize`,
  and `/alpha/search`. Step 3 — "relay the body **without parsing it**" — is exactly right for
  bodies dorang cannot construct, and step 5's best-effort metering degrades correctly for a
  compaction response that has no `usage` object.
- **zstd-compressed request bodies** (`codex/codex-rs/codex-api/src/requests/responses.rs:41-46`),
  because an unparsed relay does not care.
- **WebSocket routes** — `/realtime/calls`, the remote-control family, and Responses-over-WS —
  through step 6, frame-for-frame.
- **The security boundary**: unmapped prefixes unserved, traversal rejected, provider
  credentials never reaching the client. vLLM's development-mode routes (§A.3) are the reason
  this matters concretely — `/reset_prefix_cache` can preempt every running request, and it
  sits on the same base URL as `/v1/chat/completions`. **A prefix map that opens `/vllm` opens
  it too.**

### E.3 What it needs and does not have

Seven items, in the order they should be implemented.

**E3.1 — Route maps must be allow-lists, not prefixes.** §10.6's config maps a prefix to a
provider and joins the rest of the path. Given §A.3, that is an open door onto
`/reset_prefix_cache`, `/collective_rpc`, and the RLHF weight-update family. The engine needs
a per-route method+path allow-list, defaulting to closed:

```yaml
passthrough:
  routes:
    - prefix: /vllm
      provider: vllm-local
      allow:
        - { method: POST, path: /tokenize }
        - { method: POST, path: "/v1/responses/*/cancel" }
      # everything else 404s; /reset_prefix_cache is not reachable
    - prefix: /codex
      provider: openai-codex
      allow:
        - { method: POST, path: /responses/compact }
        - { method: POST, path: /memories/trace_summarize }
```

§10.6 already says "unmapped prefixes are not served — this is not an open proxy". This makes
the same statement true one level down, which is where the destructive routes live.

**E3.2 — Header policy must be a three-way classification, not a two-way one.** §10.6 step 4
says "replace only dorang-owned headers; strip hop-by-hop headers", and COMPATIBILITY 7.3
requires stripping six authentication headers. That leaves `x-codex-turn-state` (B4),
`x-grok-conv-id` and peers, `x-codex-beta-features`, and `ChatGPT-Account-ID` unclassified. The
engine needs:

| Class | Handling | Examples |
|---|---|---|
| dorang-owned | replaced | `x-dorang-*`, `X-Request-Id` |
| credential-bearing | **stripped**, always | the six of COMPATIBILITY 7.3, `ChatGPT-Account-ID` |
| hop-by-hop | stripped | `Connection`, `Transfer-Encoding` |
| **vendor-opaque** | **forwarded verbatim** | `x-codex-turn-state`, `x-codex-turn-metadata`, `x-openai-subagent`, `x-grok-*` except `-model-override`, `OpenAI-Beta`, `anthropic-beta`, `chatgpt-account-id`, `originator`, `session_id` |
| gateway-owned | set by dorang, client value discarded | `x-grok-model-override` (§A.2) |

The vendor-opaque class is per-provider-kind configuration, because the names are.

**E3.2a — Some headers and body fields are one unit and must move together.** `anthropic-beta:
context-management-2025-06-27` gates whether `context_management` may appear on the body at all
(`claude-code/src/services/api/claude.ts:1715-1722`), and the same pattern holds for
`cache_edits` (`:1673-1687`). A header filter and a body filter that run independently can drop
one side of a pair. The passthrough and adapter paths both need a **coupled-pair table**:
`(header, body_field)` pairs where dropping either requires dropping both, and where dropping
both is a reportable loss rather than a silent one.

This is not a new mechanism — it is the same shape as COMPATIBILITY 5.3's tool-name mapping,
where a transformation is only correct if both halves stay consistent. It just needs saying,
because the two halves are handled by different code today.

**E3.3 — Statefulness on the *adapter* path, not the passthrough path.** Two of OpenAI's three
server-side compaction mechanisms (§C.3) ride on an ordinary `/responses` call. If a caller
reaches `/v1/responses` through dorang's own adapter rather than through a passthrough prefix,
dorang decodes the body — and must then:

- preserve unknown `input[]` item types rather than rejecting them (B3);
- preserve unknown **top-level** body fields whose value is a nested array or object, which is
  what `context_management` is (B17). `canonical.Request.Extra` is
  `map[string]json.RawMessage` (`internal/canonical/request.go:60-62`), so the shape is already
  supported; what is missing is proof it survives the Responses adapter, which does not exist
  yet (`internal/wire/` contains only `openai`, and its doc comment scopes it to
  chat-completions at `internal/wire/openai/doc.go:1`);
- not persist a `CompactionTrigger` into `responses_store` as history, because it is a request
  control (`codex/codex-rs/protocol/src/models.rs:1013-1014`);
- store a returned `Compaction { encrypted_content }` item verbatim in `reasoning_blobs`, or
  in a sibling column, keyed by response id.

`canonical.Block` has `Extra` and `Kind` already, so an unknown item type has somewhere to
live. What is missing is a decode rule that says *unknown item kinds are preserved, not
dropped* — and golden tests that a `CompactionTrigger {}` item and a `context_management` array
both survive a decode/encode round trip unchanged.

**E3.3a — `store` is now load-bearing in three directions.** DESIGN §10.7 lists `Store` as a
plain Responses-family field. It is not:

- `store: true` is **required** for openclaw's server-side compaction
  (`openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:296-312`) and for
  vLLM's `background` mode (`vllm/entrypoints/openai/responses/protocol.py:459-465`);
- `store: true` is **rejected** by the Codex route — *"Store must be set to false"*
  (`openclaw/packages/ai/src/providers/openai-chatgpt-responses.ts:1471-1472`);
- vLLM **defaults it to `true`** (`vllm/entrypoints/openai/responses/protocol.py:174`) while
  Codex defaults it to `false` except on Azure (`codex/codex-rs/core/src/client.rs:915`).

So dropping, defaulting, or normalizing `store` changes behavior in opposite directions on
different backends of the same wire family. It belongs in the per-kind capability table with a
tri-state — required, forbidden, free — not in the generic parameter filter of §10.3.

**E3.4 — `responses_store` needs two more columns.** §9.2 has `items` and `reasoning_blobs`.
§B adds:

- `family_pin` — the `(kind, model-family)` that minted the opaque state in this conversation,
  so §B.3's pinning rule is enforceable rather than aspirational.
- `opaque_headers` — the `x-codex-turn-state` value and its peers, which are per-turn and
  arrive on the *response* but must go out on the *next request*. There is nowhere to put them
  today.

**E3.5 — Capability advertisement needs an opaque-state axis.** `internal/canonical/capability.go`
has `Structural` and `Droppable` (`:84-91`). Neither expresses "this deployment can accept
encrypted reasoning content minted by family X". The minimal addition is not a new bit per
vendor — that does not scale — but a per-deployment `opaque_state_realm` string in the
provider config: two deployments share a realm when state minted by one is accepted by the
other. Routing then filters on realm equality when the request carries opaque state, and §7.6
suppresses `context_window` fallback across realms. This is the smallest thing that makes
§B.3's constraint checkable.

**E3.6 — Metering must not require parsing.** §10.6 step 5 already says metering failure never
fails the request. Add: for a compaction call, the body is the *conversation*, so the request
is large and the response has no `usage`. Recording counts and bytes is the correct outcome,
and the ledger row should carry a `route_kind: passthrough` marker so a spend report does not
show a mysterious zero-token request.

**E3.7 — Capability advertisement to the caller.** A caller cannot discover which vendor routes
are open. COMPATIBILITY §9 says unimplemented routes answer `501` with a machine-readable
reason, never a silent `404`. Passthrough should do the same, and a
`GET /v1/dorang/capabilities` (or the equivalent under the admin surface) listing the open
prefixes and their allow-lists costs little and removes a whole class of "why did my compaction
call 404" support traffic.

### E.4 One thing §10.6 should explicitly *not* grow

It should not learn to *interpret* a compaction response. Rewriting the returned history,
storing it as canonical messages, or re-issuing it against a different backend all turn the
passthrough into an adapter for a proprietary protocol that has no specification and changes
without notice. §10.5a's boundary is the right one and §10.6's "relay the body without parsing
it" already enforces it. It should be stated as intent, not left as a side effect.

---

## F. Ranked recommendations

Ordered by cost of getting it wrong.

1. **MUST — dorang never sets or removes `truncation` / `truncate_prompt_tokens`.**
   Setting it converts an auditable error into a silent `200` and disables §7.6's
   `context_window` detection for that deployment
   (`vllm/entrypoints/openai/responses/protocol.py:183`, `:346`;
   `vllm/entrypoints/serve/utils/api_utils.py:178-188`). Removing it breaks a caller's explicit
   choice. Enforce with a config-load rejection, the same way §4.3 rejects reasoning on a prefix
   rule. → §A.3a, §D.5.1–2

2. **MUST — treat `context_management` as opaque, per family, and move it with its beta header.**
   The name is shared by two vendors with incompatible schemas — an array on OpenAI Responses
   (`openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:409-416`), an object
   with `edits` on Anthropic Messages
   (`claude-code/src/services/compact/apiMicrocompact.ts:34-61`) — so it must never be modelled
   by name in `CanonicalRequest`. On the Anthropic side it is gated by
   `anthropic-beta: context-management-2025-06-27` (`claude-code/src/constants/betas.ts:7`;
   `claude-code/src/services/api/claude.ts:1715-1722`) and echoed on the response
   (`claude-code/src/utils/messages.ts:400`, `:763`). This is second only because it is
   narrower than 1, not because it is less severe: it lands on `/v1/messages`, a T0 path
   (COMPATIBILITY §0) that §6 declares an adapter rather than a passthrough — so a crossing that
   cannot carry it needs a construct id and a `400` per §10.1, not silent omission.
   → §A.4a, §B.2 B22–B23, §E3.2a, §E3.3

3. **MUST — passthrough routes are method+path allow-lists, defaulting to closed.**
   vLLM's development-mode family sits on the same base URL as inference, and
   `/reset_prefix_cache` can preempt every running request
   (`vllm/entrypoints/serve/dev/cache/api_router.py:20`; VLLM.md §5). A prefix-only map opens
   them. → §E3.1

4. **MUST — classify headers four ways, not two, and forward the vendor-opaque class verbatim.**
   `x-codex-turn-state` is set from a response and echoed on the next request
   (`codex/codex-rs/codex-api/src/sse/responses.rs:62-69`;
   `codex/codex-rs/core/src/client.rs:1897-1903`). A blanket inbound-header strip breaks it;
   a blanket forward leaks credentials. Header and body filters must also share a coupled-pair
   table so neither half of a field/header unit is dropped alone. → §E3.2, §E3.2a

5. **MUST — pin a conversation carrying opaque state to a compatible deployment family, and
   suppress `context_window` fallback across families.**
   The cross-family case is a hard 400 classified as never retryable
   (`grok/crates/codegen/xai-grok-sampling-types/src/error.rs:211-214`, `:748`, `:756`), so
   fallback cannot rescue it — it burns every hop and returns the same error.
   → §B.3, §D.3, §E3.5

6. **MUST — the estimator must not undercount, and must not be byte-only.**
   `bytes/4` undercounts CJK (`jikji/pkg/tokenestimate/estimate.go:18-20`), and undercounting
   is the direction that makes fallback fail to fire (DESIGN §4.3). Fold content-class counting
   into the existing §7.4b byte scan, with a framing surcharge per message and content block and
   a margin — the design openclaw arrived at independently
   (`openclaw/src/agents/embedded-agent-runner/run/preemptive-compaction.ts:26-32`,
   `openclaw/src/agents/compaction-planning.ts:19-21`). Add a conformance test asserting
   non-undercount on a CJK + tool-JSON + image corpus. → §D.2

7. **MUST — make `store` a tri-state per kind, not a droppable parameter.**
   It is *required* for openclaw's server compaction
   (`openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:296-312`) and
   vLLM's `background` (`vllm/entrypoints/openai/responses/protocol.py:459-465`),
   *rejected* by the Codex route
   (`openclaw/packages/ai/src/providers/openai-chatgpt-responses.ts:1471-1472`), and defaults
   in opposite directions across backends of the same wire family
   (`vllm/entrypoints/openai/responses/protocol.py:174` vs
   `codex/codex-rs/core/src/client.rs:915`). → §E3.3a

8. **MUST — separate the output-cap 400 from the input-overflow 400 in §7.6, and give the
   output-cap cause no fallback chain.**
   Routing an output-cap error to `context_window` fallback resends the same oversized
   `max_tokens` to a larger model and gets the identical 400. hermes fails fast on this exact
   case for this exact reason (`hermes/agent/conversation_loop.py:3912-3914`), and carries two
   distinct signal sets to tell them apart (`hermes/agent/model_metadata.py:1443-1463`,
   `:1470-1478`). → §D.3b

9. **MUST — the over-limit error names the real limit, the estimate, and the deployment.**
   This is the whole justification for §10.5a's third row. Clients otherwise parse it out of
   error prose with seven regexes and refuse to believe any number larger than the one they
   have (`hermes/agent/model_metadata.py:1256-1264`, `:1281-1292`). Template:
   `jikji/cmd/jikjicode/runner_01_ConfigItems.go:1022`. → §C.5, §D.4.5

10. **MUST — never generate `prompt_cache_key`, `prompt_cache_retention`, or `cache_salt` a
    caller did not send, and never drop one they did.**
    The keys silently repartition a cache the caller was managing and mis-price §8's cost
    engine; `cache_salt` is a security boundary
    (`vllm/entrypoints/openai/responses/protocol.py:244-254`); retention is billable policy
    (`openclaw/packages/ai/src/transports/openai-responses-transport.ts:2131-2132`).
    → §B.2 B7–B8, B20–B21, §D.5.4

11. **MUST — `/v1/models` must not forward an upstream `ETag`.**
    dorang rewrites the model list per the calling key's allow-list (COMPATIBILITY 7.4), so an
    upstream validator would validate a body dorang did not send
    (`codex/codex-rs/codex-api/src/endpoint/models.rs:64-68`). Mint dorang's own, or send none.
    → §B.2 B15

12. **MUST — `responses_store` gains `family_pin` and `opaque_headers`.**
    Without them recommendations 4 and 5 have nowhere to keep their state, and per-turn opaque
    headers arrive on a response with no place to survive to the next request. → §E3.4

13. **SHOULD — preserve usage extensions rather than recomputing over them.**
    grok deliberately rewrites `total_tokens` from `context_details` because the cumulative
    value is wrong under server-side tool loops, and that number drives the client's compaction
    trigger (`grok/crates/codegen/xai-grok-sampler/src/client.rs:89-98`, `:137-145`).
    §10.7's normalization must add, not overwrite. Also read `cost_in_usd_ticks` — the vendor
    already priced the request (`grok/crates/codegen/xai-grok-sampling-types/src/types.rs:535-549`).
    → §A.2a, §B.2 B12–B13

14. **SHOULD — preserve unknown `input[]` item kinds through decode/encode, with a golden test
    using `CompactionTrigger {}`.**
    Remote compaction v2 has no route to detect and no field to whitelist; it is a sentinel item
    on an ordinary request (`codex/codex-rs/core/src/compact_remote_v2_attempt.rs:71`). A
    validating adapter rejects it. → §E3.3

15. **SHOULD — reconcile the §2.1 route table with what backends actually serve.**
    vLLM implements `POST /v1/responses/{id}/cancel`
    (`vllm/entrypoints/openai/responses/api_router.py:110`) which §2.1 does not list, and does
    not implement `DELETE /v1/responses/{id}` which §2.1 does. The `GET` also takes
    `starting_after` and `stream` (`:80-87`). → §A.3

16. **SHOULD — adopt `X-Models-Etag`-style catalog invalidation for kinds that offer it.**
    A backend can signal that its model table changed on the inference stream, without polling
    (`codex/codex-rs/codex-api/src/sse/responses.rs:41-45`). dorang currently has no refresh
    signal at all, and §4.3's `UnverifiedModels()` list only grows by hand. → §D.1

17. **SHOULD — validate durable state on restore and reset rather than adopt on mismatch.**
    jikji discarded a whole breaker version because a persisted timestamp had no structural
    upper bound (`jikji/internal/forme/compaction_guard_state.go:79-82`), and resets rather than
    adopts when the restored identity differs
    (`jikji/cmd/jikjicode/runner_02_reduceProjectedOuterConversation.go:908-911`). The same
    applies to `credential_state.unavailable_until`, `quota_leases`, and `budget_state`.
    → §D.4.1

18. **SHOULD — a soft-path breaker must never suppress a hard-path check.**
    *"It governs proactive attempts only; overflow recovery never consults it"*
    (`jikji/internal/forme/compaction_guard_state.go:21-23`). For dorang: a deployment circuit
    breaker must not suppress the capacity admission that would have refused the request
    anyway. Make it structural, not remembered. → §D.4.2

19. **SHOULD — meter passthrough with a `route_kind` marker and accept token-less rows.**
    A compaction call has a huge request body and a response with no `usage`. A spend report
    showing a zero-token request with no explanation is a support ticket. → §E3.6

20. **SHOULD — advertise which passthrough routes are open, and `501` the rest.**
    COMPATIBILITY §9 already requires `501` with a machine-readable reason over a silent `404`;
    passthrough should not be an exception. → §E3.7

21. **SHOULD — maintain a per-kind rejected-parameter list, separate from the supported set.**
    §10.3's `drop_unsupported` filters against a kind's supported set. The Codex route needs the
    inverse: a list of parameters the *same vendor and wire family* rejects when reached through
    a different credential
    (`openclaw/packages/ai/src/transports/openai-responses-transport.ts:2000-2007`, plus
    `input[].status` at
    `openclaw/packages/ai/src/transports/openai-responses-payload-policy.ts:333-343`). dorang's
    `codex-responses` kind (`pkg/catalog/provider_defaults.yaml:88-95`) is the right place.
    → §A.1b

22. **SHOULD — add `finish_reason` rows for the vendor-native terminal values found here.**
    COMPATIBILITY §4.1's list does not include `abort` or `repetition` (VLLM.md §2.3) or
    z.ai's `model_context_window_exceeded` (`openclaw/packages/ai/src/utils/overflow.ts:65`).
    Per 4.2 an unmapped value becomes `"stop"` and logs a warning, which for the last of these
    reports an overflow as a normal turn end. → §A.4

23. **SHOULD — record a per-deployment `overflow_behavior` capability.**
    Backends differ in whether an oversized request errors, truncates silently, or returns a
    rate-limit error (`openclaw/packages/ai/src/utils/overflow.ts:30-42`, `:113-125`). This is
    the same pattern §4.3 and VLLM.md §1.2 already use for unverified capabilities: operator
    declaration, defaulting to unknown, with the usage-based check of §D.3a as the runtime
    backstop. → §D.3a

---

## G. Unverified

Recorded rather than assumed.

- **Whether `/responses/compact` exists on `api.openai.com` as opposed to
  `chatgpt.com/backend-api/codex`.** Codex selects the base URL by auth mode
  (`codex/codex-rs/model-provider-info/src/lib.rs:244-262`) and gates remote compaction on
  provider identity (`:422-424`), which is suggestive but not proof. No live call was made.
- **The wire schema of the `/responses/compact` request body beyond the Rust struct.**
  `CompactionInput` at `codex/codex-rs/codex-api/src/common.rs:26-44` is the client's view. The
  server's accepted schema is not observable from this tree.
- **What `Compaction.encrypted_content` contains.** It is opaque by construction. Nothing in
  the Codex tree validates or inspects it.
- **Per-image token cost.** 1,600 (`hermes/agent/context_compressor.py:316`) versus 765
  (`grok/crates/codegen/xai-token-estimation/src/lib.rs:13`), and hermes' own docstring says
  1,500 (`hermes/agent/model_metadata.py:2729-2730`). Three numbers, two clients, one
  self-inconsistency.
- **Whether vLLM's `truncation: "auto"` truncates from the left or the right in practice.**
  `truncation_side` defaults to `None`, which falls back to the tokenizer default
  (`vllm/renderers/params.py:160-166`), and the tokenizer default was not read.
- **Any compaction-related setting in this machine's user configuration.**
  `.codex/config.toml` contains no compaction key; `.claude/settings.json`
  contains no context or compaction key, so Claude Code's `autoCompactEnabled` default of `true`
  (`claude-code/src/utils/config.ts:594`) applies. Absence of configuration, not absence of the
  feature.
- **Claude Code citations come from two artifacts that do not fully agree, and both are
  qualified.** `claude-code/src/` is a leaked TypeScript snapshot dated 2026-04-05, per its own
  `README.md:1`; the installed binary at `~/.local/share/claude/versions/2.1.220` is a compiled
  single-file executable whose JS is minified onto one line, so "line numbers" there are not
  meaningful and only quoted strings are. Where the two agree the source is cited. Two features
  appear **only** in the binary — proactive precompute (`precomputeCompactionEnabled`,
  `"Pds=0.2"`) and per-model `autoCompactWindow` defaults — so the leaked source is not current.
  Treat every Claude Code figure as *observed on this machine*, not as a published contract.
- **Whether Anthropic's `context_management` is generally available.** Tool-clearing strategies
  are gated to internal users in the code that builds them
  (`claude-code/src/services/compact/apiMicrocompact.ts:88-92`). A public deployment may never
  emit it. That is a reason to relay it, not a reason to model it.
- **The `clear_tool_uses_20250919` / `clear_thinking_20251015` server-side semantics.** Only the
  client's type definitions were read. What the server does with `clear_at_least` or
  `exclude_tools` was not observed.
- **The measured accuracy of the §D.2 estimator.** It is specified, not built, and the margin
  is bracketed by jikji's 5% on top of a real tokenizer
  (`jikji/internal/typebackbone/token_estimate.go:29-47`) and openclaw's 20% on top of a
  heuristic (`openclaw/src/agents/compaction-planning.ts:19-21`), rather than measured against
  dorang's own traffic.
- **Citations into `pkg/catalog/` were re-checked at the end of writing and may drift.**
  `model_catalog.yaml`, `provider_defaults.yaml`, and `catalog_test.go` were being edited
  concurrently while this document was written. Each citation into them is paired with a quoted
  anchor so it can be relocated; the line numbers are true as of this revision only.
