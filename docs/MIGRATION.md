# Migration from an Incumbent Gateway

> Moving traffic from an existing OpenAI-compatible proxy onto dorang: importing its
> configuration and its credentials, what does **not** transfer, running shadow comparison,
> reading its report, and deciding to cut over.
>
> The governing rule: **an importer that resurrects expired keys silently restores revoked
> access.** Every decision below is downstream of that.
>
> **Verified against the working tree on 2026-07-28.** The implementation is moving; §1's list of
> what does not transfer is the part most likely to have shrunk since. Re-check it against the
> build you are cutting over to.
>
> 한국어: [MIGRATION.ko.md](MIGRATION.ko.md)

---

## 0. Shape of the job

Interoperability is treated as a feature with tests, not as compatibility debt, and it happens
at three levels. They have very different costs.

| Level | Mechanism | Effort |
|---|---|---|
| **Wire** | Client SDKs work unmodified against dorang. Byte-level contracts, covered by golden tests | None — it either works or it is a bug ([COMPATIBILITY.md](COMPATIBILITY.md)) |
| **Configuration** | `dorangctl import config` converts a declarative model-list file and reports what it could not represent | An afternoon, plus the warnings |
| **Credentials** | Existing keys keep working during a bounded window; expiry and revocation are honoured | A day, and it needs a decision about the window |
| **Administrative** | Shape-compatible management paths so existing scripts keep working | **Partial** — the credential lifecycle, spend, capacity, catalog, health history and the UI are served; users, teams, models and budgets answer 501. The list is §1.2. *(This cell said "Not in this build")* |

Before planning, size the surface. An audit of a live 505-path proxy deployment against its
actually-attached clients found:

| Tier | Paths | Operations | Meaning |
|---|---:|---:|---|
| T0 | 11 | 14 | an attached client breaks immediately |
| T1 | 88 | 123 | a generic SDK call fails |
| T2 | 343 | 419 | control plane and enterprise features |
| T3 | 63 | 139 | deprecated, UI-internal, vendor passthrough |

**Reaching "nothing breaks" is 2.2% of the surface. A defensible full inference protocol
(T0+T1) is 19.6%. The remaining 80% is control plane.** That ratio decides the whole migration
plan, and it is the reason §5 is careful about what a clean shadow report actually proves.

---

## 1. What does not transfer

Read this before starting, not after.

### 1.1 Nothing that is a secret

The importer never needs, and never receives, a plaintext key — neither a caller's API key nor a
provider credential.

- **Caller keys** are imported from stored digests (§3.2). The plaintext is not in the source
  system either.
- **Provider credentials** — the upstream API keys — are **not** part of a configuration import.
  `dorangctl import config` converts `api_key` fields into `key_env` *references* and leaves you
  to set the variables. Environment variables named in the source file are explicitly not copied,
  with a warning saying so.

> ⚠️ **If provider credentials live in a sealed store, read them out BEFORE rotating any sealing
> key.** Where the sealing key defaults to the administrative key — a common arrangement —
> rotating the admin key destroys every stored provider credential. This is a one-way door and
> it is easy to walk through while doing the sensible thing.

### 1.2 The administrative surface

**This build mounts HTTP administration.** What is served and what is not is a list, not a
blanket, and [OPERATIONS.md](OPERATIONS.md) §3.2 is the authoritative one — check it against the
build you are cutting over to.

Served: the whole credential lifecycle (`/key/generate`, `/info`, `/update`, `/delete`, `/list`,
`/block`, `/unblock`, `/regenerate`, plus `/key/rotate`, `/rotate/cut`, `/secrets`, `/pend`,
`/release`), `/spend/logs`, `/global/spend/report`, `/admin/capacity`,
`/admin/catalog/explain`, `/admin/catalog/unverified`, `/health/history`, and the embedded
read-only `/ui`.

Answering **501 `dependency_not_configured`**, naming the missing piece rather than refusing
generically: `/user/*`, `/team/*`, `/model/*`, `/model_group/info`, `/budget/*`, the three
`/{user,team,tag}/daily/activity` endpoints, `/admin/credentials/health`, `/admin/quota`,
`/spend/calculate`, `/admin/pricing/preview` and `/admin/config/reload`. `internal/store` has no
Go code for `users`, `teams`, `team_members`, `deployments` or `model_aliases`, which is why.

So, for a cutover:

- Key issuance, listing and revocation work **over HTTP** — `cmd/dorang/revoke_test.go` runs the
  leaked-key incident end to end, blocking a working key with one call and watching the next
  request from it be refused. `dorangctl key …` remains available and is the same store.
- User, team and budget administration still have no interface. `SIGHUP` replaces
  `/admin/config/reload`.
- Any script or dashboard your incumbent drives through the 501 list **will not work after
  cutover.** Inventory them first — but inventory them against the list above, not against a
  blanket.

> ⚠️ **This section read "This build mounts no HTTP administration" until 2026-07-29, and it
> was the single most expensive stale claim in these documents.** It told an operator that the
> largest single piece of migration work was ahead of them when most of it was already done, and
> it named revocation-over-HTTP as absent on the page a cutover plan is written from.
> `docs/SECURITY-REVIEW.md` had already corrected the identical sentence in its own text —
> *"there **is** an API path to revoke a leaked key — which the review's plainest sentence said
> there was not"* — and `docs/OPERATIONS.md` §3.2 had documented the mounted surface in detail.
> Two documents were right and this one was never updated. A claim that survives in one file
> after being corrected in two others is not a typo; it is the absence of a step that re-derives
> every document from the code.

### 1.3 The administrative credential

The master credential is **out of band**: compared in constant time against a configured value,
never stored as a row. It is therefore never in an import, and an importer that only reads a
credential table produces a gateway with **no administrator at all** — and does not notice. The
import report says so explicitly rather than leaving it to be discovered.

Set `DORANG_MASTER_KEY` before the first start. The authenticator refuses to build without it
unless an explicit opt-out is set, which is the point: losing admin authentication has to be
typed out.

### 1.4 Anything the source file expresses that dorang does not

The importer reports rather than guesses. Unrecognised top-level sections are warned and not
imported. An entry it cannot resolve is kept whole against a placeholder provider, with a
warning — never silently dropped and never guessed at.

### 1.5 Routes this build does not serve

The T0 and T1 inference surface is built — chat completions, completions, embeddings, messages,
rerank, moderations, audio, images, Responses, models, batches and files, plus the Azure-style
deployment-in-the-path aliases. What still answers **501** is `/v1/ocr`, `/v1/vector_stores`,
`/v1/assistants`, and the entire administrative shape (§1.2).

If your incumbent serves something on that list and a client uses it, that traffic cannot cut
over yet. [OPERATIONS.md](OPERATIONS.md) §0 has the current list — check it against the build you
are cutting over to rather than against this paragraph, because the surface has been growing
milestone by milestone.

---

## 2. Importing the configuration

```
dorangctl import config /etc/litellm/config.yaml > /etc/dorang/config.yaml
```

The converted configuration goes to **stdout**; every warning goes to **stderr**, so the
redirect above captures a clean file. A summary line — providers, credentials, model groups and
warning count — is also on stderr.

The result is **defaulted but deliberately not validated**. An imported file usually needs a
decision or two from a human first, and each of those is a warning. Settle them, then:

```
dorangctl config lint /etc/dorang/config.yaml
```

### 2.1 What maps

| Source concept | dorang |
|---|---|
| `model_list[].model_name` | `models[].name` — repeats form one load-balanced group |
| `model_list[].<params>.model` | `deployments[].upstream_model` |
| `.api_base` / `.api_key` / the provider hint | normalized into `providers[]` + `credentials[]` |
| `.rpm` / `.tpm` / `.max_parallel_requests` | `limits[]` on the deployment |
| `.weight`, `.timeout`, `.stream_timeout` | deployment fields |
| drop-unsupported and explicit drop lists | `params.drop_unsupported`, `params.drop[]` |
| per-token input/output cost | `pricing.rules[]` |
| routing strategy | `models[].strategy` |
| default / context-window / content-policy fallbacks | `fallbacks.on.*` |
| `environment_variables` | **not imported** — set them in the process environment and reference with `key_env` |

Provider hints are mapped onto dorang kinds through a table covering the common ones
(`openai`, `azure`, `anthropic`, `bedrock`, `vertex_ai`, `gemini`, `cohere`, `mistral`,
`deepseek`, `xai`, `moonshot`, `minimax`, `qwen`/`dashscope`, `glm`/`zhipu`, `ollama`,
`openrouter`, `jina`, `vllm`/`hosted_vllm`, and several OpenAI-compatible hosts). **A hint that
is not in the table is assumed OpenAI-compatible and warned about** — that is the honest
default, since it is the wire shape almost every gateway speaks, and the warning is what makes
it a decision rather than an assumption.

Routing strategies map to dorang's tie-break chains: `simple-shuffle` → `[weighted_random]`,
`least-busy` → `[least_busy]`, `latency-based-routing` → `[lowest_latency]`,
`cost-based-routing` → `[lowest_cost]`, and the usage/TPM-RPM family → `[least_busy]`.

### 2.2 The one rule the importer will not break

**A `provider/model` field is resolved by consulting the declared provider list, never by
splitting on a separator.**

This is not fastidiousness. Real model names use `:` and `/` for at least three different
things — a family tag, a vendor prefix, a deployment variant — and a gateway that splits in one
code path and preserves in another makes identity, routing, aliasing and prefix affinity all
non-deterministic **for the same model**. The regression case that pins it is
`bedrock/anthropic.claude-v2:1`, which carries both separators in one name.

So the importer uses the declared provider hint, or failing that the `api_base` host. An entry
with neither keeps the whole string against a placeholder provider and warns. You then decide
what it meant; dorang does not.

### 2.3 After the import

Three things the import cannot do for you:

1. **Model the capacity axes.** A source configuration states `rpm`/`tpm`/`max_parallel_requests`
   per deployment. dorang's model — per account across all models, *and* per (key, model) — is
   richer, and the shape that matters is usually not in the source file at all because the
   source file could not express it. Read [CONFIG.md](CONFIG.md) §8 and write the axes by hand.
2. **Price anything it could not find a rate for.** Run
   `dorangctl price <model> --input N --output N` on every model group. A warning of
   `no marginal_usage rule matched` means that traffic would be recorded UNPRICED.
3. **Tell you which deployments have an undeclared context window.** Run
   `dorangctl catalog unverified`. An undeclared window is not zero and not unlimited; it neither
   excludes a deployment nor qualifies it as "larger" for the context-window fallback chain. A
   window declared **too large** is the dangerous direction: requests between the real limit and
   the declared one fail outright, and context-window fallback never fires because dorang
   believes they fit.

---

## 3. Importing credentials

### 3.1 The rule that governs everything here

> **Importing existing credentials is a migration window, not an architecture.**

A verification against a real deployment found that **the large majority of stored credentials
were already expired**. That single number changed two decisions:

- **Expired rows are imported as expired.** Importing them as live would silently restore
  revoked access — access that somebody deliberately took away, restored by a tool nobody
  thought of as a security boundary. This is the most important sentence in this document.
- **Adopting the incumbent's unsalted digest permanently is a bad trade.** The benefit was
  originally computed from a row count without checking whether the rows were alive. Once you
  know most of them are dead, "avoid reissuing a handful of live keys" is not worth a permanently
  offline-attackable key database. So legacy hashing became a *window* with a mandatory end date
  (§3.4).

### 3.2 Why the import needs no plaintext key

dorang stores `lookup = sha256(token)[:16]` as a scheme-independent index key; only
*verification* branches on scheme. That definition is load-bearing rather than convenient:

Because `lookup` is a **prefix of the legacy digest**, an importer can derive both `lookup` and
the legacy `token_hash` from the digest a source system already stores — and therefore **never
needs the plaintext key**. If `lookup` were defined any other way, importing would require
credentials nobody has, and migration would become a flag day. A test pins the identity so it
cannot drift.

### 3.3 What the importer carries, and why each field matters

Authorization fields must be carried or the gateway fails **open**:

| Field | If it is dropped |
|---|---|
| expiry | A revoked-by-expiry key works again |
| blocked flag | A deliberately blocked key works again |
| model allow-list | A key reaches models it was never permitted |
| route allow-list | A key reaches routes it was never permitted |
| budget, spend, reset time | Spend restarts at zero against a ceiling somebody set for a reason |
| rate limits | A limit stops existing |
| owning team and user | Team-scoped budget, rate limit and blocked flag all stop applying |
| object-permission references | Whatever they governed stops being governed |

Two rules that follow:

- **A key whose owning team is not present in the destination is skipped by default**, and
  reported. A key with an unresolvable team runs with no team budget, no team rate limit and no
  team blocked flag — it fails open, which is the failure mode this whole section is written
  against. An `orphan` policy exists for operators importing teams in a later pass and accepting
  the window; it is never the default.
- **A row whose `lookup` already exists is left untouched, not overwritten.** Re-running an
  import must not undo an administrator's later revocation.

Two things the importer deliberately does **not** copy:

- **A display column that stores trailing characters of the secret.** dorang stores a
  non-reversible display label instead. Copying the incumbent's column would import a partial
  secret into a new database and call it a UI affordance.
- **A plaintext credential**, if one is found in a source row. That is reported as a warning
  rather than migrated.

The report accounts for everything: rows scanned, imported, imported-as-expired, blocked,
missing-team, missing-user, already-present, skipped — plus a per-row reason for every row that
did not migrate, and a per-column reason for every source column that was deliberately not
carried. **Nothing is dropped silently.** A dry-run mode reads and reports without writing.

### 3.4 The legacy verification window

```yaml
auth:
  legacy:
    enabled: true
    until: "2026-09-30"     # required, and must be in the future
  rehash_on_use: true       # default
```

`legacy_sha256` is an unsalted single-round digest — the scheme the common incumbent stores.
With the window open, an imported key authenticates as it did before. With
`rehash_on_use` (default on), each successful legacy verification schedules an **asynchronous
upgrade to `dorang_v1`**, so the migration completes without downtime and without a flag day.

Both digests are always computed and the comparison target chosen branchlessly, so a key's
scheme is not observable in timing.

Sizing the window: it needs to be long enough that every live key is used at least once, because
that use is what upgrades it. A month of traffic is a reasonable default; check
`dorangctl key list`'s `SCHEME` column to watch the population convert. When the date passes,
legacy verification is refused and the configuration refuses to start until you remove or extend
it — which is the forcing function, and it is deliberate.

### 3.5 ⚠️ There is no CLI entry point for credential import

**`dorangctl import config` imports a configuration file. It does not import credentials.**

The credential importer is implemented in the store layer, is tested, and reads a
`LiteLLM_VerificationToken`-shaped source table. It has **no `dorangctl` subcommand and no
administrative endpoint** in this build, which means there is no supported way for an operator
to invoke it.

Until there is, the options are:

1. **Reissue.** Given that most stored credentials in a real deployment are already expired,
   reissuing the live ones with `dorangctl key create` is usually a smaller job than it sounds —
   and it is the only path that gets you `dorang_v1` hashing from the first request, with no
   window to close later.
2. **Wait for the entry point.** The mechanism, the expiry rule, the fail-open field list and
   the report all exist; only the operator-facing invocation is missing.

Plan for (1). If you were counting on (2), that is the finding this document exists to deliver
early rather than at cutover.

---

## 4. Shadow comparison

Progressive migration needs proof from real traffic, not only synthetic tests. Shadow mode sends
a sampled fraction of live requests to a reference gateway — your incumbent — and compares the
two responses structurally.

### 4.1 Configuration

```yaml
shadow:
  mode: compare                  # off | mirror | compare
  reference:
    url: https://incumbent.internal
    api_key_env: DORANG_SHADOW_REFERENCE_KEY
    timeout: 60s
  sample_rate: 0.05              # NO DEFAULT — unset means zero
  compare: {structural: true, semantic: false}
  max_cost_usd_per_day: "5"      # REQUIRED whenever mode is not off
  queue_size: 256
  workers: 4
  capture: {head_bytes: 256KiB, tail_bytes: 16KiB}
  report: {path: /var/lib/dorang/shadow.jsonl, max_bytes: 256MiB}
```

Full key reference in [CONFIG.md](CONFIG.md) §18. Five things that will catch you:

1. ⚠️ **`sample_rate` has no default.** Turning `mode` on without setting it shadows **nothing**,
   writes an empty report, and reports gate verdict `no_data` — and an empty report is precisely
   what a cutover decision is looking for. Set it explicitly.
2. **`max_cost_usd_per_day` is required.** Both modes send every sampled request twice and
   therefore cost twice. An earlier revision required the ceiling for `mirror` only, which is
   backwards: `compare` is `mirror` plus a diff, so it costs at least as much.
3. **`compare.semantic: true` is refused at load.** The knob is named in the design and no
   comparison is specified for it. Accepting it silently would mean an operator who asked for
   semantic comparison gets structural comparison and an empty report, and reads that report as
   proof of something it never checked — the one failure mode a cutover gate cannot have.
4. **`shadow:` does not hot-reload.** Its daily ceiling, sampled set and report handle are
   per-process state, and rebuilding them re-arms the ceiling — turning "$5 per day" into "$5 per
   `SIGHUP`". A changed shadow section is refused and the running configuration is kept. Restart
   to change it.
5. **The reference credential is `api_key_env` only.** This is the one place in the whole file
   that spells a secret reference that way rather than the `key_env`/`key_file`/`key_ref` triple,
   so a deployment that keeps secrets in a file or a vault **cannot express a shadow reference
   credential at all**. The client's own credential is never forwarded to the reference.

### 4.2 What is replayed, and what is not

> ⚠️ **"Send the same request to the reference" is destructive taken literally.** It includes
> `DELETE /key/…`, which would delete a key on the system still serving production.

Replay is therefore **deny by default**:

| Method | Replayed |
|---|---|
| `GET`, `HEAD`, `OPTIONS` | **Yes**, always. Safe by definition, and they cover `GET /v1/models` and the health probes — three of the eleven T0 paths, and ones clients really do break on |
| `POST` to an inference family | **Yes** — chat completions, embeddings, Anthropic messages, count-tokens. These are the routes the comparison exists for; their only side effect is spend, and spend is already bounded |
| Everything else | **No.** Not `PUT`, `PATCH` or `DELETE`; not `POST` to a passthrough, batch or administrative route |
| `/metrics` | **No.** Comparing two Prometheus dumps produces an inconclusive record per probe and nothing else |

A passthrough prefix is excluded because it can carry a file deletion or a batch cancellation and
the engine does not parse the body well enough to tell which.

**The cost of the rule is that the compared surface is smaller than the served surface**, and an
empty report therefore proves less than it appears to. That is why refusals are counted and
reported *next to the verdict* rather than being invisible — see `skipped_unsafe` in §4.4.

Two more bounds worth knowing:

- **A marker header stops two gateways pointed at each other from amplifying without bound.** A
  request that is itself a shadow copy is refused on arrival and counted as `skipped_loop`.
- **The cost ceiling stops, it does not skip.** Refusing the request that does not fit while
  continuing to admit cheaper ones would bias coverage toward cheap traffic while the counter
  still read under budget — a report that looks complete and is not. Cost is reserved before the
  call and settled after, for the same reason budget is.

### 4.3 The report

The JSONL report at `report.path` contains **one line per comparison that found a difference, or
that could not decide a dimension — and nothing else.** That asymmetry is what makes the
completion criterion work: an empty file means every comparison that ran decided every dimension
and found nothing, which is a statement worth basing a production cutover on. A file that also
carried a line per clean comparison would be a log, and nobody reads a log to decide a cutover.

Each record:

```json
{"time":"2026-07-28T09:14:02Z","request_id":"…","mode":"compare",
 "route":"chat_completions","method":"POST","path":"/v1/chat/completions",
 "model":"model-x","stream":true,
 "dorang_status":200,"reference_status":200,
 "diffs":[{"path":"$.choices[].logprobs","kind":"missing_in_dorang",
           "dorang":"","reference":"null","note":"…"}],
 "inconclusive":[{"dimension":"stream_terminator","reason":"tail window exhausted"}],
 "dorang_ms":412,"reference_ms":508,"cost_nano_usd":1240000,"cost_estimated":false}
```

| Field | Read it as |
|---|---|
| `diffs[]` | A structural divergence. `path` is a JSON field path, `kind` says how they differ |
| `inconclusive[]` | The comparison **could not decide** that dimension. Not a pass |
| `reference_error` | The reference could not be reached at all. A record with this set and no diffs **is not a clean comparison; it is a comparison that did not happen** |
| `cost_estimated` | The charge against the daily ceiling was the fallback estimate rather than a price. A run with many of these has its ceiling enforced against a guess |

**What is compared:** HTTP status, the set of JSON field paths present and the type at each path,
the response header key set, the SSE frame structure and terminator, and the error envelope's
shape, type and code when either side errors.

**What is not:** output text, ids, timestamps, `system_fingerprint` and token counts are excluded
by default, because model output is not deterministic and comparing it would produce a diff per
request. `compare.ignore_fields` adds to that set — and every field you add is a field the clean
verdict says nothing about.

### 4.4 The verdict, in one word

The health endpoint and `/metrics` both carry the gate state. `GET /health`:

```json
{"status":"healthy","shadow":{"mode":"compare","sample_rate":0.05,"cost_capped":false,
 "spent_usd":"1.240000000","limit_usd":"5.000000000","sampled":812,"compared":790,
 "clean":790,"with_diffs":0,"inconclusive":0,"queue_dropped":0,"reference_errors":0,
 "report_dropped":0,"skipped_unsafe":22,"gate":"clean"}}
```

| `gate` | Meaning |
|---|---|
| `off` | Shadowing is not running |
| `no_data` | Nothing has been compared. Usually `sample_rate` unset, or not enough traffic yet |
| `mirroring_only` | `mode: mirror` — sending and recording, comparing nothing |
| `stopped_cost_capped` | The daily ceiling tripped. **The report has stopped growing and is not complete** |
| `diffs` | At least one comparison found a difference |
| `incomplete` | Comparisons ran, but something makes the report untrustworthy: an inconclusive record, a dropped queue item, a reference error, an oversize skip, or a dropped report record |
| `clean` | Comparisons ran, every dimension decided, no differences, nothing dropped |

**`clean` is the only verdict that supports a cutover**, and it is deliberately narrow. "Zero
diffs" is not the criterion; "zero diffs *and* the run was complete" is. A comparison that
silently skips a case is worse than one that reports a difference.

The metrics carry the same numbers as counters — `dorang_shadow_compared_total`,
`_clean_total`, `_with_diffs_total`, `_inconclusive_total`, `_dropped_total`,
`_reference_errors_total`, `_skipped_unsafe_total`, `_skipped_capped_total`,
`_skipped_loop_total`, `_skipped_oversize_total`, `_report_dropped_total`,
`_unpriced_estimates_total` — plus gauges for `dorang_shadow_cost_capped`,
`_cost_spent_nano_usd`, `_cost_limit_nano_usd`, `_queue_depth` and `_report_bytes`.

### 4.5 Reading a report that is not clean

| What you see | What it means | What to do |
|---|---|---|
| `diffs` with `kind: missing_in_dorang` on a field the incumbent emits | A serialization difference. Often `logprobs: null` / `finish_reason: null`, which real OpenAI emits and the reference proxy omits | Decide which one your clients were built against. dorang follows the reference proxy by default, and the alternative is a one-line option — but it must be a **choice**, not an accident |
| `diffs` on the error envelope's `type` or `code` | The two gateways classify a condition differently. This is API surface: SDKs branch on status and `type`, and application code often matches `code` | [COMPATIBILITY.md](COMPATIBILITY.md) §11.2 has dorang's canonical condition table, with the deliberate divergences explained — budget exhaustion is a terminal `400` rather than a `429`, and "no healthy deployment" is `429` rather than `503`. Note that the *model allow-list* case answers `401` in this build, matching the reference proxy rather than the taxonomy's `403`; check the code path before filing a diff on it |
| `inconclusive` on `stream_terminator` | The capture window did not reach the end of the stream | Raise `capture.tail_bytes`. A stream's terminator is the last thing on the wire, which is why there are two windows rather than one |
| `reference_errors` climbing | The reference is not answering. Nothing is being proved | Fix the reference, then restart the run. Those requests are not "clean" |
| `queue_dropped` non-zero | The shadow queue filled and work was discarded. Coverage has a hole the report cannot show you | Raise `queue_size` or `workers`, or lower `sample_rate`. A full queue must never push back into the request path, so dropping is correct — but it makes the report incomplete |
| `report_dropped` non-zero | The report hit `max_bytes`. **A truncated report read as an empty one is the worst outcome this mechanism has** | Raise the cap, rotate the file, re-run |
| `cost_capped: true` | Shadowing stopped for the rest of the UTC day | Raise `max_cost_usd_per_day` or lower `sample_rate`, and do not read the report as complete |
| Many `cost_estimated: true` rows | dorang could not price the requests being copied, so the ceiling is being enforced against a per-call guess | Fix the pricing rules first — the ceiling is only meaningful once it is enforced against real prices |

---

## 5. The cutover decision

### 5.1 What a clean report actually covers

This is the part that is easy to get wrong, and the reason the counters exist.

**A clean report covers materially less than the whole gateway.** Three subtractions, in order of
size:

1. **~80% of a real deployment's served surface is control plane** (§0), and control-plane calls
   are not replayable — they change state on the system still serving production. `skipped_unsafe`
   is the count, and it is reported *next to the verdict* precisely so it is not read as zero.
2. **Only the routes this build serves can be compared at all.** `/v1/ocr`, `/v1/vector_stores`
   and `/v1/assistants` answer 501 on dorang, as do the administrative paths §1.2 lists as
   unserved, so for those there is nothing to compare (§1.5). ⚠️ **This line also named
   `/v1/responses`, rerank, audio, images and moderations until 2026-07-29, and all five are
   served** — §1.5, four hundred lines above, lists every one of them as built, so this file
   disagreed with itself. An exclusion list that is too long is not the safe direction: it
   silently reports "nothing to compare" for surfaces that *can* be compared, producing a clean
   verdict over less than the reader believes it covers. Re-derive it against
   [OPERATIONS.md](OPERATIONS.md) §0, not against either paragraph.
3. **Output text is not compared**, by design. Model output is not deterministic. A clean report
   proves the two gateways agree on *structure* — status, field set, types, framing, error
   shape — not that they produce the same answers.

So the honest statement of what `gate: clean` licenses is:

> For the inference routes dorang implements, at the sampled rate, over the period the run
> covered, the two gateways produced structurally identical responses, and no comparison was
> skipped, dropped, or left undecided.

That is a strong statement and a bounded one. It is not "dorang is a drop-in replacement".

### 5.2 The checklist

Before moving production traffic:

- [ ] `gate: clean`, with a **non-trivial** `compared` count. A clean verdict over 12 comparisons
      is arithmetic, not evidence.
- [ ] `skipped_unsafe` understood, not merely observed. You should be able to name what those
      requests were and how they are covered instead.
- [ ] `inconclusive`, `queue_dropped`, `report_dropped`, `reference_errors` all zero — the gate
      enforces this, but confirm you did not lower a bound to make it pass.
- [ ] `cost_capped` false for the whole run.
- [ ] The run spanned a representative period, including a peak.
- [ ] `dorangctl catalog unverified` reviewed: no deployment carrying traffic has an undeclared
      context window.
- [ ] `dorangctl price` run on every model group, with no `no marginal_usage rule matched`.
- [ ] The control-plane inventory from §1.2 has an answer — a replacement, a script rewrite, or
      an accepted gap.
- [ ] The self-hosted backend flags are set ([OPERATIONS.md](OPERATIONS.md) §5). Every one of them
      fails as a `200` that ignored what was asked.
- [ ] `DORANG_KEY_PEPPER` set explicitly, and backed up with the database.
- [ ] `DORANG_MASTER_KEY` set, and known to be set — not the generated fallback.
- [ ] A restore has been rehearsed (§[OPERATIONS.md](OPERATIONS.md) §8.4), not just a backup taken.

### 5.3 Sequencing

1. **Run dorang beside the incumbent** with `shadow.mode: compare` and a low sample rate. Nothing
   is cut over; you are collecting evidence.
2. **Fix what the report shows**, then re-run. Do not lower a bound to make a verdict pass — the
   bounds are what make the verdict mean anything.
3. **Move one client**, ideally an internal one with a fast feedback loop. Watch
   `dorang_responses_total{class="5xx"}`, `dorang_unimplemented_total` and
   `dorang_auth_failures_total`. A 501 storm names the missing route in
   `X-Dorang-Unimplemented`.
4. **Move the rest by weight**, at the load balancer. dorang's request path is stateless and any
   node can serve any request, so this is a routing decision rather than a coordination one.
5. **Keep the incumbent reachable** until the legacy verification window (§3.4) closes and
   `dorangctl key list` shows every live key on `dorang_v1`. Until then, the incumbent is your
   rollback.
6. **Turn shadowing off.** It costs a second call per sampled request forever otherwise, and its
   report will happily keep growing to `max_bytes`.

### 5.4 Rollback

There is no schema downgrade. The rollback plan is:

- **Before cutover:** point the load balancer back. dorang has changed nothing on the incumbent
  — replay is deny-by-default precisely so that stays true.
- **After cutover:** keys issued by dorang do not exist on the incumbent, and spend recorded by
  dorang is not visible to it. Rolling back after issuing keys means reissuing on the incumbent,
  and accepting a gap in spend history. That asymmetry is the reason §5.2's checklist is worth
  the time.
- **A binary rollback within dorang** is a database restore ([OPERATIONS.md](OPERATIONS.md) §8.4),
  with the same pepper.

---

## See also

- [CONFIG.md](CONFIG.md) §18 — every shadow key; §1 — the three refuse-to-start conditions an
  imported configuration is most likely to hit.
- [OPERATIONS.md](OPERATIONS.md) — day-two running, including the failure modes an incumbent
  never had because it never talked to a self-hosted engine.
- [COMPATIBILITY.md](COMPATIBILITY.md) — the wire contracts, the deliberate divergences from the
  reference proxy, and the error taxonomy a shadow diff will surface.
- [DESIGN.md](DESIGN.md) §2.4 — the credential-import rules; §14.1 — shadow comparison.
