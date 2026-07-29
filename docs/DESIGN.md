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
| R17 | Backend metrics integration — **not built**; `providers[].metrics` is refused at load and §12.4 names what serves `least_busy`/`highest_tps` instead | §12.4 |
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

Each layer is one package, and L5 is the one whose boundary is easy to get wrong, so it is
stated here. `internal/backend` owns everything between "the router chose a deployment" and
"the client's protocol has an answer": endpoint derivation, credential application, the
canonical round trip, error normalization to COMPATIBILITY §11's taxonomy, the streaming
relay, and the per-provider timeout and retry policy. `internal/app` **assembles and calls**
— it turns a configuration file into `backend.Provider` values and a routing decision into a
`backend.Target`, and never builds an HTTP request, spells a credential, or parses a
provider's error envelope itself. There is exactly one implementation of each of those, and
that is the point: a second one is only ever discovered to disagree with the first by an
upstream answering 401, or by a caller receiving a tool name they never declared.

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
  pre_stop_delay: 10s            # readiness off, still serving — §13

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

5. **Aging prevents overtaking, not starvation** — a waiter needing two saturated axes is
   never overtaken and never wins. **Closed by soft reservation**, and four things this
   correction said about it were wrong:

   - **"Bar new grants on A" is too strong.** The waiter needs one *unit*, not the axis.
     Barring the axis idles all of L; reserving one unit idles 1/L. A claim is therefore one
     unit of one axis key, counted into the in-use total so everyone except the claimant
     simply sees a fuller axis — which means the existing `inUse <= limit` invariant is the
     entire correctness argument, and nothing new is held across a wait.
   - **"It is always at the head of both queues" is not what happens.** A waiter is queued
     only on the axes that blocked it, and leaves a queue as soon as that axis has room. The
     starvation is real; the mechanism described was not.
   - **A dropped claim must be given back *and re-offered*.** Releasing one on cancellation
     without serving its bucket's queue is a lost wakeup with no time bound — a defect the
     naive reading introduces. It has its own test.
   - **The guarantee cannot cover batch work under an interactive reserve** (§11.1). A batch
     ceiling is `floor(limit × (1−reserve))` while interactive's is the full limit, so a claim
     — which occupies one unit against everybody — cannot protect a unit below the batch
     ceiling from interactive traffic entitled to sit above it. Making it bite would invert
     §11.1's protection. The exclusion is exactly that narrow: with no reserve configured,
     batch claims like anything else.

   **Deadlock freedom is a total order, and it is enforced.** Claims are taken as a *prefix*
   of the §5.7 axis order, which is total within a candidate and identical for every waiter
   because it is a property of the axis rather than the request — so the cycle a mutual
   soft-reservation would need cannot form.

   **Bounds.** Once oldest, a waiter is served within `SoftReserveAfter + axes` releases —
   11 in the worst case, against a hole that was previously unbounded. Cost is one idled unit
   per claimed axis key, and measured **+5.3%** on the mixed contended workload that provokes
   it, nil on single-axis contention (which never claims). On by default; capacity acquisition
   is ~450 ns against §15.1's measured 480 µs budget, so the trade is a liveness guarantee for a
   fraction of a percent of the gateway.

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

**Most providers have no such endpoint, and shipping a prober anyway is worse than shipping
none** — §6.2 makes the reported figure authoritative, so a guessed one overrides a correct
local count. Research across nine providers found three with a usable endpoint. Of the six
without: several expose only organization-scoped usage behind an admin credential, which is a
*different credential* from the one serving the traffic; one exposes key metadata with no
usage at all; one is project-scoped, where a key-scoped answer is not well defined; and one
publishes an endpoint whose percentage may encode **remaining** where every other provider
encodes **consumed** — a reading that looks correct at 50% and is backwards everywhere else.
That one is refused deliberately rather than implemented hopefully.

The selection rule: **a prober authenticates with the credential that serves the traffic, and
reports what is left of that credential's own allowance.** Anything else measures something
adjacent and calls it the answer.

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

- **Reservations expire [R1-19].** A process killed between reserve and settle would
  otherwise lock that amount forever, and over time reserved-but-never-settled amounts would
  exhaust a budget nothing actually spent. The leader reclaims expired holds, exactly as §5.3
  does for capacity. The two mechanisms are the same pattern and have the same safety net.

  > **Where the expiry lives, and where it does not.** This rule read "`budget_state` carries
  > `reserved_until`", and there were two columns and four store methods to match —
  > `ReserveBudget`, `SettleBudget`, `ReleaseReservation`, `SweepExpiredReservations` — with a
  > leader job sweeping them every tick. None of it ever ran. `ReserveBudget` had no caller
  > outside its own tests, so `reserved_nano` was zero in every row of every deployment, and
  > the sweep's `WHERE reserved_nano > 0` was a store round trip per tick against a predicate
  > that could not match.
  >
  > The hold that exists is §9.6's **lease block**: a node draws a block, `spent_nano` is
  > charged for the whole of it before a unit is handed out, and the units are held in memory
  > behind an atomic. The expiry is the block lease's own `expires_at` in `quota_leases`, and
  > the leader's lease-reclaim pass returns `amount - used` for every lease whose TTL has
  > passed. That net is strictly wider than the one it replaces: it also covers a node that
  > **dies holding a block already charged**, which is the state a killed process actually
  > leaves and which no reservation sweep could have reclaimed.
  >
  > The two could not both be kept, and not merely on tidiness grounds: charging a block to
  > `spent_nano` **and** reserving against `reserved_nano` counts the same money twice, which
  > is why `Ledger.readCounter` summed both columns. The reservation code, its columns
  > (migration `0006`), its sweep job and this paragraph's earlier text are gone.
- **Unexecuted requests are refunded in full [R1-20].** A request may reserve budget at the
  gate and then be rejected while waiting for capacity, never reaching an upstream. Revision
  1 defined settlement only for completion. Revision 2 makes budget a **soft hold** at the
  gate that becomes a **hard hold only after capacity is acquired**; anything that fails
  before dispatch releases the full amount.

> ⚠️ **R5 was specified, designed, and not enforced.** Assembling the gateway revealed that
> `quota.Budget` had **no call site anywhere outside its own tests**. The request path
> performed exactly one spend check — against a stored column that nothing on that path
> incremented — so a budget could never be exceeded because it was never consulted. The
> mechanism existed, was tested, and was wired to nothing.
>
> This is the failure mode a package-level test suite cannot see: every part worked, and the
> feature did not. It is also why the assembly step earns its own milestone rather than being
> treated as glue. The gate now reserves after routing (an estimate needs a deployment to
> price against), settles on the real cost, releases in full on any failure before an answer,
> and refuses a spent budget as a terminal `400`.
>
> One deviation from the text above: §6.4's soft-hold-then-harden collapses into a single hold
> taken *after* routing, because the estimate prices output at `max_tokens` and there is no
> price at all before a deployment is chosen. The soft/hard split protected a window that does
> not exist.

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

Four corrections from implementing it, each of which the formula above quietly required:

1. **A rolling window has no reset instant, and that is the *common* case here.** A 5-hour
   allowance is a rolling ring by construction; inventing an epoch-aligned boundary would
   spike urgency at a moment unrelated to the real reset. So a rolling window scores **zero
   until the provider reports a reset time** (§6.2), a calendar window uses dorang's own UTC
   boundary, and a provider-reported reset always wins. The feature depends on the provider
   probe rather than merely benefiting from it.
2. **Jitter must be keyed on (node, subject), not node alone.** A per-node factor scales every
   candidate that node ranks by the same amount, so it cannot change that node's ordering —
   it is arithmetic with no effect on the stampede it was added to prevent.
3. **The ratio diverges at the window edge**, and an infinity compares equal to itself across
   every candidate, destroying the ordering the signal exists to provide. The denominator is
   floored, giving a finite maximum.
4. **With several rules that do not expire together, urgency is the maximum** — what is most
   at stake. The minimum reading describes what may still be spent, which is admission's
   question, not routing's.

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
| `fixed_subscription` | plan cost independent of this request | most specific wins; accrues over the period |
| `adjustment` | discounts, margins, taxes | all applicable rules apply in order |
| `notional_rate` | **what this traffic would have cost at list price** — never billed | most specific wins; §8.5 |

```
cost = marginal(winner) + amortized(subscription winner) then adjustments applied in order
```

**Amortization is stated as a property, not a formula**: *the shares a period attributes sum
to the plan cost, and never to more.* Over a period that carried traffic throughout, a 100 USD
plan attributes 100 USD — at one request, at a hundred, at a million — and less, never more,
if the traffic stopped early. It is computed for
**accounting** only. Routing uses `marginal_usage` alone, because a sunk subscription cost
must not make a saturated plan look cheap. The two are separate fields, never conflated.

> ⚠️ **Revision 2 stated a formula here, and it summed to `plan_cost × H_N`.**
> The rule was `period_cost × (request_marginal ÷ period_marginal_to_date)`, computed per
> request and added up by the ledger. For N equal requests that is `plan_cost × (1 + ½ + ⅓ +
> … + 1/N)`: **a 100 USD plan attributed about 519 USD over 100 requests, 749 USD over 1,000,
> and it never stopped growing.** It is recorded here rather than quietly replaced, because
> the number reached the ledger, and every figure derived from it — per-key attribution,
> per-team chargeback, the notional-versus-actual comparison §8.5 exists for — was wrong by a
> factor that depended on traffic volume.
>
> The cause is structural, not arithmetic. Each share was an estimate of *the same quantity*
> — the plan cost per unit of traffic so far — and estimates of one quantity must supersede
> one another, not accumulate. Only increments may be added.

So the period's attributed total is **one running total that is revised, not a sum that
grows**, and a request's recorded share is the increment that brings it up to date:

- **The plan accrues with its period.** A fixed plan buys a *period*, so the fraction of the
  period that has passed is the fraction of the plan cost that has been incurred. A request
  records what has accrued since the previous settlement. Requests spread evenly across a
  period therefore take `plan_cost ÷ N` each — the per-request figure the old formula only
  ever produced for the first request.
- **Usage-to-date cannot be the denominator.** A period's total usage is unknowable until the
  period closes; every mid-period estimate of it is too small, and dividing by a denominator
  that is too small is exactly what over-attributed every request. The period is the one
  denominator known in advance, which is what makes the total bounded. The exact
  usage-weighted apportionment is still available where it is sound — as a rollup over the
  ledger's marginal column *after* the period closes — but a per-request column settled in
  real time cannot wait for it, and §8.5's notional figure already answers "who consumed the
  value" per key and per team.
- **A settled row never changes.** Restating history to fix an estimate would be worse than
  the defect it fixes: the number an operator has already seen, exported or invoiced stays
  what they saw. Shares are increments precisely so that no row has to be rewritten.
- **The cap is structural.** The elapsed fraction is clamped to the period inside the single
  function `Price`, `Settle` and `Explain` all reach the figure through — the same shape as
  the floor under `TotalNano`, which caught a third call site nobody had named. There is no
  call site left that can attribute a plan twice.
- **A reload is not a billing event.** The accumulator lives with the loaded catalog and a
  reload builds a fresh one, so an open period used to start its attribution over from the
  reload instant and attribute the rest of that period again. This was recorded as a residual
  "bounded by one plan cost per reload"; measured, it was **1,030 USD of a 100 USD plan over
  twenty reloads**, with one reload nine tenths of the way through a period putting **91.00 USD
  on a single request** — which the budget hold then reserves against *that request's* budget.
  Nor was the bound one-per-deliberate-change: `SIGHUP` re-applies unconditionally, so a
  config-management agent that HUPs on a timer billed on a timer.

  The new catalog now **adopts the previous one's state** — the period's attributed total and
  the sub-nano rounding carries — across the swap. It is closed there rather than by
  suppressing redundant reloads, and the distinction is load-bearing: a `SIGHUP` is how an
  operator picks up an edited external price catalog, an edited model-catalog overlay or a
  rotated `key_file` secret, none of which the configuration file's own mtime says anything
  about. *"Nothing in the file changed"* is not *"nothing changed"*, so the reload stays
  unconditional and is made free instead.
- **A restart is not a billing event either, and this was the larger half.** The line above
  closed the reload and left the process boundary open: the accumulator was in memory and
  nowhere else, so a new process built an empty one and the open period attributed its whole
  elapsed share again. Measured on a live reproduction of a 100 USD monthly plan driven late in
  the period: **180.46 USD attributed across two process starts**, the second start's 90.23 USD
  landing entirely on the first request after the restart — and because the budget hold reserves
  the settled cost against the *requesting* key, **a brand-new key with `max_budget: 1.00` was
  refused `400 budget_exceeded` on its first ever request**. N restarts in a period attribute N
  times, and so did N nodes, each with an accumulator of its own.

  The accumulator is now written down, in `subscription_state`, one row per rule id. Three
  properties make it cheap and make it safe:

  - **It is checkpointed, not written per request.** A store write per settlement is the
    arrangement §9.6 exists to avoid. The checkpoint runs on the same background loop as the
    budget-lease maintenance and once more on a clean shutdown.
  - **The checkpoint is projected forward by one interval**, so the durable figure is always at
    or *ahead* of what has really been attributed — the same rule §9.6 applies to a budget
    block, for the same reason. A crash therefore leaves a little of the period unattributed
    rather than resuming behind and attributing a slice of it twice: the direction this section
    permits, never the one it forbids. At the default interval that residue is 0.0012% of a
    monthly plan.
  - **The merge is monotone.** A later period replaces the row, the same period keeps the larger
    attributed total, an earlier one is dropped. Two nodes converge on the high-water mark
    instead of overwriting each other, which bounds the *fleet* rather than each node
    separately.

  The row is also the second observation the clamp below needs. See §17.1.
- **A settlement stamped in the future is clamped to the present.** The accumulator only moves
  forward, so a row stamped ahead of now attributes everything the plan will have accrued by
  that instant and leaves the real remainder of the period attributing nothing; a stamp that
  lands in the *next* period moves the period marker forward as well, so every later row of
  the real period takes the late-row branch below and the next period opens already depressed.
  Measured: one such row left **10.00 USD attributed to a 100.00 USD July**. It is clamped
  rather than refused, because a refusal fails the whole pricing call and takes the request's
  real marginal cost with it; the plan share is the only figure a bad clock can distort, so it
  is the only one adjusted. Only settlement is clamped: `Price` and `Explain` mutate nothing,
  and pricing a future instant is what a preview is for.

  > **This clamp said "one node in a cluster with a clock that runs ahead is enough", and inside
  > `Settle` that was not true.** internal/app hands the catalog `a.now` and stamps the request
  > `At: d.now()` from the same `a.now`, read first: two readings of one clock, in order, so the
  > stamp is never the later of the two and the comparison cannot be true in a default
  > deployment. The triggers named — an NTP step, a VM resume, a bad RTC — move *both* readings
  > together, which is exactly why comparing them finds nothing. A control that is reached and
  > whose condition can never hold is the §17.1 class in a form worth naming separately.
  >
  > **A clock check needs two observations, not two readings.** The second observation is the
  > durable accumulator above: its period stamp was written by whichever process held it last,
  > which may be another process on another host, and this process's clock did not produce it.
  > The comparison therefore lives at the *process boundary* as well — a stored period that has
  > not begun is re-dated to the open one, keeping its attributed total, so the open period
  > attributes only the remainder and the next one still opens at zero. Re-dating rather than
  > dropping is what keeps the fleet's shares for one period summing to one plan cost. Inside
  > `Settle` the clamp stays, and its scope is now stated honestly: it covers a caller whose
  > `At` did not come from this catalog's clock — a replayed or imported row, `dorangctl price
  > --at` — and not the gateway's own request path, where it is a tautology.
- **A share is never negative**, and a row that arrives out of order inside the open period
  attributes zero rather than clawing back what a settled row already took.
- **A period with no requests attributes nothing**, and a period whose traffic stopped early
  attributes only what had accrued while it ran. The plan still cost the operator its plan
  cost, and the honest place for that is the *gap* between the plan cost and what the period
  attributed — an idle month reads as an unattributed plan, which is the number that says the
  plan is not worth renewing. Attributing it to no request, or to the next period's first
  request, would both invent traffic that never happened.
- **A late row from a closed period attributes zero plan cost** (its marginal cost is priced
  as usual). That period's plan cost was apportioned among the rows settled while it was open
  and the accumulator that would bound a further share is gone; any positive share would be an
  unbounded guess that can push a closed period above its plan cost. This is the same
  reasoning as the backfill guard: a late row must not resurrect an over-attributing period.

The property, not the formula, is what the tests assert: for N from 1 to 10,000, a period's
settled shares sum to exactly the plan cost, with N = 100 and N = 1,000 named so the harmonic
growth cannot return unnoticed.

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
per request**. §8.1's plan cost accrues with the *period*, so the denominator is apportioned by
time rather than by usage. Over a full period the ratio is exactly right; within a period a
quiet hour reads as poor leverage and a busy one as excellent, when neither is a fact about the
plan. Read it per period, or against the notional total, not per request; `notional` per key or
team is who is consuming the value a flat bill hides — it is the usage-weighted half of the
pair, and the one to read when the question is *which* team, since the subscription column
answers only *how much of the plan has been paid for so far*; and `notional` over the period is
the number to compare against a vendor quote when the plan stops fitting. Extension headers expose it as
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
**compute seconds and audio seconds**. Cached counts are read from whichever usage field the
backend reports.

The last two used to be one component called `seconds`, priced against the request's wall
time. That is the right input for a GPU-second rate and the wrong one for a transcription
vendor, which bills the length of the recording — so the two are separate components on
separate units (`per_compute_second`, `per_audio_second`), the ambiguous spelling is a load
error, and a rule quoted on one axis against a request carrying only the other is an UNPRICED
request rather than a rate applied to the nearest available number. §10.7 has the table and
the measurement.

**The usage counts are inclusive and the rate table is exclusive, and something has to
reconcile them.** §10.7 normalizes `InputTokens` to the whole prompt with the cached and
cache-written prefix inside it, and `OutputTokens` to the whole completion with the reasoning
tokens inside it. No vendor's rate card is quoted that way: `input` is the price of the
prompt tokens the provider did *not* serve from cache, and reasoning is folded into `output`
unless the vendor prices it apart. So **a declared rate for a part carves that part out of
its parent's quantity** — `input` is charged on `input − cache_read − cache_write` and
`output` on `output − reasoning`, each subtraction applying only where the rule declares the
sub-rate — and every token is charged exactly once. An undeclared sub-rate leaves its tokens
with the parent, which is what a vendor with no cache discount charges. CONFIG §13.1a states
the rule where a catalog author reads it.

> ⚠️ **The arithmetic did the opposite of this, under §10.7's own warning.** `input` was
> charged against the whole inclusive prompt *and* `cache_read` against the cached part of it
> again. On §8.5's example card a 120-token prompt with a 40-token cached prefix and a
> 15-token completion billed **$0.0001606 against the vendor's $0.0001266 — 27% over**, and
> **5.7×** on a 90%-cached agent turn. §10.7 says in words that "a mis-mapped cache field does
> not produce a visible error, it produces a wrong invoice"; it was on the page while the
> invoice was wrong, which is the second time a prose invariant about arithmetic has been
> worth less than the arithmetic.
>
> It survived two clean parity runs and a clean cutover because **neither harness declared a
> price** — every run compared zero against zero. A verification configuration that exercises
> the accounting path with pricing switched off cannot see a pricing defect, and the harness
> configurations now price. The regression test asserts the charged amount against a
> hand-computed vendor figure, not against another function in the pricing package: both of
> those agreed throughout.

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
  (each carries notional_nano as its own column, never folded into cost_nano — §8.5,
   and marginal_nano/subscription_nano beside cost_nano — §8.1's two classes stay two
   numbers in the aggregate as well as in the row)

quota_buckets(scope, scope_key, window, metric, bucket_start, value)
quota_leases(node_id, scope, scope_key, window, metric, amount, expires_at)   [R1-14]
budget_state(subject_kind, subject_id, period, period_start, spent)
  -- spend only. The hold lives in quota_leases, not here; see §6.4 and §9.6
subscription_state(rule_id, period_start, attributed_atto, updated_at)        -- §8.1
credential_state(credential_id, health, unavailable_until, quota_snapshot, updated_at)

responses_store(response_id, created_at, expires_at, owner_key_id,
                previous_response_id, model_group, items, reasoning_blobs)   [R1-C7]

nodes · capacity_leases · files · batches · batch_requests · audit_logs

  batches         also: errors (why a failed batch failed, after a restart), expired_at
  batch_requests  also: response_body, input_offset, input_length
```

**`subscription_state` is one row per `fixed_subscription` rule, not one per node.** §8.1's
invariant is about a *period*, and a per-node row would make it an invariant about a node —
which is what it accidentally was while the accumulator lived in memory. It is also not keyed by
node because a node id is generated per process when `cluster.node_id` is unset, so a per-node
row would be written by a process that never reads it again, which is the restart the table
exists for. `attributed_atto` is decimal **digits**: the value is atto-scaled, a 100 USD plan is
10^20, and the alternatives were a pair of 64-bit limbs (exact, illegible) or a float (legible,
not exact).

**`responses_store` is not optional [R1-C7].** The Responses API carries server-side
conversation state — `store: true` plus `previous_response_id`. Revision 1 promised strict
compatibility for that endpoint while having nowhere to keep the state, leaving only two
runtime options, both of which break the promise: reject the request, or ignore the
reference and answer from a truncated conversation. `reasoning_blobs` holds the opaque
integrity-bearing reasoning handles of §10.2, keyed by response id, so they can be replayed
byte-identically. Entries expire; retention is configurable per tier.

**Three batch columns exist for reasons worth stating.** `batches.errors` is where a failed
batch records *why*, which is otherwise unavailable after a restart. `batch_requests.response_body`
is required for resume: the ledger stores no bodies, so without it a resumed batch has nothing
to rebuild its output file from and **every already-finished row would be paid for a second
time**. And `input_offset`/`input_length` let the scheduler seek into the input file rather
than hold a 200 MiB upload in memory for the life of the batch.

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

> **This gap is closed, and it is the only budget mechanism.** It read: *"Quota and budget
> state are currently in-memory only. A restart therefore resets the windows … Persisting
> them through the lease mechanism above is required before the clustering milestone."* The
> lease mechanism is `cluster.Ledger` and the gate reserves against it before every upstream
> call; §17's W9 row carries the measurements.
>
> What is worth saying here, because this is the section that classifies writes: there is no
> second budget write. `budget_state` used to carry `reserved_nano` and `reserved_until` with
> a synchronous `ReserveBudget` over them — a write to the store per request per subject,
> which is precisely the arrangement this section exists to avoid — and it had no caller
> anywhere outside its own tests. It is deleted, columns included (migration `0006`). The row
> is a spend counter; the hold is the block lease.
>
> A consequence for anything reading `budget_state.spent_nano` directly: it runs up to one
> block **ahead** of what has been spent, because the block is charged before it is handed
> out. That is what makes a crash under-spend rather than overspend, and it is why the
> per-key `spend` field reads the rollups instead.

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
`top_k`, the penalties, an unsupported reasoning control). The request still means what it
meant. Listed in `x-dorang-dropped-params`. This is the revision 1 behavior and it is correct
here.

**Structural downgrades** — the request cannot be expressed at all:

| Crossing | What is lost |
|---|---|
| prompt-cache breakpoints → a protocol without them | caching topology, and therefore cost, silently |
| multi-block tool results (text + image) → a single-string tool message | the non-text blocks |
| document/PDF content blocks → a protocol without them | the document entirely |
| richer stop reasons → a smaller enumeration | which specific terminal condition occurred |
| structured system blocks with per-block attributes → one system message | per-block attributes |

> ⚠️ **"Can the target represent it" was the wrong axis, and four parameters were filed on
> the wrong side of it.** Read as a statement about shape, that test put `stop`, `n`,
> `logprobs` and `service_tier` among the droppable knobs — so a dropped stop sequence let
> generation run past the terminator the caller stated and billed them for the extra tokens;
> `n: 4` returned one choice, and `choices[3]` became an index error in the client rather
> than an error from the gateway; `logprobs: true` returned a body without the member it
> asked for; and `service_tier: flex` ran in a band the caller did not select and charged
> them for it. Each answered `200` with the parameter listed in `x-dorang-dropped-params`,
> which is not consent — it is a header nobody parses, carrying news that the answer is not
> the one that was requested.
>
> The shape reading was never what the table above contained either. **Prompt-cache
> breakpoints are here because losing them changes the BILL**, and richer stop reasons
> because collapsing an enumeration loses *which* condition occurred. Neither is about shape.
> So the axis is restated as the question that was always deciding it:
>
> **Does the absence change what the caller RECEIVES or is CHARGED, or only which knobs
> dorang applied on the way?** A knob is droppable. Everything else needs the caller's word
> for it.
>
> That does **not** make every parameter structural, and the discipline is the point. A
> sampling prior — `logit_bias`, `top_k`, `frequency_penalty` — states no postcondition: no
> vendor promises a distribution, the same request returns different text on every call, and
> the answer under a dropped prior is indistinguishable from an ordinary re-draw of the
> request that carried it. Refusing on "the distribution is not the one requested" would
> commit dorang to refusing `top_k` on every crossing into the OpenAI family, which is an
> everyday, harmless conversion, and a caller trained to set `x-dorang-allow-lossy` on
> everything has been given a mechanism that reports nothing. The **cost of refusing more**
> is paid down where the parameter has a value that means "do the default": `n: 1` and
> `service_tier: auto` raise no capability at all, because dropping them changes nothing.
>
> The refusal set therefore has two halves, which differ in how a loss is REPORTED and not in
> whether it is refused. The **located** half is the table above — a construct with an index
> to point at (`messages[2].content[1]: application/pdf`). The **material** half is `stop`,
> `n`, `logprobs` and `service_tier` — a parameter, reported by name with the value the caller
> wrote (`service_tier: flex`), which is also what lets the `400` body fill `param` at all.
> COMPATIBILITY §7.9 is the table; `canonical.Material` is the mask.

Returning `200` after silently discarding a PDF, destroying a caching strategy or ignoring a
stop sequence is **worse than an error**: the caller has no way to know. Revision 2
therefore:

- **Fails fast with `400`** and a machine-readable body naming the unsupported construct,
  when the request actually contains one of these and the chosen backend cannot express it.
- Lets a caller opt into lossy conversion explicitly with
  `x-dorang-allow-lossy: <construct>[,…]`, in which case the loss is applied and reported in
  `x-dorang-downgraded`.
- Prefers, during routing, a backend that **can** express the request: capability becomes a
  routing filter (§7.1), so a request using constructs only one protocol family supports is
  routed there when such a deployment exists, and only fails when none does.

The first and the third are separate obligations and both are implemented. Routing answers
"does any deployment of this model express it"; the backend answers "does the one that was
chosen", against the capability set that deployment declared and after the engine
normalizations of §4.4 have run — which is the only point at which the request that will
actually be encoded exists. A gateway that gated only at routing would refuse correctly for
as long as the two capability sets agree, and silently downgrade the day they do not.

The full construct list is a versioned table in the compatibility document (§7.9), and every
entry has a conversion test in both directions.

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
| `x-dorang-spend-usd`, `-budget-usd`, `-budget-remaining-usd` | cumulative. The ceiling comes from the authorization snapshot and is always known; the SPEND comes from the budget hold, which is hydrated by the reservation, so a request that reserves nothing (a zero-rated one) omits `-spend-usd` and `-budget-remaining-usd` rather than reporting a `0` nobody looked up. Same rule as the streamed cost header: absent claims nothing, zero claims a measurement |
| `x-dorang-quota-*-used-pct` | credential quota windows |
| `x-dorang-dropped-params` | what conversion removed |
| `x-ratelimit-limit/remaining/reset-{requests,tokens}` | standard form |
| `retry-after` | on 429 |

**Header set is bounded — but standard HTTP headers are not telemetry and are never gated.**
`Retry-After` and the `x-ratelimit-*` set are always attached. An earlier draft listed them
among the extension headers, which taken literally means a `429` carries no `Retry-After`
unless the caller asked for detail — and every SDK's backoff silently stops working. The rule
is: a header the client **acts on** is unconditional; a header the client **reads** may be
gated.

Of dorang's own headers, only the identification and cost set is always attached:
`x-dorang-request-id`, `-model`, `-upstream-model`, `-deployment`, `-cost-usd`, and
`-replayable` when false — the last because it changes retry semantics a caller may be relying
on, which makes it an act-on header by the same rule.

**No cost header can carry a streamed request's cost, and none pretends to.** The headers are
written before the first frame and the request is settled after the last one, so on a stream
`x-dorang-cost-usd` is absent — as it is for any unpriced request, because dorang's own
vocabulary distinguishes "no price rule matched" from "this request was free" — and the
legacy mirror `x-litellm-response-cost`, which is otherwise emitted unconditionally so that a
missing price reads as `0` rather than as a missing header, is **omitted in this one case**
rather than made to publish a zero nobody measured. COMPATIBILITY §7.7a carries the four-state
table a reader discriminates on. The streamed cost travels on the terminal frame below, where
the number exists, and in the ledger.
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
opts in with `x-dorang-usage-events: 1`. Without it the numbers remain available from the
ledger by `x-dorang-request-id`, which is a response header and always present.

#### The terminal frame is the right place, and it can carry everything

Headers are gone before the first token; **TTFT and generation rate are not known until the
stream ends.** So the only channel that can carry them is a frame emitted just before the
terminator — `data: [DONE]` for the chat family, `message_stop` for messages.

§10.5 removes the objection that used to make this awkward. Injecting a frame is not a
violation of byte-fidelity, because the pass already rewrites `model` on every chunk and
synthesizes a terminal chunk when the backend omits one. This is one more rule in the same
single pass, and it costs one frame.

So the opt-in event carries the whole per-request record rather than usage alone:

```
event: dorang.usage
data: {"request_id":"…","model":"…","upstream_model":"…","deployment":"…",
       "attempt":1,"tokens":{"input":…,"output":…,"cache_read":…,"reasoning":…},
       "ttft_ms":…,"latency_ms":…,"tokens_per_second":…,
       "cost_usd":"…","notional_usd":"…","route_reason":"prefix_hit:depth=3"}
data: [DONE]
```

Three constraints keep it safe to add:

1. **It is opt-in and emitted before the terminator.** A client that did not ask sees an
   unchanged stream; a client that reads to `[DONE]` and stops still gets it, because it
   arrives first. A frame *after* the terminator would be invisible — which is exactly why
   trailers were removed.
2. **A named event, not a `data:`-only frame.** Clients that dispatch on event name ignore an
   unknown one; a bare `data:` frame would be handed to a JSON decoder expecting a chunk.
3. **It never replaces the ledger.** A client can drop the connection before the terminator,
   so the ledger is the record and the frame is a convenience — which also means the frame
   failing to send is not a metering failure.

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

### 10.5 Stream reconstruction is the normal case

Several sections of this design have treated rewriting a stream as a reluctant exception —
zero-copy "withdrawn", a promise that "loses", masking described as hard. That framing was
wrong, and it was distorting decisions.

**dorang is a transforming proxy. Reconstructing the stream is part of its job**, and it
already has to for reasons that have nothing to do with any optional feature:

| Rewrite | Why it is unavoidable |
|---|---|
| `model` in every chunk | §7.2 — the caller asked for an alias and must get it back |
| tool names | COMPATIBILITY 5.3 — names over 64 characters are shortened, and the model's call must be matched back to the real name |
| tool-call ids | cross-protocol ids are normalized in both directions (5.4) |
| `finish_reason` | 4.1–4.4 — a native value is mapped, and a terminal chunk is *synthesized* when the backend never sent one |
| block boundaries | §10.7 — one family has implicit boundaries and the other explicit, so crossing means synthesizing them |
| usage fields | §10.7 — cache counts normalize to inclusive, or every cached request bills wrong |

A gateway that only forwarded bytes could do none of that, and would be a worse gateway. So
the constraint was never "do not rewrite". It is:

1. **One pass, bounded memory.** Rewrites compose into a single scanner over the stream, not a
   pipeline of decoders. Nothing buffers the body.
2. **Bounded lookahead.** A rewrite that needs to see across a frame boundary holds a bounded
   tail — and that bound is stated, because it is what makes a token longer than it
   unrewritable.
3. **Frames stay valid at every boundary.** A client parsing incrementally must never see a
   partial frame or a broken escape. This is what makes rewriting *safe*, and it is the real
   discipline the "byte-faithful" language was standing in for.
4. **No added latency.** A rewrite may not hold a frame waiting for one that has not arrived,
   except where the protocol itself already requires it — §10.7's held terminal event is that
   case, and it is protocol-mandated rather than a rewriting artifact.

Everything that transforms a stream is therefore **one rewriter with composed rules**, not a
set of special cases each apologizing for itself: alias substitution, tool-name restoration,
reasoning-field normalization, credential blacklisting (§10.5c), and PII unmasking (§10.5b)
all run in the same pass over the same bytes.

> **This unlocks something previously deferred.** Normalizing `reasoning` against
> `reasoning_content` across the two self-hosted engines was left undone on the grounds that
> "normalizing means decoding every frame" — but the pass already exists and already touches
> those frames. The objection was to a cost that had already been paid. Same for SGLang's
> top-level reasoning token count.

The remaining honest limit is different and narrower: **dorang rewrites fields it understands
and relays everything else untouched.** An unmodelled block type passes through opaquely
(§10.7), because rewriting what you cannot parse is how a proxy corrupts data — and that, not
byte-fidelity, is the line worth holding.

### 10.5b Transform filters — reversible masking, and why it is hard

A filter sits between the canonical request and the backend, may rewrite the request, and may
rewrite the response on the way back. The motivating case is **PII masking**: replace a
national identity number with a placeholder before the text leaves the operator's control,
and restore it in the answer, so the upstream never sees it and the caller never notices.

```yaml
filters:
  secret: { key_env: DORANG_FILTER_SECRET }   # cluster-wide; see "Determinism" below
  plugins:
    - { name: pii-mask, path: /etc/dorang/plugins/pii_mask.lua, fail: closed }
models:
  - name: model-x
    filters:
      - plugin: pii-mask
        on: [request, response]
        scope: conversation
        patterns: [krrn, email]
```

Filters are §11.5 plugins, which is the point: an operator's disclosure rules are theirs, and
a gateway cannot ship the right pattern set for every jurisdiction. The *pattern set* is
configuration; the *plugin* decides which segments of a request to apply it to; the *host*
owns the mask table and every unmasking pass.

#### Determinism is the hard part, not secrecy — and the first draft got it wrong

The original text of this section required a **per-request random component** in every
placeholder. That is wrong, and wrong in a way that silently deletes another section.

Prefix caching (§7.4b) requires the bytes sent upstream to be byte-identical, and in order,
across the turns of a conversation. With a random component the same conversation masks
differently on turn N and turn N+1, so the upstream prefix differs, so **every masked
conversation becomes a permanent cache miss**. On a self-hosted backend that is the
difference between reusing a 40k-token prefix and recomputing it every turn.

So a placeholder is **derived, not allocated**:

```
placeholder = "[PII:" ‖ base16a(HMAC-SHA256(secret, scope ‖ salt ‖ 0x00 ‖ value)[:8]) ‖ "]"
```

Two consequences, both good:

- **Nothing has to be persisted.** The reverse table is rebuilt from the request's own text on
  every request, because the chat and messages protocols resend the whole conversation: every
  plaintext that can appear in the answer was present in the question. The mapping therefore
  survives five minutes, an hour, or a year, with zero storage.
- **Nothing has to be replicated but one constant.** The secret is operator-configured and
  shared cluster-wide. A retained *table* would have to be — unbounded, full of plaintext, and
  different on every request. A retained *secret* is one small value, set once and rotated
  deliberately. Rotation invalidates every prefix cache on the fleet, so it is a staged,
  announced operation.

**The cost has to be stated rather than implied: a deterministic placeholder is a stable
pseudonym, so the provider can link "this same person appears in these requests."** That is
not a defect to be engineered away. It is the same property as the cache hit — a backend that
cannot recognise the repeated bytes cannot reuse the cache — and any design claiming both is
wrong about one of them.

What can be bounded is how far the pseudonym reaches, which is what `scope` is for:

| `scope` | Placeholders are stable across | Prefix caching | Linkability |
|---|---|---|---|
| `conversation` *(default)* | the turns of one conversation | works | nothing the provider did not already see, since it sees those turns together |
| `principal` | everything one key sends | works, across conversations | the provider can link a key's traffic |
| `tenant` | one team | works, across a team | the provider can link a team's traffic |
| `request` | nothing | **disabled for that route** | none |

`request` is the honest end of the dial: it turns prefix affinity **off** for the request
rather than leaving a claim about bytes that can never repeat. A knob that quietly makes
another subsystem useless is worse than one that says so.

The conversation salt is the caller's session header when they sent one, and otherwise a
digest of the conversation's opening segment — stable across turns for exactly the reason the
hash chain is, because the protocols resend the history. A client that trims its history gets
new placeholders, which costs a cache miss and never a wrong answer.

#### The reversible-mask contract

1. **The table is never persisted.** Not in the ledger, not in the trace excerpt, not in a
   log, not in an audit row, not in a metric label. A redaction whose key material is written
   next to the redacted text has redacted nothing. This is the rule that survives whole, and
   it is about *durability*, not lifetime.
2. **Placeholders are unforgeable and unambiguous.** A caller who sends text that looks like a
   placeholder must not be able to make the unmasker substitute something, so a
   placeholder-shaped string in the *input* is escaped before masking runs — by being masked
   itself, which makes the round trip exact and leaves no escape syntax to study. A
   placeholder is fixed-width, and its alphabet is letters only, so it can never match a
   digit-shaped or address-shaped pattern on a later pass.
3. **Unmasking is not a blind string replace.** A model may echo a placeholder inside a longer
   token, split it across a streaming frame boundary, translate it, replay one from another
   scope, or invent one that was never issued. Only exact, whole placeholders that resolve in
   *this* scope are substituted; anything else is left alone and **counted**, because an
   invented placeholder is a signal worth seeing.
4. **Streaming makes this genuinely harder.** A placeholder can straddle two SSE frames, so
   the unmasker holds a bounded tail — one byte less than a placeholder's width. That bound is
   a real limit: a placeholder longer than it could not be reassembled, which is why they are
   short and fixed-width. It composes into §10.5's single pass; it is not a second one.
5. **Masking changes token counts and therefore cost.** Metering records what the *upstream*
   was actually sent, since that is what was billed — not the pre-mask text. The pre-request
   estimate that drives the budget hold and the routing decision is taken after the filter for
   the same reason.
6. **One mask per model, not one per filter entry.** Several filters on one model share one
   table. Two tables would mean two placeholder namespaces in one body and an unmasker that
   had to guess which one a placeholder came from — or, worse, one that counted the other
   table's placeholders as invented.

#### Retention: the one direction derivation cannot serve

Re-derivation makes the *forward* direction reproducible. It cannot make the *reverse*
direction work when the plaintext is not in the request, because an HMAC does not invert.
That happens when the conversation state lives on the **provider's** side — the Responses API
with `previous_response_id`, or any provider-held session — and generally whenever the model
quotes a placeholder whose source turn was not resent.

So a retained reverse table exists too. They are not alternatives; they solve opposite
directions. Retention is cheap — a conversation holds a handful of values, and one in-flight
40k-token request body costs more memory than a dozen retained tables — so it is held
generously rather than grudgingly: default lifetime an hour, `retain` configurable per model
filter to at least the provider's own state lifetime.

What is not negotiable about it: memory only, never serialized, bounded by entry count, with a
stated eviction policy (expired first, then oldest), and **keyed by scope** — a placeholder
resolves only inside the scope that issued it, or a caller who has seen one from another
conversation could ask a model to emit it and read someone else's text back. A rising eviction
count beside a rising unresolved count is the signal that the bound is too small for the
traffic, which is why both are counted.

#### What a filter must not be allowed to do

A filter runs inside the process holding provider credentials, so §11.5's plugin rules apply
unchanged: no credential is reachable, ceilings are enforced by the host, and a breach is
fail-open — **except** that a *failed mask* must fail **closed**. Those two rules point in
opposite directions, deliberately: a filter that cannot enrich a request should be skipped,
but a filter that was supposed to remove an identity number and did not must stop the
request. The plugin declares which kind it is; `fail: closed` is the safe declaration and the
default.

**A filter that did not run is a filter that failed**, and the original text of this rule said
only "failed", which turned out to be a different sentence. The decision a filter returns is a
value, and its zero is *no masking and no refusal* — so every way of not running returned what
a clean pass returns and the text went upstream exactly as the caller sent it. Three of them
existed, and one was reachable from traffic alone:

1. **The hook had been tripped off.** §11.5's trip is designed for hooks whose absence is
   safe. Pointed at the one hook whose absence is not, it switched PII masking off for the
   life of the process — and a burst of 48 large requests was enough to arrange it, in under a
   second, because a priced match that ignored its context outlived the wall clock, the
   resulting backlog refusals cost microseconds, and 32 of them in a row is the trip.
   Measured end to end: the identity number reaching the upstream unmasked and unrefused,
   permanently, with the burst's own requests refused and every request after them served.
2. **The plugin registered no `on_filter_request`.** Configuration checks that
   `models[].filters[].plugin` names a declared plugin; nothing checked that the plugin
   registers anything, and a misspelled hook name is enough. The chain then ran zero handlers
   and reported success.
3. **The engine had never heard of the plugin.** Not reachable through the configuration
   loader today — a declared plugin turns the engine on by itself — but the same zero decision
   was waiting behind it.

All three now take the same path as a plugin that ran and failed: the strictest `fail:` among
the plugins the model named, which for a filter defaults to closed, and a plugin this build
cannot see declared nothing and is therefore strictest of all. §11.5's trip no longer applies
to this hook at all, for the reason given there: switching a fail-closed hook off does not stop
it refusing, it only makes the refusal permanent, and the goroutine bound it was standing in
for is bounded directly.

Three limits that follow from being honest about the rest of the system, and are refusals
rather than silences:

- **A thinking block's text is not masked.** It travels with integrity material the provider
  signed and dorang replays byte-identically or not at all (§10.2). Rewriting text under a
  signature it no longer matches is a corruption dorang would have caused.
- **A tool call's arguments are not masked.** The neutral form keeps them as raw JSON
  precisely because re-encoding a decoded map reorders keys, and a model that emitted an
  ordering on turn one must see the same bytes on turn two. A filter that re-encoded them
  would break the determinism this section just bought. PII does appear in tool arguments;
  this is a stated gap, not a solved problem.
- **A surface whose text the filter cannot reach is refused**, not sent unfiltered. A model
  carrying a masking filter, used for embeddings, answers an error.

Filters resolve from the **client-facing model**, never from the chosen deployment: a
fail-back hop to another deployment must not mask differently, or the bytes change and both
the prefix claim and the unmasking go with them.

> **Do not confuse this with §10.5a's compaction non-goal.** dorang refuses to rewrite a
> caller's conversation *on its own initiative*. A filter is the operator's explicit,
> configured instruction to do exactly that, on their own traffic, and it is reversible. The
> distinction is consent, and it is why one is a non-goal and the other is a feature.

### 11.6 Tiers, and the token guard

#### Tiers

A key belongs to a **tier**, and the tier — not the caller — decides what it may claim.

| Tier | Priority | Typical limits |
|---|---|---|
| `unlimited` | highest | no budget ceiling; may be granted a priority range |
| `commercial` | middle | budget, rate limits, full model set |
| `free` | low | tight budget, small model set, low concurrency |
| *(batch work)* | **below `free`** | any tier's batch traffic yields to any tier's interactive |

Batch sitting below the free tier is deliberate. Batch has no waiting human, so latency costs
it nothing, and §11.1's interactive reserve already encodes the same judgement one level down.
A paying customer's batch job should not delay a free user's interactive request.

Tiers are **configuration, not code** — the names above are defaults, and an operator defines
their own set with their own ordering. What is fixed is that a tier is a property of the key,
assigned by an operator, and §10.5's rule holds: a caller cannot claim one.

#### The token guard

A key whose usage suddenly departs from its own history is either compromised, looping, or
newly popular, and the first two are expensive. The guard watches for a departure from the
key's **own** baseline rather than a fixed threshold, because a fixed threshold is wrong for
every key except the one it was set for.

```yaml
token_guard:
  enabled: true
  baseline_window: 7d
  trigger: { factor: 10, min_absolute: 100000 }   # both must hold
  action: pend                                    # pend | throttle | revoke | alert_only
  cooldown: 1h
```

Four things this must get right, because an automated revocation is itself a denial of
service:

1. **`pend`, not `revoke`, is the default.** A pended key is refused with a distinct,
   documented error and can be released by an operator in one action. Automatic *revocation*
   of a key that turns out to be legitimately busy is an outage the operator did not choose,
   and it is not reversible in the same sense — the caller has to be reissued a credential.
2. **Both a relative and an absolute condition must hold.** A key that used 10 tokens
   yesterday and 200 today has grown twentyfold and is not a problem. Without the absolute
   floor, the guard fires hardest on the quietest keys.
3. **A new key has no baseline**, and the guard must not treat that as anomalous or every key
   trips on its first busy hour. Below a stated minimum of history it only alerts.
4. **The action is always announced.** An event fires (§11.5) with the observed rate, the
   baseline, and which condition tripped — so the first thing the operator learns is not a
   support ticket from the affected user.

The guard is not a budget. A budget is a *stated* ceiling the caller agreed to; the guard is a
*statistical* judgement that might be wrong. They fail differently and are configured
separately, and the guard's action is deliberately the reversible one.

### 10.5c Credential blacklisting on the downstream path

An upstream can put dorang's own credential into anything it returns. It happens by accident
— an error message quoting the request, a debug page rendering headers, a validation error
echoing the field it rejected — and once it happens the operator's provider key is in a
response body headed for a caller who should never see it.

A first fix scrubbed **the credential applied to this request**. That is not enough, and the
reason generalizes: dorang holds *many* credentials, and nothing guarantees a backend can only
echo the one it was given. A shared gateway in front of it, a mis-routed retry, a provider
that logs across tenants, a copy-pasted config — any of these puts key B in a response served
on credential A. So the rule is:

> **Every credential the process holds is blacklisted from everything that leaves it.**

Not the one in use — all of them. And not only response bodies: the same set is scrubbed from
error envelopes, native error fields, trace excerpts, log lines, metric label values, audit
rows, and shadow diff records. A credential's only legitimate destination is the upstream
request it authenticates.

#### Where it costs something, and the trade

Scanning every response for a set of secrets is a multi-pattern search over the whole stream,
which is real work on a path §15 spends care keeping cheap. Two things make it affordable:

- **The credential set is small and changes rarely**, so the matcher is built once at
  configuration load, not per request. A multi-pattern automaton is O(stream length) with a
  small constant regardless of set size — the cost does not grow as an operator adds accounts.
- **It is a scan, not a parse.** It runs in the same single pass §15.2's relay already makes
  over the bytes, alongside the model rewrite.

Scrubbing rewrites the response, which is unremarkable — §10.5 establishes that rewriting is
the normal case, and this rule simply joins the others in the same single pass. It carries no
extra pass and no extra buffering.

#### Three things that make this correct rather than approximate

1. **Streaming means matching across frame boundaries.** A credential can straddle two SSE
   frames, so the scanner holds a bounded tail — the same mechanism §10.5b needs for
   placeholders, and the same bound applies: a secret longer than the tail cannot be
   reassembled, which is a stated limit rather than a silent one.
2. **A match is an incident, not a nuisance.** Redaction is the mitigation; the *finding* is
   that an upstream echoed a credential, and that is worth an alert (§11.5), a counter, and a
   named credential — because it means either that backend logs keys or something is
   misconfigured, and both outrank the individual response.
3. **The blacklist is derived, never configured.** It is the set of live credentials plus any
   secret still inside a rotation grace period (§11.2c) — a rotated-away key is exactly the one
   most likely to surface in a cached error page, and it is still a secret.

#### What this is not

It is not a general secret scanner, and it must not become one. Matching *shapes* — anything
resembling a key — would rewrite legitimate model output, and a caller asking a model to
explain an API key format would get mangled text with no explanation. dorang blacklists
**exact values it holds**, which it can be certain about, and leaves everything else alone.

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

**Security boundary** — four rules, and the last two were missing from an earlier draft:

1. Unmapped prefixes are not served. This is not an open proxy.
2. Joined paths are normalized and traversal is rejected.
3. **Redirects are never followed.** A `30x` from an upstream makes an ordinary HTTP client
   re-issue the request — **with the provider credential attached** — to a host the *upstream*
   chose. That turns any compromised or misconfigured backend into a credential-exfiltration
   primitive, and it needs no attacker access to dorang at all. The response is returned to the
   caller as-is; dorang does not chase it. A test fails if an attacker-nominated host is ever
   reached.
4. **Credentials are stripped in both directions.** "Never reach the client" was stated only
   for the request path. A backend that echoes back the key it was given would have that
   relayed straight through, so authorization headers are removed from the **response** too.

Step 3's "relay without parsing" and step 5's "price the usage" cannot both hold
unconditionally — pricing requires parsing. The reconciliation is a **bounded** tee: JSON
content types only, a hard byte ceiling, and streamed responses never touched. An unbounded
read here would reintroduce exactly the full-body buffering §15.5 prohibits, so the bound is
part of the contract rather than an implementation detail.

### 10.7 Metadata equivalence across protocol families

The three shapes dorang must speak — OpenAI chat-completions, OpenAI responses, and
Anthropic messages — name the same concepts differently and disagree on a few of them.
A neutral representation is worthless if each adapter invents its own reading, so the
mapping is **normative**, not per-adapter discretion. The rule for resolving disagreements
is **whatever the market already does**, because the value of this table is that existing
clients keep working, not that it is elegant.

Canonical names below are the field names in `CanonicalRequest` / `CanonicalResponse`.
`—` means the family has no equivalent. It does **not** by itself say what dorang does about
it: `Seed`, `TopK` and `Store` are dashes that are dropped and named, and `Stop`, `Logprobs`,
`ServiceTier` and `N` are dashes that are refused. §10.1's classification decides, and the
sentence that used to stand here — "a dash is a structural-loss case, not a droppable
parameter" — asserted the opposite of what the mask contained for four of these rows while
the mask asserted the opposite of the sentence for four others. Two documents disagreeing
about the same rows is how the misclassification survived a review: each looked correct
against the other half.

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
| `N` | `n` | — | — |
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
| `Created` | `created` | `created_at` | — |
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
   > **`input_tokens` does not identify a family.** Two of the three columns above spell the
   > prompt count that way and mean opposite things by it: `messages` excludes the cached
   > prefix, `responses` includes it. A decoder that reads the wire without knowing which
   > endpoint produced the body cannot settle the convention from the input key, and **every**
   > decoder is one of those. The passthrough relay of §10.6 step 5 never knew the route at
   > all; a converting decoder knows only which route dorang ADDRESSED, and which shape comes
   > back is the host's choice — a host serving both APIs on one base URL may answer either,
   > and a translating proxy answers in a third combination, one family's envelope around
   > another's usage block. What settles it is the
   > breakdown object, whose spelling the three families do **not** share:
   > `prompt_tokens_details` (inclusive), `input_tokens_details` (inclusive),
   > `cache_read_input_tokens` beside `input_tokens` (exclusive). The breakdown outranks the
   > input key, in either order, because JSON members are unordered.
   >
   > The cost of not knowing the third spelling is the same wrong invoice, arriving by
   > omission: a Responses body whose `input_tokens_details.cached_tokens` is dropped decodes
   > with `CacheReadTokens = 0`, and §8.5 prices a declared sub-rate by carving its quantity out
   > of the parent's — so the whole inclusive prompt is charged at the full **uncached** input
   > rate. Measured on the rate card of §8.5, a 120/15 request with a 40-token cached prefix and
   > 8 reasoning tokens was charged $0.000153_0 against the vendor's $0.000153_8.
   >
   > On a **converting** path the same body cost more than its cache row. A Responses answer
   > read by the chat decoder has its counts under keys that decoder does not know AND its
   > assistant turn under `output` rather than `choices`, so it decoded to a successful
   > response with no content and no usage at all: the same 120/15 request charged
   > $0.000000_0. The answer's own shape therefore selects the decoder — `object: "response"`
   > or an `output` array, with a `choices` array refusing — and the usage block's shape
   > settles the convention separately, because a body can carry one family's envelope around
   > another's counts and only the second question is about arithmetic.
2. **Reasoning tokens are billed as output.** One family breaks them out, another folds them
   in. `ReasoningTokens` is reported separately *and* is already contained in
   `OutputTokens`, so cost never adds them twice. A test asserts
   `OutputTokens >= ReasoningTokens`.
3. **Every decoder records WHICH counters the backend stated**, in `Usage.Reported`. A token
   counter has three states on the wire and an int holds two: the backend said 7, the backend
   said 0, or the backend said nothing. `cached_tokens: 0` is a measurement — the cache
   returned nothing on this request — and an absent breakdown is a capability statement, and
   an encoder with only the integer to look at omits the measured zero, so a customer's cache
   savings vanish from their own accounting with no error anywhere. A decoder that sets no
   flags makes every count it produces indistinguishable from one dorang invented. Where the
   vendor OMITS a breakdown it has nothing to say about — the Gemini family does — the wire
   struct needs a pointer, or the flag inverts the same defect and reports a measurement that
   was never made.

#### Usage on the non-chat surfaces

The table above is the three chat families. The other surfaces bill in the same tokens and
spell them their own way, and every one of these was read as ZERO by the decoder that owns
it until it was measured. Zero is not a rounding error here: `Result.Usage` is the single
source for the `X-Dorang-Tokens-*` headers, for internal/meter and for pricing, and §11.6's
token guard triggers on a positive total — so a surface priced at zero is also a surface the
guard cannot see. Nothing fails, so nothing alerts.

| Surface | Prompt count | Completion count | Breakdown |
|---|---|---|---|
| embeddings (relayed) | `prompt_tokens`, else `input_tokens`, else `total_tokens` | — | none |
| rerank | `prompt_tokens`, else `input_tokens`, else `total_tokens` − output | `output_tokens` | `meta.billed_units` (search units, priced apart) |
| images | `input_tokens` | `output_tokens` | `input_tokens_details` — `cached_tokens` priced, `text_tokens`/`image_tokens` carried |
| audio | `input_tokens`, else `total_tokens` | `output_tokens` | `input_token_details` (singular `token`) carried; `type` + `seconds` **priced**, as `Usage.Billed` and `Usage.AudioSeconds`, for a duration-billed model |
| gemini | `promptTokenCount` | `candidatesTokenCount` + `thoughtsTokenCount` | `cachedContentTokenCount`, `thoughtsTokenCount` |

Three rules make that table safe to read as a whole:

- **A total is a FALLBACK, never a preference, and it is taken net of any stated output half.**
  An embedding and a rerank have no completion half, so a body stating only a total is stating
  its prompt count — but a body stating 400 in, 7 out and 407 total is stating two quantities
  priced at two rates, and reading the total as the prompt count bills the generation at the
  prompt rate.
- **A billing UNIT is never converted.** The audio surface bills in tokens or in seconds,
  discriminated by `usage.type`, and a duration is not a token count. Both are carried; neither
  is folded into the other, and an encoder never names a unit the backend did not.

  > ⚠️ **The rule holds between two DURATIONS as well, and that is where it was broken.**
  > "Seconds" named two different billable quantities: the length of the recording a
  > transcription vendor bills for, and the wall time the request took. dorang carried one
  > field and one `unit: per_second`, filled from the request's elapsed time — so **a
  > ten-minute recording transcribed in eight seconds was charged as eight seconds**, 1.3% of
  > the invoice, with no error anywhere.
  >
  > Both quantities are real and both have real rates. A GPU-second rate on a self-hosted
  > deployment is priced on how long the request held the machine; a transcription rate is
  > priced on the recording. So the fix is not to pick one — it is that there is no unqualified
  > second left to pick wrongly:
  >
  > | Axis | Catalog `unit` | Rate key | Priced against |
  > |---|---|---|---|
  > | wall time | `per_compute_second` | `compute_seconds` | how long the request took |
  > | recorded media | `per_audio_second` | `audio_seconds` | `usage.seconds`, what the vendor billed |
  >
  > `unit: per_second` and the bare `seconds:` rate are **load errors** that name both
  > replacements, because an operator who writes one is not making a typo — they are writing
  > the only spelling that used to exist. The convention is decided once and written where a
  > catalog author reads it, exactly as §8.5's inclusive/exclusive rule is, rather than
  > becoming a per-catalog knob.
  >
  > A rule that names one axis while the request carries only the other is a **no-price**: the
  > request is recorded UNPRICED under §8.3 and the rule that declined is named, rather than
  > the rate being applied to the number that happens to be there. That covers the unit
  > disagreement one level up too — a `per_audio_second` rule against a transcript the vendor
  > billed in tokens, and a token rule against one it billed by duration — because in both
  > cases the arithmetic succeeds and produces a plausible figure.
- **A count with no canonical counter is carried, not deleted.** `text_tokens`/`image_tokens`,
  the audio breakdown, a vendor's `classifications` — dorang cannot price them and they are
  still line items on somebody's invoice, so they ride in `UsageExtra` at the nesting level
  they arrived at.

#### Response identity — `id` and `created`

**The upstream's `id` crosses unchanged when the answer did not change family, and is minted
in the caller's family shape when it did. `created` is the upstream's own when its family has
the member and this gateway's clock when it does not. Both rules hold on the streaming and
the non-streaming path, and that is the whole of the rule.**

Crossing the id is defensible — it is the only handle a support ticket has on the upstream's
side of an exchange — and it is defensible *only* while the id is one the caller's family
could have produced. `{"object":"chat.completion","id":"msg_2026…"}` is not: it is an
identifier of another family under a member naming this one, and no client's own shape
describes it.

> ⚠️ **This was the second place the two paths disagreed, and the first was usage.** Converting
> a Messages answer to a chat completion, the non-streaming converter emitted `"created":0` — a
> timestamp in 1970 on every converted turn — and crossed the upstream `msg_…` id verbatim,
> while the streaming writer for the identical conversion stamped a real timestamp and minted a
> `chatcmpl-` id. Same request, same upstream, two different answers, selected by nothing but
> `stream:true`.
>
> The pattern is worth naming because it has now recurred: a property is implemented once in a
> streaming writer and once in a response encoder, and the two drift. Usage was pinned by
> `TestStreamingAndNonStreamingAgreeOnUsage`; identity is pinned by
> `TestStreamingAndNonStreamingAgreeOnIdentity`. **A third such property should be pinned by an
> equivalence test on the day it is written, not on the day it is found in production.**

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
create → validating → in_progress → finalizing → completed
  rows grouped by prefix hash of the BODY (see below) so cache-adjacent work runs together
  each row is an ordinary request marked batch (§5.2), so the reserve applies on every axis
  everything flows through §5, so batch cannot exceed configured capacity
  retries with backoff on retryable statuses; partial failure is expected
complete → output JSONL + error JSONL
cancel → cancelling → in-flight drains → cancelled, partial results preserved
```

> **Grouping must hash the request body, not the JSONL line — otherwise it is a no-op that
> looks implemented.** A batch line begins with its `custom_id`, which is unique by
> construction, so a chain over the raw line diverges at the very first segment for every row
> and every group has exactly one member. Nothing fails; the cache benefit simply never
> materializes.
>
> The segment size differs too. §7.4b's 4 KiB base is chosen for routing, where bodies are
> large; most batch rows are shorter than that, so a single segment swallows the differing
> tail and again groups nothing. Grouping uses a smaller base (512 B default). **Routing and
> grouping cannot share the segment size**, and treating one constant as serving both is how
> this stays broken while appearing correct.

> **Status names follow the vendor enum, not this document's internal vocabulary.** An earlier
> draft named a `queued` state; no such value exists in the published batch object, and a
> strict SDK rejects an unknown literal. It is kept internally and rendered as `validating`.
> `finalizing` and `expired` do exist in the vendor shape and were missing here.

> **What "batch priority" actually buys is admission control, not backend scheduling.** On a
> self-hosted engine the priority field is silently ignored unless the operator enabled the
> policy (VLLM.md §1.2), and the two engines order it in opposite directions (§7.5). dorang's
> own reserve is the protection that always works. Priority passthrough is a bonus where it is
> configured, and describing it as the mechanism would be describing something that is off by
> default.

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
  path, in keeping with §9.6. The poll interval is clamped to a fraction of the margin —
  a poll slower than the margin steps over the whole window and lands on an expired token,
  which is the request-path refresh this bullet exists to avoid.

Six things implementation showed the description above does not cover, each with a
consequence:

- **In-process single-flight does not prevent the lockout it was motivated by.** The store is
  *shared with the vendor's CLI*, so the racing party is another process. The store is
  therefore re-read immediately before every exchange, and a newer token found there is
  adopted rather than a fresh one minted — turning a cross-process race into a file read.
- **A revoked token yields a 401 per in-flight request**, so "refresh once and retry once"
  without a gate is a refresh per request: exactly the ban this section warns about. The
  401 path short-circuits when the credential has already moved past the token that failed,
  and the backoff gate sits *inside* the single choke point so that path cannot walk around it.
- **"Unhealthy" and "unusable" are not the same.** A failed refresh does not invalidate the
  token it failed to replace. Unhealthy stops the credential being *chosen*; the existing
  token keeps serving until it actually expires. Conflating them drops a working account on
  one auth-server blip.
- **A store write that fails after a successful exchange is its own failure mode**: the
  refresh token is spent and its successor unrecorded, so every other reader of that store is
  now broken. The credential is marked unhealthy with that specific reason while the
  in-memory token keeps serving.
- **A token with no expiry cannot be refreshed ahead of expiry.** Treating unknown as due is a
  refresh loop, so it is never scheduled and relies on the 401 fallback alone.
- **"Tokens are secrets" cannot bind the refresher** — that is provider code, and its errors
  can carry tokens. Every token the credential has held is scrubbed from anything recorded,
  and the provider's error is deliberately **not** wrapped, because wrapping would put its
  unscrubbed text straight back into the message. The same reasoning excludes command output
  from an exec-sourced credential's errors.

Two constraints inherited from elsewhere in this design:

**An OAuth credential is still a credential**, so §7.4a2's affinity rules apply unchanged — a
conversation pinned to an account stays pinned across a token refresh, because the account is
the same account. The token is an implementation detail of talking to it.

**Tokens are secrets and follow §4.1** — never logged, never in an error message, never in a
`Snapshot`. The only thing that leaves this subsystem is the credential's opaque id and its
health.

### 11.2c Key rotation, and how fast a revocation actually takes effect

Client keys are managed the way a credential should be: **the identity is durable and the
secret is not.**

#### Rotation

A key has an id, and one or more **secrets** attached to it. Rotation mints a new secret and
leaves the old one valid for a grace period.

```yaml
auth:
  rotation:
    grace: 24h              # old secret stays valid this long after rotation
    max_secrets: 2          # at most one overlap in flight
    max_age: 90d            # policy: warn, then require rotation
```

Everything else about the key — its tier, budget, spend to date, model allow-list, rate
limits, team, and its whole ledger history — **belongs to the id, not the secret**. That is
the entire point. A rotation that also reset the limits would be a re-provisioning, and an
operator facing that will put it off, which is how a five-year-old secret happens.

- `POST /key/rotate` returns the new secret **once**, and reports when the old one expires.
- Both secrets authenticate to the same principal during the grace period, and the ledger
  records which one was used — so an operator can see whether the client actually rolled
  before the window closes, instead of finding out when it shuts.
- The grace period can be **ended early**, which is what a suspected compromise needs: rotate
  now, cut the old secret immediately, keep everything else.
- `max_age` is a policy, not an execution. dorang warns and reports; it does not silently
  break a working integration on a timer.

#### Revocation is only as fast as the cache — and that was not stated

§11.2's hot path answers from a lock-free snapshot with a TTL. That is what makes
authentication 354 ns, and it means a revoked key **keeps working until the snapshot
refreshes** — on every node independently.

For an expiry that is fine. For a **compromised key**, a pended key (§11.6), or a rotation cut
short, it is a window during which the thing you just switched off is still serving. Nothing
in the design said how long that window is, which is the same class of omission as a control
that is never called: the mechanism exists and its guarantee was never stated.

So revocation is explicit rather than incidental:

1. **A revocation, a pend, and an early grace cut all publish an invalidation**, and the
   snapshot drops that key immediately on receipt. The TTL becomes the *fallback* for a node
   that missed the message, not the mechanism.
2. **Single node**: immediate, since there is one snapshot.
   **Clustered**: through the same coordination §13 already uses, and the **worst case is
   published as a number** — the same rule §5.6 applies to overshoot. "Revocation is fast" is
   not a specification.

   The number, and the arithmetic that produces it (`auth.RevocationBound`):

   | Topology | Bound | Formula |
   |---|---|---|
   | single node | **0** | the control drops the key from the one snapshot before it returns |
   | clustered | **`poll + store_latency`** — 1.25 s at the defaults (`poll: 1s`, `store_latency: 250ms`) | `propagation(store poll + store round trip)` |

   The fallback for a node that **missed** the message is two numbers, not one, because
   the two kinds of cache entry behave differently and reporting one figure for both would
   be false:

   | Entry | Fallback | Why |
   |---|---|---|
   | learned at runtime through the store | `entry_ttl` (60 s) | it expires |
   | placed by a bulk load (`Load`, `Rejoin`) | the **reload interval** (`entry_ttl / 2`) | it does **not** expire — §9.1 makes the snapshot the authority, which is what makes a snapshot hit cost no store read. Its fallback is the interval at which the whole set is re-read, and a deployment that schedules no reload has **none**: the published figure then reads *never*, not *0* |

   The bound is stated **from the moment the control returns**, not from its call: the
   control's own durable write is synchronous and already visible to the operator who ran it,
   and folding it in would produce a figure that describes the database's write latency rather
   than the propagation this mechanism is about. An **unmeasured** propagation delay publishes
   `entry_ttl`, because an unmeasured delay is not a small one.

   Measured end to end on SQLite, four nodes with independent store handles and independent
   snapshots, `poll: 20ms`: **~21 ms** to the last node, against a published 270 ms. Single
   node: a **zero window** — the first request after the control returns is already refused.
3. **The bound is enforced by shortening the TTL for negative entries specifically.** A key
   that was refused is cheap to re-check; a key that is serving is not. The two do not need
   the same freshness, and treating them alike is what made the number large.

   > **That TTL is also the bound on the OPPOSITE event, and only this paragraph says so.**
   > A key that is used *before* it is created — a client that starts up beside its own
   > provisioning, a rotation script that hands the secret over before the row commits —
   > gets a refusal, and that refusal is cached. **Creation deliberately publishes no
   > invalidation**: there is nothing to invalidate on the other nodes, because a row that
   > has never existed cannot be in anybody's snapshot, and publishing an invalidation for
   > every key issued would put the provisioning rate onto the same bus a compromise
   > depends on.
   >
   > So the window in which a *newly created* key is still refused is bounded by
   > `negative_ttl` (5 s) and by nothing else. It is short for the same reason it is short
   > in the revocation direction — a refusal is cheap to re-check — and it is the reason
   > `negative_ttl` may not simply be raised toward `entry_ttl` to save lookups: doing so
   > lengthens both windows, and only one of them is about an attacker.
   >
   > The figure is `auth.RevocationBound().Negative`, and it is published there as "the
   > reversibility half of the same number" precisely so that this direction has a stated
   > bound rather than an assumed one.
4. **A revocation must not be lost by a node that was down.** On rejoin a node reloads rather
   than trusting a stale snapshot, and this is one of the few places the request path is
   deliberately allowed to wait: serving from a snapshot known to be stale is worse than a
   brief pause at startup.

> This interacts directly with the token guard (§11.6). A guard that pends a key but leaves it
> serving for a cache TTL has not stopped anything — it has only started a timer. Pend uses
> the same invalidation path, and the guard's own tests assert the key stops serving, not that
> a flag was set.

#### What is deliberately not built

Short-lived derived tokens — minting a brief access token from a long-lived key, the way OAuth
does — would shrink the exposure of a leaked secret further. It is not built, because it moves
a token exchange onto the client's critical path and requires every client to implement
refresh, which is a large cost for callers who mostly hold their key in an environment
variable. Rotation with a grace period gets most of the benefit for none of that, and the
door is left open: the key already has multiple secrets and an expiry per secret, which is
the shape a derived token would use.

### 11.3 Administration UI

A single SPA embedded in the binary — no external assets, works offline. Version 1 ships
three screens: **keys**, **models and deployments**, **usage and cost**. Request-log
browsing, the price calculator, and batch management follow.

### 11.4 Users and teams

Both the shape-compatible paths (§2.3) and a native admin API.

### 11.5 Events, webhooks, and the Lua plugin surface

#### Events are a bus with sinks, not an email system

The notification surface is an **event bus**; email is one sink and a webhook is another.
Framing it the other way round produces a design where the webhook is an afterthought with
weaker guarantees than the mail it was bolted onto.

```yaml
notifications:
  events: [key_created, budget_80pct, budget_exceeded, quota_exhausted,
           credential_unhealthy, batch_completed, invite]
  sinks:
    - { kind: webhook, url: …, secret_env: DORANG_WEBHOOK_SECRET, events: [budget_*, quota_exhausted] }
    - { kind: smtp,    …,                                          events: [invite, key_created] }
    - { kind: lua,     handler: on_event }
```

Several sinks may take the same event. Four rules, each closing a way this goes wrong:

1. **A webhook delivery is signed.** HMAC over the body with a per-endpoint secret, in a
   header. A delivery carrying budget and quota state is an information leak the moment the
   URL is reachable by anything else, and the receiver otherwise has no way to distinguish a
   real delivery from a forged one.
2. **At-least-once with an idempotency key**, bounded retries with backoff, and a visible
   dead-letter count. A receiver that is down must not become dorang's problem: bounded queue,
   counted drops, and **never on the request path** — a budget crossing 80% is *discovered*
   while serving a request; the notification about it is not sent there.
3. **No secret in a payload.** A `key_created` event carries the key's id and label, never the
   key. Same rule as §2.4's refusal to copy a display column that leaked trailing characters.
4. **Deduplicate per subject per period.** `budget_80pct` is true on every request past the
   threshold, so without this it fires on every request — the difference between an alert and
   a filter rule someone writes to make it stop.

#### Lua is the extension mechanism, and the reason is plugins

The hooks could have been a fixed set of Go callbacks. Lua is chosen deliberately: it is
small, fast, and embeds cleanly, and it is what makes a **plugin structure** possible — an
operator drops in a file that registers handlers, rather than the gateway shipping every
policy anyone might want.

This is the project's first non-trivial runtime dependency, so the constraint that keeps §0.2
true is explicit: **the VM must be pure Go**. A cgo interpreter would break the static binary
that makes the notebook tier's "zero required dependencies" literal rather than aspirational.
The VM is gopher-lua, about 1.8 MB of binary.

It supplies one of the three ceilings below and not the other two — a context check runs
between VM instructions, so a wall clock can stop a hook, but there is no per-state
instruction counter and no per-state allocation accounting. Both are built on top, because a
wall clock alone is not a sandbox: a hook allocating in a tight loop exhausts the process long
before a 200 ms deadline fires. The instruction ceiling is source instrumentation — a charge
at every function body, every loop body and every backward `goto`, which are the only three
ways *Lua source* can run unboundedly.

**Source instrumentation bounds instructions, not work**, and the distinction is not academic:
one instruction can enter a builtin, and no rewrite of the source can see inside host code. A
builtin whose work is not bounded by what it *returns* is therefore charged for that work
before it runs. Two families needed it. The pattern matcher backtracks superlinearly in a
subject the caller supplies — `.-.-.-@` against 256 bytes ran **4 seconds** while costing two
units of budget, because `string.find` returns two integers. And `tonumber` reads its whole
argument to return a number: 8 KiB of digits ran past **twenty seconds** with no ceiling ever
firing. Both are now priced by running the match against a budgeted copy of the matcher first,
and refusing before the real one is asked.

**Charging work bounds CPU and nothing else**, which is the same mistake one level down, and
it left two holes that the pricing pass looked straight past:

- **The stack is a resource.** The matcher is a recursive VM — one Go frame per branch
  explored — so a greedy quantifier costs a frame per character it consumes, and the caller
  chooses how many by choosing how long the text is. `^(.*)=(.*)$` against 800 KiB of a
  caller's own message text, eight requests at once, took **3 072 MB of goroutine stack** with
  `limitHits = 0` and no ceiling firing; Go kills the *process* at a 1 GB stack, so a large
  enough segment was a crash rather than a refusal. Depth is now bounded explicitly inside the
  matcher — 10 000 frames, about 4 MB per match — and the same bound covers gopher-lua's own
  copy, because the refusal happens before it is asked. The cost is stated rather than hidden:
  a quantifier that must carry more than 10 000 characters in one run is refused, and a filter
  that wants whole documents should ask the host (`dorang.mask` runs Go regexps, which have no
  stack of their own) rather than backtrack through them in Lua.
- **A priced call still needs the wall clock.** The pricing run took no context, so it spent
  its whole step budget on a goroutine nobody was waiting for — the one thing in the sandbox
  the watchdog could not reach. It now takes the invocation's context and checks it every
  thousand steps.

`string.gsub` needed a third charge for a third reason: gopher-lua rebuilds the entire subject
**once per match**, so the work is O(matches × length) and neither the scan charge nor the
result charge could see it — `gsub(s, "a", "y")` over 400 KiB ran the wall clock out with no
ceiling counted. The same call written with a *function* replacement was charged one byte per
position, because the length of what a callback returns does not exist until it returns; it is
now charged where it does exist, in a wrapper around the callback.

The same reasoning reaches three *operators*, which have the identical problem and no call to
hang a charge on. `s == s2`, `s < s2` and `t[s]` are single VM instructions that compare or hash
every byte of a string whose length the **caller** chose, and §10.5b's masking filter is a hook
that exists to look at caller text — so no exotic plugin was required, only one that compares or
indexes by something a caller sent. Measured against 64 KiB operands with a hook spending its
whole 5 M-instruction budget, against **130 ms** for the same budget of ordinary Lua:

| | unpriced | priced |
|---|---|---|
| `s == s2` | 753 ms | 1 ms |
| `s < s2` | **40.1 s** | 68 ms |
| `t[s]` | 3.16 s | 2 ms |
| `t[s] = 1` | 7.17 s | 4 ms |
| `rawequal(s, s2)` | 803 ms | 1 ms |

The fix is *not* the one the `..` rewrite suggests. Replacing the operator with a host call would
mean reimplementing `__eq`, `__lt`, `__le` and `__index` — gopher-lua exports no entry point for
`<=` at all, so one of the four would have been a guess — and it could not have covered `t[k] = v`
in any case, because an assignment target cannot become a call. Instead the *operand* is wrapped
in a charge for its length and the operator stays in the VM, so every metamethod keeps its exact
semantics and reads, writes and table constructors are all covered by one wrapper.

**The cost is small because most sites are not wrapped.** A site is charged only where neither
side's cost is already fixed at load: a literal on either side bounds the work by its own length,
and so does an operand that cannot be a string (`#x`, `not x`, a comparison, a constructor). That
leaves `req.model == "gpt-4"`, `t.field`, `t[i]` and `i <= n` untouched — and leaves the shipped
`pii_mask.lua` filter with **no charged site at all**, its per-request cost unchanged at 12.8 µs
with identical allocations. An ordinary policy hook pays about 130 ns per request for the two
dynamic lookups it does make; one charged site costs about 64 ns and allocates nothing. The
spelled-out twins are priced alongside the operators — `rawequal`, `rawget`, `rawset`, `next`,
`pairs`, and `table.sort` without a comparator — because pricing either half alone would have
bought nothing.

What is left is bounded rather than open. `t[k]` walks an `__index` chain and `t[k] = v` walks
`__newindex`, so one instruction can probe up to gopher-lua's hundred tables; no caller string
changes that depth, because it is a shape the plugin built. A hook spending its whole budget on ninety-deep lookups measures **3.6 s**,
and the default 200 ms wall clock stops it there. That backstop is real here where it was not for
the pattern family, and for the reason that made these operators chargeable in the first place: a
context check runs between VM instructions, so a *loop* of operators is interruptible where one
`string.find` was not.

The memory ceiling is charged allocation: every allocation is either O(1) per charge, and so
bounded by the instruction ceiling, or is charged before it happens.

Hooks: `on_request`, `on_route`, `on_response`, `on_event`, and §10.5b's `on_filter_request`.
There is deliberately no `on_filter_response`: unmasking runs once per streamed frame inside
§10.5's single pass, and calling an untrusted VM there would put a sandbox on the token path
for a job the host already does exactly.

- **Loading is explicit configuration**, never a scan of a writable directory. A plugin
  mechanism that picks up whatever appears in a path is a code-execution primitive.
- **The value shape is versioned.** A plugin written against today's `on_request` keeps
  working, or is told plainly that it does not.
- **A plugin is untrusted code inside the process that holds provider credentials.** No
  credential, token, or key material is reachable from any hook, and that is tested
  adversarially rather than assumed — a plugin author need not be hostile for this to matter.
- Instruction, memory, and wall-clock ceilings are enforced **by the host**, not by
  cooperation. Disabled by default on the hot path, where the cost must be an untaken branch.
- **Fail-open on a limit breach; fail-closed only on an explicit deny from `on_request`.**
  A broken extension must not take the gateway down, and a policy that says no must be obeyed.
  That asymmetry is the whole safety argument.

### 11.5a Email driver detail

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

A hook is **abandoned, not killed** — Go cannot kill a goroutine — so what is bounded is *how
many*. Bounding abandonment *over time* is not the same as bounding it *at an instant*, and
only the second is a bound: without it, a hook stuck inside one uninterruptible call pins a
core per request and keeps every one of them after being taken out of service — the operator
then sees hooks disabled and load unexplained.

The instantaneous bound needs **two** supplies, and the first version of this paragraph
published one. It said "a fixed number per hook may be outstanding at once; past that, an
invocation is refused before a goroutine exists to abandon", and the check that implemented it
reads a counter incremented at *abandonment* while refusing at *admission* — so a burst passes
admission before any of it has been counted. Measured: sixteen simultaneous requests to a
wedged hook, sixteen abandoned goroutines, against a stated bound of eight, with no trip
involved and nothing a plugin did wrong. A bound a request rate can exceed is not a bound.

What holds is a reservation taken before the goroutine exists and released when it exits:

- **At most 32 goroutines per hook at any instant**, whatever arrives at once, for the
  engine's whole life. It binds only on hooks that are already failing — concurrency is
  arrival rate times duration and duration is capped by the wall clock, so reaching it takes
  160 invocations a second that each burn the entire 200 ms ceiling, where the shipped masking
  filter takes 12.8 µs and would need 2.5 M/s.
- **At most 8 of those abandoned** — timed out, no longer waited for, still running — after
  which invocations are refused before a goroutine exists. So the steady state past a burst is
  8, and 32 is what a burst may transiently cost.

Two smaller corrections come with it. A hook that *finishes* as its deadline passes is not
abandoned: the deadline is followed by a bounded grace, and only a hook that misses that too
is counted, because counting a leak that did not happen is how thirty-two of them switched a
working hook off. And the refusals **count toward the trip only for a hook that fails open**.
That instruction was right for the hook it was written for — a wedged enrichment hook that
refuses is doing nothing in silence, so switching it off is the honest end state — and wrong
for §10.5b's masking filter, where a refusal *is* the loud outcome and "switched off" is not a
state that exists. Refusals cost microseconds, so counting them turned a burst into a
permanent decision in well under a second; see §10.5b for what that decision then did.

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
upstream connect, TTFT, total.

**Throughput is derived, not stored.** The ledger keeps `ttft_ms`, `latency_ms` and the output
token count, and generation rate follows from them:

```
tokens_per_second = tokens_output ÷ (latency_ms − ttft_ms)
```

TTFT is subtracted deliberately: it is queueing and prefill, not generation, and including it
makes a backend with a deep queue look like a slow generator (§7.5a). Storing the quotient as
well would be a fourth number that can disagree with the three it comes from — the derivation
is exact, so the columns are the record and the rate is a view over them.

The same three numbers feed §7.5a's routing signals through a smoothed average, and are
exposed per deployment as `dorang_deployment_tokens_per_second` and `dorang_ttft_seconds`.
Routing wants the smoothed value because one slow request should not move a decision; an
operator asking "what did this key actually get" wants the exact per-request figures. Both
come from the same measurement taken once, at the first byte. OTLP export is optional; the breakdown is always recorded.

Message content defaults to a truncated excerpt (512 characters) in `request_traces`,
subject to sampling and a daily byte budget. `none` and `hash` are also available.

### 12.3 Metrics

Requests, duration, TTFT, tokens, cost, capacity in-flight and wait time per axis,
credential health, provider quota percentage, budget consumption ratio, prefix hit ratio,
fallbacks by reason, metering drops, and spool depth.

#### Three series that are emitted and were not written down

The list above is a list of *subjects*, and three series that exist in the scrape belonged to
none of them. All three answer the same kind of question — **is a bound being pushed on, and
is anything being lost while it is** — which is the class of series that is useless unless an
operator knows to look for it.

| Series | Type | What a non-zero value means |
|---|---|---|
| `dorang_auth_lookup_throttled_total` | counter | Credential lookups refused **without consulting the store**, because the unknown-key budget (§11.2, `auth.miss_budget`) was empty. Every distinct unknown key used to be one database round trip an unauthenticated caller could buy for nothing; this is what that amplifier costs now. **Read it against `dorang_auth_store_calls_total`: this one rising while that one flattens is the bound holding.** A sustained non-zero value with no attacker means the budget is sized below the deployment's real rate of lookups that find nothing — a client retrying a key that was revoked, most likely — and those callers are getting `503 auth_unavailable` |
| `dorang_meter_records_refused_closed_total` | counter | Completed requests handed to the meter **after it was closed**, and refused. It is the only way §12.1's numeric path can lose a count, and the window is shutdown, where the drain races the meter's own close. It is a counter rather than a silence because "the ledger stopped listening before the last requests finished" is not a thing to infer from an absence |
| `dorang_metering_degraded_reason{reason="meter_closed"}` | gauge | The reason set gained a sixth member for the row above. Any non-zero `..._refused_closed_total` raises `dorang_metering_degraded` with this reason, so the shutdown loss is visible on the same signal as every other metering degradation rather than only in a counter nobody is alerting on |

The full reason set of `dorang_metering_degraded_reason` is therefore `none`,
`trace_queue_full`, `spool_full`, `spool_error`, `sink_error` and `meter_closed`. Sampling and
the daily byte budget still never set it: those are policy, and conflating policy with failure
makes the signal useless on any deployment that samples.

> **Three of the labels above were not sourceable as written, and the reason is worth
> recording rather than quietly dropping them.**
>
> - **Capacity wait time per axis.** The broker grants across every axis atomically and never
>   records which one blocked, so the label has no value to carry. Wait time is emitted
>   globally, and the HELP text says why — a label that is always the same value is worse than
>   no label, because it implies a breakdown exists.
> - **Prefix hit ratio per model.** The prefix table is keyed by content digest and is never
>   told the model — deliberately, since §7.4b seeds the chain with the group id and needs
>   nothing else. The per-model figure is counted at the request observation point instead;
>   the table's own exact global figures are separate metrics.
> - **`credential_health`.** dorang's circuit breaker is per **deployment**, not per
>   credential — §12.3 named a thing that does not exist. Deployment health is now its own
>   metric, and credential health means only what OAuth refresh reports about a credential.
>
> **A metric dorang cannot compute is absent, never zero** — the rule VLLM.md §3.3 exists to
> teach, where a load endpoint returns an attractive zero forever when unconfigured. Absent
> metrics here include time-to-first-token before any sample, prefix ratios with prefix off,
> budget ratio with no ceiling, and a rolling quota window with no reported reset instant.
>
> Naming is validated rather than reviewed: a `_ratio` must be in [0,1], a `_percent` in
> [0,100], a `_total` must be a counter, and a histogram must name its unit. That check
> immediately caught a `_total` declared as a gauge in shipped code.

### 12.4 Backend metrics

A provider may declare a metrics endpoint to scrape. Queue depth and cache utilization then
become routing signals for `least_busy` and `highest_tps`. Collection ships first; using it
for routing is opt-in behind a flag.

> **R17 is not built, and `providers[].metrics` is refused at load rather than left inert.**
> The configuration block existed — `enabled`, `endpoint`, `interval`, with a validator that
> required an endpoint when the flag was set — and the validator was its only reader anywhere
> in the tree. Nothing fetched the URL. That is §17.1's dominant defect class in its most
> convincing disguise: the key was checked, so it looked wired, and CONFIG §6.2's four
> engine-specific traps read as operational advice for a live path.
>
> Refusing costs the operator nothing here, which is why it is the right disposition rather
> than a strict one. **Both strategies this section names are implemented and neither depends
> on a scrape**: `least_busy` ranks on `internal/capacity`'s live occupancy of the axis the
> request would reserve — dorang's own count, exact, with no poll interval to be stale over —
> and `highest_tps` on `internal/health`'s measured output tokens per second from completed
> requests. Both treat "no samples" as no opinion (§7.5a), so an unused deployment is neither
> favoured nor punished. The refusal names them.
>
> What a scrape adds is the **engine's** view rather than dorang's, and it is a better signal
> in exactly one case: a self-hosted backend also serving traffic that did not come through
> this gateway. That case is real and is what R17 is for. The traps below are why a collector
> is not the hard part of it — a vLLM started with `--disable-log-stats` answers `200` with
> zero series, which a naive scraper reads as *idle* and routes toward.

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

### Node identity

`cluster.node_id` names a node in the `nodes` registry, in the leadership lease, in every
`quota_leases` row and in the ledger's budget draw. Those are four different things keyed by one
value, which is why the value has to be **distinct per node** and why getting it wrong is not
confined to leadership.

Two processes carrying the same id are one node to all four. One registry row, so neither is ever
declared dead and neither one's leases are reclaimed; one share of every leased limit, drawn down
by two; and one leadership lease that **both** hold — because the store sees the second acquire as
the incumbent renewing. The fencing token does not move, so both processes carry a token equal to
the row's and both pass the fence check. Every mechanism in this section works as specified and
there are two leaders, running every leader-only job twice, including the batch assignment §9.2
prices at *every finished row paid for twice*.

The refusal is therefore in the registry and not in the lease, and it is a refusal to **start**:

- `Register` is a compare-and-swap on a per-process incarnation, so a process cannot take an id
  another process is still beating on. The loser does not join, lead, or run a leader job.
- `Heartbeat` carries the same incarnation, so a process that was frozen past the node TTL and
  legitimately superseded learns it from the row, stops leading, and stays out. This is the
  identity analogue of the fencing token's argument about terms.
- A restart is not a duplicate: a lapsed row is adopted, a cleanly drained node left none.
- An **empty** `node_id` derives one per process. That is the only setting that cannot collide,
  and its whole cost is that a restarted process cannot recognise its own leases.

A configuration file cannot prove the id is unique — it only ever describes one node — so
validation catches the one shape that is provably shared, an un-substituted template
(`${HOSTNAME}`, `{{ … }}`, `<…>`), and the run-time check catches the rest.

### Draining

The order, and why each step is where it is:

| # | Step | Bound |
|---|---|---|
| 1 | Readiness goes false | immediate |
| 2 | **Keep serving.** The listener stays open, requests are answered in full | `server.pre_stop_delay` |
| 3 | Listener closes | immediate |
| 4 | In-flight requests finish | `server.shutdown_grace` |
| 5 | Whatever is left is cancelled with a named cause, then cut | 2 × 1s |
| 6 | Leases and reservations released, unspent quota blocks returned, registry row removed | `server.shutdown_grace` |

**Step 2 is the one that is easy to omit and the one that matters.** Every load balancer
discovers unreadiness by *polling*. Between the readiness flip and the poll that notices
it there is a window in which the balancer is still routing here, so closing the listener
at step 1 is connection-refused at the client on every rolling restart — the exact failure
a graceful drain exists to prevent. The delay is configuration and not a constant because
it describes the *balancer*, not dorang:

```
pre_stop_delay  >=  probe period × failure threshold
                  + probe timeout
                  + endpoint-withdrawal propagation
```

`deploy/kubernetes.yaml` ships `periodSeconds: 2`, `failureThreshold: 2`,
`timeoutSeconds: 1` — five seconds of detection — against the default `pre_stop_delay:
10s`. An operator who changes one changes both. `0` is legitimate and means "nothing is
routing to me": a single node, a workstation. A second SIGTERM also skips the wait, so
Ctrl-C never appears to hang.

**Step 5: a cut stream gets an answer, not a reset.** A completion legitimately streams for
minutes, and 30 seconds of grace will sometimes expire on one. The response status went out
with the first chunk and cannot change (§7.6), so the only channel left is the body — and
§10.5 already establishes that dorang rewrites streams as a matter of course. At the grace
boundary every in-flight request's context is cancelled with a named cause, and the handler's
error becomes an in-band `gateway_shutting_down` frame followed by the terminator. A client
that is merely reset cannot tell a deploy from a crash from a bad network, and it has already
been billed for the tokens it received; a client that is told can retry against the node that
is still up. Only after that does the connection close.

The cancellation is also what keeps the *ledger* honest. It lets each handler's normal
unwind run, so the terminal usage record reaches the meter — and the drain is precisely
when that is at risk, because the process closes the meter and flushes the spool the moment
the drain returns. Work billed upstream and never recorded is silent revenue loss.

**One grace, not two.** A separate, longer grace for streams would buy nothing: the pod does
not go away until the longer window elapses, so the deployment's termination budget is sized
off it either way, and the shorter window could only cut short the requests that were going to
finish first anyway — the drain returns the instant the last request does, so a generous grace
costs nothing when nothing is slow. What a long stream needs at the boundary is an answer,
which is step 5, not more time.

**Step 6 is last, and deregistration with it.** Nothing routes off the `nodes` table — the
request path is stateless and the balancer decides — so leaving the row in place for the
duration of the drain costs nothing, while removing it early costs two things. The leader
reclaims a dead node's leases by walking that table, so a drain interrupted between step 1
and step 6 would leave leases it can no longer attribute; and the published overshoot of §5.6
divides limits by the *live* node count, so a node that deregistered while still serving and
still holding leases would make every peer size its share as if it were gone. Readiness is the
signal to the balancer, the registry row is the signal to the cluster, and they correctly stop
being true at different moments.

**Termination budget.** An orchestrator must allow
`pre_stop_delay + 2 × shutdown_grace + 10s` — the drain and the post-drain teardown can each
take a full grace, plus the two cut-over waits. Below that, SIGTERM becomes SIGKILL part way
through, which skips returning the unspent quota blocks and makes the restart inexact (§9.6).

**Single node: there is a gap, and it is accepted.** dorang has no socket handoff and no
`SO_REUSEPORT`, so between the old process closing its listener and the new one binding there
is nothing listening. Two nodes and a balancer remove it, which is what R14 requires and what
`deploy/kubernetes.yaml` ships. The notebook and single-node tiers take the gap: it is a
sub-second window and the alternative — inheriting a listener across an exec — buys
zero-downtime for a tier that is not serving production traffic, at the cost of a
process-lifecycle mechanism on the one path that is meant to have no moving parts. Stated
here rather than discovered in production.

**Wiring.** internal/app constructs exactly one `cluster.Node` per process and takes its
ledger from it rather than building a second one — two ledgers under one node id would be two
in-memory block caches over the same rows. The node is built whether or not clustering is on,
because the durable budget (§9.6) is on the request path either way; `cluster.enabled` decides
whether it **joins**. With it false nothing registers, nothing campaigns, no goroutine of that
package runs and the `nodes` table is never touched, which is what §0.2's "no required
dependencies" costs in a package about coordination. With it true the node registers,
heartbeats, campaigns, and runs the leader jobs — partition maintenance, the reservation sweep
of §5.3 and §6.4, and lease reclaim.

Lease reclaim is the one whose absence cost money: a node that crashes holding quota leases
never returns them, so the quota is leaked until someone notices and clears it by hand.

> **Still not wired, after that change.** The request path does not route quota through
> `cluster.Node.Coordinator` — internal/capacity still counts concurrency per node, so the
> coordinator owns the leases and publishes the accuracy without admitting or refusing
> anything. `RollupCompactionJob` and `BatchAssignmentJob` have no caller, so rollup
> compaction and batch assignment still run on every node instead of on the leader. And
> `capacity_mode: shared-redis` is refused at load: this build ships the protocol
> (`cluster.RedisClient`, the Lua scripts, `NewRedisShared`) and no client that speaks it, so
> accepting it would give an operator a coordinator reporting `shared-redis` while
> coordinating through the store.

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
| Load | `testing/perf` — the assembled gateway over a real socket against a fake backend with a known fixed delay, with §15.1's overhead boundary instrumented per request. This is where the published latency, allocation and footprint numbers come from |
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

Four corrections, the first of which is a defect rather than an omission:

1. ⚠️ **"Send the same request to the reference" is destructive taken literally.** It includes
   `DELETE /key/…`, which would delete a key on the system still serving production. Replay is
   **deny-by-default**: reads always, inference `POST`s only, nothing else. And since the
   served surface is roughly 80% control plane (COMPATIBILITY §0), a clean report necessarily
   covers materially less than the whole gateway — so the count of *skipped-as-unsafe* is
   reported beside the verdict rather than left implicit.
2. **The cost ceiling must stop, not skip.** Refusing the request that does not fit while
   continuing to admit cheaper ones biases coverage toward cheap traffic while the counter
   still reads under budget — a report that looks complete and is not. Cost is also reserved
   before the call and settled after, for the same reason §9.6 gives for budget.
3. **Two gateways running alongside can be pointed at each other**, which amplifies one
   request without bound. A marker header is sent and refused on arrival.
4. **Shadow does not hot-reload**, despite §4.1's rule that everything does. Rebuilding it
   re-arms the daily ceiling, which turns "$5 per day" into "$5 per `SIGHUP`". A changed
   shadow section is refused rather than applied.

**A verdict of "clean" means more than "no diffs".** It requires comparisons to have happened,
with no inconclusive records, no queue drops, no reference errors, and no dropped report
records. A comparison that cannot decide a dimension writes an inconclusive record naming it,
counted separately — because the whole value of this mechanism is that an empty report is
trustworthy, and a comparison that silently skips a case is worse than one that reports a
difference.

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
| `warm-local` — auth cached, local capacity, prefix on, ≤4 KiB body | **480 µs** | 2 ms | headline, **measured**; was published as 200 µs, see below |
| `cold-auth` — auth cache miss, one store read | — | 15 ms | |
| `shared-redis` — clustered exact capacity | — | 5 ms | +1 RTT, deliberate |
| `external-auth` — delegated auth | — | governed by the callee | stated, not promised |
| `lua-enabled` — hooks active | — | +hook ceiling | ceiling is configured |
| `prefix-1MiB` / `prefix-16MiB` — large bodies | — | **55 ms / 852 ms** | **measured**; published as 6 ms / 60 ms, and misnamed — see below |

| Other | Target | Measured |
|---|---|---|
| Added TTFT, streaming | p99 < 1 ms | **p99 648 µs** — holds |
| Idle RSS, notebook profile | < 100 MB | **62 MB** including the load generator and a fake backend in the same process — holds |
| 1000 concurrent streams | < 300 MB **including** the replay budget (§15.4) | **256 MB** for three participants in one address space — holds, with the gateway's own share strictly less |
| Metering on vs off | **< 5% of the gateway-overhead budget** (see note below). Measured **+148 ns** steady, **+110 ns** at a full buffer = **0.031%** of the measured 480 µs warm-local p50 |  |

> **"5%" needed a denominator.** An earlier draft said only "metering on vs off < 5%", which never
> said 5% *of what*. Measured against a no-op meter the ratio is **7.6×** — but the no-op returns after
> one branch, so dividing by it measures nothing about a real request. The requirement is 5% of the
> **gateway-overhead budget**, so added-nanoseconds-against-that-budget is the figure that answers it.
> Both numbers are reported, and the gate asserts steady state **and** a full buffer — at which point
> metering is *cheaper* (110 ns), because a failed ring push skips the payload copy while the numeric
> path does identical work. That asymmetry is the two-queue split (§12.1) behaving as designed.
>
> The gate's absolute bound stayed at **10 µs** when the budget above was corrected from 200 µs to
> 480 µs. 5% of the corrected figure would be 24 µs, and a bound that loosens because the thing it
> is a fraction of got slower is a ratchet pointing the wrong way.

#### The 200 µs p50 was never measured, and it does not hold

`testing/perf` is the harness that measures it: the assembled gateway (`internal/app`, over a
real socket) against a fake backend with a known fixed delay, with the boundary above
instrumented per request rather than inferred. Gateway overhead is the time inside the handler
minus every nanosecond spent waiting for the upstream — measured as the time inside `RoundTrip`
with a connection in hand plus the time blocked in a `Read` of the upstream body. That
subtraction is per request, so a p99 means something; subtracting two *distributions*, or a
fixed sleep, would not survive a streaming relay whose work is interleaved with the upstream's.

Measured on a 16-core AMD Ryzen 7 8745HS, warm, prefix on, metering with traces at full
sampling, 100 pricing rules loaded, two deployments competing, four concurrent clients:

| request body | p50 | p90 | p99 |
|---|---|---|---|
| 256 B | 191 µs | 276 µs | 367 µs |
| 1 KiB | 248 µs | 337 µs | 457 µs |
| **4 KiB** (the profile's stated ceiling) | **492 µs** | 656 µs | **855 µs** |
| 16 KiB (outside the profile) | 1.27 ms | 1.65 ms | 2.03 ms |

So the published 200 µs held for a request of a few hundred bytes and was never true at the
4 KiB the profile names. The p99 of 2 ms holds with better than a factor of two, and the p99 is
the number an operator notices: a p50 is a story about the machine, a p99 is a story about a
client that timed out.

**Where it goes.** Every configurable feature is free: measured with each of prefix affinity,
trace metering, a 100-rule price catalog and a second deployment turned on and off in turn, no
arm moved the p50 outside the noise (341 µs bare, 344 µs with all four). The cost is the four
JSON passes a cross-protocol gateway makes over the body — decode the client's request into the
canonical form, encode the upstream's, decode the upstream's response, encode the client's —
and they are ~85% of the CPU the request path burns. `internal/wire/openai.DecodeRequest` alone
runs at **31 MB/s with 143 allocations per KiB**, because each wire type's `UnmarshalJSON`
parses its own bytes twice: once into the struct and once into a `map[string]json.RawMessage`
to split the unmodelled members out (`wirejson.SplitExtra`). That is the whole of the slope in
the table above, and it is where a p50 improvement would have to come from.

**The measurement depends on the offered rate, and the profile should say so.** The same gateway
doing the same work measured 259 µs at 1585 req/s, 347 µs at 558 req/s and 510 µs at 344 req/s
per client, because between sparse requests the caches go cold and the CPU drops clock. The
figures above are all at a stated concurrency and rate for that reason; a latency number quoted
without one is the kind of unfalsifiable number the top of this section refuses.

#### The large-body rows were wrong by an order of magnitude, and misnamed

| profile | published p99 | measured p99 |
|---|---|---|
| 256 KiB | — | 17.8 ms |
| `prefix-1MiB` | 6 ms | **55 ms** |
| `prefix-16MiB` | 60 ms | **852 ms** |

The name is the more useful defect. **The prefix chain is about 1% of these figures.** Its own
benchmark puts a 16 MiB chain at **7.1 ms and 728 B in 17 allocations**, running at 2.36 GB/s —
exactly the O(log n) digests over one hash pass §7.4b promises, and one of the few numbers in
this document that was measured before it was published. The other 99% is the codec. A profile
named after the cheap component invites precisely the wrong optimization, so the rows keep their
configuration names and this note says what is actually in them.

### 15.2 Techniques

1. **Immutable routing snapshot** — configuration swaps by pointer; the read path takes no lock.
2. **No allocation in the gate** — pooled request structures; only the fields needed
   (`model`, `stream`, size markers) are scanned before the request is routed, never a full
   unmarshal. `BenchmarkGateOnly` measures route lookup plus authentication plus authorization
   at **0 allocs/op**, and `internal/server/peek.go` is why.

   > **This says "gate", not "hot path", and the change is a correction.** It read "no
   > allocation on the hot path", which was never true of a request that gets dispatched.
   > Measured end to end (`testing/perf`, `BenchmarkGatewayRequest` minus its no-gateway
   > control) one 4 KiB non-streaming request allocates about **390 objects and 71 KB** — 16×
   > the request body. Essentially all of it is the four JSON passes of §15.1's note, and none
   > of it is in the gate. The claim was true about the code it was written about and false
   > about the sentence it was written in, which is the failure mode this document exists to
   > avoid.
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

#### Where the ceiling is, measured

`testing/perf`'s `TestThroughputCeiling` drives the assembled gateway against a fake backend
with a 5 ms think time and 4 KiB bodies, and reports where it stops scaling. On a 16-core
Ryzen 7 8745HS, with the load generator and the fake backend in the same process:

| concurrent clients | req/s | overhead p50 | overhead p99 |
|---|---|---|---|
| 1 | 155 | 711 µs | 1.13 ms |
| 16 | 2 419 | 793 µs | 1.45 ms |
| 64 | 10 138 | 407 µs | 1.24 ms |
| **128** | **19 607** | 423 µs | 3.21 ms |
| 256 | 20 417 | 424 µs | 5.99 ms |
| 512 | 19 606 | 435 µs | 18.50 ms |

Throughput is linear in offered concurrency to about 128 clients and flat after it. The knee is
sharper in the p99 than in the throughput, which is what an operator sees first: past it, extra
concurrency buys queueing and nothing else.

**What holds it there is CPU, and the CPU is JSON.** At the knee the three participants together
saturate the machine, and 85% of the request path's own cycles are the four codec passes of
§15.1. It is not a lock. Mutex profiles taken in steady state at the knee — after warm-up, so
the connection-pool and SQLite cold starts are excluded — put the largest identified contention
inside `capacity.Broker`, reached three or more times per request (`TryAcquire`, `Release`, and
once per candidate from `least_busy`), and even that is smaller than the Go allocator's heap
lock, which is itself a consequence of the 390 allocations per request rather than a cause.
None of `internal/prefix`'s table, `internal/meter`'s ring or the auth snapshot appears at all.

> **`least_busy` takes one turn at the broker's single mutex per candidate**, so the routing
> decision's lock traffic scales with the size of the model group —
> `BenchmarkRouteParallelByCandidates` measures 1.4 µs/op at one candidate, 1.9 at four and
> 3.8 at sixteen. Batching it into one acquisition was written, measured and **reverted**: both
> shapes were slower, because the staged query carries strings and writing it into a reused
> heap slice costs a GC write barrier per candidate, and because doubling the work done while a
> saturated mutex is *held* costs more than the turns it saves. The comment on that benchmark
> records both numbers. On the assembled gateway it is not the ceiling — throughput was flat at
> 19-20k req/s from one deployment to eight — because a real request spends ~400 µs elsewhere
> and touches that lock about 1% of the time.

**Footprint follows the allocation rate, not the concurrency.** At the knee the heap in use is
about 600 MB against 66 MB live after collection — Go's pacer doing its job against a 1.3 GB/s
allocation rate (20k req/s × 65 KB), not a leak. With a thousand streams held genuinely open
the resident set is 256 MB (§15.1). An operator who needs a smaller heap at high throughput
sets `GOGC` or `GOMEMLIMIT`; an operator who needs both wants the codec passes reduced.

> The first version of this measurement reported 2.7 GB, and it was the harness: `testing/fake`
> retains every request it has served — verbatim body, cloned header — which is what makes a
> scenario able to assert on what reached the wire, and which is a gigabyte of bookkeeping after
> a hundred thousand 4 KiB requests. It was being read as the gateway's. Every arm now resets the
> fakes before it starts and before it reports.

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
          pricing,meter,store,auth,admin,batch,cluster,config,luaext,notify,
          backend,mask,metrics,keyguard,probe,shadow,tokenest}
internal/app         the assembly (§1): the one place every package is named at once
pkg/catalog          provider defaults, model catalog, base pricing
ui/                  embedded admin SPA
deploy/              compose for tests, Dockerfile, examples
docs/                DESIGN, REVIEW, CONFIG, COMPATIBILITY (+ .ko)
testing/             fake upstreams, scenario harness, §15.3 workload, load harness (perf)
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

### 17.1 What the assembly milestone is for

M-assembly is not glue. Two of the defects found so far were invisible to every package's own
tests and could only appear when the parts were joined:

- **A feature wired to nothing.** Budget enforcement (R5) was specified, implemented, tested,
  and never called. Every unit test passed; the requirement was not met.
- **A reservation that named a different target than the dispatch.** Capacity reserved against
  one credential while the work went to another, because the chosen credential lived inside a
  reservation the executor never received. No assertion about either side could see it.

Both share a shape: **an interface satisfied on both ends and connected on neither**. A test
suite organized by package cannot detect that, because there is no package where the defect
lives. The round-trip test through the assembled stack — real router, real capacity, real
pricing, real metering, asserting the ledger row and the reserved axis — is the only thing
that does.

#### The four rules this section exists to state

The rest of §17.1 is a case list, and the cases are here to earn the rules. The rules are the
part that transfers, so they are stated first and each names the case that produced it.

1. **A harness may substitute a dependency, never a value the system under test is responsible
   for producing.** Where a harness fills something in, that is itself the assertion worth
   writing; if it cannot be written as an assertion, the harness is compensating for a gap
   rather than exercising a path. — *"The harness that hid the bug"*, from `Decision.PriorityTier`.
2. **A harness that reimplements its subject cannot fail, and the difference between what it
   does and what the subject does is a defect list.** Where the difference cannot be closed in
   the same change, it belongs in the suite as a characterization subtest that skips itself the
   day the real path catches up — not as a silently narrower assertion. — *"the harness's second
   dispatch path"*, which produced three such differences and closed two.
3. **For any control of the form "A disagrees with B", the first question is where A and B were
   each observed.** If the answer is "the same place, twice", the control is documentation. The
   fix is not to strengthen the comparison but to find the reading that can actually differ. —
   *"a control that is reached and can never fire"*, from §8.1's future-settlement clamp.
4. **A disposition is the same shape as the defect it disposes of.** "Closed" that names a file
   or a test is only true once that file or test exists on the branch it is claimed for — and
   the mirror holds: an **open** disposition needs the same evidence a closed one does. Five of
   this repository's six long-lived documents were asserting something the code had closed when
   the 2026-07-29 reconciliation began, and one asserted both at once within a single file. —
   result 1 below, and `docs/SECURITY-REVIEW.md`'s two correction sections.

**This is not a pair of anecdotes. It is the dominant defect class in this codebase**, and a
security review counted nine instances independently. Extracting the backend layer added four
more, all of the same shape — a value computed, stored, documented, and never read:

| Configured and never applied | Consequence |
|---|---|
| `providers[].timeout` | **no upstream attempt had a deadline** |
| `providers[].retry` | defaulted, validated, never read |
| an upstream `429`'s `Retry-After` | reached the cooldown but never the client, so §11.4's mandatory header could not fire |
| `Decision.PriorityTier` | §7.5's `service_tier` fold has been dead in production |

A later sweep closed twelve more and found eleven beyond them. Three results from it are worth
carrying here, because each says something about the class rather than about one setting:

1. **A closed finding was not closed.** The security review recorded the per-key rate limits as
   "Closed — patched + wired" and described a file, a field and five tests, none of which
   existed. A disposition is the same shape as the defect it disposes of — satisfied on paper,
   connected to nothing — and a closed finding is not re-checked. The rule: a disposition that
   names a file or a test is only closed once that file or test exists.

2. **Two of the twelve were already fixed and the documentation still said otherwise.** The
   metering-degraded gauge and the passthrough counter both existed; the prose describing them
   as absent was the only record, and prose does not fail a build. A claim about the code that
   nothing executes decays exactly like code that nothing calls.

3. **The guard that catches the class is narrow, and worth having anyway.**
   `TestEveryConfiguredFieldIsReadSomewhere` walks `config.Config` and requires every
   `yaml`-tagged field to be named somewhere outside `internal/config`. It cannot see a value
   that is read and then dropped — which is precisely how `PriorityTier` failed — and it is
   vacuous for short field names. It found eleven unwired settings on its first run regardless.
   The half it cannot cover has no automated guard: for a control that is not a configuration
   field, the only thing that finds it is an assembled-stack test asserting an observable the
   owning package cannot produce alone.

#### The harness that hid the bug

The last row deserves its own note. `PriorityTier` is computed by the router and dropped on the
way out — and the scenario harness **splices it back in itself**. So the scenario suite passes,
green, while the production path has never sent the field.

That is the mirror image of the lesson above: a package test can prove a check works while
nothing calls it, and an integration harness can supply the very value the real path fails to.
Both produce a passing suite over a broken system.

The rule that follows: **a test harness may substitute a dependency, but never a value the
system under test is responsible for producing.** Where a harness fills something in, that is
itself the assertion worth writing — and if it cannot be written as an assertion, the harness
is compensating for a gap rather than exercising a path.

Consequence for the milestone gates in §17: a package is not done when its tests pass. It is
done when something end to end exercises it and asserts an observable outside it.

#### Applying the rule: the harness's second dispatch path

`PriorityTier` was one spliced field. The same harness also held its own `convertResponse`, its
own `relayStream`, its own request encoder and its own priority splice — a complete second
dispatch path, the third instance of the pattern after `internal/backend`'s zero-importer copy
and the harness's own `len/3+16` token estimator, which carried the identical `bytes/3` defect
that was refusing multimodal traffic in production.

The harness now calls `internal/backend` for all of it. What that changed is measurable rather
than aesthetic. Four one-line defects introduced into the production conversion path — the
response no longer carrying the caller's model name (§7.2), the per-engine priority splice
negated (§7.5), the `service_tier` fold dropped, and the streaming relay restamping the wrong
name — left the old scenario suite **completely green**. Every one of them now fails a named
scenario.

Removing the substitution also exposed three things the harness could do that production could
not. Each was a passing assertion that proved nothing. **Two are closed; the third is the
oldest thing still open on this page.**

| The harness did | Production did not | Consequence | Status |
|---|---|---|---|
| scan a relayed stream for an in-band `"error"` frame and report a failed outcome | `backend.relay` copied it through and returned nil | the failure reached neither `internal/health` nor §7.6's committed-stream boundary; a backend that failed every stream after the first frame kept its full share of traffic | **closed** — `errorFrame`/`errorMember` in `internal/backend/stream.go`, applied on the relay scan and again on the terminal check, with `internal/backend/streamfail_test.go` |
| surface `*anthropic.OpaqueError` to the caller | `backend.encodeError` flattened it to a message string under `conversion_failed`, with no `Unwrap` | the `Construct` id that `x-dorang-allow-lossy` takes never reached the caller, so §10.1's "refuse, and say what to opt into" was prose only | **closed** — `internal/backend/errors.go` matches `*anthropic.OpaqueError` with `errors.As` rather than flattening it |
| read `x-ratelimit-reset-requests` into `Outcome.ResetAt` | **nothing sets `Outcome.ResetAt` from an upstream at all** | an upstream 429 that signals its reset with a rate-limit header and no `Retry-After` reaches neither the deployment cooldown nor the client's `Retry-After` | **open**, re-verified at `ef94f58`. `router.Outcome.ResetAt` has no producer anywhere in the tree, test or otherwise, because `backend.Result` carries no reset instant for `backendResult` to assign — `retryAfter()` in `internal/backend/errors.go` reads `Retry-After` and nothing else. **Two corrections to the disposition itself**, both found by re-deriving it rather than reading it: (1) the row understated the gap by naming only `Outcome`. The client-facing half is `upstreamError`, which fills `server.Error.RetryAfterSeconds` from `Retry-After` alone, so §11.4's header is the thing actually missing on a reset-header-only 429; (2) the row overstated the consumer chain. `st.resetAt` feeds the four terminal errors in `canFallBack`, and **`canFallBack` runs only when `req.Previous != nil`** — which in `internal/app`'s dispatch loop is exactly the case where `lastErr != nil` and the router's error is discarded in favour of the upstream's (`dispatch.go`, and the comment there defends it: "a hop that cannot be taken reports the failure that made us look for one"). So those four assignments are unreachable from the assembled gateway even once the field has a producer, and a fix that stops at `Outcome` closes nothing. What remains reachable and worth having: the `CauseQuotaExhausted` cooldown in `Router.Report`, and `Retry-After` on the client's response. **The change is therefore in `internal/backend`**: parse the reset headers beside `Retry-After` into a `Result.ResetAt`, prefer `Retry-After` when both are present, fill `RetryAfterSeconds` from the reset instant when it is not, and assign the field in `backendResult`. **The test that closes it must drive a 429 carrying only a reset header and read `Retry-After` off the client's response** — asserting on `Outcome` would pass throughout the defect, which is rule 1 above applied to its own fix |

One duplication survives, and it is worth naming so it is not mistaken for a clean result.
`internal/app`'s `backendResult` and `backendCause` — the mapping from a `backend.Result` onto
a `router.Outcome` — are unexported, so the harness holds a mirror of them. Lifting that pair
into `internal/backend`, or exporting it, would leave the harness with no copy of anything at
all. Until then, the mirror is the last place the two can drift; the residual risk is bounded
because the harness's copy classifies only what §10.5a documents as the frontend's job.

The generalization: **a harness that reimplements the subject cannot fail, and the difference
between what it does and what the subject does is a defect list.** Where the difference cannot
be closed in the same change, it belongs in the suite as a characterization subtest that skips
itself the day the real path catches up — not as a silently narrower assertion.

#### Where the class shows up next: the accounting surface

A cutover reproduction against a live incumbent found the same shape twice more, both in the
ledger and neither reachable from any request table:

| Column | Had | Lacked |
|---|---|---|
| `request_logs.deployment_id` | a writer, a reader, a JSON name on `/spend/logs`, and the value on `x-dorang-deployment` | anything that assigned it |
| `request_logs.streamed` | a writer, a reader, a JSON name | anything that assigned it |

Per-deployment attribution was therefore *in the response header and not in the row* — the worst
of the two arrangements, because an operator who checks the header believes it was recorded.
Both are wired now, and both are asserted against the response rather than against a
constructor: `TestLedgerTotalTokensEqualsTheAnswerTheClientGot` compares `deployment_id` to the
header on the same request, and `TestStreamedRequestIsRecordedAsStreamed` drives a turn the
client reads as an event stream.

**The same reproduction found the class's sharper form: not a value nobody reads, but one value
computed two ways.** `meter.Tokens.Total` summed all five token dimensions while
`canonical.Usage.TotalTokens` — the wire — is input plus output, because the cache counts are
part of the input and the reasoning count is part of the output. Both functions were internally
consistent and each had passing tests; what disagreed was dorang's answer against dorang's bill.
A request whose body said `"total_tokens": 120` was recorded as 128, and a live session's rollup
read 30,929 against 30,355 on the wire. Cost was unaffected only because no `reasoning` rate
happened to be configured — a property of one catalog, not of the code.

Two rules follow, and they are the reason this sits beside the wired-to-nothing entries rather
than under a heading of its own:

1. **A number the client is given and a number the operator is billed for the same request are
   one value, and must be produced by one expression.** There is now one: `Tokens.Total` is the
   same function `canonical.Usage.TotalTokens` is, and `server.Usage` carries the same
   convention on the relay path — `scanUsage` normalizes the Anthropic family's cache-exclusive
   `input_tokens` into dorang's inclusive one, so a passthrough row is not a third definition.
2. **The assertion has to compare the two artifacts, not the two functions.** A test that
   `Total()` returns a particular sum passes throughout this defect. The test that does not is
   the one that reads `usage.total_tokens` out of the response body and compares it to the
   ledger row for that request id — which is what the named test above does, with a fixture
   carrying cache and reasoning counts precisely so the two rules give different answers.

#### The variant: a control that is reached and can never fire

The next cutover reproduction found three more of the wired-to-nothing shape on the same
surface — `request_logs.subscription_cost_nano` had a column, a JSON name on `/spend/logs` and
no producer *and the same conflation one materialization up*, where `/global/spend/report`
answered `marginal_spend` from `cost_nano`; `/key/info` rendered `spend` from a column nothing
on the request path writes; a budget hold that takes no reservation reported an unhydrated `0`
as a measurement — and then a fourth that is worth separating, because it is not a value nobody
reads.

**§8.1's future-settlement clamp was reached on every settlement and its condition could never
be true.** It compares the row's stamp against the catalog's clock, and internal/app supplies
both from `a.now` — the stamp first, the catalog's reading after. The rule was written for an
NTP step, a VM resume, a bad RTC and a fast node in a cluster; the first three move both
readings together, and the fourth never reached this process at all because the accumulator was
in memory. Re-driven with the clocks coupled the way the app couples them, the defect the clamp
was written for reproduced to the cent — a stray 50.00 USD, a July attributing 10.00 USD of
100.00 USD, an August opening at 49.00 USD — while the clamp did nothing.

**Closed at `8e6016d`, and closed by moving the observation rather than by strengthening the
comparison.** `pricing.Catalog.RestoreState` adopts a `SubscriptionState` written by an earlier
process, and *there* the comparison is real: `now` is this process's reading of the present and
`PeriodStart` was stamped by whatever process wrote the row — possibly on another host with
another clock, which is what makes a disagreement possible at all. A stored period that has not
begun yet is re-dated to the open one rather than dropped, so the attributed total stays a bound
and the next period still opens at zero. `Catalog.Settle`'s own clamp is left in place and its
doc comment now says outright that it cannot fire in a default deployment — a control that is
documented as documentation is not the same defect as one that is documented as a guard. Pinned
by `internal/pricing/processboundary_test.go`, which drives whole processes each with exactly
one clock.

Three things follow, and the third is the one that generalizes:

1. A guard whose inputs come from one source is not a guard. **Two readings of one clock, taken
   in order, cannot disagree**, and no amount of testing at the call site changes that.
2. **A fixture that injects the two inputs independently proves the comparison, not the
   deployment.** The original test set the catalog's clock and the row's stamp separately, which
   is a thing no gateway process does; it passed against a tautology. The replacement drives
   whole processes, each with exactly one clock, which is the arrangement `internal/app` has.
3. The generalization: **for any control of the form "A disagrees with B", the reviewer's first
   question is where A and B were each observed.** If the answer is "the same place, twice", the
   control is documentation. The fix is not to strengthen the comparison but to find the reading
   that can actually differ — here, a stamp that crossed a process boundary, which only exists
   because §8.1's accumulator was made durable in the first place.

#### The reclassification: unreachable by decision is not the same finding

This list is only worth reading if every entry on it is real, so one entry is taken off it.

`internal/wire/openai/responses.go`'s **upstream** half is complete, tested, and — at `8e6016d`,
where this was checked — had no non-test caller: `DecodeResponsesResponse`,
`ResponsesResponseToCanonical`, `MarshalResponsesRequest`, `EncodeResponsesRequest`,
`DecodeResponsesRequest` and `ResponsesItemsToMessages`. Counted by shape alone that is the
ninth instance of the wired-to-nothing pattern, and it was recorded as one.

**It is not one.** `internal/backend/openai.go` routes `/v1/responses` to the **chat** endpoint
deliberately, and says why: `/v1/responses` is served by a strict subset of the deployments
`/v1/chat/completions` is. `OpChat` and `OpResponses` encode alike as a chat request, decode
alike as a chat response, and `internal/backend/t1.go` re-renders the answer with
`MarshalResponsesResponse`. So dorang's client-facing Responses surface **works**; what has no
caller is the path that would address an upstream speaking Responses *natively*, and nothing in
the shipped catalog asks for one.

The disposition matters more than the count. A reader who greps for these symbols finds the
routing comment, concludes the ledger is wrong, and stops trusting the rest of it — which is the
same failure as a stale "closed". **A control with no caller and a written reason is a bounded
decision; a control with no caller and no reason is a defect.** The test that tells them apart
is whether removing the code would change any behaviour a configuration can reach.

The residual risk is stated rather than dismissed: `ResponsesResponseToCanonical` is the one
decoder that knows all three families' `input_tokens` spellings, and at `8e6016d` nothing
exercised it against a live convention — unvalidated code on the money path the day an upstream
is pointed at natively.

**That residual is the half that is being closed, and by the argument above rather than by the
count.** The decode path now selects on the *answer's own shape* — `object: "response"` or an
`output` array, with a `choices` array refusing — because which shape comes back is the host's
choice and not dorang's: a host serving both APIs on one base URL may answer either, and a
translating proxy answers in a third combination. So `DecodeResponsesResponse` is reached, and
`ResponsesResponseToCanonical` with it. The other four — `MarshalResponsesRequest`,
`EncodeResponsesRequest`, `DecodeResponsesRequest`, `ResponsesItemsToMessages` — are the
*request* direction and remain unreached by the same decision, which is still written down in
`internal/backend/openai.go`. **Two of six moved for a stated reason and four did not**, which
is the disposition this entry was reclassified to hold.

## 18. Open risks

**This table is the authoritative status of every W-numbered risk.** Where another document
disagrees with it, this one is right and the other is stale — `docs/CONFIG.md` §8.1 asserted W8
open for a full day after `039c0b6` closed it, and `docs/DESIGN.ko.md` §18 was missing W8
through W11 entirely, which is four closed risks a Korean reader could not see. Rows are in
numeric order for exactly that reason: an out-of-order register is one a mirror can silently
truncate.

**Open: W5 and W7.** Everything else is closed or mitigated.

| # | Risk | Status |
|---|---|---|
| W1 | Cluster + local capacity mode | **closed** — refuses to start (§5.6) |
| W2 | Credential import premise | **closed** — verified; motivation corrected (§2.4, REVIEW R1-A) |
| W3 | Dependence on an upstream batch endpoint | **closed** — own scheduler (§11.1) |
| W4 | Prefix computation cost | **mitigated** — byte-based, streaming; M7 gate proves it |
| W5 | Cross-protocol conversion loss | **open** — enumerated in headers; may need per-pair fidelity tests beyond golden |
| W6 | Single-mutex broker throughput | **mitigated** — M2 gate decides; sharding invariant defined (§5.7) |
| W7 | Sticky/prefix hit-rate dilution across nodes | **open** — documented; consistent hashing recommended, Redis sharing available |
| W8 | **Multi-axis waiter starvation under sustained saturation** (§5.4 correction 5) | **closed** — soft reservation as a prefix-ordered claim on one unit per axis key; deadlock excluded by the §5.7 axis order; the oldest waiter is served within `SoftReserveAfter + axes` releases. Measured +5.3% on the mixed contended benchmark, nil on single-axis. On by default. ⚠️ The off switch is `capacity.Config.SoftReservations`, a Go option an embedder sets — **`internal/app` sets neither it nor `SoftReserveAfter`, so no YAML reaches either.** This row read "with an off switch" without that qualification, which is §17.1's own class at low stakes: a control with no path from configuration. The behaviour an operator gets is the default, and CONFIG §8.1 now says so |
| W9 | **Quota and budget state is in-memory only** (§9.6) | **closed** — the request path reserves against the durable ledger. The gate holds an upper bound before every upstream call, settles with the actual cost, and refuses an exhausted budget as a terminal `400` (§6.4); a restart re-reads the counter rather than starting the period over, and a graceful stop returns the unspent part so a planned restart costs nothing. The hold is taken after the routing decision rather than at the gate, because §6.4's estimate prices output at `max_tokens` and there is no price before a deployment is chosen — the property that mattered (concurrent requests cannot both see the pre-spend balance, and anything that never reaches an upstream is refunded in full) is unaffected. No in-memory path is kept beside it: `quota.Budget` serializes on one mutex where the ledger takes an atomic compare-and-swap on a block it already holds, so the durable path is also the cheaper one. Was **mechanism closed** — durable leased blocks measured at 400 requests to 5 store writes; a crash can only under-spend, and the leader returns the unspent part. Was **open** — a restart resets the windows |
| W10 | **Case-insensitive JSON decode diverges from the case-sensitive gate** (COMPATIBILITY 2.0) | **closed** — strict type-directed filtering in every request decode path, plus three further gate defects found while closing it: an escaped duplicate key that authorized one model and dispatched another (fail-open), a case-insensitive fallback inside the gate itself, and a stream flag that was OR-ed rather than assigned. A differential fuzzer (13.7M execs) and a mirror test that fails when the gate drifts now hold the two sides together. Was **open** — `encoding/json` fills a tagged field from a differently-cased key while the scanner does not, so a request can be authorized as one thing and dispatched as another. Needs a case-sensitive decode path in every wire adapter plus a differential test against the gate. Security-relevant: it is an allow-list bypass, not merely an inconsistency |
| W11 | **Revocation latency was never specified** (§11.2c) | **closed** — revocations, pends and early grace cuts publish a durable invalidation; every node drops the key from its snapshot on receipt, and the TTL is now the fallback for a node that missed the message rather than the mechanism. The worst case is published as a number (`auth.RevocationBound`): **0 on one node**, **`poll + store_latency` clustered** — 1.25 s at the defaults — with `entry_ttl` as the stated fallback and an unmeasured propagation delay publishing the TTL rather than an optimistic figure. Four further defects were found while closing it: a **found-but-refusing row was cached for the SERVING lifetime**, so a pended key was re-checked a full `entry_ttl` later on a node that missed the message (rule 3 was written and not implemented); `Rejoin` had to drop the snapshot **before** the reload rather than replace it after, or a failed reload left the stale set serving — the exact state rule 4 exists to prevent; a rejoining node had to read the invalidation watermark **before** the reload, or a message published during it was skipped; and **a row placed by a bulk load never expires** (§9.1), so publishing `entry_ttl` as its fallback was simply wrong — its fallback is the reload interval, and with no reload scheduled it has none, which the figure now says rather than reporting a number that does not apply. Measured end to end: **zero window** single-node, **~21 ms** to the last of four nodes at `poll: 20ms`. Was **open** — the auth snapshot's TTL is what makes authentication 354 ns, and it also means a revoked, pended, or rotation-cut key keeps serving until the snapshot refreshes, per node |
