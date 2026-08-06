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

## Why dorang cannot use `/responses` yet

The adapter comment names the blocker itself, and it is not the routing:

> internal/wire/openai gained a Responses request/response encoder, but no
> Responses STREAM decoder — that surface's events are a typed sequence
> (`response.output_text.delta` and friends) with nothing in common with a chat
> chunk. Routing the kind to /v1/responses today would trade a working streaming
> deployment for a non-streaming one. […] switching the kind over is one line in
> adapterFor the day a stream decoder exists.

So the decoder is the work and the routing is its consequence. For codex it is
not optional in either direction: that host **requires** `stream: true`, so
without a stream decoder there is no way to reach it at all.

## The order this has to be done in

1. **A Responses stream decoder** — the typed event sequence into the canonical
   stream, including tool calls, reasoning blocks and the terminal usage.
2. **Per-kind routing** — hosts that serve both keep the chat route, hosts that
   serve only `/responses` get it. One line each, once (1) exists.
3. **A way for a deployment to state `store: false` and `stream: true`** — codex
   refuses without them and there is no configuration field today.
4. **A credential error that names its cause.** Mounting the operator's
   `~/.codex/auth.json` (0600, uid 1000) into a distroless image running as
   `nonroot` produced `credential_unavailable` and nothing else. Unreadable,
   expired and malformed are three different operator actions and they currently
   share one message.

Items 2 and 3 are small and blocked on 1. Item 4 is independent.
