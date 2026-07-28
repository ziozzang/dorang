# dorang — Design

> Revision 2 · 2026-07-28
> Revision 1 was reviewed adversarially; see [REVIEW.md](REVIEW.md). Every change below
> that carries a **[R1-n]** tag exists because revision 1 was wrong about something.
>
> 한국어: [DESIGN.ko.md](DESIGN.ko.md)

---

## 0. Scope

### 0.1 What dorang is

A single-binary LLM gateway in Go. It authenticates callers, routes each request to one of
several interchangeable backends, enforces capacity and spend limits that match how provider
plans actually work, and records what happened — **without the recording being the thing you
turn off to make it fast.**

### 0.2 Scale range: notebook to enterprise

The same binary and the same configuration file must work across this whole range. You
scale by changing configuration, not by changing deployment model.

| | **Notebook** | **Team** | **Enterprise** |
|---|---|---|---|
| Storage | SQLite, embedded | PostgreSQL | PostgreSQL, partitioned |
| Coordination | none | none | Redis or PostgreSQL leases |
| Nodes | 1 | 1–2 | N, leader-elected maintenance |
| Capacity accuracy | exact | exact | exact (shared) or bounded-overshoot (leased) |
| Telemetry | in-process, sampled | full ledger | ledger + rollups + retention |
| Required dependencies | **0** | 1 | 2 |
| Target throughput | 100s req/s | 1000s req/s | 10k+ req/s per node |

Two consequences the design must honor everywhere:

- **Nothing may be mandatory that a notebook cannot run.** Redis, a metrics stack, and a
  separate database are all optional. A single `dorang` process with a config file must serve
  requests with no other software present.
- **Nothing may be structurally single-node.** Every mechanism that holds state declares how
  it behaves with N nodes, including "refuses to run with N > 1" as an explicit answer.

### 0.3 Full protocol, and speed as a co-requirement

Inference protocols are implemented **in full**, streaming included. Provider-native routes
are covered by one generic passthrough engine (§10.6) rather than one adapter each.
Administrative and enterprise-adjacent surfaces are filled in over time, and unimplemented
routes answer `501` with a reason — never a silent `404`.

Speed is not a constraint applied to that; it is a co-requirement. Adding a protocol must
not add hot-path cost. That is precisely why §10.1 uses a neutral intermediate representation
(N+M adapters, not N×M), why §10.6 exists, and why §15 is non-negotiable.

### 0.4 Requirements traceability

| # | Requirement | Section |
|---|---|---|
| R1 | Absorb a declarative model-list/router configuration model | §2, §4 |
| R2 | Provider configuration, overridable per provider | §4.3 |
| R3 | Multi-key/multi-provider with **per-key** *and* **per-key-per-model** concurrency | **§5** |
| R4 | Rolling quota exhaustion moves traffic to another key | §6 |
| R5 | Cost ceiling stops spending | §6.4 |
| R6 | Fail-back to a different model in the same class | §7.6 |
| R7 | Alias in, real model upstream, both reported back | §7.2, §10.4 |
| R8 | Per-key usage, timing, tracing, cumulative cost | §9, §12 |
| R9 | Cost varying by model/provider/time/token-length, and a way to render it | §8 |
| R10 | Multi-user, bearer auth, key management with a UI | §11 |
| R11 | PostgreSQL, docker-compose for tests | §9, §14 |
| R12 | Cache-sticky routing, TTL skip, ordered prefix matching | **§7.4** |
| R13 | Priority by cost or by measured throughput | §7.5 |
| R14 | Two or more nodes, highly available | §13 |
| R15 | chat/completions, messages, responses; metadata and reasoning-level mapping | §10 |
| R16 | Cost and statistics in extension headers | §10.4 |
| R17 | Backend metrics integration | §12.4 |
| R18 | Pass scheduling priority through to the backend | §7.5, §10.5 |
| R19 | Batch API | §11.1 |
| R20 | Go, with rigorous unit and scenario tests | §14 |
| R21 | User/team administration, email (API-defined, Lua-extensible) | §11.4, §11.5 |
| R22 | High throughput, low footprint, fast response, correct tracing | **§15** |

---

## 1. Architecture

```
                         ┌──────────────────────────────────────────────┐
   OpenAI SDK   ─┐       │            dorang (one binary)               │
   Anthropic SDK ├─HTTP─▶ ┌──────────────────────────────────────────┐ │
   Coding agents ─┘       │ L1  Frontend — protocol adapters          │ │
   Web UIs                │     openai-chat · openai-responses        │ │
                          │     anthropic-messages · embeddings       │ │
                          │     rerank · audio · images · batch       │ │
                          └───────────────┬──────────────────────────┘ │
                          │               ▼ CanonicalRequest            │
                          │ ┌──────────────────────────────────────┐   │
                          │ │ L2  Gate — auth, quota, admission     │   │
                          │ └───────────────┬──────────────────────┘   │
                          │ ┌───────────────▼──────────────────────┐   │
                          │ │ L3  Router — alias → group → target   │   │
                          │ │     sticky · prefix · cost · latency  │   │
                          │ │     fallback chain · circuit breaker  │   │
                          │ └───────────────┬──────────────────────┘   │
                          │ ┌───────────────▼──────────────────────┐   │
                          │ │ L4  Capacity — multi-axis reservation │   │
                          │ └───────────────┬──────────────────────┘   │
                          │ ┌───────────────▼──────────────────────┐   │
                          │ │ L5  Backend — provider adapters       │   │
                          │ └───────────────┬──────────────────────┘   │
                          │ ┌───────────────▼──────────────────────┐   │
                          │ │ L6  Meter — async accounting + trace  │   │
                          │ └──────────────────────────────────────┘   │
                          └────┬───────────────┬──────────────┬─────────┘
                               ▼               ▼              ▼
                         SQLite | PostgreSQL  Redis(opt)   upstream APIs
```

One process. No worker-process split — the Go runtime uses the cores. Redis is optional
and only becomes necessary for exact shared capacity across nodes (§5.6).

---

## 2. Interoperability

dorang is not a fork or a rewrite of any specific product. It interoperates on two levels
because that is what makes it adoptable, and both are treated as **features with tests**,
not as compatibility debt.

### 2.1 Wire interoperability — strict

Client SDKs must work unmodified. These are byte-level contracts covered by golden tests.

| Path | Notes |
|---|---|
| `POST /v1/chat/completions`, `/chat/completions` | streaming SSE included |
| `POST /v1/completions` | legacy |
| `POST /v1/embeddings` | |
| `POST /v1/rerank`, `POST /v2/rerank` | |
| `POST /v1/messages`, `/v1/messages/count_tokens` | Anthropic surface |
| `POST /v1/responses`, `GET`/`DELETE /v1/responses/{id}` | |
| `POST /v1/audio/speech`, `/v1/audio/transcriptions`, `/v1/audio/translations` | |
| `POST /v1/images/generations`, `/v1/images/edits`, `/v1/images/variations` | |
| `POST /v1/moderations`, `POST /v1/ocr` | |
| `GET /v1/models`, `GET /v1/models/{id}` | |
| `POST /v1/files`, `GET`/`DELETE /v1/files/{id}`, `GET /v1/files/{id}/content` | |
| `POST /v1/batches`, `GET /v1/batches[/{id}]`, `POST /v1/batches/{id}/cancel` | |
| `GET /health`, `/health/liveliness`, `/health/readiness` | container probes |
| `GET /metrics` | Prometheus |

Authentication accepts `Authorization: Bearer …`, `x-api-key`, and `api-key`.

#### Model names are opaque — one rule, no exceptions **[R1-C3]**

Real model names use `:` in at least three different ways: a family tag (`family:31b`), a
vendor prefix (`vendor:model-5.1`), and a deployment variant (`model-flash:cloud`). A
gateway that splits on `:` in one code path and preserves it in another makes identity,
routing, aliasing, and prefix affinity **all** non-deterministic for the same model.

Revision 1 stated the rule and then broke it — provider-prefix inference, `provider/model`
splitting at import, and an unspecified group-id normalization feeding the prefix hash.
Revision 2 fixes exactly one rule:

1. **A model name is an opaque string.** No component splits it on any character.
2. **Provider identity comes only from explicit configuration fields** — `providers[].name`
   and `deployments[].upstream_model` — never from parsing the model string.
3. **Prefix-based capability inference** (§4.3) reads the *configured* upstream model to
   choose **defaults only**. It never affects identity, routing, or hashing.
4. `model_group_id` has **one** normalization function, used by every caller including the
   prefix hash seed (§7.4b). One function, one answer.
5. Import maps a `provider/model` field by **consulting the declared provider list**, not by
   splitting on a separator, and reports anything it cannot resolve rather than guessing.

Real colon-bearing names are frozen as golden tests so a regression here fails loudly.

### 2.2 Configuration interoperability — import

dorang reads a declarative model-list configuration in the widely used proxy format
(`dorang import config <file>`), normalizes it into dorang's own schema, and reports what
it could not represent. The mapping:

| Source concept | dorang |
|---|---|
| `model_list[].model_name` | `models[].name` — repeats form a load-balanced group |
| `model_list[].<params>.model` | `deployments[].upstream_model` |
| `.api_base` / `.api_key` / provider hint | normalized into `providers[]` + `credentials[]` |
| `.rpm` / `.tpm` / `.max_parallel_requests` | `limits[]` on the deployment |
| `.weight`, `.timeout`, `.stream_timeout` | deployment fields |
| drop-unsupported and explicit drop lists | `params.drop_unsupported`, `params.drop[]` |
| per-token input/output cost | `pricing.rules[]` (§8) |
| routing strategy | `models[].strategy` |
| default / context-window / content-policy fallbacks | `fallbacks.on.*` (§7.6) |

### 2.3 Administrative interoperability — shape-compatible

Common management paths keep their shape so existing scripts and UIs keep working:
`/key/*`, `/user/*`, `/team/*`, `/model/*`, `/model_group/info`, `/budget/*`,
`/spend/{logs,calculate}`, `/global/spend/report`, `/{user,team,tag}/daily/activity`,
`/health/history`. Everything else answers `501` with a machine-readable reason.

### 2.4 Credential import **[R1-A]**

Importing existing credentials is a **migration window**, not an architecture.

- Keys dorang issues use `dorang_v1`: `HMAC-SHA256(pepper, token)`, pepper from
  `server.key_pepper_env`. A stolen database is not offline-attackable.
- `legacy_sha256` — an unsalted single-round digest, matching the common incumbent scheme —
  is supported **only** when `auth.legacy.enabled: true`, which **requires**
  `auth.legacy.until: <date>`. After that date legacy verification is refused.
- The importer **must honor expiry and revocation**. Verification against a real deployment
  found that the large majority of stored credentials were already expired; importing them
  as live would silently restore revoked access. Expired rows are imported as expired.
- Authorization fields must be carried or the gateway fails **open**: expiry, blocked flag,
  model allow-list, route allow-list, budget and spend and reset time, rate limits, owning
  team and user, and object-permission references.
- The administrative/master credential is **out-of-band** — compared in constant time
  against a configured value, never stored as a row. A gateway that only reads an imported
  database loses admin auth entirely.
- Do **not** copy a display column that stores trailing characters of the secret. dorang
  stores a non-reversible display label instead.
- If provider credentials are also being imported from a sealed store, read them **before**
  rotating the sealing key. Where the sealing key defaults to the admin key, rotating it
  destroys every stored provider credential.

**Lookup with two schemes stays one lookup.** A scheme-independent index key
`lookup = sha256(token)[:16]` selects the row; only verification branches on scheme.

> That definition is load-bearing for import, not merely convenient: since it is a **prefix of
> the legacy digest**, an importer can derive both `lookup` and the legacy `token_hash` from
> the digest a source system already stores, and therefore **never needs the plaintext key**.
> If `lookup` were defined any other way, importing would require credentials nobody has, and
> migration would become a flag day. A test pins the identity so it cannot drift.
With `auth.rehash_on_use`, a successful legacy verification schedules an asynchronous
upgrade to `dorang_v1`, so the migration completes without downtime and without a flag day.

---

## 3. Domain model

```
Provider ──< Credential ──< CapacityLimit
    │                            ▲
    └──< Deployment ─────────────┘
             │
   ModelGroup ┘        same client-facing name ⇒ one load-balanced group
       ▲
     Alias               virtual name → ModelGroup
       ▲
   ModelClass            interchangeable groups ⇒ fail-back scope
```

| Concept | Definition |
|---|---|
| **Provider** | one upstream service: kind, base URL, adapter and defaults |
| **Credential** | one authenticating identity at a provider — **the unit quotas and concurrency actually attach to** |
| **Deployment** | (provider, credential pool, upstream model) — one routing candidate |
| **ModelGroup** | the set of deployments behind one client-facing name |
| **Alias** | virtual name → model group |
| **ModelClass** | interchangeable model groups; the scope fail-back may delegate within |
| **CapacityLimit** | (axis, metric, ceiling) |

---

## 4. Configuration

### 4.1 Principles

- **File is the baseline; the database overlays it.** A file alone must be enough to run.
- **Secrets never appear in configuration.** `key_env`, `key_file`, `key_ref`. An inline
  literal is accepted only when `server.env: development` and refuses to start otherwise.
- **Every section hot-reloads.** File watch, `SIGHUP`, and an admin endpoint. In-flight
  requests keep the snapshot they started with (§15.2).

### 4.2 Shape

```yaml
version: 1

server:
  listen: ":4100"
  env: production
  master_key_env: DORANG_MASTER_KEY
  key_pepper_env: DORANG_KEY_PEPPER
  request_timeout: 600s
  shutdown_grace: 30s

storage:
  driver: sqlite                 # sqlite | postgres
  sqlite: { path: ~/.dorang/dorang.db }
  postgres: { url_env: DORANG_DATABASE_URL, max_conns: 32 }

cluster:
  enabled: false
  node_id: ""
  redis_url_env: DORANG_REDIS_URL
  capacity_mode: local           # local | shared-redis | shared-pg | leased

auth:
  legacy:  { enabled: false, until: "" }
  rehash_on_use: true

# This example is kept internally consistent — every cross-reference below
# resolves — and a test loads it verbatim. An earlier draft did not, and the
# config validator caught nine dangling references in it.
providers:
  - name: plan-a                 # a per-model-limit coding plan
    kind: glm
    base_url: https://plan-a.example/v1
    timeout: 180s
    max_concurrency: 20          # route axis
    capacity_group: plan-a-pool  # provider-group axis membership
    params: { drop_unsupported: true, drop: [] }
    retry:  { max_attempts: 2, backoff: exponential, base: 500ms }
    usage_probe: { enabled: true, fetcher: glm, interval: 60s }
  - name: cloud-a                # a per-account-limit cloud, two accounts
    kind: ollama-cloud
    base_url: https://cloud-a.example/v1
    timeout: 180s
    capacity_group: cloud-a-pool

credentials:
  - { id: plan-a-1, provider: plan-a,  key_env: PLAN_A_KEY_1 }
  - { id: plan-a-2, provider: plan-a,  key_env: PLAN_A_KEY_2 }
  - { id: acct-1,   provider: cloud-a, key_env: CLOUD_A_KEY_1, capacity_group: acct-1 }
  - { id: acct-2,   provider: cloud-a, key_env: CLOUD_A_KEY_2, capacity_group: acct-2 }

capacity:
  provider_groups:
    cloud-a-pool: { max_concurrency: 6 }   # both accounts together
    plan-a-pool:  { max_concurrency: 20 }
  credential_groups:
    acct-1: { max_concurrency: 3 }         # per account, ALL models
    acct-2: { max_concurrency: 3 }
  models:
    - { provider: plan-a, model: model-x, max_concurrency: 7 }   # per (key, MODEL)
    - { provider: plan-a, model: model-y, max_concurrency: 7 }   # so both together = 14
  principals: { default: { max_concurrent: 32, max_queue_wait: 30s } }
  interactive_reserve: 0.3       # §11.1 — fraction of every axis batch may not take

key_rotation:
  strategy: least_used           # round_robin | least_used | failover | random
  providers:
    cloud-a:
      affinity_group: cloud-a-accounts
      stickiness: { scope: session, on_capacity: spill }
      keys:
        - { id: acct-1, key_env: CLOUD_A_KEY_1, max_concurrency: 3, capacity_group: acct-1 }
        - { id: acct-2, key_env: CLOUD_A_KEY_2, max_concurrency: 3, capacity_group: acct-2 }

models:
  - name: model-x
    class: chat-large
    strategy: [prefix_sticky, lowest_cost, least_busy]
    deployments:
      - { provider: plan-a,  upstream_model: model-x,       credentials: [plan-a-1, plan-a-2], weight: 10, priority: 0 }
      - { provider: cloud-a, upstream_model: model-x:cloud, credentials: [acct-1, acct-2],     weight: 5,  priority: 1 }
  - name: model-y
    class: chat-small
    deployments:
      - { provider: plan-a, upstream_model: model-y, credentials: [plan-a-1, plan-a-2] }
  - name: model-z
    class: chat-large
    deployments:
      - { provider: cloud-a, upstream_model: model-z:cloud, credentials: [acct-1, acct-2] }

aliases: { model-small: model-y, model-large: model-x }
classes: { chat-large: [model-x, model-z], chat-small: [model-y] }

routing:
  sticky: { enabled: true, ttl: 1h, purge_interval: 5m, key: [api_key, session_id] }
  prefix:
    enabled: true
    chunk_bytes: 4096            # BYTE boundaries — no tokenizer on the hot path [R1-4]
    checkpoints: logarithmic     # [R1-5]
    max_bytes: 64MiB             # budget by memory, not entry count [R1-6]
    ttl: 1h

fallbacks:
  on:
    rate_limit:      [same_group, same_class]
    quota_exhausted: [same_group, same_class]
    context_window:  [same_class_larger]
    content_policy:  [same_class]
    upstream_5xx:    [same_group, same_class]
    timeout:         [same_group]
    budget_exceeded: []          # never — failing is correct
    auth:            []
  max_hops: 3
  budget_ms: 120000

pricing: { catalog: /etc/dorang/pricing.yaml, currency: USD }

metering:
  numeric:  { enabled: true }                  # always on, fixed cardinality [R1-2]
  trace:    { store_messages: truncated, truncate_chars: 512,
              sample_rate: 1.0, daily_byte_budget: 8GiB }   # [R1-3]
  spool:    { dir: ~/.dorang/spool, max_bytes: 2GiB }       # [R1-1]
  flush_interval: 250ms

observability: { prometheus: true, otlp_endpoint: "", log_level: info, log_format: json }

extensions:
  lua:
    enabled: false
    dir: /etc/dorang/lua
    hooks: [on_request, on_route, on_response, on_email]
    limits: { instructions: 5000000, memory_mb: 32, timeout: 200ms }
```

### 4.3 Provider kinds

A `kind` selects, in one word: the wire adapter, capability defaults, the prompt-cache
scheme, and the reasoning-control shape. Three layers compose, later overriding earlier:
**kind defaults → model-name prefix rules → explicit configuration**.

```yaml
kind_aliases: { minimax-cn: minimax, dashscope: qwen }

kinds:
  openai:           { api: openai-chat,        cache: openai_cache_key,        reasoning: reasoning_effort }
  openai-responses: { api: openai-responses,   cache: openai_cache_key,        reasoning: reasoning_effort }
  anthropic:        { api: anthropic-messages, cache: anthropic_cache_control, reasoning: thinking_budget }
  glm:              { api: openai-chat,        cache: openai_cache_key,        reasoning: glm_thinking }
  qwen:             { api: openai-chat,        cache: openai_cache_key,        reasoning: enable_thinking }
  deepseek:         { api: openai-chat,        cache: openai_cache_key,        reasoning: reasoning_effort }
  moonshot:         { api: openai-chat,        cache: openai_cache_key }
  minimax:          { api: anthropic-messages, cache: anthropic_cache_control }
  mistral:          { api: openai-chat,        cache: openai_cache_key }
  xai:              { api: openai-chat,        cache: openai_cache_key,        reasoning: reasoning_effort }
                    # ^ probed 2026-07-28: this host serves BOTH shapes — see note below
  google:           { api: gemini,             cache: google_cached_contents }
  ollama:           { api: openai-chat,        cache: ollama_keep_alive }
  openrouter:       { api: openai-chat,        cache: openrouter_cache }
  cohere:           { api: cohere }
  jina:             { api: jina }
  vllm:             { api: openai-chat,        metrics: prometheus, priority: native }
  sglang:           { api: openai-chat,        metrics: prometheus }        # §4.4
  bedrock | vertex | azure: { … }
  echo:             { api: echo }              # deterministic, tests only
```

> **A "contradiction" that turned out to be a choice.** Two independent third-party catalogs
> declare the xAI host as a Responses-shaped API where this design declares it chat-shaped.
> Rather than adjudicate, it was probed directly, unauthenticated, for the cost of two
> requests: `/v1/chat/completions` rejects an empty body with *"Messages cannot be empty"* and
> `/v1/responses` rejects it with *"missing field `input`"*. **Both routes are served.**
> Neither source was wrong, and the disagreement was really an unstated assumption that a host
> speaks one shape.
>
> The lesson generalizes: an `api` shape is a property of the **deployment**, not the provider,
> and must be settable per deployment. It also cost nothing to establish — an unauthenticated
> probe distinguishes "route absent" from "route present, request malformed" without spending
> a token, and should be the first move whenever catalogs disagree about a wire shape.
>
> Noted in passing: the two routes on that single host return *different error envelopes* —
> one bare, one with a stringified `code`. Error normalization (§10.1) cannot assume
> consistency even within one provider.

Model-name prefix rules supply context window and max output when a name matches a known
family, so a new model of a known family works without configuration.

> **Prefix rules may NOT supply reasoning capability.** An earlier draft of this section said
> they could, which directly contradicts §10.2 and review finding C5: generalizing a
> reasoning capability observed on one model version to a whole family is the exact defect
> that made revision 1 silently drop the control on the family's other members. Reasoning
> capability comes only from an explicit, dated model entry. A configuration that tries to
> attach reasoning to a prefix rule is **rejected at load**, not ignored.

**Undeclared is not zero-shaped.** A numeric capability that has not been observed is
recorded as undeclared rather than filled with a plausible guess, because a wrong context
window feeds context-window fallback routing (§7.6) and would silently misroute. Operators
supply real values per deployment; `UnverifiedModels()` lists what still needs a probe.

#### The error direction is not symmetric

A context window is the one capability where being wrong in each direction costs
differently, so the tie-break rule is stated rather than left to taste:

- **Too large** — requests between the real limit and the declared one fail outright, and
  **context-window fallback never fires**, because dorang believes they fit. A hard failure
  the routing layer is blind to.
- **Too small** — those requests route to a larger-context model unnecessarily. A cost, not
  a failure.

**When credible sources disagree, take the smaller.** The larger value can only be adopted
from a probe, never from a citation.

#### The same model name is not the same limit

Mining several catalogs turned up one name carrying four different context windows across
four hosts, and another differing 4× between its native API and a reseller. Limits are a
property of the **deployment**, not of the weights. This is direct evidence for keying
identity on `(kind, model)` rather than on the model string — and for treating a number
sourced from a *different* provider's catalog as no evidence at all.

A number that is merely a source's own fallback constant is also no evidence: it records
that nobody looked. Values equal to a catalog's documented default are rejected for that
reason, not accepted for convenience.

### 4.4 Self-hosted backends are a first-class class, not a special case

vLLM and SGLang are **day-zero backends**, and they are treated as one class rather than two
adapters. The reason is not tidiness: a self-hosted engine is the only kind of backend where
the operator controls the flags, so it is the only kind where dorang can *state what
configuration makes standard protocol behavior apply* — and then hold both engines to it.

Three commitments follow.

**One surface, both engines.** A caller speaking OpenAI or Anthropic must not be able to tell
which engine is behind a model. Every request field, response field, usage counter, stop
reason, and error shape is normalized to the canonical names of §10.7. Where an engine cannot
express something, that is a structural downgrade (§10.1) and surfaces as such — never as a
silent difference in behavior between two deployments of the same model.

**Metadata parity is part of that surface.** Token accounting, cache counters, context window,
and reasoning fields are reported identically regardless of engine. This is the part most
likely to be got wrong quietly, because a mis-mapped cache counter produces no error — only a
wrong invoice (§10.7).

**A published operator profile, not a list of caveats.** Each engine's required flags are
documented as a runbook: set these, and the market-standard protocols work as written. Every
flag entry names **what silently breaks without it**, because that is the failure mode that
matters here — the engines accept requests and return `200` while ignoring what was asked.
The profile also lists flags that must **not** be set: one SGLang option silently disables
authentication entirely, which no amount of gateway-side care can compensate for.
[VLLM.md](VLLM.md) §5 is the vLLM profile; [SGLANG.md](SGLANG.md) carries SGLang's, plus the
row-by-row normalization table that makes the single-surface claim checkable rather than
aspirational.

**What dorang does not do here.** It does not tune the engine, does not compact (§10.5a), and
does not paper over a missing flag by emulating the behavior in the gateway. If an operator
has not enabled prompt-token details, dorang reports that cached-token pricing is unavailable
rather than guessing at it. Emulation would make the gateway's numbers disagree with the
engine's, and disagreeing numbers are worse than absent ones.

---

## 5. Capacity — multi-axis reservation

The hardest requirement. **Within one provider, the unit a concurrency limit counts over
differs.** One account may allow 3 in flight across all models; a coding plan may allow 7
in flight *per model*, so serving two models means 14 concurrent requests on one key.

### 5.1 Axes

A reservation may need several of these at once.

| Axis | Key | Configured at | Example |
|---|---|---|---|
| `route` | `route:<provider>` | `providers[].max_concurrency` | a deployment's own ceiling |
| `provider_group` | `pgroup:<g>` | `capacity.provider_groups` | several providers sharing a pool |
| `model` | `model:<provider>\0<model>` | `capacity.models[]` | **per (key, model)** |
| `credential_group` | `cgroup:<g>` | `capacity.credential_groups` | **per account, any model** |
| `key` | `key:<provider>\0<cred>` | `key_rotation…keys[].max_concurrency` | a key's own ceiling |
| `principal` | `prin:<key\|user\|team>` | `capacity.principals` | per caller |
| `global` | `global:` | `capacity.global` | process/cluster ceiling |

### 5.2 Metrics

| Metric | Kind | Mechanism |
|---|---|---|
| `max_concurrent` | gauge | counted reservation, released on completion |
| `rpm` | rate | token bucket |
| `tpm` | rate | token bucket with **logical-request settlement** (§5.5) |
| `max_queue` | gauge | queue-depth ceiling; over it, `429` |

### 5.3 Acquisition — atomic, all-or-nothing

```
Acquire(provider, model, provider_group, candidates[], preferred, on_capacity) →
  loop:
    under one lock:
      check route → provider_group → model → principal → global
        any unavailable ⇒ fail whole attempt, holding NOTHING
      for each credential candidate, preferred first:
        check (credential_group, key) for THAT candidate's provider
        first candidate that passes wins
        on_capacity=wait  ⇒ only the preferred candidate is tried
        on_capacity=spill ⇒ continue to the next candidate
      commit: increment every selected key at once
    success ⇒ Reservation
    failure ⇒ enqueue on the blocking axis and wait (§5.4)
```

Check and commit are one critical section, so **partial occupancy never exists** and a
waiter never holds one slot while queuing for another. Deadlock is structurally impossible.

**Candidate atomicity across providers [R1-11].** A model group may span providers, so
`spill` can move between candidates whose credential axes belong to *different* providers.
The rule: a credential candidate is the triple `(provider, credential_group, key)` and is
checked and committed **as a unit**. Spilling replaces the whole triple. Provider-scoped
axes (`route`, `provider_group`, `model`) are re-derived for the candidate's provider, not
carried over from the previous one.

**Reservation expiry.** Beyond `defer Release()`, every reservation carries
`deadline = now + request_timeout + 30s`. A sweeper reclaims reservations that outlive it,
so a panic or a leaked goroutine cannot permanently consume capacity.

**Client disconnect propagates upstream [R1-10].** When a streaming client goes away,
releasing the local slot is not enough — the upstream still counts the request against its
own limit, and dorang would immediately send another request into a full upstream and take
a `429`. Cancellation is therefore propagated to **abort the upstream connection** before
the slot is released. Local in-flight and upstream in-flight must not diverge.

### 5.4 Waiting — per-axis FIFO, targeted wakeup **[R1-7]**

Revision 1 woke every waiter on release. With many requests queued behind a small limit,
one succeeds and the rest re-acquire the lock, re-check, and sleep again — failed lock
acquisitions dominate useful work, and arrival order is lost. Worse, a waiter needing
several axes systematically loses to one needing a single axis, so the common case starves
and eventually returns `429`, which is a deadlock as far as the caller is concerned.

Revision 2:

- Each axis owns a **FIFO wait queue**.
- A waiter enqueues on the axis that blocked it, recording the full axis set it needs.
- On release, that axis wakes **only as many head-of-queue waiters as slots freed**, and
  hands them a direct grant rather than a signal to re-race.
- A waiter whose other axes have since filled goes to the head of the queue it now blocks
  on, carrying its **original enqueue timestamp**.
- **Aging**: effective priority rises with total wait time, so a multi-axis waiter cannot be
  indefinitely overtaken by single-axis waiters.

Completion gate: a saturation benchmark at 0 / 100 / 1000 waiters against limits of
1 / 7 / 32, asserting bounded wakeups per release and FIFO fairness within an axis.
**Measured: exactly 1.00 wakeups per grant at every scale** — a broadcast would be O(waiters)
per release.

#### Six corrections found while implementing this

The implementation surfaced problems the design text did not answer. Recorded because each
one is a place a plausible reading produces broken behavior.

1. **A waiter must enqueue on the blocking axis of *every* candidate it tried, not one.**
   The text said "the axis that blocked it", singular. With `spill`, if the preferred
   candidate blocks on one key and a later candidate's key frees, a single-queue waiter is
   never woken. Queue on all of them (capped), and remove from all on grant or cancel.

2. **"Wake as many waiters as slots freed" under-specifies a failed probe.** A waiter that
   fails to acquire consumes no slot, so a literal reading leaves freed slots idle behind it.
   Probe up to `freed + slack` with a hard cap — still O(1) per release.

3. **Strict FIFO and the interactive reserve conflict.** Batch and interactive waiters share
   a queue but have different effective ceilings, so a blocked batch waiter at the head
   head-of-line blocks exactly the interactive traffic §11.1 exists to protect. A re-blocked
   head waiter is parked for the remainder of the pass, keeping its sequence number, so the
   next-oldest gets a turn.

4. **The reserve must not round a slot away.** `floor(10 × (1 − 0.3))` evaluates to **6**, not
   7, in binary floating point, so the reserve silently exceeds what was configured. An
   epsilon is required. And `floor(1 × 0.7) = 0` makes an axis unsatisfiable rather than
   merely contended — that returns an error immediately instead of blocking forever.

5. ⚠️ **Aging prevents overtaking, not starvation. This is an open hole.** §5.3 forbids
   holding one slot while queuing for another, which is what makes deadlock structurally
   impossible. The cost is that a waiter needing axes A and B can ping-pong indefinitely
   while both stay saturated: it is always at the head of both queues, yet never finds both
   free at the same instant. Aging does not fix this, because the waiter is never overtaken —
   it simply never wins. Closing it needs a **soft reservation**: when the oldest waiter on A
   is blocked on B, bar new grants on A until it is served, accepting a small throughput loss
   for a liveness guarantee. That protocol is not yet designed; see §18 W8.

6. **Check order.** §5.3 and §5.7 stated different orders. §5.7's is authoritative because it
   has a stated rationale. Under one lock the outcome is identical; only the reported
   blocking axis differs, which matters for diagnostics rather than correctness.

### 5.5 TPM belongs to the logical request **[R1-8]**

Revision 1 contradicted itself: one section settled TPM against measured usage, another
said rates need no rollback. Both cannot hold, and under fallback the second is actively
harmful — three hops would each pre-charge, so one logical request could consume triple.

Revision 2:

- Input tokens are counted exactly before dispatch. Output is pre-charged at `max_tokens`,
  or a stated fraction of the model's max output when absent.
- **Every pre-charge is tagged with the logical request id.** On completion, only the
  successful hop settles against measured usage; **every failed hop is refunded in full.**
- RPM is *not* refunded — a request that reached the upstream consumed a request slot even
  if it failed. This asymmetry is deliberate and stated, not an oversight.

### 5.6 Multiple nodes **[R1-1, R1-3]**

| `capacity_mode` | Behavior | Accuracy |
|---|---|---|
| `local` | per-node counters | **exact on one node; N× overshoot with N nodes** |
| `shared-redis` | atomic Lua acquire/release with lease TTL | exact; +1 RTT on hot path |
| `shared-pg` | lease table with advisory locks | exact; +1 RTT, higher |
| `leased` | node leases a block, decrements locally, leader rebalances | **bounded overshoot ≤ block size × (nodes − 1)** |

**Hard guard.** `cluster.enabled: true` with `capacity_mode: local` **refuses to start.**
Revision 1 called this a recommendation. It is not: silently exceeding a provider's plan
limit produces upstream `429`s, which cascade into the fallback chain (§7.6) and consume
the capacity of unrelated models in the same class. The failure surfaces far from its cause.

Small limits (single digits) cannot be usefully divided across nodes, so `leased` rejects
any limit below `cluster.min_leasable` (default 16) and requires a shared mode for those.

Every mode publishes its **maximum possible overshoot as a number**. "Approximately
accurate" is not an acceptable specification.

### 5.7 Implementation and its evolution **[R1-2]**

Version 1 is a single mutex over the whole broker: simple, obviously correct, short critical
section. It ships that way and is measured.

If measurement demands sharding, the shard key is **the provider**, and it is a
**hard invariant that every axis key of one reservation resolves to one shard**. This is
enforced by construction: `route`, `provider_group`, `model`, `credential_group`, and `key`
are all provider-scoped, and `spill` replaces the whole candidate triple (§5.3), so a
reservation never spans providers.

The two non-provider axes are handled outside the shard, **before** it, as plain atomic
counters with compensating release: `global` and `principal` are single counters, so
acquiring them is a CAS that either succeeds or fails immediately, with nothing held while
waiting. Order is fixed — global, principal, then the provider shard — so no cycle exists.

Revision 1 proposed sharding without saying how a reservation spanning shards stays atomic,
which would have reintroduced either deadlock or partial occupancy. The invariant above is
what makes the evolution safe, and it is asserted by a test, not by a comment.

---

## 6. Quota and budget

Orthogonal to §5: concurrency is "how many right now", quota is "how much within a window".

### 6.1 Locally metered quota

```yaml
quotas:
  - { window: 5h,     metric: cost_usd,     limit: 3.0, on_exhaust: cooldown }
  - { window: weekly, metric: tokens_total, limit: 200000000 }
```

Windows are minute-bucket rings, O(1) to maintain. `on_exhaust` is `cooldown` (step aside
until reset — this is what moves traffic to the next key, R4), `disable`, or `passthrough`.

### 6.2 Provider-reported quota, combined not replaced **[R1-4]**

Where a provider exposes remaining quota, that figure covers **all** use of the key,
including traffic that never passed through dorang — so local metering alone under-counts.
But a poll is up to one interval stale, and during a burst the local counter is the
*fresher* signal. Revision 1 declared the provider "the truth" and discarded local delta,
which meant dorang routed most aggressively exactly when its information was worst.

Revision 2 combines them:

```
effective_used = max( provider_reported_used ,
                      provider_reported_used_at_last_poll + local_delta_since_that_poll )
```

The provider figure re-baselines on every poll; local metering supplies the delta in
between. Neither source can hide a burst.

Fetches run off the request path, in parallel, with per-provider timeouts. A failed fetch
**never** disables a credential — a failed read is not an exhausted quota. The last good
snapshot is retained and the staleness is exposed.

### 6.3 Quota across nodes **[R1-2]**

Revision 1 promised atomic shared quota, asynchronous local counting, and no synchronous
store access on the hot path. All three cannot hold: either nodes overshoot while counts
converge, or every request contends on one row.

Revision 2 applies §5.6's model to quota as well — `local`, `shared-redis`, `shared-pg`, or
`leased` — with the same hard guard and the same requirement to publish maximum overshoot
as a number. Quota and concurrency now use one accuracy vocabulary rather than two.

### 6.4 Budget

```yaml
budget:
  period: monthly
  limit_usd: 10.0
  on_exceed: stop
```

Budgets **reserve before spending** so concurrent requests cannot overshoot. The estimate
is deliberately an upper bound: exact input tokens priced, plus output priced at
`max_tokens`.

Two corrections from review:

- **Reservations expire [R1-19].** `budget_state` carries `reserved_until`. A process killed
  between reserve and settle would otherwise lock that amount forever, and over time
  reserved-but-never-settled amounts would exhaust a budget nothing actually spent. The
  leader sweeps expired reservations, exactly as §5.3 does for capacity. The two mechanisms
  are the same pattern and now have the same safety net.
- **Unexecuted requests are refunded in full [R1-20].** A request may reserve budget at the
  gate and then be rejected while waiting for capacity, never reaching an upstream. Revision
  1 defined settlement only for completion. Revision 2 makes budget a **soft hold** at the
  gate that becomes a **hard hold only after capacity is acquired**; anything that fails
  before dispatch releases the full amount.

Budgets attach to a credential, key, user, team, or globally. Exceeding one is **not**
a fallback condition — failing is the correct outcome, and it surfaces as a terminal `400`
rather than a `429` **[R1-21]**. A `429` is a rate-limit signal: emitting one here would send
the request down the fallback chain and spend a different subject's budget on a model the
caller never asked for.

---

## 7. Routing

### 7.1 Pipeline

```
resolve alias → resolve group → filter (quota, budget, policy, capability)
  → order by strategy → try acquire (health, then capacity — §5.3; next candidate on failure)
  → execute → on error classify → fallback (§7.6) → meter (§12)
```

> **Health is not in the filter stage, because health is not a query.** An earlier draft put it
> there. Admitting a half-open deployment *consumes* its single probe slot, so evaluating
> health for every candidate during filtering burns probes on candidates that are never
> dispatched — delaying recovery precisely when the system is under the load that makes
> recovery matter. It is therefore evaluated once per candidate inside the try-acquire walk,
> where a positive answer is immediately followed by a dispatch.

**A no-opinion signal must never read as a winning one.** Three inputs can be absent, and each
has a direction in which absence silently wins:

| Signal | Absent means | The trap |
|---|---|---|
| latency sample | unproven | zero is the *fastest* value — an unproven backend wins on ignorance |
| throughput sample | unproven | zero is the *slowest* — it loses forever and never earns the sample that would let it compete |
| price | unpriced | zero is the *cheapest* — the deployment nobody priced wins every group, permanently and silently |

All three resolve the same way: **absent is no opinion, not a value.** The candidate is skipped
by that comparator and ranked by the next one in the chain, and a counter records it so an
operator can see what is unmeasured rather than inferring it from suspicious routing.

Two of these were only half-stated before. §8.3's "an unpriced model costs zero" is right for
*accounting* and wrong for *routing*: routing reads marginal cost only when it is actually
known. And the throughput direction is the more dangerous of the latency pair, because losing
forever reads as conservative and survives review.

### 7.2 Alias **[R1-C1]**

Upstream always receives the **real** model id. The response body's `model` field carries
back **the name the client asked for**, so client-side comparisons keep working. The real
model is exposed in headers (§10.4).

Revision 1 claimed streaming chunks could be patched "by byte offset, without decoding".
That is wrong, and wrong in the *common* case rather than an edge case:

- **Lengths differ by default.** `model-small` → `qwen3.5:397b` is not the same length.
  An in-place byte swap is only possible when the replacement is the same size, which is
  the rare case, not the normal one.
- **Frame boundaries are arbitrary.** A raw copy reads at buffer boundaries, so the
  `"model":"…"` field can straddle two reads. A scanner that assumes it sits in the first
  buffer either misses it or corrupts JSON.
- **The field is not reliably in the first frame.** Some backends omit it early or only
  emit it with the terminal usage frame.
- **Compression.** If a content encoding is negotiated, patching compressed bytes is not
  possible at all.

Revision 2 states the honest mechanism:

- dorang requests **`Accept-Encoding: identity`** from upstream on streaming responses. It
  must read the terminal usage frame anyway, so it never wanted compressed bytes.
  Compression toward the *client* is unaffected and negotiated separately.
- The relay is a **single-pass line-oriented scanner**, not a JSON decoder: it splits on SSE
  frame boundaries, and rewrites only the `model` value when a frame contains one, carrying
  a partial frame across reads. Everything else is forwarded untouched.
- Cost is a byte scan at memory bandwidth with no per-frame allocation and no full decode.
  §15.2 no longer claims "zero-copy"; it claims **single-pass, no-decode**, which is what
  is actually achievable while honoring R7.
- When the requested name and the upstream name are identical, the scanner short-circuits
  to a plain copy.

### 7.3 Strategies

`round_robin` (weighted) · `least_busy` · `lowest_cost` · `lowest_latency` ·
`highest_tps` · `sticky` · `prefix_sticky` · `priority` · `weighted_random`.

A list composes them as a tie-break chain: `[prefix_sticky, lowest_cost, least_busy]`.

### 7.4 Cache affinity

#### (a) Session stickiness

Key is `(tenant, group, session)` with tenant as the leading component so two tenants never
share a pin. Entries expire on **creation** time, not last use, because the premise is that
the upstream cache is gone after the TTL — refreshing on use would defeat the point. An
unhealthy or exhausted target discards the pin immediately.

#### (a2) Credential affinity — a correctness constraint, not a cache optimization

Several accounts on the same provider is the normal case, not an edge case: two or more
subscription plans on one vendor, several accounts on a cloud, a pool of keys behind one
model. §5 already treats each credential as the unit that quotas and concurrency attach to.
What §7.4a did not say is that **for stateful APIs the credential is also the unit that
conversation state attaches to** — and that changes stickiness from an optimization into a
constraint.

Four things are scoped to the account, not to the model or the provider:

| Account-scoped state | What happens if a later turn lands elsewhere |
|---|---|
| Server-side response handles (`previous_response_id` and equivalents) | The handle does not resolve. Hard error, or worse, silently answering from a truncated conversation |
| Integrity-protected reasoning blocks (§10.2) | The receiving account cannot validate a block it did not issue |
| Prompt cache residency | Silent cost and latency regression — the cache is cold, and nothing reports it |
| Quota and spend windows (§6) | Not a failure, but the reason the pool exists |

Only the third is recoverable. The first two are **correctness failures**, and the first can
fail in the direction that produces a plausible answer to a different question.

So the design distinguishes two strengths of affinity, and picking the wrong one is the bug:

- **Preferred** — the default. Cache-driven. If the preferred credential is at capacity,
  `on_capacity: spill` moves to another account, accepting a cold cache. Correct whenever the
  conversation is stateless, which is most chat traffic.
- **Pinned** — required. The conversation carries account-scoped state, so another account is
  not a worse choice, it is a **wrong** one. `spill` must not apply. The request waits for
  the pinned credential, or fails with a reason naming the pin. It never silently lands
  elsewhere.

**dorang infers the pin rather than trusting configuration for it.** A request that carries a
server-side handle or an opaque reasoning block is pinned by that fact, whatever the
stickiness setting says — because the setting expresses a preference about cost, and this is
not a question about cost. Configuration can *widen* nothing here; it can only choose the
policy for the unpinned case.

```yaml
key_rotation:
  providers:
    plan-vendor:
      affinity_group: plan-vendor-accounts
      stickiness:
        scope: session            # none | run | session | conversation
        on_capacity: spill        # applies ONLY to unpinned requests
        pin_on_state: true        # default; a stateful request pins regardless of the above
      keys:
        - { id: plan-1, key_env: PLAN_KEY_1, max_concurrency: 7, capacity_group: plan-1 }
        - { id: plan-2, key_env: PLAN_KEY_2, max_concurrency: 7, capacity_group: plan-2 }
```

Two consequences worth stating because they are easy to miss:

1. **A pinned request that cannot be served is a terminal failure, not a fallback.** Falling
   back re-runs the same impossibility on every hop and returns the same error more slowly —
   the identical reasoning as the protocol-family pin in §7.6, one level finer. The pin is the
   credential, not merely the family.

2. **Pinning interacts with quota.** If the pinned account's quota is exhausted (§6), dorang
   cannot move the conversation to a healthy account without breaking it. The honest outcome
   is to fail and say *which* account is exhausted and when it resets, so the caller can
   decide whether to wait or start a fresh conversation. Silently continuing on another
   account would trade a visible limit for an invisible corruption.

Scenario tests: two accounts on one provider, a stateful conversation, the preferred account
saturated — assert the request **waits or fails** and never spills; the same conversation
without state — assert it spills cleanly; and a pinned account with exhausted quota — assert a
terminal error naming the account and its reset time.

#### (b) Prefix matching, order-exact **[R1-4, R1-5, R1-6]**

Routing to whichever backend still holds the conversation prefix requires knowing the prefix
matched **in order**. Hashing a set of messages, or each message independently, collides on
reordering. A hash chain makes that structurally impossible:

```
c₁, c₂, … = the request's message bytes cut at fixed BYTE boundaries
h₀ = H(group_id)
hᵢ = H(hᵢ₋₁ ‖ len(cᵢ) ‖ cᵢ)
```

A match at depth *i* proves `c₁..cᵢ` are byte-identical **and in that order** — reordering,
insertion, and deletion all diverge. Including `len` removes boundary ambiguity.

Three corrections from review:

- **Byte boundaries, not token boundaries.** Revision 1 cut at token boundaries, which
  requires tokenizing the entire conversation before routing. On a long conversation that
  alone exceeds the whole latency budget, and it contradicts the rule against buffering full
  bodies. The chain property never depended on tokens. Chunks are cut on **raw request
  bytes, hashed incrementally as the body streams in**, so there is no tokenizer and no
  second pass on the hot path.
- **Logarithmic checkpoints.** A fixed shallow depth cannot distinguish two conversations
  that share a system prompt and diverge afterwards — they collide at the deepest tracked
  node and "longest common prefix wins" becomes false. Checkpoints are fine-grained early
  and then double: 4 KiB, 8, 16, 32, 64, 128, 256, 512 KiB, 1 MiB…, so long prefixes are
  distinguished with a logarithmic number of entries.
- **Budget by bytes.** Revision 1 budgeted by entry count using an optimistic per-entry
  size. Real retained size is materially larger, and one request writes an entry at every
  checkpoint. The table is bounded by `max_bytes`, deployment ids are interned as integers,
  and the retained-size budget is a measured milestone gate.

A miss lets the next strategy choose, and that choice is recorded at every checkpoint.

### 7.5 Priority

**Inside dorang**: admission queue order, by principal priority class, FIFO within a class.

**To the backend**: emitted so the upstream scheduler can act on it.

```yaml
priority_mapping:
  classes: { realtime: 0, interactive: 2, batch: 10 }   # canonical: LOWER is more urgent
  emit:
    vllm:   { field: priority, direction: ascending }    # lower first — native
    sglang: { field: priority, direction: descending }   # HIGHER first — inverted, see below
    openai: { field: service_tier, map: { realtime: priority, interactive: default, batch: flex } }
    header: X-Request-Priority
```

Unknown backends receive only the header, which is harmless if ignored.

> **Negating for a descending engine puts dorang's entire scale on the non-positive half-line**,
> which is internally consistent and dangerous on a *shared* server: any co-tenant sending a
> naive positive priority then outranks all dorang traffic, realtime included. `EmitRule.Base`
> offsets the emitted range for exactly this case. It defaults to plain negation, because on a
> dedicated engine that is correct and an arbitrary offset is not — but an operator sharing an
> engine must set it, and the profile says so.

> ⚠️ **The two self-hosted engines order priority in opposite directions, using the same field
> name and type, and both return `200` either way.** vLLM schedules the lowest value first;
> SGLang schedules the **highest** first by default. A single shared constant is therefore
> wrong on one of them — with the canonical map above, sending it unchanged to SGLang makes
> **batch outrank realtime**, which is not a degradation but an inversion, and nothing in the
> response reveals it.
>
> So `direction` is a required property of the emit rule, not a detail. The canonical scale
> stays lower-is-more-urgent (matching the majority convention and §7.5's existing classes),
> and the adapter negates for descending engines. A scenario test asserts that the same
> canonical class produces opposite wire values on the two engines — because a test asserting
> only "a number was sent" would pass while the behaviour is backwards.
>
> This is the clearest justification for §4.4's single-surface commitment. Two engines that
> both "support priority" and both accept the same JSON disagree about what it means, and only
> a normalizing layer that knows each engine's direction can present one coherent contract.
> Details and citations: [VLLM.md](VLLM.md) §1.2, [SGLANG.md](SGLANG.md) §3.

### 7.5a Measured-signal routing: TTFT, health, and expiring quota

Three ranking inputs come from measurement rather than configuration. All three are
**comparators in the chain (§7.3)**, never overrides — they reorder healthy candidates and can
never promote one that a pin, capability filter, quota, or budget has excluded.

#### (a) Latency and throughput

`internal/health` maintains a smoothed time-to-first-token and a smoothed generation rate per
deployment, updated from every completed request. `lowest_latency` ranks on the first,
`highest_tps` on the second, and they are deliberately separate signals: a backend can answer
promptly and generate slowly, or the reverse, and choosing on the wrong one is how a "fastest"
router loses to round-robin on long outputs. Generation rate excludes time-to-first-token, so a
deep queue is not mistaken for a slow generator.

A deployment with no samples has **no opinion** and is ranked by the next comparator (§7.1) —
absent must never read as fastest or as slowest.

#### (b) Circuit state feeds ordering, not just admission

Health's binary admit/refuse is applied at try-acquire (§7.1). Its *continuous* part — recent
failure rate and consecutive-failure count — is available to ordering, so a deployment that is
technically still closed but visibly degrading sinks in the ranking before it trips. This is
the cheap half of resilience: it costs nothing to prefer the healthier of two working
backends, and it means the circuit breaker fires less often because traffic drained first.

#### (c) Expiring quota — use it or lose it

This one is not a performance signal at all, and it is the reason this section exists.

A subscription window that **resets** is use-it-or-lose-it: an allowance at 20% consumed with
one hour left on a weekly window is about to discard 80% of what was already paid for. Routing
that ignores this systematically wastes the cheapest capacity available, and does so invisibly
— nothing fails, the bill is simply higher than it needed to be.

```
urgency = unused_fraction ÷ remaining_fraction_of_window
```

where both are in [0,1]. An allowance 20% used with 10% of its window left scores 0.8 ÷ 0.1 =
8.0; the same allowance at the start of the window scores ≈0.8. `quota_urgency` ranks
candidates by that value, descending.

Three constraints, because this is the kind of optimization that quietly does harm:

1. **It applies only to quota that actually expires.** A prepaid balance that rolls over, or
   pay-as-you-go where unused simply means unspent, has **zero** urgency — spending it early
   buys nothing and forfeits optionality. The distinction is a property of the quota
   (`resets: true`), not an inference from its shape.
2. **It never overrides cost when the alternatives are not equivalent.** Preferring an
   expiring allowance is right when the alternative is *also* already paid for; it is wrong
   when the alternative is free. So `quota_urgency` composes as a tie-break *after*
   `lowest_cost` by default, and an operator who wants it earlier must say so.
3. **It must not create a stampede at the window edge.** Every node computing the same urgency
   at the same moment converges on the same credential, and its concurrency limit then becomes
   the bottleneck for the whole fleet. Urgency is therefore damped by current occupancy — the
   same `least_busy` term already available — and the ranking is jittered per node.

The honest framing: this trades a small amount of routing predictability for a real reduction
in waste, and it only pays on subscription plans with resetting windows. On a pure
pay-as-you-go deployment it is inert, which is the correct behaviour rather than a limitation.

### 7.6 Fail-back

| Cause | Detection | Default chain |
|---|---|---|
| `rate_limit` | 429 | same group → same class |
| `quota_exhausted` | local or reported quota | same group → same class |
| `context_window` | pre-computed, or a 400 signature | same class, larger context |
| `content_policy` | 400/403 signature | same class |
| `upstream_5xx` | 5xx | same group → same class |
| `timeout` | deadline | same group |
| `budget_exceeded` | budget | **none** |
| `auth` | 401/403 | **none**; credential marked exhausted |

`same_class` is what satisfies R6 — delegation to a *different model* of equivalent class.

> ⚠️ **Opaque state pins a conversation to a protocol family — preference is not enough.**
> Once a conversation carries family-scoped opaque state (an integrity-protected reasoning
> block, a server-side response handle, a vendor compaction cursor), a fallback that crosses
> families cannot succeed: the receiving family classifies the foreign state as
> **never-retryable**, so every remaining hop burns and the caller gets the same `400` three
> times slower. Capability routing (§10.1) must therefore treat this as a **hard pin**, not a
> ranking input. A request whose family has no healthy deployment fails immediately and says
> why — spending the hop budget to arrive at the identical error is worse than failing fast.
> Evidence and the full opaque-state inventory: [EXTENSIONS.md](EXTENSIONS.md) §B.

**Streaming boundary**: fallback is permitted only before the first byte reaches the client.
After that, an error event ends the stream. Duplicated output is worse than a visible
failure. This boundary is enforced by a test.

Bounded by `max_hops` and a wall-clock budget. **`max_hops` counts hops after the first dispatch**, so `max_hops: 3` permits four attempts in total — stated because either reading is defensible and the difference is a whole extra request. Per-deployment circuit breakers remove
repeatedly failing targets from candidacy, with a single probe on half-open.

---

## 8. Cost

Prices vary by model, by provider, by time of day, and by input size — and a subscription
may coexist with per-token pricing on the same request.

### 8.1 Rule classes **[R1-9]**

Revision 1 used a single-winner rule model. That is broken: a credential-scoped subscription
rule outranks the model-scoped token rule and zeroes token cost; summing them instead
contradicts "most specific wins". They are different *kinds* of cost, not competing
descriptions of one.

Revision 2 gives each class its own winner, and composes across classes:

| Class | Meaning | Composition |
|---|---|---|
| `marginal_usage` | per-token, per-request, per-character, per-second | most specific rule wins |
| `fixed_subscription` | plan cost independent of this request | most specific wins; amortized |
| `adjustment` | discounts, margins, taxes | all applicable rules apply in order |
| `notional_rate` | **what this traffic would have cost at list price** — never billed | most specific wins; §8.5 |

```
cost = marginal(winner) + amortized(subscription winner) then adjustments applied in order
```

**Amortization has one formula**: `period_cost × (request_marginal ÷ period_marginal_to_date)`,
falling back to elapsed-fraction when marginal is zero. It is computed for **accounting**
only. Routing uses `marginal_usage` alone, because a sunk subscription cost must not make a
saturated plan look cheap. The two are separate fields, never conflated.

### 8.5 Notional cost — what a subscription is actually worth

A subscription plan bills a flat amount, so its marginal cost per request is zero. That is
correct for billing and useless for everything else: it cannot answer whether the plan is
worth renewing, which team consumes the value, or what the bill would look like after
outgrowing it. Those are the questions an operator on a subscription actually has.

So dorang computes a fourth figure alongside the three billing ones: **what this exact traffic
would have cost at the provider's pay-as-you-go list rate**.

```yaml
rules:
  - id: plan-a-subscription
    class: fixed_subscription
    match: { credential: plan-a-1 }
    unit: subscription
    amount_per_period: "20.00"
    period: monthly

  - id: plan-a-list-rate
    class: notional_rate                     # never billed, never budgeted
    match: { provider: plan-a, model: model-x }
    unit: per_1m_tokens
    input:  "0.85"
    output: "3.40"
    cache_read: "0.19"
    source: "vendor public price page"       # required
    as_of: 2026-07-28                        # required
```

**This is an estimate, and the design treats it as one throughout.** Three rules follow from
that, and each exists because the alternative is a number that looks authoritative and is not:

1. **`source` and `as_of` are required.** A notional rate with no provenance is a guess
   wearing a currency symbol. Rules missing either are a validation error, not a warning —
   the same discipline §4.3 applies to model capabilities, for the same reason.
2. **It never touches billing, budget, quota, or routing.** `NotionalNano` is a separate
   field and a separate rollup column. It is absent from `TotalNano` by construction, so it
   cannot leak into a spend check by omission. Routing continues to use marginal cost only
   (§8.1) — a notional figure is what traffic *would* cost elsewhere, which says nothing about
   the cost of the choice in front of the router.
3. **Adjustments do not apply to it.** A discount or margin is a billing construct, and the
   notional figure is by definition the vendor's list price — discounting the estimate of what
   someone else would charge defeats the point of computing it. There is deliberately no way
   to target it.

4. **Notional lines stay out of the cost component breakdown.** A caller who sums components
   instead of reading the total must still get the billed figure. Keeping the exclusion true
   for both access paths is what makes "structurally impossible" mean it.

5. **Missing is reported, never zero.** A model with no notional rule reports the figure as
   unavailable and increments a counter, exactly as §8.3 does for an unpriced model. Silently
   returning zero would make a subscription look infinitely efficient — the most flattering
   possible answer, and the one most likely to go unquestioned.

What it buys, stated plainly so the feature is judged on it: `notional ÷ amortized
subscription` is the plan's realized leverage — **with one caveat that matters when reading it
per request**. A flat plan usually has no `marginal_usage` rule at all, so §8.1's amortization
falls back to elapsed-fraction, and the denominator is apportioned by *time* rather than by
usage. Over a full period the ratio is exactly right; within a period a quiet hour reads as
poor leverage and a busy one as excellent, when neither is a fact about the plan. Read it
per period, or against the notional total, not per request; `notional` per key or team is who is consuming
the value a flat bill hides; and `notional` over the period is the number to compare against a
vendor quote when the plan stops fitting. Extension headers expose it as
`x-dorang-notional-usd`, and the ledger carries it per request, so the comparison is available
at any grain rather than only in aggregate.

A subscription's own rate card is also the natural place to notice that a plan has become a
bad deal. dorang does not act on that — it is not the gateway's decision — but it makes the
number impossible to miss.

### 8.2 Matching, pre-indexed **[R1-10]**

Specificity order: credential → deployment → (provider, model) → model → model prefix →
provider → default. Ties break on explicit priority, then rule id, deterministically.

Rules are **indexed by their static dimensions when configuration loads**, so a request
evaluates only the handful of candidate rules for its provider/model/credential, and only
time-dependent predicates are evaluated per request. Per-candidate results are memoized
within a request, because cost-based routing evaluates every candidate.

### 8.3 Arithmetic **[R1-11]**

Integer nano-units alone give neither precision nor safety: a sub-nano per-token price
rounds to zero or one per request, a systematic error that accumulates, and lifetime
accumulation can overflow a signed 64-bit nano value.

- Prices parse as **exact decimals**, never through binary floating point.
- Multiplication uses **128-bit intermediate** precision.
- Rounding happens **once, at the end**, half-to-even, with the remainder carried into the
  next settlement so repeated small requests do not drift.
- Every write is range-checked; overflow is an error, not a negative cost.

Components: input, output, cached read, cache write, reasoning, request, characters,
seconds. Cached counts are read from whichever usage field the backend reports.

> An earlier draft also listed `images` as a component, but the request type carries no image
> count, so the component was unreachable. Per-image pricing is expressible today as
> `per_request` on an image endpoint; a dedicated component is only worth adding once a
> frontend actually counts images, and inventing the field before then would ship an
> untested path.

**Estimating and settling are different operations.** The carried remainder and the
period-to-date accumulator are *state*, and routing must not mutate state — a cost-based
router prices every candidate on every request, so if pricing had side effects the losers
would corrupt the ledger. Estimation is pure and rounds each figure independently; only
settlement carries the remainder. The exactness guarantee of §8.3 therefore holds across
settlements, not across estimates, and that distinction is asserted by a test.

An unpriced model costs zero **and** increments a counter and logs a warning. Silent
zero-cost accounting is the failure mode this whole section exists to avoid.

### 8.4 Rendering

A preview endpoint returns the applied rule chain per class, each component's rate and
quantity, the subtotal, the final amount, and **why each rule was selected**. The admin UI
calculator and the CLI use the same engine, so there is one answer, not three.

---

## 9. Storage

### 9.1 Principles

- **The hot path does not touch the store.** Auth is an in-memory snapshot; metering is
  asynchronous.
- **Ledger and rollups are separate**, with different retention.
- **Indexes are derived from the query set**, which is fixed first (§9.3).
- Migrations are embedded and applied at startup. SQLite and PostgreSQL share one schema
  definition with dialect-specific DDL.

### 9.2 Tables

```
users · teams · team_members
api_keys(id, lookup, token_hash, hash_scheme, key_label, user_id, team_id,
         models[], allowed_routes[], max_budget, budget_period, budget_reset_at,
         rpm_limit, tpm_limit, max_parallel, priority_class, tags[], blocked,
         expires_at, created_at, updated_at)

providers · credentials · deployments · model_aliases · model_classes · pricing_rules

request_logs(...)            -- ledger; daily partitions (PostgreSQL); retention by tier
request_traces(request_id, ts, excerpt)   -- separate, sampled, byte-budgeted  [R1-3]

usage_by_key_hour · usage_by_model_hour · usage_by_team_day   -- purpose-built  [R1-13]
  (each carries notional_nano as its own column, never folded into cost_nano — §8.5)

quota_buckets(scope, scope_key, window, metric, bucket_start, value)
quota_leases(node_id, scope, scope_key, window, metric, amount, expires_at)   [R1-14]
budget_state(subject_kind, subject_id, period, period_start,
             spent, reserved, reserved_until)                                 [R1-5]
credential_state(credential_id, health, unavailable_until, quota_snapshot, updated_at)

responses_store(response_id, created_at, expires_at, owner_key_id,
                previous_response_id, model_group, items, reasoning_blobs)   [R1-C7]

nodes · capacity_leases · files · batches · batch_requests · audit_logs
```

**`responses_store` is not optional [R1-C7].** The Responses API carries server-side
conversation state — `store: true` plus `previous_response_id`. Revision 1 promised strict
compatibility for that endpoint while having nowhere to keep the state, leaving only two
runtime options, both of which break the promise: reject the request, or ignore the
reference and answer from a truncated conversation. `reasoning_blobs` holds the opaque
integrity-bearing reasoning handles of §10.2, keyed by response id, so they can be replayed
byte-identically. Entries expire; retention is configurable per tier.

**Message excerpts are not inline in the ledger [R1-3].** At the top of the scale range an
inline excerpt column dominates storage and drags ledger writes down with it. Excerpts live
in `request_traces`, sampled, with a daily byte budget that is enforced rather than hoped for.

Expected volume, stated rather than left to discovery:

| Tier | Rate | Ledger rows/day | Ledger bytes/day | Excerpts at 100% |
|---|---|---|---|---|
| Notebook | ~1 req/s | ~86 k | ~25 MB | ~44 MB |
| Team | ~50 req/s | ~4.3 M | ~1.3 GB | ~2.2 GB |
| Enterprise | ~2 k req/s | ~173 M | ~52 GB | ~88 GB |

Excerpt sampling and retention are set per tier from this table. Defaults: notebook keeps
everything, team samples, enterprise samples hard and shortens ledger retention.

### 9.3 Query set drives indexes **[R1-12]**

The ledger's own APIs — spend logs, per-user/team/tag activity, trace lookup, UI browsing —
were unsupported by revision 1's single `(ts, id)` key, so every one degraded to a partition
scan. The supported queries are fixed first:

| Query | Index |
|---|---|
| recent requests for a key | `(api_key_id, ts DESC, id DESC)` |
| recent requests for a team | `(team_id, ts DESC, id DESC)` |
| by trace id, within a range | `(trace_id, ts DESC, id DESC)` |
| spend for a credential over a range | `(credential_id, ts DESC, id DESC)` |
| errors over a range | partial index on `status >= 400`, `(ts DESC, id DESC)` |
| by tag | normalized `request_log_tags (tag, ts DESC, request_id DESC)` |

All ledger queries require a bounded time range and paginate. Unbounded search is refused.

Three corrections the implementation forced:

- **The trailing `id` is not decoration.** Pagination is keyset — `(ts, id) < (?, ?)` — so
  without `id` in the index every page costs a sort. An earlier draft listed `(key, ts DESC)`
  and would have paginated by sorting.
- **Trace-id lookup takes a range like everything else.** The draft exempted it, which
  contradicts the same section's own rule: on a partitioned ledger a bare trace-id lookup
  fans out across every partition's index for the whole retention period.
- **There is deliberately no default partition.** It converts a missing-partition failure
  from loud to silent, and then attaching the real partition later requires a full scan of
  everything that landed in the default. The writer instead pre-creates ahead and recovers
  in-line when the server reports no partition for a row.

**Cross-node write ordering matters.** Rollup batches are written in sorted primary-key
order. Two nodes flushing overlapping key sets in map-iteration order deadlock against each
other — a failure that appears only under concurrency and only sometimes.

### 9.4 Rollups **[R1-13]**

A single full-cube hourly key would bloat permanently and create hot-row contention when one
key dominates a bucket. Instead there are a few purpose-built materializations, each
matching a real query, and each node **pre-aggregates in memory and merges once per flush**,
so the number of writes touching a given row is bounded by node count, not request count.

### 9.5 Partitions **[R1-15]**

Partition pre-creation (at least two days ahead) and retention pruning ship **in the same
milestone as the writer**, not with clustering. Revision 1 scheduled them six milestones
apart, which would have failed every insert at the first midnight after metering shipped.
Clustering later restricts the job to the leader; it does not introduce it.

---

### 9.6 The write path — what may be deferred, and what may not

Metering is not the only thing that writes. Latency comes from whichever write is *not*
deferred, so every write is classified here rather than optimized one at a time, and each
carries a stated memory cost — a buffer with no bound is a leak with a schedule.

| Write | On the request path? | Mechanism | Memory |
|---|---|---|---|
| Numeric usage | no | per-CPU counters, merged and flushed on an interval | fixed: shards × cardinality cap |
| Request trace | no | bounded ring → durable local spool → batched insert | ring bytes + spool file cap |
| Rollups | no | merged in memory, one upsert per bucket per flush | bounded by live bucket count |
| Auth lookup | **miss only** | one coalesced read, then a snapshot | snapshot size, bounded by key count |
| Quota counters | no | minute-ring in memory, periodic upsert | fixed ring per (subject, window) |
| **Budget reserve/settle** | **yes — necessarily** | see below | small |
| Response store (stateful Responses) | **yes on write** | insert before the response is returned | bounded by payload cap |
| Batch state | no | scheduler-owned, checkpointed | bounded by in-flight rows |
| Capacity, sticky, prefix | never | memory only | prefix budgeted in bytes (§7.4b) |

**Budget is the one write that cannot be deferred, and pretending otherwise is how a budget
gets exceeded.** Deferring the reservation means two concurrent requests both see the
pre-spend balance, and §6.4's whole point is that they must not. So the reservation is
synchronous — but it is made cheap rather than made asynchronous:

- The hot path touches a **per-node in-memory reservation** guarded by an atomic, not the
  store. Durability comes from the lease the node already holds (§6.3), not from a write per
  request.
- The store sees a write when a **lease is taken or renewed**, which is per block of budget
  rather than per request. Lease size is the knob that trades store traffic against maximum
  overshoot, and §5.6 already requires that number to be published.
- Settlement is asynchronous and batched, because settling late is safe: the reservation was
  an upper bound, so the correction only ever releases budget.

**The response store is the other synchronous write**, and it is unavoidable for a different
reason: the caller receives a handle and may use it on the very next request, so the row must
exist before the response leaves. It is bounded by the payload cap and is only on the path for
stateful Responses traffic — which is why §2.1 treats it as its own surface rather than a
property of every request.

Three rules that follow, and are testable:

1. **No deferred write may be unbounded.** Every buffer has a byte cap, and reaching it is
   visible (§12.1's degraded state), never silent. A queue that grows until the process dies
   has converted a store outage into an outage.
2. **A deferred write must survive a restart or be counted as lost.** The trace spool is
   durable for exactly this reason. Quota rings and rollups are reconstructible from their
   last upsert plus the ledger, so losing the tail costs precision, not correctness — and the
   window over which that is true is the flush interval, which is why it is short.
3. **Nothing on the request path waits on a flush.** A flush that back-pressures into the
   request path defeats the whole arrangement; back-pressure surfaces as a drop with a counter
   instead. This is the one place where losing data is the correct answer, because the
   alternative is losing the request.

> **Known gap.** Quota and budget state are currently in-memory only. A restart therefore
> resets the windows, which is safe for concurrency (nothing is over-granted) but wrong for
> accounting — a monthly budget would silently start over. Persisting them through the lease
> mechanism above is required before the clustering milestone, since a lease that does not
> outlive its node is not a lease.

## 10. Protocols

### 10.1 Neutral representation

Frontends decode to a `CanonicalRequest`; backends encode from it. N×M adapter pairs become
N+M, which is what makes "full protocol" affordable at constant hot-path cost.

Cross-protocol conversion is required in both directions — an Anthropic-shaped request may
target an OpenAI-shaped backend and vice versa.

#### Two kinds of loss, handled differently **[R1-C6]**

Revision 1 said losses are "enumerated in a response header". A header carries a flat list
of parameter names, which is adequate for one kind of loss and useless for the other.

**Droppable parameters** — a knob the backend does not have (`logit_bias`, `seed`,
`top_logprobs`, an unsupported reasoning control). The request still means what it meant.
Listed in `x-dorang-dropped-params`. This is the revision 1 behavior and it is correct here.

**Structural downgrades** — the request cannot be expressed at all:

| Crossing | What is lost |
|---|---|
| prompt-cache breakpoints → a protocol without them | caching topology, and therefore cost, silently |
| multi-block tool results (text + image) → a single-string tool message | the non-text blocks |
| document/PDF content blocks → a protocol without them | the document entirely |
| richer stop reasons → a smaller enumeration | which specific terminal condition occurred |
| structured system blocks with per-block attributes → one system message | per-block attributes |

Returning `200` after silently discarding a PDF or destroying a caching strategy is **worse
than an error**: the caller has no way to know. Revision 2 therefore:

- **Fails fast with `400`** and a machine-readable body naming the unsupported construct,
  when the request actually contains one of these and the chosen backend cannot express it.
- Lets a caller opt into lossy conversion explicitly with
  `x-dorang-allow-lossy: <construct>[,…]`, in which case the loss is applied and reported in
  `x-dorang-downgraded`.
- Prefers, during routing, a backend that **can** express the request: capability becomes a
  routing filter (§7.1), so a request using constructs only one protocol family supports is
  routed there when such a deployment exists, and only fails when none does.

The full construct list is a versioned table in the compatibility document, and every entry
has a conversion test in both directions.

### 10.2 Reasoning level mapping

One neutral control, folded onto each backend's shape:

```
reasoning = { enabled, effort: none|minimal|low|medium|high|xhigh|max,
              budget_tokens?, summary: none|concise|detailed }
```

#### Capability is keyed by model, not by kind **[R1-C5]**

Revision 1 keyed the folding table on the provider `kind`. That is wrong twice over: within
one provider, reasoning control changes across model generations, and revision 1
generalized a two-level effort scale observed on **one specific model version** to an entire
family — so on the family's other members the reasoning control would be silently ignored
or rejected.

Revision 2 keys folding on **(kind, model capability)**, where capability comes from the
model catalog, matched by version-aware rules, exactly as the prior art it cites does.
An unverified capability is **not assumed**: the catalog marks it `unknown`, dorang omits
the control, and reports it in `x-dorang-dropped-params` rather than guessing. Populating a
model's real accepted form is an empirical step against the live endpoint, recorded in the
catalog with the date it was verified.

| Capability | Wire form | Folding |
|---|---|---|
| `effort_scale(levels…)` | `reasoning_effort` | clamp into the declared level set; never emit a level the model does not declare |
| `responses_reasoning` | `reasoning: {effort, summary}` | direct |
| `thinking_budget` | `thinking: {type, budget_tokens}` | see below |
| `thinking_flag` | `{thinking: {type: enabled\|disabled}}` | boolean only |
| `enable_thinking` | `enable_thinking` (+ budget where declared) | boolean, budget when supported |
| `unknown` / `none` | omitted | reported as dropped |

#### Budget is a function of the request, not a constant **[R1-C4]**

A fixed effort→budget table breaks when the caller asks for a small output: the budget must
stay strictly below the output ceiling, so every effort level collapses to the same value
and the scale becomes meaningless — or, if the ceiling is raised to fit the table, dorang
has silently multiplied the caller's requested output limit and their bill.

```
budget = min( table[effort], max_tokens − reserve )     reserve ≥ 1
if budget < min_thinking_budget:  reasoning is disabled for this request,
                                  and `x-dorang-dropped-params` says so
```

dorang never raises a caller's `max_tokens` to accommodate a reasoning budget. Reducing the
caller's requested output is not a decision the gateway is entitled to make silently.

#### Reverse mapping is best-effort and says so **[R1-C2]**

Revision 1 said reasoning output is "re-assembled" into the caller's protocol. That word
promises something unattainable. Where a protocol attaches **integrity-protected reasoning
blocks** that must be echoed verbatim on the next turn of a tool-use exchange, dorang cannot
synthesize a valid one from a different backend's plain-text reasoning field — and the
failure lands on the *second* turn of an agentic flow, not the first, which is the worst
possible place to discover it.

Revision 2:

- Reasoning **content** is normalized best-effort into the caller's shape and marked
  `x-dorang-reasoning: derived` when it did not come from a protocol-native block.
- Reasoning **blocks that carry integrity material are stored as opaque handles** keyed by
  response id, and replayed byte-identically when the caller echoes them back. dorang never
  fabricates or re-signs one.
- If a caller echoes a block dorang cannot produce for the selected backend, capability
  routing (§10.1) prefers a backend that can. If none exists, dorang fails with `400` and
  names the reason. It does not forward a fabricated block and it does not silently drop it.
- Round-tripping across reasoning representations is documented as **lossy and
  non-reversible**, with a conformance test for the two-turn tool-use case specifically.

### 10.3 Parameters

`drop_unsupported` (default on) filters against the kind's supported set; `params.drop[]`
removes named parameters unconditionally; `params.set{}` and `params.default{}` inject.
Every removal is reported.

### 10.4 Extension headers

| Header | Content |
|---|---|
| `x-dorang-request-id` | join key for logs |
| `x-dorang-model` | the name the client asked for |
| `x-dorang-upstream-model`, `real-model` | the real model id |
| `x-dorang-provider`, `-credential`, `-deployment` | selected target (ids, never secrets) |
| `x-dorang-attempt`, `-fallback-from`, `-route-reason` | routing decision |
| `x-dorang-queue-ms`, `-ttft-ms`, `-latency-ms` | latency breakdown |
| `x-dorang-tokens-*` | input, output, cache read/write, reasoning |
| `x-dorang-cost-usd` | this request |
| `x-dorang-notional-usd` | list-rate equivalent (§8.5) — an estimate, never billed |
| `x-dorang-spend-usd`, `-budget-usd`, `-budget-remaining-usd` | cumulative |
| `x-dorang-quota-*-used-pct` | credential quota windows |
| `x-dorang-dropped-params` | what conversion removed |
| `x-ratelimit-limit/remaining/reset-{requests,tokens}` | standard form |
| `retry-after` | on 429 |

**Header set is bounded.** Only the identification and cost headers are always attached.
The rest are emitted when `x-dorang-detail: full` is requested, or when
`observability.always_full_headers` is set. Revision 1 attached roughly thirty headers to
every response, which risks intermediary header-size limits and adds bytes ahead of the
first streamed byte — against the very target it was serving **[R1-C9]**.

**Streaming delivery is one channel, opt-in [R1-C9].** Revision 1 promised a terminal SSE
event *and* HTTP trailers. Trailers are a dead channel — mainstream LLM client libraries
read the SSE body and never surface them — so "both" meant "one, plus an unused one". Worse,
injecting a dorang-authored SSE frame into the upstream stream contradicts forwarding the
upstream bytes untouched.

Revision 2: post-hoc values for streaming responses are delivered **only** when the client
opts in with `x-dorang-usage-events: 1`. Without it the stream stays byte-faithful to
upstream (modulo the alias rewrite of §7.2) and the numbers are available from the ledger by
`x-dorang-request-id`, which is a response header and always present.

### 10.5 Priority passthrough

See §7.5. **A client-supplied priority is ignored by default.**

Clamping to a permitted range was the earlier rule and it is not safe enough. Priority is a
claim on shared capacity, so if callers may set it, every caller eventually sets the most
urgent value — not maliciously, just because it is free and appears to help. At that point the
scale carries no information, and the callers who left it alone are the ones penalised. A
clamp only bounds how far the self-elevation goes; it does not remove the incentive.

```yaml
capacity:
  principals:
    default:            { client_priority: ignore }          # the default
    batch-pipeline:     { client_priority: allow, range: [batch, interactive] }
    latency-sensitive:  { client_priority: allow, range: [interactive, realtime] }
```

- `ignore` — the hint is dropped and the principal's configured class is used. Dropping is
  reported in `x-dorang-dropped-params`, because silently discarding something a caller sent
  is the behaviour §10.3 exists to prevent.
- `allow` — the hint is honoured, clamped to the stated range. A principal granted a range has
  been given a budget of urgency deliberately, by an operator who can see the whole fleet.

The asymmetry is deliberate: **an operator can grant urgency, a caller cannot claim it.**
Which class a request belongs to is a statement about its importance relative to other
tenants' work, and the caller is the one party with no view of that.

### 10.5a dorang does not compact — a decided non-goal

dorang knows a deployment's real context window (VLLM.md §1.1) and the caller often does not,
which makes "compact on the caller's behalf" a tempting feature. **It is a non-goal.**

Compaction means rewriting someone's conversation: dropping turns, eliding tool results, or
substituting a summary. A gateway doing that silently changes what the model was asked, and
the caller has no way to see it in the response. The failure is not a wrong answer the caller
can spot — it is a plausible answer to a question they did not ask. An agent loop that
compacts its own history knows what it discarded and can compensate; a gateway does not and
cannot.

So the context window is used for **routing and refusal**, never for rewriting:

| Situation | dorang does |
|---|---|
| Request fits the target's window | dispatch |
| Does not fit, a same-class deployment with a larger window exists | route there (§7.6 `context_window`) |
| Does not fit anywhere in the class | **fail with a clear error naming the real limit** |
| Caller invokes a vendor's own compaction API | **pass it through** (§10.6), do not interpret it |

**Compaction is passthrough by default, and that is the only safe default.** The survey found
four incompatible server-side mechanisms across two vendors — a route, a sentinel item buried
inside an input array with no request-line signal at all, an array-shaped field, and an
object-shaped field gated by a beta header — of which two share a field name and nothing else.
A gateway that tried to recognize them would have to detect four unrelated shapes correctly,
and would silently mishandle the fifth that ships next month.

So dorang does not detect compaction at all. Compaction endpoints and fields are relayed
byte-identically as opaque state (§10.1), including the response, so the caller's own
compaction cursor round-trips intact. dorang neither strips these fields when it does not
recognize them nor normalizes them when it does — **not recognizing something is not a reason
to remove it.**

The one thing dorang must still do is *account* for it: a compaction round trip consumes
tokens and costs money, so it is metered like any other request even though its body is never
inspected (§10.6 step 5).

> **Estimation must err pessimistic, and `bytes/4` does not.** That ratio undercounts CJK
> substantially, and undercounting is precisely the optimistic direction §4.3 warns about:
> dorang believes the request fits, dispatches it, and context-window fallback never fires.
> The estimator must be script-aware and biased high — an over-estimate costs an unnecessary
> route to a larger model, an under-estimate costs a hard failure the router cannot see.
>
> **A `200` is not proof the request fit.** Some backends silently clamp oversized input;
> at least one truncates and returns a normal-looking length stop with no output. So overflow
> detection cannot rely on error signatures alone, and dorang must never enable a backend's
> own auto-truncation option — doing so disables the very detection §7.6 depends on.

Failing is the correct outcome in the third row. The caller learns the real limit — which is
information they did not have and cannot get elsewhere — and decides for themselves what to
drop. That is a better outcome than a silently shortened conversation.

### 10.6 Generic passthrough engine

Provider-native routes are opened by configuration, not by writing an adapter each time.

```yaml
passthrough:
  enabled: true
  routes:
    - { prefix: /anthropic, provider: anthropic-main }
    - { prefix: /vllm,      provider: vllm-local }
  default: { auth: dorang, meter: true, timeout: 600s }
```

1. Authenticate with a dorang key (or pass client credentials through, if configured).
2. Strip the prefix, join onto the provider's base URL.
3. Relay the body **without parsing it**, both directions, streaming.
4. Replace only dorang-owned headers; strip hop-by-hop headers.
5. Meter best-effort: if the response is JSON with a usage object, price it; otherwise record
   counts and bytes. Metering failure never fails the request.
6. WebSocket upgrades are relayed frame-for-frame by the same engine.

**Security boundary**: unmapped prefixes are not served — this is not an open proxy. Joined
paths are normalized and traversal is rejected. Provider credentials never reach the client.

### 10.7 Metadata equivalence across protocol families

The three shapes dorang must speak — OpenAI chat-completions, OpenAI responses, and
Anthropic messages — name the same concepts differently and disagree on a few of them.
A neutral representation is worthless if each adapter invents its own reading, so the
mapping is **normative**, not per-adapter discretion. The rule for resolving disagreements
is **whatever the market already does**, because the value of this table is that existing
clients keep working, not that it is elegant.

Canonical names below are the field names in `CanonicalRequest` / `CanonicalResponse`.
`—` means the family has no equivalent, which is a §10.1 structural-loss case, not a
droppable parameter.

#### Request

| Canonical | chat-completions | responses | messages |
|---|---|---|---|
| `Model` | `model` | `model` | `model` |
| `Messages` | `messages[]` | `input[]` | `messages[]` |
| `System` | `messages[role=system\|developer]` | `instructions` | `system` (top-level) |
| `MaxOutputTokens` | `max_completion_tokens`, falling back to `max_tokens` | `max_output_tokens` | `max_tokens` (**required**) |
| `Temperature` / `TopP` | same | same | same |
| `TopK` | — | — | `top_k` |
| `Stop` | `stop` | — | `stop_sequences` |
| `Stream` | `stream` | `stream` | `stream` |
| `Tools` | `tools[].function` | `tools[]` (flat) | `tools[]` (flat, `input_schema`) |
| `ToolChoice` | `tool_choice` | `tool_choice` | `tool_choice` |
| `ParallelToolCalls` | `parallel_tool_calls` | `parallel_tool_calls` | `disable_parallel_tool_use` (**inverted**) |
| `ResponseFormat` | `response_format` | `text.format` | — |
| `Reasoning` | `reasoning_effort` | `reasoning{effort,summary}` | `thinking{type,budget_tokens}` |
| `Seed` | `seed` | — | — |
| `Logprobs` | `logprobs`,`top_logprobs` | — | — |
| `EndUser` | `user` | `user` | `metadata.user_id` |
| `Metadata` | `metadata` | `metadata` | `metadata` |
| `PreviousResponseID` | — | `previous_response_id` | — |
| `Store` | — | `store` | — |
| `ServiceTier` | `service_tier` | `service_tier` | — |
| `CacheBreakpoints` | — | — | `cache_control` on blocks |

Three of these are traps and each gets a test:

- **`max_tokens` is required in messages and optional elsewhere.** Crossing into that family
  without one must supply the model's max output from the catalog, not omit the field.
- **`ParallelToolCalls` is inverted** in one family. A naive copy silently reverses it.
- **`max_completion_tokens` supersedes `max_tokens`** in chat-completions. Reading the wrong
  one caps output at the legacy value.

#### Response and usage

| Canonical | chat-completions | responses | messages |
|---|---|---|---|
| `ID` | `id` | `id` | `id` |
| `Model` | `model` (restamped every chunk) | `model` | `model` |
| `StopReason` | `choices[].finish_reason` | `status` + `incomplete_details.reason` | `stop_reason` |
| `StopSequence` | — | — | `stop_sequence` |
| `InputTokens` | `usage.prompt_tokens` | `usage.input_tokens` | `usage.input_tokens` |
| `OutputTokens` | `usage.completion_tokens` | `usage.output_tokens` | `usage.output_tokens` |
| `CacheReadTokens` | `usage.prompt_tokens_details.cached_tokens` | `usage.input_tokens_details.cached_tokens` | `usage.cache_read_input_tokens` |
| `CacheWriteTokens` | — | — | `usage.cache_creation_input_tokens` |
| `ReasoningTokens` | — | `usage.output_tokens_details.reasoning_tokens` | (inside output tokens) |
| `TotalTokens` | `usage.total_tokens` | `usage.total_tokens` | — (but see COMPATIBILITY 6.8) |

**Token accounting is the part that must be exactly right**, because it feeds cost, quota,
and budget — a mis-mapped cache field does not produce a visible error, it produces a wrong
invoice. Two normalizations are mandatory:

1. **Input tokens are reported the same way in all three directions.** One family reports
   cache reads *inside* the input count; another reports them *outside* it. dorang normalizes
   to **inclusive** — `InputTokens` is the full prompt count, cache reads and writes included,
   matching the OpenAI convention — and the encoder **subtracts** on the way out for the
   family that expects exclusive. Getting this wrong mis-counts every cached request, which in
   an agentic workload is most of them.

   > ⚠️ **This paragraph said "exclusive" until it was corrected, and the inversion is worth
   > recording rather than quietly fixing.** The implementation had gone inclusive — which is
   > what `canonical.Usage` documents, what `TotalTokens() = input + output` requires, what the
   > OpenAI adapter does in both directions, and the only reading under which COMPATIBILITY
   > 6.7's `input_tokens = prompt − cache_read − cache_creation` type-checks at all. Following
   > the design literally would have billed **every cached request through an OpenAI backend
   > roughly 1.8× over**, with no error anywhere.
   >
   > That is precisely the failure this very section warns about two paragraphs above — "a
   > mis-mapped cache field does not produce a visible error, it produces a wrong invoice" —
   > and the warning was written with the direction backwards. A prose invariant about
   > arithmetic is worth less than the arithmetic: the rule is now stated as the formula the
   > encoder implements, and a round-trip test in each adapter asserts it, so the next
   > inversion fails a test instead of an audit.
2. **Reasoning tokens are billed as output.** One family breaks them out, another folds them
   in. `ReasoningTokens` is reported separately *and* is already contained in
   `OutputTokens`, so cost never adds them twice. A test asserts
   `OutputTokens >= ReasoningTokens`.

#### Streaming events

| Canonical | chat-completions | messages |
|---|---|---|
| stream open | first chunk with `delta.role` | `message_start` |
| text delta | `delta.content` | `content_block_delta{text_delta}` |
| tool call start | `delta.tool_calls[i]` with `id`+`name` | `content_block_start{tool_use}` |
| tool argument delta | `delta.tool_calls[i].function.arguments` | `content_block_delta{input_json_delta}` |
| block end | (implicit) | `content_block_stop` |
| stop | `finish_reason` | `message_delta{stop_reason}` |
| usage | final chunk when opted in | `message_delta.usage` |
| stream end | `data: [DONE]` | `message_stop` |

The asymmetry that costs the most: one family has **explicit block boundaries** and the other
has **implicit** ones. Going from implicit to explicit means synthesizing `content_block_stop`
at every transition and holding the terminal event until the block is closed — the stateful
part of COMPATIBILITY 6.6, and the reason that adapter has its own fuzz target.

#### TODO — Vertex

Aligning with Vertex/Gemini shapes (`contents[]`, `systemInstruction`, `generationConfig`,
`usageMetadata`) is **noted, not planned**. It is a fourth naming of the same concepts with
its own quirks, and it earns its place only when a deployment actually needs it. The table
above is deliberately structured so a fourth column can be added without disturbing the
three that carry real traffic.

---

## 11. Batch, users, administration

### 11.1 Batch — backend-independent

The batch scheduler is dorang's own. It does not depend on any upstream exposing a batch
endpoint, because that cannot be assumed across backends. A native batch path is an
**optional accelerator** enabled per provider, not the foundation.

```
upload → validate (JSONL, unique custom ids, known models, size and line ceilings)
create → queued → in_progress
  rows grouped by prefix hash (§7.4b) so cache-adjacent work runs together
  each row is an ordinary request at batch priority (§7.5)
  everything flows through §5, so batch cannot exceed configured capacity
  retries with backoff on retryable statuses; partial failure is expected
complete → output JSONL + error JSONL
cancel → cancelling → in-flight drains → cancelled, partial results preserved
```

**Interactive work is protected on every axis [R1-12].** Revision 1 capped batch at a share
of *credential* concurrency, which does not protect the *model* axis — batch could take a
model's entire limit while staying under its credential share, starving interactive traffic
for that model completely.

Revision 2 uses `capacity.interactive_reserve`: on **every** axis, a fraction of the ceiling
is reserved for non-batch work and batch cannot occupy it. The scenario test includes
model-axis contention specifically, which revision 1's test would not have caught.

### 11.2 Authentication

Bearer token → scheme-independent lookup (§2.4) → in-memory principal. Misses consult the
store once, coalesced. Team, user, and key limits all apply; the most restrictive wins.
External key management may be delegated over HTTP with a fixed principal schema.

### 11.2b OAuth credentials refresh themselves

Some providers authenticate by OAuth rather than a static key, and an access token that
expires mid-flight is a `401` the caller did nothing to deserve. dorang refreshes them, and the
mechanics matter more than the feature:

```yaml
credentials:
  - id: plan-oauth-1
    provider: some-plan
    auth: oauth
    oauth:
      source: file                       # file | exec | env
      path: ~/.some-vendor/auth.json     # the vendor CLI's own store
      refresh_margin: 5m
      account_id_field: account_id       # sent as a separate header where required
```

- **Refresh happens ahead of expiry, not on failure.** `refresh_margin` triggers renewal while
  the current token is still valid, so a refresh never sits on a request's critical path. A
  `401` is treated as a *second* signal — refresh once and retry once — because clock skew and
  server-side revocation both exist, but it is the fallback, not the mechanism.
- **One refresh per credential, coalesced.** Concurrent requests on an expiring token must not
  each mint a refresh: some providers invalidate the previous refresh token on use, so a
  stampede does not merely waste calls, it can **lock the account out**. Refresh is
  single-flight per credential, and the losers wait for the winner's result.
- **The token store is shared with the vendor's own CLI, so writes must not corrupt it.**
  Refreshed tokens are written atomically (temp file, fsync, rename) with the original file
  mode preserved. A half-written credential file breaks the CLI too, and the operator will not
  suspect the gateway.
- **A refresh failure marks the credential unhealthy; it does not fail the fleet.** The
  credential steps aside exactly as an exhausted quota does (§6.1), traffic moves to another
  account, and the reason is reported. What it must *not* do is retry in a tight loop against
  an auth server — that is how a recoverable expiry becomes a rate-limit ban.
- **Refresh never happens on the request path.** A background loop per credential, off the hot
  path, in keeping with §9.6.

Two constraints inherited from elsewhere in this design:

**An OAuth credential is still a credential**, so §7.4a2's affinity rules apply unchanged — a
conversation pinned to an account stays pinned across a token refresh, because the account is
the same account. The token is an implementation detail of talking to it.

**Tokens are secrets and follow §4.1** — never logged, never in an error message, never in a
`Snapshot`. The only thing that leaves this subsystem is the credential's opaque id and its
health.

### 11.3 Administration UI

A single SPA embedded in the binary — no external assets, works offline. Version 1 ships
three screens: **keys**, **models and deployments**, **usage and cost**. Request-log
browsing, the price calculator, and batch management follow.

### 11.4 Users and teams

Both the shape-compatible paths (§2.3) and a native admin API.

### 11.5 Email and Lua extension

```yaml
notifications:
  email:
    driver: smtp            # smtp | http | lua | none
  events: [key_created, budget_80pct, budget_exceeded, quota_exhausted,
           credential_unhealthy, batch_completed, invite]
```

Lua hooks — `on_request`, `on_route`, `on_response`, `on_email` — run in a sandbox with
instruction, memory, and wall-clock ceilings. Disabled by default on the hot path. Exceeding
a limit skips the hook and warns (fail-open), except an explicit deny from `on_request`,
which is honored (fail-closed).

---

## 12. Observability

### 12.1 Two metering queues **[R1-1, R1-2]**

Revision 1 had one ring buffer and dropped the ledger when it filled, while claiming rollups
stayed accurate. Both halves were wrong: dropping the ledger loses exactly the tracing the
requirements ask for, and keeping multi-dimensional rollups accurate requires touching a
keyed aggregate, which is not the free stack-allocated operation that was assumed.

```
request completes
   ├─▶ numeric event  → per-CPU fixed-cardinality accumulator  (ALWAYS kept)
   │                     merged and flushed to rollups
   └─▶ trace payload  → bounded queue → durable local spool → ledger tables
                         (sampled; droppable, and the drop is visible)
```

- **Numeric accounting is never lost.** Counters are fixed-cardinality per CPU, merged on
  flush. Cost, tokens, and error counts survive any back-pressure.
- **Trace payloads spool to local disk before the store**, so a store stall costs disk, not
  data. When the spool is also full, drops are counted and the gateway reports
  `metering_degraded` through health and metrics. It is never silent.

### 12.2 Tracing

Per request: trace id, span ids, and a latency breakdown — queue, route, capacity wait,
upstream connect, TTFT, total. OTLP export is optional; the breakdown is always recorded.

Message content defaults to a truncated excerpt (512 characters) in `request_traces`,
subject to sampling and a daily byte budget. `none` and `hash` are also available.

### 12.3 Metrics

Requests, duration, TTFT, tokens, cost, capacity in-flight and wait time per axis,
credential health, provider quota percentage, budget consumption ratio, prefix hit ratio,
fallbacks by reason, metering drops, and spool depth.

### 12.4 Backend metrics

A provider may declare a metrics endpoint to scrape. Queue depth and cache utilization then
become routing signals for `least_busy` and `highest_tps`. Collection ships first; using it
for routing is opt-in behind a flag.

For vLLM specifically — a **day-zero** backend, not an afterthought — the metric names,
their four traps, the per-response load header that beats polling, and the load endpoint
that returns an attractive-looking zero forever when unconfigured are all specified in
[VLLM.md](VLLM.md) §3. That document also resolves two things this design had left open:
a vLLM deployment's real context window is readable with one unauthenticated GET (§1.1),
and `priority` is **silently ignored** under the default scheduler policy while its own
field description claims otherwise (§1.2).

---

## 13. Clustering

| State | Storage | Across nodes |
|---|---|---|
| configuration | store | shared, change-notified |
| auth snapshot | memory | per node, TTL + invalidation |
| ledger and rollups | store | shared, append and merge |
| quota and budget | store (+ Redis) | §5.6 / §6.3 modes |
| concurrency | §5.6 mode | `local` forbidden when clustered |
| sticky and prefix tables | memory (optionally Redis) | node-local by default |
| batch jobs | store | leased to one node |

The request path is stateless; any node can serve any request. Sticky and prefix tables are
node-local by default, which costs hit rate but never correctness — consistent hashing at
the load balancer recovers it, and sharing them through Redis is available at the cost of
one lookup.

The leader — elected through a store lock — owns rollup compaction, partition maintenance,
expiry sweeps (capacity reservations §5.3 **and** budget reservations §6.4), batch
assignment, and lease rebalancing.

Draining: readiness off, in-flight requests finish within the grace period, leases and
reservations released, then exit.

---

## 14. Testing

| Layer | Focus |
|---|---|
| Unit | routing decisions, capacity acquisition, quota windows, price matching, protocol conversion, reasoning folding |
| Property | **prefix chain order-sensitivity** (permutation, insertion, deletion must all diverge), price monotonicity, budget never exceeded |
| Concurrency | capacity under `-race`; no lost reservations; **bounded wakeups**; FIFO fairness; no multi-axis starvation |
| Fuzz | JSONL, SSE, protocol converters |
| Golden | request/response bytes for every frontend×backend pair |
| Integration | SQLite and PostgreSQL, migrations, rollup correctness, partition rollover across midnight |
| Scenario | below |
| Benchmark | per-milestone gates (§15.3) |
| Soak | 24 h; zero leaked goroutines, reservations, or memory |

Required scenarios, all automated:

1. Two accounts × 3 concurrent → the 7th waits; a release admits it
2. One key × 7 per model → two models reach 14 concurrent; the 15th waits
3. Both constraints together → blocks at the account total, not the model limit
4. Quota exhausted → that credential steps aside, traffic continues on another, returns after reset
5. Budget cap → never exceeded, under 100 concurrent requests
6. Model failure → same-group fallback → whole group down → same-class delegation
7. Stream interrupted → fallback only before first byte; zero duplicated output after
8. Alias → real model upstream, requested name in body, real model in header
9. Same prefix → same target; **reordered messages → free to choose differently**
10. Sticky TTL elapsed → re-routed
11. Two nodes, one killed → no interruption, no double-counted budget or quota
12. Configuration import → normalized model and deployment structure matches golden
13. Legacy credential import → expired rows stay expired; live rows authenticate; rehash-on-use upgrades them
14. Batch of 1000 with partial failures → correct output/error JSONL; **interactive latency on the contended model axis within bound**
15. Anthropic-shaped request through an OpenAI-shaped backend, tool calls included
16. Metering on vs off → within the stated bound, measured in steady state **and** at a full buffer
17. Saturation: 1000 waiters on a limit of 7 → bounded wakeups, FIFO, no starvation
18. Midnight rollover with metering active → no failed writes

### 14.1 Shadow comparison

Progressive migration needs proof from real traffic, not only synthetic tests.

```yaml
shadow:
  mode: compare              # off | mirror | compare
  reference: { url: …, api_key_env: … }
  sample_rate: 0.05
  compare: { structural: true, semantic: false }
  max_cost_usd_per_day: 5
```

Structure only — status, field set, types, header keys — because model output is not
deterministic. Mirroring sends each sampled request twice and therefore costs twice, so the
sample rate is low and a daily cost ceiling is enforced. An empty diff report is the
completion criterion for taking over traffic.

---

## 15. Performance

### 15.1 Targets, per profile **[R1-8]**

A single latency number is unfalsifiable — any path that misses it can be defined as out of
scope. Targets are stated per profile, and the measurement boundary is fixed:

> **Gateway overhead** = from the last byte of the request line+headers being read, to the
> first byte written upstream, **plus** from the last upstream byte to the last byte written
> to the client. It **excludes** upstream time. It **includes** authentication, routing,
> capacity acquisition, pricing, parameter transformation, and enqueueing metering.

| Profile | p50 | p99 | Notes |
|---|---|---|---|
| `warm-local` — auth cached, local capacity, prefix on, ≤4 KiB body | 200 µs | 2 ms | headline |
| `cold-auth` — auth cache miss, one store read | — | 15 ms | |
| `shared-redis` — clustered exact capacity | — | 5 ms | +1 RTT, deliberate |
| `external-auth` — delegated auth | — | governed by the callee | stated, not promised |
| `lua-enabled` — hooks active | — | +hook ceiling | ceiling is configured |
| `prefix-1MiB` / `prefix-16MiB` — large bodies | — | 6 ms / 60 ms | scales with body size, stated separately |

| Other | Target |
|---|---|
| Added TTFT, streaming | p99 < 1 ms |
| Idle RSS, notebook profile | < 100 MB |
| 1000 concurrent streams | < 300 MB **including** the replay budget (§15.4) |
| Metering on vs off | **< 5% of the gateway-overhead budget** (see note below). Measured **+148 ns** steady, **+110 ns** at a full buffer = **0.074%** of the 200 µs warm-local p50 |

> **"5%" needed a denominator.** An earlier draft said only "metering on vs off < 5%", which never
> said 5% *of what*. Measured against a no-op meter the ratio is **7.6×** — but the no-op returns after
> one branch, so dividing by it measures nothing about a real request. The requirement is 5% of the
> **gateway-overhead budget**, so added-nanoseconds-against-that-budget is the figure that answers it.
> Both numbers are reported, and the gate asserts steady state **and** a full buffer — at which point
> metering is *cheaper* (110 ns), because a failed ring push skips the payload copy while the numeric
> path does identical work. That asymmetry is the two-queue split (§12.1) behaving as designed.

### 15.2 Techniques

1. **Immutable routing snapshot** — configuration swaps by pointer; the read path takes no lock.
2. **No allocation on the hot path** — pooled request structures; only the fields needed
   (`model`, `stream`, size markers) are scanned, never a full unmarshal.
3. **Single-pass, no-decode streaming relay [R1-C1]** — upstream SSE frames are scanned,
   never fully decoded. `Accept-Encoding: identity` is requested upstream. The scanner
   rewrites only the `model` value and reads only the terminal usage frame, carrying a
   partial frame across reads; everything else is forwarded byte-for-byte. When the
   requested and upstream names match and no usage rewriting is needed, it degrades to a
   plain copy. Revision 1's "zero-copy with offset patching" claim was not achievable
   alongside aliasing, and is withdrawn.
4. **Tuned upstream connection pools**, keep-alive, cached DNS.
5. **Split asynchronous metering** (§12.1).
6. **O(1) auth** against a lock-free snapshot.
7. **Sampled structured logging**; successful requests are metrics, not log lines.

### 15.3 Throughput is specified as a workload **[R1-9]**

"20k req/s" means nothing without saying what a request is. The benchmark workload is fixed:
1 KiB / 32 KiB / 400 KiB bodies; keys drawn from a skewed distribution; 100 pricing rules
loaded; prefix routing on and off; metering persisting to a real store; ten minutes of steady
state. Two numbers are reported separately and both must hold:

- **Served throughput** — what the request path sustains.
- **Durable throughput** — what the storage layer absorbs without growing a backlog.

### 15.4 Replay memory is budgeted process-wide **[R1-6]**

Fallback requires the ability to resend, which requires retaining the body. A per-request
cap alone is not a bound: many concurrent requests each just under the cap exceed the whole
memory target. There is a **process-wide replay budget**; a request that cannot fit is
marked non-replayable (and says so in a header) rather than being retained anyway.

### 15.5 Prohibited on the hot path

Synchronous store access — except the one deliberate round trip of an exact shared capacity
mode, which is a stated cost, not an accident. Reflection, regular expressions, and
formatted string construction. Full-body buffering beyond the replay budget.

---

## 16. Repository layout

```
cmd/dorang · cmd/dorangctl
internal/{server,canonical,wire/<family>,router,capacity,prefix,health,quota,
          pricing,meter,store,auth,admin,batch,cluster,config,luaext,passthrough}
pkg/catalog          provider defaults, model catalog, base pricing
ui/                  embedded admin SPA
deploy/              compose for tests, Dockerfile, examples
docs/                DESIGN, REVIEW, CONFIG, COMPATIBILITY (+ .ko)
testing/             fake upstreams, scenario harness
```

---

## 17. Milestones

Every milestone carries a **performance gate**, because deferring performance validation to
the end means discovering structural problems after everything is built on top of them
**[R1-18]**.

| M | Scope | Completion gate |
|---|---|---|
| M0 | scaffolding, license, CI, test dependencies | build and test green |
| M1 | configuration, catalogs, `/v1/models`, echo backend | **golden test of normalized structure only** — routing behavior is M7's gate, not M1's **[R1-17]** |
| M2 | **capacity** — axes, atomic reservation, per-axis FIFO, aging | scenarios 1–3, 17; **saturation benchmark**; `-race` |
| M3 | openai-chat both directions, streaming relay, fallback skeleton | scenario 7; **overhead benchmark at 1 KiB / 32 KiB / 400 KiB** |
| M4 | auth, storage, migrations, credential import | scenario 13 |
| M5 | metering, pricing, extension headers, **partitions and retention** | scenarios 16, 18; **durable-throughput benchmark** |
| M6 | quota, budget, provider probes, reservation sweeps | scenarios 4, 5 |
| M7 | all strategies, sticky, **prefix chain** | scenarios 9, 10; property tests; **prefix benchmark at 4 KiB / 1 MiB / 16 MiB** |
| M8 | messages, responses, embeddings, rerank, audio, images, moderations, ocr | scenario 15 |
| M8.5 | **generic passthrough + WebSocket relay** | passthrough routes open by configuration alone |
| M9 | admin API, three UI screens, **shadow mode** | shape-compatible paths work; shadow diff report |
| M10 | batch and files | scenario 14, including model-axis contention |
| M11 | clustering, leader, shared capacity, leases | scenarios 11; **shared-mode p99 gate** |
| M12 | Lua hooks, email, backend metrics | R21, R17 |
| M13 | remaining protocol surface and admin API | no route answers 404 where 501 is correct |
| M14 | integration regression and soak | every §15.1 target measured, not estimated |

M2, M3, and M4 are independent and run in parallel. M5–M8 parallelize per adapter once M3
fixes the neutral representation. M9 and M12 are independent of the core throughout.

---

## 18. Open risks

| # | Risk | Status |
|---|---|---|
| W1 | Cluster + local capacity mode | **closed** — refuses to start (§5.6) |
| W2 | Credential import premise | **closed** — verified; motivation corrected (§2.4, REVIEW R1-A) |
| W3 | Dependence on an upstream batch endpoint | **closed** — own scheduler (§11.1) |
| W4 | Prefix computation cost | **mitigated** — byte-based, streaming; M7 gate proves it |
| W5 | Cross-protocol conversion loss | **open** — enumerated in headers; may need per-pair fidelity tests beyond golden |
| W6 | Single-mutex broker throughput | **mitigated** — M2 gate decides; sharding invariant defined (§5.7) |
| W7 | Sticky/prefix hit-rate dilution across nodes | **open** — documented; consistent hashing recommended, Redis sharing available |
| W8 | **Multi-axis waiter starvation under sustained saturation** (§5.4 correction 5) | **open** — inherent to no-partial-holding; needs a soft-reservation protocol. Not a deadlock and not an overtaking problem, so aging does not close it. Measurable: a waiter that never wins while both its axes stay saturated |
| W9 | **Quota and budget state is in-memory only** (§9.6) | **open** — a restart resets the windows. Safe for concurrency, wrong for accounting: a monthly budget silently starts over. Must be persisted through the lease mechanism before clustering, since a lease that does not outlive its node is not a lease |
