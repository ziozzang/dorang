# dorang in front of an incumbent LiteLLM

A compose project that stands dorang up **beside** a live LiteLLM and routes to it
as a single upstream provider. It adds nothing to the incumbent and removes
nothing from it: the model set, the provider credentials and every existing
consumer stay exactly where they are, and dorang contributes only its own auth,
budgets, metering and routing on top.

That arrangement is circular on purpose. Any breakage is attributable to dorang,
and the fallback is to stop using it.

It also sidesteps the blocker that stops a direct cutover. With
`STORE_MODEL_IN_DB=True` the incumbent's `proxy.yaml` holds no `model_list` at
all — the models live in `LiteLLM_ProxyModelTable` and their upstream credentials
are database rows, not a file anyone can translate. Fronting the proxy needs none
of that data. Going direct does, and is a separate exercise.

Everything this creates is removable by stopping one compose project.

---

## What it does not touch

Read this before running anything. The incumbent is load-bearing.

- It never modifies, restarts or reconfigures the LiteLLM containers, their
  compose file, or their database. The only access it needs to the incumbent's
  database is a **read-only** `SELECT` during the key import.
- It joins the incumbent's docker network as **`external: true`**, so
  `docker compose down` cannot remove it.
- It repoints no existing consumer. Moving one is a separate, deliberate act.
- Its own state is two named volumes and one directory.

---

## Layout

Keep the deployment **outside** the dorang checkout. It holds credentials; the
checkout holds the template and this runbook.

```
<deployment-dir>/
  docker-compose.yml       copied from this directory
  .env                     credentials — mode 0600, never committed
  config/dorang.yaml       from config.template.yaml, models generated
  src/                     the build context (see step 2)
```

---

## 1. Check the ground

```sh
# The upstream, its network, and the port dorang wants.
docker ps --filter name=litellm
docker network inspect litellm_default --format '{{range .Containers}}{{.Name}} {{end}}'
ss -lnt | grep -w 4100 || echo "4100 free"
```

`4100` is dorang's default. If it is taken, change the published port in
`docker-compose.yml` **and** `server.listen` in the config.

## 2. Build context

Build from the committed tree rather than a working directory. The repository
carries no `.dockerignore`, so a `COPY . .` from a live checkout drags in `.git`,
any local build artifacts and any agent scratch directories — into an
intermediate build layer that persists in the local builder cache.

```sh
mkdir -p src && git -C <path-to-dorang-checkout> archive --format=tar HEAD | tar -x -C src
```

## 3. Secrets

```sh
umask 077
cat > .env <<'EOF'
DORANG_SOURCE=./src
DORANG_VERSION=dev
DORANG_MASTER_KEY=
DORANG_KEY_PEPPER=
DORANG_PG_PASSWORD=
DORANG_LITELLM_KEY=
DORANG_DATABASE_URL=
EOF
chmod 600 .env
```

Fill it in:

| Variable | What it is |
|---|---|
| `DORANG_SOURCE` | build context from step 2 |
| `DORANG_MASTER_KEY` | dorang's administrative credential. Compared **out of band** — it is never a row in the database, and no import can create one. Generate it: `openssl rand -hex 24` |
| `DORANG_KEY_PEPPER` | HMAC pepper for `dorang_v1` key hashing. **Losing it invalidates every natively issued key.** Generate once: `openssl rand -hex 32` |
| `DORANG_PG_PASSWORD` | dorang's own PostgreSQL password |
| `DORANG_LITELLM_KEY` | a virtual key **issued by the incumbent**, used as dorang's upstream credential. Prefer one scoped to the models dorang serves over the incumbent's master key — dorang does not need administrative rights upstream |
| `DORANG_DATABASE_URL` | `postgres://dorang:<DORANG_PG_PASSWORD>@dorang-db:5432/dorang?sslmode=disable` |

## 4. Configuration

```sh
mkdir -p config && cp <checkout>/deploy/litellm-front/config.template.yaml config/dorang.yaml
```

Then edit the three `REPLACE_ME` markers — `auth.legacy.until`, and the example
pricing rule — and generate the model list from the upstream's own answer:

```sh
./gen-models.sh http://127.0.0.1:4000/v1 "$DORANG_LITELLM_KEY" config/dorang.yaml
```

Build the image and validate the configuration before starting anything:

```sh
docker compose build
docker run --rm -v "$PWD/config/dorang.yaml:/tmp/c.yaml:ro" --env-file .env \
  dorang:local --check --config /tmp/c.yaml
```

`--check` validates and exits without touching the database, so it is safe to run
against a configuration you have not committed to yet.

### Pricing is per **million** tokens

`rates: {input: …, output: …}` carry the `per_1m_tokens` unit. A model that costs
$2.50 per million input tokens is `"2.50"`, not `"0.0000025"`.

A per-token value is **not rejected**. It prices every request to zero,
`/spend/calculate` still reports the rule as matched with `"missing": false`, and
the ledger fills with a spend of 0. Check one request before trusting the
numbers:

```sh
curl -s -X POST localhost:4100/spend/calculate -H "Authorization: Bearer $DORANG_MASTER_KEY" \
  -H 'Content-Type: application/json' -d '{"model":"<id>","prompt_tokens":1000000,"completion_tokens":0}'
```

The `total` should be the model's per-million input price. If it is `0`, the
rates are off by a factor of a million.

## 5. Volume ownership — nothing to do here any more

The image runs as `nonroot` (uid 65532) and the Dockerfile declares
`VOLUME /var/lib/dorang`. That combination used to crash-loop on the first start
with `mkdir /var/lib/dorang/.dorang: permission denied`: Docker created the
volume owned by `root`, the process could not create its state directory inside
it, and the image is distroless — no shell, nothing to fix it with from the
inside.

The image now ships `/var/lib/dorang` **already owned by 65532**, and Docker
seeds a fresh named volume from the image with its ownership, so the `state:`
volume this compose file declares comes out owned by the serving user. The
out-of-band `chown` this section used to prescribe is no longer needed; if you
ran it on an existing volume it stays correct and costs nothing.

One case still needs a `chown` and it is not this one: a **bind mount**
(`-v /srv/dorang:/var/lib/dorang`) keeps the host directory's ownership and
Docker copies nothing into it. If you swap the named volume for a host path,
`chown 65532:65532` it first. See CONFIG §23.

## 6. Start

```sh
docker compose up -d
docker compose ps
curl -s localhost:4100/health/readiness
curl -s -H "Authorization: Bearer $DORANG_MASTER_KEY" localhost:4100/v1/models \
  | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["data"]), "models")'
```

The model count must equal what the upstream returns for the same key. If it does
not, `gen-models.sh` ran against a key with a narrower allow-list than the one in
`.env`.

`/metrics` is **authenticated** — it carries per-key spend, per-credential quota
state and the whole model list, so it is not on the unauthenticated probe port:

```sh
curl -s -H "Authorization: Bearer $DORANG_MASTER_KEY" localhost:4100/metrics | head
```

---

## 7. Importing the incumbent's keys

`dorangctl import keys` reads the incumbent's `LiteLLM_VerificationToken` table
and never needs a plaintext secret: the incumbent stores `sha256(token)`, and
dorang's index key is defined as the first half of exactly that digest. Imported
keys land under `hash_scheme legacy_sha256` and upgrade to `dorang_v1` the first
time each is presented.

It reports by default and writes only with `--commit`. **Read the report.**

```sh
DSN='postgres://<user>:<password>@litellm_db:5432/litellm?sslmode=disable'

docker compose exec -T dorang /usr/local/bin/dorangctl import keys \
  --config /etc/dorang/config.yaml --from "$DSN"          # reports, writes nothing

docker compose exec -T dorang /usr/local/bin/dorangctl import keys \
  --config /etc/dorang/config.yaml --from "$DSN" --commit
```

`--from` is the only argument that routinely carries a password. It is kept out
of error messages by design, but it is still on a command line: prefer supplying
it from a variable, and clear your history.

### What the report will not tell you

The import carries the incumbent's authorization columns verbatim. Three of them
do not mean the same thing on the other side, and the report is silent about all
three. **Check each one before you point a consumer at an imported key.**

```sh
docker compose exec -T dorang-db psql -U dorang -d dorang -c \
  "SELECT key_alias, models, allowed_routes, object_permission_id
     FROM api_keys WHERE source='litellm'
    ORDER BY key_alias;"
```

**1. Route-group names become route allow-lists that match nothing.**
The incumbent's `allowed_routes` may hold a *group name* such as
`llm_api_routes`, which it expands internally to a set of paths. dorang treats
the column as a list of literal paths, so the entry matches no request and the
key is refused **every route** with `403 route_not_allowed` — including
`/v1/models`. It authenticates fine; it can do nothing. Clear the list, or
replace it with the real paths:

```sh
curl -s -X POST localhost:4100/key/update -H "Authorization: Bearer $DORANG_MASTER_KEY" \
  -H 'Content-Type: application/json' -d '{"key_id":"<id>","allowed_routes":[]}'
```

**2. Model sentinels become model allow-lists that match nothing.**
`models: {all-team-models}` and similar sentinels are the incumbent's way of
saying "whatever the team has". dorang reads them as a literal model named
`all-team-models`, so the key is refused every model. Same direction of failure —
closed — and the same fix.

**3. `object_permission_id` is carried and never enforced.**
If the incumbent kept a key's model restriction in `LiteLLM_ObjectPermissionTable`
rather than in the key's own `models` column, dorang stores the id and consults
it **nowhere**. The key arrives with an empty `models` list, and an empty list
allows everything. This is the one that fails **open**: a key can silently gain
access it did not have. Resolve it by hand before committing:

```sh
# on the incumbent's database, read-only
psql -c "SELECT object_permission_id, models FROM \"LiteLLM_ObjectPermissionTable\";"
```

For every imported key with a non-null `object_permission_id`, copy that row's
`models` into the dorang key's own allow-list, or refuse the key.

### Keys with a team are skipped, and that is correct

`--on-missing-team` defaults to `skip`: a key whose team is not present in dorang
is refused rather than imported, because importing it would leave it with no team
budget, no team rate limit and no team blocked flag — it would fail open. On a
deployment whose keys mostly carry a team, expect most of them to skip.

`--on-missing-team orphan` imports them anyway with the team dropped. Do not
reach for it to make the numbers look better. It is for an operator who is
importing teams in a later pass and has accepted the window.

Expect the report to name the incumbent's own console team (often
`litellm-dashboard`) among the missing ones. Those rows are UI session keys, not
integrations.

---

## 8. Operating it

| Action | Command |
|---|---|
| Reload configuration | `docker compose kill -s HUP dorang` — hot, no restart, in-flight requests keep their snapshot |
| Issue a key | `docker compose exec -T dorang /usr/local/bin/dorangctl key create --config /etc/dorang/config.yaml --alias <name> --budget-usd <n>` |
| Read the ledger | `GET /spend/logs` — **requires** `start_date`, `end_date` **and** a filter (`key_id`, `team_id`, `trace_id`, `tag` or `errors_only`). An unbounded or unfiltered query is refused, which differs from the incumbent |
| Stop | `docker compose down` — leaves the data |
| Remove entirely | `docker compose down -v` — removes both volumes |

Nothing outside this directory and its two volumes is affected by any of these.

### Known behaviour worth having in your head

- **Readiness does not reflect store reachability.** If dorang's own PostgreSQL
  goes away, `/health/readiness` keeps answering `ready`. Requests from callers
  whose principal is already cached keep succeeding; a caller whose key has not
  been seen recently gets a `503`. A balancer polling readiness will keep routing
  to a node that cannot authenticate anyone new. Recovery needs no restart:
  reconnection is transparent and metering buffered during the outage is written
  when the store returns.
- **Cost is computed once, at request time.** Fixing a wrong price does not
  reprice rows already in the ledger.
- **An unpriced model is not an error.** It meters tokens and reports a spend of
  zero, and logs `no marginal price rule matched` once per model.

---

## 9. Moving a consumer onto it

One at a time, starting with something you own.

1. Copy the consumer's configuration. Do not edit the live one.
2. Point `base_url` at `http://<host>:4100/v1` and set its key to one dorang
   issued, or an imported one you have checked against §7.
3. Run real work through it — several turns, tool calls, streaming, a long
   context — not a smoke test.
4. Compare `/spend/logs` against what the client was told, over the whole
   session, before trusting the ledger.

Reverting is repointing `base_url` back. Nothing on the incumbent changed.
