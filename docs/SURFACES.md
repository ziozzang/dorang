# Which surface each provider actually serves

A provider's *documented* surface and its *served* surface are different things,
and dorang routes on the second. This file is the measurement, the date it was
taken, and what dorang does with it.

Measured 2026-08-06 by sending a minimal well-formed body to each path on each
of the operator's live plans and recording the status. A `404` means the route
is not there; a `400` means it is there and the body was wrong, which is the
answer that matters most — it distinguishes *absent* from *reachable and picky*.

| provider | `/chat/completions` | `/messages` | `/responses` |
|---|---|---|---|
| Ollama Cloud | 200 | **200** | **200** |
| qwen token plan | 200 | 404 | **200** |
| z.ai coding plan | 200 | 404 | 404 |
| local llama.cpp | 200 | 200 | 200 |
| codex (ChatGPT OAuth) | **403** | — | **200** |

## What each row costs today

**z.ai coding plan** serves chat and nothing else, so dorang's current routing is
already correct for it. Nothing to do.

**Ollama Cloud serves all three** and dorang uses one. A caller's Anthropic
`/v1/messages` request is converted to chat before it goes out, and every
construct §10.1 reports as lost on that crossing — `thinking` blocks, multi-block
tool results, per-block cache breakpoints — is lost against a host that would
have accepted it natively.

**qwen token plan serves `/responses` and not `/messages`**, which is the
opposite of what was assumed. The live configuration's own comment records why
it uses `openai-chat`: the Responses surface "flaked once in nine calls with an
upstream 500". That is a reason to prefer chat, and it is not the same as the
surface being absent — the note should say *unstable*, not *unavailable*.

**codex serves `/responses` and only `/responses`.** dorang sends it to
`/v1/chat/completions` and gets a 403. The reason is in
`internal/backend/openai.go`, and it is a stale premise rather than an oversight:

> DESIGN §4.3 records that the hosts declaring this kind serve BOTH routes, so
> the chat route is a correct address for them

Measured: `POST /backend-api/codex/chat/completions` → **403**;
`POST /backend-api/codex/responses` → **400** (`store` and `stream`, below). The
premise is false for this host, so the conclusion built on it fails here.

## The codex surface enforces two fields

Both are mandatory and each is refused separately:

```
store:false + stream:true   -> 200
stream:true alone           -> 400  {"detail":"Store must be set to false"}
store:false alone           -> 400  {"detail":"Stream must be set to true"}
neither                     -> 400
```

Headers are not the gate — the same call with `User-Agent: dorang/dev` and
without `chatgpt-account-id` or `OpenAI-Beta` still answers 200. `store` and
`stream` are the whole contract.

A third fact, measured 2026-09-12 through dorang: the host refuses an output
ceiling — `POST /responses` with `max_output_tokens: 16` answers
`400 Unsupported parameter: max_output_tokens`, buffered and streaming alike.
That is what `providers[].params.drop` is for ("this endpoint answers 400 to
this field"), so the codex provider carries `params: {drop: [max_tokens]}`, and
the removal is reported to the caller as `X-Dorang-Dropped-Params: max_tokens`
(with `X-Dorang-Detail: full`) — observed on the collected buffered path, which
is the proof that the loss report survives collection rather than being lost on
a side path. A caller's ceiling is therefore not honoured by this host and the
caller is told so; nothing quieter would be honest.

## How dorang reaches `/responses`

The adapter comment used to name the blocker itself, and it was not the routing:

> internal/wire/openai gained a Responses request/response encoder, but no
> Responses STREAM decoder — that surface's events are a typed sequence
> (`response.output_text.delta` and friends) with nothing in common with a chat
> chunk. […] switching the kind over is one line in adapterFor the day a stream
> decoder exists.

The decoder exists now (`internal/wire/openai/responsesstream.go`, tested
against a verbatim capture of this host), and the routing followed:

- The catalog kind carries `responses_only: true` (`codex-responses`). That one
  declaration selects `responsesAdapter` in the backend AND gates the two
  provider settings below at start-up, so there is no second list to disagree
  with it. A kind declaring the flag next to any api but `openai-responses` is a
  catalog lint error.
- `providers[].params.force_stream` / `store_false` state the host's contract
  once. They are refused on any kind that is not `responses_only`, because only
  this adapter reads them.
- A non-streaming caller still receives one buffered answer. The stream is read
  back and collected into the neutral form INSIDE the adapter's decode, which is
  what makes the answer pass through the same served-model note, tool-argument
  check, §10.5b transform and client encoder as every JSON answer — an Anthropic
  Messages caller gets a message, a `/v1/responses` caller gets a response. A
  stream that stops before its terminal event is `upstream_stream_truncated`; an
  error event is `upstream_stream_error` with the credential scrubbed exactly as
  the relay scrubs it; neither is offered to the fallback chain, because the
  host bills the generation it began.
- `response.failed` is an error on both paths (scrubbed, terminal), never a
  stop; a `data:` payload that is not JSON fails the collection rather than
  leaving a hole; a transport failure under the forced stream is read for what
  arrived, so a terminal event that got through is still one answer.
- Measured end to end against the live surface, buffered and streaming, from a
  distroless container holding the operator's `~/.codex/auth.json`.

## The order this was done in

1. **A Responses stream decoder** — done. The typed event sequence into the
   canonical stream, including tool calls, reasoning blocks and the terminal
   usage with `tool_usage` (server-side web search and image spend, priced).
2. **Per-kind routing** — done, by the catalog's `responses_only` flag rather
   than a kind-name table, so an operator's own Responses-only host under any
   kind gets the same adapter by declaring it.
3. **A way for a deployment to state `store: false` and `stream: true`** — done,
   `params.force_stream` / `params.store_false`, refused where inert.
4. **A credential error that names its cause** — done. Unreadable, malformed and
   read-only are three categories from `internal/auth`'s own sentinels, never
   from provider text.
