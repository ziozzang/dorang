# Operations

> The runbook: installing, starting, issuing keys, adding backends, reading the metrics,
> responding to alerts, backup, upgrade, draining, and the failure modes that will actually
> happen.
>
> This describes **the built system**. Where the design specifies behaviour this build does not
> have, it is marked ⚠️ **not in this build** and named in §12. Configuration keys are
> documented in [CONFIG.md](CONFIG.md); this document does not repeat them.
>
> **Verified against the working tree on 2026-07-28.** The implementation is moving, and the
> served surface in §0 has been growing milestone by milestone — check it against the build you
> are running rather than against this page. **A claim of absence is the claim most likely to
> have expired.**
>
> 한국어: [OPERATIONS.ko.md](OPERATIONS.ko.md)

---

## 0. What this build serves

Before planning anything, know the surface. Everything not listed under "served" answers **501
with a machine-readable code**, never a silent `404`.

### 0.1 Served

**T0 — an attached client breaks immediately without these:**

| Path | Methods | Authenticated |
|---|---|---|
| `/v1/chat/completions`, `/chat/completions` | POST | yes |
| `/v1/embeddings`, `/embeddings` | POST | yes |
| `/v1/messages`, `/v1/messages/count_tokens` | POST | yes |
| `/v1/models`, `/models` | GET, HEAD | yes |
| `/health`, `/health/liveness`, `/health/liveliness`, `/health/readiness` | GET, HEAD, OPTIONS | **no** |
| `/metrics` | GET, HEAD | **yes — master credential or an admin key**, unless `observability.metrics.public: true` |

**T1 — a generic SDK call fails without these:**

| Path | Methods | Notes |
|---|---|---|
| `/v1/completions`, `/completions` | POST | legacy text completions |
| `/v1/rerank`, `/rerank`, `/v2/rerank` | POST | three deployed spellings, one protocol |
| `/v1/moderations`, `/moderations` | POST | |
| `/v1/audio/speech` | POST | JSON in, bytes out |
| `/v1/audio/transcriptions`, `/v1/audio/translations` | POST | **multipart** |
| `/v1/images/generations` | POST | |
| `/v1/images/edits`, `/v1/images/variations` | POST | **multipart** |
| `/v1/responses` | POST | plus `/v1/responses/{id}`, `/{id}/input_items`, `/{id}/cancel` — the stateful sub-resources are mounted only when the response store exists |
| `/v1/models/{id}`, `/models/{id}` | GET, HEAD | |
| `/v1/batches`, `/v1/batches/{id}`, `/v1/batches/{id}/cancel` | GET, POST | |
| `/v1/files`, `/v1/files/{id}`, `/v1/files/{id}/content` | GET, POST, DELETE | |

**Deployment-in-the-path aliases**, for Azure-shaped and legacy-engine-shaped clients:
`/engines/{model}/{chat/completions,completions,embeddings}` and
`/openai/deployments/{model}/…` — the latter additionally covering `audio/speech`,
`audio/transcriptions`, `audio/translations`, `images/generations`, `images/edits` and
`responses`. These work only because the route table is **specificity-ordered**; a naive prefix
router swallows every one of them into a configured passthrough catch-all, and the symptom is an
Azure-shaped client silently reaching a vendor passthrough.

**Passthrough**: `<prefix>/…` for each `passthrough.routes[]` entry, authenticated per route.

Two registration details that are deliberate rather than accidental:

- `/chat/completions` sits **beside** `/v1/chat/completions` rather than redirecting to it. Both
  base-URL conventions exist in the wild and both must work; a `307` on a POST costs a round trip
  and some clients drop the body.
- `/health/liveliness` is not a typo dorang chose. The widely-deployed proxy this surface
  interoperates with spells it that way, container manifests in the field probe that exact path,
  and the correct spelling exists alongside it.

### 0.2 Not served — 501 with a reason

| Code | Meaning |
|---|---|
| `route_not_implemented` | A route dorang declares and this build has not mounted: `/v1/ocr`, `/v1/vector_stores`, `/v1/assistants`. `/v1/responses`, `/v1/files` and `/v1/batches` are also on this list, because their subsystems are mounted conditionally — a build without them must answer "declared, not built" rather than "no such path" |
| `route_unknown` | Anything else |

A 501 carries `X-Dorang-Unimplemented: <path>`. A known path with the wrong method answers
**405** with an `Allow` header and code `method_not_allowed`.

**The HTTP administrative surface is mounted.** `/key/*`, `/user/*`, `/team/*`, `/model/*`,
`/budget/*`, `/spend/*`, `/admin/*` and the embedded read-only UI at `/ui` are served by
`cmd/dorang`, behind `DORANG_MASTER_KEY` or a key whose owning user holds an administrative role.
Every mutation writes an `audit_logs` row.

Not every endpoint has storage behind it. The ones that do not answer **501
`dependency_not_configured`** naming the missing piece rather than a generic refusal — see §3.2
for the list and §12 for why. The distinction matters when you are reading a 501: a
`dependency_not_configured` says *this build cannot serve it*; a `route_not_implemented` says
*dorang has not built it*.

### 0.3 One behaviour worth knowing before you read a log

**A model outside a key's allow-list answers `401`, not `403`.** The caller authenticated fine,
so `403` reads better — but the reference proxy every deployed client was built against answers
`401` here, and clients branch on it. It is a deliberate divergence from what the error taxonomy
would otherwise imply; see [COMPATIBILITY.md](COMPATIBILITY.md) §7.2 and §11.2.

The byte-level contracts for everything served are in [COMPATIBILITY.md](COMPATIBILITY.md), and
every row there is a golden test rather than a note.

---

## 1. Install

### 1.1 Binary

Release archives carry both binaries for `linux/amd64`, `linux/arm64`, `darwin/amd64` and
`darwin/arm64`, each with a `.sha256` beside it.

```
tar xzf dorang_v1.2.3_linux_amd64.tar.gz
sha256sum -c dorang_v1.2.3_linux_amd64.tar.gz.sha256
install -m 0755 dorang dorangctl /usr/local/bin/
dorang --version
```

`CGO_ENABLED=0` throughout: the SQLite driver is pure Go, so the result is a static binary with
no libc dependency. That is what makes the notebook tier's "zero required dependencies" literally
true, and it is why a release binary runs on any kernel of the right architecture.

From source:

```
make build          # -> bin/dorang, bin/dorangctl
make test
make test-race
```

### 1.2 Container

```
docker pull ghcr.io/ziozzang/dorang:1.2.3
```

The runtime layer is `gcr.io/distroless/static-debian12:nonroot` — the two binaries and the TLS
root store, no shell, no package manager, nothing to pivot from if the process is ever
compromised. It runs as `nonroot`, exposes `4100`, and declares a volume at `/var/lib/dorang`.

```
docker run -d --name dorang \
  -p 4100:4100 \
  -v dorang-state:/var/lib/dorang \
  -v /etc/dorang:/etc/dorang:ro \
  -e DORANG_MASTER_KEY \
  -e DORANG_KEY_PEPPER \
  -e CLOUD_A_KEY_1 \
  ghcr.io/ziozzang/dorang:1.2.3
```

The entrypoint is `dorang --config /etc/dorang/config.yaml`.

Two things about the image that used to bite you, and no longer do:

**The `HEALTHCHECK` works.** It invokes `dorangctl health --addr http://127.0.0.1:4100`, which
probes `/health/liveliness` and exits non-zero on anything but a `200`. Liveness rather than
readiness on purpose: a draining node is alive and must not be restarted (§13), and conflating
the two turns a graceful drain into a kill. `--ready` probes readiness when that is what you
want, and `--addr` defaults from `DORANG_LISTEN`, so an image that moved its listener does not
also have to rewrite its probe.

This was broken for the whole of the image's life: there was no `health` subcommand, so the
call exited 2 with `unknown command "health"` and the container was reported unhealthy from
`start-period + 3 × interval` onwards regardless of the process's real state. The image is
distroless — no `curl`, no `wget`, no shell — so nothing else in it could have probed either.
`cmd/dorangctl/health_test.go` now reads the Dockerfile's own `HEALTHCHECK` line and feeds its
arguments to the real dispatcher, because the two halves disagreeing was the defect, not either
half on its own.

**`DORANG_STATE_DIR=/var/lib/dorang` is read.** Every shipped state path is written
`~/.dorang/…`, and a leading `~` resolves to the state directory: the database, the trace
spool, batch blobs, the shadow report, and the generated key pepper. Setting an absolute path
in the file still wins — that is an instruction, not a default, and re-rooting it silently would
make the variable a way to redirect a deliberate choice.

Before this, `~` was the serving user's home, which under `nonroot` is `/home/nonroot`: outside
the declared volume, in the container's writable layer, and gone on `docker rm`. The database
was the visible loss; the pepper was the expensive one, because regenerating it makes every api
key issued under it unverifiable.

To place state somewhere other than the volume, say so:

```yaml
storage: {driver: sqlite, sqlite: {path: /srv/dorang/dorang.db}}
metering: {spool: {dir: /srv/dorang/spool}}
```

### 1.3 Kubernetes probes

```yaml
livenessProbe:
  httpGet: {path: /health/liveness, port: 4100}
  periodSeconds: 10
readinessProbe:
  httpGet: {path: /health/readiness, port: 4100}
  periodSeconds: 2
```

Use **both**, and do not point liveness at readiness. A draining node is alive and must not be
restarted; conflating the two turns a graceful drain into a kill. Liveness stays `200
{"status":"alive"}` for the whole drain, and readiness flips to `503 {"status":"draining"}` the
instant the drain begins.

The readiness probe's period and failure threshold are what `server.pre_stop_delay` must be sized
against — see §10, and `deploy/kubernetes.yaml` for probe settings the default already covers.

`terminationGracePeriodSeconds` must exceed the pre-stop delay plus the drain plus the post-drain
teardown, the last two each bounded by `server.shutdown_grace`. Budget
`pre_stop_delay + 2 × shutdown_grace + 10s`.

---

## 2. First start

### 2.1 The zero-configuration start

With no `--config`, no `$DORANG_CONFIG` and no `dorang.yaml` in the working directory, the
server comes up on documented defaults: SQLite at `~/.dorang/dorang.db`, no upstreams, listening
on `:4100`. It serves `/health*`, `/metrics` and `/v1/models` (empty), and 501s the rest — which
is exactly what "a single binary with no required dependencies serves requests" means. A file
named explicitly and then not found is an error, because that is a typo rather than a choice.

### 2.2 The normal start

```
export DORANG_MASTER_KEY="$(head -c 32 /dev/urandom | base64)"
export DORANG_KEY_PEPPER="$(head -c 32 /dev/urandom | base64)"  # generated, not a literal
export CLOUD_A_KEY_1=…

dorangctl config lint /etc/dorang/config.yaml
dorang --config /etc/dorang/config.yaml --check
dorangctl migrate --config /etc/dorang/config.yaml
dorang --config /etc/dorang/config.yaml
```

On success:

```
dorang v1.2.3 listening on [::]:4100 (/etc/dorang/config.yaml)
```

On failure, every problem is printed at once with its YAML path:

```
dorang: /etc/dorang/config.yaml: refusing to start, 2 problem(s):
  cluster.capacity_mode: cluster.enabled is true with capacity_mode "local", which refuses to start: …
  credentials[1].key_env: environment variable CLOUD_A_KEY_2 is not set
```

### 2.3 Set the pepper before you issue anything

If `DORANG_KEY_PEPPER` is unset, a pepper is **generated once and written beside the SQLite
database**, so the notebook tier works with nothing installed. The consequences:

- The pepper travels with the database file. Back them up together, or every issued key becomes
  unverifiable.
- On more than one node, each generates its own pepper, and keys issued by one node fail
  authentication on every other. **Set the variable explicitly on any deployment with more than
  one process.** The server logs when it generated one.

### 2.4 Migrations

`dorangctl migrate` applies embedded migrations and exits, reporting what it applied:

```
applied 3 migration(s), 0 -> 3:
  1 initial
  2 batch_columns
  3 notional_column
```

or `schema is already at version 3; nothing to apply`. The server also applies migrations at
start-up, so the explicit call is for staged deployments where you want the schema change to be
a separate, reviewable step.

### 2.5 Verify

```
curl -s localhost:4100/health/readiness            # {"status":"ready"}
curl -s localhost:4100/metrics | head
curl -s localhost:4100/v1/models -H 'Authorization: Bearer sk-…'
```

---

## 3. Issuing keys

Keys are `sk-` prefixed, 32 bytes of randomness, base64url without padding. What is persisted is
`HMAC-SHA256(pepper, token)`, so a stolen database is not offline-attackable. Nothing recoverable
is kept, which is why the token is printed once.

```
dorangctl key create --config /etc/dorang/config.yaml \
  --alias "team-search" \
  --user u_42 --team t_7 \
  --models model-large,model-small \
  --routes /v1/chat/completions,/v1/models \
  --budget-usd 250 \
  --ttl 2160h \
  --priority-class interactive
```

| Flag | Meaning |
|---|---|
| `--alias` | Display alias |
| `--user`, `--team` | Owning ids |
| `--models` | Comma-separated allow-list; empty allows all. Names are compared **whole** |
| `--routes` | Comma-separated route allow-list; empty allows all |
| `--budget-usd` | Spend ceiling. `0` means no ceiling |
| `--rpm`, `--tpm` | Rate ceilings. ⚠️ Stored and **not enforced** in this build — see §12 |
| `--max-parallel` | Concurrency ceiling. ⚠️ Stored and **not enforced**; use `capacity.principals.<key-id>` instead |
| `--ttl` | Expire after this long. `0` never expires |
| `--priority-class` | Priority class name |

The token goes to **stdout**; the confirmation goes to stderr, so `dorangctl key create … > key.txt`
captures exactly the token.

```
dorangctl key list --config /etc/dorang/config.yaml --limit 50
dorangctl key revoke --config /etc/dorang/config.yaml <key-id>
```

`key list` shows id, label, alias, user, team, hash scheme, state (`active` / `blocked` /
`expired`), spend, budget, source and creation time.

**Revoke blocks, it does not delete.** The ledger references the key id, and a deleted row would
turn every historical request into an orphan. An expired or blocked key must stay present and be
refused *as such*, never disappear and be refused as unknown.

⚠️ **A running gateway caches credentials.** A revocation takes effect within the cached entry's
TTL or on the next reload, not instantly. `dorangctl key revoke` says so on stderr. If you need
it now, `SIGHUP` the process after revoking.

### 3.1 Revoking a leaked key

This is the incident-response procedure. It needs an HTTP client and the administrative
credential, and nothing else — no shell on the box, no database client, no restart.

```
curl -s -X POST http://gateway:4000/key/block \
  -H "Authorization: Bearer $DORANG_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"key_id":"<key id>"}'
```

The response is the key's new state. An `audit_logs` row is written before the call returns; a
mutation that could not be audited is refused rather than applied silently, and if the write
fails *after* the change was applied you get `500 audit_write_failed` telling you to reconcile
rather than retry.

**How fast it takes effect.** `internal/auth` caches a loaded credential for `DefaultEntryTTL`
(60 s), so the key stops working within a minute of the block. `Authenticator.use` enforces
`Blocked` on every request independently of the authorization checks, so no second gate has to be
reached. `SIGHUP` clears the cache immediately if the minute matters.

**In-flight batches stop too.** `batchExecutor` re-resolves the owning credential per row within
its 30 s owner-resolution TTL and calls `Authorize`, so a blocked key's running batch stops
spending. Before that check existed, a batch submitted before a block kept spending until it
finished.

**Finding the key id from a leak report.** The label is derived from the first 8 hex characters
of the lookup digest (`store.LabelFor`) and never from the secret's own characters, so a leaked
token's label identifies the row without the token going anywhere:

```
curl -s "http://gateway:4000/key/list?limit=200" \
  -H "Authorization: Bearer $DORANG_MASTER_KEY"
```

⚠️ **Never send the plaintext key as a parameter.** `/key/info` and `/key/block` take `key_id`
and refuse a `key` parameter with code `secret_in_request`. A live credential in a URL is a live
credential in your access logs, your proxy traces and your shell history — which is how the leak
you are responding to often happened.

**Rotating instead of blocking.** `POST /key/regenerate` mints a new secret behind the same key
id, keeping every authorization field, and returns the new token exactly once. The cutover is
immediate: there is no grace window in which the old secret still verifies, because a grace
window on an incident path keeps the compromised secret working for its duration.

**Before this surface was mounted** the only lever was a hand-written
`UPDATE api_keys SET blocked = 1 WHERE id = '<key id>'` against the database, which needs shell
access to the node, cannot be done from a runbook over HTTP, and leaves no audit row. That is no
longer the procedure. It still works as a break-glass path if the administrative credential
itself is what leaked.

### 3.2 What the administrative surface serves, and what it does not

Mounted and working:

| Path | Notes |
|---|---|
| `/key/generate`, `/key/info`, `/key/update`, `/key/delete`, `/key/list`, `/key/block`, `/key/unblock`, `/key/regenerate` | The whole credential lifecycle. `/key/regenerate` cuts the old secret outright — it is the incident path. **`spend` is read from DESIGN §9.4's per-key rollup**, over the key's own `budget_duration` window, so it is the same figure `/spend/logs` and `/global/spend/report` report. It used to come from `api_keys.spend_nano`, which the importer writes and the request path does not, so **every key reported `"spend": 0`** while the ledger, the response header and the report all agreed on another number. The durable budget counter is deliberately not the source: a node charges a whole lease block to it before spending a unit (§9.6), so it runs ahead of reality by design. A key with no dorang traffic yet keeps whatever the importer wrote, so an imported spend is still visible until this deployment has a figure of its own |
| `/key/rotate`, `/key/rotate/cut`, `/key/secrets` | DESIGN §11.2c rotation: a new secret with a grace window, an early cut, and the list an operator reads to answer "did the client roll?". Same durable key id, so budget, spend, allow-list and ledger history are untouched |
| `/key/pend`, `/key/release` | §11.6's reversible refusal. Distinct from block, because a pend is a statistical judgement that might be wrong and an operator releases it in one action |
| `/spend/logs` | Per-request ledger. Requires one of `key_id`, `team_id`, `trace_id`, `tag` or `errors_only`, plus a bounded date range — §9.3 refuses an unbounded scan rather than answering it slowly |
| `/global/spend/report` | Aggregated spend over a bounded range, read off DESIGN §9.4's rollups. `group_by` takes `day` and **one** of `model`, `key` or `team` — those are the keys the three materializations actually have. §9.4 keeps purpose-built rollups rather than a cube, so any other grouping (`user`, `tag`, `provider`, or two subject dimensions at once) answers **501** naming what is missing instead of turning into a table scan; `/spend/logs` joins those per request. `notional_spend` is reported as `null`, never as `0`: the rollups carry no list-rate column, and §8.5 forbids the flattering answer. `marginal_spend` and `subscription_spend` are read from their own columns and agree with `/spend/logs` row for row — they used to be `cost_nano` reported twice, so a flat plan's share was aggregated as marginal usage on the report an operator compares models by. Rows written before those columns existed report `0` for both, because the split was never recorded and there is nothing to recover it from |
| `/admin/capacity` | Live broker occupancy per axis |
| `/admin/catalog/explain`, `/admin/catalog/unverified` | Where each catalogued model field came from |
| `/health/history` | In-process ring; it starts empty on every restart |
| `/ui` | Read-only operator UI. Sign in with an administrative credential; the session is one hour, which is also the lag before a revoked credential loses the UI |

Answering **501 `dependency_not_configured`** in this build, because the storage behind them does
not exist yet — `internal/store` has no Go code for `users`, `teams`, `team_members`,
`deployments` or `model_aliases`:

| Path | Missing piece | Use instead |
|---|---|---|
| `/user/*`, `/team/*` | directory | `dorangctl`, or the database |
| `/model/*`, `/model_group/info` | model registry | configuration + `SIGHUP` |
| `/budget/*` | budget store | `dorangctl key create --budget-usd` |
| `/user/daily/activity`, `/team/daily/activity`, `/tag/daily/activity` | a per-day per-subject per-**model** cube, which §9.4 deliberately does not materialize | `/global/spend/report` for the single-dimension question, `/spend/logs` for the per-request one |
| `/admin/credentials/health`, `/admin/quota` | quota registry | `/metrics` |
| `/spend/calculate`, `/admin/pricing/preview` | pricing engine | — |
| `/admin/config/reload` | reloader | `SIGHUP` |

### 3.3 Administrative scope

There are two kinds of administrator and the difference is a fact the schema already holds.

- **Global.** `DORANG_MASTER_KEY`, and an administrative key that belongs to **no team**. This is
  the operator: every subject, every endpoint.
- **Team-scoped.** An administrative key that **does** belong to a team (`api_keys.team_id`). It
  administers that team's keys, members, budgets and spend, and nothing else.

The scope is derived from the key rather than stored, so it cannot be forgotten: a key that is on
a team is scoped by being on a team. There is no `admin_team` role to set and no migration to
skip.

What a team-scoped administrator gets:

| Attempt | Answer |
|---|---|
| Its own team's key, by id | Served |
| Another team's key, by id | **404**, identical to an id that does not exist — a 403 would turn every key id into an existence oracle |
| `?team_id=` naming another team | **403 `out_of_scope`** — a parameter refusal discloses nothing and tells you which parameter to fix |
| `/key/list` with no filter | Its own team only, never the deployment |
| `/spend/logs` reaching another team's rows via `key_id` or `trace_id` | Rows dropped; the store filter is an optimization, the row check is the enforcement |
| Minting a key on another team, or on no team | **403** — a key on no team would be a *global* administrator |
| Changing a user's `user_role` | **403** — the role is what decides who administers, so writing it is escalation |
| `/model/*`, `/admin/*`, `/global/spend/report`, `/user/new`, `/team/new`, `/health/history` | **403** — deployment-wide, and there is no honest per-team view of "reload the configuration" |

A credential that authenticates but may not administer gets **403**, not 401: a working key being
told its key does not work sends the operator to the wrong problem. **401** means no usable
credential, and it is now the answer to every administrative path for an unauthenticated caller —
including the unknown ones. The route table is not readable before authentication.

### 3.4 The administrative credential

`DORANG_MASTER_KEY` is compared out of band in constant time and is **never a row**. It is not
issued, not listed, not revocable through `dorangctl`. Rotating it is an environment change plus
a restart.

The authenticator refuses to build without it unless an explicit opt-out is set. That is
deliberate: a gateway that reads only an imported credential database has no administrator at
all and does not notice.

---

## 4. Adding a provider and a model

The order matters, because every reference is checked by name at load.

**1. Declare the provider.**

```yaml
providers:
  - name: cloud-b
    kind: anthropic
    base_url: https://api.example-cloud-b.invalid
    timeout: 180s
    max_concurrency: 12
    capacity_group: cloud-b-pool
```

**2. Declare its capacity group**, or the provider reference is refused:

```yaml
capacity:
  provider_groups: {cloud-b-pool: {max_concurrency: 12}}
```

**3. Declare a credential**, with its secret as a reference and the environment variable actually
set:

```yaml
credentials:
  - {id: cloud-b-1, provider: cloud-b, key_env: CLOUD_B_KEY_1, capacity_group: cloud-b-acct}

capacity:
  credential_groups: {cloud-b-acct: {max_concurrency: 6}}
```

**4. Check what the catalog thinks the model can do**, before you route to it:

```
dorangctl catalog explain anthropic my-model-name
dorangctl catalog unverified
```

`catalog explain` prints, per field, the resolved value, which layer won it and which file wrote
it. An operator whose context window is wrong needs to know which file to edit; an operator who
wrote an overlay needs to know whether it applied. Both are guesses without this.

`catalog unverified` is the probe list: models dorang will answer capability questions about from
data carrying no verification date. **An unverified capability is not the same as an absent one**,
and reasoning capability in particular is never inferred from a name prefix — generalizing an
effort scale observed on one model version to a whole family is exactly how the control gets
silently dropped on the family's other members.

**5. Add the deployment**, either to a new group or as a second candidate on an existing one:

```yaml
models:
  - name: model-x
    class: chat-large
    strategy: [prefix_sticky, lowest_cost, least_busy]
    deployments:
      - {provider: plan-a,  upstream_model: model-x, credentials: [plan-a-1], weight: 10, priority: 0}
      - {provider: cloud-b, upstream_model: my-model-name, credentials: [cloud-b-1], weight: 5, priority: 1}
```

**6. Price it**, or it routes as free. An unpriced model has *no opinion* for routing rather than
being cheapest, but it is also recorded as UNPRICED rather than as costing zero:

```
dorangctl price model-x --config /etc/dorang/config.yaml --input 12000 --output 800 --cached-read 9000
```

`price` and `catalog explain` take their subject **first** and their flags after; `key create`,
`key list` and `migrate` take flags only. That is not cosmetic — Go's flag package stops at the
first non-flag token, so `dorangctl price --config … model-x --input 10` parses nothing.

This prints the applied rule chain per class, each component's rate and quantity, the subtotal,
the final amount, **why each rule was selected**, and the notional list-rate equivalent with its
provenance and age. It runs the same evaluator the ledger runs, so the number here, the number
the calculator would show and the number in the ledger are the same number.

If it warns `no marginal_usage rule matched`, the request would be recorded UNPRICED. Fix that
before the deployment carries traffic.

**7. Lint, then reload.**

```
dorangctl config lint /etc/dorang/config.yaml
kill -HUP $(pidof dorang)
```

A failed reload keeps the running configuration and logs
`reload refused, keeping the running configuration: …`. Nothing is applied partially.

Remember what a reload does **not** rebuild: the store, the authenticator, the meter and the
capacity broker. A change to `storage.*` or to any capacity ceiling needs a restart.

---

## 5. Self-hosted backends

dorang does not tune the engine and does not paper over a missing flag by emulating the
behaviour. If prompt-token details are not enabled, it reports cached-token pricing as
unavailable rather than guessing — because emulation would make the gateway's numbers disagree
with the engine's, and disagreeing numbers are worse than absent ones.

So the flags are the operator's job, and both engines **accept requests and return `200` while
ignoring what was asked** when they are missing. The full profiles, with what silently breaks
without each flag:

- **vLLM** — [VLLM.md](VLLM.md) §5, plus §1.2 (priority is silently ignored under the default
  scheduler policy, and its own field description claims otherwise), §3 (load signals and their
  four traps), and §8 (five places vLLM's documentation contradicts its source).
- **SGLang** — [SGLANG.md](SGLANG.md) §8.1 (the shared runbook, side by side with vLLM), §8.2
  (set these), §8.3 (**do not** set these), and §8.4 (security posture — read before exposing an
  SGLang port at all).

Three items from those documents that change what an operator does today, and are worth
repeating here because they are silent failures:

| Symptom | Cause | Fix |
|---|---|---|
| Every cached request is billed at full rate | vLLM: `--enable-prompt-tokens-details` unset. SGLang: `--enable-cache-report` unset | Set the flag. dorang cannot detect the difference; a null `cached_tokens` and a genuine zero look identical |
| Agentic clients get plain text instead of tool calls, with no error | SGLang: `--tool-call-parser` unset. The tools are still rendered into the prompt and the model's native syntax lands in `message.content` | Set the parser. vLLM 400s in this situation; SGLang returns 200 |
| `priority` has no effect | vLLM: `--scheduling-policy priority` unset. SGLang: `--enable-priority-scheduling` unset | Set it. And note that the two engines order priority in **opposite directions** — [CONFIG.md](CONFIG.md) §20.1 |

⚠️ **Never set `--allow-auto-truncate` (SGLang) or send `truncation: "auto"` /
`truncate_prompt_tokens` (vLLM).** Both turn a context overflow into a silent `200` over a
truncated prompt, which removes the only signal `context_window` fallback has.

---

## 6. Reading the metrics

`GET /metrics` returns Prometheus text exposition format 0.0.4 (`text/plain; version=0.0.4`). It
is hand-rolled rather than backed by a client library — the notebook tier has no required
dependencies, and that includes a metrics library.

**`/metrics` authenticates.** It needs the master credential (or an admin key) unless
`observability.metrics.public: true` says otherwise, and `observability.prometheus: false`
removes the route entirely — it then answers 501 with a reason, like any other route this build
does not serve. Both were unread before: the scrape carries per-key spend, per-credential quota
state and every configured model name, on whatever port the gateway listens on.

A scrape configuration therefore needs a credential:

```yaml
scrape_configs:
  - job_name: dorang
    authorization: {type: Bearer, credentials_file: /etc/prometheus/dorang-master-key}
    static_configs: [{targets: ["dorang:4100"]}]
```
The config key is never read. If the port is reachable, so is the scrape. Put it behind your
network boundary.

⚠️ **Every metric is unlabelled by caller.** There are no per-path, per-model or per-key labels
anywhere, deliberately: axis keys and model names are unbounded cardinality, and a gateway that
lets a client mint label values has handed out a denial of service on its own metrics registry.
Per-key and per-model figures come from the ledger, not from the scrape.

### 6.1 Core metrics

| Metric | Type | What it measures |
|---|---|---|
| `dorang_requests_total` | counter | Requests served |
| `dorang_responses_total{class}` | counter | Responses by status class: `1xx`…`5xx` |
| `dorang_request_duration_seconds{le}` | histogram | Gateway request duration. Buckets from 100 µs to 60 s |
| `dorang_request_bytes_total` | counter | Request body bytes read |
| `dorang_response_bytes_total` | counter | Response body bytes written |
| `dorang_unimplemented_total` | counter | Requests answered 501 |
| `dorang_auth_failures_total` | counter | Refused by authentication or authorization |
| `dorang_late_errors_total` | counter | Errors delivered **in band** because the response had already started |
| `dorang_handler_panics_total` | counter | Panics recovered in a handler |
| `dorang_meter_panics_total` | counter | Panics recovered in the meter; requests unaffected |
| `dorang_passthrough_requests_total` | counter | Requests served by the generic passthrough engine, counted at entry — a relay that cannot dial its upstream still counted |
| `dorang_websocket_upgrades_total` | counter | WebSocket upgrades relayed |
| `dorang_replay_refused_total` | counter | Requests marked non-replayable because the process-wide replay budget was full |
| `dorang_body_too_large_total` | counter | Requests refused for exceeding the body cap |
| `dorang_shadow_observed_total` | counter | Requests captured for shadow comparison |
| `dorang_observer_panics_total` | counter | Panics recovered in the shadow observer |
| `dorang_inflight_requests` | gauge | Requests currently being served |
| `dorang_replay_bytes` | gauge | Request-body bytes retained for replay |
| `dorang_ready` | gauge | `1` when accepting new work, `0` while draining |
| `dorang_uptime_seconds` | gauge | Seconds since start |

### 6.2 Shadow metrics

Present only when `shadow.mode != off`. All unlabelled. Full list and meaning in
[MIGRATION.md](MIGRATION.md) §4.4. The ones an alert should watch:

`dorang_shadow_cost_capped` (gauge, `1` when the daily ceiling has stopped shadowing),
`dorang_shadow_with_diffs_total`, `dorang_shadow_inconclusive_total`,
`dorang_shadow_dropped_total`, `dorang_shadow_reference_errors_total`,
`dorang_shadow_skipped_unsafe_total`, `dorang_shadow_report_dropped_total`.

> `dorang_shadow_cost_stops_total` is declared as a **gauge** despite the `_total` suffix. It is
> a monotonic count of ceiling trips; treat it as a counter in queries and expect your linter to
> complain.

### 6.3 The health endpoints

| Path | Normal | Draining |
|---|---|---|
| `/health/liveness`, `/health/liveliness` | `200 {"status":"alive"}` | `200 {"status":"alive"}` |
| `/health/readiness` | `200 {"status":"ready"}` | **`503 {"status":"draining"}`** |
| `/health` | `200 {"status":"healthy"}` | **`503 {"status":"draining"}`** |

When shadowing is on, the body carries a `"shadow"` object with the gate verdict:

```json
{"status":"healthy","shadow":{"mode":"compare","sample_rate":0.05,"cost_capped":false,
 "spent_usd":"1.240000000","limit_usd":"5.000000000","sampled":812,"compared":790,
 "clean":790,"with_diffs":0,"inconclusive":0,"queue_dropped":0,"reference_errors":0,
 "report_dropped":0,"skipped_unsafe":22,"gate":"clean"}}
```

A stopped shadow never makes the gateway unhealthy. A stopped shadow is a diagnostic failure,
not a serving failure, and taking a pod out of rotation over one turns a diagnostic into an
outage. That is precisely why it appears on the health endpoint: "is the cutover gate still
running" is a question asked of a health endpoint, and a ceiling that silently stopped shadowing
three days ago would otherwise be discovered by someone reading an empty diff report as proof of
readiness.

### 6.4 The per-request headers

Every response carries these, unconditionally:

`X-Dorang-Request-Id`, `X-Dorang-Model`, `X-Dorang-Upstream-Model`, `X-Dorang-Deployment`,
`X-Dorang-Cost-Usd`, plus `Retry-After` on a 429 and the `X-Ratelimit-*` set when known.

The rule is: **a header the client acts on is unconditional; a header the client reads may be
gated.** Gating `Retry-After` behind a telemetry flag means every SDK's backoff silently stops
working, which an earlier draft did by taking the "bounded header set" rule literally.

Send `X-Dorang-Detail: full` (or set `observability.always_full_headers`) to add:

`X-Dorang-Provider`, `-Credential`, `-Attempt`, `-Fallback-From`, `-Route-Reason`, `-Queue-Ms`,
`-Ttft-Ms`, `-Latency-Ms`, `-Tokens-Input`, `-Tokens-Output`, `-Tokens-Cache-Read`,
`-Tokens-Cache-Write`, `-Tokens-Reasoning`, `-Notional-Usd`, `-Spend-Usd`, `-Budget-Usd`,
`-Budget-Remaining-Usd`, `-Quota-<Window>-Used-Pct`, `-Dropped-Params`,
`-Native-Stop-Reason`, `-Replayable`.

A counter header is **omitted at zero**, because an absent counter and a counter of zero are
different claims. `X-Dorang-Replayable` appears only when it is `false`, and it is
unconditional for that reason — it changes retry semantics a caller may be relying on.

`X-Dorang-Native-Error-Type` is attached on errors regardless of the detail flag. The upstream's
own error `type` and `code` are recorded and surfaced there, and **never put in the response
body**: forwarding them would make a client branching on `type` mis-branch on a vendor-specific
string.

Three inbound request-id headers are honoured, first non-empty wins, capped at 128 bytes:
`X-Dorang-Request-Id`, `X-Request-Id`, `X-Correlation-Id`.

---

## 7. Alerts

Each row is a condition worth paging on, what it actually means, and the first action.

### 7.1 Serving

| Alert | Expression | What it means | Do |
|---|---|---|---|
| Gateway erroring | `rate(dorang_responses_total{class="5xx"}[5m]) > 0` | A gateway fault or an upstream 5xx that survived the fallback chain. `502` is upstream-after-fallback; `500` is dorang's own fault | Check `dorang_handler_panics_total` first. A non-zero panic counter is a bug, not a capacity problem |
| Panics | `increase(dorang_handler_panics_total[15m]) > 0` | A handler panicked and was recovered. The request failed; the process did not | Capture the log line and the request id. This is always a defect |
| Late errors | `rate(dorang_late_errors_total[5m]) > 0` | An error had to be delivered **in band** because the response had already started. The client saw HTTP 200 followed by an error event | Correlate with upstream health. This is the boundary fallback deliberately will not cross — duplicated output is worse than a visible failure |
| Not ready | `dorang_ready == 0` for > `pre_stop_delay + shutdown_grace` | The node is draining, or the drain did not finish | If no deploy is in progress, the process is stuck draining. See §10 |
| 501 storm | `rate(dorang_unimplemented_total[5m])` rising | A client is calling a route this build does not serve — very often `/v1/responses` or an admin path (§0.2) | Read `X-Dorang-Unimplemented` from a sample response. It names the path |
| Auth failures | `rate(dorang_auth_failures_total[5m])` rising | Expired keys, a revoked key still in a client's config, or a key issued under a different pepper | `dorangctl key list` shows state per key. If *every* key fails, suspect the pepper (§2.3) |
| Replay refusals | `rate(dorang_replay_refused_total[5m]) > 0` | The process-wide replay budget is full, so bodies are no longer retained and those requests cannot fall back | Large bodies plus high concurrency. Either is fine alone. Watch `dorang_replay_bytes` |
| Body refusals | `rate(dorang_body_too_large_total[5m]) > 0` | Requests over the body cap | A client is sending something it did not send before — often an embedded document |

### 7.2 Latency

| Alert | Expression | What it means | Do |
|---|---|---|---|
| Gateway overhead p99 | `histogram_quantile(0.99, rate(dorang_request_duration_seconds_bucket[5m]))` | ⚠️ This includes upstream time. It is **not** the gateway-overhead figure the design targets | Do not alert on the design's 2 ms p99 against this series. Alert on a baseline you measured for your own traffic |
| Duration histogram flat at the top bucket | all mass in `le="+Inf"` | Requests are exceeding 60 s. Usually a long generation, occasionally a wedged upstream | Correlate with `dorang_inflight_requests` — a wedged upstream holds slots |

> **On the published targets.** The design's numbers are *gateway overhead*: from the last byte
> of the request line and headers being read to the first byte written upstream, plus from the
> last upstream byte to the last byte written to the client, **excluding upstream time**.
> Measured components: the gate alone is 59 ns and zero-allocation; end to end through the HTTP
> surface is 1.5 µs and 17 allocations, 0.75% of the warm-local budget; routing with ten
> candidates and prefix, sticky, capacity and pricing all live is ~5.5 µs, 2.8%; metering adds
> 148 ns. `dorang_request_duration_seconds` measures something else, and no exported series
> isolates the overhead. Use the headers (`X-Dorang-Queue-Ms`, `-Ttft-Ms`, `-Latency-Ms`) for
> per-request breakdown.

### 7.3 Shadow

| Alert | Expression | What it means | Do |
|---|---|---|---|
| Shadow stopped by cost | `dorang_shadow_cost_capped == 1` | The daily ceiling tripped. Shadowing is off for the rest of the UTC day, and the report has stopped growing | **Do not read the report as complete.** Raise `max_cost_usd_per_day` or lower `sample_rate` |
| Differences found | `increase(dorang_shadow_with_diffs_total[1h]) > 0` | A structural divergence from the reference gateway | Read the JSONL report. [MIGRATION.md](MIGRATION.md) §4.5 |
| Inconclusive | `increase(dorang_shadow_inconclusive_total[1h]) > 0` | A comparison could not decide a dimension. **Not evidence of sameness** | The record names the dimension and the reason |
| Work dropped | `increase(dorang_shadow_dropped_total[1h]) > 0` | The shadow queue was full and work was discarded. Coverage has a hole you cannot see from the report | Raise `queue_size` or `workers`, or lower `sample_rate` |
| Report truncated | `increase(dorang_shadow_report_dropped_total[1h]) > 0` | The report hit its byte cap. **A truncated report read as an empty one is the worst outcome this mechanism has** | Raise `report.max_bytes`, rotate the file, and re-run |

### 7.4 Metering degradation

The meter tracks six degradation reasons — `trace_queue_full`, `spool_full`, `spool_error`,
`sink_error`, `meter_closed` and `none` — with hysteresis, so a steady drop rate cannot flap the
signal. Five of them are about the **droppable** trace half of §12.1; `meter_closed` is the one
that is not, and it is the only way the numeric path can lose a count. It is
readable in two places:

| Where | What |
|---|---|
| `dorang_metering_degraded` / `dorang_metering_degraded_reason{reason=…}` | the gauge and its state set. Sampling and the daily byte budget never set them: those are policy, and conflating policy with failure makes the signal useless on any deployment that samples |
| `GET /health` → `"metering"` | `{"degraded":…,"reason":…,"dropped":…,"spool_bytes":…}`, present on every response rather than only when degraded — otherwise "not degraded" and "this build does not report it" look the same |

| Alert | Expression | Why |
|---|---|---|
| Metering degraded | `dorang_metering_degraded == 1` | Trace payloads are being lost or failing to ship. Numeric accounting is unaffected (§12.1), so the bill is still right and the traces behind it are not |
| Counts lost at shutdown | `increase(dorang_meter_records_refused_closed_total[1h]) > 0` | Requests finished **after the meter closed** and were refused — the one condition under which the numeric path loses a count, so the bill for those requests is missing rather than approximate. It raises `dorang_metering_degraded_reason{reason="meter_closed"}`. The window is shutdown, where the drain races the meter's own close: lengthen `server.shutdown_grace` past your p99 request, and check that the drain is not being cut short by a second SIGTERM |

It never changes the health status code. Losing trace payloads is a data-quality failure, not a
serving failure, and taking the pod out of rotation for it would turn a metering incident into
an outage.

### 7.5 The credential-lookup bound

| Alert | Expression | What it means | Do |
|---|---|---|---|
| Credential lookups throttled | `rate(dorang_auth_lookup_throttled_total[5m]) > 0` | Requests were refused `503 auth_unavailable` **without the store being consulted**, because the unknown-key budget (`auth.miss_budget`) was empty. An unknown index key carries no identity, so dorang cannot tell an attacker's first miss from a legitimate one and does not pretend to: the refusal is a retryable 503, not a 401 | Read it beside `dorang_auth_store_calls_total`. **This rising while that flattens is the bound holding** — a miss storm is costing the database nothing, which is the whole point. Sustained and with no attack, the budget is below the deployment's real rate of lookups that find nothing: a client retrying a revoked key, or a cold fleet reading every key from the store. Raise `auth.miss_budget.rate`, or `.burst` if it is a start-up spike |

---

## 8. Backup and restore

### 8.1 What has to be backed up

| Item | Where | Loss means |
|---|---|---|
| The store | `storage.sqlite.path`, or PostgreSQL | Issued keys, budgets and spend, the ledger, batch state, response-store rows |
| The key pepper | `$DORANG_KEY_PEPPER`, **or a file beside the SQLite database** | Every issued key becomes unverifiable. There is no recovery — the tokens are not stored |
| The master key | `$DORANG_MASTER_KEY` | Administrative access |
| The configuration | `--config` path | Reconstructable, but slowly |
| The price catalog | `pricing.catalog` | Historical figures stay in the ledger; new requests become UNPRICED |
| Model catalog overlays | `$DORANG_CATALOG_PATH` layers | Context windows and capabilities fall back to embedded defaults, which is the "too large" direction §4.3 warns about |
| Provider secrets | wherever `key_env` / `key_file` point | Everything stops |

The spool (`metering.spool.dir`) is deliberately **not** on this list. It holds trace payloads in
transit; losing it costs excerpts, not accounting. Numeric accounting never travels through it.

### 8.2 SQLite

```
systemctl stop dorang
sqlite3 /var/lib/dorang/dorang.db ".backup '/backup/dorang-$(date -u +%FT%TZ).db'"
cp /var/lib/dorang/dorang.db.pepper /backup/     # if the pepper was generated
systemctl start dorang
```

`.backup` is safe on a live database, but stopping first also freezes the pepper file and the
spool at a consistent point. **Back up the database and the pepper together** — they are one
artefact.

### 8.3 PostgreSQL

```
pg_dump --format=custom --no-owner "$DORANG_DATABASE_URL" > dorang-$(date -u +%F).dump
```

The ledger is the large table and is daily-partitioned. If dump time is a problem, exclude
`request_logs` and `request_traces` and accept that a restore loses history but keeps keys,
budgets, spend state and batch state — which is what makes the gateway serve.

### 8.4 Restore

```
systemctl stop dorang
# restore the database, then:
dorangctl migrate --config /etc/dorang/config.yaml
dorangctl config lint /etc/dorang/config.yaml
systemctl start dorang
dorangctl key list --config /etc/dorang/config.yaml | head
```

Restore with the **same pepper**. A key list that shows rows while every request answers 401 is
the signature of a pepper mismatch.

⚠️ **Durable state should be validated on restore, not adopted.** A persisted
`credential_state.unavailable_until` with no structural upper bound is a stored outage: restore a
month-old backup and a credential can come back marked unavailable until a date that has no
relationship to anything. Check `credential_state` after restoring an old backup, and prefer
resetting a cooldown to trusting a timestamp from another era.

---

## 9. Upgrade

The schema is versioned and migrations are embedded and forward-only. There is no downgrade
path, so the rollback plan is a database restore.

**Single node:**

```
dorangctl config lint /etc/dorang/config.yaml         # with the NEW dorangctl
dorang --config /etc/dorang/config.yaml --check       # with the new dorang
cp dorang /usr/local/bin/dorang.new && mv /usr/local/bin/dorang.new /usr/local/bin/dorang
systemctl restart dorang
```

**Fleet, rolling:**

1. Back up (§8) and note the schema version: `dorangctl migrate` reports it.
2. Apply migrations **once**, from one node, before rolling: `dorangctl migrate`.
3. Roll nodes one at a time. Each drains (§10) before it stops.
4. Watch `dorang_ready`, `dorang_responses_total{class="5xx"}` and `dorang_auth_failures_total`
   through the roll.

Two ordering rules:

- **Migrate before rolling, not during.** Two binaries at different schema versions writing the
  same ledger is a schema race, and the partition writer's in-line recovery is designed for a
  missing partition, not for a missing column.
- **A configuration change and a binary change should not be the same step.** If the new binary
  refuses the old configuration, you want to know which of the two caused it.

`--check` with the new binary against the old configuration is the cheapest pre-flight there is,
and it catches the case that actually happens: a key the new schema no longer accepts, or one it
now requires.

---

## 10. Draining a node

`SIGTERM` or `SIGINT` starts a drain. The sequence:

1. **Readiness flips to `503` immediately.** `dorang_ready` goes to 0. Liveness stays `200`.
2. **The process keeps serving for `server.pre_stop_delay` (default 10s).** The listener stays
   open and requests are answered in full — this is the window in which your load balancer
   notices the readiness failure and stops routing here. Nothing else changes during it.
3. The listener closes, so new connections are refused at TCP level.
4. In-flight requests **run to completion and get their normal status**. No 503 is injected into
   a request already being served.
5. When they finish, or when `server.shutdown_grace` expires, every request still running is
   cancelled and told why. A **stream ends with an in-band error frame** carrying code
   `gateway_shutting_down` followed by `data: [DONE]`, not a TCP reset — so a client can tell
   "the gateway is restarting, retry" from "something broke". Each cancelled request is still
   metered before the connection closes.
6. The process then tears down in reverse order of construction, bounded by the same grace:
   the shadow worker pool, the meter (which drains to disk and makes one final flush attempt),
   then the store. **Unspent budget lease blocks are returned**, so a planned restart is exact
   — the durable counter ends up holding precisely what was spent. A crash skips this, and that
   difference is the whole of the published overshoot.

If the grace expires with work still in flight, the process exits **1** with:

```
dorang: drain grace expired with requests still in flight
```

That is a real signal, not noise: something was still running after the window you configured.

**Sizing the pre-stop delay.** This is the one number you must compute from your own
infrastructure, because it describes your *balancer*, not dorang. Every balancer discovers
unreadiness by polling, and if the listener closes before the poll that notices it, the balancer
is still routing to a socket that no longer accepts — connection-refused on every rolling
restart, which is the exact failure the drain exists to prevent.

```
pre_stop_delay  >=  probe period × failure threshold
                  + probe timeout
                  + however long the balancer takes to withdraw the endpoint
```

`deploy/kubernetes.yaml` ships `periodSeconds: 2`, `failureThreshold: 2`, `timeoutSeconds: 1` —
five seconds of detection — against the default `pre_stop_delay: 10s`. **Change a probe and you
must change the config with it.** No `preStop` hook is needed or wanted: the wait happens inside
the process, and the shipped image is distroless, so there is no `/bin/sleep` to exec.

Set `pre_stop_delay: 0` where nothing is routing to the process — a single node, a workstation.
An *absent* key takes the default, so writing `0` really does mean none. A second `SIGTERM` also
skips the wait, so `Ctrl-C` never appears to hang.

```yaml
terminationGracePeriodSeconds: 80       # pre_stop_delay + 2 × shutdown_grace + 10s
```

Sizing rule: `terminationGracePeriodSeconds > server.pre_stop_delay + 2 × server.shutdown_grace`,
because the drain and the teardown each get the full grace and the pre-stop delay comes on top of
both. Below it, `SIGTERM` becomes `SIGKILL` part way through the drain — which skips returning
the unspent quota blocks and makes the restart inexact.

**A single node cannot restart without a gap, and this is accepted.** dorang has no socket
handoff and no `SO_REUSEPORT`: between the old process closing its listener and the new one
binding, there is nothing listening, and a client that connects in that window is refused. Two
nodes behind a balancer remove it entirely, which is what `deploy/kubernetes.yaml` ships and what
requirement R14 asks for. **On the notebook and single-node tiers, plan for a sub-second outage
on every restart** — it is not something a longer grace or a larger `pre_stop_delay` can close.

**In a cluster**, the leader's work — rollup compaction, partition maintenance, capacity and
budget reservation sweeps, batch assignment, lease rebalancing — moves on election when the
leader leaves. Leases and reservations are released as part of the drain. A node with a stable
`cluster.node_id` reclaims its own leases on restart instead of waiting for them to expire, which
is why §CONFIG 4 covers setting it.

A stable id has to be a **distinct** id. Two processes under one `cluster.node_id` are one node to
the registry, to the leadership lease and to every leased limit, and every leader-only job then
runs on both — including batch assignment, where §DESIGN 9.2 records that two nodes picking up the
same batch pays for every finished row twice. The second process refuses to start:

```
cluster: node id dorang-0 is in use by another process; this node will not join, lead or run any
leader job.
```

If you see that line, two pods, hosts or containers are carrying the same id — a StatefulSet
template that did not substitute, an id baked into an image, a configuration file copied to a
second host. Fix the id; do not restart into it. The one benign case is an **unclean** restart
under a configured id within the 30-second heartbeat TTL, which clears itself once the previous
row lapses. If you cannot guarantee uniqueness, leave `cluster.node_id` empty: the derived
per-process id cannot collide, and it costs only that lease TTL on restart.

---

## 11. Troubleshooting

These are the failure modes the design already documents, which makes them the ones that will
actually happen. Each is hard to diagnose for the same reason: **nothing returns an error.**

### 11.1 A backend returns 200 while ignoring what was asked

**The shape.** Both self-hosted engines accept a request, ignore a field they do not have
configured, and answer `200`. Nothing in the response says so. This is the single largest class
of integration risk with vLLM and SGLang, and it is why their operator profiles list *what
silently breaks* rather than *what the flag does*.

| Observation | Likely cause | Confirm | Fix |
|---|---|---|---|
| `priority` has no scheduling effect | Default scheduler policy on either engine | vLLM: not detectable from the response at all — the only endpoint exposing the policy is development-mode. SGLang: set `--abort-on-priority-when-disabled` and one throwaway request returns 503 with a stable message | vLLM `--scheduling-policy priority`; SGLang `--enable-priority-scheduling` |
| Batch traffic outranks realtime on SGLang | The two engines order priority in **opposite directions** and both return 200 | Compare the emitted wire value against the engine's direction | `--schedule-low-priority-values-first`, or set the emit direction per provider |
| Tool calls arrive as plain text, `finish_reason: "stop"`, `tool_calls: null` | SGLang without `--tool-call-parser`. The tools are rendered into the prompt and the model's native syntax lands in `content` | The content contains the model's tool syntax verbatim | `--tool-call-parser <p>` |
| `tools` present, client gets content and no error, on vLLM | `tool_choice: null` sent alongside `tools`. An explicit null passes validation and then takes a branch that never invokes the parser | Look for a literal `null` in the request body | dorang normalizes a literal `null` to `"auto"`; if you are bypassing that via passthrough, do not send it |
| `reasoning` / `reasoning_content` always null | `--reasoning-parser` unset on either engine | `<think>` tags stay inline in `content` | Set the parser. Note the field name differs: vLLM `reasoning`, SGLang `reasoning_content` |
| A request that should not have fit returned 200 with a normal-looking answer | The backend silently truncated. Some clamp oversized input; at least one truncates and returns a `length` stop with **zero output tokens** | Compare reported `input_tokens` against the declared window. Reported input above the window is the signature | Never enable the backend's own auto-truncation. `finish_reason: "length"` with zero output is an overflow, not a completion |
| A quota error arrives as HTTP 200 | Some vendors return an error envelope inside a success status | The body carries a vendor status field rather than an error object | dorang classifies failures by HTTP status; a quota-exhausted `200` is neither retried nor recorded as an outage, and is **priced as a successful request** |

**The general rule.** A `200` is not proof the request was served as asked. Where you can
cross-check a number dorang already has — reported input tokens against the declared window,
`cached_tokens` against whether the flag is set — do; where you cannot, the operator declaration
in the provider configuration is the only source of truth, and dorang flags an unverified
capability rather than assuming.

### 11.2 A metric reads zero because a flag is unset, not because the system is idle

**The shape.** A load signal reads its most attractive possible value when it is unconfigured,
and a least-busy router believes it.

| Signal | Zero means | How to tell |
|---|---|---|
| vLLM `/metrics` returns 200 with **no `vllm:` series** | `--disable-log-stats` is set | The endpoint is reachable and the body has no `vllm:` lines. "Endpoint reachable, no series" and "idle" look identical and mean opposite things |
| vLLM `/load` returns `{"server_load": 0}` **forever** | `--enable-server-load-tracking` unset | Unconfigured `0` is indistinguishable from genuinely idle — while being the most attractive value to a least-busy router. **Do not use `/load` without independently confirming the flag** |
| SGLang `sglang:cache_hit_rate` is 0 most of the time | Not a flag — the gauge is hard-reset to `0.0` on every decode report | Compute the rate from `sglang:cached_tokens_total` and `sglang:prompt_tokens_total` instead |
| SGLang `sglang:utilization` is 0 or **`-1`** | Its input has no setter; `-1` in PD-prefill mode | A negative utilization passes any naive threshold check |
| SGLang `/v1/loads` returns all-zero fields | A zero snapshot is written at writer construction, before any forward pass | **Treat `max_total_num_tokens == 0` as "not ready", not "idle."** An empty `{"loads": []}` with HTTP 200 means the shared-memory attach failed, i.e. broken rather than idle |
| SGLang `/metrics` **404s** | `--enable-metrics` unset | This is the honest failure and the better one: dorang can distinguish it from the status code alone |

**Inside dorang, the same rule applies to routing inputs.** Three signals can be absent, and each
has a direction in which absence silently *wins*:

| Signal | Absent means | The trap |
|---|---|---|
| latency sample | unproven | zero is the **fastest** value — an unproven backend wins on ignorance |
| throughput sample | unproven | zero is the **slowest** — it loses forever and never earns the sample that would let it compete |
| price | unpriced | zero is the **cheapest** — the deployment nobody priced wins every group, permanently and silently |

All three resolve as **no opinion**: the candidate is skipped by that comparator, ranked by the
next one in the chain, and a counter records it. If routing looks wrong and no error has been
raised, ask which comparator had no data rather than which one is broken.

### 11.3 A quota reading is stale during a burst

**The shape.** A provider-reported quota poll is up to one `usage_probe.interval` old. During a
burst, the local counter is the fresher signal, and the provider figure is the more complete one.
dorang combines them:

```
effective_used = max( provider_reported_used ,
                      provider_reported_used_at_last_poll + local_delta_since_that_poll )
```

so a lagging poll cannot erase a burst. If you are diagnosing a quota decision:

1. **A failed fetch never disables a credential.** A failed read is not an exhausted quota. The
   last good snapshot is retained and its staleness is exposed.
2. Reported percentages come from the last poll plus the local delta, so a figure that looks
   behind is expected between polls. Shorten `usage_probe.interval` if the gap matters — it is
   also the floor on how often one credential is read, so a second poll inside it replays the
   last snapshot rather than spending a request.
2a. ⚠️ **If a probe appears to change nothing, check these two before anything else.** The
   provider must report in the metric's units, and most report a percentage of an allowance
   whose size they do not publish — such a window carries its RESET INSTANT only until you
   declare the size with `usage_probe.allowances[].limit` ([CONFIG.md](CONFIG.md) §6.1a). And
   the credential must have a rule for the figure to gate: quota is keyed by (window, metric),
   and the rule comes from `models[].deployments[].limits[]`. A probe enabled for a credential
   with no rule is logged at start-up saying exactly that.
3. `x-dorang-quota-<window>-used-pct` on a response (with `X-Dorang-Detail: full`) is what the
   router saw for that request.
4. ⚠️ **Rolling windows are exact only under `capacity_mode: local`.** A rolling 5-hour allowance
   has no natural period boundary, so shared and leased modes key it to an epoch-aligned grid.
   A "5 hours" that resets on a grid is a real approximation.
5. ⚠️ **Some vendors report *remaining* quota through an endpoint whose field names say
   *consumed*.** Combining a remaining value as if it were a used value inverts the sign. Check
   the fetcher against the vendor's actual semantics before trusting a percentage.

### 11.4 A budget appears exhausted after a restart

**The shape.** Budget is the one write that cannot be deferred — deferring the reservation means
two concurrent requests both see the pre-spend balance, which is the exact thing
reserve-before-spend exists to prevent. So it is synchronous, but made cheap: the hot path
touches a per-node in-memory reservation guarded by an atomic, and durability comes from a
**lease block** the node already holds. The store sees a write per block, not per request —
measured at 400 requests to 5 store writes.

A **graceful** stop returns the unspent part of every block, so a planned restart is exact. A
**crash** does not, and the unreturned remainder is charged. That is the published overshoot, and
it is bounded by the block size: `DefaultBudgetBlockNanoUSD` is $0.05 per block.

So, when a budget reads lower than expected after a restart:

| Cause | Signature | Do |
|---|---|---|
| Ungraceful stop | The gap is at most one block per node, i.e. ≤ $0.05 × nodes | Nothing. It is bounded and it is charged in the safe direction — a crash can only **under**-spend the allowance, never over |
| Contention reported as exhaustion | A terminal `400` against a limit that is 98% free | This was a real defect and is fixed; if you see it on an old build, upgrade |
| A different key than you think | Budgets attach to a key, and a key's budget period matters | `dorangctl key list` shows spend and budget per key |
| Budget refusal read as a rate limit | The response is a **terminal `400`** with code `budget_exceeded`, deliberately not a `429` | Correct behaviour. A `429` would invite a retry *and* mark the condition as a fallback trigger, which would spend a **different subject's** budget on a model the caller never asked for |

Two things that are **not** causes any more, and were:

- Budget state used to be in-memory only, so a restart silently started the period over. It is
  now reserved against the durable ledger and a restart re-reads the counter.
- Budget enforcement was once specified, implemented, tested, and **wired to nothing**: the
  request path checked spend against a column that nothing on that path incremented, so a budget
  could never be exceeded because it was never consulted. Every unit test passed and the
  requirement was not met. If a budget is not being enforced on any build, that shape — an
  interface satisfied on both ends and connected on neither — is the first thing to check.

### 11.5 Other things worth knowing before you page someone

| Symptom | Explanation |
|---|---|
| Every key fails authentication after a move or a restore | Pepper mismatch (§2.3, §8.4). Key rows exist; the digests were computed under a different pepper |
| A revoked key still works | Credentials are cached. It clears within the entry TTL (60 s) or on `SIGHUP` — see §3.1 |
| A reload "did nothing" | It probably succeeded and rebuilt only what is rebuildable. Storage, the authenticator, the meter and the capacity broker are **not** replaced on reload — that needs a restart |
| A shadow configuration change is ignored | Correct. `shadow:` is the one section that refuses to hot-reload, because rebuilding it re-arms the daily cost ceiling |
| A passthrough prefix 501s | Its provider has no `base_url`, so the route was dropped at build time rather than pointed at nothing |
| A model routes to the wrong deployment and the decision headers look right | Historically this exact shape existed: pinned and quota-filtered candidate lists were cut from one shared buffer, so every candidate carried the last one's provider, and reservations landed on another deployment's axes **while the decision reported the correct identity**. No assertion about a decision can observe that. If you see it, capture `X-Dorang-Deployment` *and* the capacity snapshot together |
| A multi-axis request waits forever while both its axes stay busy | Risk W8, open. A waiter needing two saturated axes can sit at the head of both queues and never find them free at the same instant. Aging cannot close it — the waiter is never overtaken, it simply never wins. Widen one of the two axes |

---

## 12. Designed, not in this build

Operationally relevant gaps, so you do not plan around something that is not there.

| Area | Status |
|---|---|
| **HTTP administration** | Mounted (§3.1–3.3). The credential lifecycle including rotation and pend, `/spend/logs`, capacity, catalog, health history and `/ui` are served; users, teams, deployments, budgets and the aggregate spend reports answer `501 dependency_not_configured` because `internal/store` has no code for those tables. Use `dorangctl` for those |
| **Audit trail readback** | `audit_logs` is written by every administrative mutation and `/audit/list` is a named 501. Query the table directly |
| `observability.otlp_endpoint` | No exporter is wired. The latency breakdown is recorded and not exported |
| `capacity.*.rpm`, `.tpm` | **Refused at load**, naming the working home: `deployments[].limits[]` for a per-deployment rate, the api key's own `rpm_limit`/`tpm_limit` for a per-caller one |
| **Lua** | Lua **runs**, sandboxed, on the request path — the VM is gopher-lua, pure Go. Plugins are declared one at a time as `filters.plugins[].path` and never scanned for; `extensions.lua.dir` holds `*.policy`, the total policy language, and a `.lua` file **under that directory** is still a **load error** rather than a file silently ignored, because a directory that runs whatever appears in it is a code-execution primitive. A compiled-in Go `Native` is the third way. See [CONFIG.md](CONFIG.md) §16 and §17. ⚠️ **This row read "there is no Lua interpreter" until 2026-07-29** — false since `a0d5871`, and it is the kind of gap an operator plans around, which is exactly what this table exists to prevent |
| `on_route` re-routing | The hook sees the chosen deployment and may refuse it; it cannot ask for a different one |
| Top-level `quotas:` and `budget:` blocks | Not in the schema. Budgets are per-key via `dorangctl key create --budget-usd` |
| Credential import from an incumbent database | `dorangctl import keys --from <dsn>`, reporting by default and writing with `--commit` — see [MIGRATION.md](MIGRATION.md) §3.5. ⚠️ **This row read "has no CLI entry point" until 2026-07-29**, and it was true: the importer was complete in the store and reachable from nothing an operator could type |
| Rate limiting across nodes | The rolling minute is per process. An N-node deployment enforces N times every `rpm_limit` and `tpm_limit`. The durable ledger would close it, at a store write per request — not taken |
| `tpm_limit` bounds the NEXT request | A token count does not exist until settlement, so one enormous request can cross the ceiling once before anything refuses |
| Prefix / cluster metrics | State exists; nothing exports it. Capacity and health are readable at `/admin/capacity` and `/health/history` |
| `providers[].params.drop*`, `routing.prefix.checkpoints`, `deployments[].stream_timeout`, `key_rotation.…affinity_group`/`…stickiness.scope`, `cluster.redis_url_env`, `observability.log_level`/`.log_format` | Load and do nothing. ⚠️ **`providers[].usage_probe` left this row on 2026-07-29** — it is wired (§6.2), and `providers[].metrics` left it too, in the other direction: it is refused at load. The list is held as executable state in `internal/config/consumed_test.go` for the field names that guard can see; the rest are prose in [CONFIG.md](CONFIG.md) §23.1, which is why that half has to be re-derived by hand rather than trusted |
| **Backend metrics scraping** | There is no scraper, and `providers[].metrics` is now **refused at load** rather than accepted and ignored. §12.4's queue-depth and cache-utilization signals have no collector. This costs you nothing unless you are running a self-hosted backend that also serves traffic from outside dorang: `least_busy` ranks on dorang's own live capacity occupancy and `highest_tps` on measured output tokens per second, both without a poll — name them in `models[].strategy`. See [CONFIG.md](CONFIG.md) §6.2. ⚠️ The gap was previously understated as "the poll interval is not read" |

---

## See also

- [CONFIG.md](CONFIG.md) — every configuration key, its default, and what breaks if it is wrong.
- [MIGRATION.md](MIGRATION.md) — moving from an incumbent gateway.
- [COMPATIBILITY.md](COMPATIBILITY.md) — the wire contracts a client observes, and the error
  taxonomy.
- [VLLM.md](VLLM.md) §5, [SGLANG.md](SGLANG.md) §8 — the flags a self-hosted backend needs.
- [DESIGN.md](DESIGN.md) §18 — the open risks, with W8 the one an operator can hit.
