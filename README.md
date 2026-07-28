# dorang

> *dorang* (도랑) — the small channel beside a stream that splits, holds, and rejoins the flow.

**dorang** is a high-performance LLM gateway written in Go. It sits between your
applications and your model providers, and it is designed to run the same way on a
laptop as it does on a fleet — **notebook to enterprise, one binary, no required
dependencies.**

**한국어 문서: [README.ko.md](README.ko.md)**

---

## Why

Most gateways make you choose between *capable* and *fast*. dorang refuses the trade:
every feature below is designed so the request hot path stays allocation-free and the
cost of adding protocols is constant, not linear.

### Capacity that matches how subscriptions actually work

Real provider plans do not have one concurrency number. One account may allow 3
in-flight requests **across all models**, while a coding plan on another provider allows
7 in flight **per model**, so two models means 14. dorang models this directly:

```yaml
capacity:
  credential_groups:
    account-a: { max_concurrency: 3 }        # per account, any model
    account-b: { max_concurrency: 3 }
  models:
    - { provider: coding-plan, model: model-x, max_concurrency: 7 }   # per (key, model)
    - { provider: coding-plan, model: model-y, max_concurrency: 7 }
```

Five axes — route, provider group, model, credential group, key — are reserved
**atomically, all-or-nothing**, so a waiter never holds one slot while queuing for
another. Deadlock is structurally impossible.

### Quota and budget that actually stop

- Rolling **5-hour / daily / weekly / monthly** windows over tokens, requests, or cost.
- Polls provider-reported quota endpoints where they exist, so the number reflects
  *all* usage of that key — not just what went through the gateway.
- On exhaustion a credential steps aside and traffic moves to the next key automatically.
- Budgets **reserve before spending**, so concurrent requests cannot overshoot the cap.

### Prefix-aware routing that cannot get the order wrong

Routing to the backend that still holds your KV cache requires knowing the conversation
prefix matched *in order*. dorang chains the hashes:

```
h₀ = H(group)                        hᵢ = H(hᵢ₋₁ ‖ len(cᵢ) ‖ cᵢ)
```

A match at depth *i* proves chunks `c₁..cᵢ` are byte-identical **and in that order**.
Reordering, insertion, or deletion all produce a different value.

### Telemetry you never have to turn off

Per-key usage, cost, latency, TTFT, token counts, and fallback paths are recorded for
every request. Measurement runs off the hot path through per-CPU ring buffers, and the
design target is that turning it on costs less than 5%.

### Full protocol surface

OpenAI chat/completions/embeddings/responses/audio/images/moderations, Anthropic
messages, rerank, batch, files — plus a **generic passthrough engine** so provider-native
routes are opened by configuration rather than by writing another adapter.

---

## Scaling range

| | Notebook | Small team | Enterprise |
|---|---|---|---|
| Storage | SQLite (embedded) | PostgreSQL | PostgreSQL + partitions |
| Coordination | none | none | Redis or PostgreSQL leases |
| Nodes | 1 | 1–2 | N, leader-elected maintenance |
| Telemetry | in-process, sampled | full ledger | ledger + rollups + retention |
| Dependencies | **none** | 1 | 2 |

The same binary and the same config file work across the whole range. You scale by
changing configuration, not by changing deployment model.

---

## Status

🚧 **Design stage.** Implementation has not started; there is no runnable code yet.

- [Design](docs/DESIGN.md) — architecture, configuration schema, algorithms, milestones
- [Adversarial review](docs/REVIEW.md) — findings against this design and their dispositions

## License

[Apache License 2.0](LICENSE). Distributions must preserve the dorang attribution in
[NOTICE](NOTICE), using the standard Apache 2.0 §4(d) NOTICE mechanism.
