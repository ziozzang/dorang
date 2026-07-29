# Adversarial Review — Round 1

> Reviewed: `docs/DESIGN.md` (revision 1), 2026-07-28
> Method: independent adversarial passes, each instructed to **default to refuting**.
> Reviewers were given the design plus read access to the reference codebases and the
> live environment, and were told that "looks fine" is a worthless conclusion.
>
> 한국어: [REVIEW.ko.md](REVIEW.ko.md)

Findings are recorded with their disposition. `ACCEPTED` means revision 2 of the design
changes to address it. `ACCEPTED (reframed)` means the defect is real but the reviewer's
proposed fix was not the one taken. `PARTIAL` means the finding rests on an assumption
that does not fully hold.

---

## R1-A — Empirical verification: credential hashing and migration premise

The design claimed existing gateway API keys could be adopted without reissue by matching
the incumbent's hashing. This was tested against the actual source and database rather
than assumed.

### Verdict: mechanism CONFIRMED, motivation REFUTED

| Claim | Result |
|---|---|
| Hashing is plain `sha256(token).hexdigest()` | **CONFIRMED.** Single function, one argument, no salt/HMAC/pepper/master-key participation. Verified against installed package source and independently against a second on-disk copy at a different version — byte-identical. |
| The full `sk-...` string including prefix is hashed | **CONFIRMED.** No stripping or normalization. |
| Non-`sk-` keys are rejected before lookup | **CONFIRMED.** Hard 401 gate, explicitly to prevent token hashes being replayed as keys. |
| All stored tokens are 64-char lowercase hex | **CONFIRMED.** 54/54 rows, min length 64, max length 64, zero plaintext, zero mixed formats. |
| Master key participates in key hashing | **REFUTED.** Compared in constant time against the **raw** value, before any DB lookup. It is not a row in the token table. A gateway that only reads the DB silently loses admin auth. |
| **"54 keys keep working" is a meaningful benefit** | **REFUTED.** **48 of the 54 are already expired** — the most recent lapsed 27 days ago, the oldest over a year ago. Those 48 also reference a team that no longer exists. **Only 6 keys are live**, and all 6 are clean: zero spend, no budgets, no metadata. |

### Disposition: ACCEPTED (reframed) — this changes the design's motivation, not only its details

The original design (§11.2a) justified permanently adopting the legacy digest as
"no reissue for 54 keys". That justification does not survive contact with the data.
Adopting an unsalted single-round digest is a **permanent cryptographic constraint** with
no rekeying path, and a stolen database dump is directly attackable against any weak
operator-supplied key. Trading that for **six** key rotations is a bad trade.

Revision 2 therefore:

- Makes `dorang_v1` (`HMAC-SHA256(pepper, token)`) the **only** scheme for keys dorang issues.
- Keeps `legacy_sha256` strictly as an **import-time compatibility mode**, off by default,
  with a mandatory expiry date in configuration. It exists to make a migration window
  possible, not to be an architecture.
- Requires the importer to **refuse to resurrect expired credentials**. Honoring `expires`
  is mandatory; a migration that ignored it to make a headline number true would silently
  restore revoked credentials.
- Records that the following authorization columns must be carried or the gateway fails
  **open**: `expires`, `blocked`, `models`, `allowed_routes`, `max_budget`/`spend`/reset,
  `tpm_limit`/`rpm_limit`, `team_id`, `user_id`, object-permission references.
- Records that the admin/master credential is **out-of-band**, never a stored row.
- Notes that the incumbent's `key_name` column stores the last 4 characters of the key in
  clear. An importer must **not** copy it verbatim; dorang stores a non-reversible
  display prefix instead.

Also corrected: the design described model configs as "encrypted with the master key".
More precisely, they are sealed with XSalsa20-Poly1305 under `sha256(salt_key)`, where the
salt key falls back to the master key when a dedicated salt is unset — which is the case
here. **Rotating that key during a migration destroys every stored provider credential.**
The importer must therefore read credentials *before* any key rotation, and say so.

---

## R1-B — Performance, data model, and milestone integrity

18 findings. Summary of those accepted, grouped by the design decision they invalidate.

### Metering (3 findings) — ACCEPTED

1. **Ledger loss under back-pressure contradicts the tracing requirement.** The design said
   a full ring buffer drops ledger rows and keeps only rollup counters. If the database
   stalls, per-request traces vanish exactly when they are most needed.
   → Revision 2 spools to a local durable queue before the database, and exposes an
   explicit `metering_degraded` state rather than losing data silently.

2. **"Keep numeric rollups accurate while dropping the ledger" is not free.** Accurate
   multi-dimensional rollups require the producer to touch a keyed aggregate, which is not
   the stack-allocated, lock-free operation the design assumed.
   → Revision 2 splits metering into **two queues**: a fixed-cardinality numeric
   accumulator per CPU (always maintained, cheap, mergeable) and a variable-length trace
   payload queue (droppable, with the drop surfaced).

3. **Stored message excerpts do not belong inline in the request ledger.** At the top of the
   stated scale range the excerpt column alone dominates storage.
   → Revision 2 moves excerpts to a separate sampled table with a byte budget, and the
   design now states expected row size, daily volume, and retention per scale tier.

### Prefix routing (3 findings) — ACCEPTED, with a different fix

4. **Token-boundary chunking cannot meet the latency target.** Chunking at *token*
   boundaries requires tokenizing the whole conversation before routing. On a long
   conversation this alone exceeds the entire latency budget, and it contradicts the
   design's own rule against buffering full request bodies.
   → Revision 2 chunks on **byte** boundaries over the raw request bytes as they stream in.
   No tokenizer on the hot path. The hash chain property (order integrity) is unchanged —
   it never depended on tokens. Latency targets are now stated per input-size tier.

5. **A fixed 16-step depth cannot distinguish long prefixes.** Two conversations sharing a
   system prompt but diverging afterwards collide at the deepest tracked node, so the
   "longest common prefix wins" promise is false beyond that depth.
   → Revision 2 uses **logarithmic checkpoints**: fine-grained early, then doubling. Long
   prefixes are distinguished without a linear increase in entries.

6. **Per-entry memory was underestimated by roughly 2–3×**, and a single request writes an
   entry at every depth, so the table fills far faster than assumed.
   → Revision 2 budgets by **bytes, not entry count**, interns deployment identifiers as
   integers, and states a measured retained-size budget as a milestone gate.

### Capacity (1 finding) — ACCEPTED, and it is the most important one

7. **Broadcast wakeup produces a thundering herd.** With many requests queued behind a small
   limit, releasing one slot wakes every waiter; one succeeds and the rest re-acquire the
   global lock, re-check, and sleep again. Failed lock acquisitions can dominate useful
   work, and arrival order is not preserved.
   → Revision 2 replaces broadcast with **per-axis FIFO wait queues and targeted wakeup**:
   a release hands the slot to a bounded number of eligible waiters directly. This also
   restores fairness, which broadcast did not provide. A saturation benchmark
   (0/100/1000 waiters × limits of 1/7/32) becomes a milestone completion gate.

> Note: the reviewed design inherited its broker shape from a single-operator coding agent,
> where the waiter count is small enough that broadcast is harmless. The defect only appears
> at gateway concurrency. This is a good example of why inherited mechanisms need
> re-validation against the new load profile rather than being adopted on reputation.

### Quota and budget (4 findings) — ACCEPTED

> Findings 19–21 were raised later, during implementation, and are numbered at the end of
> this section's sequence rather than inserted — renumbering would break every `[R1-n]` tag
> already carried in the design.

8. **Asynchronous local counting and multi-node accuracy are mutually exclusive as written.**
   Either nodes overshoot the limit while their counts converge, or every request contends
   on one shared row and the "no database on the hot path" rule breaks.
   → Revision 2 applies the same **reservation/lease** model already used for budgets:
   a node leases a block of quota and decrements locally. Small limits force an atomic
   shared path. Each mode now declares its **maximum possible overshoot as a number**,
   rather than implying exactness it cannot deliver.

19. **Budget reservations had no expiry, while capacity reservations did.** The two are the
   same reserve-then-settle pattern, but only one had a safety net. A process killed between
   reserving and settling locks that amount forever, and over time a budget is exhausted by
   money nobody spent — a false stop with no way to diagnose it.
   → Revision 2 gives the budget hold an expiry and a leader-run reclaim, matching §5.3.
   Same pattern, same net.
   → **Correction.** Revision 2 put the expiry on `budget_state.reserved_until`, swept by a
   leader job. That mechanism was implemented, tested and never called — `ReserveBudget` had
   no caller outside its own tests — so the sweep queried a predicate that could not match.
   The hold that exists is §9.6's lease block, whose `quota_leases.expires_at` the leader's
   lease-reclaim pass already honours; the reservation code and its two columns are deleted
   (migration `0006`). The net is wider than the one described here: it also returns a block
   held by a node that died after the block was charged, which the reservation sweep could
   not reach. See DESIGN §6.4.

20. **A request could consume budget without ever reaching an upstream.** Budget was reserved
    at the gate; the request can then be refused while waiting for capacity and never
    dispatch. Revision 1 defined settlement only for completion, so that reservation leaked.
    → Revision 2 makes the gate hold **soft**, hardening it only once capacity is acquired,
    and refunds in full for anything that fails before dispatch.

21. **Budget refusal must not be a fallback trigger.** Exceeding a budget surfaces as a
    terminal `400`, deliberately not `429`. A `429` is a rate-limit signal and would send the
    request down the fallback chain (§7.6) to spend a *different* subject's budget on a model
    the caller never asked for. The error type carries an explicit terminal flag so this
    cannot be re-derived incorrectly downstream.

### Pricing (3 findings) — ACCEPTED

9. **A single-winner rule model cannot express subscription plus per-token cost.** A
   credential-scoped subscription rule outranks the model-scoped token rule, zeroing token
   cost — or, if rules were summed, it contradicts the stated "most specific wins".
   → Revision 2 splits rules into **classes** (`marginal_usage`, `fixed_subscription`,
   `adjustment`); each class picks its own winner and the results compose. Subscription
   amortization gets one stated formula, and routing-time estimated cost is kept separate
   from accounting-time imputed cost.

10. **Linear rule matching per candidate per request is a hot-path cost that was never
    counted.** With many rules and many candidates this becomes millions of predicate
    evaluations per second.
    → Revision 2 pre-indexes rules by their static dimensions at configuration load, leaving
    only time-dependent predicates for request time, and caches per-candidate results
    within a request.

11. **Integer nano-unit arithmetic does not by itself give precision or overflow safety.**
    Sub-nano per-token prices round to zero or one per request, a systematic error; and
    lifetime global accumulation can overflow a signed 64-bit nano value.
    → Revision 2 parses prices as exact decimals, multiplies in 128-bit intermediate
    precision, rounds once at the end with carried remainder, and range-checks before
    storage.

### Storage and schema (3 findings) — ACCEPTED

12. **The request ledger has no index supporting its own query API.** Every documented
    lookup — by key, by team, by trace id, by credential — degrades to a partition scan.
13. **The hourly rollup's full-cube primary key both bloats indexes permanently and creates
    hot-row contention** when one key dominates a bucket.
14. **Partition maintenance was scheduled six milestones after the writer that needs it**,
    so inserts would fail at the first midnight after metering ships.
    → Revision 2 fixes the query set first and derives indexes from it; splits rollups into
    a small number of purpose-built materializations with per-node pre-aggregation before
    merge; and moves partition pre-creation and retention into the same milestone as the
    writer, leaving only leader-election scoping to the clustering milestone.

### Targets and process (4 findings) — ACCEPTED, and this changes how the work is sequenced

15. **The latency targets had no measurement boundary**, so paths that obviously exceed them
    (cold authentication, shared-state capacity, external auth, extension hooks) could be
    excluded to make the number true.
    → Revision 2 states targets **per profile** and defines exactly which timestamps bound
    "gateway overhead".
16. **The throughput target did not specify a workload**, so it could be passed by a
    configuration that exercises none of the real work.
    → Revision 2 fixes a workload specification and reports two separate numbers: what the
    server can serve, and what the storage layer can sustain without backlog.
17. **The metering overhead target was only meaningful in steady state.** It is now measured
    under consumer lag and at a full buffer as well.
18. **Performance validation was deferred to the final milestone.** Discovering a structural
    problem in capacity waiting, ledger throughput, or prefix cost at integration time would
    invalidate everything built on top.
    → Revision 2 attaches a **specific performance gate to each milestone**, and the final
    milestone becomes integration regression only, not first discovery.

### One finding held as PARTIAL

The storage-volume argument assumed the top of the stated scale range as a *sustained*
rate. That is a capability ceiling, not a steady-state load, so the headline volume figure
overstates the typical case. The underlying defect is still real — the design had no
storage budget at all — so the fix is accepted; only the framing is adjusted. Revision 2
states volume per scale tier rather than a single number.

---

---

## R1-C — Protocol conversion and streaming correctness

The sharpest pass. Three findings invalidate specific mechanisms rather than parameters.

### C1 — Streaming alias rewrite by byte offset is impossible — ACCEPTED

The design promised zero-copy relay of upstream stream bytes *and* rewriting the response
`model` field to the requested alias, by patching the first frame at a byte offset without
decoding. These cannot both hold:

- **The replacement is a different length in the normal case.** An alias exists precisely so
  the two names differ. In-place byte swapping requires equal length, which is the exception.
- **Frame boundaries are arbitrary.** A raw copy reads at buffer boundaries, so the field can
  straddle two reads; a first-buffer assumption either misses it or corrupts JSON.
- **The field is not reliably in the first frame.** Some backends omit it early.
- **Compression** makes byte patching impossible outright.

→ Revision 2 withdraws the zero-copy claim and states what is actually achievable: request
`identity` encoding upstream (which is required anyway to read the terminal usage frame),
and run a **single-pass line-oriented scanner** that rewrites only the `model` value and
carries partial frames across reads. It short-circuits to a plain copy when no rewrite is
needed.

> Independent confirmation arrived from a separate audit of a live deployment: the reference
> implementation restamps `model` on **every** chunk. Per-frame rewriting is the required
> behavior, not an optimization to be avoided. Revision 1 was optimizing away a correctness
> requirement.

### C2 — Reasoning output cannot be "re-assembled" across protocols — ACCEPTED

Revision 1 said reasoning output is re-assembled into the caller's protocol shape. Where a
protocol attaches integrity-protected reasoning blocks that must be echoed verbatim on the
next turn of a tool-use exchange, a gateway cannot synthesize a valid one from a different
backend's plain-text reasoning field. The failure lands on the **second** turn of an agentic
flow — the worst place to find it, and precisely the flow the design names as a target.

→ Revision 2 makes reverse mapping explicitly best-effort and lossy, stores
integrity-bearing blocks as **opaque handles** replayed byte-identically, prefers a capable
backend through capability routing, and fails with a named `400` rather than forwarding a
fabricated block or silently dropping one.

### C3 — Model-name syntax was self-contradictory — ACCEPTED

The analysis declared colons literal and any parsing of them "immediately incompatible",
then the design introduced provider-prefix inference, `provider/model` splitting during
import, and a group-id normalization feeding the prefix hash. Real model names use colons
three different ways — vendor prefix, family tag, and deployment variant — so inconsistent
normalization would make identity, routing, aliasing, and prefix affinity **all**
non-deterministic for the same model.

→ Revision 2 fixes one rule: **model names are opaque, colons are always literal, and
provider identity comes only from explicit configuration fields, never from parsing the
model string.** Prefix-based capability inference operates on the configured upstream model
for *defaults only* and never for identity. `model_group_id` has exactly one normalization
function, used everywhere. Real colon-bearing names are frozen as golden tests.

### C4 — Reasoning budget must be a function of the request — ACCEPTED

A fixed effort→budget table breaks against a small requested output ceiling: either every
effort level collapses to the same value and the scale is meaningless, or the gateway raises
the caller's output limit — silently multiplying their bill. Revision 1 said only
"reconciled" and picked neither.

→ Revision 2: `budget = min(table[effort], max_tokens − reserve)`; below the minimum useful
budget, reasoning is disabled and reported. **dorang never raises a caller's `max_tokens`.**

### C5 — Capability was keyed on provider, not model — ACCEPTED

Revision 1 keyed reasoning folding on the provider kind, and generalized a two-level effort
scale observed on **one model version** to a whole family. On the family's other members the
reasoning control would be silently ignored or rejected.

→ Revision 2 keys folding on **(kind, model capability)** from the catalog, with
version-aware matching. Unverified capability is `unknown`: the control is omitted and
reported, never guessed. Each capability records the date it was empirically verified.

### C6 — A header cannot express structural loss — ACCEPTED

`x-dorang-dropped-params` carries a flat list of parameter names. That is adequate for a
missing knob and useless for a construct that cannot be expressed at all — cache
breakpoints, multi-block tool results, document blocks, richer stop reasons. Returning `200`
after discarding a document or destroying a caching strategy is **worse than an error**.

→ Revision 2 separates droppable parameters from structural downgrades: the latter
**fail fast with `400`** naming the construct, are opt-in-able via an explicit header, and
influence routing — capability becomes a routing filter so a request is preferentially sent
to a backend that can express it.

### C7 — Stateful Responses had no storage — ACCEPTED

The design promised strict compatibility for the Responses API, which includes server-side
conversation state, while the schema had nowhere to keep it. Either option available at
runtime — reject or ignore — violates the promise.

→ Revision 2 adds the response store to the schema (§9.2) and to the milestone that ships
the endpoint.

### C8 — Header and trailer delivery — ACCEPTED

Trailers are a dead channel: mainstream LLM client libraries read the SSE body and never
surface them, so promising "an SSE event **and** trailers" meant one channel plus an unused
one. Separately, injecting a gateway-authored SSE frame contradicts forwarding upstream
bytes untouched. And attaching ~30 headers to every response risks intermediary limits and
adds bytes ahead of the first streamed byte.

→ Revision 2: trailers removed; post-hoc streaming values are **opt-in** via a request
header; the always-attached header set is bounded, with the rest behind a detail flag.

### C9 — Credential import claim — SUPERSEDED

This reviewer flagged the migration claim as under-evidenced. It was, and the dedicated
verification in R1-A settled it with stronger evidence than either party had.

---

## Standing consequence

Two inherited assumptions failed here for the same reason: they were adopted from a source
where they were true, into a context where they are not. The capacity broker's broadcast
wakeup was fine for one operator and wrong for a gateway; the credential-migration benefit
was computed from a row count without checking whether those rows were alive.

Revision 2 adds a rule: **an inherited mechanism must be re-validated against dorang's load
profile and data before adoption**, and the validation must be a test, not a reading.
