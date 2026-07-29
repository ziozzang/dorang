# SGLang as a Second First-Class Backend

> Integration specification, derived from source at `sgl-project/sglang` commit `d9cf7b0`
> (main, 2026-07-28). Every behavioral claim below was read out of that tree, not inferred.
> Cite the commit rather than a release: the checkout is shallow and untagged.
>
> Companion to [VLLM.md](VLLM.md), and structured to be read against it row for row.
> Where SGLang behaves like vLLM this document says so and moves on. The value is in §7.
>
> 한국어: [SGLANG.ko.md](SGLANG.ko.md)

SGLang is *closer* to dorang's target surface than vLLM in two ways that matter — it speaks
Anthropic natively, and its load endpoint is real rather than a flag-gated zero — and
*further away* in two that matter more: priority runs in the opposite direction, and the
OpenAI error envelope is not OpenAI's.

Both engines fail quietly in the same places. They just fail quietly in **different** places,
which is worse for a gateway than either one alone.

---

## 0. The four findings that change dorang's design

### 0.1 Priority direction is inverted, and dorang's class map is exactly wrong for SGLang

vLLM schedules **lower first**. SGLang schedules **higher first** by default. Same field
name, same type, opposite meaning, no error either way.

```
schedule_policy.py:181   self.priority_sign = 1 if schedule_low_priority_values_first else -1
schedule_policy.py:378       x.priority * priority_sign,     # ascending sort
```

`--schedule-low-priority-values-first` defaults to `False`
(`server_args.py:826-830`), so `priority_sign = -1`, so the ascending sort on
`-priority` puts the **largest** value first. The help text says so independently:
`server_args.py:810` — *"Requests with higher priority integer values will be scheduled
first by default."*

DESIGN §7.5's map `{realtime: 0, interactive: 2, batch: 10}` is correct for vLLM and
**precisely inverted** for a default-configured SGLang: batch jobs would outrank realtime
ones. This is the single highest-severity item in this document, because both engines
accept the same integer, return 200, and produce opposite orderings.

Two ways out, and dorang should do both:

- **Per-provider `priority_direction`**, declared by the operator (`lower_first` for vLLM,
  `higher_first` for SGLang), with the class map inverted at emit time.
- **Recommend `--schedule-low-priority-values-first`** in the SGLang runbook (§8), which
  makes SGLang agree with vLLM and lets one map serve both.

Details, preemption, and the rest in §3.

### 0.2 The context window is readable with one unauthenticated GET — same call, same field

```
GET {base_url}/v1/models  →  data[i].max_model_len
```

`http_server.py:1792` sets it from `tokenizer_manager.model_config.context_len`, the
effective post-override value. `ModelCard.max_model_len` is declared at
`protocol.py:78`. The route is registered unconditionally (`http_server.py:1780`).

**This is bit-for-bit the same contract VLLM.md §1.1 established**, including the LoRA rule:
adapter cards carry `max_model_len=None` and a `parent` pointing at the base model
(`http_server.py:1800-1807`). dorang's existing `/v1/models` probe works against both
engines with no branching. That is a genuinely good outcome and worth stating plainly.

The differences are in what *else* is available, and they are covered in §2. The important
one: SGLang additionally exposes `/server_info`, which returns the entire `ServerArgs`
dataclass — every flag §8 talks about, readable at runtime — and also, unredacted, the
server's own API keys.

### 0.3 `/v1/messages` is served natively, and it is an adapter, not a passthrough

`http_server.py:1939` registers `POST /v1/messages`; `:1949` registers
`POST /v1/messages/count_tokens`. No flag gates either. The request and response models are
a genuine Anthropic implementation (`anthropic/protocol.py`, 518 lines), and the serving
layer (`anthropic/serving.py`, 1452 lines) converts Anthropic → `ChatCompletionRequest` →
the OpenAI chat pipeline → back to Anthropic events.

For dorang this is a real capability, not a checkbox: an Anthropic-shaped request can be
forwarded rather than translated. But it is an adapter with the **same lossy collapse
COMPATIBILITY 6.4 already specifies**, so dorang inherits the same obligation to report the
truth out of band:

```
anthropic/serving.py:68-72   STOP_REASON_MAP = {"stop": "end_turn", "length": "max_tokens",
                                                "tool_calls": "tool_use"}
```
Everything else — including `abort` — falls to `end_turn` with a WARNING
(`anthropic/serving.py:1287-1292`, `:1045-1051`).

Full event list, usage shape, and the fields that are accepted-and-dropped are in §1.2.

### 0.4 The OpenAI error envelope is not nested, and there are five shapes on one server

```python
# protocol.py:88-93
class ErrorResponse(BaseModel):
    object: str = "error"
    message: str
    type: str
    param: Optional[str] = None
    code: int
```

`serving_base.py:225` returns `error.model_dump()` **flat**. An OpenAI SDK reads
`body["error"]["message"]`; against SGLang that key does not exist. vLLM at least nests.
This is not a "normalize the code field" problem like VLLM.md §2.2 — it is a *structural*
one, and an unmodified OpenAI SDK will raise on parse rather than surface the message.

Five shapes, all reachable on the same deployment, are enumerated in §6.2.

---

## 1. HTTP surface

### 1.1 Registered routes

Every route below is decorated in `python/sglang/srt/entrypoints/http_server.py` unless
noted. `admin?` means `@auth_level(AuthLevel.ADMIN_OPTIONAL)` — see §1.4 for what that
actually enforces.

**OpenAI-compatible**

| Route | Line | Gate |
|---|---|---|
| `POST /v1/chat/completions` | 1659 | none |
| `POST /v1/completions` | 1651 | none |
| `POST /v1/embeddings` | 1669 | none |
| `POST /v1/classify` | 1681 | none |
| `POST /v1/tokenize`, `POST /tokenize` | 1693, 1698 | none |
| `POST /v1/detokenize`, `POST /detokenize` | 1711, 1716 | none |
| `POST /v1/score` | 1837 | none |
| `POST /v1/rerank` (also PUT) | 1880 | none |
| `POST /v1/responses` | 1845 | none |
| `GET /v1/responses/{id}`, `POST /v1/responses/{id}/cancel` | 1864, 1872 | none |
| `GET /v1/models` | 1780 | none |
| `GET /v1/models/{model:path}` | 1812 | none |
| `POST /v1/audio/transcriptions` | 1729 | none |
| `WS /v1/realtime` | 1769 | transcription subset only (`:1772-1776`) |

`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings` and `/v1/models` all exist and
are OpenAI-shaped **in field names**. They are not OpenAI-shaped in three respects — extra
fields, error envelope, and streaming chunk contents — all in §6.

**Anthropic-compatible** — `POST /v1/messages` (1939), `POST /v1/messages/count_tokens`
(1949). Neither is gated.

**Ollama-compatible** — `POST /api/chat` (1910), `POST /api/generate` (1916),
`GET /api/tags` (1924), `POST /api/show` (1930). Each path is read from an environment
variable at import time (`SGLANG_OLLAMA_CHAT_ROUTE` etc.), so **the route paths themselves
are operator-mutable**. `GET|HEAD /` returns the literal string `"SGLang is running"`
(1903-1907), or `"Ollama is running"` if `SGLANG_OLLAMA_ROOT_ROUTE` is set (1892-1899).

**Vendor**: `GET /ping` (1960) and `POST /invocations` (1966) for SageMaker — `/invocations`
is `/v1/chat/completions` under another name. `POST /vertex_generate` (1977), path
overridable by `AIP_PREDICT_ROUTE`.

**Native**: `POST|PUT /generate` (828), `/encode` (881), `/classify` (893).

**Health and info**: `GET /health`, `GET /health_generate` (616-617);
`GET /model_info` (703) and deprecated `GET /get_model_info` (693);
`GET /server_info` (743) and deprecated `GET /get_server_info` (733);
`GET /v1/loads` (`v1_loads.py:80`, router included unconditionally at 451) and deprecated
`GET /get_load` (768). `GET /get_weight_version` and `GET /weight_version` (723-724)
**exist only to return 404 with a deprecation message** (`:727-730`) — a gateway probing
them will see a 404 that means "moved", not "absent".

`GET /openapi.json` is served unless `DISABLE_OPENAPI_DOC` is set (`:431`).

### 1.2 `/v1/messages` — what is actually spoken

This is the section that decides whether dorang forwards or translates.

**Events emitted.** `message_start`, `content_block_start`, `content_block_delta`,
`content_block_stop`, `message_delta`, `message_stop`. Framing is
`event: <type>\ndata: <json>\n\n` — both lines (`anthropic/serving.py:156-158`), matching
COMPATIBILITY 6.1. There is **no `[DONE]`** and **no `ping`**: `PingEvent` is declared
(`anthropic/protocol.py:477-478`) and never constructed anywhere in `serving.py`. An
`error` event exists and is emitted mid-stream on upstream failure
(`anthropic/serving.py:924-945`, `:1033-1036`). Content block types produced are `text`,
`thinking`, and `tool_use`, plus a `signature_delta` when the backend supplies a thinking
signature (`:868-877`).

**`stop_reason` values.** The wire type is a four-value `Literal`:
`end_turn`, `max_tokens`, `stop_sequence`, `tool_use` (`anthropic/protocol.py:436-438`,
`:509-511`). But `STOP_REASON_MAP` (`serving.py:68-72`) can only ever produce three of
them — **`stop_sequence` is declared and never emitted**, because the adapter never reads
`matched_stop` from the OpenAI layer. So SGLang has the information (§6.4) and discards it
on this path. `refusal`, `pause_turn`, and `model_context_window_exceeded` do not exist.

**`stop_sequence` is omitted, not null.** `serving.py:762` serializes with
`model_dump(exclude_none=True)`, so the key is absent. Anthropic's own API emits
`"stop_sequence": null`. COMPATIBILITY 6.5 says the field is null on the adapter path;
against SGLang dorang must synthesize the key rather than forward the object.

**Usage.** `input_tokens` is **exclusive of cache reads** — `serving.py:107` computes
`max(prompt - cached, 0)` — which is Anthropic's own convention and matches DESIGN §10.7's
normalization target. `cache_read_input_tokens` appears only when non-zero (`:127-128`).
**`cache_creation_input_tokens` is never set anywhere** — grep it: the field is declared at
`protocol.py:47` and has no writer. There is **no `total_tokens`**, so SGLang does *not*
reproduce the asymmetry COMPATIBILITY 6.8 describes; dorang must add it when
`compat.anthropic_total_tokens` is on.

**Accepted and silently dropped.** `metadata` is declared (`protocol.py:367`) and never
read — so `EndUser` via `metadata.user_id` is lost. `cache_control` on content blocks does
not exist in the model at all: `TextBlock` is `{type, text}` (`protocol.py:54-56`) and
Pydantic's default `extra='ignore'` discards it. **Anthropic prompt-caching breakpoints are
not expressible.**

**Accepted, logged, and not enforced.** `thinking.budget_tokens` (warned at
`serving.py:576-581`), `thinking.display: "omitted"` (`:591-595`), `betas` (`:623-627`),
`output_config.task_budget` (`:614-618`). Anthropic server tools — `web_search_*`,
`computer_*`, `bash_*`, `text_editor_*` — are parsed, recognized, and **skipped** with an
INFO log (`:641-644`), so a request using them returns 200 having quietly dropped the tool.

**Honored.** `system` (string or block list), `temperature`, `top_p`, `top_k`,
`stop_sequences` → `stop`, `tools` → OpenAI tools, `tool_choice`,
`thinking.type != "disabled"` → reasoning on, `output_config.effort` →
`reasoning_effort` with `xhigh → max` (`serving.py:606-609`). `max_tokens` is required and
validated positive (`protocol.py:389-394`). Streaming forces
`stream_options.include_usage=True` (`serving.py:557-561`), so usage always arrives.

**No `priority`.** The field does not exist on `AnthropicMessagesRequest`, and the
conversion builds its `ChatCompletionRequest` from an explicit whitelist
(`serving.py:540-563`) that never includes it. Same conclusion as VLLM.md §1.2: Anthropic
traffic needing priority must be translated to `/v1/chat/completions`.

**Errors are correctly Anthropic-shaped** — `{"type":"error","error":{"type","message"}}`
(`serving.py:1383-1389`, and the FastAPI handlers at `http_server.py:483-491`), with a
documented status→type map (`serving.py:74-85`) and 5xx messages scrubbed to
`"Internal server error"` (`:169-170`). This is the one error surface on the server that a
stock SDK parses correctly.

**`count_tokens` returns exactly `{"input_tokens": N}`** (`serving.py:1438-1442`), matching
COMPATIBILITY 6.9. It does not accept a `?beta=true` query parameter — extra query params
are ignored by FastAPI, so it is harmless but not honored.

### 1.3 There is no batch API — settled by route registration

`/v1/batches` **does not exist**. A grep for `batches` across `python/sglang/srt/` returns
only unrelated identifiers (`run_batch_cpu` timing fields, `batch_idx` loop variables,
`schedules batches` in a docstring). There is no batch-job class, no polling route, and no
offline batch CLI in this tree — SGLang does not even have vLLM's `run_batch` script.

Nor is there a synchronous fan-out equivalent of vLLM's
`/v1/chat/completions/batch`. The closest thing is that `/generate` and `/encode` accept
list-valued inputs and fan out internally (`io_struct.py:475`, `:783`), but that is the
native API, not an OpenAI batch shape, and it is still one all-or-nothing HTTP request.

**DESIGN §11.1's decision that the batch scheduler is dorang's own is confirmed correct
for both engines.** SGLang gives it no partial credit at all.

### 1.4 Dev and admin routes, and what actually gates them

SGLang has a much larger admin surface than vLLM, and its gate is weaker than it looks.

**Destructive or state-mutating, marked `ADMIN_OPTIONAL`:** `/flush_cache` (905),
`/set_internal_state` (797), `/add_external_corpus` (923), `/remove_external_corpus` (948),
`/list_external_corpora` (966), the six `/hicache/storage-backend*` routes (981-1092),
`/start_profile` `/stop_profile` `/set_trace_level` `/freeze_gc` (1093-1137), the three
expert-distribution routes (1138-1170), fourteen weight-update routes (1171-1400),
`/get_weights_by_name` (1403), `/release_memory_occupation` `/resume_memory_occupation`
(1419, 1431), `/weights_checker` (1443), `/slow_down` (1462), the three LoRA routes (1476,
1487, 1499), `/open_session` `/close_session` (1510, 1524), `/configure_logging` (1534),
`/abort_request` (1544), `/pause_generation` `/continue_generation` (1622, 1635), and
`/scale_elastic_ep` `/is_scaling_elastic_ep` (`elastic_ep.py:14`, `:75`).
`/dumper/{method}` (809) is registered only when `DUMPER_SERVER_PORT=reuse`.

**`ADMIN_OPTIONAL` means "open when no key is configured."** `utils/auth.py:128-138`:
with neither `--api-key` nor `--admin-api-key`, `AuthDecision(allowed=True)`. So on a
default-launched server *every route in that list is unauthenticated*, including
`/flush_cache`, `/slow_down`, `/pause_generation`, and the weight-update family.

Three further properties an operator must know:

- **`--api-key` is only wired up in single-tokenizer mode.** `http_server.py:2431` takes the
  `tokenizer_worker_num == 1` branch; the middleware is added at `:2448-2459` inside it. The
  `else` branch at `:2460` adds no auth at all. **A server launched with
  `--tokenizer-worker-num 2 --api-key secret` is fully unauthenticated** and reports no error.
- **`/health*` and `/metrics` are always allowed**, by prefix, even with keys configured
  (`utils/auth.py:100-101`). That is deliberate for k8s and Prometheus, and it means a
  `/health_generate` probe runs a real one-token generation on an otherwise locked server.
- **CORS is `allow_origins=["*"]` with `allow_credentials=True`** (`http_server.py:433-438`),
  unconditionally, on every route.

**dorang must never call any of these against a production backend.** The same conclusion
as VLLM.md §5, with more surface behind it: `/flush_cache` drops the radix cache,
`/slow_down` throttles the scheduler, and `/pause_generation` stops it.

---

## 2. Context and model metadata

The highest-value section, and the one where SGLang is genuinely better than vLLM.

### 2.1 `GET /v1/models`, field by field

```python
# protocol.py:69-78
class ModelCard(BaseModel):
    id: str
    object: str = "model"
    created: int = Field(default_factory=lambda: int(time.time()))
    owned_by: str = "sglang"
    root: Optional[str] = None
    parent: Optional[str] = None
    max_model_len: Optional[int] = None
```

Wrapped in `ModelList{object:"list", data:[...]}` (`protocol.py:81-85`).

- **`max_model_len` carries the effective context window** — `http_server.py:1792`,
  from `model_config.context_len`.
- `id` and `root` are `served_model_name`, which defaults to `model_path`
  (`server_args.py:4004-4005`).
- LoRA adapters are appended when `--enable-lora`, with `parent` set to the base model and
  `max_model_len=None` (`http_server.py:1797-1807`) — identical to vLLM's rule.
- **`created` is `int(time.time())` evaluated per response**, not a fixed constant. It
  changes on every call. COMPATIBILITY 7.4 requires a constant; dorang restamps this field
  anyway, so it is a note rather than a defect.
- `owned_by` is the literal `"sglang"`.

`GET /v1/models/{model}` returns a bare `ModelCard` (not wrapped) and 404s on a name
mismatch with a **nested, string-coded** error envelope
(`http_server.py:1818-1828`) — which is not the shape any other error on this server uses.

### 2.2 `/model_info` — no context length

```python
# http_server.py:707-719
{"model_path", "tokenizer_path", "is_generation", "preferred_sampling_params",
 "weight_version", "has_image_understanding", "has_audio_understanding",
 "model_type", "architectures"}
```

There is **no context-length field here.** `/get_model_info` is the same handler behind a
deprecation warning (`:693-700`). A gateway that reaches for the obviously-named endpoint
gets everything except the one number it wanted.

### 2.3 `/server_info` — everything, including the keys

```python
# http_server.py:754-765
msgspec_to_builtins({**dataclasses.asdict(server_args), **_global_state.scheduler_info,
                     "internal_states": ..., "version": ..., "kv_events": ...})
```

This is the whole `ServerArgs` dataclass. It answers every question §8 asks — the resolved
`context_length`, `schedule_policy`, `enable_priority_scheduling`,
`schedule_low_priority_values_first`, `page_size`, `enable_cache_report`,
`tool_call_parser`, `reasoning_parser`, `allow_auto_truncate`, `max_running_requests` —
without a completion request. It is the SGLang analog of the vLLM development-mode endpoint
VLLM.md §1.2 could not use, except it is registered unconditionally and carries no
`ADMIN_OPTIONAL` marker.

**It is also unredacted.** `api_key` (`server_args.py:1281`) and `admin_api_key`
(`server_args.py:1286`) are ordinary dataclass fields and `asdict` includes them; there is
no redaction anywhere in the call path. Two consequences:

- If only `--admin-api-key` is set, `NORMAL`-level routes are open
  (`utils/auth.py:143-146`), so `/server_info` **hands the admin key to any caller.**
- Under `--tokenizer-worker-num > 1` (§1.4), the same is true regardless of which keys are set.

dorang may read `/server_info` for capability discovery on a trusted-network deployment,
but it must be an **explicitly enabled per-provider option**, must never be logged, and its
availability must not be assumed.

### 2.4 How the context length is derived, and what overrides it

`model_config.py:733-765`:

```python
derived_context_len = get_context_length(self.hf_text_config)
if context_length is not None:
    if context_length > derived_context_len:
        # raises ValueError unless SGLANG_ALLOW_OVERWRITE_LONGER_CONTEXT_LEN=1
    else:
        self.context_len = context_length
else:
    self.context_len = derived_context_len
```

`--context-length` (`server_args.py:545-553`) narrows only. Widening past the model's own
derived value is a **startup error** unless
`SGLANG_ALLOW_OVERWRITE_LONGER_CONTEXT_LEN=1` (`environ.py:750`, default False). vLLM warns;
SGLang refuses. That is the safer behavior and worth noting in the operator profile.

### 2.5 Practical answer

**Yes. One unauthenticated `GET /v1/models`, field `data[i].max_model_len`.** Identical call
and field to vLLM. dorang needs no per-engine branching for context discovery.

Two supporting facts, both the same as vLLM: `null` means undeclared (LoRA cards), and
**there is no max-output value to read and dorang must not synthesize one.** The
`/v1/completions` caveat differs slightly — see §6.6.

`POST /v1/tokenize` exists (`http_server.py:1693`) but there is no evidence it returns
`max_model_len` the way vLLM's does; that was not verified (§9).

---

## 3. Scheduling priority

### 3.1 The field

`priority: Optional[int] = None` on `CompletionRequest` (`protocol.py:388`),
`ChatCompletionRequest` (`:830`), `EmbeddingRequest` (`:1175`), `ClassifyRequest` (`:1203`).
On `ResponsesRequest` it is `priority: int = Field(default=0, ...)` (`:1500`) — non-optional
with a default, which matters below.

It reaches the engine through `serving_chat.py:771`, `serving_completions.py:131`,
`serving_embedding.py:168`, `serving_classify.py:74`, into
`GenerateReqInput.priority` (`io_struct.py:272`) and
`TokenizedGenerateReqInput.priority` (`io_struct.py:866`), then onto the `Req` object
(`scheduler.py:2178`, `schedule_batch.py:865`).

**`/v1/responses` declares it and never plumbs it.** Neither `GenerateReqInput`
construction in `serving_responses.py` (`:338`, `:2376`) passes `priority`. The value is
read into a local, decremented once per built-in-tool round trip
(`:2350`, `:2409`), and dropped. So vLLM's §1.2 "responses mutates priority" hazard does
**not** apply here — the field is simply dead. Different failure, same practical advice:
do not route priority-bearing traffic through `/v1/responses`.

### 3.2 Direction, confirmed three ways

Established in §0.1. The three independent confirmations:

1. `schedule_policy.py:181` — `priority_sign = 1 if schedule_low_priority_values_first else -1`
2. `schedule_policy.py:376-381` — ascending sort on `(priority * priority_sign, wait_queue_entry_time)`
3. `server_args.py:810` — help text stating higher-first is the default

A fourth, decisive one: the default fill for a missing priority
(`scheduler.py:2469-2473`) uses `sys.maxsize` when low-values-first and
`-sys.maxsize - 1` otherwise — in both cases the *worst* value. An unprioritized request is
always scheduled last among prioritized ones.

### 3.3 Policies, and the one that crashes

`server_args.py:792-807` — choices are `lpm`, `random`, `fcfs`, `dfs-weight`, `lof`,
`priority`, `routing-key`. **Default `fcfs`.**

Implementations live in two enums (`schedule_policy.py:149-162`):
cache-aware `lpm`, `dfs-weight`; cache-agnostic `fcfs`, `lof`, `random`, `routing-key`.

**`priority` is an advertised choice with no implementation.** It matches neither enum, so
`_validate_and_adjust_policy` falls through to
`raise ValueError(f"Unknown schedule_policy: {policy=}")` (`schedule_policy.py:261`).
This is SGLang's counterpart to vLLM's self-falsifying field description: the CLI advertises
something that does not work. It fails loudly at startup rather than quietly at runtime,
which is the better failure — but dorang's config validator must not offer it.

Two silent downgrades to know about: `lpm` degrades to `fcfs` when the waiting queue exceeds
128 (`schedule_policy.py:240-242`), and any cache-aware policy degrades to `fcfs` when the
radix cache is disabled (`:253-255`).

### 3.4 Is priority silently ignored under the default? Yes — but SGLang can be told not to

`--enable-priority-scheduling` defaults to `False` (`server_args.py:808-812`). With it off:

- The protocol layer has no validator for `priority`.
- `tokenizer_manager._set_default_priority` (`:3281-3288`) only *fills*; it never strips or
  rejects.
- The value is forwarded to the scheduler (`tokenizer_manager.py:1279`) and stored on the
  `Req`.
- `SchedulePolicy.calc_priority` returns at `schedule_policy.py:208` without touching the
  queue, because the sort is behind `if self.enable_priority_scheduling`.

So the default is **200 OK, no warning, no effect** — the same shape as vLLM's §1.2, and
undetectable from the response.

**But SGLang provides an opt-in rejection that vLLM has no equivalent of:**

```python
# scheduler.py:2474-2489
elif (not self.enable_priority_scheduling
      and req.priority is not None
      and self.abort_on_priority_when_disabled):
    ... "Using priority is disabled for this server. Please send a new request
         without a priority." ... HTTPStatus.SERVICE_UNAVAILABLE
```

`--abort-on-priority-when-disabled` (`server_args.py:821-825`, default `False`) turns the
silent drop into a **503 with a stable message substring**. That makes SGLang's priority
support *probeable*: dorang can send one throwaway request with a priority and learn from
the response whether the operator's declaration is true.

That probe is only sound when the operator sets that flag, so the recommendation in §8 is
to set **both** `--enable-priority-scheduling` and `--abort-on-priority-when-disabled` — the
first makes priority work, the second makes a misconfiguration loud.

Priority scheduling also **hard-asserts `schedule_policy in {fcfs, lof}`**
(`server_args.py:8413-8416`); it has no effect under `lpm`, `dfs-weight`, `random`, or
`routing-key`, and the server refuses to start rather than degrade.

### 3.5 Preemption — SGLang has it, vLLM does not

VLLM.md §1.2 concludes "priority does not mean preemption." **On SGLang it does**, and it is
on by default whenever priority scheduling is on:

```python
# scheduler.py:1104-1108
self.enable_priority_preemption = (
    self.enable_priority_scheduling and not self.server_args.disable_priority_preemption)
```

`schedule_policy.py:1254-1324` sorts running requests least-important-first, and preempts
those whose priority delta exceeds `--priority-scheduling-preemption-threshold`
(default **10**, `server_args.py:831-835`):

```python
# schedule_policy.py:1291-1293
priority_diff = (req.priority - running_req.priority) * (-priority_sign)
if priority_diff > self.priority_scheduling_preemption_threshold:
```

Preemption is all-or-nothing: if the victims do not free enough tokens, nothing is preempted
(`:1304-1305`).

**A preempted request is re-queued, not aborted.** It goes back through
`_add_request_to_queue` (`scheduler.py:3085-3087`), keeps its `output_ids`, and resumes.
The client sees latency, not an error. Two costs: its KV prefix is **not** re-inserted into
the radix tree (`schedule_batch.py:1755` carries the acknowledging TODO), so each round is a
full recompute; and `retraction_count` is incremented (`:1515`) but never bounded, so a
steady stream of high-priority traffic can starve a low-priority request indefinitely.

**Consequences for DESIGN §7.5's class map.** With the default threshold of 10 and dorang's
spacing `{0, 2, 10}`, only realtime-vs-batch clears the bar (delta 10 is *not* `> 10`;
actually nothing clears it). Either widen the bands or lower the threshold — the SGLang
runbook in §8 recommends `--priority-scheduling-preemption-threshold 5` with bands
`{0, 5, 10}` inverted for direction, so exactly one level of preemption is possible.

**One case where priority does abort.** Queue-full admission
(`scheduler.py:2492-2539`): when `--max-queued-requests` is reached, the lowest-priority
waiting request is evicted with `type: "abort"`, HTTP 503, message
`"The request is aborted by a higher priority request."` Without priority scheduling the
incoming request is rejected instead with `"The request queue is full."`
`--max-queued-requests` defaults to `None`, i.e. unbounded
(`server_args.py:746-750`), and is **ignored entirely in disaggregation mode**
(`scheduler.py:2446-2448`).

### 3.6 What else a gateway can send that influences scheduling

| Field | Cite | Effect |
|---|---|---|
| `lora_path` | `io_struct.py:234` | Hard admission gate — `scheduler.py:3008` skips a request whose adapter would exceed `max_loras_per_batch`. **Same rank-ahead-of-priority rule as VLLM.md §6.** |
| `routed_dp_rank` | `io_struct.py:255` | Pins to a DP worker. Range-validated with a 400 (`tokenizer_manager.py:690-692`) — unlike vLLM's `X-data-parallel-rank`, which ignores malformed values. `data_parallel_rank` is a deprecated alias (`protocol.py:304-313`). |
| `routing_key` | `io_struct.py:259` | Directly reorders the queue under `--schedule-policy routing-key` (`schedule_policy.py:388-410`). Also readable from the `X-SMG-Routing-Key` header (`serving_base.py:275`). |
| `extra_key` / `cache_salt` | `protocol.py:826`, `:828` | Radix namespace — see §5. |
| `session_id`, `session_params` | `io_struct.py:161`, `:231` | Session-tagged KV; mutually exclusive (`io_struct.py:353`). |
| `bootstrap_host/port/room` | `io_struct.py:248-252` | PD-disaggregation targeting. The client supplies a host and port the **server** connects to; treat as privileged. |
| `sampling_params.max_new_tokens` | — | Secondary sort key under `lof` (`schedule_policy.py:360`) and drives the preemption token math (`:1286`). |

**`rid` is declared and dead on the OpenAI routes.** `serving_base.py:140-149`:
`_generate_request_id_base` executes `return None` before the block that would read
`request.rid`. No subclass overrides it. So unlike vLLM — which honors `X-Request-Id`
unconditionally and adopts it as the engine request id — **SGLang gives dorang no way to
correlate a request into engine logs over the OpenAI surface.**

The `x-override-rid` / `x-override-priority` header family
(`request_headers.py:10-19`) applies **only to native `/generate`**
(`http_server.py:835-836`) and only when `SGLANG_ENABLE_REQUEST_HEADER_OVERRIDES=1`
(`environ.py:254`, default False). DESIGN §7.5's `X-Request-Priority` header is inert here.

### 3.7 Request timeouts

There is no per-request deadline field. Two global env vars, both disabled by default:
`SGLANG_REQ_WAITING_TIMEOUT` (`environ.py:430`) aborts queued requests past a deadline with
503 `"Request waiting timeout reached."` (`scheduler.py:2541-2558`), and
`SGLANG_REQ_RUNNING_TIMEOUT` (`environ.py:432`) aborts in-flight ones with 503
`"Request running timeout reached."` (`scheduler.py:1463-1476`).

---

## 4. Metrics

> **This section is a specification for R17, not a description of a running path.** dorang
> ships no metrics scraper and `providers[].metrics` is refused at load (CONFIG §6.2). `least_busy`
> and `highest_tps` rank on dorang's own occupancy and measured throughput and need no poll.

### 4.1 The gate, and why it is better than vLLM's

```python
# http_server.py:280-282
if server_args.enable_metrics:
    add_prometheus_middleware(app)
```

`add_prometheus_middleware` (`utils/common.py:2342-2352`) appends a `Mount("/metrics", ...)`
to `app.routes`. That is the only `/metrics` registration in the HTTP server, and there is no
catch-all route.

**Without `--enable-metrics`, `GET /metrics` is a 404.** This is strictly better than vLLM's
trap (VLLM.md §3.1, last bullet), where `--disable-log-stats` leaves the endpoint returning
200 with zero series and "unreachable" is indistinguishable from "idle." dorang can
distinguish the two cases from the status code alone.

`--enable-metrics` defaults to `False` (`server_args.py:1469-1470`), so a default-launched
SGLang has no metrics at all.

### 4.2 Metrics worth scraping

Names are colon-prefixed `sglang:`. Scheduler metrics carry
`{model_name, engine_type, tp_rank, pp_rank, moe_ep_rank}` plus `dp_rank` when DP is on and
`priority` when priority scheduling is on (`metrics_collector.py:1070-1082`). Tokenizer
metrics carry only `{model_name, engine_type}` plus the same conditionals
(`tokenizer_manager.py:614-624`). **There is no `engine` label** — SGLang's analog is
`engine_type`, whose value is the disaggregation role, not a DP rank.

| Signal | Metric | Cite |
|---|---|---|
| running requests | `sglang:num_running_reqs` | `metrics_collector.py:270` |
| waiting requests | `sglang:num_queue_reqs` (+ `sglang:num_grammar_queue_reqs`) | `:276`, `:282` |
| KV utilization | `sglang:full_token_usage` — **prefer this over `sglang:token_usage`** | `:316`, `:310` |
| time to first token | `sglang:time_to_first_token_seconds` | `:1631` |
| prefix cache | `sglang:cached_tokens_total{cache_source}` — **tokens**, use with `sglang:prompt_tokens_total` | `:1525`, `:1451` |
| preemption | `sglang:num_retracted_requests_total` | `:464` |
| throughput | `sglang:gen_throughput` (token/s) | `:288` |
| HTTP | `sglang:http_requests_total`, `sglang:http_responses_total{status_code}` | `utils/common.py:2383`, `:2389` |
| constants | `sglang:context_len`, `sglang:page_size`, `sglang:max_total_num_tokens` | `metrics_collector.py:1028`, `:1016`, `:992` |

### 4.3 The traps

VLLM.md §3.1 lists four. SGLang has more, and several are worse because the metric is not
merely misnamed but **dead or reset**.

- **`sglang:cache_hit_rate` is hard-reset to `0.0` on every decode report.**
  `metrics_reporter.py:815` sets the local to `0.0` unconditionally at the top of
  `report_decode_stats`, and `:875` assigns it to the gauge. Prefill reports set the real
  value (`:635`, `:655`), but decode reports fire every `--decode-log-interval` iterations
  (default 40) and prefill reports only on extend batches. **The gauge spends most of its
  time at zero**, and any `avg_over_time` over it measures the prefill/decode report ratio
  rather than the hit rate. This is the single most misleading series on the surface, and it
  is exactly the "attractive default" failure mode VLLM.md §3.3 flags for `/load` — except
  here it is the *only* named hit-rate metric. **Compute the rate from
  `sglang:cached_tokens_total` and `sglang:prompt_tokens_total` instead.**
- **`sglang:is_cuda_graph` is dead.** Four references exist in the tree
  (`metrics_collector.py:139`, `:593`, `:594`, `:1339`) and none assigns
  `stats.is_cuda_graph`. It is a permanent `0`, i.e. "CUDA graphs never used." Use
  `sglang:cuda_graph_passes_total{mode}` (`:599`).
- **`sglang:utilization` is stuck at `0`, or literally `-1`.** `metrics_reporter.py:1061-1073`
  guards on `max_running_requests_under_SLO`, which has no setter anywhere (the source
  carries the acknowledging TODO at `:1065`); in PD-prefill mode it is assigned `-1`. A
  negative utilization will pass any naive threshold check.
- **`sglang:engine_startup_time` and `sglang:engine_load_weights_time` are hardcoded `0.0`**
  at their only call site (`scheduler.py:979-980`).
- **`sglang:num_retracted_reqs` and `sglang:num_paused_reqs` are gauges carrying deltas.**
  Reset to zero after each publish (`metrics_reporter.py:890-892`). Their documentation
  strings say "the number of retracted requests." Use the `_total` counters.
- **`sglang:token_usage` is rounded to 2 decimals** (`pool_stats_observer.py:124`) — 1%
  granularity — and is `max(full, swa, mamba)` rather than the KV pool. It is a fraction
  0–1, like vLLM's, and like vLLM's the name does not say so. The source itself carries
  `FIXME: misleadingly named "token_usage"` at `metrics_collector.py:78`.
- **`sglang:inter_token_latency_seconds` has a `_sum`/`_count` scale mismatch.**
  `metrics_collector.py:1725-1738` increments `_sum` by the raw interval once, and the bucket
  by `num_new_tokens`. `rate(_sum)/rate(_count)` — the canonical mean — is wrong. Worse, an
  observation above the top bucket bound matches no bucket and is dropped entirely, so the
  tail is under-reported in both buckets and count.
- **TTFT is never recorded on PD-prefill nodes** (`tokenizer_manager.py:2593-2603`). The
  series silently has zero observations rather than being absent.
- **Only `attn_tp_rank == 0` emits the scheduler gauges** unless
  `--enable-metrics-for-all-schedulers` is set (`metrics_collector.py:1054-1058`). But the
  retract counters, EPLB, and startup constants bypass that gate (`scheduler.py:3232`,
  `metrics_reporter.py:948`, `scheduler.py:972`), so `sum()` across `tp_rank` multiplies
  retract counts by the TP size.

**Two cardinality hazards, both reachable by an unauthenticated caller:**

1. `--tokenizer-metrics-allowed-custom-labels` allow-lists label *keys*, read from the
   `x-custom-labels` header; the **values are arbitrary client strings**
   (`serving_base.py:265-269`, `tokenizer_manager.py:2585-2588`) and land on every tokenizer
   metric.
2. With priority scheduling on, `priority` becomes a label
   (`tokenizer_manager.py:2592`) and `_known_priorities` accumulates every integer ever seen
   and re-emits all of them on each scrape (`metrics_collector.py:1112-1117`).

If dorang forwards client-controlled values into either, it turns a gateway feature into a
denial of service on the backend's metrics registry. Do not.

### 4.4 `/v1/loads` — the endpoint vLLM's `/load` should have been

This is the clearest win over vLLM.

`GET /v1/loads` (`v1_loads.py:80`) is registered **unconditionally**
(`http_server.py:449-451`) and **requires no flag**. It returns, per DP rank
(`load_snapshot.py:183-212`): `num_running_reqs`, `num_waiting_reqs`,
`num_waiting_uncached_tokens`, `num_used_tokens`, `num_total_tokens`, `num_active_tokens`,
`max_total_num_tokens`, `max_running_requests`, `token_usage`, `gen_throughput`,
`cache_hit_rate`, `utilization`, plus optional `memory`, `speculative`, `lora`,
`disaggregation`, `queues` sections selected by `?include=`. `?format=prometheus` renders
the same data in exposition format.

The data is real: schedulers publish a shared-memory snapshot on every prefill and idle
transition, and every `--load-snapshot-publish-interval` decode iterations (default 15,
`server_args.py:1488-1490`; `scheduler.py:3630`, `:3773`).

**Contrast with VLLM.md §3.3 in full.** vLLM's `/load` returns `0` forever without
`--enable-server-load-tracking`, and an unconfigured `0` is the most attractive possible
value to a least-busy router. SGLang's needs no flag and returns live data.

Two failure modes remain, and dorang must code for both:

- **A zero snapshot is written at writer construction** (`load_snapshot.py:329`), before any
  forward pass. Every field is `0`, including `max_total_num_tokens`. **Treat
  `max_total_num_tokens == 0` as "not ready", not "idle."**
- **SHM attach failure returns an empty list with HTTP 200** (`load_snapshot.py:430-433`,
  `:496-497`). `{"loads": []}` means broken, not idle. That is at least distinguishable from
  the zeroed case.

`/get_load` (`http_server.py:768`) is a deprecated shim over the same data with a narrower
field set. Prefer `/v1/loads`.

`/v1/loads?format=prometheus` emits a **second, colliding namespace**: names use
`sglang_` (underscore) rather than `sglang:`, every field is declared `# TYPE ... gauge`
including the genuine cumulative counters `total_prefill_uncached_tokens` and
`total_prefill_busy_us`, and the only label is `dp_rank` (`v1_loads.py:62-72`). Scrape the
JSON form, not this one.

### 4.5 No per-response load headers

vLLM's `endpoint-load-metrics-format: JSON` trick (VLLM.md §3.2) **has no SGLang
equivalent.** The only response header set anywhere in `entrypoints/` is SSE plumbing
(`http_server.py:1858`). `request_headers.py` is inbound-only. For SGLang, `least_busy` must
poll `/v1/loads` — which is cheap and unauthenticated, so this is a smaller loss than it
sounds.

---

## 5. Prefix caching

**RadixAttention is on by default.** `--disable-radix-cache` defaults to `False`
(`server_args.py:896-898`). Two configurations force it off internally
(`server_args.py:3624`, `:3648`).

**Granularity is `page_size` tokens, and the default is 1.** `--page-size` is `None` by
default (`server_args.py:855-859`) and resolved per platform and attention backend:
`_page_size_default` returns **1** on standard CUDA (`arg_groups/overrides.py:1969`), and
various backends override to 16, 64, or 128 (`overrides.py:967`, `:982`, `:1888`, `:1929`).
`match_prefix` truncates the lookup key to a multiple of `page_size` before matching
(`mem_cache/radix_cache.py:399` — `key = key.page_aligned(self.page_size)`).

So SGLang matches at **token granularity by default**, where vLLM's default is 16-token
blocks. Unlike vLLM, the effective value is **readable at runtime** — `sglang:page_size`
(`metrics_collector.py:1016`) and `/server_info`.

### 5.1 dorang's byte-boundary prefix chain should not change

VLLM.md §4 gave three reasons. Checking each against SGLang:

1. **Aligning to the engine's boundary would force tokenization on the hot path.** Holds
   identically, and is now *more* clearly pointless: the default granularity is 1 token, so
   there is nothing to align to.
2. **The effective block size is silently rewritten by the attention backend.** Holds — the
   overrides above are exactly that. But the consequence is milder, because SGLang publishes
   the resolved value rather than hiding it behind a dev endpoint.
3. **Alignment would buy nothing, because the lookup is prefix-chained.** Holds, and is
   directly confirmed: `RadixCache.match_prefix` walks the tree over the token-id sequence
   (`radix_cache.py:405` → `_match_prefix_helper`), so any byte-identical prefix yields an
   identical token prefix and therefore an identical match — regardless of where dorang cut
   its chunks.

**Conclusion: DESIGN §7.4b stays exactly as designed, for both engines.** VLLM.md's
rejection of hot-path tokenization is now confirmed twice rather than once.

### 5.2 `cache_salt` exists here too, with a second field alongside it

```python
# serving_base.py:151-162
def _compute_extra_key(self, request):
    parts = []
    for key in ["cache_salt", "extra_key"]:
        ...
    return "".join(parts) if parts else None
```

Both `cache_salt` and `extra_key` are declared on `CompletionRequest`
(`protocol.py:384`, `:386`) and `ChatCompletionRequest` (`:826`, `:828`), concatenated, and
carried into `RadixKey.extra_key` (`radix_cache.py:75`), which namespaces the whole tree:

> `radix_cache.py:359-367` — *"Entries that share identical leading token ids but have
> different `extra_key` values are intentionally kept disjoint and never share prefix
> nodes."*

**Same trade-off, same recommendation as VLLM.md §4.** It would give DESIGN §7.4a's
`(tenant, group, session)` isolation real force inside the engine, and it would destroy
cross-tenant reuse of shared system prompts. It should be an explicit per-provider option,
**off by default** — and since both engines support it under the same field name, dorang can
express it once.

Three SGLang-specific caveats, and the first is the one that bites:

- **`cache_salt` does not namespace the L3 storage tier.** The hierarchical cache's
  SHA-256 page hash is computed from **token ids only** — `_native_hash_input`
  (`mem_cache/cpp_utils/native_hash.py:45-52`) reads `raw_token_ids` / `token_ids` and never
  touches `extra_key`. Storage keys are namespaced by model name and TP/PP rank
  (`hicache_storage.py:376-386`), not by tenant. **On a `--hicache-storage-backend`
  deployment, two tenants with identical token prefixes share L3 blocks even with distinct
  salts.** If dorang uses `cache_salt` for isolation, it must record that the guarantee holds
  at L1/L2 and *not* at L3, and treat hicache storage as a shared-tenancy surface.
- **The two fields are concatenated without a separator.** `"".join(parts)`
  (`serving_base.py:162`) means `cache_salt="a", extra_key="bc"` and
  `cache_salt="ab", extra_key="c"` land in the same namespace. dorang should populate
  **exactly one** of the two, never both.
- **`cache_salt` cannot be set through `/v1/messages`.** The Anthropic adapter builds its
  `ChatCompletionRequest` from a whitelist (`anthropic/serving.py:540-563`) that omits it.
  Anthropic-shaped traffic needing cache partitioning must be translated to
  `/v1/chat/completions`.

Two things SGLang folds into `extra_key` on its own, which dorang should not duplicate:
the LoRA adapter id (`schedule_batch.py:822-828`), so adapters already get disjoint
subtrees; and the elastic-EP size (`scheduler.py:2100-2104`).

Multimodal inputs are cache-*friendly* here, unlike the usual expectation: image, audio and
video placeholder token ids are overwritten with a content hash
(`schedule_batch.py:146-148`, `mm_utils.py:1104-1106`) so identical media produce identical
token ids and match in the tree. Whisper is the exception — it forces
`disable_radix_cache = True` (`server_args.py:5628-5636`).

### 5.3 Cache administration

`/flush_cache` (`http_server.py:905`) drops the entire radix tree, the request pool, the KV
allocator, the grammar cache, **and resets the metrics** (`scheduler.py:3938-3950`). It is
`ADMIN_OPTIONAL` — i.e. **open by default** (§1.4) and reachable by `GET`. It is not
destructive to in-flight work: it is a no-op returning `success=False` unless the scheduler
is fully idle (`scheduler.py:3960-3966`), and `?timeout=` defers rather than failing. That
makes it less dangerous than vLLM's dev-mode prefix reset, which can preempt running
requests — but it still wipes every tenant's cache at once, and there is no per-prefix or
per-salt flush.

The `/hicache/storage-backend*` family (`:981-1092`) manages the L3 tiers. Attach (PUT),
detach (DELETE), and status (GET) each additionally hard-require `--admin-api-key` to be
configured (`:1026`, `:1058`, `:1082`). **The two clear routes do not** —
`POST /hicache/storage-backend/clear` (`:997`) and the deprecated
`GET /clear_hicache_storage_backend` (`:981`) carry only `ADMIN_OPTIONAL`, so on a
keyless server anyone who can reach the port can wipe a **shared** L3 store used by every
node pointed at it.

There is no API to query, pin, or prefetch a specific prefix. Same as vLLM.

### 5.4 One case where dorang's chunk granularity would matter

Everything in §5.1 holds for the *lookup*. It stops holding if dorang ever consumes SGLang's
KV-event stream for cache-aware routing. That stream publishes a `block_size` equal to
`page_size` and requires subscribers to hash at exactly that size:

> `server_args.py:8812-8813` — *`"block_size": <page_size>,  # subscribers MUST hash prompts
> at this size`*
> `server_args.py:8822-8824` — *"a placeholder `block_size` would cause silent KV-cache
> misses by hashing prompts at the wrong granularity on the router side"*

The events carry a chained `parent_block_hash` (`mem_cache/events.py:60-83`) built from a
SHA-256 chain over page-sized token spans (`cpp_utils/hash_binding.cpp:62-75`, `:203-205`).
A byte-boundary chunker cannot reproduce those keys.

This is not a reason to change §7.4b. It is a reason to keep KV-event routing **out of
scope**: it would require dorang to tokenize on the hot path at the engine's exact page size,
which is the defect §7.4b already rejected, now with a second engine's evidence behind it.

---

## 6. Protocol divergences from OpenAI

Ordered by how quietly they fail.

| # | Divergence | Consequence |
|---|---|---|
| 6.1 | **`tools` without `--tool-call-parser` returns 200 with the tool call as plain text.** Three independent guards short-circuit on `self.tool_call_parser` (`serving_chat.py:826`, `:1532-1536`, `:532`), but the tools are still rendered into the prompt (`:825`). | The model emits its native tool syntax into `message.content`, `tool_calls` is `null`, `finish_reason` is `"stop"`. **Every agentic client breaks with no error.** vLLM 400s in this situation; SGLang does not. This is the highest-severity item in §6. |
| 6.2 | **Five error envelope shapes on one server.** (a) flat `{"object","message","type","param","code"}` for OpenAI routes (`serving_base.py:225`); (b) *nested* `{"error":{...}}` for the same errors when streaming (`:241`); (c) OpenAI-nested for `/v1/responses` (`http_server.py:531-539`); (d) Anthropic-shaped for `/v1/messages` (`:483-491`); (e) `{"error": "Unauthorized"}` — a bare **string** — from the auth middleware (`utils/auth.py:188-197`). | An OpenAI SDK cannot parse (a). `code` is an int status. `type` is one of `"BadRequestError"`, `"BadRequest"`, `"Bad Request"`, or the stringified status `"400"`, depending on which of four call paths produced it. **The only stable discriminators are the HTTP status and `object == "error"`.** dorang must normalize per COMPATIBILITY 7.1 and must not branch on `type`. |
| 6.3 | **Every streaming chunk carries `reasoning_content` and `matched_stop`, including as explicit `null`.** `StreamDelta.reasoning_content` is declared without a default *deliberately* so it always serializes (`sse_utils.py:16-23`), and `StreamChoice` has no `omit_defaults` (`:28-35`). | Violates COMPATIBILITY 2.1 ("absent fields omitted, never null") and 2.2 ("nothing more") simultaneously. dorang must strip both on the compat egress path. Non-streaming responses have the same problem plus an unrequested `metadata: {"weight_version": ...}` (`serving_chat.py:1615`). |
| 6.4 | **`finish_reason` can be `abort`.** Internal types are `stop`, `length`, `abort` (`schedule_batch.py:163`, `:199`, `:213`), passed through verbatim (`serving_chat.py:1573`); `tool_calls` is synthesized post-hoc from `stop` (`:1338-1339`, `:1734-1736`). `content_filter` and `function_call` are declared in the `Literal` (`protocol.py:1067`) and never produced. | Same `abort` value vLLM emits. **`repetition` does not exist here** — a grep finds no such finish reason. COMPATIBILITY §4 needs an `abort` row, which it already needs for vLLM. |
| 6.5 | **Unknown request fields are silently dropped**, on OpenAI *and* Anthropic models. Neither `ChatCompletionRequest` (`protocol.py:720`), `CompletionRequest` (`:317`), nor `AnthropicMessagesRequest` (`anthropic/protocol.py:361`) declares a `model_config`; Pydantic v2 defaults to `extra='ignore'`. The only `extra="allow"` in the file is on `TokenizeRequest` (`protocol.py:1325`). | Identical to vLLM's §2.4. A typo returns 200 with default behavior. dorang cannot rely on either engine to validate anything it passes through. |
| 6.6 | **`max_tokens` defaults differ between the two OpenAI endpoints, and one rejects `null`.** `/v1/completions`: `max_tokens: int = 16` (`protocol.py:330`) — **non-Optional**, so an explicit `null` is a **400**, where vLLM coerces it back to 16. `/v1/chat/completions`: both `max_tokens` and `max_completion_tokens` default to `None` and `None` is forwarded (`:994` — `self.max_completion_tokens or self.max_tokens`). | dorang must send a real integer to `/v1/completions` for both engines, and must never send `null` there for SGLang. Note the `or`: `max_completion_tokens: 0` silently falls through to `max_tokens`. |
| 6.7 | **`usage.reasoning_tokens` is a top-level field, not `completion_tokens_details.reasoning_tokens`** (`protocol.py:203`), and defaults to `0` so it is always emitted. `total_tokens` excludes it (`usage_processor.py:123`). | DESIGN §10.7's `ReasoningTokens` mapping needs an SGLang-specific read. `completion_tokens_details` does not exist on this server. |
| 6.8 | **`prompt_tokens_details` is `null` unless `--enable-cache-report` AND `cached_tokens > 0`.** Flag defaults `False` (`server_args.py:1319-1323`); the object is `None` on a zero count (`usage_processor.py:15`). | Exactly VLLM.md §5's `--enable-prompt-tokens-details` row: **without it, DESIGN §8's cost engine prices cached prompt tokens at full rate.** |
| 6.9 | **`tool_choice: null` with `tools` present is normalized to `"auto"`** (`protocol.py:858-866`) — the *opposite* of vLLM, where an explicit null silently disables tool parsing. But `tool_choice: "none"` **removes the tool schemas from the prompt entirely** (`serving_chat.py:816`), where OpenAI still shows them to the model. | dorang's vLLM-motivated normalization of literal `null` → `"auto"` is harmless here and should stay. The `"none"` behavior is a real semantic difference that changes model output. |
| 6.10 | **`--reasoning-parser` is required or `reasoning_content` is always null.** Both gates require the server flag (`serving_chat.py:501`, `:1508`); `separate_reasoning: true` (the request default, `protocol.py:803`) is accepted and ignored without it, leaving `<think>` tags inline in `content`. A parse failure returns **500**, not 400 (`serving_chat.py:1524-1528`). | Same shape as vLLM's `--reasoning-parser` row. **The field name differs from vLLM**: SGLang uses `reasoning_content`, vLLM's response field is `reasoning`. DESIGN §10.2's reverse mapping needs both. |
| 6.11 | **The `model` field is not validated on `/v1/chat/completions` or `/v1/messages`.** `_validate_request` (`serving_chat.py:603-659`) checks messages, tool_choice, tool schemas, and token budget — never the model name. SGLang's own docs state this explicitly. | vLLM 404s on a model mismatch, which VLLM.md §2.10 correctly reads as a *routing* failure signature. **SGLang gives no signal at all** — a misrouted request is served by whatever model is loaded. dorang must verify the model name against `/v1/models` at startup rather than relying on the request to fail. |
| 6.12 | **A colon in the model name selects a LoRA adapter.** `_parse_model_parameter` (`serving_base.py:40-53`) splits `model` on the first colon and treats the suffix as an adapter name, overriding `lora_path`. `served_model_name` is asserted colon-free at startup (`server_args.py:8350-8353`). | A client-facing model alias like `llama3:8b` or `qwen:7b-instruct` — the Ollama convention — is silently reinterpreted. dorang must not pass through colons in the upstream model name for this backend. |
| 6.13 | **`user`, `best_of`, `suffix`, `service_tier`, `store`, `metadata`, and `CompletionRequest.custom_labels` are all accepted and dropped.** `best_of` (`protocol.py:325`) and `suffix` (`:337`) have exactly one reference each — their declaration. `service_tier`/`store`/`metadata` are not request fields at all outside `/v1/responses`. | vLLM has three different behaviors for three unsupported fields (§2.7). SGLang has one: drop everything. Simpler, and equally undetectable. **`service_tier` must never be used as a priority fallback for either backend.** |
| 6.14 | **`--allow-auto-truncate` turns context overflow into a silent 200.** Default `False` (`server_args.py:1378-1382`); when set, both the input-length and total-token checks log a warning and truncate instead of raising (`tokenizer_manager.py:1059-1066`, `:1080-1088`). | dorang's `context_window` fallback (DESIGN §7.6) has **no signature to match** on such a deployment, and the client silently receives a completion over a truncated prompt. This flag must be recorded in the provider config and treated as a capability downgrade. |
| 6.15 | **`stream_options.include_usage: false` can be overridden ON by the server.** `--stream-response-default-include-usage` (`server_args.py:1397-1401`) forces usage into every stream (`entrypoints/openai/utils.py:84-85`). | COMPATIBILITY 3.1 requires usage *only* when the client asked. dorang must strip an unrequested usage chunk on egress. |
| 6.16 | **Extra SSE chunk types.** A `hidden_states` chunk (`serving_chat.py:1374`) and an `sglext` chunk with `choices: []` (`:1391-1401`) can appear mid-stream. The two usage chunks serialize differently: chat does not exclude nulls (`:1434`), completions does (`serving_completions.py:461`). | dorang's SSE accumulator must tolerate unknown chunk shapes rather than assuming every frame has a `choices[0].delta`. |

### 6.17 Context-window overflow signature

Five distinct wordings, none identical to vLLM's:

| Condition | Text | Cite |
|---|---|---|
| input alone | `The input (N tokens) is longer than the model's context length (M tokens).` | `tokenizer_manager.py:1069-1070` |
| input + max_tokens | `Requested token count exceeds the model's maximum context length of M tokens. ...` | `:1092-1097` |
| output budget | `max_completion_tokens is too large: N.This model supports at most M completion tokens.` (the missing space is in the source) | `serving_chat.py:650-651` |
| scheduler-side | `Input length (N tokens) exceeds the maximum allowed length (M tokens).` | `managers/utils.py:211-215` |
| multimodal | `Multimodal prompt is too long after expanding multimodal tokens.` | `scheduler.py:2334`, `:2616` |

**VLLM.md §2's recommended substring `"maximum context length"` matches only the second of
these.** The cross-engine substring is the shorter **`"context length"`**, which matches
vLLM's three wordings and SGLang's first two — and still misses SGLang's other three.

The honest conclusion: **prefer the pre-computed path from §2.** Signature matching is a
backstop, it must be per-engine, and on a `--allow-auto-truncate` deployment it will never
fire at all.

---

## 7. Normalization table

Canonical names are the `CanonicalRequest` / `CanonicalResponse` fields from DESIGN §10.7.
Marks: **=** identical · **~** differs but mappable · **✗** not expressible.

### 7.1 Request

| Canonical | vLLM | SGLang | |
|---|---|---|---|
| `Model` | `model`; **404 on mismatch** | `model`; **not validated**, and a `:` selects a LoRA adapter (`serving_base.py:40-53`) | ~ |
| `Messages` | `messages[]` | `messages[]` | = |
| `System` | `messages[role=system\|developer]` | same; Anthropic `system` flattened to a string (`anthropic/serving.py:136-153`) | = |
| `MaxOutputTokens` | `max_completion_tokens` → `max_tokens`; completions defaults 16, `null` coerced to 16 | same precedence (`protocol.py:994`); completions defaults 16 but **`null` is a 400** (`:330`) | ~ |
| `Temperature` / `TopP` | same | same | = |
| `TopK` | `top_k` (extension) | `top_k` (`protocol.py:788`) | = |
| `Stop` | `stop` | `stop`, plus `stop_token_ids` and `stop_regex` (`protocol.py:794-795`) | = |
| `Stream` | `stream` | `stream` | = |
| `Tools` | `tools[].function`; **400 without the parser flag** | `tools[].function`; **200 with tool syntax as plain text** without `--tool-call-parser` (§6.1) | ~ |
| `ToolChoice` | `tool_choice`; explicit `null` **disables parsing** | normalized `null` → `"auto"`; `"none"` **strips schemas from the prompt** | ~ |
| `ParallelToolCalls` | `parallel_tool_calls`; **post-filters the response** | forwarded into grammar construction (`serving_chat.py:834`, `:846`); with `tool_choice:"auto"` and no constraint there is **no enforcement path** | ~ |
| `ResponseFormat` | `response_format` + nested structured-output object | `response_format`, plus `regex`, `ebnf`, `json_schema`, `structural_tag` | ~ |
| `Reasoning` | `reasoning_effort`; needs `--reasoning-parser` | `reasoning_effort` (extended: accepts a float 0–0.99 and `"max"`, `protocol.py:706-717`) + `separate_reasoning`; needs `--reasoning-parser` | ~ |
| `Seed` | `seed` | `seed`, renamed to `sampling_seed` internally (`protocol.py:1013`) | = |
| `Logprobs` | `logprobs`, `top_logprobs` | same, but `top_logprobs` is **not gated on `logprobs: true`** as OpenAI requires | ~ |
| `EndUser` | `user` — declared and explicitly ignored | `user` — declared and silently dropped; Anthropic `metadata.user_id` also dropped | ✗ |
| `Metadata` | `metadata` | **not a request field** on chat/completions | ✗ |
| `PreviousResponseID` | `previous_response_id` | `/v1/responses` only | ~ |
| `Store` | `store` | `/v1/responses` only | ~ |
| `ServiceTier` | `service_tier` — accepted, **zero consumers** | **not a field** on chat/completions | ✗ |
| `CacheBreakpoints` | — | Anthropic `cache_control` is **dropped by `extra='ignore'`** | ✗ |
| `Priority` | `priority`, **lower-first**, ignored under default `fcfs` | `priority`, **higher-first by default**, ignored unless `--enable-priority-scheduling`; dead on `/v1/responses`; absent from `/v1/messages` | ~ **direction-inverted** |
| `CacheSalt` | `cache_salt`; partitions the KV cache | `cache_salt` + `extra_key`, concatenated **without a separator**; partitions L1/L2 but **not the L3 storage tier**; not settable via `/v1/messages` | ~ |
| `RequestID` | `X-Request-Id` honored unconditionally | `rid` field is **dead code** (`serving_base.py:140-142`); header override is `/generate`-only and env-gated | ✗ |
| `DPRank` | `X-data-parallel-rank` header; malformed **silently ignored** | `routed_dp_rank` body field + `X-Data-Parallel-Rank` header; out-of-range is a **400** | ~ |

### 7.2 Response and usage

| Canonical | vLLM | SGLang | |
|---|---|---|---|
| `ID` | `id` | `id` | = |
| `Model` | `model` | `model` | = |
| `StopReason` | `stop`, `length`, `tool_calls`, `content_filter`, **`abort`**, **`repetition`** | `stop`, `length`, `tool_calls`, **`abort`**. `content_filter`/`function_call` declared, never emitted. No `repetition`. | ~ |
| `StopSequence` | — | **`matched_stop`** on every choice (`protocol.py:1070`) — the matched token id or string. Present on the OpenAI surface, **discarded by the Anthropic adapter**. | ~ **SGLang-only** |
| `InputTokens` | `usage.prompt_tokens` (inclusive of cache reads) | `usage.prompt_tokens` (inclusive) | = |
| `OutputTokens` | `usage.completion_tokens` | `usage.completion_tokens` | = |
| `CacheReadTokens` | `usage.prompt_tokens_details.cached_tokens`; needs `--enable-prompt-tokens-details` | `usage.prompt_tokens_details.cached_tokens`; needs `--enable-cache-report`, **and is `null` when the count is 0** | ~ |
| `CacheWriteTokens` | — | — (`cache_creation_input_tokens` declared, never written) | ✗ |
| `ReasoningTokens` | — | **`usage.reasoning_tokens`, top level** (`protocol.py:203`), always present, defaults 0 | ~ **SGLang-only, non-OpenAI position** |
| `TotalTokens` | `usage.total_tokens` | `usage.total_tokens` = prompt + completion, **excludes reasoning** (`usage_processor.py:123`) | = |
| `ReasoningText` | `reasoning` (response), `reasoning_content` accepted on request | **`reasoning_content`** both ways (`protocol.py:1057`, `:1111`) | ~ **different field name** |
| error envelope | `{"error":{message,type,param,code:int}}` | **flat** `{object:"error",message,type,param,code:int}`; nested only when streaming | ~ **structural** |
| `logprob` sentinel | `-9999.0` stands in for `-inf` | not observed; **UNVERIFIED** | ? |
| multimodal token detail | — | `prompt_tokens_details.{image,audio,video}_tokens` (`protocol.py:182-184`) | ~ **SGLang-only** |

### 7.3 Anthropic surface

| Canonical | vLLM `/v1/messages` | SGLang `/v1/messages` | |
|---|---|---|---|
| endpoint | present, unknown keys dropped | present, native model, adapter to the OpenAI pipeline | = |
| events | — | `message_start`, `content_block_{start,delta,stop}`, `message_delta`, `message_stop`, `error`. **No `ping`, no `[DONE]`.** | = matches COMPATIBILITY 6.2 |
| block types | — | `text`, `thinking`, `tool_use`, plus `signature_delta` | = |
| `StopReason` | — | three values only; everything else → `end_turn` with a WARNING | ~ same collapse as COMPATIBILITY 6.4 |
| `StopSequence` | — | **declared in the `Literal`, never emitted**; key omitted (not null) by `exclude_none` | ✗ |
| `TotalTokens` | — | **absent** — SGLang does not reproduce COMPATIBILITY 6.8's asymmetry | ✗ |
| `CacheWriteTokens` | — | never written | ✗ |
| `CacheBreakpoints` | — | `cache_control` dropped by `extra='ignore'` | ✗ |
| `EndUser` | — | `metadata` accepted, never read | ✗ |
| `Priority` | absent | absent | ✗ |
| server tools | — | `web_search_*` / `computer_*` / `bash_*` / `text_editor_*` parsed, then **skipped with an INFO log** | ✗ |
| `thinking.budget_tokens` | — | accepted, warned, **not enforced** | ✗ |
| error envelope | — | correct Anthropic shape | = |

**The five ✗ rows on the Anthropic surface are the reason dorang cannot treat SGLang's
`/v1/messages` as a pure passthrough.** It is a very good adapter; it is still an adapter,
and dorang's own Anthropic egress path must fill in `stop_sequence`, `total_tokens`, and the
cache-write counter, and must reject or degrade `cache_control` explicitly rather than
letting it vanish.

---

## 8. Operator configuration profile

What an operator must set so that standard OpenAI and Anthropic protocol behavior applies
as-is. Side by side, with what silently breaks when it is missing.

### 8.1 Both engines — the shared runbook

| Concern | vLLM | SGLang | Without it |
|---|---|---|---|
| tool calling | `--enable-auto-tool-choice` + `--tool-call-parser <p>` | `--tool-call-parser <p>` | vLLM: **400**. SGLang: **200 with the tool call as plain text.** SGLang's failure is the dangerous one. |
| reasoning | `--reasoning-parser <p>` | `--reasoning-parser <p>` | The reasoning field is always null and `<think>` tags stay inline in `content`. |
| cached-token billing | `--enable-prompt-tokens-details` | `--enable-cache-report` | `cached_tokens` is null → **DESIGN §8 prices cached prompt tokens at full rate.** |
| priority | `--scheduling-policy priority` | `--enable-priority-scheduling` | `priority` accepted, ignored, 200. |
| metrics | do **not** pass `--disable-log-stats` | `--enable-metrics` | vLLM: 200 with zero series, indistinguishable from idle. SGLang: **404**, which is honest. |
| context cap | `--max-model-len` | `--context-length` | Both report the effective value on `/v1/models`. SGLang **refuses to widen** past the model's own value; vLLM warns. |

### 8.2 SGLang-specific — set these

| Flag | Cite | What it does | Without it |
|---|---|---|---|
| `--enable-priority-scheduling` | `server_args.py:808` | Makes `priority` do anything | Silently ignored (§3.4) |
| `--schedule-low-priority-values-first` | `:826` | **Makes SGLang agree with vLLM on direction** | dorang's `{realtime:0, batch:10}` map is **inverted** — batch outranks realtime |
| `--abort-on-priority-when-disabled` | `:821` | Turns an ignored priority into a 503 with a stable message | dorang cannot probe whether priority works |
| `--default-priority-value <n>` | `:816` | Fills unspecified priorities | Unprioritized requests get `±sys.maxsize`, i.e. scheduled last, **and** emit a `priority="None"` Prometheus label |
| `--priority-scheduling-preemption-threshold 5` | `:831` | Lets one band preempt the next | Default 10 means dorang's `{0,2,10}` bands never preempt at all (§3.5) |
| `--enable-cache-report` | `:1319` | Populates `cached_tokens` | Cost engine over-bills every cached request |
| `--tool-call-parser <p>` | `:1346` | Structured `tool_calls` | §6.1 — the worst silent failure on this backend |
| `--reasoning-parser <p>` | `:1324` | Populates `reasoning_content` | Always null |
| `--enable-metrics` | `:1469` | Registers `/metrics` | 404 |
| `--enable-metrics-for-all-schedulers` | `:1483` | Per-rank scheduler gauges | Under DP attention, every rank's data collapses onto TP-0's series |
| `--max-queued-requests <n>` | `:746` | Bounds the waiting queue | Unbounded; the queue grows until memory does |
| `--served-model-name <n>` | `:1291` | Names the model on `/v1/models` | Defaults to the full `model_path`. **Must not contain a colon** (`:8350`) |

### 8.3 SGLang-specific — do **not** set these

| Flag | Why |
|---|---|
| `--schedule-policy priority` | Advertised choice with **no implementation**; crashes scheduler init (§3.3) |
| `--allow-auto-truncate` | Turns context overflow into a **silent 200 over a truncated prompt** (§6.14). dorang's `context_window` fallback stops working |
| `--tokenizer-worker-num > 1` *with* `--api-key` | **The auth middleware is never installed** in multi-tokenizer mode (`http_server.py:2431`, `:2460`). The server is fully open |
| `--stream-response-default-include-usage` | Forces usage into streams the client did not ask for, violating COMPATIBILITY 3.1 |
| `--tokenizer-metrics-allowed-custom-labels` with client-derived values | Label *keys* are allow-listed; **values are not** (§4.3) |

### 8.4 Security posture — read before exposing an SGLang backend

SGLang's admin surface is larger than vLLM's and open by default. On a default launch, with
no keys configured, an unauthenticated caller can flush the radix cache, pause generation,
throttle the scheduler, update weights, load and unload LoRA adapters, and read
`/server_info`.

Minimum posture for a backend dorang talks to:

1. **Never expose the SGLang port outside the trust boundary.** Both `--api-key` and
   `--admin-api-key` are bearer-token comparisons with no rate limiting, and `/health*` and
   `/metrics` bypass them by prefix (`utils/auth.py:100-101`).
2. **Set `--admin-api-key` in addition to `--api-key`**, so `ADMIN_OPTIONAL` routes stop
   accepting the ordinary key (`utils/auth.py:128-132`).
3. **Treat `/server_info` as a secret-bearing endpoint.** It returns `api_key` and
   `admin_api_key` in cleartext (§2.3). If dorang reads it for capability discovery, that
   must be opt-in per provider and the response must never be logged.
4. **Keep `--tokenizer-worker-num` at 1** on any server where `--api-key` is set.
5. **If `--hicache-storage-backend` points at a shared store, note that its clear routes are
   not admin-gated** (§5.3) and that `cache_salt` does not isolate tenants at that tier
   (§5.2). A shared L3 store is a shared-tenancy surface with an unauthenticated wipe.
6. CORS is `*` with credentials, unconditionally (`http_server.py:433-438`). A browser on
   any origin can reach every route the network allows.

---

## 9. Unverified, and where SGLang's documentation disagrees with its source

### 9.1 Unverified

Recorded rather than assumed:

- The release number for this commit — shallow clone, no tags.
- Whether `POST /v1/tokenize` returns a context-length field the way vLLM's does. The route
  exists (`http_server.py:1693`); its response model was not read.
- Whether SGLang emits a `-9999.0`-style logprob sentinel. No such constant was found, but
  the sampling subtree (`python/sglang/srt/sampling/`) was not in the checkout.
- What `max_new_tokens=None` resolves to downstream on `/v1/chat/completions`. Same missing
  subtree. What *is* verified is that the total-token context check is skipped entirely when
  it is `None` (`tokenizer_manager.py:1074-1078`).
- The actual enforcement of `parallel_tool_calls`. It is forwarded into grammar construction
  (`serving_chat.py:834`), but `python/sglang/srt/function_call/` was not in the checkout, so
  whether the constraint restricts generation to one call is unknown.
- Whether `/v1/messages` correctly resets `content_block_index` across a tool-use turn. The
  helpers exist (`anthropic/serving.py:857-915`); the ordering was not traced end to end.
- Whether `--enable-metrics-for-all-schedulers` changes the `/v1/loads` snapshot. The two
  read different sources, but the runtime effect was not observed.
- Behavior of the ZMQ load-snapshot transport under `enable_dp_attention` with `nnodes > 1`
  (`load_snapshot.py:64-122`). The SHM path was traced; the ZMQ ownership election was not.
- Where `disable_finished_insert` is ever set true. It defaults to `False`
  (`mem_cache/cache_init_params.py:30`) and suppresses radix insertion when set
  (`mem_cache/radix_cache.py:441-443`), but no assignment site exists in the checked-out tree.
- Whether the deterministic-inference and `--enable-mis` paths that force
  `disable_radix_cache` also disable the `sglang:cache_hit_rate` gauge, or leave it
  publishing zeros indistinguishable from §4.3's reset bug.

### 9.2 Where SGLang's own documentation is wrong or incomplete

Source was preferred throughout. `docs/` does not exist at this commit — the documentation
tree is `docs_new/`, which is the Mintlify site.

| Claim | Reality |
|---|---|
| `--schedule-policy priority` is an accepted value (`server_args.py:802` `choices`) | No implementation; `raise ValueError(f"Unknown schedule_policy: {policy=}")` at `schedule_policy.py:261` |
| `sglang:num_retracted_reqs` is *"The number of retracted requests"* (`metrics_collector.py:459`) | It is a delta since the last publish, reset to 0 each time (`metrics_reporter.py:890-892`) |
| `sglang:token_usage` — the name implies token count | It is a fraction 0–1, rounded to 2 decimals, and is `max(full, swa, mamba)`. The source itself carries `FIXME: misleadingly named` at `metrics_collector.py:78` |
| `sglang:utilization` — *"The utilization"* (`metrics_collector.py:567`) | Permanently 0, or `-1` in PD-prefill mode; its input has no setter (`metrics_reporter.py:1061-1073`) |
| `sglang:is_cuda_graph` | Never assigned anywhere; permanently 0 |
| `sglang:engine_startup_time`, `sglang:engine_load_weights_time` | Hardcoded `0.0` at the only call site (`scheduler.py:979-980`) |
| Observability doc: *"You can enable them by adding `--enable-metrics`"* (`docs_new/docs/advanced_features/observability.mdx`) | Correct, but omits that `/metrics` is a **404** without it, and omits `--enable-metrics-for-all-schedulers` entirely |
| Anthropic doc: *"SGLang does not validate the request `model` field"* (`docs_new/docs/basic_usage/anthropic_api.mdx`) | Correct for `/v1/messages` and `/v1/chat/completions`, but `GET /v1/models/{model}` **does** 404 on a mismatch (`http_server.py:1817-1828`). Two endpoints on one server disagree about whether the model name means anything |
| Anthropic doc: *"any client built for the Anthropic Messages API … can talk to a self-hosted SGLang server without changes"* | True for the common path. Silently untrue for `cache_control`, `metadata`, `stop_sequence`, `usage.cache_creation_input_tokens`, `thinking.budget_tokens`, and the four server-tool families — each accepted and dropped (§1.2) |
| Anthropic doc's `--tool-call-parser` note: *"tool schemas are still accepted but the model's tool calls come back as raw text"* | Accurate, and worth crediting — the docs name the §6.1 failure explicitly, where the source does not |

### 9.3 One documentation finding worth acting on

`docs_new/docs/basic_usage/anthropic_api.mdx` documents that Claude Code prepends a
per-request attribution block to the system prompt whose hash changes every turn, defeating
radix prefix reuse, and that `CLAUDE_CODE_ATTRIBUTION_HEADER=0` removes it.

**That failure mode is not SGLang-specific — it applies to any gateway, including dorang.**
A per-request-varying token near the head of the prompt collapses prefix reuse to whatever
precedes it. dorang's cache-affinity design (DESIGN §7.4) should treat "a varying prefix
injected by the client" as a first-class hazard, and dorang must not introduce one of its
own by prepending request-scoped metadata to a forwarded prompt.
