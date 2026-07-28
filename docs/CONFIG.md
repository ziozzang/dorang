# Configuration Reference

> Every key the configuration schema accepts: its type, its default, what it does, and
> **what breaks when it is wrong**. Section order follows [DESIGN.md](DESIGN.md) §4.2.
>
> This describes the configuration **this build reads**, not the one the design describes.
> Where the design specifies a key that the schema does not accept, or the schema accepts a
> key that nothing acts on, §23 says so by name. Reading §23 before §24 will save you an
> afternoon.
>
> A working file to copy from: [`deploy/config.example.yaml`](../deploy/config.example.yaml).
> A test loads it verbatim, so it cannot drift from the validator.
>
> **Verified against the working tree on 2026-07-28**, by loading each configuration through
> `dorangctl config lint` rather than by reading the validator. The implementation is moving:
> **a claim of absence is the claim most likely to have expired**, so re-check §23 against the
> build you are running before planning around anything in it.
>
> 한국어: [CONFIG.ko.md](CONFIG.ko.md)

---

## 0. How the file is read

### 0.1 One file, decoded strictly

`version: 1` is the only schema version this build understands. Anything else is refused with
the version it found and the version it wanted.

The decode is strict in three ways, and each exists because the alternative fails silently:

| Rule | Consequence of the alternative |
|---|---|
| **An unknown key is an error**, not an ignored line | A typo is otherwise invisible until the behavior it was meant to change fails to happen |
| **Exactly one YAML document** per file | A second document after `---` would be silently discarded |
| **Every problem is reported at once**, each with its YAML path | A file with nine defects should take one round trip to fix, not nine |

The nine-defect number is not hypothetical: the validator found nine dangling references —
providers, credentials, capacity groups and models that were named and not declared — in the
design's own example configuration the first time it was run against it.

A **missing** file is not always an error. With neither `--config` nor `$DORANG_CONFIG` set,
the server looks for `dorang.yaml`, and if it is not there it starts on documented defaults:
SQLite, no upstreams, listening on `:4100`. A file named explicitly and then not found **is**
an error, because that is a typo rather than a choice.

### 0.2 Scalar forms

| Form | Accepted | Notes |
|---|---|---|
| Duration | `250ms`, `30s`, `1h`, `2h45m` | A **bare number is seconds** — `timeout: 180` is 180s. Negative is refused |
| Size | `4096`, `64MiB`, `8GiB`, `512KB` | `KiB/MiB/GiB/TiB/PiB` are powers of 1024; `KB/MB/GB/TB/PB` are powers of 1000. Case-insensitive |
| Decimal | `"0.0000025"`, `"20.00"`, `"5"` | Held as **text** and parsed exactly. Prices never pass through binary floating point (§8.3). Quote them — YAML would otherwise hand you a float |
| Path | `~/.dorang/dorang.db` | A leading `~` expands to the process user's home directory |

### 0.3 Secrets

A secret value never appears in this file. Every key material field is a reference, and
**exactly one** of these four may be set:

```yaml
key_env:  NAME             # read from the process environment
key_file: /path/to/key     # read from a file; a trailing newline is trimmed
key:      literal          # accepted ONLY when server.env is "development"
```

Setting none is an error. Setting two is an error. A `key_env` that names an unset or empty
variable is an error, and so is an unreadable or empty `key_file`.

`key_ref` is **refused**. It used to load and validate and leave the credential with no usable
secret: the request then went upstream with no `Authorization` header at all, and the failure
arrived as a `401` from the provider with nothing in the file to explain it. There is no
resolver in this build, so the key is a load error naming `key_env` and `key_file` instead — a
vault agent that writes a file or exports a variable satisfies both.

A resolved secret is unreachable from every rendering path — `String`, `%v`, `%#v`, YAML
marshal, JSON marshal and every error this package produces return the redacted *source*
(`key_env:PLAN_A_KEY`), never the value. An inline literal is moved out of its field during
resolution so it cannot be re-marshalled back into a file or a log line.

### 0.4 Defaults

Every default in this document is applied at load, is idempotent, and never overwrites a value
the file set. Fields where an explicit `false` or `0` differs from "unset" are carried as
pointers for exactly that reason: `metering.numeric.enabled: false` is a refusal, not an
absence, and the validator treats it as one.

The shortest working file is `version: 1` plus `providers`, `credentials` and `models`.

### 0.5 Hot reload — and the one section that does not

The configuration reloads on a change to the file, on `SIGHUP`, and through the admin reload
endpoint. The file is stat'ed every 2 seconds; a rename-into-place — which is how most editors
and configuration-management tools write — changes identity as well as timestamp and is
detected.

Three properties worth relying on:

- **A failed reload changes nothing.** The file is re-read, defaulted, resolved and validated,
  and only then swapped. A broken edit degrades to "no change" plus a logged error, never to a
  partial apply. This is asserted by pointer identity, not by absence of an error.
- **In-flight requests keep the snapshot they started with.** Readers take an atomic pointer
  and no lock.
- **Not everything is rebuilt.** The router, the price catalog and the upstream table are
  rebuilt and swapped. The store, the authenticator, the meter and the **capacity broker are
  not** — swapping a broker would drop live reservations, and swapping a store would drop the
  connection pool under an in-flight ledger write. A change to `storage.*` or to a capacity
  ceiling therefore needs a restart, not a `SIGHUP`.

⚠️ **`shadow:` does not hot-reload, deliberately.** Its daily cost ceiling, its sampled set and
its report handle are per-process state. Rebuilding them re-arms the ceiling, which turns
"$5 per day" into "$5 per `SIGHUP`". A changed `shadow` section is **refused**, and the running
configuration is kept.

### 0.6 Checking a file before it serves anything

```
dorang --config /etc/dorang/config.yaml --check
dorangctl config lint /etc/dorang/config.yaml
dorangctl config lint --catalog /etc/dorang/models.d /etc/dorang/config.yaml
```

Both run the same validator the server runs. Under `--check`, a secret that cannot be **read
on this machine** is downgraded to a warning — a configuration is routinely linted where the
deployment's key material is not — while everything else stays an error, including an inline
literal outside development. `dorangctl config lint` additionally lints the model catalog
layers, including whatever `$DORANG_CATALOG_PATH` names, so it lints what a server would load
rather than only what was typed.

`--check` is not a complete pre-flight. §23.3 lists the errors that survive it.

---

## 1. The three configurations that refuse to start

The design names three. Each is a hard refusal rather than a warning, and the reasoning is
worth more than the rule — an operator who understands why will not spend a day working around
it.

### 1.1 Clustering with local capacity accounting

```yaml
cluster:
  enabled: true
  capacity_mode: local      # refuses to start
```

**The rule.** `cluster.enabled: true` with `capacity_mode: local` refuses to start. Use
`shared-redis`, `shared-pg` or `leased`.

**Why it is not a recommendation.** Node-local counting is *exact* on one node. On N nodes
every ceiling is counted N times over, so a provider ceiling of 7 admits up to 7N concurrent
requests. That much is obvious. What is not obvious is where the damage lands.

The provider answers the excess with `429`. A `429` is classified as `rate_limit`, whose
default fallback chain is `[same_group, same_class]` (§12). So the overshoot does not simply
produce errors on the model that overshot — it **spends the capacity of every other model in
the same class**, on requests that were never routed there. A saturated coding plan degrades a
chat model nobody touched, three hops away, and the metric that moved is on the wrong
deployment. The failure surfaces far from its cause, which is the property that makes it
expensive to diagnose rather than merely wrong.

Revision 1 of the design called this a recommendation. It is not, and the message the validator
prints says so at length rather than referring you here.

**Every mode publishes its maximum overshoot as a number.** "Approximately accurate" is not an
acceptable specification:

| Mode | Maximum overshoot | Hot-path cost |
|---|---|---|
| `local` | `limit × (nodes − 1)` | none |
| `shared-redis` | **0** | +1 RTT |
| `shared-pg` | **0** | +1 RTT, higher than Redis |
| `leased` | `block × (nodes − 1)` | none on the steady path |

The shared modes are asserted to admit *exactly* the limit, not merely at most it: a mode that
under-admits is safe rather than exact, and would leave an operator short of capacity they
paid for.

### 1.2 An inline literal secret outside development

```yaml
server:
  env: production
credentials:
  - {id: acct-1, provider: cloud-a, key: sk-literal}   # refuses to start
```

**The rule.** `key:` is accepted only when `server.env: development`.

**Why.** A configuration file is the one artefact that gets committed, copied into a ticket,
pasted into a chat window and rendered by a templating system. Every one of those is a
disclosure path that a `key_env` reference does not have. The rule is not that literals are
insecure in themselves; it is that a file which *may* contain a secret has to be handled as
though it does, forever, by everyone who touches it — and that discipline does not survive
contact with an incident at 3 a.m.

`development` exists so a laptop is not made unusable by the rule. It is not a security
posture: it also relaxes nothing else, so switching `env` to work around a production
deployment's key handling buys exactly one thing and costs the audit trail.

### 1.3 Legacy key hashing with no end date

```yaml
auth:
  legacy:
    enabled: true
    until: ""            # refuses to start
```

**The rule.** `auth.legacy.enabled: true` requires `auth.legacy.until: <date>`, and the date
must still be in the future. `2026-12-31`, `2026-12-31T00:00:00` and a full RFC 3339 timestamp
are all accepted. A window that has already closed refuses to start with the instant it closed.

**Why.** `legacy_sha256` is an unsalted single-round digest — the scheme the common incumbent
gateway stores. Accepting it lets an existing deployment's keys keep working during a
migration, which is worth real money in avoided coordination. Accepting it *permanently* means
dorang's key database is offline-attackable forever, in exchange for never finishing a
migration that takes an afternoon.

The number that settled it: a verification against a real deployment found that the large
majority of stored credentials were **already expired**. Adopting a weak digest permanently to
avoid reissuing a handful of live keys is a bad trade, and it only looked like a good one
because the original estimate counted rows without checking whether they were alive.

So legacy verification is a **window**, and a window has an end. `auth.rehash_on_use` (default
on) upgrades each legacy hash to `dorang_v1` the first time it verifies, so the window closes
itself without a flag day and without downtime.

### 1.4 Configurations that fail later, not at load

These are not refusals to start in the validator's sense; they fail at assembly or at request
time, so `--check` passes and the server does not come up (or comes up doing nothing). They
are listed here because operationally they behave like refusals.

| Configuration | When it fails | What you see |
|---|---|---|
| `pricing.rules[].rates.images` | **Assembly.** Lint passes | `pricing rule X: the images component is not priced by this build` |
| `pricing.rules[].rates.images` | **Load.** `internal/pricing` has no per-image unit | `the images component is not priced by this build` — price the request instead |
| `shadow.mode` on with no `sample_rate` | Never. It runs and shadows **nothing** | Gate verdict `no_data`, empty report, and an empty report read as proof (§18) |
| `storage.driver: postgres` with the URL variable unset | **Assembly.** Lint only checks that the variable is *named* | Store open fails at start-up |

---

## 2. `server`

```yaml
server:
  listen: ":4100"
  env: production
  master_key_env: DORANG_MASTER_KEY
  key_pepper_env: DORANG_KEY_PEPPER
  request_timeout: 600s
  shutdown_grace: 30s
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `listen` | string | `:4100` | The listen address, `host:port` | Empty is refused. `--listen` on the command line overrides it, which is how you run two instances from one file |
| `env` | `production` \| `development` | `production` | Selects whether an inline literal secret is accepted (§1.2) | `development` in production makes a committed key a valid configuration |
| `master_key_env` | string | `DORANG_MASTER_KEY` | Names the variable holding the administrative credential, compared out of band in constant time and **never stored as a row** | Empty is refused. If the variable itself is unset, the authenticator refuses to build unless an explicit opt-out is set — losing admin authentication has to be typed out, because a gateway that reads only an imported database loses it silently otherwise |
| `key_pepper_env` | string | `DORANG_KEY_PEPPER` | Names the variable holding the HMAC pepper for `dorang_v1` key hashing | Empty is refused. **If the variable is unset, a pepper is generated once and written beside the SQLite database.** That keeps the zero-dependency notebook tier working, but the pepper travels with the database file — a multi-node deployment that does not set the variable will find each node issuing keys the others cannot verify |
| `request_timeout` | duration | `600s` | The whole-request deadline. It also sets the capacity reservation's expiry (`request_timeout + 30s`), so a leaked goroutine cannot hold a slot forever | Zero or negative is refused. Too short truncates long generations; too long lets a wedged upstream hold a reservation for the whole window |
| `shutdown_grace` | duration | `30s` | How long a drain waits for in-flight requests, and separately how long the post-drain teardown may take | Negative is refused. Shorter than your p99 request turns a rolling restart into visible errors: the process hard-closes and returns exit code 1 with `drain grace expired with requests still in flight` |

---

## 3. `storage`

```yaml
storage:
  driver: sqlite
  sqlite:   {path: ~/.dorang/dorang.db}
  postgres: {url_env: DORANG_DATABASE_URL, max_conns: 32}
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `driver` | `sqlite` \| `postgres` | `sqlite` | Selects the ledger and control-plane store | Anything else is refused. The SQLite driver is pure Go, so the binary is static and the notebook tier needs nothing installed |
| `sqlite.path` | path | `~/.dorang/dorang.db` | The database file. Its parent directory is created with mode `0700` | Empty is refused when the driver is `sqlite`. This file also carries the generated key pepper (§2) and is where the ledger, budgets and issued keys live — losing it loses all four |
| `postgres.url_env` | string | `DORANG_DATABASE_URL` | Names the variable holding the connection URL | Empty is refused when the driver is `postgres`. The **variable's contents are not checked at load** — only that a name is given — so a wrong URL fails at start-up rather than at lint |
| `postgres.max_conns` | int | `32` | Connection pool ceiling | Zero or negative is refused. Too small serialises the ledger writer against the request path's budget reservations; too large exhausts the server's `max_connections` across a fleet |

Migrations are embedded and applied at start-up. SQLite and PostgreSQL share one schema
definition; only four things are dialect-specific — placeholder syntax, the migration lock,
partitioning, and missing-partition detection. Timestamps are integer microseconds in every
column, because SQLite has no timestamp type and one dialect had to give; integers are exact,
sort correctly, carry no timezone, and make daily partition bounds arithmetic.

There is deliberately **no default partition** on PostgreSQL. A default partition converts a
missing-partition failure from loud to silent, and attaching the real partition afterwards then
requires a full scan of everything that landed in the default. The writer pre-creates
partitions ahead and recovers in line when the server reports no partition for a row.

---

## 4. `cluster`

```yaml
cluster:
  enabled: false
  node_id: ""
  redis_url_env: DORANG_REDIS_URL
  capacity_mode: local
  min_leasable: 16
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `enabled` | bool | `false` | Turns on multi-node behaviour: leader election, leased coordination, and the §1.1 guard | Leaving it `false` on a fleet gives you N independent gateways each counting its own ceilings — the exact overshoot §1.1 refuses, arrived at by omission instead of by configuration |
| `node_id` | string | `""` | Names this process in its lease rows | Empty derives one **per process**. A restarted node then cannot reclaim its own leases and must wait for them to expire. Set it on every clustered node |
| `redis_url_env` | string | `DORANG_REDIS_URL` | Names the variable holding the Redis URL | Required to be non-empty when `capacity_mode: shared-redis` |
| `capacity_mode` | `local` \| `shared-redis` \| `shared-pg` \| `leased` | `local` | How capacity **and quota** are coordinated across nodes. One accuracy vocabulary, not two | See §1.1. `local` with `enabled: true` refuses to start |
| `min_leasable` | int | `16` | The smallest ceiling `leased` mode will divide across nodes | Zero or negative is refused. Any `max_concurrency` below this value, anywhere in the file, is rejected under `leased` — a single-digit limit cannot be usefully divided, and pretending it can produces a fleet where most nodes hold a block of zero |

Under `leased`, the validator checks `min_leasable` against `providers[].max_concurrency`,
`capacity.provider_groups[].max_concurrency`, `capacity.credential_groups[].max_concurrency`,
`capacity.models[].max_concurrency` and `key_rotation.providers[].keys[].max_concurrency`. Each
violation names the path and the value.

> **Rolling windows are exact only in `local` mode.** A rolling 5-hour allowance has no natural
> period boundary, so the shared and leased modes key it to an epoch-aligned grid. That is a
> real approximation and it is recorded rather than hidden; the design does not yet address it.

---

## 5. `auth`

```yaml
auth:
  legacy: {enabled: false, until: ""}
  rehash_on_use: true
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `legacy.enabled` | bool | `false` | Accepts the unsalted incumbent digest during a migration window | See §1.3 |
| `legacy.until` | date | `""` | The window's end. Required when `legacy.enabled` is true | An unparseable date, or one already past, refuses to start |
| `rehash_on_use` | bool | `true` | A successful legacy verification schedules an asynchronous upgrade to `dorang_v1` | Turning it off means the migration never completes on its own, and the window closes with keys that then stop working |

Both digests are always computed and the comparison target is chosen branchlessly, so the
scheme a key uses is not observable in timing. Authentication is 354 ns and zero-allocation on
the cached path; crypto dominates, and the snapshot lookup itself is 55 ns.

---

## 6. `providers`

One upstream service. `name` and `kind` are required.

```yaml
providers:
  - name: plan-a
    kind: glm
    base_url: https://api.example-plan-a.invalid
    timeout: 180s
    max_concurrency: 20
    capacity_group: shared-pool
    params:
      drop_unsupported: true
      drop: []
      set: {}
      default: {}
    retry: {max_attempts: 2, backoff: exponential, base: 500ms}
    usage_probe: {enabled: false, fetcher: glm, interval: 60s}
    metrics: {enabled: false, endpoint: http://127.0.0.1:8000/metrics, interval: 15s}
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `name` | string | — | The provider's identity. **Provider identity comes only from here and from `deployments[].upstream_model`, never from parsing a model string** | Empty or duplicate is refused. Everything that references a provider — credentials, capacity groups, deployments, passthrough routes, pricing matches — is checked against this name |
| `kind` | string | — | Selects, in one word: the wire adapter, capability defaults, the prompt-cache scheme and the reasoning-control shape. Resolved through the model catalog, so an overlay can add a kind this build has never heard of | Empty is refused. An unknown kind is **not** refused at load — it resolves through the catalog's layers, and `dorangctl catalog explain <kind> <model>` tells you which layer answered |
| `base_url` | string | `""` | The upstream root. **dorang appends the API version segment unless the URL you write already contains one** — see §6.0 | Empty means the provider cannot be dialled, and falls back to the one the `kind` declares. A passthrough route whose provider has no `base_url` is **silently dropped** rather than pointed at nothing |
| `timeout` | duration | `0` | Per-provider request timeout. A deployment may override it | Zero falls through to `server.request_timeout` |
| `max_concurrency` | int | `0` | The `route` capacity axis — this provider's own ceiling | Negative is refused. `0` means the axis does not constrain, **not** a ceiling of zero |
| `capacity_group` | string | `""` | Membership in the `provider_group` axis, for several providers sharing one upstream pool | A group not declared under `capacity.provider_groups` is refused, by name |
| `params.drop_unsupported` | bool | `true` | Filters parameters the kind cannot express, rather than forwarding them | Turning it off surfaces upstream `400`s that the client has never seen. The reference proxy every existing client was built against drops silently, which is why this defaults on |
| `params.drop[]` | []string | `[]` | Removes these parameters unconditionally | An empty or whitespace-only entry is refused |
| `params.set{}` | map | `{}` | Injects unconditionally, overriding the caller | ⚠️ This is the lever that can disable a safety mechanism. Setting `truncation: auto` or `truncate_prompt_tokens` here suppresses the very `400` that context-window fallback keys on, across the whole provider — see [EXTENSIONS.md](EXTENSIONS.md) §A.3a |
| `params.default{}` | map | `{}` | Injects only when the caller did not send them | Safer than `set`, and still capable of the same harm with the same two parameters |
| `retry.max_attempts` | int | `2` | In-provider retries, distinct from the fallback chain of §12 | Negative is refused |
| `retry.backoff` | `exponential` \| `linear` \| `constant` | `exponential` | Retry spacing | Anything else is refused |
| `retry.base` | duration | `500ms` | First retry interval | Negative is refused |
| `usage_probe.enabled` | bool | `false` | Polls the provider's own view of remaining quota | Without it, quota is metered locally only, and local metering under-counts by exactly the traffic that did not go through dorang |
| `usage_probe.fetcher` | string | `""` | Which provider-specific fetcher to use | Required when the probe is enabled |
| `usage_probe.interval` | duration | `0` | Poll interval | Negative is refused. A poll is up to one interval stale — see §6.1 below |
| `metrics.enabled` | bool | `false` | Scrapes a backend metrics endpoint for `least_busy` / `highest_tps` | See §6.2 |
| `metrics.endpoint` | string | `""` | The scrape URL | Required when `metrics.enabled` is true |
| `metrics.interval` | duration | `0` | Scrape interval | — |

### 6.0 What `base_url` means, and what dorang appends to it

**Write the root the vendor documents. dorang appends the version segment unless your URL
already contains one, and then appends the operation's path.**

| You write | dorang addresses (chat) |
|---|---|
| `https://api.example.com` | `https://api.example.com/v1/chat/completions` |
| `https://api.example.com/v1` | `https://api.example.com/v1/chat/completions` |
| `https://api.example.com/api/anthropic` | `https://api.example.com/api/anthropic/v1/messages` |
| `https://api.example.com/api/coding/paas/v4` | `https://api.example.com/api/coding/paas/v4/chat/completions` |
| `https://api.example.com/v1/openai` | `https://api.example.com/v1/openai/chat/completions` |

"Already contains one" means **anywhere in the path**, not only at the end — a segment
spelled `v` followed by a digit (`v1`, `v4`, `v1beta`). The last row is why: some vendors
put the version before the family prefix, and a rule that looked only at the final segment
would address `…/v1/openai/v1/chat/completions`.

> ⚠️ **The third row is the one that used to be wrong, and it failed silently.** dorang
> supplied the version only for a bare host, so an Anthropic-compatible coding plan
> configured as `https://…/api/anthropic` was addressed at `…/api/anthropic/messages` — not
> a route. One such vendor answers a request for a route it does not serve with **HTTP 200**
> and `{"code":500,"msg":"404 NOT_FOUND"}`, which dorang rendered as a successful assistant
> turn with empty content. Both halves are now fixed: the version is appended, and an
> upstream 200 that is not a response of its family is a `502 upstream_shape`.
>
> That second half now covers **every** surface, not only chat: embeddings, `count_tokens`,
> rerank, moderations, images, transcription, translation, speech and the Gemini adapter each
> refuse a 200 whose body is not an answer of the shape they serve. The test is the presence of
> a payload-bearing member or the family's own discriminator — either one, never both — so a
> vendor that omits the discriminator and a genuinely empty result set both still pass. Speech
> is the exception that proves the rule: its answer is an audio container with no members at
> all, so the test there is a body that is neither empty nor a JSON object.

**The escape hatch, if a vendor really serves the unversioned route.** A `base_url` that
already ends in the operation's own path is used verbatim — write
`https://api.example.com/api/gateway/chat/completions` and nothing is inserted. It pins one
operation, so it suits a provider that serves one.

### 6.1 Why provider-reported quota is combined, not substituted

Where a provider exposes remaining quota, that figure covers **all** use of the key, including
traffic that never passed through dorang. Local metering alone therefore under-counts. But a
poll is up to one interval stale, and during a burst the local counter is the *fresher* signal.
Declaring the provider "the truth" and discarding the local delta — the first design — meant
dorang routed most aggressively exactly when its information was worst.

```
effective_used = max( provider_reported_used ,
                      provider_reported_used_at_last_poll + local_delta_since_that_poll )
```

The provider figure re-baselines on every poll but never below the previous baseline plus the
local delta, so a lagging poll cannot erase a burst. Fetches run off the request path with
per-provider timeouts, and **a failed fetch never disables a credential** — a failed read is not
an exhausted quota. The last good snapshot is retained and its staleness is exposed.

### 6.2 Backend metrics have engine-specific traps

Enabling `metrics` does not by itself make `least_busy` correct. Both self-hosted engines have
metrics that are misnamed, dead, or reset:

- vLLM's `kv_cache_usage_perc` is a fraction 0–1 despite the name, and
  `--disable-log-stats` leaves `/metrics` returning **200 with zero series** — indistinguishable
  from idle while meaning the opposite. [VLLM.md](VLLM.md) §3.
- SGLang's `/metrics` **404s** when `--enable-metrics` is unset, which is the honest failure,
  and its `sglang:cache_hit_rate` is hard-reset to `0.0` on every decode report so the gauge
  spends most of its time at zero. [SGLANG.md](SGLANG.md) §4.3.

Read the engine's section before you route on its numbers.

---

## 7. `credentials`

One authenticating identity at a provider — **the unit quotas and concurrency actually attach
to**, and, for stateful conversations, the unit conversation state attaches to.

```yaml
credentials:
  - id: plan-a-1
    provider: plan-a
    key_env: DORANG_EXAMPLE_PLAN_A_KEY
    capacity_group: plan-a-account
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `id` | string | — | The credential's identity. It is **not secret**: it appears in routing decisions, metrics, headers and errors | Empty or duplicate is refused |
| `provider` | string | — | Which provider this identity authenticates at | Must name a declared provider. A deployment that lists a credential belonging to another provider is refused, by name |
| `key_env` / `key_file` / `key` | secret ref | — | Where the secret comes from (§0.3) | Zero or two or more sources is refused. `key_ref` is refused outright |
| `capacity_group` | string | `""` | Membership in the `credential_group` axis — per account, across every model that account can serve | A group not declared under `capacity.credential_groups` is refused |

> **Key material may be declared twice and the schema does not say which wins.** The same
> credential id can carry a secret reference under `credentials[]` *and* under
> `key_rotation.providers[].keys[]`. The two are merged rather than one being chosen, which is
> what makes the example configuration's split — provider binding in one place, concurrency
> ceiling in the other — mean what it says. If both carry a *different* secret, the resolution
> order is unspecified. Declare the secret once.

### 7.1 Credential affinity is a correctness constraint, not a cache optimization

Several accounts on one provider is the normal case. What is easy to miss is that for stateful
APIs the credential is also the unit **conversation state** attaches to. Four things are
account-scoped:

| Account-scoped state | If a later turn lands elsewhere |
|---|---|
| Server-side response handles (`previous_response_id` and equivalents) | The handle does not resolve — a hard error, or worse, a plausible answer drawn from a truncated conversation |
| Integrity-protected reasoning blocks | The receiving account cannot validate a block it did not issue |
| Prompt cache residency | Silent cost and latency regression; nothing reports it |
| Quota and spend windows | Not a failure — it is the reason the pool exists |

Only the third is recoverable. So affinity has two strengths, and picking the wrong one is the
bug: **preferred** spills to another account under capacity pressure and is right for stateless
chat; **pinned** waits or fails, because another account is not a worse choice but a wrong one.

dorang **infers** the pin rather than trusting configuration for it. A request carrying a
server-side handle or an opaque reasoning block is pinned by that fact, whatever
`key_rotation.…stickiness` says, because the setting expresses a preference about cost and this
is not a question about cost. Configuration chooses the policy for the *unpinned* case only.
The design's `stickiness.pin_on_state` knob does not exist in the schema for that reason.

---

## 8. `capacity`

The multi-axis ceilings. Within one provider, the unit a concurrency limit counts over
differs — one account may allow 3 in flight across all models while a coding plan allows 7
*per model*, so serving two models means 14 concurrent requests on one key. The axes exist to
express both at once.

```yaml
capacity:
  provider_groups:   {shared-pool: {max_concurrency: 24}}
  credential_groups: {plan-a-account: {max_concurrency: 7}, acct-1: {max_concurrency: 3}}
  models:
    - {provider: plan-a, model: model-x, max_concurrency: 7}
    - {provider: plan-a, model: model-y, max_concurrency: 7}
  principals: {default: {max_concurrent: 32, max_queue_wait: 30s}}
  global: {max_concurrency: 256}
  interactive_reserve: 0.3
```

### 8.1 The axes

| Axis | Where it is configured | Counts over |
|---|---|---|
| `route` | `providers[].max_concurrency` | one provider |
| `provider_group` | `capacity.provider_groups` | several providers sharing an upstream pool |
| `model` | `capacity.models[]` | **one (key, model) pair** |
| `credential_group` | `capacity.credential_groups` | **one account, every model** |
| `key` | `key_rotation.providers[].keys[].max_concurrency` | one key |
| `principal` | `capacity.principals` | one caller, keyed by **API key id** |
| `global` | `capacity.global` | the process (or the cluster, under a shared mode) |

Every axis a request needs is reserved in **one critical section**, all or nothing. Partial
occupancy never exists, so a waiter never holds one slot while queueing for another, and
deadlock is structurally impossible. Waiting is per-axis FIFO with targeted wakeup: measured at
exactly **1.00 wakeups per grant** with 1000 waiters against limits of 1, 7 and 32, where a
broadcast would be `O(waiters)` per release. The full seven-axis acquire path is 455 ns and one
allocation.

⚠️ **The cost of "no partial holding" is a liveness hole that is open.** A waiter needing two
saturated axes can sit at the head of both queues and never find them free at the same instant.
That is not a deadlock and not an overtaking problem, so the aging mechanism cannot close it;
it needs a soft-reservation protocol that is not yet designed. It is filed as risk W8, and it
is measurable: a waiter that never wins while both its axes stay saturated.

### 8.2 Keys

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `provider_groups.<name>.max_concurrency` | int | `0` | The provider-group ceiling | Negative is refused. **`0` is not a ceiling of zero — it means this axis does not constrain.** A group referenced by a provider but not declared here is refused |
| `credential_groups.<name>.max_concurrency` | int | `0` | The per-account ceiling, across every model | Same. This is the axis that models "3 in flight, any model" |
| `models[].provider` | string | — | Which provider the (key, model) ceiling belongs to | Must be declared |
| `models[].model` | string | — | The **upstream** model name the ceiling counts over | Empty is refused |
| `models[].max_concurrency` | int | `0` | The per-(key, model) ceiling | This is the axis that models "7 per model, so two models is 14" |
| `principals.<id>.max_concurrent` | int | `32` for `default` | The per-caller ceiling. `<id>` is an **API key id**; the entry named `default` applies to every caller with no entry of its own | Negative is refused. A `default` entry is always present — if you do not write one, `{max_concurrent: 32}` is inserted |
| `global.max_concurrency` | int | unset | The process- or cluster-wide ceiling | Absent means no global ceiling. Omitting the whole `global:` block is different from writing `{max_concurrency: 0}` only in intent; both leave the axis unconstrained |
| `interactive_reserve` | float | `0.3` | The fraction of **every** axis batch work may not occupy | Must be in `[0,1)`. `1.0` or higher is refused — it would make every axis unusable by batch entirely, which is expressible as `0.999…` and is more likely a typo |

⚠️ **Only `max_concurrency` / `max_concurrent` is enforced.** The schema accepts `rpm`, `tpm`
and `max_queue` on every capacity group, and `rpm`, `tpm`, `max_queue` and `max_queue_wait` on a
principal. The validator checks them for sign and then **nothing reads them**: the capacity
broker implements counted gauges only, with no token buckets and no queue-depth ceiling. See
§23.1. A ceiling you believe you set and that does nothing is worse than no ceiling, so this is
the single most important row in this document.

### 8.3 Why `interactive_reserve` is per-axis, and two arithmetic traps

An earlier design capped batch at a share of *credential* concurrency. That does not protect
the *model* axis: batch could take a model's entire limit while staying under its credential
share, starving interactive traffic for that model completely. The reserve therefore applies on
**every** axis.

Two things the arithmetic required:

- `floor(10 × (1 − 0.3))` evaluates to **6**, not 7, in binary floating point, so the reserve
  silently exceeds what was configured. An epsilon is applied.
- `floor(1 × 0.7) = 0` makes an axis *unsatisfiable* rather than merely contended. That returns
  an error immediately instead of blocking forever. If you set a ceiling of 1 and a reserve, the
  batch share of that axis is zero and batch is refused there — which is correct, but is a
  refusal rather than a wait.

Strict FIFO and the reserve also conflict: a blocked batch waiter at the head of a queue
head-of-line blocks exactly the interactive traffic the reserve exists to protect. A re-blocked
head waiter is parked for the remainder of the pass, keeping its sequence number, so the
next-oldest gets a turn.

---

## 9. `key_rotation`

```yaml
key_rotation:
  strategy: least_used
  providers:
    cloud-a:
      affinity_group: cloud-a-accounts
      stickiness: {scope: session, on_capacity: spill}
      keys:
        - {id: acct-1, key_env: KEY_1, max_concurrency: 3, capacity_group: acct-1}
        - {id: acct-2, key_file: /etc/dorang/secrets/2.key, max_concurrency: 3, capacity_group: acct-2}
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `strategy` | `round_robin` \| `least_used` \| `failover` \| `random` | `least_used` when any rotation is configured, otherwise unset | Intended to order the key pool | Anything else is refused. ⚠️ **This build does not act on it** — credentials are tried in configuration order within a deployment. See §23.1 |
| `providers.<name>` | map | — | The rotation policy for one provider's keys | The provider must be declared |
| `providers.<name>.affinity_group` | string | `""` | A label for the pool | — |
| `providers.<name>.stickiness.scope` | `session` \| `api_key` \| `user` \| `team` \| `tenant` \| `none` | `""` | The scope a credential pin is keyed on | Anything else is refused |
| `providers.<name>.stickiness.on_capacity` | `wait` \| `spill` | `""` | What an **unpinned** request does when its preferred credential is at capacity | Anything else is refused. It **never** applies to a pinned request: a pinned conversation that spills is not a worse choice, it is a wrong one (§7.1) |
| `providers.<name>.keys[].id` | string | — | The credential id this key's ceiling counts over | Empty or duplicate within the pool is refused. If the id names a credential of a *different* provider, that is refused by name |
| `providers.<name>.keys[].key_env` etc. | secret ref | — | Key material (§0.3) | Same rules |
| `providers.<name>.keys[].max_concurrency` | int | `0` | The `key` capacity axis | Negative is refused; under `leased`, below `cluster.min_leasable` is refused |
| `providers.<name>.keys[].capacity_group` | string | `""` | Credential-group membership | Must be declared under `capacity.credential_groups` |

---

## 10. `models`, `aliases`, `classes`

```yaml
models:
  - name: model-x
    class: chat-large
    strategy: [prefix_sticky, lowest_cost, least_busy]
    deployments:
      - provider: plan-a
        upstream_model: model-x
        credentials: [plan-a-1]
        weight: 10
        priority: 0
      - provider: cloud-a
        upstream_model: model-x:cloud
        credentials: [acct-1, acct-2]
        weight: 5
        priority: 1
        timeout: 90s
        stream_timeout: 60s
        limits:
          - {metric: rpm, value: 600}
          - {metric: tpm, value: 400000}

aliases: {model-large: model-x}
classes: {chat-large: [model-x]}
```

### 10.1 A model name is opaque

**No component splits a model name on any character.** Real names use `:` for at least three
different things — a family tag (`family:31b`), a vendor prefix (`vendor:model-5.1`), a
deployment variant (`model-flash:cloud`). A gateway that splits in one code path and preserves
in another makes identity, routing, aliasing and prefix affinity all non-deterministic for the
same model. Provider identity comes only from `providers[].name` and
`deployments[].upstream_model`.

The consequence for configuration: `model-x:cloud` is a name, not a structure, and dorang will
never infer a provider from it.

### 10.2 Keys

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `models[].name` | string | — | The client-facing name. Compared **whole** | Empty or duplicate is refused. A name that is also an alias is refused — the same name cannot mean two things |
| `models[].class` | string | `""` | The model class, which is the scope fail-back may delegate within | A class not declared under `classes` is refused. A model that declares a class but is **not listed among that class's members** is also refused: the two directions must agree |
| `models[].strategy[]` | []string | `[prefix_sticky, lowest_cost, least_busy]` | The tie-break chain. Allowed: `round_robin`, `least_busy`, `lowest_cost`, `lowest_latency`, `highest_tps`, `sticky`, `prefix_sticky`, `priority`, `weighted_random`, `quota_urgency` | Anything else is refused. `quota_urgency` composes **after** `lowest_cost` by default (§7.5a(c)): preferring an expiring allowance is right only when the alternative is also already paid for. The default chain is "stay warm, then stay cheap, then stay idle" |
| `models[].deployments[]` | list | — | The routing candidates, in configuration order — which is the final tie-break, so it is deterministic rather than map-random | A model with no deployments is refused |
| `deployments[].provider` | string | — | Which provider | Must be declared |
| `deployments[].upstream_model` | string | — | The real model id sent upstream. Used **verbatim**; never parsed | Empty is refused |
| `deployments[].credentials[]` | []string | `[]` | The accounts that may serve this deployment, in preference order | Each must be declared, and must belong to this deployment's provider — both refused by name otherwise |
| `deployments[].weight` | int | `0` | Biases `round_robin` and `weighted_random`. Zero is treated as one | Negative is refused |
| `deployments[].priority` | int | `0` | Orders the `priority` strategy. **Lower is preferred** | Negative is refused |
| `deployments[].timeout` | duration | `0` | This deployment's request timeout. It also bounds the capacity reservation | Negative is refused |
| `deployments[].stream_timeout` | duration | `0` | Streaming idle timeout | Negative is refused |
| `deployments[].limits[]` | list of `{metric, value}` | `[]` | Metric names: `max_concurrent`, `rpm`, `tpm`, `max_queue` | An unknown metric or a negative value is refused. ⚠️ **Only `rpm` and `tpm` do anything**, and they become a rolling-minute quota on the *credential*, not on the deployment — see below |
| `aliases.<name>` | string | — | Virtual name → model group. Upstream receives the real id; the response body carries back the name the client asked for | An alias that is also a model name is refused. An alias whose target is another alias is refused: **aliases do not chain** |
| `classes.<name>[]` | []string | — | Interchangeable groups | A class with no members is refused; a member listed twice is refused; a member that is not a declared model is refused |

> **A deployment's `rpm` / `tpm` become a per-credential quota, and a credential serving two
> deployments takes the strictest of the two.** The file states the ceiling on the deployment;
> §3 of the design attaches quotas to the credential. Resolving that by taking the strictest is
> deliberate: being wrong in the other direction would let a configured limit quietly stop
> existing. If two deployments on one credential need different rates, split the credential.

### 10.3 Why the response carries the alias back

Upstream always receives the real model id. The response body's `model` field carries back
**the name the client asked for**, restamped on every streaming chunk. That is not an
optimization to avoid — the reference proxy every existing client was built against restamps
per chunk, and a client that pins on `model` breaks otherwise.

The mechanism is a single-pass line-oriented scanner over SSE frames, not a JSON decode, and
dorang requests `Accept-Encoding: identity` upstream so there are no compressed bytes to patch.
When the requested and upstream names are identical the scanner short-circuits to a plain copy.
The earlier "patch the first frame by byte offset" claim was withdrawn: lengths differ in the
normal case, frame boundaries are arbitrary, the field is not reliably in the first frame, and
compression forecloses it entirely.

---

## 11. `routing`

```yaml
routing:
  sticky: {enabled: true, ttl: 1h, purge_interval: 5m, key: [api_key, session_id]}
  prefix: {enabled: true, chunk_bytes: 4096, checkpoints: logarithmic, max_bytes: 64MiB, ttl: 1h}
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `sticky.enabled` | bool | `true` | Pins a session to a target for the life of the upstream cache | — |
| `sticky.ttl` | duration | `1h` | How long a pin lives **from creation** | Zero or negative is refused when sticky is enabled. It is deliberately **not** refreshed on use: the premise is that the upstream cache is gone after the TTL, so refreshing would defeat the point |
| `sticky.purge_interval` | duration | `5m` | How often expired pins are swept | Negative is refused |
| `sticky.key[]` | []string | `[api_key, session_id]` | Components of the pin key. Allowed: `api_key`, `session_id`, `user`, `team`, `tenant` | An empty list or an unknown component is refused. The tenant component leads the real key internally, so two tenants never share a pin |
| `prefix.enabled` | bool | `true` | Order-exact prefix affinity by hash chain | — |
| `prefix.chunk_bytes` | size | `4096` | The base segment size. **Byte boundaries — there is no tokenizer on the hot path** | Zero or negative is refused |
| `prefix.checkpoints` | `logarithmic` \| `fixed` | `logarithmic` | How depths are chosen | Anything else is refused. `fixed` cannot distinguish two conversations that share a system prompt and diverge afterwards — they collide at the deepest tracked node and "longest common prefix wins" becomes quietly false |
| `prefix.max_bytes` | size | `64MiB` | The table is budgeted by **retained bytes**, not by entry count | Zero or negative is refused. One request writes an entry at every checkpoint, and per-entry size had been underestimated before this was measured; deployment ids are interned to keep an entry at 24 bytes |
| `prefix.ttl` | duration \| `until_evicted` | `1h` | **Default** entry lifetime, overridden per provider and per deployment. Unlike a session pin, a prefix entry **refreshes on use** — a prefix still in use is keeping the upstream cache warm | Zero or negative is refused when prefix is enabled; `until_evicted` is accepted |

#### The affinity lifetime is a fact about the backend

`prefix.ttl` models one thing: **how long the backend still holds the KV blocks for this
prefix.** That is not a property of dorang, and it differs between vendors by more than an
order of magnitude.

| Backend | What it actually does | Write |
|---|---|---|
| Anthropic prompt caching | ~5 minutes on the default tier, 1 hour on the extended tier — and the tier is a per-**request** property | `5m` or `1h`, per deployment |
| OpenAI automatic prompt caching | roughly 5–10 minutes, not contractual | `5m` |
| Gemini explicit caching | an operator-set TTL on the cached content itself | whatever you set there |
| vLLM, SGLang | **no TTL at all** — blocks live until LRU eviction under memory pressure | `until_evicted` |

Getting it wrong is not symmetric. Too long and the router keeps pinning a conversation to a
node that no longer holds the prefix: sticky routing with none of the benefit, paid for in lost
load balance and a hot node. Too short and hits that were there are thrown away.

So the lifetime is settable at three levels, most specific winning:

```yaml
routing:
  prefix: {ttl: 30m}                     # the default

providers:
  - {name: fleet-vllm, kind: vllm, prefix_ttl: until_evicted}
  - {name: cloud-a,    kind: anthropic, prefix_ttl: 5m}

models:
  - name: model-x
    deployments:
      - {provider: cloud-a, upstream_model: …, prefix_ttl: 1h}   # extended cache tier
```

`until_evicted` means the entry never expires on a clock. It is still bounded: the affinity
table is budgeted by **bytes** (`prefix.max_bytes`), and eviction runs in two passes — expired
entries first, then the coldest by last use. An entry with no lifetime is simply never in the
first pass, which is exactly the contract a self-hosted engine's prefix cache has.

The chain is `h₀ = H(group)`, `hᵢ = H(hᵢ₋₁ ‖ cᵢ ‖ len(cᵢ))`. A match at depth *i* proves
`c₁..cᵢ` are byte-identical **and in that order** — swap, reverse, insert, delete, prepend and
append all diverge. The length is a *suffix* rather than a prefix, which is what makes the chain
fully streamable: a trailing partial segment's length is not known until it ends, and putting it
first would have forced buffering the body. 16 MiB costs 728 B/op at 2.4 GB/s; a 4 KiB request
is 2 µs; a lookup on a hit is 110 ns with zero allocations.

Segments grow geometrically. A 16 MiB body yields 13 digests where fixed 4 KiB chunking would
yield 4096.

⚠️ **A varying token near the head of a prompt collapses prefix reuse to whatever precedes it.**
Some clients prepend a per-request attribution block whose hash changes every turn. This is not
an engine-specific problem and dorang cannot fix it from the outside — but dorang must also
never introduce one of its own by prepending request-scoped metadata to a forwarded prompt.

---

## 12. `fallbacks`

```yaml
fallbacks:
  on:
    rate_limit:      [same_group, same_class]
    quota_exhausted: [same_group, same_class]
    context_window:  [same_class_larger]
    content_policy:  [same_class]
    upstream_5xx:    [same_group, same_class]
    timeout:         [same_group]
    budget_exceeded: []
    auth:            []
  max_hops: 3
  budget_ms: 120000
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `on.<cause>` | []string | the table above | The chain for one cause. Causes: `rate_limit`, `quota_exhausted`, `context_window`, `content_policy`, `upstream_5xx`, `timeout`, `budget_exceeded`, `auth`. Targets: `same_group`, `same_class`, `same_class_larger` | An unknown cause or target is refused. **`budget_exceeded` and `auth` must have empty chains** — a non-empty one is refused |
| `max_hops` | int | `3` | Bounds fallback attempts **after the first dispatch**, so `3` permits **four attempts in total** | Negative is refused. Zero disables fallback entirely. Either reading of "hops" was defensible and the difference is a whole extra request, which is why it is stated |
| `budget_ms` | int | `120000` | Wall-clock ceiling on a whole routing session, from the first routing call | Negative is refused |

**Why `budget_exceeded` and `auth` cannot fall back.** Falling back on an exceeded budget would
send the request to a different deployment and spend a **different subject's** budget on a model
the caller never asked for. Budget refusal is therefore a terminal `400`, deliberately not a
`429` — a `429` is a rate-limit signal that both invites a retry and marks the condition as a
fallback trigger. An authentication failure marks the credential exhausted; retrying it
elsewhere spends hops to arrive at the same answer.

**Streaming boundary.** Fallback is permitted only before the first byte reaches the client.
After that, an error event ends the stream. Duplicated output is worse than a visible failure,
and the boundary is enforced by a test.

> ⚠️ **Opaque state pins a conversation to a protocol family, and that pin is hard.** Once a
> conversation carries family-scoped opaque state — an integrity-protected reasoning block, a
> server-side response handle, a vendor compaction cursor — a fallback that crosses families
> cannot succeed: the receiving family classifies foreign state as never-retryable, so every
> remaining hop burns and the caller gets the same `400` three times more slowly. See
> [EXTENSIONS.md](EXTENSIONS.md) §B.3.

---

## 13. `pricing`

```yaml
pricing:
  catalog: /etc/dorang/pricing.yaml
  currency: USD
  rules:
    - id: plan-a-tokens
      class: marginal_usage
      match: {provider: plan-a, model: model-x}
      rates: {input: "0.0000025", output: "0.00001", cached_read: "0.00000025"}
    - id: plan-a-subscription
      class: fixed_subscription
      match: {credential: plan-a-1}
      period: monthly
      amount: "20.00"
    - id: house-margin
      class: adjustment
      percent: "5"
```

### 13.1 Keys

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `catalog` | path | `""` | An external price list, merged with the inline rules into one catalog | **A named-but-absent catalog is not fatal** — the inline rules may be the whole price list, and an unpriced model is already visible through a counter rather than through a refusal to start. A malformed one *is* fatal |
| `currency` | string | `USD` | The ISO code every amount is denominated in. It overrides the catalog file's own `currency` | Empty is refused |
| `rules[].id` | string | `""` | Rule identity, and the final deterministic tie-break | A duplicate id is refused |
| `rules[].class` | `marginal_usage` \| `fixed_subscription` \| `adjustment` \| `notional_rate` | `marginal_usage` | Which kind of cost this is | A `notional_rate` rule must carry `source` and `as_of`, and no other class may (§8.5) |
| `rules[].priority` | int | `0` | Breaks a specificity tie before the id does | Negative is refused |
| `rules[].match.{credential,provider,model,model_prefix,deployment}` | string | `""` | The specificity ladder | A `provider` or `credential` that is not declared is refused. `model` and `model_prefix` are **not** checked against declared models — a rule may legitimately price a model that no deployment currently serves |
| `rules[].rates.<component>` | decimal | — | Per-unit rates. Components: `input`, `output`, `cached_read`, `cache_write`, `reasoning`, `request`, `characters`, `seconds`, `images` | A `marginal_usage` rule with no rates is refused. ⚠️ **`images` passes validation and then refuses to assemble** — the request type carries no image count, so the component is unreachable and is reported rather than silently priced at zero. Use `request` on an image endpoint |
| `rules[].period` + `rules[].amount` | string + decimal | — | A `fixed_subscription` rule's period and cost | Both required for that class; a zero amount is refused |
| `rules[].percent` | decimal | — | An `adjustment` rule's percentage | Required for that class; zero is refused |

### 13.2 Classes compose; they do not compete

A single-winner rule model is broken: a credential-scoped subscription rule outranks the
model-scoped token rule and zeroes token cost, while summing them contradicts "most specific
wins". They are different *kinds* of cost, not competing descriptions of one.

```
cost = marginal(winner) + amortized(subscription winner), then adjustments applied in order
```

Each class elects its own winner. Marginal cost is bit-identical with and without a
subscription rule present, and **routing sees only the marginal figure** — a sunk plan cost must
not make a saturated plan look cheap.

Arithmetic never touches binary floating point: prices parse from their string form, products go
through 192-bit `mulDiv`, and amounts round once, half-to-even, with the remainder carried per
settlement bucket. Two thousand single-token requests at $0.0005/1M sum to exactly 1000 nano
instead of all rounding to zero.

**Estimating and settling are different operations.** The carried remainder and the
period-to-date accumulator are *state*, and a cost-based router prices every candidate on every
request — so if pricing had side effects the losers would corrupt the ledger. Estimation is
pure; only settlement carries.

An unpriced model costs zero for **accounting** and increments a counter and logs a warning.
For **routing**, an unpriced model has *no opinion* and is ranked by the next comparator, because
zero is the cheapest value and the deployment nobody priced would otherwise win every group
permanently and silently.

### 13.3 The catalog file has a different, larger schema

The external catalog is not the same shape as `pricing.rules`. It supports things the inline
form does not, and the two are spliced into one document before compilation:

| Inline `pricing.rules` | Catalog file |
|---|---|
| `rates: {input: …, cached_read: …}` | `input:`, `cache_read:` as **top-level** rule fields — note `cached_read` → **`cache_read`** |
| `amount` + `period` | `amount_per_period` + `period` |
| `percent` | `op: percent` + `amount`, plus `applies_to` and `order` |
| — | `unit:`, `tiers[]` + `tier_mode`, `when: {time_of_day, tz, weekday, date_range}` |
| — | **`class: notional_rate`** with mandatory `source:` and `as_of:` |
| — | a file-level `tz:` |

The name translation is deliberate in both directions rather than one file being changed to
match the other.

**`notional_rate` is what a subscription is actually worth.** A flat plan bills a fixed amount,
so its marginal cost per request is zero — correct for billing and useless for deciding whether
to renew it, which team consumes its value, or what the bill would look like after outgrowing
it. A `notional_rate` rule records what the same traffic would have cost at the provider's
pay-as-you-go list price. It is:

- **structurally excluded from the bill** — a separate field, a separate rollup column, absent
  from the total, and absent from the component breakdown so a caller who sums components rather
  than reading the total still gets the billed figure;
- **required to carry `source` and `as_of`**, both as load errors rather than warnings. A rate
  with no provenance is a guess wearing a currency symbol. Those two fields on any *other* class
  are also rejected, because a silently ignored provenance field is the same defect as a missing
  one;
- **reported as missing rather than as zero**, with its own flag rather than the unpriced-model
  one. Zero would make a subscription look infinitely efficient, which is the most flattering
  possible answer and the one least likely to be questioned.

> One caveat before trusting the leverage ratio. A flat plan usually has no `marginal_usage`
> rule at all, so amortization falls back to elapsed fraction and the denominator is apportioned
> by *time* rather than by usage. Over a full period the ratio is right; within one, a quiet hour
> reads as poor leverage and a busy one as excellent, when neither is a fact about the plan.

Preview any of this with `dorangctl price <model> --input N --output N`, which runs the same
evaluator the ledger runs.

---

## 14. `metering`

```yaml
metering:
  numeric: {enabled: true}
  trace:   {store_messages: truncated, truncate_chars: 512, sample_rate: 1.0, daily_byte_budget: 8GiB}
  spool:   {dir: ~/.dorang/spool, max_bytes: 2GiB}
  flush_interval: 250ms
```

Two queues. Numeric accounting goes to per-CPU fixed-cardinality counters that are **never
dropped**; trace payloads go through a bounded ring to a durable local spool and **are**
droppable, with the drop counted.

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `numeric.enabled` | bool | `true` | Cost, tokens and error counts | ⚠️ **Setting it `false` is refused**, with a message telling you to reduce `trace.sample_rate` instead. Numeric accounting must survive any back-pressure; the whole two-queue split exists so that it can |
| `trace.store_messages` | `none` \| `hash` \| `truncated` | `truncated` | What of a message body reaches `request_traces` | Anything else is refused. `truncated` is the only one that lets you read a request back |
| `trace.truncate_chars` | int | `512` | Excerpt length | Negative is refused. Excerpts are copied into a byte arena rather than retained as substrings — a 512-byte substring of a 400 KiB body would pin the whole body while queued |
| `trace.sample_rate` | float | `1.0` | Fraction of requests whose trace payload is kept | Must be in `[0,1]`. At the enterprise tier, 100% excerpts are ~88 GB/day; at the notebook tier, ~44 MB/day |
| `trace.daily_byte_budget` | size | `8GiB` | A hard daily ceiling on trace bytes | Negative is refused. It is enforced, not hoped for |
| `spool.dir` | path | `~/.dorang/spool` | The durable buffer between the trace queue and the store, so a store stall costs **disk rather than data** | Empty is refused. An empty *value* is not the same as an unwritable directory, which surfaces as a degraded state rather than at load |
| `spool.max_bytes` | size | `2GiB` | The spool's own cap | Negative is refused. Reaching it drops traces and counts them — a queue that grows until the process dies has converted a store outage into an outage |
| `flush_interval` | duration | `250ms` | How often counters merge and the spool ships | Zero or negative is refused. It is also the window over which a crash loses precision: quota rings and rollups are reconstructible from their last upsert plus the ledger, so losing the tail costs precision rather than correctness, and this interval is how much |

**Measured cost of metering:** +148 ns steady state and **+110 ns at a full buffer**, which is
0.074% of the 200 µs warm-local p50 budget. The full buffer being *cheaper* is the split working
as designed — a failed ring push skips the payload copy while the numeric path does identical
work. Note that "metering on vs off < 5%" needed a denominator: against a no-op meter the ratio
is 7.6×, but the no-op returns after one branch, so dividing by it measures nothing about a real
request. The requirement is 5% of the gateway-overhead budget.

Sampling exclusions deliberately do **not** raise the degraded flag. Conflating them would leave
it permanently true on any sampled deployment, which is the same as having no flag.

⚠️ **The degraded state is not observable in this build.** See §23.2.

---

## 15. `observability`

```yaml
observability:
  prometheus: true
  otlp_endpoint: ""
  log_level: info
  log_format: json
  always_full_headers: false
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `prometheus` | bool | `true` | Serves `GET /metrics` | `false` removes the route, which then answers 501 with a reason like any other unserved route — not 200 with an empty body |
| `metrics.public` | bool | `false` | Serves `/metrics` without authentication | Off by default. The scrape carries per-key spend, per-credential quota state and every configured model name, so it needs the master credential unless a deployment says otherwise in writing |
| `otlp_endpoint` | string | `""` | OTLP trace export target | ⚠️ Not acted on in this build; the latency breakdown is recorded regardless. §23.1 |
| `log_level` | `debug` \| `info` \| `warn` \| `error` | `info` | Log verbosity | Anything else is refused |
| `log_format` | `json` \| `text` | `json` | Log encoding | Anything else is refused |
| `always_full_headers` | bool | `false` | Attaches the full extension header set to **every** response instead of only when a caller sends `x-dorang-detail: full` | Turning it on adds roughly thirty headers ahead of the first streamed byte, which risks intermediary header-size limits and costs the latency target it was serving |

**The header rule, because it is easy to get backwards:** a header the client **acts on** is
unconditional; a header the client **reads** may be gated. So `Retry-After` and the
`x-ratelimit-*` set are always attached — gating `Retry-After` behind a telemetry flag means
every SDK's backoff silently stops working on a `429`. Of dorang's own headers, only
`x-dorang-request-id`, `-model`, `-upstream-model`, `-deployment`, `-cost-usd` and
`-replayable` (when false) are unconditional. The full list is in
[OPERATIONS.md](OPERATIONS.md) §6.

---

## 16. `extensions`

```yaml
extensions:
  lua:
    enabled: false
    dir: /etc/dorang/lua
    hooks: [on_request, on_route, on_response, on_email]
    limits: {instructions: 5000000, memory_mb: 32, timeout: 200ms}
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `lua.enabled` | bool | `false` | Turns on the sandboxed hooks. Off, the engine is a typed nil and the hot path costs one nil check | — |
| `lua.dir` | path | `/etc/dorang/lua` | Where hook programs live | Empty is refused when enabled |
| `lua.hooks[]` | []string | `[]` | Allowed: `on_request`, `on_route`, `on_response`, `on_email` | Anything else is refused |
| `lua.limits.{instructions,memory_mb,timeout}` | int/int/duration | `5000000` / `32` / `200ms` | Per-hook ceilings | Negative is refused. **An enabled hook with any of the three at zero is refused** — an unbounded hook on the hot path is not a hook, it is an outage waiting for a bad program |

Semantics: exceeding a ceiling skips the hook and warns (**fail-open**), except an explicit deny
from `on_request`, which is honoured (**fail-closed**). A panic in a hook is contained. No hook
can see a secret — the views handed to them are constructed without key material rather than
filtered afterwards.

**Three ways to write an extension, one contract.** `extensions.lua.dir` holds `*.policy`
files: a total policy language with no loops, no calls and no string building, so a program
terminates by construction. Lua *plugins* are declared under [`filters.plugins`](#17-filters),
not here, because §11.5 requires loading to be explicit configuration — a directory that runs
whatever appears in it is a code-execution primitive. Compiled-in Go `Native` hooks are the
third. All three see the same secret-free views and obey the same rules (a `Native` gets
wall-clock and panic containment only; it is Go code, so a memory ceiling over it means
nothing).

> **A `.lua` file under `extensions.lua.dir` is a load error**, and the message says where the
> file belongs. It is neither run — that would make the directory a code-execution primitive —
> nor silently ignored, which would leave an operator believing a filter is enforced.

**About the ceilings.** The VM is gopher-lua, pure Go, about 1.8 MB of binary. It supplies the
wall clock (a context check runs between VM instructions) and neither of the other two: there
is no per-state instruction counter and no per-state allocation accounting. Both are built on
top rather than withdrawn.

- *Instructions* are charged by rewriting the plugin's syntax tree at load: a charge at every
  function body, every loop body and every backward `goto`, which are the only three ways Lua
  can execute unboundedly. `instructions` therefore counts work, not merely back-edges.
- *Memory* is charged allocation. Every allocation is either O(1) per charge — hence bounded by
  the instruction ceiling — or is charged before it happens: string concatenation is rewritten
  into a charged host call, and `string.rep`, `string.format`, `string.gsub`, `string.byte` and
  `table.concat` pre-flight their worst case. The budget is **cumulative, not live**: bytes
  charged are never refunded, because a host cannot see when Lua's collector frees a string.
  That bounds peak memory from above, and it means a long-running hook that allocates and
  releases repeatedly hits the ceiling earlier than its true footprint deserves.

**What the sandbox does not contain**, each for a reason: `io`, `os`, `debug`, `coroutine` and
the package loader are never opened; `load`, `loadstring`, `loadfile`, `dofile` and `require`
are removed because each turns text into a function that never passed the instrumenter;
`_G`, `getfenv` and `setfenv` because they hand out the globals table, where the charge
functions live; `pcall` and `xpcall` because a ceiling a plugin can catch is not a ceiling;
`math.random` because a filter that is not a pure function of its input produces different
upstream bytes on every turn and silently kills prefix caching.

> One capability the hook points deliberately do **not** have: `on_route` sees the chosen
> deployment and may refuse it, but cannot ask for a different one. That is a property of the
> fail-back machinery, not of the hook.

---

## 17. `filters`

Transform filters sit between the canonical request and the backend (§10.5b). The motivating
case is reversible PII masking: an identity number is replaced with a placeholder before the
text leaves the operator's control and restored in the answer.

```yaml
filters:
  secret:
    key_env: DORANG_FILTER_SECRET
  plugins:
    - name: pii-mask
      path: /etc/dorang/plugins/pii_mask.lua
      fail: closed
      config: {skip_system: "true"}

models:
  - name: model-x
    filters:
      - plugin: pii-mask
        on: [request, response]
        scope: conversation
        patterns: [krrn, email, {name: employee_id, regexp: 'EMP-\d{6}'}]
        retain: 1h
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `secret` | secret ref | — | The cluster-wide seed a placeholder is derived from | **Required unless every filter uses `scope: request`.** It must be the same on every node and across restarts: a per-process seed masks the same text differently everywhere, so the bodies reaching a backend differ byte for byte and every prefix cache cold-starts (§7.4b). Rotating it invalidates every cache on the fleet — a staged, announced operation |
| `plugins[].name` | string | — | How a model refers to the plugin | Must be unique; a model naming a plugin that is not declared is refused at load |
| `plugins[].path` | path | — | The Lua file. Named explicitly, never scanned for | A missing file is a startup failure, not a filter that quietly does nothing |
| `plugins[].fail` | `closed` \| `open` | `closed` | What happens when the plugin does not complete | `closed` refuses the request. This inverts §11.5's fail-open on purpose: a filter that cannot *enrich* should be skipped, but one that was supposed to remove an identity number and did not must stop the request |
| `plugins[].config` | map[string]string | `{}` | Handed to the plugin as `dorang.config` while it loads | It holds no secret; it is readable by untrusted code |
| `models[].filters[].plugin` | string | — | Which declared plugin runs | A dangling name is refused |
| `models[].filters[].on` | []string | `[request, response]` | Which halves run | `[response]` alone is refused: there is nothing to unmask if nothing was masked |
| `models[].filters[].scope` | `conversation` \| `principal` \| `tenant` \| `request` | `conversation` | How far a placeholder's stability reaches | See below — this is the one knob with a privacy/performance trade in it |
| `models[].filters[].patterns[]` | name or `{name, regexp}` | — | What gets masked. Built-ins: `krrn`, `email` | A pattern that can match the empty string is refused. An unknown name with no regexp is refused |
| `models[].filters[].retain` | duration | `1h` | How long the reverse table holds a mapping in memory | It should be at least as long as the provider keeps its own conversation state. Memory only, never written anywhere |

**Scope is the whole trade, and it is not avoidable.** A placeholder is derived —
`HMAC(secret, scope ‖ salt ‖ value)` — so the same text always masks to the same bytes, which
is what keeps the upstream prefix identical across turns and the backend's KV cache alive. The
price is that a deterministic placeholder is a stable pseudonym: within its scope, the provider
can link "this same person appears in these requests". That is the same property as the cache
hit, not a bug next to it.

| `scope` | Stable across | Prefix caching | Linkability |
|---|---|---|---|
| `conversation` | the turns of one conversation | works | nothing the provider did not already see |
| `principal` | everything one key sends | works, across conversations | a key's traffic links |
| `tenant` | one team | works, across a team | a team's traffic links |
| `request` | nothing | **turned off for that route** | none |

`request` disables prefix affinity for the request rather than leaving a claim about bytes that
can never repeat.

**Three limits, stated rather than silent.** A thinking block's text is not masked (it travels
with provider integrity material dorang replays byte-identically). A tool call's arguments are
not masked (the neutral form keeps them as raw JSON because re-encoding reorders keys, and a
filter that re-encoded them would break the determinism above). A surface whose text the filter
cannot reach — embeddings, for instance — is **refused** on a filtered model rather than sent
unfiltered.

**Counters.** `dorang_filter_masked_total`, `_restored_total`, `_unresolved_total` and
`_refused_total`. `_unresolved_total` rising means a model is emitting placeholder-shaped text
this gateway never issued, which is worth knowing. No counter carries a label derived from
caller text, and no placeholder or original ever reaches a log line, a metric, the ledger or a
trace.

---

## 18. `passthrough`

Provider-native routes, opened by configuration rather than by writing an adapter each time.

```yaml
passthrough:
  enabled: false
  routes:
    - {prefix: /anthropic, provider: cloud-a}
    - {prefix: /plan-a, provider: plan-a, auth: dorang, meter: true}
  default: {auth: dorang, meter: true, timeout: 600s}
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `enabled` | bool | `false` | Turns the engine on. **An unmapped prefix is not served: this is not an open proxy** | With it off, no passthrough route is registered at all |
| `routes[].prefix` | string | — | The path prefix to map | Empty, not starting with `/`, containing `..`, or duplicated — all refused. `..` is refused because joined paths are normalized and traversal is rejected, and rejecting it at load is cheaper than proving the normalizer right |
| `routes[].provider` | string | — | Where the remainder of the path is joined onto | Must be declared. ⚠️ **A route whose provider has no `base_url` is silently dropped** rather than pointed at nothing — the route simply does not exist, and you find out with a 501 |
| `routes[].auth` | `dorang` \| `client` \| `none` | from `default.auth` | `dorang` authenticates with a dorang key; `client` passes the caller's own credential through; `none` is unauthenticated | Anything else is refused. `none` makes that prefix a public proxy onto the provider |
| `routes[].meter` | bool | from `default.meter` | Best-effort metering | With it off, spend through that prefix is invisible |
| `routes[].timeout` | duration | from `default.timeout` | Per-route timeout | Negative is refused |
| `default.auth` / `.meter` / `.timeout` | — | `dorang` / `true` / `600s` | Applied to any route that does not override | — |

**Four security rules, and the last two were missing from an earlier draft:**

1. Unmapped prefixes are not served.
2. Joined paths are normalized and traversal is rejected.
3. **Redirects are never followed.** A `30x` from an upstream makes an ordinary HTTP client
   re-issue the request — *with the provider credential attached* — to a host the **upstream**
   chose. That turns any compromised or misconfigured backend into a credential-exfiltration
   primitive, needing no attacker access to dorang at all.
4. **Credentials are stripped in both directions.** "Never reach the client" was stated only
   for the request path; a backend that echoes back the key it was given would have had it
   relayed straight through.

⚠️ **A prefix map is not an allow-list.** Both self-hosted engines put destructive
development-mode routes on the same base URL as inference — vLLM's `/reset_prefix_cache` can
preempt every running request, and SGLang's `/flush_cache`, `/slow_down` and `/pause_generation`
are open by default on a keyless server. Mapping `/vllm` maps those too. This build offers no
per-route method+path allow-list; until it does, only map a prefix onto a provider whose whole
surface you are willing to expose to whoever can reach that prefix. See
[EXTENSIONS.md](EXTENSIONS.md) §E3.1 and [SGLANG.md](SGLANG.md) §8.4.

---

## 19. `shadow`

Compares a sampled fraction of live traffic against a reference gateway during a migration.
Full procedure in [MIGRATION.md](MIGRATION.md) §4.

```yaml
shadow:
  mode: "off"
  reference: {url: "", api_key_env: DORANG_SHADOW_REFERENCE_KEY, timeout: 60s}
  sample_rate: 0.05
  compare: {structural: true, semantic: false, ignore_fields: []}
  max_cost_usd_per_day: "5"
  unpriced_estimate_usd: "0.01"
  queue_size: 256
  workers: 4
  capture: {head_bytes: 256KiB, tail_bytes: 16KiB}
  report: {path: ~/.dorang/shadow.jsonl, max_bytes: 256MiB}
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `mode` | `off` \| `mirror` \| `compare` | `off` | `mirror` sends and records; `compare` sends, records and diffs | Anything else is refused. Quote `"off"` — bare `off` is YAML boolean false |
| `reference.url` | string | `""` | The gateway being compared against | Required when mode is not `off`. Must be `http`/`https`, must have a host, and **must carry no query or fragment** — neither can survive being joined with the request path |
| `reference.api_key_env` | string | `""` | Names the variable holding the **reference gateway's own** credential. The client's credential is never forwarded there | This is the one place in the file that spells a secret reference `api_key_env` rather than the `key_env`/`key_file` pair. **A deployment that keeps secrets in a file or a vault cannot express a shadow reference credential at all** |
| `reference.timeout` | duration | `60s` | Bounds one reference call | Negative is refused. It is deliberately shorter than `server.request_timeout`: a shadow call that outlives the request it copies is holding a worker for a comparison nobody will read |
| `sample_rate` | float | **`0`** | Fraction of eligible requests shadowed | Must be in `[0,1]`. ⚠️ **There is no default.** Turning `mode` on without setting this shadows nothing, produces an empty report, and reports gate verdict `no_data` — and an empty report is exactly what a cutover decision looks for |
| `compare.structural` | bool | `true` | Compares status, field-path set, types at each path, header keys, and the error envelope | With `mode: compare` and structural off, the configuration is refused with "use `mode: mirror` instead" |
| `compare.semantic` | bool | `false` | — | ⚠️ **Setting it true is refused.** The design names the knob and specifies no comparison for it. Accepting it silently would mean an operator who asked for semantic comparison gets structural comparison and an empty report, and reads that report as proof of something it never checked — the one failure mode a cutover gate cannot have |
| `compare.ignore_fields[]` | []string | `[]` | Field paths excluded on top of the built-in set (ids, timestamps, `system_fingerprint`, output text, token counts). A bare name matches at any depth; a path beginning `$.` matches exactly; a trailing `*` matches a prefix | An empty entry is refused. Every field you ignore is a field a clean report says nothing about |
| `max_cost_usd_per_day` | decimal | — | Daily ceiling on reference-gateway spend | ⚠️ **Required** whenever mode is not `off`. Both modes send every sampled request twice and therefore cost twice; an earlier revision required the ceiling for `mirror` only, which is backwards — `compare` is `mirror` plus a diff |
| `unpriced_estimate_usd` | decimal | `0.01` | What one shadow call is charged when dorang could not price the request it copies | Deliberately not zero. Charging zero would make the ceiling unenforceable for exactly the traffic whose cost is unknown, which is the traffic most worth capping |
| `queue_size` | int | `256` | Bounds the shadow work queue. Past it, work is dropped and counted | Zero or negative is refused. A full queue must never push back into the request path |
| `workers` | int | `4` | Concurrent reference calls | Zero or negative is refused |
| `capture.head_bytes` | size | `256KiB` | Bytes kept from the start of a response | Zero or negative is refused — a comparison needs the response it compares. Large enough that an ordinary chat response is captured whole, which is what keeps a comparison conclusive |
| `capture.tail_bytes` | size | `16KiB` | Bytes kept from the end | Negative is refused. Two windows rather than one, because a stream's terminator is the last thing on the wire and a single head buffer loses exactly it |
| `report.path` | path | `~/.dorang/shadow.jsonl` | The JSONL diff report | **Required when mode is `compare`**: an empty diff report is the completion criterion, and a report that is not written is not empty, it is absent |
| `report.max_bytes` | size | `256MiB` | Report cap | Negative is refused. Reaching it drops records and counts them — a truncated report read as an empty one is the worst outcome this mechanism has |

`queue_size × (head_bytes + tail_bytes)` is the memory shadowing may hold: 256 × 272 KiB ≈ 68 MiB
worst case at the defaults.

---

## 20. `notifications`

An **event bus with pluggable sinks**, not an email system. Everything under `email` is delivery;
everything beside it is the pipeline — and the pipeline exists because a notification is a
deferred write, and §9.6 requires a deferred write to be bounded, visible when it drops, and
never on the request path.

```yaml
notifications:
  email:
    driver: none                  # smtp | http | lua | none
    from: dorang@example.invalid
    to: [ops@example.invalid]
    smtp: {addr: "mail.internal:587", username: dorang, key_env: SMTP_PASSWORD,
           starttls: true, tls_skip_verify: false, timeout: 10s, helo: ""}
    http: {url: "https://hooks.example.invalid/dorang", key_env: DORANG_WEBHOOK_SECRET,
           timeout: 10s, headers: {}}
  events: [key_created, budget_80pct, budget_exceeded, quota_exhausted,
           credential_unhealthy, batch_completed, invite]
  queue_size: 256
  workers: 1
  dedup_period: 1h
  dedup_periods: {}
  retry: {max_attempts: 3, initial_backoff: 1s, max_backoff: 30s,
          breaker_threshold: 5, breaker_cooldown: 1m}
```

### 19.1 Pipeline

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `events[]` | []string | `[]` | Allowed: `key_created`, `budget_80pct`, `budget_exceeded`, `quota_exhausted`, `credential_unhealthy`, `batch_completed`, `invite` | Anything else is refused |
| `queue_size` | int | `256` | Bounds the queue between the request path and the sender. Reaching it **drops, and the drop is counted** | Negative is refused. At a few hundred bytes per notification this is bounded memory and far more than a healthy deployment holds. A queue that grows instead would put a wedged mail server inside the request path |
| `workers` | int | `1` | Delivery workers | Negative is refused. **One is deliberate**: mail is not a throughput problem, and N workers hammering a server that is already failing is the behaviour the breaker exists to prevent |
| `dedup_period` | duration | `1h` | How often one subject may raise the same event | Negative is refused. Without it `budget_80pct` fires on **every request past the threshold** — the difference between something an operator reads and something an operator filters. An hour is the interval at which someone wants reminding that a budget is still at 80%, not the interval at which requests arrive |
| `dedup_periods.<event>` | duration | — | Per-event override. **Zero disables deduplication for that event** | An unknown event name or a negative value is refused. `key_created` happens once by construction and is not deduplicated |
| `retry.max_attempts` | int | `3` | Delivery attempts before the notification is dropped and counted | Negative is refused |
| `retry.initial_backoff` | duration | `1s` | First retry interval | Negative is refused |
| `retry.max_backoff` | duration | `30s` | Backoff ceiling | Negative is refused, **and shorter than `initial_backoff` is refused** with both values named |
| `retry.breaker_threshold` | int | `5` | Consecutive failures before delivery stops being attempted at all | Negative is refused |
| `retry.breaker_cooldown` | duration | `1m` | How long the breaker stays open | Negative is refused |

The request path does exactly two things: ask whether this alert is already accounted for (a
sharded map lookup, zero allocations), and if not, hand over a notification (a non-blocking
enqueue). Rendering, redaction, connection and retry all happen on a worker.

### 19.2 Drivers

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `email.driver` | `smtp` \| `http` \| `lua` \| `none` | `none` | Which sink delivers | Anything else is refused |
| `email.from` | string | `""` | Envelope sender | Must contain `@`. **Required by the `smtp` driver** |
| `email.to[]` | []string | `[]` | Default recipients | Each must contain `@`. **At least one is required by the `smtp` driver** — an address that is not an address fails at delivery time, which is to say during the incident the notification was meant to report |
| `email.smtp.addr` | string | `""` | `host:port` | Required by `smtp`, and must parse as `host:port` |
| `email.smtp.username` | string | `""` | SMTP user | **Required alongside a password** |
| `email.smtp.key_env` / `key_file` | secret ref | — | The SMTP password, as a normal secret reference (§0.3) | An inline literal outside development is refused, as everywhere else |
| `email.smtp.starttls` | bool | `false` | Upgrades the connection | ⚠️ **Setting a username with `starttls: false` is refused.** Go's SMTP client will not send `PLAIN` over an unencrypted connection to anything but a loopback server, so this would otherwise fail at delivery time; saying so at load is the cheaper discovery |
| `email.smtp.tls_skip_verify` | bool | `false` | Accepts an unverifiable certificate | A deliberate downgrade, for a private relay |
| `email.smtp.timeout` | duration | `10s` | Per-delivery timeout | Negative is refused |
| `email.smtp.helo` | string | `""` | The name dorang announces itself as | — |
| `email.http.url` | string | `""` | Webhook endpoint | Required by `http`. Must be `http`/`https` with a host. **A query string is fine here** — unlike a shadow reference the URL is used whole rather than joined with a request path, and hosted webhooks routinely carry a token in one |
| `email.http.key_env` / `key_file` | secret ref | — | The signing secret | ⚠️ **Required, not optional.** A delivery is signed with HMAC-SHA256 over the body, and a receiver with no secret cannot tell a real delivery from a forged one. The payload carries budget and quota state, so an unsigned webhook is an information leak waiting for a reachable URL |
| `email.http.timeout` | duration | `10s` | Per-delivery timeout | Negative is refused |
| `email.http.headers{}` | map | `{}` | Extra request headers | — |

⚠️ **`driver: lua` requires the `on_email` hook.** Selecting it with `extensions.lua.enabled:
false`, or with a `hooks` list that omits `on_email`, is **refused** — a mail system that silently
sends nothing is exactly the failure this section is about.

### 19.3 No secret in a payload

A notification is assembled from named fields, and three independent things must agree before one
reaches a message:

1. The field name is on that event's allow-list. Anything else is dropped and counted — a caller
   cannot add a field by accident.
2. The field name is not on the refusal list, regardless of any allow-list.
3. The value does not *look* like key material. A value that starts like a provider token, or that
   is long and opaque, is replaced with a redaction marker and counted.

So a `key_created` notification carries the key id and label and **cannot carry the key**, whether
or not the caller tried. Both directions are asserted by tests — from the caller's side and from
the renderer's.

---

## 21. `priority_mapping`

```yaml
priority_mapping:
  classes: {realtime: 0, interactive: 2, batch: 10}
  emit:
    vllm:   {field: priority}
    openai: {field: service_tier, map: {realtime: priority, interactive: default, batch: flex}}
    header: X-Request-Priority
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `classes.<name>` | int | `{realtime: 0, interactive: 2, batch: 10}` | The canonical scale. **Lower is more urgent** | Negative is refused. Setting any class replaces the whole default map, so name all three |
| `emit.header` | string | `X-Request-Priority` | The header sent to a backend that understands no native priority field | Refused if empty *and* no backend mappings are given: a backend with no native field still receives the header |
| `emit.<backend>.field` | string | — | The backend's native priority field | Empty is refused |
| `emit.<backend>.map.<class>` | string | — | A class → wire-value translation for backends whose field is not numeric | A class that is not declared under `classes` is refused, and so is an empty value |

Any key under `emit` other than `header` names a backend, and a backend block may contain only
`field` and `map`. Anything else is refused with the line number.

### 20.1 The two self-hosted engines order priority in **opposite directions**

⚠️ vLLM schedules the **lowest** value first. SGLang schedules the **highest** first by default.
Same field name, same type, both return `200` either way, and nothing in the response reveals
which happened. With the canonical map above, sending it unchanged to SGLang makes **batch
outrank realtime** — an inversion, not a degradation.

The canonical scale stays lower-is-more-urgent and the adapter negates for descending engines.
The recommended fix on the SGLang side is `--schedule-low-priority-values-first`, which makes
SGLang agree with vLLM and lets one map serve both. Details: [VLLM.md](VLLM.md) §1.2,
[SGLANG.md](SGLANG.md) §0.1 and §3.

Two further things an operator must know before relying on priority at all:

- **Both engines ignore it silently by default.** vLLM needs `--scheduling-policy priority`;
  SGLang needs `--enable-priority-scheduling`. Without them the value travels into the engine
  and has no consumer. dorang records the operator's declaration and flags an unverified
  emission on the decision rather than assuming it works.
- **Negating for a descending engine puts dorang's whole scale on the non-positive half-line.**
  On a *shared* engine, any co-tenant sending a naive positive priority then outranks all dorang
  traffic, realtime included.

### 20.2 A client cannot claim urgency

A client-supplied priority hint is **ignored by default**. Clamping it into a permitted range
was the earlier rule and it is not safe enough: priority is a claim on shared capacity, so if
callers may set it, every caller eventually sets the most urgent value — not maliciously, just
because it is free and appears to help. The scale then carries no information and the callers
who left it alone are the ones penalised. A clamp bounds how far the self-elevation goes; it
does not remove the incentive.

The asymmetry is deliberate: **an operator can grant urgency, a caller cannot claim it.** The
The grant is `capacity.principals.<id>.client_priority: allow` with a `range` of two priority
class names, and the hint travels in `X-Request-Priority`. Both halves are implemented: a
principal with no grant has its hint dropped and the drop reported.

Dropping the hint is reported in `x-dorang-dropped-params` rather than being silent.

---

## 21a. `compat`

The three places [COMPATIBILITY.md](COMPATIBILITY.md) names a divergence an operator gets to
choose. All three are documented there as operator-settable; until this section existed the
schema had no `compat:` block at all, so a file that set any of them failed to load with an
unknown-key error and the document was describing a knob that was not there.

```yaml
compat:
  legacy_headers: false         # mirror the reference proxy's response header names
  usage_chunk_choices: stub     # stub | empty
  anthropic_total_tokens: true  # true | false
```

| Key | Type | Default | What it does | What breaks if it is wrong |
|---|---|---|---|---|
| `legacy_headers` | bool | `false` | Adds the reference proxy's response header spellings **alongside** dorang's own (COMPATIBILITY §7.7a lists them name by name). Nothing is renamed and nothing is removed | Nothing breaks; it emits another vendor's names, which is why it is off by default. Turn it on for a cutover and off once nothing reads them. The failure it prevents is silent: an exporter reading `x-litellm-response-cost` does not error when the header stops arriving, it reports **zero** |
| `usage_chunk_choices` | string | `stub` | COMPATIBILITY §3.3. `stub` is the reference proxy's `"choices":[{"index":0,"delta":{}}]`; `empty` is strict OpenAI's `[]`. **Both are served.** | A client written against the reference proxy reads `choices[0].delta` off the usage chunk and gets an index error on `empty`, so the default stays `stub`. Selecting `empty` also takes a same-family stream off the byte-relay fast path — on that path the usage chunk is the upstream's own bytes and dorang does not choose its shape, so the guarantee costs one decode per frame. Any value other than the two is refused at load as unknown, rather than falling back to the default |
| `anthropic_total_tokens` | bool | `true` | COMPATIBILITY §6.8. Reproduces the reference implementation's non-spec `usage.total_tokens` on non-streaming Anthropic responses. **Both values are served**; `false` is the strict vendor shape | Nothing breaks either way — the member is additive and every SDK in the family tolerates one it does not model. It applies to non-streaming answers only: a **streamed** message carries no `usage.total_tokens` at either setting, because that asymmetry *is* §6.8 and not a gap in the switch |

Two of the three used to be **refused** at their non-default value. The reason is worth keeping
because it is the point of §23: DESIGN §17.1 names "a setting that loads, validates and is read
by nothing" as this repository's dominant defect, and the two ways not to commit it are to wire
the setting or to refuse it. `internal/wire` had encoded both shapes of both pairs since it was
written and `internal/config` had parsed both spellings, but no expression related one to the
other — so the loader refused, and the refusal message named the missing hop.

That hop exists now: `backend.Call.UsageChunkChoices` and `backend.Call.AnthropicTotalTokens`,
filled per request from the reloadable dispatch state. All three keys are served at both of
their values, and the refusals are gone with the gap they described. What is still refused is a
value this schema does not know — `usage_chunk_choices: stubb` is a load error, because
defaulting a typo silently would hand an operator the shape they did not ask for.

---

## 22. Referential integrity

Every cross-reference is checked at load, by name, with the YAML path:

| Reference | Must resolve to |
|---|---|
| `providers[].capacity_group` | a key of `capacity.provider_groups` |
| `credentials[].provider` | a `providers[].name` |
| `credentials[].capacity_group` | a key of `capacity.credential_groups` |
| `capacity.models[].provider` | a `providers[].name` |
| `key_rotation.providers.<name>` | a `providers[].name` |
| `key_rotation.…keys[].id` | if it names a declared credential, that credential's provider must match |
| `key_rotation.…keys[].capacity_group` | a key of `capacity.credential_groups` |
| `models[].class` | a key of `classes`, **and** the model must be a member of it |
| `models[].deployments[].provider` | a `providers[].name` |
| `models[].deployments[].credentials[]` | a `credentials[].id` of the same provider |
| `aliases.<name>` | a `models[].name`, never another alias, never a model name itself |
| `classes.<name>[]` | a `models[].name`, no duplicates |
| `pricing.rules[].match.provider` / `.credential` | a declared provider / credential |
| `passthrough.routes[].provider` | a `providers[].name` |
| `priority_mapping.emit.<backend>.map.<class>` | a key of `priority_mapping.classes` |

---

## 23. Environment variables

| Variable | Read by | Default name settable in | Notes |
|---|---|---|---|
| `DORANG_CONFIG` | `dorang`, `dorangctl` | — | Supplies `--config`'s default. Setting it makes the path **explicit**, so a missing file becomes an error |
| `DORANG_MASTER_KEY` | server | `server.master_key_env` | The out-of-band administrative credential |
| `DORANG_KEY_PEPPER` | server, `dorangctl key` | `server.key_pepper_env` | HMAC pepper. Unset generates one beside the database — see §2 |
| `DORANG_DATABASE_URL` | server | `storage.postgres.url_env` | Only when `driver: postgres` |
| `DORANG_REDIS_URL` | server | `cluster.redis_url_env` | Only when `capacity_mode: shared-redis` |
| `DORANG_CATALOG_PATH` | server, `dorangctl` | — | PATH-separated model-catalog layers, applied **last**. A missing layer is an **error**, not a skip: a silently dropped layer is exactly the question the catalog exists to answer |

`dorangctl key create` and `dorangctl migrate` resolve the pepper the same way the server does.
A CLI that issued keys under a different pepper would issue keys the server cannot verify.

`DORANG_STATE_DIR` relocates every state path written with a leading `~` — the database, the
trace spool, the generated key pepper, batch blobs and the shadow report. The container image
sets it to the directory it declares as a volume. Without it, `~` is the serving user's home
directory, which under the image's `nonroot` user is `/home/nonroot`: outside the volume, in
the container's writable layer, and gone on restart. Losing the generated pepper makes every
api key issued under it unverifiable.

---

## 24. Keys the schema accepts that this build does not act on

Everything in this section validates, loads, and has no effect. It is listed by name because a
ceiling you believe you set and that does nothing is worse than no ceiling.

### 23.1 Accepted and inert

| Key | Status |
|---|---|
| `models[].deployments[].limits[]` with `metric: max_concurrent` or `max_queue` | Only `rpm` and `tpm` are consumed, and as a per-credential quota (§10.2) |
| `providers[].usage_probe` | §6.2's fetchers exist in `internal/probe`; nothing constructs one from configuration |
| `providers[].metrics.interval` | The endpoint and the enabled flag are read; the poll interval is not |
| `providers[].params.drop`, `.drop_unsupported` | §10.3's two knobs never reach the conversion path — only the kind's own capability set decides what is dropped |
| `routing.prefix.checkpoints` | The chain is cut logarithmically; `fixed` validates and selects nothing |
| `models[].deployments[].stream_timeout` | Only the non-stream timeout reaches the upstream call |
| `key_rotation.providers[].affinity_group` | Not read, and not validated either |
| `key_rotation.providers[].stickiness.scope` | Not read; the session key comes from `routing.sticky.key` |
| `cluster.redis_url_env` | Required for `capacity_mode: shared-redis` and no Redis client is constructed anywhere |
| `observability.otlp_endpoint` | No exporter is wired |
| `observability.log_level`, `.log_format` | No logger reads either; diagnostics go through the `Logf` hook the embedder supplies |

`internal/config/consumed_test.go` holds this list as executable state rather than prose:
adding a setting with no consumer fails the build, and so does wiring one without striking it
from the list. It cannot see the four rows whose Go field name is too common to search for
(`interval`, `drop`, `stream_timeout`), which is why they are still written down here.

### 23.1a Closed since the last pass

Everything below used to be in the table above.

| Key | What it does now |
|---|---|
| `capacity.*.max_queue`, `capacity.principals.<id>.max_queue`, `.max_queue_wait` | Wired. `max_queue` bounds the wait queue per axis and refuses past it; `max_queue_wait` bounds how long one of a principal's requests may wait, and a **pinned** request is the one that uses it — an unpinned one spills or falls back rather than queueing (§7.4a2, §7.6). Batch is exempt: it is the work that is meant to wait (§11.1) |
| `capacity.*.rpm`, `.tpm` on every group, on `global`, on `models[]` and on `principals` | **Refused**, with the working home named. A capacity axis counts concurrent reservations, released when a request finishes; a rate needs a time window, which `internal/quota` owns. Put a per-deployment rate on `models[].deployments[].limits[]` and a per-caller rate on the api key's own `rpm_limit`/`tpm_limit` |
| `key_rotation.strategy` | Wired. All four names now choose the preferred credential; a credential pin and a sticky entry still outrank the rotation |
| `observability.prometheus` | Wired. `false` removes the `/metrics` route, which then answers 501 like any other unserved route |
| `observability.metrics.public` | New. `/metrics` authenticates and requires the master credential by default; this opens it deliberately |
| `capacity.principals.<id>.client_priority` + `range` | New, and §10.5's own example now loads |
| `pricing.rules[].class: notional_rate` with `source` and `as_of` | New. §8.5's class is no longer catalog-only |
| `pricing.rules[].rates.cache_read` | New spelling, alongside `cached_read`; both mean the same component in both files |
| `pricing.rules[].rates.images` | Now a **load** error rather than an assembly error — it used to pass `config lint` and then stop the server from starting |
| `key_ref` | **Refused**, naming `key_env` and `key_file` |
| `providers[].prefix_ttl`, `models[].deployments[].prefix_ttl` | New. The affinity lifetime is per backend, because that is what it models; `until_evicted` is the honest value for vLLM and SGLang |
| `DORANG_STATE_DIR` | Wired. A leading `~` in any state path resolves to it, so the image's defaults land inside its declared volume |
| `compat.legacy_headers` | New, and wired end to end. It had existed as a `server.Options` field with a working consumer that **no configuration could reach** — `internal/app` never set it — so COMPATIBILITY §7.7's mirroring claim was true of the code and false of every deployment. §21a |
| `compat.usage_chunk_choices` | Wired. `empty` reaches `openai.StreamConfig.UsageChunkChoices` through `backend.Call`, so a streaming usage chunk now carries strict OpenAI's `"choices":[]` when it is asked for. It had been **refused** at that value: the encoder had emitted both shapes since it was written and no configuration could select one. Selecting it also takes a same-family stream off the byte-relay fast path, where the usage chunk is the upstream's bytes rather than dorang's. §21a, COMPATIBILITY §3.3 |
| `compat.anthropic_total_tokens` | Wired. `false` reaches `anthropic.ResponseOptions.TotalTokens`, so a non-streaming Anthropic answer omits the non-spec `usage.total_tokens`. Also **refused** at that value before. Streaming is unchanged at either setting, deliberately — the asymmetry is §6.8 itself. §21a |

### 23.2 Designed and not in the schema at all

| Design section | What is missing |
|---|---|
| §6.1 `quotas:` — rolling `5h`/`daily`/`weekly`/`monthly` windows over `cost_usd` or `tokens_total`, with `on_exhaust` | There is **no top-level `quotas:` block**. The only quota rules this build creates are rolling-minute request and token counters derived from `deployments[].limits[].rpm`/`.tpm` |
| §6.4 `budget:` — a `period`/`limit_usd`/`on_exceed` block | There is **no top-level `budget:` block**. Budgets are per-API-key, set with `dorangctl key create --budget-usd`, and enforced through the durable ledger |
| §7.4a2 `stickiness.pin_on_state` | Not in the schema — and correctly so: the pin is inferred from the request, not configured (§7.1) |
| §11.5 "Lua hooks" | The hook points, ceilings, fail-open/fail-closed rule and secret-free views are built; **there is no Lua interpreter** and a `.lua` file is a load error. §16 explains why, and what runs instead |

### 23.3 What `--check` does not catch

- A `key_env` variable that is set on the linting machine and unset on the serving one, or the
  reverse (secret failures are downgraded to warnings under `--check`).
- A `storage.postgres.url_env` whose *contents* are wrong.
- Nothing about pricing that is not a syntax or a naming error: `rates.images` and a
  `notional_rate` rule missing its provenance are both refused at load now.
- A `passthrough.routes[]` entry whose provider has no `base_url`, which is dropped silently.
- Anything about whether the upstreams exist, answer, or speak the protocol their `kind` claims.

---

## 25. Three worked configurations

The same binary and the same schema across the whole range. You scale by changing
configuration, not by changing deployment model.

| | Notebook | Team | Enterprise |
|---|---|---|---|
| Storage | SQLite, embedded | PostgreSQL | PostgreSQL, partitioned |
| Coordination | none | none | Redis or PostgreSQL leases |
| Nodes | 1 | 1–2 | N, leader-elected maintenance |
| Capacity accuracy | exact | exact | exact (shared) or bounded overshoot (leased) |
| Telemetry | in-process, sampled | full ledger | ledger + rollups + retention |
| **Required dependencies** | **0** | 1 | 2 |
| Target throughput | 100s req/s | 1000s req/s | 10k+ req/s per node |

### 24.1 Notebook — zero dependencies

One process, one file, nothing installed. The binary is static (the SQLite driver is pure Go),
so this runs on `scratch`.

```yaml
version: 1

server:
  listen: "127.0.0.1:4100"
  env: development            # a laptop, and nothing is committed from here
  request_timeout: 600s

storage:
  driver: sqlite
  sqlite: {path: ~/.dorang/dorang.db}

providers:
  - name: local-vllm
    kind: vllm
    base_url: http://127.0.0.1:8000/v1
    timeout: 300s
    max_concurrency: 8

credentials:
  - {id: local, provider: local-vllm, key_env: LOCAL_VLLM_KEY}

capacity:
  principals: {default: {max_concurrent: 8}}

models:
  - name: local-large
    class: chat-large
    strategy: [prefix_sticky, least_busy]
    deployments:
      - {provider: local-vllm, upstream_model: my-model, credentials: [local]}

classes: {chat-large: [local-large]}

metering:
  trace: {store_messages: truncated, sample_rate: 1.0}
  spool: {dir: ~/.dorang/spool, max_bytes: 256MiB}

observability: {log_level: debug, log_format: text}
```

Notes that matter at this tier:

- `env: development` is what lets you write `key: sk-…` inline while experimenting. Do not carry
  this file to a server; §1.2 exists for the file, not for the machine.
- `DORANG_KEY_PEPPER` unset is fine here: a pepper is generated beside `dorang.db` and the two
  travel together. Back them up together or every issued key becomes unverifiable.
- `sample_rate: 1.0` keeps everything. At ~1 req/s that is ~44 MB/day of excerpts.
- Set the vLLM flags before you trust anything the gateway reports —
  `--enable-prompt-tokens-details` in particular, without which cached prompt tokens are priced
  at full rate. [VLLM.md](VLLM.md) §5.

### 24.2 Team — one dependency

PostgreSQL, one or two nodes, one shared account pool, real cost accounting. Still no
coordination service: `cluster.enabled: false` is correct here as long as the *second* node is a
standby rather than a peer serving traffic — two peers with `local` accounting is the overshoot
of §1.1 arrived at by omission.

```yaml
version: 1

server:
  listen: ":4100"
  env: production
  master_key_env: DORANG_MASTER_KEY
  key_pepper_env: DORANG_KEY_PEPPER      # MUST be set: two nodes, one key database
  request_timeout: 600s
  shutdown_grace: 45s                    # longer than p99, so a rolling restart is quiet

storage:
  driver: postgres
  postgres: {url_env: DORANG_DATABASE_URL, max_conns: 32}

providers:
  - name: cloud-a
    kind: openai
    base_url: https://api.example-cloud-a.invalid/v1
    timeout: 120s
    max_concurrency: 24
    capacity_group: cloud-a-pool
    usage_probe: {enabled: true, fetcher: openai, interval: 60s}
  - name: plan-a
    kind: glm
    base_url: https://api.example-plan-a.invalid
    timeout: 180s
    max_concurrency: 14
    capacity_group: plan-a-pool

credentials:
  - {id: acct-1,   provider: cloud-a, key_env: CLOUD_A_KEY_1, capacity_group: acct-1}
  - {id: acct-2,   provider: cloud-a, key_env: CLOUD_A_KEY_2, capacity_group: acct-2}
  - {id: plan-a-1, provider: plan-a,  key_file: /etc/dorang/secrets/plan-a.key,
     capacity_group: plan-a-account}

capacity:
  provider_groups:
    cloud-a-pool: {max_concurrency: 6}     # both accounts together
    plan-a-pool:  {max_concurrency: 14}
  credential_groups:
    acct-1: {max_concurrency: 3}           # per account, ALL models
    acct-2: {max_concurrency: 3}
    plan-a-account: {max_concurrency: 7}
  models:
    - {provider: plan-a, model: model-x, max_concurrency: 7}   # per (key, MODEL)
    - {provider: plan-a, model: model-y, max_concurrency: 7}   # so both together = 14
  principals:
    default: {max_concurrent: 16}
  global: {max_concurrency: 64}
  interactive_reserve: 0.3

key_rotation:
  providers:
    cloud-a:
      stickiness: {scope: session, on_capacity: spill}
      keys:
        - {id: acct-1, key_env: CLOUD_A_KEY_1, max_concurrency: 3, capacity_group: acct-1}
        - {id: acct-2, key_env: CLOUD_A_KEY_2, max_concurrency: 3, capacity_group: acct-2}

models:
  - name: model-x
    class: chat-large
    strategy: [prefix_sticky, lowest_cost, least_busy]
    deployments:
      - {provider: plan-a,  upstream_model: model-x,       credentials: [plan-a-1], weight: 10, priority: 0}
      - {provider: cloud-a, upstream_model: model-x:cloud, credentials: [acct-1, acct-2],
         weight: 5, priority: 1, limits: [{metric: rpm, value: 600}, {metric: tpm, value: 400000}]}
  - name: model-y
    class: chat-small
    deployments:
      - {provider: plan-a, upstream_model: model-y, credentials: [plan-a-1]}

aliases: {model-large: model-x, model-small: model-y}
classes: {chat-large: [model-x], chat-small: [model-y]}

pricing:
  catalog: /etc/dorang/pricing.yaml
  currency: USD

metering:
  trace: {store_messages: truncated, truncate_chars: 512, sample_rate: 0.2, daily_byte_budget: 4GiB}
  spool: {dir: /var/lib/dorang/spool, max_bytes: 2GiB}
  flush_interval: 250ms

observability: {log_level: info, log_format: json}
```

Notes that matter at this tier:

- **Set `DORANG_KEY_PEPPER` explicitly.** With two nodes and a generated pepper, each node
  generates its own and the keys one issues are unverifiable by the other.
- `sample_rate: 0.2` at ~50 req/s keeps ~440 MB/day of excerpts instead of ~2.2 GB.
- The `plan-a` numbers are the shape the axes exist for: 7 per model *and* 7 per account means
  two models can reach 14 concurrent on one key while the account ceiling still holds at the
  provider-group level.
- `usage_probe` on `cloud-a` is what makes quota reflect traffic that did not go through dorang
  (§6.1). It is also what makes expiring-quota reporting meaningful at all.

### 24.3 Enterprise — two dependencies

N nodes, exact shared capacity, partitioned ledger, hard sampling.

```yaml
version: 1

server:
  listen: ":4100"
  env: production
  master_key_env: DORANG_MASTER_KEY
  key_pepper_env: DORANG_KEY_PEPPER
  request_timeout: 600s
  shutdown_grace: 60s

storage:
  driver: postgres
  postgres: {url_env: DORANG_DATABASE_URL, max_conns: 64}

cluster:
  enabled: true
  node_id: ""                    # set per node from the orchestrator, e.g. the pod name
  redis_url_env: DORANG_REDIS_URL
  capacity_mode: shared-redis    # exact; +1 RTT on the hot path, deliberately
  min_leasable: 16

providers:
  - name: fleet-vllm
    kind: vllm
    base_url: http://vllm.internal:8000/v1
    timeout: 300s
    max_concurrency: 256
    capacity_group: fleet
    metrics: {enabled: true, endpoint: http://vllm.internal:8000/metrics, interval: 15s}
  - name: cloud-a
    kind: openai
    base_url: https://api.example-cloud-a.invalid/v1
    timeout: 120s
    max_concurrency: 128
    capacity_group: cloud
    usage_probe: {enabled: true, fetcher: openai, interval: 60s}

credentials:
  - {id: fleet-1, provider: fleet-vllm, key_file: /run/secrets/fleet.key, capacity_group: fleet-acct}
  - {id: acct-1,  provider: cloud-a,    key_env: CLOUD_A_KEY_1, capacity_group: acct-1}
  - {id: acct-2,  provider: cloud-a,    key_env: CLOUD_A_KEY_2, capacity_group: acct-2}

capacity:
  provider_groups: {fleet: {max_concurrency: 256}, cloud: {max_concurrency: 96}}
  credential_groups:
    fleet-acct: {max_concurrency: 256}
    acct-1: {max_concurrency: 48}
    acct-2: {max_concurrency: 48}
  principals: {default: {max_concurrent: 32}}
  global: {max_concurrency: 512}
  interactive_reserve: 0.3

models:
  - name: chat-primary
    class: chat-large
    strategy: [prefix_sticky, lowest_cost, least_busy]
    deployments:
      - {provider: fleet-vllm, upstream_model: my-70b, credentials: [fleet-1], weight: 10, priority: 0}
      - {provider: cloud-a, upstream_model: model-x, credentials: [acct-1, acct-2],
         weight: 3, priority: 1, limits: [{metric: rpm, value: 3000}, {metric: tpm, value: 4000000}]}

classes: {chat-large: [chat-primary]}

pricing: {catalog: /etc/dorang/pricing.yaml, currency: USD}

metering:
  trace: {store_messages: hash, sample_rate: 0.01, daily_byte_budget: 8GiB}
  spool: {dir: /var/lib/dorang/spool, max_bytes: 8GiB}
  flush_interval: 250ms

observability: {log_level: warn, log_format: json}

priority_mapping:
  classes: {realtime: 0, interactive: 2, batch: 10}
  emit:
    vllm: {field: priority}
    header: X-Request-Priority
```

Notes that matter at this tier:

- **`node_id` must be distinct and stable per node.** An empty value derives one per *process*,
  so a restarted node cannot reclaim its own leases and waits for them to expire instead.
- `capacity_mode: shared-redis` costs one round trip on the hot path, against a p99 target of
  5 ms for that profile. It is the price of exact accounting; `leased` trades it for a published
  overshoot of `block × (nodes − 1)`.
- `min_leasable: 16` is inert under `shared-redis` and becomes load-bearing the moment you
  switch to `leased` — at which point every `max_concurrency` under 16 in this file is refused.
- `store_messages: hash` with `sample_rate: 0.01` at ~2 k req/s keeps roughly 880 MB/day of the
  ~88 GB/day that full excerpts would cost.
- `key_ref` is **refused by this build**: a credential declared that way would
  load and have no usable secret until an external resolver exists. Use `key_env` or `key_file`
  today.
- The vLLM `metrics` scrape is only useful if `--disable-log-stats` is **not** set. Without that,
  `/metrics` returns 200 with zero series and `least_busy` reads every backend as idle
  ([VLLM.md](VLLM.md) §3.1).

---

## See also

- [OPERATIONS.md](OPERATIONS.md) — installing, running, reading the metrics, and what to do when
  something is wrong.
- [MIGRATION.md](MIGRATION.md) — importing an incumbent gateway's configuration and credentials,
  and the shadow-comparison cutover.
- [DESIGN.md](DESIGN.md) §4 — the schema's rationale.
- [VLLM.md](VLLM.md) §5 and [SGLANG.md](SGLANG.md) §8 — the flags a self-hosted backend needs
  before any of the above means what it says.
