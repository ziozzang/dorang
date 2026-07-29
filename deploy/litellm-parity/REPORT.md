# LiteLLM parity, and the cutover

> **Disposition note — 2026-07-29, `8e6016d`.** The verdict below is a measurement at `a8a0ca1`
> and is not edited. Since it was taken, **all four of the defects it names as "still wrong" are
> closed**, each with a named test: N1 (a restart re-attributing a subscription period's elapsed
> share), N2 (`subscription_spend` with no producer), N3 (`/key/info` reporting `spend: 0`) and
> N4 (the unhydrated first budget). The dispositions are under "What this run found" and in the
> ranked list; §E's `fixed_subscription` warning is lifted there too. **Do not read the verdict
> paragraph as current status** — read it as what was true when it was measured, which is the
> only thing a measurement is ever evidence of.

## Verdict — 2026-07-29, `a8a0ca1`

**Yes. dorang can replace this LiteLLM deployment.** The request surface has not moved in three
runs, the token column has one rule and six readers that agree with it, and **the money column now
equals the vendor's own arithmetic to the nano on every case that was wrong before** — measured
through the running gateway against hand-computed invoices, and measured again on the previous
binary so the change is a before/after and not a claim. A priced streamed request no longer
publishes a cost of zero.

The two blockers of the last verdict are closed, and both were verified at the level they were
reported — off the wire and out of the ledger, one gateway process per column:

| | `94de856` charged | `a8a0ca1` charges | the vendor's invoice, by hand |
|---|---:|---:|---:|
| DESIGN §8.5's own card, prompt 100 (40 cached) / completion 20 | `$0.0001606` | **`$0.0001266`** | `$0.0001266` |
| a 90%-cached agent turn, 1000 (900 cached) / 0 | `$0.002725` | **`$0.000475`** | `$0.000475` |
| every sub-rate declared, 100 (40 read, 10 written) / 20 (8 reasoning) | `$0.000000165` | **`$0.000000099`** | `$0.000000099` |
| a card with a cache rate the backend never exercises | `$0.000153` | `$0.000153` | `$0.000153` |
| live, through this LiteLLM: `gpt-oss:20b`, 68 / 8 | — | **`$0.000085`** | `$0.000085` |

26.9% over, 5.74× over and 66.7% over are now 0.0% over, three times. The fourth row is the
degenerate case and it was never wrong: a rule whose `cache_read` rate meets a backend reporting no
cached tokens bills the whole prompt at the `input` rate, which is what that vendor charges.

**The streamed cost header.** At `94de856` a priced streamed request published
`x-litellm-response-cost: 0` while its ledger row recorded `160600` nano. At `a8a0ca1` the header
is **absent**, the ledger row for that request id carries the real figure, and the §10.4 usage
event on the same stream carries it too — `cost_usd: "0.0001266"` on the rig, `"0.000085"` on the
live deployment, both equal to their ledger rows. §7.7a's four states were driven and all four are
distinct.

What an operator must do to cut over, and what to watch in the first hour, is §E. It is three
configuration decisions and one dashboard re-point; none of it is a code change.

**What is still wrong, and why none of it blocks.** The ranked list is at the end. The largest item
is new and was found by this run: a **process restart re-attributes a `fixed_subscription` period's
elapsed share**, so two process starts put **$180.46 on a $100 plan**, and the first request after
a restart was refused `400 budget_exceeded` on a key that had never spent anything. It does not
block a cutover from LiteLLM because LiteLLM has no subscription class to reproduce — **but do not
declare a `fixed_subscription` rule until it is fixed.** The other two are a `/key/info` that
reports `spend: 0` for every key while the ledger and the rollups carry the right number, and a
cost decomposition that files a plan share under `marginal_spend`. Both are reporting surfaces with
a correct source behind them, and both were invisible until a configuration declared prices.

Measured against `a8a0ca1`; the binary reports `v0.0.0-20260728230432-a8a0ca14351c`. The suite was
re-run once during this pass — `go test -race -count=1 ./...`, exit 0, 35 packages `ok`, no `FAIL`
and no `DATA RACE` — on a tree that had moved to `c6b1f08`, whose entire delta is 45 lines of
`testing/scenario/metering_test.go`. Nothing measured below is on that path.

---

## Verdict history

Superseded verdicts are kept rather than overwritten. A record that shows only the current state
cannot be audited.

### 2026-07-29, `94de856` — superseded

> **Still not — but the reason has changed, and it is now a smaller surface with a larger number
> behind it: all five defects that blocked the last verdict are fixed and verified against the
> wire, and what blocks this one is that the `pricing:` block the cutover checklist tells an
> operator to write bills the cached prefix and the reasoning tokens twice — 27% over on DESIGN
> §8.5's own example rates, several-fold on the cache-heavy agentic traffic this gateway exists to
> aggregate — while a priced streamed request publishes `x-litellm-response-cost: 0` and records
> the real cost in the ledger.**
>
> The token column is right now. The money column is not, and money is the column a replacement is
> judged on.

Its two blockers were D1 (the pricing convention) and D2 (the streamed cost header). Both were
fixed at `6004a98` and both are re-measured in §C below against a hand-computed vendor figure and
against the previous binary, not read off the commit. Its item 3 (the token guard's fourth spelling
of "tokens consumed") and its item 4 (`/global/spend/report` → `501`) are also closed, and are
re-verified in §D.

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

Its five blockers were C1, A1, C2, C5 and C3/C4. All five were fixed at `15cf9ae` and `94de856`
and re-verified in that round — off the wire and out of the ledger, not by reading the commits.
They are not re-billed here, except where a later change could have disturbed them: §D re-checks
every reader of the token total, which is what C1 was about.

---

## What is in this directory

| File | What it is |
|---|---|
| `dorang.yaml.tmpl` | The 39-model LiteLLM surface reproduced on dorang **as a front proxy**, with that same LiteLLM as the single upstream. One upstream behind both gateways, so every difference the harness reports is the gateway's. **It now declares prices** — see below |
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

It cannot see a conversation. That is what §B exists for.

**The configurations now declare prices, and that is the change that matters most about this
harness.** The last verdict's finding was that neither runner could see a pricing defect because
both priced everything at zero, so every run compared zero against zero. They no longer do, and
§B's cost column is now a measurement that can be checked against a rate card by hand — it was. The
blind spot is narrowed, not closed: **the priced configuration still cannot discriminate the two
pricing conventions on live traffic** (gap G5), so §C's discriminating evidence is still a
controlled upstream, and says so.

---

# §A — The request table, re-measured at `a8a0ca1`

Measured **2026-07-29**. `go test -race -count=1 ./...` exit 0, 35 packages `ok`, no `FAIL` and no
`DATA RACE`. LiteLLM `1.93.0`.

```
                     40 cases
before (87cbe21)     PASS 13   DIFF 27   FAIL 0     58 divergences, 21 at high severity
       (e515a48)     PASS 13   DIFF 27   FAIL 0     41 divergences, 18 at high severity
prev   (94de856)     PASS 13   DIFF 27   FAIL 0     41 divergences, 18 at high severity
now    (a8a0ca1)     PASS 14   DIFF 26   FAIL 0     40 divergences, 17 at high severity
```

**One row moved, and it is A9's again.** `nim:deepseek-v4-flash [stream]` answered `200` on both
gateways this time, where the previous run had LiteLLM answering `500` and the run before that had
dorang answering `500`. Three runs, three different answers on one model, every one of them the
upstream's — see A9. Nothing else in the table changed by a single divergence.

That is the expected result and it is worth saying rather than burying: **none of the fixes since
`94de856` is on the request path**, so a request table cannot register them. The table's job here
was to prove that a change to the number every subsystem consumes landed without disturbing the
surface, and it did.

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
| chat-stream | `POST /chat/completions nim:deepseek-v4-flash [stream]` | 200 | 200 | **PASS** | 0 |
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
`zai:glm-5-turbo` answers `502`. dorang reproduces every one with the same status. Ten of the 26
DIFFs are those five models × (streaming, non-streaming), and in each the only divergence is the
error *envelope*, which is A3.

### A1 — 32 MiB body cap: LiteLLM `400`, dorang `413` — the cap is configurable

Closed as a capability at `94de856` and unchanged: `server.max_body_bytes` exists, is wired through
`internal/app`'s `server.Options` literal, and is honoured — a configured 1 MiB cap refuses a 2 MiB
body at `1048576` and passes 512 KiB, and `TestConfigBodyCapDefaultMatchesTheServers` pins the
duplicated 32 MiB default across `internal/config` and `internal/server`.

**What remains is the status.** dorang refuses over-cap with `413 request_too_large`; LiteLLM has no
cap and forwards the 33 MiB body to its router, which rejects it for an unrelated reason with `400`.
That is COMPATIBILITY §11.2 and is defensible — and it is a difference an operator can route around
by raising the cap, rather than one requiring a rebuild.

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

Four runs, four different pairs on this model's `[stream]` case, all of them the upstream's:

| Run | LiteLLM | dorang |
|---|---:|---:|
| `87cbe21` | timeout | 200 |
| `e515a48` | 200 | 500 (`ResourceExhausted: Worker local total request limit reached (651/48)`) |
| `94de856` | 500 | 500 |
| `a8a0ca1` | 200 | 200 |

**Flaky upstream, faithfully surfaced.** This model also takes minutes to answer an 8-token request,
on both gateways. It is the single row that separates this run's counts from the previous one's.

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

# §B — The cutover, re-run at `a8a0ca1`

Parity on a request table is not replacement. This is the part that is — and this is the first run
in which it has a **money column**.

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
| upstream turns | 4 | 4 |
| tool calls | 3 — `file.read`, `file.write`, `file.read` | 3 — `file.read`, `file.write`, `file.read` |
| `summary.md` written | `notes.txt has 5 lines` | `notes.txt has 5 lines` |
| final answer | correct | correct |
| wall clock | 12.1 s | 10.2 s |
| model name in the client's result event | `gpt-oss:20b` | `gpt-oss:20b` |

`dorang_requests_total{status="200",endpoint="chat_completions"}` reads **4**, and there is no other
chat status on the counter. No hang, no crash, no orphaned process. **Nothing was silently lost** —
content, tool calls, tool results and the final answer all survived the gateway.

The four claims about the *shape* of the traffic — that streaming was on with
`stream_options.include_usage`, that a tool call assembled across three SSE frames survives, that
the multi-turn round trip carries an assistant turn with `tool_calls` and no content back up, and
that the legacy header mirror emits `X-Litellm-Call-Id`, `X-Litellm-Model-Id`,
`X-Dorang-Real-Model` and the two detail-gated names — were established against a **recording
upstream** in the previous round and are unchanged. They are not re-billed here.

## What the ledger says about that session — and what it now costs

| | Value |
|---|---|
| rows | 4, all `status: 200` |
| `streamed` | `true` on all four |
| `deployment_id` | `gpt-oss:20b\|litellm\|gpt-oss:20b` on all four |
| unfiltered `/spend/logs` | `501`, `"a ledger query needs a filter: key_id, team_id, trace_id, tag or errors_only"` |

```
row  prompt  completion  reasoning   total       spend
 1    7,678         99         83    7,777   $0.0068629
 2    7,466         89         57    7,555   $0.0066487
 3    7,398         90         59    7,488   $0.0065943
 4    7,137        569        540    7,706   $0.00800105
                                    ------   -----------
                                    30,526   $0.02810695
```

**Four accumulators, one number, and now a fifth:**

| | value |
|---|---:|
| sum of the per-request ledger rows | 30,526 tokens / `$0.02810695` |
| `dorang_tokens_total{input}` + `{output}` | 29,679 + 847 = **30,526** |
| `dorang_cost_nano_total` | **28,106,950** nano = `$0.02810695` |

And the money is checkable by hand, which is what pricing the configuration bought. Row 1's model
declares `input: 0.85`, `output: 3.40` and the upstream reports no cached tokens:

```
7,678 x $0.85/1M  =  $0.0065263
   99 x $3.40/1M  =  $0.0003366
                     ----------
                     $0.0068629     — the ledger row, exactly
```

Row 4 the same way: `7,137 x 0.85 + 569 x 3.40 = $0.00800105`. Note what row 4 also shows: 540 of
its 569 completion tokens were reasoning, and it is charged for 569 output tokens and no more,
because this rule declares no `reasoning` rate. That is CONFIG §13.1a's degenerate case on live
traffic.

**What this run still cannot see.** Those reasoning counts are the only breakdown this deployment
reports, and `gpt-oss:20b`'s rule declares no `reasoning` rate — so the carve-out never fires, and
the live legs still cannot tell the two pricing conventions apart. Gap G5.

# §C — The two blockers, re-measured

Verified with a controlled upstream, because a controlled upstream is the only way to get a usage
object in which every breakdown field is a **strict subset** of the count it belongs to — which is
what makes the two arithmetics distinguishable at all. Every figure below is one request through a
running gateway, read off its response headers and out of its ledger, and compared against an
invoice computed by hand from the rate card.

The **before** column is not quoted from the previous verdict. `94de856` was built into a worktree
and run against the same mock, the same configuration and the same rates, so the two columns differ
only in the binary.

### C1 — the pricing convention — **closed**

The rig declares four cards and the mock reports four usage objects:

| card | rates per 1M | usage (dorang's inclusive counts) |
|---|---|---|
| `card-model` | `input 0.85, output 3.40, cache_read 0.19` — DESIGN §8.5's own | `100 (40 cached) / 20` |
| `cache90-model` | `input 2.50, cache_read 0.25` — OpenAI-shaped | `1000 (900 cached) / 0` |
| `reasoning-model` | `input .001, output .002, cache_read .0001, cache_write .0005, reasoning .002` | `100 (40 read, 10 written) / 20 (8 reasoning)` |
| `plain-model` | `input 0.85, output 3.40, cache_read 0.19` | `100 / 20`, **no breakdown reported at all** |

| case | `94de856` | `a8a0ca1` | the vendor's invoice, by hand | ledger row |
|---|---:|---:|---|---:|
| §8.5's card | `0.0001606` | **`0.0001266`** | `60×0.85 + 40×0.19 + 20×3.40` = `0.0001266` | `0.0001266` |
| 90% cached | `0.002725` | **`0.000475`** | `100×2.50 + 900×0.25` = `0.000475` | `0.000475` |
| every sub-rate | `0.000000165` | **`0.000000099`** | `50×.001 + 40×.0001 + 10×.0005 + 12×.002 + 8×.002` = `0.000000099` | `9.9e-08` |
| nothing reported cached | `0.000153` | `0.000153` | `100×0.85 + 20×3.40` = `0.000153` | `0.000153` |

`0.0001606 ÷ 0.0001266 = 1.2686` and `0.002725 ÷ 0.000475 = 5.7368` — the 27% and the 5.7× of the
last verdict, reproduced on the old binary and then measured away. `x-dorang-cost-usd`,
`x-litellm-response-cost` and the ledger row carry the same figure in every row.

The fourth row is the one that proves the rule degrades correctly rather than merely differently: a
model whose backend never mentions a cache is billed for its whole prompt at the `input` rate even
though its rule declares `cache_read`, because the carve-out is driven by the measured quantity and
that quantity is zero. CONFIG §13.1a states the convention an author reads — `input` is charged on
`input_tokens − cache_read − cache_write`, `output` on `output_tokens − reasoning`, each
subtraction applying only when the rule declares that sub-rate.

Live, through this LiteLLM with the harness's own card: `gpt-oss:20b`, `68 / 8`, header
`x-dorang-cost-usd: 0.000085` = `x-litellm-response-cost` = ledger row = `68×0.85/1M + 8×3.40/1M`.
That row is a **weak** confirmation on its own and is recorded as such — no cached tokens, no
declared reasoning rate, so old rule and new rule coincide on it. The discriminating evidence is the
controlled rig above.

The package's own assertion is against the same hand-computed constant rather than against another
dorang function: `TestChargedAmountMatchesTheVendorInvoice` fails on `160_600` by name and requires
`126_600`. A test that compares two internal functions cannot catch a convention error, because both
of them agreed — which is how this survived two clean parity runs.

### C2 — the streamed cost header — **closed**, and §7.7a's four states discriminate

At `94de856`, one streamed priced request:

```
wire:    x-litellm-response-cost: 0        x-dorang-cost-usd: absent
ledger:  cost_nano = 160600                streamed = 1
```

At `a8a0ca1`, the same request on the same rig:

```
wire:    x-litellm-response-cost: absent   x-dorang-cost-usd: absent
sse:     event: dorang.usage
         {"request_id":"032ab157…","cost_usd":"0.0001266", …}
ledger:  spend = 0.0001266                 streamed = true
```

They agree, or the header is absent. All four of §7.7a's states were driven on one instance carrying
a priced model, an unpriced model and a zero-rated model, and they are mutually distinct:

| request | `x-litellm-response-cost` | `x-dorang-cost-usd` | §7.7a reading |
|---|---|---|---|
| priced, non-streaming | `0.0001266` | `0.0001266` | priced at *n* |
| unpriced, non-streaming | `0` | *absent* | no price rule matched |
| priced but zero-rated, non-streaming | `0` | `0` | priced, and free |
| **priced, streamed** | *absent* | *absent* | **the answer streamed — read the usage event or the ledger** |
| unpriced, streamed | *absent* | *absent* | the answer streamed (the same state; it makes no claim about pricing) |

The remedy the fourth state points at was driven too, on the rig and on the live deployment: the
`dorang.usage` SSE event carries a `cost_usd` equal to the ledger row in both — `0.0001266` and
`0.000085`. The flag is computed from the `Content-Type` the dispatcher actually set rather than
from the request's `stream: true`, so a request that asked to stream and was answered with a
complete body still publishes its cost.

One documentation residue: §7.7a's mirror table still marks `x-litellm-response-cost` **"yes,
always"** in its *Always on?* column, three rows above the four-state table that says it is absent
on a stream. The two tables now disagree on the page.

---

# §D — What the change to the number every subsystem consumes did not break

Six readers of "how many tokens was that" and every reader of "what did it cost" were checked
against the ledger, from outside the process wherever the ceiling could be observed from outside.

### D1 — the tokens-per-minute ceiling counts what the ledger counts — **verified on the wire**

The fourth reader the last sweep found was `app/metrics.go` preferring a backend-stated `Total`. The
rig has a model for exactly that: the mock reports `total_tokens: 99` against `prompt 10,
completion 5`.

```
key with tpm_limit: 20        request 1 -> 200
                              request 2 -> 200
                              request 3 -> 429  auth: rate limit exceeded (key: tpm)
```

Two admitted then refused is 15 per request. Had the ceiling taken the backend's 99, the *second*
request would have been refused. The client was told `total_tokens: 15` and the ledger row says
`10 / 5 / 15`. Three readers, one number, and the discriminating one is observable from outside.

### D2 — the token guard reads the same function — verified in process

Not observable from the wire: the guard compares a rate against a baseline and its default action is
reversible. `TestTokenTotalHasOneRule` asks all six readers the same question about a request whose
every breakdown field is non-zero and none equal to its parent (`120 / 15`, cache-read 40,
cache-write 10, reasoning 5) and requires `135` from each — naming `140` as "adds Reasoning" and
`190` as "sums all five" so a failure names the defect rather than a delta. The guard's answer goes
through the **real** `meterAdapter.Record` into a real `keyguard` with a capturing history, because
the defect was a caller that had written the sum out for itself and only the caller's own path can
show that. `TestNoSecondTokenTotalRuleIsWrittenAnywhere` then AST-scans every non-test file in the
module for a `+` chain mixing a cache or reasoning term with an input or output term, exempting the
normalization case structurally rather than by list. Both pass.

### D3 — budgets agree with the ledger — **verified on the wire**

On a key minted with `max_budget: 0.01`, after 18 priced requests:

```
x-dorang-spend-usd:              0.001762598
x-litellm-key-spend:             0.001762598
x-dorang-budget-remaining-usd:   0.008237402   (= 0.01 - 0.001762598)
sum of the key's ledger rows:    0.001762598
```

Exact, to the nano, and it includes the streamed rows whose responses published no cost header. The
header lags its own request by that request's *reservation* rather than its settled cost — a request
that will cost `0.0001266` moves the reported spend by an estimate first — so the two agree at rest
and not mid-flight, which is what a reservation is.

Enforcement is durable across a restart: a key with `max_budget: 0.0003` was served twice, refused
`400 budget_exceeded` on the third, and after a full process restart was **still** refused on its
next request. The ledger and the enforced budget do not disagree.

### D4 — the rollups and the metrics agree with the ledger — **verified on the wire**

`/global/spend/report` answered `501` at `94de856` — re-confirmed on the old binary during this run.
It answers now, over the three §9.4 materializations, and it reconciles:

```
/global/spend/report?group_by=model    total spend 0.001762598   total tokens 3,920   18 requests
/global/spend/report?group_by=key      one key     0.001762598               3,920
sum of /spend/logs rows                            0.001762598               3,920
```

per model as well: `card-model` 4 requests × `0.0001266` = `0.0005064`; `cache90-model` 2 ×
`0.000475` = `0.00095`; `plain-model` 2 × `0.000153` = `0.000306`. On the live cutover,
`dorang_cost_nano_total` equalled the ledger sum to the nano (§B).

### D5 — the subscription reload residual is closed; a restart still re-attributes

A `fixed_subscription` rule of `100.00` monthly on the credential, driven late in the period:

```
first request of a fresh process        x-dorang-cost-usd: 90.225883054   (the elapsed share)
next request, ~3 s later                                    0.000239050
20 x SIGHUP, then a request                                 0.000259001   (a ~3.5 s slice)
next request, 3 s later                                     0.000239062
```

**Twenty reloads attribute nothing.** `Catalog.AdoptState` carries the period's attributed total
across the swap, and the measurement shows it: the request after twenty reloads is a time slice, not
a plan share. The `1,050 USD of a 100 USD plan` is gone.

**A process restart is not covered, and it is worse than the reload was** — N1.

### D6 — the cost header names the incumbent actually emits — unchanged, re-measured

A direct probe of the deployment this run, on a `gpt-oss:20b` success:

```
x-litellm-response-cost-original:         0.0
x-litellm-response-cost-margin-amount:    0.0
x-litellm-response-cost-margin-percent:   0.0
x-litellm-response-cost-discount-amount:  0.0
x-litellm-key-spend:                      0.0
(no x-litellm-response-cost)
```

`x-litellm-response-cost` — the one name §7.7a models — is **not on this deployment's responses at
all**, streamed or not. A cost exporter pointed at this deployment is reading one of the four dorang
does not mirror. §7.7a names them with reasons; the gap itself is unchanged from the last two runs.

---

## What this run found

All four are only reachable through a `pricing:` block — the previous verdict's blind spot, which
is why they are new — and the first two only through a `fixed_subscription` rule, which no
configuration in this directory declares.

> **Disposition, added 2026-07-29 at `8e6016d`. All four are closed.** The measurements below are
> left exactly as they were taken at `a8a0ca1` — a measurement edited in place stops being
> evidence, and N1's `$180.46 of a $100 plan` is the number that made the case. What follows is
> the disposition, which is a different kind of statement and is dated on its own.
>
> | # | Closed by | Named test |
> |---|---|---|
> | N1 | The subscription accumulator is serialized across a process boundary. `pricing.SubscriptionState` is the durable form — which period is open, and how much of the plan cost it has already attributed, as exact atto-scaled digits because 100 USD is 10²⁰ atto and no 64-bit integer holds it — and `Catalog.RestoreState` adopts it at start-up | `TestARestartDoesNotRefuseAFreshKeyOnItsFirstRequest` |
> | N2 | `internal/app/metering.go` writes `MarginalCostNano: t.MarginalCostNano` and `SubscriptionCostNano: t.SubscriptionCostNano` where it used to write the whole cost to the first. The decomposition now flows dispatch → `meter/accum.go` → `meter/types.go` → `store/ledger.go`, and both §9.4 materializations carry both columns | `TestAPlanShareIsRecordedAsSubscriptionSpend` |
> | N3 | `/key/info` reads DESIGN §9.4's per-key rollup over the key's own `budget_duration` window instead of `api_keys.spend_nano`, so it is the same figure `/spend/logs` and `/global/spend/report` report. The durable budget counter is deliberately not the source: a node charges a whole lease block to it before spending a unit | `TestKeyInfoReportsTheSpendTheLedgerRecorded`, which asserts the **two routes agree** rather than asserting a literal — a literal would pass against a re-point to any other constant |
> | N4 | The budget hold is hydrated whether or not the request takes a reservation | `TestTheFirstZeroCostRequestDoesNotClaimAZeroSpend`, three requests because two of them are the discriminator: a header that is simply never emitted would pass an assertion about the first one alone |
>
> **"N1, N2, N3 and N4 have no test. That is the point of naming them."** — the closing line of
> this document. It was the right thing to write and it is now false: naming them is what got
> them tested. The line is left standing at the end of the file with its correction beside it,
> because deleting the sentence would delete the argument it was making.
>
> Also closed: **ranked item 7**, §7.7a's mirror table saying `x-litellm-response-cost` is
> "always on" three rows above the table saying it is absent on a stream. COMPATIBILITY §7.7a
> now says what its own pair table says, and records what it said before. **The Korean mirror
> had it right all along** — `COMPATIBILITY.ko.md` §7.7 already stated the streaming absence and
> its reason, so the English was the drifted copy, not the translation.

### N1 — a restart re-attributes the whole elapsed share of a subscription period

The accumulator "lives for the life of this Catalog", and a restart builds one from nothing. There
is no store behind it.

```
after the first process, ledger total       $ 90.227737838   of a $100.00 monthly plan
restart, one request                        $ 90.229299890   attributed again, on one row
after the second process, ledger total      $180.458128041
```

DESIGN §8.1 states the invariant this breaks in its own words: *"the shares a period attributes sum
to the plan cost, and never to more."* Two process starts, 1.8 plan costs. N restarts in a period —
or N nodes in a cluster, each with its own accumulator — attribute N times.

**It is client-visible, not merely an accounting figure.** internal/app reserves the settled cost
against the requesting key's budget, so the re-attributed share lands on whoever sends the first
request after the restart:

```
fresh process, brand-new key, max_budget: 1.00, never used
POST /v1/chat/completions  ->  400  {"code":"budget_exceeded",
    "message":"the key budget for this credential is exhausted for the current monthly period"}
```

A key that has never spent anything is refused on its first request, because a plan share it has no
relationship to was booked against it.

**Why it does not block this cutover**: LiteLLM has no subscription class, so a configuration that
reproduces this deployment declares none and the defect is unreachable. **Do not declare a
`fixed_subscription` rule until it is fixed.** `6004a98` identified the single-request share as
known and deliberate — a plan already paid for is attributed to the traffic that used it — and what
is not deliberate is that the running total resets to zero with the process.

### N2 — a plan share is filed under `marginal_spend`, and `subscription_spend` has no producer

The same subscription request, read back out of the ledger:

```
"spend": 90.22929989,  "marginal_spend": 90.22929989,  "subscription_spend": 0
```

`internal/app/metering.go` writes `MarginalCostNano: t.CostNano` — the whole cost — and nothing ever
writes `SubscriptionCostNano`, because `meter.Trace` carries a single `CostNano` and `pricing.Cost`'s
three-way split is dropped at that boundary. `/spend/logs` and `/global/spend/report` both report it,
so the decomposition is wrong in both. The **total** is right everywhere.

DESIGN §8.1: *"The two are separate fields, never conflated."* They are conflated. Pre-existing —
`git log -S` puts the line at `cb898a1`, long before any of the pricing work — and **unobservable
until a configuration declared a plan**, which is the previous verdict's blind spot one class over.

### N3 — `/key/info` reports `spend: 0` for every key

```
GET /key/info?key_id=…      "spend": 0,  "max_budget": 0.01
the key's ledger rows                    0.001889198
the key's own response header            0.001889198
/global/spend/report by key              0.001889198
```

`api_keys.spend_nano` is read by `/key/info` and written by nothing on the request path — the budget
accounting lives in the quota gate, which is why enforcement works and this column does not. It is a
**silent zero on the endpoint a per-key spend dashboard is most likely to read**, which is precisely
the failure mode COMPATIBILITY §7.7a exists to prevent, on a route §7.7a does not cover. Whether the
incumbent's own value moves could not be observed: the client key available here gets `403` on
LiteLLM's `/key/info`, the same limitation as G2 and G3.

The number is reachable — `/spend/logs` and `/global/spend/report` both carry it and both agree with
the ledger — so this is a re-point, not a hole. §E says so.

### N4 — the first zero-cost request after a start reports an unhydrated budget

```
immediately after a restart, a zero-rated request:
    x-dorang-spend-usd: 0        x-dorang-budget-remaining-usd: 0.01
the same key's true spend at that moment:   0.001889198   (remaining 0.008110802)
the next priced request on that key:
    x-dorang-spend-usd: 0.001938498         (= 0.001889198 + its own reservation)
a zero-rated request after that:
    x-dorang-spend-usd: 0.002142398         = the ledger total, exactly
```

A request whose estimated cost is zero takes no reservation, so the budget hold is never hydrated
from the store and `Consumed()` answers from an empty one. One request per subject per process
start, on zero-estimate traffic only, and enforcement is unaffected. Low, and named because it is
the same shape as everything else on this page: a header answering `0` for a number it has not
looked up.

---

# §E — Cutting over

Not defects; the checklist the runs produced. The first three lines changed with this verdict.

| | Why |
|---|---|
| `compat: {legacy_headers: true}` | Off by default. Without it a dashboard reading `x-litellm-*` silently reports zero. `cutover.sh` sets it; `dorang.yaml.tmpl` deliberately does not, so the parity table measures dorang's own surface |
| a `pricing:` block | Every model is unpriced without one. **Writing one is now mechanical**: transcribe the vendor's card, each published price under its own name, and write nothing for the components the vendor does not price separately (CONFIG §13.1a). The warning that used to be on this line is closed |
| ~~**no `fixed_subscription` rule yet**~~ | N1, and **the warning is lifted at `8e6016d`**: the accumulator now crosses a process boundary, so a restart no longer re-attributes the period's elapsed share. A faithful LiteLLM reproduction still declares none, because LiteLLM has no subscription class — that is a fidelity argument, not a defect one |
| ~~per-key spend from `/spend/logs` or `/global/spend/report`~~ | N3, **closed at `8e6016d`**: `/key/info` reads the §9.4 rollup and reports the same number the other two routes do. Any of the three now answers "what has this key spent" |
| a cost exporter that understands an absent header | §7.7a state 4. On agent traffic every turn streams, so **every turn publishes no cost header**. Read the `dorang.usage` event, or join the ledger on `x-dorang-request-id`. Note also that this deployment emits four `x-litellm-response-cost-*` names dorang does not mirror, and does not emit the one it does |
| `context_window` per model | A4. Without it `GET /models` drops `max_input_tokens` |
| `server: {max_body_bytes: …}` | Raise it if a client posts bodies over 32 MiB today |
| `compat: {anthropic_total_tokens: false}` | A8, if `/v1/messages` clients must see this LiteLLM's exact usage object |

## The first hour

Five checks, in the order they fail:

1. **`no marginal price rule matched <provider>/<model>; the request is unpriced`** in the log, one
   per request. This is how a missing rate is found, and a missing rate is silent everywhere else.
   It should stop appearing within minutes of the cutover.
2. **`/global/spend/report?group_by=model` against the incumbent's spend for the same window.** They
   should differ by the rate cards, not by a factor. A factor of 1.27 or of 5.7 is §C's defect
   returning; a factor of 2 on one model is a card transcribed into the wrong component.
3. **`dorang_cost_nano_total` against the sum of `/spend/logs`.** They agreed to the nano here. If
   they drift, the meter is dropping events rather than mispricing them.
4. **The 429 rate on the tokens-per-minute ceiling.** It now counts `input + output` and nothing
   else, so keys throttled against an inflated count will have more headroom than they did. More
   traffic admitted is the *expected* direction; less means something else changed.
5. **`404` where clients expected `400`** in their own error logs (A2), and `KeyError: usage` in any
   strict transcription client (A5). Both are one line in a client, and neither is silent.

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

**G5 — the priced configurations still cannot discriminate the pricing convention on live
traffic.** This is new, and it is the residue of the blind spot the last verdict named. The configs
now declare prices, which is what gives §B a cost column that can be checked against a card — and it
was, by hand, on every row. But a carve-out only fires when a sub-rate is *both* declared and
measured, and on this deployment the two never meet: `gpt-oss:20b`, the one model the cutover drives,
declares `cache_read` and `cache_write` and the upstream reports `cached_tokens: 0` on every turn;
the one rule carrying a `reasoning` rate is `glm-5`'s, and `glm-5` is retired upstream and answers
`500`. So the live legs verify that dorang charges the declared card correctly, and the controlled
rig in §C remains the only thing that can tell the two conventions apart. **The harness's blind spot
is narrowed, not closed.**

---

# What remains, ranked

**As ranked at `a8a0ca1`. Five of the thirteen are closed at `8e6016d`** — items 1, 2, 3, 7 and
8, which is every one this run *found* plus the documentation contradiction. The ranking is left
in its measured order rather than renumbered, so that "what the harness found and what it cost"
stays legible; the closed rows carry their closure inline. Items 4-6 and 9-13 are unchanged and
each is documentation, configuration, or a deliberate vendor-matching choice.

| # | What | Where | Blocks a replacement? |
|---|---|---|---|
| 1 | ~~A process restart re-attributes a `fixed_subscription` period's elapsed share. $180.46 attributed of a $100 plan across two starts; the first request after a restart refused `400 budget_exceeded` on a key that had never spent~~ | N1 | **Closed at `8e6016d`.** The accumulator crosses the process boundary as `pricing.SubscriptionState`; `TestARestartDoesNotRefuseAFreshKeyOnItsFirstRequest`. The `fixed_subscription` warning in §E is lifted with it |
| 2 | ~~`/key/info` reports `spend: 0` for every key; the column has no producer on the request path~~ | N3 | **Closed at `8e6016d`.** `/key/info` reads the §9.4 per-key rollup; `TestKeyInfoReportsTheSpendTheLedgerRecorded` asserts it against `/global/spend/report` rather than against a literal |
| 3 | ~~A plan share is recorded as `marginal_spend`; `subscription_spend` is always `0`. The total is right~~ | N2 | **Closed at `8e6016d`.** The three-way split survives the `meter.Trace` boundary; `TestAPlanShareIsRecordedAsSubscriptionSpend` |
| 4 | Four `x-litellm-response-cost-*` names this deployment emits are unmirrored, and the one dorang models is not on this deployment's responses at all | D6 | Mildly, for an exporter built on this deployment |
| 5 | On a stream there is no cost header at all — by design, and every agent turn streams | §C2 | No, once the exporter reads the usage event or the ledger. It must be told |
| 6 | `GET /models` drops `max_input_tokens` / `max_output_tokens`; `owned_by` changes | A4 | Mildly, and it is configuration |
| 7 | ~~§7.7a's mirror table says `x-litellm-response-cost` is "always on" three rows above the table that says it is absent on a stream~~; §6.8's claim about the reference implementation's `total_tokens` | §C2, A8 | **First half closed at `8e6016d`.** COMPATIBILITY §7.7a's column now says what its pair table says, and records what it said before. The Korean mirror had it right already, so the English was the drifted copy. The §6.8 half stands |
| 8 | ~~The first zero-cost request after a process start reports an unhydrated spend of `0` and a full budget~~ | N4 | **Closed at `8e6016d`.** `TestTheFirstZeroCostRequestDoesNotClaimAZeroSpend` |
| 9 | Oversized body: `413` where LiteLLM answers `400` | A1 | No — the cap is configurable, so the status is the only difference left |
| 10 | Unknown model `400 → 404` | A2 | Only a client branching on status; dorang matches the vendor |
| 11 | `/v1/messages` omits the empty text block LiteLLM appends on a truncated turn | A8 | Only a client indexing `content[-1].text`; dorang matches the vendor |
| 12 | `usage` present-and-null becomes absent on transcriptions | A5 | Only a client using `[]` rather than `.get()` |
| 13 | Error `type` / `param` vocabulary | A3 | No — LiteLLM's values are `null` and the string `"None"`; nothing could branch on them |

**Closed since the previous verdict**, each re-verified above by measurement rather than from a
commit message: the pricing convention (§C1, with the old binary as the before column), the streamed
cost header and §7.7a's fourth state (§C2), the tokens-per-minute ceiling's preference for a
backend-stated total (D1, discriminated on the wire), the token guard's fourth spelling of "tokens
consumed" (D2), `/global/spend/report`'s `501` (D4), and the subscription reload residual (D5).

**What found what.** The request table found none of it, in four consecutive runs. The cutover found
none of it. Every defect in the last three verdicts was found by driving a controlled upstream
against a configuration built to make one specific number wrong in a visible way — and the four
found this round were found by declaring prices the harness does not declare, three of them a
*subscription* price that no configuration in this directory carries. The pattern is four for four:
**the harness sees what it has configured.**

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

§C's and §D's controlled measurements are not scripted here. The rig is:

- a stub OpenAI-compatible upstream serving a **fixed** usage object per model, in which every
  breakdown field is a strict subset — `prompt_tokens_details.cached_tokens`,
  `cache_creation_input_tokens`, `completion_tokens_details.reasoning_tokens` — plus one model that
  states a `total_tokens` contradicting its own parts and one that reports no breakdown at all;
- a dorang configuration declaring the four cards of §C1, an unpriced model, a zero-rated model, a
  key with `max_budget`, a key with `tpm_limit: 20`, and — for D5 and N1 — a `fixed_subscription`
  rule of `100.00` monthly;
- the same configuration run under a binary built from the previous commit, for the before column.

The unit tests that pin each conclusion are named where they exist:
`TestChargedAmountMatchesTheVendorInvoice`, `TestAnAgenticCacheHitIsNotBilledAsAFreshPrompt`,
`TestReasoningIsNotBilledAsOutputAndAgainAsReasoning`,
`TestAnUndeclaredSubRateLeavesItsTokensWithTheParent`,
`TestGraduatedBracketsChargeTheCachedPrefixOnce`, `TestStreamedRequestDoesNotPublishACostOfZero`,
`TestTheDiscriminatorStillDistinguishesUnpricedFromPriced`, `TestTokenTotalHasOneRule`,
`TestNoSecondTokenTotalRuleIsWrittenAnywhere`.

**N1, N2, N3 and N4 have no test.** That is the point of naming them.

> **They have tests now** — `TestARestartDoesNotRefuseAFreshKeyOnItsFirstRequest`,
> `TestAPlanShareIsRecordedAsSubscriptionSpend`, `TestKeyInfoReportsTheSpendTheLedgerRecorded`
> and `TestTheFirstZeroCostRequestDoesNotClaimAZeroSpend`, all at `8e6016d`. The sentence above
> is kept rather than corrected because it is the argument, not the status: naming a defect with
> nothing pinning it is what got it pinned, and a report that quietly rewrote itself once the
> work landed would have deleted the only evidence that naming works. Dated 2026-07-29.
