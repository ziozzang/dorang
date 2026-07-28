# LiteLLM parity, and the cutover

## Verdict — 2026-07-29, `94de856`

**Still not — but the reason has changed, and it is now a smaller surface with a larger number
behind it: all five defects that blocked the last verdict are fixed and verified against the wire,
and what blocks this one is that the `pricing:` block the cutover checklist tells an operator to
write bills the cached prefix and the reasoning tokens twice — 27% over on DESIGN §8.5's own
example rates, several-fold on the cache-heavy agentic traffic this gateway exists to aggregate —
while a priced streamed request publishes `x-litellm-response-cost: 0` and records the real cost in
the ledger.**

The token column is right now. The money column is not, and money is the column a replacement is
judged on. `Tokens.Total()` was fixed and the same subset-added-again rule survives in two places
the sweep did not reach: `internal/pricing`'s component quantities, and the token guard's feed.

What closed, and what it took:

| | Was | Is |
|---|---|---|
| C1 ledger `total_tokens` | 120 on the wire, 128 in the ledger; 30,929 against 30,355 over a session | **120/120, 146/146, 133/133, 38,416/38,416** — measured against the response body of the same request |
| C2 unfiltered `/spend/logs` | `500 internal_error`, message discarded | **`501 not_implemented`**, naming the four working filters |
| A1 `server.max_body_bytes` | refusal named a knob that did not exist | **configurable**; a 1 MiB cap refuses at 1,048,576 and passes 512 KiB |
| C3/C4 `deployment_id`, `streamed` | columns with no producer | **both populated**, on the mock and on five live agent turns |
| C5 cost mirror | absent on an unpriced request; `x-dorang-spend-usd` flat $0.00 | **`0` and always present**; `x-dorang-cost-usd` still absent, and the pair discriminates. Spend now moves: `$0.0000493` → `$0.0002099` |

What blocks, in one line each — §D has the measurements:

1. **`pricing` charges `input` against the whole inclusive input and `cache_read` against the cached
   part of it again**, and `output` against the whole output and `reasoning` against its subset
   again. DESIGN §10.7 warned about exactly this shape — *"a mis-mapped cache field does not produce
   a visible error, it produces a wrong invoice"* — and the invoice is in `internal/pricing`.
2. **A priced request that streams publishes `x-litellm-response-cost: 0`** while its ledger row
   records the real cost, because headers are stamped before usage is known. Every agent turn
   streams. Before C5's fix the header was absent and asserted nothing.
3. **The token guard is still fed `Input + Output + Reasoning`** — 128 for the request whose own
   answer said 120, which is the exact pair C1 was named for.

Neither the request table nor the cutover can see any of the three. That is the same sentence the
last verdict ended on, and it is still the finding that matters most about this harness.

---

## Verdict history

Superseded verdicts are kept rather than overwritten. A record that shows only the current state
cannot be audited.

### 2026-07-29, `e515a48` — superseded

> **dorang can serve this LiteLLM deployment's clients today — a real agent client ran the same
> multi-turn, tool-calling, streaming task through dorang and through LiteLLM and could not tell
> them apart — but it cannot yet *replace* the deployment for anyone who reads the numbers:
> dorang's own ledger records a `total_tokens` that disagrees with the `total_tokens` it just sent
> the client, records no cost at all under the configuration in this directory, and answers an
> unfiltered `/spend/logs` with `500 internal_error`.**
>
> Everything standing in the way is in §C, ranked at the end. None of it is on the request path,
> and none of it was reachable from the request table.

Its five blockers were C1, A1, C2, C5 and C3/C4. All five were fixed at `15cf9ae` and `94de856`,
and all five are re-verified in §C below at the level they were reported — off the wire and out of
the ledger, not by reading the commits.

---

## What is in this directory

| File | What it is |
|---|---|
| `dorang.yaml.tmpl` | The 39-model LiteLLM surface reproduced on dorang **as a front proxy**, with that same LiteLLM as the single upstream. One upstream behind both gateways, so every difference the harness reports is the gateway's |
| `dorang-direct.yaml.tmpl` | The cutover shape — dorang against the real providers. It loads; it is **not validated against the live surface**. See gap G3 |
| `parity.py` | The differential harness: one request to each gateway per case, structural comparison of the two answers |
| `run.sh` | Renders the config, starts dorang on a scratch state directory, runs the harness, stops dorang |
| `jikjicode.yaml.tmpl`, `cutover.sh` | §B — a real agent client, pointed once at dorang and once at LiteLLM, doing the same task |

Both runners read `LITELLM_OPENAI_BASE_URL` and `LITELLM_OPENAI_API_KEY` from the operator's
environment file. Nothing either one writes contains a credential: the rendered configs still
reference keys by `key_env:`, and only base URLs are substituted.

### What the harness measures, and what it cannot

It compares **shape**, not text: field presence, JSON types, SSE framing, usage invariants, error
envelopes. Two calls to one model differ by nature, so nothing here compares generated bytes. A
case is `PASS` only when the statuses match and the structural diff is empty.

It cannot see a conversation. That is what §B exists for. It cannot see an invoice either, and that
is what §C and §D exist for — **every finding in this report that blocks a cutover was invisible to
both.**

---

# §A — The request table, re-measured at `94de856`

Measured **2026-07-29** against `94de856`, binary built from a clean tree at that commit.
`go build`, `go vet` and `gofmt` clean; `go test -race -count=1 ./...` exit 0, 35 packages `ok`,
no `FAIL` and no `DATA RACE`. LiteLLM `1.93.0`.

```
                     40 cases
before (87cbe21)     PASS 13   DIFF 27   FAIL 0     58 divergences, 21 at high severity
prev   (e515a48)     PASS 13   DIFF 27   FAIL 0     41 divergences, 18 at high severity
now    (94de856)     PASS 13   DIFF 27   FAIL 0     41 divergences, 18 at high severity
```

**Nothing moved.** Not one verdict, not one divergence count, not one path. The single row that
differs from the previous run is `nim:deepseek-v4-flash [stream]`, where **LiteLLM itself** went
`200` → `500` — the flaky NIM upstream of A9 again, this time refusing the incumbent's call
instead of dorang's, which leaves the row status-identical rather than split.

That is the expected result and it is worth saying rather than burying: **none of the five fixes is
on the request path**, so a request table cannot register them. The table's job here was to prove
that five accounting changes landed without disturbing the surface, and it did.

The column below is `94de856`. The `before`/`after` attribution of the previous pass — 17
divergences closed by the eight fixes measured at `e515a48`, and the six of those eight the request
table could not see — is unchanged and is not repeated.

| Group | Case | LiteLLM | dorang | Verdict | Diffs |
|---|---|---:|---:|---|---:|
| models | `GET /models` | 200 | 200 | DIFF | 3 |
| models | `GET /models/{id} (colon id)` | 403 | 200 | DIFF | 5 |
| chat | `POST /chat/completions gpt-oss:20b` | 200 | 200 | PASS | 0 |
| chat | `POST /chat/completions glm-5` | 500 | 500 | DIFF | 1 |
| chat | `POST /chat/completions zai:glm-5-turbo` | 502 | 502 | DIFF | 1 |
| chat | `POST /chat/completions deepseek-v3.2` | 500 | 500 | DIFF | 1 |
| chat | `POST /chat/completions nim:deepseek-v4-flash` | 200 | 200 | DIFF | 1 |
| chat | `POST /chat/completions gemini-3-flash-preview` | 500 | 500 | DIFF | 1 |
| chat | `POST /chat/completions kimi-k2.5` | 200 | 200 | PASS | 0 |
| chat | `POST /chat/completions minimax-m2.7` | 200 | 200 | PASS | 0 |
| chat | `POST /chat/completions qwen3-coder-next` | 500 | 500 | DIFF | 1 |
| chat | `POST /chat/completions local` | 200 | 200 | DIFF | 1 |
| chat-stream | `POST /chat/completions gpt-oss:20b [stream]` | 200 | 200 | PASS | 0 |
| chat-stream | `POST /chat/completions glm-5 [stream]` | 500 | 500 | DIFF | 1 |
| chat-stream | `POST /chat/completions zai:glm-5-turbo [stream]` | 502 | 502 | DIFF | 1 |
| chat-stream | `POST /chat/completions deepseek-v3.2 [stream]` | 500 | 500 | DIFF | 1 |
| chat-stream | `POST /chat/completions nim:deepseek-v4-flash [stream]` | 500 | 500 | DIFF | 1 |
| chat-stream | `POST /chat/completions gemini-3-flash-preview [stream]` | 500 | 500 | DIFF | 1 |
| chat-stream | `POST /chat/completions kimi-k2.5 [stream]` | 200 | 200 | PASS | 0 |
| chat-stream | `POST /chat/completions minimax-m2.7 [stream]` | 200 | 200 | PASS | 0 |
| chat-stream | `POST /chat/completions qwen3-coder-next [stream]` | 500 | 500 | DIFF | 1 |
| chat-stream | `POST /chat/completions local [stream]` | 200 | 200 | PASS | 0 |
| embeddings | `POST /embeddings jina-embeddings-v5-text-nano` | 200 | 200 | PASS | 0 |
| embeddings | `POST /embeddings embeddinggemma-300m` | 200 | 200 | PASS | 0 |
| embeddings | `POST /embeddings pplx-embed-v1-0.6b` | 200 | 200 | PASS | 0 |
| embeddings | `POST /embeddings llama-nemotron-embed-vl-1b-v2` | 200 | 200 | PASS | 0 |
| rerank | `POST /rerank jina-reranker-v3` | 200 | 200 | DIFF | 2 |
| audio | `POST /v1/audio/speech tts-1` | 500 | 500 | DIFF | 2 |
| audio | `POST /v1/audio/transcriptions whisper-1` | 200 | 200 | DIFF | 1 |
| images | `POST /v1/images/generations (argument error)` | 500 | 400 | DIFF | 0 |
| messages | `POST /v1/messages gpt-oss:20b` | 200 | 200 | DIFF | 3 |
| errors | `unknown model (chat)` | 400 | 404 | DIFF | 2 |
| errors | `unknown model (embeddings)` | 400 | 404 | DIFF | 2 |
| errors | `bad api key` | 401 | 401 | DIFF | 1 |
| errors | `no auth header` | 401 | 401 | DIFF | 1 |
| errors | `bad api key on GET /models` | 401 | 401 | DIFF | 1 |
| errors | `malformed JSON body` | 400 | 400 | DIFF | 1 |
| errors | `unknown parameter` | 200 | 200 | PASS | 0 |
| errors | `auth via x-api-key (not Authorization)` | 200 | 200 | PASS | 0 |
| errors | `oversized body (33 MiB)` | 400 | 413 | DIFF | 4 |

## §A-remaining — what is still different, ranked by whether it breaks a real client

**LiteLLM's own upstream failures are parity evidence, not defects.** Five of the ten sampled chat
models fail on LiteLLM *directly*: `glm-5`, `deepseek-v3.2`, `gemini-3-flash-preview` and
`qwen3-coder-next` answer `500` carrying `"<model> was retired at 2026-07-15"` from the backend, and
`zai:glm-5-turbo` answers `502`. dorang reproduces every one with the same status. Ten of the 27
DIFFs are those five models × (streaming, non-streaming), and in each the only divergence is the
error *envelope*, which is A3.

### A1 — 32 MiB body cap: LiteLLM `400`, dorang `413` — **the cap is now configurable**

The half of this that could break a working client is closed. `server.max_body_bytes` exists, is
wired through `internal/app`'s `server.Options` literal, and is honoured. Measured with a
deliberately small cap so the *configured* value is what is being tested rather than the default:

```
server: {max_body_bytes: 1MiB}

2 MiB body   → 413  {"error":{"message":"request body exceeds max_body_bytes (1048576 bytes)", …}}
512 KiB body → 200
```

The refusal now names a knob that exists, and reports the operator's number rather than the
built-in one. `TestConfigBodyCapDefaultMatchesTheServers` pins the duplicated 32 MiB default across
`internal/config` and `internal/server`, which is the only way those two can disagree.

**What remains is the status.** dorang refuses over-cap with `413 request_too_large`; LiteLLM has no
cap and forwards the 33 MiB body to its router, which rejects it for an unrelated reason with `400`.
That is COMPATIBILITY §11.2 and is defensible — and it is now a difference an operator can route
around by raising the cap, rather than one requiring a rebuild.

### A2 — Unknown model: LiteLLM `400`, dorang `404`

Two rows (`/chat/completions`, `/embeddings`).

```
litellm  400  {"error":{"message":"…Invalid model name passed in model=…",
                        "type":"None","param":"None","code":"400", …}}
dorang   404  {"error":{"message":"no model group or alias named …",
                        "type":"invalid_request_error","param":"model","code":"model_not_found"}}
```

Deliberate and documented: COMPATIBILITY §11.2 puts unknown-model at `404` / `model_not_found`,
which is the vendor's own answer. It is the status change most likely to be *noticed* during a
migration, because a mistyped model name is the commonest client-side error and an SDK's
`NotFoundError` branch is not its `BadRequestError` branch. One line in a client, and dorang is the
one matching OpenAI.

### A3 — The error envelope: nine rows

| | LiteLLM | dorang |
|---|---|---|
| upstream 5xx | `"type": null` | `"type": "api_error"` |
| unknown model | `"type": "None"` (the four-character *string*) | `"type": "invalid_request_error"` |
| bad key | `"type":"token_not_found_in_db"`, `"param":"key"` | `"type":"authentication_error"`, `"param":null` |
| no auth header | `"type":"auth_error"`, `"param":"None"` | `"type":"authentication_error"`, `"param":null` |
| malformed JSON | `"param":"request_body"` | `"param":null` |

COMPATIBILITY §7.1 requires `type` to be a string and `param` to be the offending field or null.
LiteLLM emits JSON `null` for `type` on upstream faults and Python's `str(None)` on others.
**No client can have had a working branch on that**, which is why these rows are low risk despite
the harness scoring them `high` — it flags null-versus-concrete because a client reading a member
off a null object raises, and here it is dorang that supplies the concrete value.

The status is identical in every one of the nine, so a client branching on status sees nothing.

dorang **forwards the upstream's status** rather than folding every upstream fault into `502` —
`500` stays `500`, `502` stays `502`. That is what keeps these nine rows status-identical to
LiteLLM, and `15cf9ae` corrected §11.2's "Upstream 5xx after fallback → 502" row to say so: folding
a provider's retired-model `500` into a `502` tells a client dorang failed when the provider did.
`TestUpstream5xxKeepsItsOwnStatus` pins it.

### A4 — `GET /models` loses `max_input_tokens` / `max_output_tokens`; `owned_by` changes

LiteLLM emits `max_input_tokens` and `max_output_tokens` on **4 of its 39** entries; dorang emits
neither, because the front-proxy config declares no context window and dorang will not invent one.
`owned_by` goes from `"openai"` to `"dorang"` on all 39.

Every client calls this endpoint first, and a model picker that renders a context-window size gets
an absent key rather than an error. **Silent, medium risk, and fixable in configuration** by
declaring `context_window` on the models that have one.

### A5 — `/v1/audio/transcriptions`: `usage` present-and-null becomes absent

```
litellm  {"text":"","usage":null}
dorang   {"text":""}
```

`resp["usage"]` raises on dorang where it returned `None` on LiteLLM. Low — almost every client uses
`.get()` — but it is a real `KeyError` waiting in a strict one.

### A6 — `GET /models/{id}`: LiteLLM `403`, dorang `200`

The LiteLLM virtual key is not permitted that route at all: *"Virtual key is not allowed to call
this route. Only allowed to call routes: ['llm_api_routes']"*. dorang serves it. **dorang is
strictly more capable**; nothing breaks. It is in the table because a status changed.

### A7 — `POST /v1/images/generations` with `prompt` omitted: LiteLLM `500`, dorang `400`

LiteLLM leaks a Python signature as a `500`: `Router.aimage_generation() missing 1 required
positional argument: 'prompt'`. dorang answers `400 invalid_request_error` with `"param":"prompt"`.
**dorang's is the actionable one.** The mechanism is not observable from outside — dorang's message
is its canonical *4xx* wording, so dorang's own upstream call answered 4xx where the harness's
direct call got a 5xx — and that is recorded rather than guessed at.

### A8 — `/v1/messages`: three differences, one of them a `compat:` default

```
litellm  content: [{"type":"thinking","thinking":"…","signature":null},{"type":"text","text":""}]
         usage:   {"input_tokens":68,"output_tokens":8}
dorang   content: [{"type":"thinking","thinking":"…"}]
         usage:   {"input_tokens":68,"output_tokens":8,"total_tokens":76}
```

- **The empty text block.** LiteLLM appends `{"type":"text","text":""}`; dorang does not. An
  Anthropic client reading `content[-1].text` raises on dorang. It is an artifact of `max_tokens: 8`
  — every token went to reasoning — so it is confined to turns truncated before any text existed.
  dorang matches the vendor here and LiteLLM does not; the risk is a client built against LiteLLM's
  habit.
- **`signature: null`** on the thinking block: LiteLLM emits the member, dorang omits it.
- **`usage.total_tokens`** is dorang's, from `compat.anthropic_total_tokens` defaulting to `true`.
  COMPATIBILITY §6.8 justifies that default as reproducing "the reference implementation's non-spec
  `usage.total_tokens`" — **and this reference implementation does not emit it.** For byte-parity
  with *this* deployment the setting wants `false`, and §6.8's claim should be narrowed to the
  version it was observed on.

### A9 — `nim:deepseek-v4-flash`: the row that changes between runs, and never because of dorang

Three runs, three different answers on this model's `[stream]` case, all of them the upstream's:

| Run | LiteLLM | dorang |
|---|---:|---:|
| `87cbe21` | timeout | 200 |
| `e515a48` | 200 | 500 (`ResourceExhausted: Worker local total request limit reached (651/48)`) |
| `94de856` | 500 | 500 |

**Flaky upstream, faithfully surfaced** — the non-streaming case for the same model answered `200`
on both gateways in this run. This model also takes ~5 minutes to answer an 8-token request, on both
gateways.

### A10 — additive members, no risk

`rerank` gains `model` and `meta.billed_units.search_units: 0`; the `audio/speech` error gains `code`
and `param`; LiteLLM's own `provider_specific_fields` extension is not reproduced.

---

## Skip list — what the 40 cases do and do not touch

The harness samples across **provider families**, not across model ids. 39 models, 40 cases:

| Class | Exercised | Named and skipped |
|---|---|---|
| chat (28) | `gpt-oss:20b`, `glm-5`, `zai:glm-5-turbo`, `deepseek-v3.2`, `nim:deepseek-v4-flash`, `gemini-3-flash-preview`, `kimi-k2.5`, `minimax-m2.7`, `qwen3-coder-next`, `local` — each non-streaming and streaming | the other 18: `deepseek-v4-flash`, `deepseek-v4-pro`, `gemma4:31b`, `glm-4.7`, `glm-5.1`, `gpt-oss:120b`, `kimi-k2.6`, `minimax-m3`, `mistral-large-3:675b`, `nim:deepseek-v4-pro`, `nim:minimax-m2.7`, `nim:step-3.5-flash`, `public`, `public-free`, `qwen3.5:397b`, `zai:glm-4.5`, `zai:glm-5`, `zai:glm-5.1`. Each sits on a family already sampled |
| embedding (5) | `jina-embeddings-v5-text-nano`, `embeddinggemma-300m`, `pplx-embed-v1-0.6b`, `llama-nemotron-embed-vl-1b-v2` | `jina-embeddings-v5-text-small` — same vendor and adapter as the nano |
| rerank (1) | `jina-reranker-v3` | — |
| transcription (2) | `whisper-1`, on a silent WAV the harness synthesizes | `gpt-4o-transcribe` |
| speech (2) | `tts-1` | `tts-1-hd` |
| image (1) | `gpt-image-1.5`, **argument-error probe only** | actual generation, behind `--include-image`; it costs money and is not run |

Every generative case uses a two-token prompt and `max_tokens: 8`, one call per gateway.

---

# §B — The cutover, re-run at `94de856`

Parity on a request table is not replacement. This is the part that is.

## What was run

**`jikjicode`** is an agent client on this machine that talks to real providers. One of its
providers was pointed at a locally running dorang instead of its usual endpoint, and a real task was
run through it — multi-turn, with tool calls, with streaming on, against a non-trivial context (a
26-tool schema and a ~7,500-token prompt per turn).

```
jikjicode ──(openai-compatible)──► dorang :4198 ──► the same LiteLLM ──► the same models
jikjicode ──(openai-compatible)──►                       LiteLLM ──► the same models
```

Both legs are `cutover.sh`, which renders `jikjicode.yaml.tmpl` twice — once with dorang's listener
as the base URL, once with `LITELLM_OPENAI_BASE_URL` — and changes nothing else.

**The operator's own agent configuration was not read and not modified.** `jikjicode --config
<file>` disables config discovery entirely, and the rendered roster is a minimal standalone file in
the run directory holding exactly one provider. The embedded orchestration runtime is off, because
it spawns sub-agents and those are real model calls that are not what is being measured.

The task: *read `notes.txt`, count its lines, write `summary.md` containing exactly
`notes.txt has N lines`, then read it back and report.* Unachievable without tools — the answer is a
property of a file the model cannot see until it reads it — and checkable from outside the
transcript.

dorang ran on a scratch state directory with the front-proxy config plus `compat: {legacy_headers:
true}`, which is the one setting a cutover needs and a request table cannot see.

## What worked

| | dorang leg | LiteLLM leg |
|---|---|---|
| exit | 0 | 0 |
| upstream turns | 5 | 5 |
| tool calls | 4 — `file.list`, `file.read`, `file.apply_patch`, `file.read` | 4 — `file.list`, `file.read`, `file.write`, `file.read` |
| `summary.md` written | `notes.txt has 5 lines` | `notes.txt has 5 lines` |
| final answer | correct | correct |
| wall clock | 10.0 s | 9.9 s |
| model name in the client's result event | `gpt-oss:20b` | `gpt-oss:20b` |

The dorang leg reached for `file.apply_patch` where the LiteLLM leg used `file.write`; both wrote
the same file with the same contents. The same model makes different tool choices between runs —
the previous pair differed the same way, in the other direction. Everything the task required
happened on both.

`dorang_requests_total{status="200",endpoint="chat_completions"}` reads **5**, and there is no other
chat status on the counter.

Four claims are verified against a **recording upstream** rather than inferred from a transcript, so
they cost nothing and are facts:

- **Streaming was on.** Every provider call carries `"stream": true` and
  `"stream_options": {"include_usage": true}`. Worth knowing for anyone repeating this: a control
  run *without* `--include-partial-messages` sends `"stream": false`, so a cutover test that omits
  that flag is not testing the streaming path at all.
- **Tool calls assembled across streamed chunks survive dorang.** The recording upstream split one
  call's `arguments` across three SSE frames; the client reassembled the complete JSON through
  dorang.
- **The multi-turn round trip survives dorang.** Turn 2's request carries
  `[system, developer, user, assistant, tool, user]` with `tool_calls` on the assistant turn — so
  the assistant turn that has a tool call and no content went back up through dorang and was
  accepted.
- **The legacy header mirror works.** With `compat.legacy_headers: true`, a response carries
  `X-Litellm-Call-Id`, `X-Litellm-Model-Id`, `X-Dorang-Real-Model` and — under
  `x-dorang-detail: full` — `X-Litellm-Attempted-Retries` and `X-Litellm-Response-Duration-Ms`.

No hang, no crash, no orphaned process; dorang exited on signal. Long-run memory was not
instrumented, so "no leak" is not claimed beyond that.

**Nothing was silently lost.** Content, tool calls, tool results and the final answer all survived
the gateway.

## What the ledger says about that session — the half a client cannot see

This is where the previous run's blockers lived, and it is the same query re-run:

| | Value |
|---|---|
| rows | 5, all `status: 200` |
| `streamed` | **`true` on all five** — was `false` on every row |
| `deployment_id` | **`gpt-oss:20b\|litellm\|gpt-oss:20b` on all five** — was `""` on every row |
| unfiltered `/spend/logs` | **`501`**, `"a ledger query needs a filter: key_id, team_id, trace_id, tag or errors_only"` — was `500 internal_error` |

And the session rollup, which is the number C1 was named for:

| | prompt | completion | reasoning | `total_tokens` |
|---|---:|---:|---:|---:|
| dorang's `usage_by_model_hour` | 37,778 | 638 | 461 | **38,416** |
| dorang's `usage_by_key_hour` | 37,778 | 638 | 461 | **38,416** |
| sum of the per-request ledger rows | 37,778 | 638 | 461 | **38,416** |
| what the wire reported (`prompt + completion`) | 37,778 | 638 | — | **38,416** |
| what the old rule would have recorded | | | | 38,877 |

**Four accumulators, one number.** The previous run's equivalent read 30,929 against 30,355.

# §C — The five defects, re-verified

Verified off the wire and out of the ledger, not by reading the commits. Where a controlled upstream
was enough, one was used: it costs nothing, and it can report a usage object in which every
breakdown field is a strict subset — which is what makes the two arithmetics distinguishable at all.

The controlled shape is `prompt 100 / completion 20 / cached 40 / cache-write 10 / reasoning 8`.
The client is owed `100 + 20 = 120`. The old rule sums all five and gets **178**.

### C1 — the ledger's `total_tokens` against the answer the client got — **closed**

| Case | wire `usage.total_tokens` | ledger row, same request | old rule |
|---|---:|---:|---:|
| controlled, non-streaming | 120 | **120** | 178 |
| controlled, streamed (`include_usage`) | 120 | **120** | 178 |
| controlled, `77 / 69 / reasoning 59` | 146 | **146** | 205 |
| live, one request through this LiteLLM | 133 | **133** | 133 |
| live agent session, 5 streamed turns | 38,416 | **38,416** | 38,877 |

The third row is the shape the previous report took from a direct probe of this deployment and
predicted the ledger would compute as 205. It records 146.

The fourth row is the instruction taken literally — `usage.total_tokens` off the wire and the ledger
row for that same request id — and it is a *weak* confirmation on its own: this LiteLLM's
`gpt-oss:20b` reports no `completion_tokens_details.reasoning_tokens` on the non-streaming path, so
old rule and new rule coincide at 133. Saying so matters more than quoting it. The discriminating
live evidence is the fifth row, where five streamed turns carried 461 reasoning tokens and the
rollup still equals `prompt + completion`.

`meter.Tokens.Total()` is now `Input + Output`, the same function as `canonical.Usage.TotalTokens`,
with the convention stated on the struct rather than left to a comment elsewhere.
`internal/app/metrics.go`'s `totalTokens` — the TPM ceiling's feed — carries the same rule, and
prefers the dispatcher's stated total when it has one.

**The rollups reconcile.** On the controlled rig, `usage_by_model_hour` and `usage_by_key_hour`
equal the sum of `request_logs.total_tokens`, per model and overall (`480 / 146 / 120`, key total
`746`), with `cost_nano` summing the same way (`660 + 215 = 875` nano). Under the old rule the
`mock-controlled` bucket would have read 712 against the wire's 480.

### C2 — unfiltered `/spend/logs` — **closed**

```
GET /spend/logs?start_date=…&end_date=…&limit=100
→ 501 {"error":{"message":"a ledger query needs a filter: key_id, team_id, trace_id, tag or errors_only",
                "type":"not_implemented_error","param":null,"code":"not_implemented"}}
```

Reproduced twice: on the controlled rig and on the live cutover. `admin.Unsupported` wraps the
sentinel instead of interpolating it as text, so `errors.Is(err, admin.ErrUnsupported)` holds, the
`501` case matches, and the message an operator can act on is the message they get. With a filter
the endpoint still answers `200` with the rows. The refusal is itself recorded in the ledger, as a
`501` row on `admin_spend`.

### A1 — `server.max_body_bytes` — **closed**

The fifth of the five, measured in §A-remaining A1 above because its residue is a request-table row:
a configured 1 MiB cap refuses a 2 MiB body at `1048576` and passes 512 KiB.

### C3 / C4 — `deployment_id` and `streamed` — **closed**

Measured on the controlled rig with five served requests, two of them streamed:

| request | `streamed` | `deployment_id` | matches header |
|---|---|---|---|
| non-streaming ×3 | `false` | `mock-controlled\|mock\|mock-controlled` | `x-dorang-deployment`, `x-litellm-model-id` |
| streamed ×2 | **`true`** | same | same |

and on the live cutover, where all five rows are `streamed: true` against requests that carried
`"stream": true`, an SSE body and a tool call reassembled from three frames, and all five carry
`gpt-oss:20b|litellm|gpt-oss:20b` where every row was empty before.

`Event.Streamed` is taken from the content type actually written, not from the `stream: true` the
request asked for, so a request that asks to stream and is refused before the first frame is not
recorded as one.

### C5 — the cost mirror — **closed for the non-streaming path**; see D2 for what it opened

Measured on one instance carrying a priced model and an unpriced one:

| request | `x-litellm-response-cost` | `x-dorang-cost-usd` | §7.7a reading |
|---|---|---|---|
| priced, non-streaming | `0.0001606` | `0.0001606` | priced at *n* — correct |
| unpriced, non-streaming | **`0`** | *absent* | not priced — correct |
| priced but zero-rated | `0` | `0` | priced and free — correct |

The pair discriminates exactly as documented, and the mirror no longer vanishes on a deployment with
no `pricing:` block. dorang still logs `no marginal price rule matched …; the request is unpriced`
once per request, which is how an operator finds the gap.

**`Result.SpendNanoUSD` has a producer.** On a key minted with `max_budget: 0.01`, two successive
priced requests:

```
request 1   x-dorang-spend-usd: 0.0000493   x-litellm-key-spend: 0.0000493
            x-dorang-budget-usd: 0.01       x-dorang-budget-remaining-usd: 0.0099507
request 2   x-dorang-spend-usd: 0.0002099   x-litellm-key-spend: 0.0002099
            x-dorang-budget-usd: 0.01       x-dorang-budget-remaining-usd: 0.0097901
```

It was a flat `$0.00` on every response of every budgeted deployment. It now moves, and it is the
spend of the binding subject — the one whose ceiling `x-dorang-budget-usd` reports — so the pair is
two numbers about one budget rather than two budgets' halves.

**The notional header** is gated on `NotionalPriced` rather than `Priced`, so a billed request with
no list rate publishes nothing instead of the `$0.00` DESIGN §8.5 rule 5 forbids by name. Read in
the code and pinned by `TestNotionalHeaderIsAbsentWhenThereIsNoListRate`; not separately
re-measured out-of-process, because it needs a `notional_rate` rule the parity configuration does
not carry.

**The third convention `15cf9ae` found** — `server.scanUsage`, where the Anthropic family's
`input_tokens` is cache-*exclusive* — is normalized to dorang's inclusive convention before it
reaches the ledger, so a metered passthrough is no longer short by the whole cached prefix.
`TestUsageScanning` asserts `{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":2}` →
`Input 5, Output 4, CacheRead 2, Total 9`. Verified by reading the test and the code; not reproduced
out-of-process, because this configuration declares no passthrough prefix.

---

# §D — What the fixes did not reach, and what one of them opened

Five accounting changes landed at once. These are what checking their interactions found.

### D1 — `pricing` charges the cached prefix twice, and the reasoning tokens twice

**This is the one that stops the cutover.**

`internal/pricing`'s `quantityOf` maps each rate component to a measured quantity:

```go
case cInput:      q = req.InputTokens        // the WHOLE prompt count
case cCacheRead:  q = req.CacheReadTokens    // …and the cached part of it, again
case cOutput:     q = req.OutputTokens       // the WHOLE completion count
case cReasoning:  q = req.ReasoningTokens    // …and the reasoning part of it, again
```

`InputTokens` is inclusive of both cache counts and `OutputTokens` is inclusive of reasoning. DESIGN
§10.7 states both, and states the consequence of getting it wrong in its own words:

> **Token accounting is the part that must be exactly right**, because it feeds cost, quota, and
> budget — a mis-mapped cache field does not produce a visible error, it produces a wrong invoice.

Measured, using **DESIGN §8.5's own example rate block** (`input 0.85`, `output 3.40`,
`cache_read 0.19` per 1M — a vendor's published list rates, carrying the `source:` and `as_of:` that
section requires) against usage of `prompt 100 (40 cached) / completion 20`:

| | per 1M units | USD |
|---|---:|---:|
| dorang charges | `100×0.85 + 20×3.40 + 40×0.19` = 160.6 | **`0.0001606`** — the figure on `x-dorang-cost-usd` and in the ledger row |
| the vendor bills for that usage | `60×0.85 + 40×0.19 + 20×3.40` = 126.6 | `0.0001266` |

**26.9% over**, on the documentation's own example. With a `reasoning` rate declared as well
(`input 0.001 / output 0.002 / cached_read 0.0001 / cache_write 0.0005 / reasoning 0.002` against
`100 (40 cached, 10 written) / 20 (8 reasoning)`) dorang charges `0.165` per 1M against a true
`0.099` — **66.7% over**, measured on the same rig. On the agentic traffic this gateway exists to
aggregate, where the cached fraction is most of the prompt, an OpenAI-shaped
`input 2.50 / cache_read 0.25` at a 90% cache hit charges `2.725` against a true `0.475` — **5.7×**.

This is the same defect as C1, one column over. C1 was tokens, which nobody bills against; this is
the invoice. The previous report saw half of it and scoped it away —

> cost is unaffected unless a `reasoning` rate is configured, in which case it would be charged twice

— and the cache half was never named. Writing a `pricing:` block is item 2 on this report's own
cutover checklist (§E), so an operator who follows the checklist with a real vendor rate table
over-bills.

**Why it blocks**: reconciliation against LiteLLM's cost figures cannot succeed, it fails silently,
and it fails in the over-charging direction on exactly the workload the gateway is for. It is the
sentence the last verdict used about tokens, now true about money.

### D2 — a priced streamed request publishes `x-litellm-response-cost: 0`

C5 made the legacy cost mirror unconditional. Response headers are stamped when the status line goes
out, which on a streamed response is **before the upstream has reported any usage**. So:

```
priced model, "stream": true
  wire:    x-litellm-response-cost: 0        x-dorang-cost-usd: absent
                                             x-dorang-spend-usd, x-dorang-budget-usd: absent
  ledger:  spend = 0.0001606
```

Measured on the budgeted-key rig: the streamed request's ledger row carries the same `0.0001606`
its two non-streamed siblings do, and the response asserted `0`.

COMPATIBILITY §7.7a's discriminator table has three rows and none of them covers this one. Read
literally, `x-litellm-response-cost: 0` with `x-dorang-cost-usd` absent means *"no price rule matched
this model — the number is unknown, not zero"*, which is the wrong answer about a priced request that
cost money. **Every agent turn streams**, so on the traffic §B measures this is not the edge case.

What changed and what did not: before the fix the header was **absent** on a stream, and §7.7a's own
argument is that an exporter treats absence as zero — so the exporter's *number* is unchanged. What
changed is that dorang now makes an affirmative claim where it previously stayed silent, and the
documented way to tell "unpriced" from "priced" now misclassifies. The incumbent has the same
constraint and resolves it the other way: **LiteLLM emits no `x-litellm-response-cost` at all** on
this deployment's streamed *or* non-streamed successes (D3).

The limitation is inherent — a cost cannot be in a header that precedes the body. Asserting `0`
rather than staying silent is a choice, and §7.7a should either carry the fourth row or the mirror
should stay absent when the cost is not yet knowable.

### D3 — the four cost header names the incumbent actually emits are still unmirrored

Re-measured this run, on both the streaming and non-streaming `gpt-oss:20b` cases and on a direct
probe of the deployment:

```
x-litellm-response-cost-original:         0.0
x-litellm-response-cost-margin-amount:    0.0
x-litellm-response-cost-margin-percent:   0.0
x-litellm-response-cost-discount-amount:  0.0
x-litellm-key-spend:                      0.0
(no x-litellm-response-cost)
```

`x-litellm-response-cost` — the one name §7.7a models and now always emits — is **not on these
responses at all**. A cost exporter pointed at this deployment is reading one of the four dorang
does not model. §7.7a now names them with reasons, which is the right treatment; the gap itself is
unchanged.

### D4 — the token guard still carries the rule C1 was named for

`internal/app/metering.go`, seventy lines above the line C1 fixed:

```go
if tokens := r.Tokens.Input + r.Tokens.Output + r.Tokens.Reasoning; tokens > 0 && ev.KeyID != "" {
    _ = a.guard.Observe(context.Background(), ev.KeyID, tokens, a.now())
}
```

Reasoning is a subset of output. Measured through `meterAdapter.Record` into a real
`keyguard.MemHistory`, for the controlled request:

| reader | value |
|---|---:|
| what the wire said | 120 |
| ledger `total_tokens` | 120 |
| TPM ceiling (`metrics.totalTokens`) | 120 |
| **token guard `Observe`** | **128** |

That is the exact `120` against `128` C1 was named for. It did not go away; it moved. There are now
three definitions of "tokens consumed" in the binary, and the guard holds a fourth spelling — it
adds reasoning but not the cache counts, so it matches neither the old rule nor the new one.

**Why it is not a blocker**: the guard compares an observed rate against a baseline computed the same
way, so a *steady* reasoning fraction cancels in the `factor` test, and nothing a customer sees is
affected. **Why it is still a defect**: `trigger.min_absolute` is an absolute token count compared
against an inflated number, so the guard arms earlier than configured; and a key that moves to a
reasoning-heavy model can nearly double its measured rate with no change in real consumption —
which, under the default `action: pend`, is an automatic key suspension on a false signal.

### D5 — `/global/spend/report` answers `501`

```
GET /global/spend/report?start_date=…&end_date=…&group_by=model
→ 501 {"message":"aggregate spend reporting has no rollup query behind it in this build;
                  /spend/logs serves the per-request ledger", …}
```

The rollups are written and, as of C1, correct — they are just not readable through the
administration surface. The endpoint an operator would naturally reach for to reconcile a month
against LiteLLM's is the one that is not there; `/spend/logs` with a filter plus client-side
aggregation is the workaround, and it works. Pre-existing, not a regression, and worth naming
because C1's whole subject was reconciliation.

---

# §E — What a cutover must configure that this directory does not

Not defects; the checklist the runs produced.

| | Why |
|---|---|
| `compat: {legacy_headers: true}` | Off by default. Without it a dashboard reading `x-litellm-*` silently reports zero. `cutover.sh` sets it; `dorang.yaml.tmpl` deliberately does not, so the parity table measures dorang's own surface |
| a `pricing:` block | Every model is unpriced without one — and **see D1 before writing one**: declaring a vendor's `cache_read` or `reasoning` rate alongside its `input`/`output` rates over-bills |
| `context_window` per model | A4. Without it `GET /models` drops `max_input_tokens` |
| `server: {max_body_bytes: …}` | Now possible (A1). Raise it if a client posts bodies over 32 MiB today |
| `compat: {anthropic_total_tokens: false}` | A8, if `/v1/messages` clients must see this LiteLLM's exact usage object |

---

# Gaps in the reproduction itself

Limits of what could be observed from outside the LiteLLM deployment, referenced by name from the
configuration templates.

**G1 — `base_url` cannot be an environment reference.** dorang expands environment variables in
secret fields only (`key_env` / `key_file`); `providers[].base_url` is a literal string. The
upstream host therefore cannot be named by variable in the file, and the template must be rendered
(`envsubst '${LITELLM_OPENAI_BASE_URL}'`) before use. The key is *not* substituted — it stays a
`key_env:` reference read from the process environment at load — so no rendered file holds a
credential.

**G2 — the three LiteLLM-side aliases are declared as models.** `local`, `public` and `public-free`
are aliases on the LiteLLM side. Resolving them dorang-side needs their targets, and
`GET /model/info` answers **403** to the client key available here (*"Virtual key is not allowed to
call this route"*). They are therefore declared as models forwarding their own name: correct
behaviour, wrong shape.

**G3 — `dorang-direct.yaml.tmpl` is inference, not observation.** Same 403. The true provider,
upstream model id, api base and per-deployment fallback behind each of the 39 routes are not
observable from a client key. 20 of the 39 are attributed from the operator's credential set plus the
model ids; the other 19 are absent from that file rather than guessed at. It is the shape of a
cutover for an operator with LiteLLM admin access to complete, and **must not be deployed as-is.**

**G4 — LiteLLM's own fallbacks are invisible.** They are configured per `model_name` upstream and
not visible from a client key, so `dorang.yaml.tmpl` disables class fallback rather than reproduce a
chain it cannot see. `x-litellm-attempted-fallbacks: 0` on every observed response is consistent
with none firing and is not proof that none are configured.

---

# What remains, ranked

| # | What | Where | Blocks a replacement? |
|---|---|---|---|
| 1 | `pricing` charges `input` against the whole inclusive input and `cache_read` against the cached part again; same for `output` and `reasoning`. 27% over on DESIGN §8.5's own example rates, 5.7× on a 90%-cached workload | D1 | **Yes.** Silent, in the over-charging direction, on the traffic the gateway is for, and reached by following this report's own checklist |
| 2 | A priced streamed request publishes `x-litellm-response-cost: 0` while the ledger records the real cost; §7.7a's discriminator reads that pair as "not priced" | D2 | **Yes**, for a cost exporter, and silently. Every agent turn streams |
| 3 | The token guard is fed `Input + Output + Reasoning` — 128 for the request whose answer said 120 | D4 | No customer effect. Can pend a key on a false signal under the default action |
| 4 | `/global/spend/report` → `501`; the rollups are correct but not readable through the admin surface | D5 | No — `/spend/logs` plus client-side aggregation does it |
| 5 | Four `x-litellm-response-cost-*` names this deployment emits are unmirrored, and dorang now always emits the one name it does not | D3 | Mildly, for an exporter built on this deployment |
| 6 | `GET /models` drops `max_input_tokens` / `max_output_tokens`; `owned_by` changes | A4 | Mildly, and it is configuration |
| 7 | Oversized body: `413` where LiteLLM answers `400` | A1 | No longer — the cap is configurable, so the status is the only difference left |
| 8 | Unknown model `400 → 404` | A2 | Only a client branching on status; dorang matches the vendor |
| 9 | `/v1/messages` omits the empty text block LiteLLM appends on a truncated turn | A8 | Only a client indexing `content[-1].text`; dorang matches the vendor |
| 10 | `usage` present-and-null becomes absent on transcriptions | A5 | Only a client using `[]` rather than `.get()` |
| 11 | Error `type` / `param` vocabulary | A3 | No — LiteLLM's values are `null` and the string `"None"`; nothing could branch on them |
| 12 | §7.7a's discriminator table has no row for a streamed priced request; §6.8's claim about the reference implementation's `total_tokens` | D2, A8 | Documentation — and item 12's first half is what makes item 2 misread rather than merely incomplete |

**Closed since the previous verdict**, each re-verified in §C rather than taken from a commit
message: C1 (ledger `total_tokens`), C2 (unfiltered `/spend/logs`), A1 (body-cap configurability),
C3 and C4 (`deployment_id`, `streamed`), C5 (the cost mirror on the non-streaming path,
`Result.SpendNanoUSD`'s producer, and the notional header's gate).

Items 1 and 2 are what now stand between "serves the traffic" and "replaces the deployment". As
before, **neither is on the request path, and neither would have been found by a request table** —
and item 1 would not have been found by the cutover either, because the configuration both runs use
declares no prices. The harness that found the last five blockers could not have found this one.

---

## Reproducing

```bash
./run.sh --list                       # the 40 cases, spending nothing
./run.sh --json out.json              # the table in §A
./cutover.sh both                     # §B, two agent sessions
DORANG_BIN=/path/to/older ./run.sh    # a before/after column
```

`run.sh` and `cutover.sh` each build a binary from the working tree unless `DORANG_BIN` names one,
run against a `mktemp -d` state directory, and remove or report it on exit. Neither writes into the
repository and neither touches anything under the operator's home directory.

§C's and §D's controlled-upstream measurements are not scripted here. They need a stub upstream that
reports a usage object in which every breakdown field is a strict subset, and a configuration
carrying a priced model, an unpriced model, a budgeted key, a vendor-shaped rate block and a
deliberately small `max_body_bytes`. The shape is stated precisely enough above to rebuild, and the
unit tests that pin each conclusion are named where they exist:
`TestLedgerTotalTokensEqualsTheAnswerTheClientGot`, `TestStreamedRequestIsRecordedAsStreamed`,
`TestConfiguredBodyCapIsHonoured`, `TestUnfilteredSpendLogsIsNotImplementedRatherThanInternalError`,
`TestUnpricedRequestStillMirrorsTheCostHeaderAsZero`, `TestPricedRequestMirrorsTheCostHeaderExactly`,
`TestNotionalHeaderIsAbsentWhenThereIsNoListRate`, `TestUsageScanning`.

**Nothing in D1, D2 or D4 has a test.** That is the point of naming them.
