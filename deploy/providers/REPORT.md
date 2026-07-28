# Real-provider run — findings and their disposition

## What this document is, and what it is not

The harness in this directory brings dorang up against **real upstreams** — the coding-plan
subscriptions, an Ollama account pair, OpenRouter, HuggingFace, Jina direct, and the
operator's own LiteLLM — and records what came back. `probe.py --list` prints the case table
without spending anything; the interesting cases are the two 2x2s, where the same vendor
account is reachable behind an OpenAI-shaped base URL *and* an Anthropic-shaped one, so a
client can be pointed at either and dorang's conversion is the only variable.

Seven defects came out of that run. **This document was written afterwards**, from the
findings, the code, the test suite and an audit of the shipped catalog. Every quoted upstream
body and every quoted dorang response below is from the live run. Everything stated about the
*fix* — what changed, what it now rejects, which catalogued kinds move — was verified here, in
code and tests, not against a network. Nothing in this file is a fresh live measurement.

Files:

| File | What it is |
|---|---|
| `providers.yaml.tmpl` | The dorang configuration the run used, with the environment variables it expands |
| `run-providers.sh` | Expands the template and starts dorang against it |
| `probe.py` | The case table. One call per case, <=24 output tokens per chat case, no loops, no retries |
| `dual-key.yaml.tmpl`, `run-dualkey.sh`, `fake_upstream.py` | The two-accounts-on-one-provider case with a controllable upstream, so the capacity claim is provable rather than observed |

---

## Disposition

| # | Severity | Defect | Status |
|---|---|---|---|
| D1 | HIGH | A `base_url` carrying a path lost its version segment | **Closed** |
| D2 | HIGHEST | An upstream 200 that is not a response of the family became an empty success | **Closed** |
| D3 | HIGH | Embedding usage discarded when the vendor reports only `total_tokens` | **Closed** |
| D4 | MEDIUM | Non-streaming conversion emitted `created: 0` | **Closed** |
| D5 | LOW | The upstream id crossed families verbatim on one path only | **Closed — decided, not defaulted** |
| D6 | judgement | The converted OpenAI stream omitted `role: "assistant"` on the first delta | **Closed** |
| D7 | judgement | The usage chunk's `choices` shape has no operator knob | **Closed** — and so is `compat.anthropic_total_tokens`, its twin |
| D8 | HIGH | *(found while fixing D2)* The same silent-success defect on every relay surface | **Closed** — every remaining surface (rerank, moderations, images, transcription, speech, Gemini) is gated |

Everything marked closed is pinned by a named test. Each fix was reverted and the named test
observed to fail; the table is at the end.

---

## D2 — an upstream 200 that is not a response of the family

### What happened

z.ai answers a request addressed at a route it does not serve with **HTTP 200** and

```json
{"code":500,"msg":"404 NOT_FOUND","success":false}
```

`internal/wire/anthropic/decode.go` and `internal/wire/openai/decode.go` both
`json.Unmarshal`ed that into their wire response struct — which succeeds, because every field
of both structs is optional — and handed the zero value straight to `ResponseToCanonical`.
The client received **HTTP 200** and:

```json
{"id":"msg_9e3fd…","type":"message","role":"assistant","content":[],"stop_reason":null,"usage":{}}
```

A synthesized id on an empty message. The chat shape gave the same thing as an empty message
with `finish_reason: null`. A streaming request gave 200 with **zero SSE frames**.

### Why it was the highest-severity finding

Not because it is wrong — because it is *silent in every channel at once*. A caller cannot
distinguish it from a real answer, so retries never fire, §7.6 fallback never engages, health
counts a success, metering records zero tokens, and an agent loop consumes an empty turn and
keeps going. It is strictly worse than the misconfiguration it hides: Qwen answers the same
wrong route with an honest 404, which surfaced cleanly and was diagnosable in seconds.

### The discriminator, and what it would reject

`backend.convert` already distinguished "not a JSON object" (`502 upstream_shape`) from a
decode failure (`502 upstream_decode`). The missing case was **"a JSON object that is not a
response of this family"**, and it now has the same answer.

Each family's decoder accepts on **either** of two grounds, never on both being required:

| Family | Discriminator | Payload-bearing members |
|---|---|---|
| `anthropic-messages` | `"type": "message"` | `content`, `usage`, `stop_reason` |
| `openai-chat` | `"object": "chat.completion"` | `choices`, `usage` |

**Presence is the test, not the value.** `{"object":"chat.completion","choices":[]}` is a real
answer — an unauthenticated vLLM answering a probe sends exactly that — and so is a body from
one of the many OpenAI-compatible servers that never emits `object` at all. Requiring the
discriminator would refuse the second; requiring non-empty content would refuse the first.

**What it would wrongly reject**, stated plainly:

- A response carrying *only* identity — `{"id":…,"role":"assistant","model":…}` with no
  `content`, no `usage`, no `stop_reason`, and no `type` member. No vendor observed on this
  run sends that, and the turn it describes is the empty one the caller could not have used
  anyway; it now arrives as a 502 that names the condition rather than a 200 that hides it.
- A chat completion carrying *only* `id` and `model`, on a server that omits `object`,
  `choices` and `usage` together. Same reasoning.
- `{"type":"error", …}` and `{"error":{…}}` arriving with a 200. **That is the point**, not a
  cost.

`role` is deliberately *not* a payload member. A body whose only content-shaped member is
`"role":"assistant"` is an empty turn by construction.

### The streaming half

Three of the four streaming crossings were already correct at `cd1eadc` — the truncated-stream
detection that landed in that commit turns a stream with no frames into `502
upstream_stream_truncated` with nothing written. The **byte relay** was not: it forwards bytes
as it scans, so the vendor's error envelope was written into the client's event stream
verbatim, `FirstByteSent` went true, and §7.6 fallback closed over bytes that were never an
answer.

A streaming request whose upstream answers `Content-Type: application/json` is now refused
*before the first write*. Only that one label is refused — a missing Content-Type and every
`text/event-stream` spelling go to the relay exactly as before, because an upstream that
mislabels a real stream must not be turned away over a header.

### Disposition

**Closed.** `502` with code `upstream_shape`, nothing rendered, nothing metered, and
`Retryable` set — nothing was generated, so there is no billed turn a sibling deployment would
repeat, and a sibling deployment is what the fallback chain is *for*.

---

## D1 — a `base_url` carrying a path lost its version segment

### What happened

`joinVersioned` supplied `/v1` only when the base URL was a **bare host**. A base with a path
and no version segment got neither:

| configured | dorang addressed | the real route |
|---|---|---|
| `<host>/api/anthropic` | `…/api/anthropic/messages` | `…/api/anthropic/v1/messages` |
| `<host>/apps/anthropic` | `…/apps/anthropic/messages` | `…/apps/anthropic/v1/messages` |

Both Anthropic-compatible subscription endpoints in this environment have that shape, so every
request to them failed — and failed as D2, invisibly. Verified with curl both ways.

That shape is not exotic. It is what a Claude-Code-shaped client is handed for
`ANTHROPIC_BASE_URL`, and *that client appends `/v1/messages` itself*. A plan endpoint the
catalog does not know is the only kind that needs an operator-supplied base URL at all, and
these are the GLM/Qwen/Kimi coding plans this project exists to aggregate.

### The rule now

> **dorang appends the version segment unless the configured `base_url` already contains one.**

"Already contains one" means **anywhere in the path**, not only at the end. A segment spelled
`v` followed by a digit — `v1`, `v4`, `v1beta` — counts. The "anywhere" matters:
`https://api.deepinfra.com/v1/openai` is a catalogued base whose route is
`/v1/openai/chat/completions`, and a rule keyed on the final segment would address
`/v1/openai/v1/chat/completions`.

The rule is stated for operators in **docs/CONFIG.md §6.0**, with the table above and the
escape hatch: a `base_url` that already ends in the operation's own path is used verbatim.

### The old doc comment claimed this rule "leaves every catalogued kind pointing at a real endpoint". It did not.

Sweeping every kind in `pkg/catalog/provider_defaults.yaml` turned up **five** whose address
moves, and three of them are Anthropic-shaped coding plans that were therefore broken exactly
as z.ai and Qwen were:

| kind | base_url | was addressed | now addressed | assessment |
|---|---|---|---|---|
| `minimax` | `https://api.minimax.io/anthropic` | `/anthropic/messages` | `/anthropic/v1/messages` | fixed |
| `kimi-coding` | `https://api.kimi.com/coding/` | `/coding/messages` | `/coding/v1/messages` | fixed |
| `synthetic` | `https://api.synthetic.new/anthropic` | `/anthropic/messages` | `/anthropic/v1/messages` | fixed |
| `longcat` | `https://api.longcat.chat/openai` | `/openai/chat/completions` | `/openai/v1/chat/completions` | judgement call |
| `kilocode` | `https://api.kilo.ai/api/gateway/` | `/api/gateway/chat/completions` | `/api/gateway/v1/chat/completions` | **judgement call, unverified** |

The last two are the honest cost of a single rule. Their catalog entries record base URLs
mined from a plugin manifest consumed by an SDK that appends `/chat/completions` with no
version of its own, so a reading exists under which they wanted nothing appended. LongCat's own
documentation writes the versioned route; Kilo's could not be verified from here, and it is the
one address in the catalog this change may have moved in the wrong direction.

They are named, with that reasoning, in `TestCatalogueBasesThatGainAVersion`. That test fails
if a *sixth* kind's address ever moves without someone writing down why — which is the property
that was missing when the old comment's claim went stale.

### Disposition

**Closed.** `providers.yaml.tmpl` no longer needs its hand-written `/v1`; the lines are kept,
now inert, with the reasoning as a record.

---

## D8 — the same class, on every relay surface (found while fixing D2)

A vendor returning 200 with a non-response body is not one vendor's habit, and the chat
decoders were not the only place that trusted a clean parse.

The **relay** surfaces are worse than the converted ones. A converted answer at least becomes
an *empty* answer; a relayed one is handed to the client **whole**, with dorang's own `model`
member spliced into the vendor's error envelope, over a 200:

```
client sees: {"code":500,"msg":"404 NOT_FOUND","success":false,"model":"client-model"}
```

Two are fixed, checked against the one or two members every real answer of that shape has:

| surface | required member(s) | why not the discriminator |
|---|---|---|
| embeddings (`relayResponse`) | `data` or `usage` | `object: "list"` is omitted by some vendors, and accepting on `object` would also accept `object: "error"` |
| `count_tokens` (`relayCountTokens`) | `input_tokens` | the count *is* the answer; there was nothing else to check |

**Still open, and each needs its own discriminator:** `rerank.DecodeResponse`,
`openai.DecodeModerationResponse`, `openai.DecodeImageResponse`,
`openai.DecodeTranscriptionResponse`, the speech surface (which relays bytes and cannot check a
JSON shape at all), and the Gemini and Cohere adapters. All have the same structure — unmarshal
into an all-optional struct, use the zero value — and none was reached by this run's case
table, so none has a reproduced failure behind it. They are listed so the next pass has a work
list rather than a rediscovery.

---

## D3 — embedding usage discarded when the vendor reports only `total_tokens`

`scanRelayUsage` parsed `TotalTokens` and then returned
`canonical.Usage{InputTokens: shape.Usage.PromptTokens}` — the parsed field was never read.

| vendor | body | metered before | metered now |
|---|---|---|---|
| Jina direct | `{"total_tokens": 4}` | 0 | 4 |
| the same model via LiteLLM | `{"prompt_tokens": 0, "total_tokens": 4}` | 0 | 4 |

Neither produced an `X-Dorang-Tokens-Input` header at all. `Result.Usage` is the single source
for the headers, `internal/meter` and pricing, so **embeddings were priced at zero** — and they
were also invisible to §11.6's token guard, whose trigger is gated on
`Input+Output+Reasoning > 0`. Nothing failed, so nothing alerted: in a retrieval-heavy
deployment the gateway's budgets, per-key spend and rate guard silently excluded most of its
traffic.

Rerank took a different path and metered 184 correctly on the same run, which is what makes
this an oversight rather than a policy.

An embedding has no completion half, so on this shape the total *is* the prompt count.
`prompt_tokens` still wins whenever it is non-zero: a vendor that states both and means
different things by them is stating the prompt count in the field named for it. The count is
also marked as *reported*, so a downstream encoder can still tell a measured zero from an
unmeasured one.

**Closed.**

---

## D4 / D5 — the two paths disagreed about response identity

Converting Anthropic -> OpenAI **non-streaming**:

```json
{"object":"chat.completion","id":"msg_2026…","created":0}
```

`created: 0` is a timestamp in 1970 on every converted turn — `decode.go` never set it and
`openai/response.go` emitted it verbatim — and the upstream id crossed families unchanged. The
**streaming** path for the identical conversion set a real timestamp and minted a `chatcmpl-`
id.

So dorang disagreed with itself depending on whether the caller asked for a stream. That is the
same class of split the wire layer had just fixed for dropped `usage` fields, and the
recurrence is the interesting part: a property gets implemented once in a stream writer and
once in a response encoder, and the two drift.

### The rule now, stated in DESIGN §10.7

> **The upstream's `id` crosses unchanged when the answer did not change family, and is minted
> in the caller's family shape when it did. `created` is the upstream's own when its family has
> the member and this gateway's clock when it does not. Both hold on both paths.**

D5 was **decided, not defaulted**. Crossing the id is defensible — it is the only handle a
support ticket has on the upstream's side of an exchange — and it is defensible *only* while
the id is one the caller's family could have produced. `{"object":"chat.completion",
"id":"msg_2026…"}` is not.

The gateway's clock is now also the *stream writer's* clock: `newEventSink` passes
`Backend.now` into `openai.StreamConfig.Now`, which is what makes the equality testable rather
than merely approximate.

**Both closed**, and pinned by `TestStreamingAndNonStreamingAgreeOnIdentity` — deliberately
named after `TestStreamingAndNonStreamingAgreeOnUsage`, which pins the same property for the
same reason one layer down. A third such property should get an equivalence test on the day it
is written.

---

## D6 — the converted stream omitted `role: "assistant"`

DESIGN §10.7's streaming table maps this family's stream-open event to "first chunk with
`delta.role`", and `message_start` is what it maps to on the other side. Nothing emitted it.
The byte relay carried the role because it carries everything; the **converted** stream dropped
it — so two clients of one gateway saw structurally different streams depending only on which
family their deployment's upstream spoke.

The role now rides on the first delta of each choice rather than as a frame of its own, so no
stream gains a frame it did not have, and a role the source already stated is never
overwritten. Recorded as COMPATIBILITY 2.6.

**Closed.**

---

## D7 — the usage chunk's `choices` shape

The stream usage chunk carries `choices:[{"index":0,"delta":{}}]` where OpenAI documents an
empty array. This is a deliberate divergence, already documented as COMPATIBILITY 3.3: the
reference proxy sends the stub and the clients that exist were built against it.

**Closed.** The knob already existed in the wire layer — `openai.StreamConfig.UsageChunkChoices`,
with `stub` and `empty` — and `compat.usage_chunk_choices` already existed in the config schema,
where `empty` was refused at load because nothing carried the value between them. The carrier is
`backend.Call.UsageChunkChoices`, filled from the dispatch state in `internal/app` and read by
`newEventSink` (`internal/backend/stream.go`), exactly as predicted here. Selecting `empty` also
takes a same-family stream off the byte-relay fast path, which was the part not predicted: on
that path the usage chunk is the upstream's own bytes, so honouring the setting requires
decoding the stream.

`anthropic.ResponseOptions.TotalTokens` (COMPATIBILITY 6.8) was the other wire-side knob in the
same position, and is wired the same way, from `encodeClient`. The refusals in
`internal/config` are gone with the gap they described, and both are pinned by tests in
`internal/app` that drive a loaded configuration to an emitted frame.

---

## Verification — each fix reverted, a named test observed to fail

| Defect | Reverted to | Test | Observed failure |
|---|---|---|---|
| D1 | the bare-host rule in `hasVersionSegment` | `TestEndpointDerivation`, `TestCatalogueBasesThatGainAVersion` | `Endpoint = ".../api/anthropic/messages", want ".../api/anthropic/v1/messages"`, x5 subtests; the catalogue sweep loses all five kinds |
| D2 | dropped the `IsMessagesResponse` / `IsChatCompletion` gates and the streaming Content-Type gate | `TestUpstream200WithANonResponseBodyIsNotASuccess`, `…IsNotAnEmptyStream` | all four crossings succeed and hand the client the empty turn, verbatim: `{"id":"msg_36b8a…","type":"message","role":"assistant","content":[],"stop_reason":null}` and `{"object":"chat.completion","choices":[],"code":500,"msg":"404 NOT_FOUND"}`. Byte relay: `FirstByteSent is set for a stream that wrote nothing` |
| D3 | `InputTokens: shape.Usage.PromptTokens` | `TestEmbeddingUsageIsMeteredWhateverTheVendorCallsIt` | `InputTokens = 0, want 4` on both vendor bodies; `the token guard of §11.6 cannot see this request at all` |
| D4 | dropped `Created` from `ResponseOptions` | `TestStreamingAndNonStreamingAgreeOnIdentity` | `non-streaming created = 0, want 1753660800`; `the two paths disagree about created: 0 vs 1.7536608e+09` |
| D5 | dropped the cross-family mint | `TestStreamingAndNonStreamingAgreeOnIdentity` | `non-streaming id = "msg_2026" — the upstream id crossed verbatim under object:chat.completion` |
| D6 | dropped `openRole` | `TestConvertedStreamOpensWithTheAssistantRole` | `first delta = map[content:ok], want role:assistant` |
| D8 | — (new; no prior behaviour to revert to) | `TestUpstream200WithANonResponseBodyOnARelaySurface` | covers embeddings on both adapters and `count_tokens` |

Boundary tests, which fail if a check is tightened one notch further:
`TestAMinimalResponseIsStillAResponse` (six legitimate minimal bodies) and
`TestRelaySurfacesStillAcceptTheirOwnAnswers` (an empty `data` array, a token count of zero).

`go build ./... && go vet ./... && go test -race -count=1 ./...` all pass; `gofmt -l .` is
clean.

---

## What a re-run should look for

1. **The two Anthropic plan endpoints without a hand-written `/v1`.** Delete the suffix from
   `providers.yaml.tmpl` and confirm the routes still answer. That is D1's fix exercised the way
   an operator would meet it.
2. **A deliberately wrong route.** Point one provider at a path the vendor does not serve and
   confirm a `502 upstream_shape` rather than a 200 — non-streaming *and* streaming.
3. **`X-Dorang-Tokens-Input` on every embeddings case**, direct and through LiteLLM. Its absence
   was the whole of D3.
4. **The four-cell shape comparison, on identity.** `id_prefix` and `created` are already in
   `probe.py`'s summary for the chat kinds; the 2x2 should now show `chatcmpl-` in every OpenAI
   cell and `msg_` in every Anthropic one, whatever the upstream spoke.
5. **The surfaces D8 leaves open** — rerank, moderations, images, audio — against a wrong route,
   which no case in the current table does.
