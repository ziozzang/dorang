# Changelog

Notable changes. Dates are the day the work landed; the git history is the
record of record.

The format is not "features added". This project's history is mostly *defects
found in its own claims*, and the entries that matter are the ones where a
published number, a documented control or a passing test turned out not to mean
what it said. Those are kept rather than tidied away — see `docs/DESIGN.md`
§17.1, which is the ledger of the patterns behind them.

## Unreleased

### `/v1/responses` learned to stream, and the 501 it used to answer with

The refusal existed because the Responses event stream is a different protocol
from chat SSE — a typed event tree, not a framing of the same chunks — and
answering the non-streaming body to a client that asked for events would have
been a half-working endpoint. The upstream half (the decoder) had existed for
a while; what was missing was the client-facing encoder, which turned out to
be one emitter plus one sink case, not a new pipeline.

- **The event tree is emitted, with the part lifecycle.** dorang's own decoder
  ignores `content_part.added`/`done` — `item.done` carries the assembled body
  — but the deployed protocol's clients do not: measured against a live codex,
  which logs a text delta with no open part as a protocol violation. The
  frames are emitted for the protocol as deployed, not as dorang reads it.
- **The streamed terminal and the buffered body are one rendering** — the same
  encoder, a test pinning the two byte-equal — after the first draft let two
  text fragments of one run complete as two parts.
- **The store's "before the client has the id" invariant cannot hold for a
  stream** (the `created` event carries the id), so it is restated as what it
  protected: the row exists before the handler returns, from the terminal
  event's own object, and `previous_response_id` never observes a gap. A store
  failure is logged rather than failed — the answer is served and cannot be
  retracted — and the id 404s in the TTL-expired shape. The one condition that
  would fail after the fact (a store-keeping stream with no store) is refused
  at decode, before a byte is written.
- **Verified live**, not only in suites: dev instance → production dorang →
  ollama cloud `gpt-oss:120b`, a text turn and a real tool-call turn
  (argument deltas, done with the assembled arguments), and codex itself
  streaming through the new surface.
- COMPATIBILITY gains §6a (the streamed event-tree contract, eight rows) and
  a Responses row in §11.1a's mid-stream error framing; the DESIGN §0.2
  citations for the named-501 rule were stale and now name §0.3.

- **A non-function tool never reached the chat surface — until it did.** The
  chat encoder forwarded a Responses caller's tool `type` verbatim, so codex's
  `namespace` containers and `web_search` declaration went out as
  `{"type":"namespace"}` and were refused by the first strict backend that saw
  them (z.ai 1214 "tools[N].type: type is illegal" — which is what kept a
  streamed codex turn off every chat-shaped backend, found while verifying the
  streaming deployment live). The declaration is now dropped and named on the
  loss ledger: a parameter fact, not a structural downgrade.

### The operator dashboard: one screen worth reading, one dead, one that lied

Opened against a live ledger, all three screens answered `200`. That was the
whole problem.

- **`/ui/keys` showed `spend: 0` for every key** while `/key/list`, from the
  same store, reported the ledger's figure for the same keys. The fix for this
  exact defect already existed — `hydrateSpend`, which re-points the field at
  §9.4's rollup — and it was a method on the JSON request type, so the UI could
  not reach it. It now hangs off the API and takes a context, and one function
  produces the number for both surfaces. The test reads the rendered `<td>` and
  the `/key/list` JSON and compares the **digits**; asserting that a handler
  returned 200, or that an enrichment ran, is what let the column ship.

- **The footer asserted single-sourcing on the two pages that were not
  single-sourced.** "Every figure comes from the same engine the API and the CLI
  use, so there is one answer rather than three (DESIGN §8.4)" was printed by
  the shared layout — under the spend column above, and under a page that had
  produced no figure at all. That sentence is the strongest claim on the page
  and it stops a reader from cross-checking, so it now belongs to the screen
  that earns it, and screens with no figures say nothing.

- **`/ui/models` answered 501 in every deployment**, and all three pages linked
  to it. `/model/*` staying 501 is still right — the routing table is compiled
  from configuration, so a `deployments` row would route nothing differently —
  but that argument is about *writes*. The read-only screen is served from the
  routing table the process compiled, with the same deployment ids the ledger
  records, and the page says which table it is showing. The **navigation is now
  built from what the process can serve this viewer**: a link that answers 501
  on every click does not tell an operator that a dependency is optional.

- **Two of the six tiles on `/ui/usage` could never hold a value.** The rollup
  adapter hard-wired `NotionalKnown: false` with a comment that the rollups had
  no notional column; they have had one since migration 0002, and what was
  missing was a **writer**. §8.5's list-rate figure was computed for every
  priced request and published in `x-dorang-notional-usd`, and it stopped at the
  meter boundary. It now travels to the ledger row and to all three
  materializations. Migration 0008 adds the two counts a *sum* cannot carry —
  requests priced at list rate, and requests that reached pricing with no
  notional rule — because a total that is short by an unknown amount is exactly
  the flattering answer §8.5 rule 5 forbids. Buckets written before the counts
  existed report `unavailable`, which is the truth about them.

- **An admin UI session outlived the credential that created it.** Revoking the
  key left an open tab administering the deployment for up to the session TTL —
  an hour, against the 270 ms bound §11.2c publishes for the same revocation
  everywhere else, on the surface the key was revoked *from*. The session now
  carries the key id and re-checks it on every request: blocked, deleted,
  pended, expired or moved to another team ends it on the next click. A role
  revoked on the owning *user* is not on the key and still waits for the TTL,
  which is stated rather than implied. The session cookie is also marked
  `Secure` when the **browser's** hop was TLS, not only when TLS terminated at
  dorang — behind the reverse proxy OPERATIONS recommends, it was shipping
  without it.

- Two adjacent findings, from looking at the rest of the same screen: a pended
  key rendered as **active** (§11.6's refusal, shown as service), and the app's
  key adapter dropped `tier`, `pended_at` and `pend_reason` on the way out of
  the store, so `/key/info` reported a fixed `"tier": ""` and `"pended": false`
  for every key on every deployment. Both are read-path fixes. `/key/update`
  still cannot *write* a tier — `UpdateAPIKey` has no such column in its
  statement — and that is left named rather than half-built.

### Utilization pricing, and two things deliberately not built

- **A `marginal_usage` rule may scale its rate by how contended the backend was**
  (`utilization: {slope, max_multiplier}`, DESIGN §8.6). Off unless a rate card asks
  for it; there is no global switch, because a price that varies is a business
  decision. The factor's range is `[1.0, max_multiplier]`, `max_multiplier` is
  required, and a catalog declaring more than **4.000x** fails to load — a bound on
  what a mis-scaled metric can do to an invoice, not an opinion about prices. The
  factor, the ceiling and the provenance travel on the response beside
  `x-dorang-cost-usd` and into three ledger columns.

- **vLLM emits `kv_cache_utilization: 0.0` for two different facts**, and one of them
  is not "idle". Read out of vLLM's own source: the field defaults to `0.0` when the
  engine had no metrics for the request, so a well-formed load header claims an empty
  machine. This is VLLM.md §3.1's `--disable-log-stats` trap wearing a disguise, and
  it is *worse* than the scrape it was supposed to replace: a scrape fails visibly
  with zero series, this succeeds and is wrong. dorang refuses an exact zero. The
  refusal is free — at zero occupancy the factor is 1.0 either way — so nobody pays a
  different amount and the row says "not observed" instead of a measurement nobody
  made.

- **No `/metrics` scraper, and R17 stays refused** — for a pricing reason. A gauge has
  an *age*, not an interval, so pricing a request with one means picking an instant
  that is not a property of that request; and a poll is node-local, so two nodes at
  different phases of one interval bill the same request differently into one shared
  ledger. The same signal is fine for routing, where a wrong decision is cheap and
  self-correcting. The cost of that call is stated rather than hidden: **every
  streamed answer is charged the base rate**, which in an agent deployment is most
  traffic.

- **No damping and no jitter**, though `quota.Ranker` has both written for this exact
  shape. They are not needed: a routing quote carries no observation, because the
  occupancy a request will run at is not knowable before it runs — so the loop between
  "route away from load" and "charge more for load" is open, and a test asserts the
  routing trajectory is identical tick-for-tick with the feature on and off. Building
  the loop deliberately gives 137 backend switches over 200 ticks against 0. Damping
  is refused on its own merits too: an EWMA is node-local state, and a price computed
  from node-local state is the scraper's objection let back in through the door.
  A jittered *ranking* costs nobody anything; a jittered *price* is two bills for two
  identical requests, keyed on a hash of the node id.

### The import moved data and not meaning, and a blacklist let three axes through

Found by dogfooding: dorang now runs beside the operator's live LiteLLM,
fronting it, with its 54 keys imported.

- **Three of that gateway's authorization idioms survived the import as data
  rather than as meaning, and one of them failed OPEN.** A route *group*
  (`allowed_routes: {llm_api_routes}`) and a model sentinel
  (`models: {all-team-models}`) are instructions about a set; copied across as
  literal allow-list entries they match nothing, so the key is refused every
  route and every model. Those two fail closed and announce themselves.
  `object_permission_id` does not: it reached the store and the admin JSON and
  stopped there, never into `auth.Limits`, so a key whose model restriction
  lived in that table arrived with an **empty** allow-list — and empty allows
  everything. It did not fire only by luck, because the one key carrying it
  pointed at a row whose `models` was itself empty. Pointed at the other row,
  the import would have granted all 39 models with no line in the report. The
  table is now **read** and the two lists intersected — never unioned, never
  widened — and the four ways resolution can fail are each refused *by name*,
  including an unknown restriction column, because the meta-column list is a
  whitelist. `--on-untranslatable=clear` deliberately does not apply to this one
  in either direction: clearing it drops a *restriction*, and a widening is not
  something a flag should decide.

- **The two-axis pricing guard was a blacklist**, so it could only refuse what
  someone had already enumerated — tokens with `compute_seconds`, and either
  unit with characters, both billed. It is now a whitelist of which quantities
  each stated billing unit puts on the invoice, so a component nobody thought of
  is refused rather than billed. A test that had been *asserting the defect* —
  it used a duration-stating upstream to prove a wall-clock rate "still charges
  wall time" — now uses the case that rate is actually for.

- **An unpriceable request charged quota 32.26 USD of a 100 USD plan**,
  attributed nothing to the ledger, and burned the accrual permanently. Accrual,
  quota and ledger now read one variable. The distinction that had to be drawn:
  `Missing` and `NoPrice` are not the same fact — a flat plan has no marginal
  rule by construction, so withholding on `Missing` would leave flat plans
  unbilled forever.

- **Migration `0006` was the first destructive migration in the tree**, and it
  dropped `budget_state.reserved_nano` and `reserved_until` in the release that
  stopped using them — while OPERATIONS tells an operator to migrate once,
  before rolling. Followed exactly as documented that took the fleet down: the
  old binary names both columns in a `SELECT` and an `INSERT`, and the budget
  path fails **closed** with `503 budget_unavailable` on every unrolled node.
  Reproduced against a populated database. `0006` now executes nothing and a
  later release drops the columns; its version stays occupied rather than the
  file being deleted, so the number can never be reused and silently skipped.
  The rule is written down (DESIGN §9.5a): a migration may add and may widen,
  and may never remove what the previous release reads.
  `TestMigrateOntoADatabaseWithData` runs the **previous release's own
  statements** against each migrated schema, because a migration test that
  starts from an empty database cannot fail the way an upgrade does.

### The image could not start on the volume its own Dockerfile names

- **`VOLUME /var/lib/dorang` plus `USER nonroot` crash-loops.** Docker creates
  the volume root-owned, the process cannot `mkdir` its spool inside it, and
  `restart: unless-stopped` spins it. The image is distroless, so there is no
  shell and it cannot fix itself: an operator following the Dockerfile's own
  advice hit a wall with no hint. The directory is now created in the build
  stage owned by 65532 and `COPY --chown`'d into the runtime stage, because
  Docker seeds a *fresh* named volume from the image mount point including
  ownership. `VOLUME` stays — dropping it would silently put the database and
  the key pepper in the writable layer for anyone who runs without `-v`.
- **The mode is not the mechanism**, and the Dockerfile now says so: BuildKit
  gives the directory `0755` and Docker's local driver gives the volume root
  `0755` regardless, so a `chmod` there is a no-op. Ownership decides. Named
  volumes are the only case Docker seeds — a bind mount still needs a host-side
  `chown` and a PVC needs `fsGroup`, both now documented, and the Kubernetes
  manifest sets `fsGroup` so swapping `emptyDir` for a PVC is not an outage.
- **Readiness did not reflect store reachability.** With the database stopped,
  `/health/readiness` kept answering ready with `degraded: false` — a balancer
  routing to a node that cannot authenticate anyone new. The threshold is three
  failed consultations spanning at least fifteen seconds with no success
  between, and both halves are the judgement: a count alone fires in 50 ms under
  load, and a duration alone is satisfied by one call that hangs for the store
  timeout and then fails. Recovery is instant with no hysteresis, because the
  observed outage recovered with `RestartCount 0` and buffered metering flushed
  on return — readiness must not be what delays a recovery that needed no
  restart.

### Verified against a real deployment

- **LiteLLM replacement**: a real agent client ran the same multi-turn,
  tool-calling, streaming task through dorang and through a live LiteLLM and
  could not tell them apart. 39 models, every endpoint family. `testing/parity/`
  holds the harness and the dated reports.
- **PostgreSQL and two-node HA**, measured with two real processes against one
  database: one leader elected in 5.0 s; the published `shared-pg` overshoot of
  0 held (39 admitted against a ceiling of 40); revocation propagated in 9–15 ms
  against a published 270 ms; a rolling restart lost 0 of ~200,000 requests.
- **Lease reclaim after a hard kill is 65–70 s**, not the 30 s node TTL an
  operator would infer, and nothing had published it. The reclaim deliberately
  waits out the *lease* rather than the heartbeat, which is what makes it safe.

### Performance

- Request-path decode parses once instead of twice, at both levels it was doing
  it: warm-local p50 **469 → 355 µs** at the profile's stated 4 KiB ceiling,
  allocations per 4 KiB request **391 → 262**, throughput ceiling
  **20,489 → 23,532 req/s**.
- **Encode stopped re-scanning every nested value once per level of its
  nesting**: warm-local p50 **352 → 248 µs**, throughput ceiling
  **24,758 → 30,979 req/s**, encode's share of the request path
  **34.6% → 12.2%**, and `encoding/json.appendCompact` is gone from the profile
  entirely. The contract for a `json.Marshaler` is that the method returns a
  *finished* document, which the encoder then folds into the buffer it is
  building with a full pass of the JSON state machine — so the text of one
  message was re-scanned by the part's marshal, by the content's, by the
  message's and by the request's. `wirejson.Appender` writes into the caller's
  buffer instead. **`MarshalJSON` stays** on every nested type, unchanged and
  still reflective: keeping it leaves two independent serializers for the
  differential to compare, and the planner *refuses* any shape it does not model
  — an embedded field, `,string`, a `json.Number`, an interface, a recursive
  type — falling back to `encoding/json`, so an unhandled shape is slow and
  never wrong. Read the allocation row carefully, because its two halves are
  different sizes: the count fell 6% and the bytes 35%, since what a `Marshaler`
  per level costs is not one object per value but a copy of the whole subtree at
  every level above it. A gate on the count alone would have registered almost
  nothing, which is why `testing/perf` now bounds both.
- §15.1's published 200 µs p50 was found not to hold — it was true only below
  about 512 B — and every figure in that section is now a measurement with the
  concurrency it was taken at. It has since been corrected three more times, up
  to 480 µs and then down to 375 and 249 as the codec got faster twice, and the
  doc comments that quote it are corrected with it. The harness gates came
  *down* with each improvement; a ceiling left where it was has stopped being a
  ceiling. The one bound that did **not** move is metering's 10 µs: it is
  expressed as 5% of the warm-local p50, and a bound that loosens because the
  thing it is a fraction of moved is a ratchet rather than a bound.

### Correctness

- **A user block, a team block, and every user- and team-level limit reached no
  decision on any node, at any latency.** DESIGN §11.2 authorizes against three
  subjects — the key, its user, its team — and `auth.Principal` has carried
  fields for all three since it was written. The one conversion that builds a
  principal from a stored row populated the key's and left the other two nil,
  because the credential read joined `api_keys` and `api_key_secrets` and nothing
  else. Every guard downstream is nil-guarded, correctly, since an unowned key
  genuinely has no owner — so nothing failed, no test caught it, and
  `users.blocked`, `teams.blocked`, both `max_budget_nano` columns, both rate
  ceilings and `teams.max_parallel` decided nothing. A 60-second propagation
  window was the smaller half of the story; the larger half was that there was
  nothing to propagate. The credential read now LEFT-joins the owning user and
  team **in the same statement** — §2.4's one-round-trip rule is intact, and
  there is a test that counts — and `cluster.AuthPrincipal` takes the owners as a
  required argument, so a construction site cannot omit them silently. A user
  block now reaches a second node in **19.6 ms** at `poll: 20ms` against the
  published 270 ms, and has no window at all on the node that took the call.
  Recorded as W12.
- `/user/*`, `/team/*` and `/budget/*` stop answering `501` — `internal/store`
  gained the `users`, `teams` and `team_members` code it never had, and
  `internal/app` wires the `Directory` and `BudgetStore` seams `internal/admin`
  had declared and nothing filled. A team ceiling is now a real budget hold
  against its own durable counter, and `/budget/delete` clears a ceiling while
  preserving the spend recorded under it, which is how an operator ends a budget
  outage. `/model/*` stays `501` **on purpose**: `deployments` and
  `model_aliases` have a schema and no *reader* — routing is compiled from the
  configuration file — so an adapter over them would answer `200`, write the row
  and route no traffic differently, which is the same "stores a value nobody
  reads" failure the rest of this entry is about.
- A Gemini deployment was routed *and* encoded against the OpenAI wire shape's
  capability set, while its encoder has no field for `cache_control`, `logprobs`,
  `service_tier`, a thinking block or a structured system prompt. Both §10.1
  gates admitted the request, the encoder dropped the construct, and the client
  got a `200` with no header. Unifying the two capability computations had proved
  only that they agreed — both switched on the same resolved `api` and returned
  one of the same two constants, so no configuration could separate them, and
  both were wrong together for the one wire shape with no constant of its own.
  A capability set is now declared beside the encoder that honours it.
- **The encode differential refuted its own replacement twice, and both were
  real defects rather than porting mistakes.** `openai.Content`,
  `openai.StopSequences` and `anthropic.BlockList` are string-or-array types
  that carry their own `MarshalJSON`, and all three called
  `encoding/json.Marshal`, whose HTML escaping is on by default — so a user
  message reading `a && b <tag>` went upstream as
  `a \u0026\u0026 b \u003ctag\u003e` while the `name` beside it went as itself. That is COMPATIBILITY §2.1a's exact
  failure mode, on the one field a caller controls, and it was unpinned because
  every existing escaping test sat on a plain struct field where the package
  encoder's `SetEscapeHTML(false)` already applied. The second: `MarshalWithExtra`
  splices an unmodelled member's bytes verbatim, and its result had always gone
  back through `encoding/json`, which compacted it on the way in — so the first
  path with no encoder above it would have put a caller's whitespace on the wire
  for the first time in the project's history. Neither was caused by the new
  encoder; the new encoder is what made them visible, because two serializers
  that must agree cannot both be silently wrong in the same way.
- The backend adapters passed no `Loss` to either encoder, so every located
  downgrade was discarded where it was produced, and two documented promises had
  no writer at all: §10.2's "reasoning is disabled … and `x-dorang-dropped-params`
  says so", and the thinking-block *signature* dropped crossing into the OpenAI
  family — which no capability mask can see, because the bit is held while the
  encoder drops the signature.
- **Every imported price list was a millionth of its true value**, and every imported
  character card a thousandth. `dorangctl import config` copied LiteLLM's per-token literal
  into a per-million-token field, and its own test asserted the pass-through. The failure is
  silent: requests round to zero while `/spend/calculate` reports the rule matched with
  `"missing": false`. Four more rates were not imported at all, including cache *creation*.
  The shipped `config.example.yaml` and `docs/CONFIG.md` §13 had the same unit confusion by
  hand, while `docs/DESIGN.md`'s catalog format had it right — so the two shipped examples
  disagreed, and the one an operator copies was the wrong one.
- Pricing charged the cached prefix and reasoning tokens twice, 27% over on the
  design's own example card and 5.7× on a 90%-cached workload. Neither
  verification harness could see it, because both priced everything at zero.
- Subscription amortization summed to `plan_cost × H_N`: a 100 USD plan billed
  519 USD over 100 requests, 749 over 1,000.
- An upstream `200` carrying a body that is not a response was served to the
  client as a successful, empty answer — silently, so no retry and no fallback.
- A ten-minute recording was billed as eight seconds, because audio was priced
  on wall time.
- Failed streams reported as successes, so a backend failing every stream kept
  its full share of traffic, and a truncation reached the client wearing
  `finish_reason: "stop"`.
- A burst of traffic switched the PII mask off permanently, through three
  separately-reasonable changes — one of which was requested in this repository.

### Security

- The streaming path scrubbed nothing, so an upstream error naming a credential
  handed the operator's provider key to the tenant.
- An unknown key cost an unbounded store lookup and the caller paid nothing:
  5,000 distinct keys, 5,000 round trips, now 500 with a sustained bound.
- Two processes sharing a `node_id` both led, both passed the fence, and the
  loser emptied the incumbent's ceiling on its way out.
- The Lua sandbox's instruction ceiling could be walked through by one builtin:
  `string.find` on a caller-supplied 256-byte subject ran 4 seconds costing two
  units of budget, and `tonumber` on 8 KiB ran past twenty.

### Documentation

- `docs/SECURITY-REVIEW.md` reached `main` **without the code it described** —
  ten of twelve closure claims were false. The document is corrected in place
  with the record of what it said, and the merge that carried it is the reason
  every closure claim in this project is now re-verified rather than trusted.
