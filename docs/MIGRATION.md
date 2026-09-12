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
| **Credentials** | `dorangctl import keys` migrates them from the incumbent's database (§3.5); existing keys keep working during a bounded window; expiry and revocation are honoured | A day, and it needs a decision about the window |
| **Administrative** | Shape-compatible management paths so existing scripts keep working | **Partial** — the credential lifecycle, users, teams, budgets, spend, capacity, catalog, health history and the UI are served; models answer 501. The list is §1.2. *(This cell said "Not in this build", then "users, teams, models and budgets answer 501")* |

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
`/release`), **`/user/*`**, **`/team/*`** (including `member_add` / `member_delete`),
**`/budget/*`**, `/spend/logs`, `/global/spend/report`, `/admin/capacity`,
`/admin/catalog/explain`, `/admin/catalog/unverified`, `/health/history`, and the embedded
read-only `/ui`.

Answering **501 `dependency_not_configured`**, naming the missing piece rather than refusing
generically: `/model/*`, `/model_group/info`, a `/budget/*` call naming a `credential` or `global`
subject, the three `/{user,team,tag}/daily/activity` endpoints, `/admin/credentials/health`,
`/admin/quota`, `/spend/calculate`, `/admin/pricing/preview` and `/admin/config/reload`.

`/model/*` is the one worth understanding before you plan around it. It is **not** waiting on a
table — `deployments` and `model_aliases` have been in the schema since the first migration. It is
waiting on a *reader*: the routing table is compiled from the configuration file, and nothing in
the binary reads those two tables, so an adapter over them would answer `200`, write your row and
route no traffic differently. Model and deployment administration is still `SIGHUP` plus the file.

So, for a cutover:

- Key issuance, listing and revocation work **over HTTP** — `cmd/dorang/revoke_test.go` runs the
  leaked-key incident end to end, blocking a working key with one call and watching the next
  request from it be refused. `dorangctl key …` remains available and is the same store.
- **User, team and budget administration now have an interface, and it is enforced.** A blocked
  user's keys stop serving on the node that took the call before it answers and fleet-wide within
  the bound §10.1 publishes for a revocation (measured 19.6 ms at `poll: 20ms` against 270 ms).
  A team ceiling is a real budget hold against its own durable counter; clearing one restores
  service without erasing the spend recorded under it. Two behaviours to know before you script
  against them: deleting a user does **not** delete its keys (they are announced as revoked and
  then serve *unowned*), and `/budget/*` addresses a budget by **subject** — `budget_id:
  "team:eng"` — because dorang has no reusable named-budget object; §9.2 puts the ceiling on the
  subject row. That is the one place the request body differs from the incumbent's.
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

> ⚠️ **The surface grew again, and if you sized a migration against the previous revision of this
> section you have less work than you planned, not more.** `/user/*`, `/team/*` and `/budget/*`
> answered 501 until 2026-08-03 and are served now; only `/model/*` is left of the four this
> section used to list. If your cutover plan carried "write a replacement for user and team
> administration" as a line item, that line is closed, and the inventory in the checklist below
> should be re-run against §1.2 as it now reads rather than against the copy you took.
>
> One thing that changed underneath is worth knowing even if you never call these routes: a
> **user or team `blocked` flag, budget ceiling, rate limit and model allow-list are now enforced
> on the request path.** They were columns nothing read — the gateway's authorization envelope
> carried only the key — so any of them you had already put in the database, by the `INSERT`s this
> section used to send you to, applied to **nothing**, at any latency, on any node.
>
> `dorangctl import keys` does not create `users` or `teams` rows; `dorangctl import users` and
> `dorangctl import teams` do (§3.5), from the incumbent's own tables, and a plain key import
> cannot have planted them. Hand-written rows and rows loaded by your own migration script can. If you have
> any, read `users.blocked`, `teams.blocked` and `max_budget_nano` on both tables **before** you
> cut over. Rows that were inert are now live, and the first place you would notice is a tenant
> being refused. The importer's own warning — *"imported keys reference teams that are not
> present; their team-scoped limits do not apply"* — described the intended behaviour correctly
> and was, until this build, equally true of the teams that *were* present.

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
`/v1/assistants`, and the part of the administrative shape §1.2 lists as unserved — which is now
`/model/*` and a short tail, not the whole of it.

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
| `additional_drop_params` / `drop_params: [...]` | `providers[].params.drop[]`. **Load-bearing**: applied to the request before the encoder runs, and reported in `x-dorang-dropped-params`. A name whose removal would change *what the model is asked* rather than *how it samples* — `tools`, `messages`, `response_format` and the rest — is **refused by name** rather than imported, and so is `max_tokens` on an `anthropic`-shaped provider |
| `drop_params: false` | **not imported.** The source relays the caller's JSON, so "do not filter" means "let the provider answer 400". dorang converts, and a parameter the target's wire shape has no field for cannot be forwarded into anything — `params.drop_unsupported: false` is a load error. Writing it would have produced a file that fails your very next `config lint` |
| `litellm_params.max_tokens` | **not imported, per row.** The source applies it as a DEFAULT for a caller who named none; dorang's `deployments[].max_output_tokens` is a CEILING that refuses a caller who named more. They constrain opposite halves of the traffic, so moving the literal would start failing requests that succeed today. dorang takes the default from the model catalog; write `max_output_tokens` yourself if you meant the cap |
| `model_info.access_via_team_ids` | **not imported, per row** — and the capability is not missing. dorang's model allow-list is consulted for the key, its user AND its team, and every subject must allow, so a key-level list narrows its team's and can never widen it. A team is a directory row rather than a line in the configuration file, so the import has nowhere to put it: set it with `POST /team/update {"models": [...]}`. ⚠️ **The relation inverts.** The source says "only these teams may reach this model" and dorang says "this team may reach only these models", and an EMPTY dorang list allows EVERYTHING — so a team you never give a list to still reaches the model, and the restriction is only true once every other team has one |
| per-token input/output cost | `pricing.rules[]` — **rescaled**: per-token rates are multiplied by 10⁶ and per-character rates by 10³, because dorang's fields are per 1M tokens and per 1K characters. The importer used to copy the literal, which made every imported card a millionth of its true price, silently — every request rounded to zero while `/spend/calculate` still reported the rule matched |
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

Four things the import cannot do for you:

1. **Model the capacity axes.** A source configuration states `rpm`/`tpm`/`max_parallel_requests`
   per deployment. dorang's model — per account across all models, *and* per (provider,
   upstream model) — is
   richer, and the shape that matters is usually not in the source file at all because the
   source file could not express it. Read [CONFIG.md](CONFIG.md) §8 and write the axes by hand.
2. **Price anything it could not find a rate for, and check one price against the vendor's card.**
   Run `dorangctl price <model> --input N --output N` on every model group. A warning of
   `no marginal_usage rule matched` means that traffic would be recorded UNPRICED.
   Then `POST /spend/calculate` for one request whose cost you can compute by hand from the
   vendor's published card, and compare. A rate that is wrong by a factor rather than absent
   produces no warning at all — the rule matches, `"missing"` is `false`, and the figure is
   simply wrong. That is what the ×10⁶ defect looked like from the outside.
3. **Re-express what the source stated on the model row and dorang states elsewhere.** Two
   settings come out of the import as warnings rather than as configuration, and both are the
   kind that reads as absent rather than as lost:
   `model_info.access_via_team_ids` (a team's model allow-list, `POST /team/update`, and see
   the inversion warning in §2.1 — this is the one that is silently PERMISSIVE if you skip it)
   and `litellm_params.max_tokens` (a per-deployment default versus dorang's ceiling). Grep the
   import report for `NOT imported`; that phrase is reserved for a setting that meant something.
4. **Tell you which deployments have an undeclared context window.** Run
   `dorangctl catalog explain <kind> <model>` for each deployment you route to and read the
   `context_window` row: `undeclared` in the ORIGIN column is the answer. An undeclared window is
   not zero and not unlimited; it neither excludes a deployment nor qualifies it as "larger" for
   the context-window fallback chain. A window declared **too large** is the dangerous direction:
   requests between the real limit and the declared one fail outright, and context-window
   fallback never fires because dorang believes they fit.

   > This step named `dorangctl catalog unverified` until that command was rewritten. It never
   > reported context windows and does not now — it reports verification state, which is §2.4's
   > subject and a different axis entirely.

---

### 2.4 What the catalog knows about its own entries

Two commands read the catalog, and neither one guesses.

`dorangctl catalog explain <kind> <model>` is the provenance view: one row per field with the
resolved value, the layer that won it and the file that wrote it. It is what §2.3 item 4 uses,
and it is the only place `undeclared` is distinguishable from `0`.

`dorangctl catalog unverified` reports **verification state** — what happened the last time
anybody put each entry to a live endpoint. It prints a count per state, then the two states that
carry a finding with the evidence inline, then the two that carry none summarised by provider
kind. `--state S` lists one state on its own.

| State | What it means | What to do |
|---|---|---|
| `verified` | Asked, and it answered as itself | Nothing |
| `denied` | Asked; this plan is not entitled. **The model exists** | Buy the entitlement or stop routing to it. This is not an absent model, and deleting the entry would lose a true fact |
| `substituted` | Asked, and a **different** model answered | A live routing defect: you asked for one model, were served another, and are billed for the traffic. Stop routing to the name |
| `citation_only` | Nobody could ask — no credential for that provider exists where this catalog is maintained | Nothing is wrong with the entry; its evidence is a citation rather than a probe. Close one with `catalog verify` and a key |
| `unchecked` | Nobody has asked, and nothing says why | Backlog |

The output, abridged, on the catalog this build ships:

```
245 catalogued models. What happened the last time an endpoint was asked:

  verified       38   asked, and it answered as itself
  denied         9    asked; this plan is not entitled. The model EXISTS
  substituted    3    asked; a DIFFERENT model answered
  citation_only  195  nobody could ask: no credential for the route here
  unchecked      0    nobody has asked, and nothing says why

substituted (3) — asked; a DIFFERENT model answered:
  kind=glm model=glm-5.1
      served instead: glm-5.2 (asked 2026-08-03)
      200 OK, body `"model":"glm-5.2"`. Same on three consecutive runs.
  …

reasoning capability is unknown for 244 of 245 models. That is a different question and a
different probe (DESIGN §10.2): a model can be verified to exist and still have an unknown
reasoning control.
```

**Two axes, and the old output conflated them.** `unverified` never meant "we are not sure this
model exists" — it meant the entry's **reasoning capability** is unknown, which is a different
question with a different probe and a different answer. The command now keeps them visibly
apart: the states above are about whether an endpoint answered to the name, and the reasoning
count is one line on its own at the end. A model can be `verified` and still have an unknown
reasoning control, and on the shipped catalog almost every one of them is. The command used to
print a single flat list of everything undated, which was accurate and useless — an entry nobody
had typed sat beside one that had been asked and had given a definite answer, and nothing in the
output said which was which.

#### Closing one: `dorangctl catalog verify`

```
dorangctl catalog verify --kind <kind> --key-env <VAR> [--write <file>]
```

This sends **one real request per catalogued entry on that kind** and reads the answer. A
`/models` listing is evidence and not proof, and both directions have been observed on this
catalog's own providers: a listing omitted a model that was still being served, and it named nine
that refuse every request. It cannot see the third case at all — a substituted model answers
`200`, and only the response body says which model actually replied.

The probes are minimal, one short user turn with `--max-tokens` defaulting to 16, because they
bill to your own plan. `--dry-run` prints what would be asked and asks nothing; it is the only
mode that needs no credential:

```
$ dorangctl catalog verify --kind glm --dry-run
would ask https://api.z.ai/api/coding/paas/v4 for 8 model(s), max_tokens=16:
  glm-4.5  (chat)
  glm-4.6  (chat)
  …
```

`--model a,b` narrows to named entries, `--base-url` overrides the endpoint, `--timeout` is per
request (120s), `--catalog` loads overlays first.

**Three refusals are built in**, and each one is a conclusion the command will not draw:

1. **The key is named by an environment variable and is never taken as a flag value.**
   `--key-env VAR` takes the variable's *name*; there is no flag that accepts a key. A key passed
   as a flag lands in shell history and in every `ps` on the box. An empty variable is refused
   too, with the reason: an unasked model must stay unasked rather than acquire a date.
2. **A 404 that names no missing model is a path error, not an absent model.** Absence is read
   from the error's *language* — "does not exist", "no such model", "unknown model" — and never
   from the model name appearing somewhere in the body, because a short model id occurs in almost
   any message and the failure mode is a live entry reported as missing. A Responses-only
   endpoint answering a chat request produces exactly this 404, and it is reported as a wrong
   path.
3. **If every probe fails 401/403 with none succeeding, nothing is written and the credential is
   blamed.** One model refusing on a route where others answer is a fact about that model; every
   model refusing identically is a fact about the key. This is not hypothetical: a token whose
   scope lacked inference access returned 403 on every model, and recording that run would have
   written two hundred-odd false denials into a catalog whose absences are supposed to mean
   something. A bare 403 with no eligibility language concludes nothing on its own either — only
   a refusal that names eligibility, on a route where something else succeeded, is a `denied`.

`--write FILE` emits a catalog overlay loadable by the same loader that reads the embedded data,
so the output of verification is input to the catalog with no transcription step in between. Load
it with `--catalog` or `$DORANG_CATALOG_PATH`, and review it first — a `verified:` there says the
endpoint answered to the name on that date, not that the entry's numbers are right.

Only the three outcomes that are facts about a model are written: `verified`, `substituted` and
`denied`. **A `retired` or `absent` result is reported and never written**, because acting on one
is a *deletion* — the catalog's answer to a model that serves nobody is to remove the entry with
the reason in a comment, and no tool should delete catalog rows on the strength of one HTTP
response. Errors are not written either; they are facts about the network or the key.

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
carried. **Nothing is dropped silently.** Reading and reporting without writing is the
default; §3.5 is how you run it.

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

### 3.5 Running the import

**`dorangctl import config` imports a configuration file. `dorangctl import keys` imports
credentials.** They are separate verbs over separate sources, deliberately: the first writes
YAML to stdout, the second writes rows into this deployment's store and therefore grants
access.

```
dorangctl import keys --from postgres://user:pass@host/litellm          # reads and reports
dorangctl import keys --from postgres://user:pass@host/litellm --commit # writes
```

**It reports by default and writes only with `--commit`.** That is not caution for its own
sake — the report *is* the deliverable. It names every row that did not migrate and why, every
source column that was not carried and why, and how many rows came in already expired, which
per §3.1 is usually most of them. Read it before you commit; it frequently changes the answer
to "is this worth importing at all".

| Flag | |
|---|---|
| `--from <dsn>` | required. A `postgres://` URL or a path to a SQLite file |
| `--from-driver` | `postgres` or `sqlite`. Inferred from `--from` when unset |
| `--commit` | write. Without it nothing is written and the report is identical |
| `--table` | source table, default `LiteLLM_VerificationToken` |
| `--on-missing-team` | `skip` (default) refuses a key whose team is absent — see §3.3 for why that failure is the safe one — or `orphan` to import it with the team dropped |
| `--on-untranslatable` | `skip` (default) refuses a key whose allow-list holds an idiom dorang cannot express, or `clear` to import it with that allow-list dropped. See §3.6 |
| `--object-permission-table` | source table an `object_permission_id` points into, default `LiteLLM_ObjectPermissionTable`. It is **read**: see §3.6 |
| `--limit` | read at most N source rows, for a first look at a large table |
| `--expect-admin-key` | say so if you believe the administrative credential is among the rows. It never is, and the report will explain why |

Re-running is safe: a key already present is left untouched and counted, so a re-import cannot
undo a revocation you made in between.

**The rows a key refers to.** A key carries its `team_id` and `user_id`, and the team's budget
ceiling, rate limit, model allow-list and blocked flag are enforced on the request path — so a
fleet of imported keys whose teams were never created runs with every team-scoped limit silently
absent, which is the failure §3.3 is written against. Two more verbs create those rows from the
incumbent's own tables, with the same report-first, write-on-`--commit` rule:

```
dorangctl import users --from postgres://user:pass@host/litellm --commit
dorangctl import teams --from postgres://user:pass@host/litellm --commit   # then keys
```

**Users first, then teams, then keys.** A team's `members_with_roles` names users, and a member
whose user row is absent is reported and not carried (`--members` is on by default); a team
already present is left untouched, so carry the members on the first run. Both verbs read the
same columns the key importer does where the meaning is the same — `max_budget`, `spend`,
`budget_duration`, `budget_reset_at`, `tpm_limit`, `rpm_limit`, `models` (with §3.6's idioms and
`--on-untranslatable`, with one difference the level makes: `no-default-models` is the incumbent's
default for every user and team it creates and at that level means "this level grants nothing, the
key decides" — which is what an empty dorang list says there, since key, user and team must each
allow — so it is removed, reported as cleared, and the row imports; on a key it stays refused),
`metadata`, the timestamps — and the report names every column that is
not carried with the reason: `password` and `sso_user_id` are credential material dorang has no
use for; `soft_budget`, `model_max_budget` and `model_spend` are per key or per ledger in dorang;
a user's `teams` list is carried from the team side. A user without an email is skipped, because
dorang requires one; `--synthetic-email-domain example.invalid` gives such a user
`<user_id>@example.invalid` instead.

| Flag (teams / users) | |
|---|---|
| `--from`, `--from-driver`, `--commit`, `--limit`, `--on-untranslatable` | as for keys |
| `--table` | source table, default `LiteLLM_TeamTable` / `LiteLLM_UserTable` |
| `--members` | teams: carry `members_with_roles` into team membership (default on) |
| `--synthetic-email-domain` | users: the domain for a user the source left without an email; unset skips such users, reported |

### 3.6 The three idioms that do not mean the same thing on the other side

The import carries the incumbent's authorization columns verbatim. Three of them are not
statements dorang can read as written, and the first of the three is the reason this section
exists at all.

Measured against a live incumbent with 54 keys: the arithmetic was exact — 54 scanned, 4
imported, 50 skipped for a missing team, every `lookup` matching `sha256(token)[:32]` of the
source digest — and all three idioms crossed as data with their meaning gone. The report said
nothing about any of them. It does now: every one of them appears in the report with the key,
the column and the value.

**1. `object_permission_id` — the one that failed OPEN, and is now resolved.**
A key's model restriction may live in the key's own `models` column *or* in a row of
`LiteLLM_ObjectPermissionTable` that the key points at. Carrying the id without reading that
table left dorang's `models` **empty**, and an empty allow-list allows **every** model — a key
silently gaining access it did not have.

The import now **reads that table** and folds the row's `models` into the key's own allow-list,
taking the intersection where both restrict. The report counts each one under *object
permissions resolved*. When the id **cannot** be resolved — the table is unreadable, the row is
absent, the row restricts something dorang has no allow-list for (`vector_stores`,
`mcp_servers`), or the key and the row have no model in common — **the key is refused and
named.** `--on-untranslatable=clear` does not apply to it, in either direction: clearing this
one drops a restriction rather than an unmatchable literal, and a widening is not something a
flag should decide.

**2. `allowed_routes: {llm_api_routes}` — a route group, not a path.**
The incumbent expands group names into sets of paths. dorang matches literal paths, `*`, or a
prefix ending `/*`, so a group name matches no request and the key is refused **every** route
with `403 route_not_allowed` — including `/v1/models`. Three of the four importable keys on the
live database carried this.

**3. `models: {all-team-models}` — a sentinel, not a model.**
Same shape, same direction: `all-team-models`, `all-proxy-models`, `all-model-access` and
`no-default-models` are instructions about the set of model names, and dorang reads them as one
literal model nobody serves. Two live keys carried this.

Both of the last two fail **closed**, which is why they are second priority and not third: the
key authenticates and can do nothing. Both are refused by default and named in the report. To
import them anyway:

```
dorangctl import keys --from <dsn> --on-untranslatable=clear --commit
```

`clear` drops the allow-list dorang could not express, which at key level means **unrestricted**
— a widening. It is opt-in for that reason, and every key it touches is named in the report as
`cleared`. For the model sentinels it clears the *whole* key-level list rather than the sentinel
entry, because a union containing "every team model" *is* every team model: dropping the
sentinel and keeping the literal beside it would leave the key narrower than the incumbent had
it, which is a different wrong answer and a quieter one. A key left unrestricted at key level is
still bounded by its team's and its user's limits.

Route groups are detected by shape, not by a list of names: anything that is not a path and is
not `*` is a name for a set dorang cannot enumerate. A group this code has never heard of is
refused too, which is the only direction that cannot fail open.

> **This section used to say the opposite, and it was right at the time.** It read *"There is
> no CLI entry point for credential import … no `dorangctl` subcommand and no administrative
> endpoint … Plan for reissuing."* The importer was complete and tested and had no operator-
> facing invocation, so the honest thing was to say so before a cutover rather than during
> one. The invocation exists now. **Reissuing is still a legitimate choice** — it is the only
> path that gets you `dorang_v1` hashing from the first request, with no §3.4 window to close
> later — but it is no longer the only one.

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
| `diffs` on the error envelope's `type` or `code` | The two gateways classify a condition differently. This is API surface: SDKs branch on status and `type`, and application code often matches `code` | [COMPATIBILITY.md](COMPATIBILITY.md) §11.2 has dorang's canonical condition table, with the deliberate divergences explained — budget exhaustion is a terminal `400` rather than a `429`, and "no healthy deployment" is `429` rather than `503`. Note that the *model allow-list* case answers `403 permission_error`, per §11.2 — an earlier revision of this line said `401`, which was true of the build before that row was corrected and is the kind of stale note that sends an operator chasing a diff that does not exist |
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
- [ ] `dorangctl catalog explain` run for every deployment carrying traffic: none has an
      `undeclared` context window (§2.3 item 4).
- [ ] `dorangctl catalog unverified` reviewed (§2.4): **zero `substituted`** on any model you
      route to — a substitution means you are billed for a model you did not ask for — and every
      `denied` is a known entitlement gap rather than a surprise.
- [ ] `dorangctl price` run on every model group, with no `no marginal_usage rule matched`.
- [ ] The control-plane inventory from §1.2 has an answer — a replacement, a script rewrite, or
      an accepted gap.
- [ ] The self-hosted backend flags are set ([OPERATIONS.md](OPERATIONS.md) §5). Every one of them
      fails as a `200` that ignored what was asked.
- [ ] `DORANG_KEY_PEPPER` set explicitly, and backed up with the database.
- [ ] `DORANG_MASTER_KEY` set, and known to be set — not the generated fallback.
- [ ] A restore has been rehearsed (§[OPERATIONS.md](OPERATIONS.md) §8.4), not just a backup taken.
- [ ] The store is the one you intend to keep (§6.1). There is no SQLite→PostgreSQL converter, so
      a shadow phase run on SQLite means re-importing keys before cutover.
- [ ] If more than one node: `capacity_mode` is not `local`, every `node_id` is distinct or empty,
      and `pre_stop_delay` covers the balancer's detection window (§6.4). The first two refuse to
      start; the third loses requests on every roll without saying so.

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
   node can serve any request, so *routing* is a routing decision. Standing the second node up is
   not: it needs a shared store and three settings that the gateway refuses to start without, or
   silently exceeds a ceiling with. See §6 before you add a node.
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

## 6. The store, and the second node

Everything above is about one dorang process on its default store. An incumbent that is already
backed by PostgreSQL is usually already more than one process, and the replacement has to be too.
This section is the part of the plan that is *not* a routing decision.

### 6.1 Choosing the store

dorang defaults to SQLite because [DESIGN.md](DESIGN.md) §0.2 requires a binary with no mandatory
dependencies. That default is right for the shadow phase in §4 and wrong for the deployment you
are migrating to:

| | SQLite | PostgreSQL |
|---|---|---|
| Nodes | **1** | 1–N |
| `cluster.enabled` | must stay `false` | `true` for two or more |
| Ledger partitioning | none — one table, retention deletes rows | daily partitions, retention drops whole partitions ([DESIGN.md](DESIGN.md) §9.5) |
| Restart gap | a sub-second outage on every restart, and it cannot be closed | none, with two nodes and a balancer |

```yaml
storage:
  driver: postgres
  postgres:
    url_env: DORANG_DATABASE_URL   # postgres://user:pass@host/dorang?sslmode=require
    max_conns: 32
```

**There is no SQLite→PostgreSQL converter.** If you ran the shadow phase on SQLite, the keys you
imported live in that file. Either run the shadow phase on the store you intend to keep, or re-run
`dorangctl import keys` against PostgreSQL before cutover — it reads the *incumbent's* database,
so it is repeatable and needs no plaintext (§3.2).

### 6.2 dorang can share the incumbent's database server

It does not have to share its schema, and should not. Name a schema in the DSN and dorang's
thirty-odd tables land inside it:

```
DORANG_DATABASE_URL='postgres://user:pass@host/litellm?search_path=dorang&sslmode=require'
```

Create it once (`CREATE SCHEMA dorang;`) and dorang's migrations do the rest. This keeps the
incumbent's tables and dorang's out of each other's way during the overlap in §5.3, when both
gateways are live and `dorangctl import keys --from postgres://…` is reading one while dorang
writes the other. Sharing a *server* is a capacity decision; sharing a *schema* is a collision
waiting for the first migration.

### 6.3 Migrations

Embedded, forward-only, and applied inside one transaction under an advisory lock, so several
nodes starting at once is the normal case rather than a race — the loser blocks, then finds every
step recorded and applies nothing.

- **On an empty database** and **on a database that already holds rows**, the set applies cleanly.
  Both are tested against a real PostgreSQL, the second one step at a time with data planted
  between every pair of steps (`TestMigrateOntoADatabaseWithData`).
- **The SQLite and PostgreSQL migration sets are compared**, after application, by introspecting
  both engines: tables, columns, type classes, nullability, primary keys, index columns,
  uniqueness and partial-index predicates (`TestMigrationSetsDoNotDrift`). They agree.
- **Upgrading a fleet:** apply once, from one node, *before* rolling — see
  [OPERATIONS.md](OPERATIONS.md) §9. Two binaries at different schema versions writing the same
  ledger is a schema race.

### 6.4 The three settings a second node needs

```yaml
cluster:
  enabled: true
  node_id: ""            # empty derives one per process, and cannot collide
  capacity_mode: shared-pg
server:
  pre_stop_delay: 10s    # >= your balancer's detection window
```

Each of them fails in its own way, and two of the three fail loudly:

- **`capacity_mode` must not be `local`.** `cluster.enabled: true` with `capacity_mode: local`
  **refuses to start** ([DESIGN.md](DESIGN.md) §5.6). Node-local counting is exact on one node and
  counts every ceiling once per node on N, so two nodes admit twice a provider's plan limit and
  the resulting upstream `429`s cascade into the fallback chain and consume the capacity of
  unrelated models. `shared-pg` publishes a maximum overshoot of **0**, and that figure is
  measured across two real processes rather than asserted.
- **`node_id` must be distinct, or empty.** Two processes carrying the same id are *one* node to
  the registry, to the leadership lease and to every leased limit — one lease that both hold, so
  every leader-only job runs twice. The second process refuses to start. An empty `node_id`
  derives one per process and is the only setting that cannot collide; its whole cost is that a
  restarted process cannot recognise its own leases and waits out their TTL.
- **`pre_stop_delay` is the one that fails quietly.** It is how long a draining node keeps serving
  *after* readiness goes false, and it exists because every balancer discovers unreadiness by
  polling. Set it below your balancer's detection window and a rolling restart is
  connection-refused at the client:

  ```
  pre_stop_delay >= probe period x failure threshold + probe timeout
                    + endpoint-withdrawal propagation
  ```

  Measured, two nodes over one PostgreSQL, a client driving both through a readiness-aware
  balancer polling at 100 ms: with `pre_stop_delay: 0s`, a rolling restart lost **870 of 59 600**
  requests. With `pre_stop_delay: 3s` and nothing else changed, **0 of 200 229**
  (`TestRollingRestartOfTwoNodesLosesNoRequest`). `deploy/kubernetes.yaml`'s probes need five
  seconds; the shipped default of `10s` covers them.

### 6.5 What two nodes actually cost you, in numbers

These are measured on two `dorang` processes against one PostgreSQL, with real clocks — not with
an injected clock in one process. An operator planning an incident response or a fleet size needs
the arithmetic, not the adjective:

| Event | Measured | Arithmetic |
|---|---|---|
| A leader is elected from cold, two nodes | **5.0 s** | one tick (5 s) |
| A **hard-killed** node's quota and budget leases return to the fleet | **65–70 s** | ledger lease TTL (60 s) + up to two leader ticks (5 s each) — **not** the 30 s node TTL, which only declares the node dead |
| A revocation reaches a node that did not issue it | **9–15 ms** | `poll + store_latency`; at `poll: 20ms` that is a published bound of 270 ms |
| Budget ceiling across both nodes | **0 overshoot** | 39 requests admitted against a ceiling of 40, driven concurrently at both nodes |
| A rolling restart of both nodes | **0 lost**, over four runs of ~200 000 requests each | with `pre_stop_delay` set per §6.4 |

The reclaim figure is the one worth reading twice. The registry declares a node dead after the
node TTL (30 s) — a Go-level default, **not** a YAML key, and strict decoding makes
`cluster.node_ttl` a load error, which matters on a page whose subject is which settings refuse
to start. The reclaim then deliberately refuses to take a lease that has not itself
expired — a lapsed heartbeat is a declaration, a lapsed lease is a fact, and reclaiming a live
node's block is how a two-node cluster once admitted 190 against a limit of 100. So a killed
node's units come back on the *lease* clock, not the heartbeat clock. Size a fleet against 70 s.

**Budget overshoot across nodes is 0 while every holder renews its lease**, and one lapse adds up
to the whole ceiling. That condition is not a footnote: it is the same mechanism as the recovery
above, pointed in the other direction.

---

## See also

- [CONFIG.md](CONFIG.md) §18 — every shadow key; §1 — the three refuse-to-start conditions an
  imported configuration is most likely to hit.
- [OPERATIONS.md](OPERATIONS.md) — day-two running, including the failure modes an incumbent
  never had because it never talked to a self-hosted engine.
- [COMPATIBILITY.md](COMPATIBILITY.md) — the wire contracts, the deliberate divergences from the
  reference proxy, and the error taxonomy a shadow diff will surface.
- [DESIGN.md](DESIGN.md) §2.4 — the credential-import rules; §14.1 — shadow comparison; §5.6 —
  the published overshoot of each capacity mode; §13 — multi-node operation and the drain.
