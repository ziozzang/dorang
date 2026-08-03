# Adversarial Security Review

Target: `dorang` — an LLM gateway holding provider credentials, enforcing per-tenant
authorization and budgets, and proxying between untrusted callers and paid upstreams.

Method: read `docs/DESIGN.md` (§2.4, §4.1, §5, §6, §7.4, §9, §10.6, §11.2),
`docs/COMPATIBILITY.md` (§2.0, §11), `docs/REVIEW.md`, then `internal/auth`,
`internal/server`, `internal/admin`, `internal/store`, `internal/app`, `internal/shadow`,
`internal/backend`, `internal/batch`, `internal/capacity`, `internal/prefix`,
`internal/router`, `internal/quota`, `internal/config`, `internal/meter`, `cmd/`, and part
of `internal/probe` (which appeared in the tree during the review). No file in the
repository was modified. Nothing was executed except one offline reproduction of
`dorangctl import config` against a locally authored file containing a fabricated key. No
call was made to any external service and no real credential was used.

The three defects already fixed — parser disagreement at the gate, credential exfiltration
via redirect, and a control wired to nothing — were used as templates. All three shapes
recur. The third recurs the most.

> 한국어: [SECURITY-REVIEW.ko.md](SECURITY-REVIEW.ko.md)

---

## Findings

### [CRITICAL] Batch submission dispatches paid model calls with neither the model allow-list nor the budget gate

- **Location:** `internal/app/batch.go:344-366` (create), `internal/app/batch.go:302-309`
  (route), `internal/batch/scheduler.go:431` (row dispatch), `internal/app/app.go:317`
  (mounted)
- **Attack:** any holder of an ordinary `sk-` key whose `models` allow-list is restricted —
  say to `gpt-4o-mini`. Upload a JSONL file to `POST /v1/files` whose rows name a model the
  key is *not* allowed to use (`claude-opus-4`, or any name in the catalog), then
  `POST /v1/batches` with that `input_file_id`. Every row dispatches upstream.
- **Impact:** complete bypass of the per-key/user/team model allow-list, and complete bypass
  of the budget ceiling, on a path that spends the operator's provider credentials. The
  batch scheduler holds capacity reservations, so it is not even rate-limited into
  irrelevance — it will run the whole file.
- **Evidence:** the gate in `internal/server/server.go:411-416` is the only place the
  allow-list is consulted for a non-inference route:

  ```go
  if rq.Principal != nil {
      if err := rq.Principal.Authorize(Access{Model: rq.Model, Route: rq.Path}); err != nil {
  ```

  `rq.Model` comes from `peekRequest` over the *batch-create* body, which is
  `{"input_file_id","endpoint","completion_window","metadata"}` — it has no top-level
  `model` field, so `rq.Model == ""`. `internal/auth/principal.go:156` then skips the check
  entirely: `if a.Model != "" && !allowedIn(...)`. The second check,
  `Principal.AllowsModel`, lives only in `handleInference`
  (`internal/server/handlers.go:100`), and `/v1/batches` is handled by
  `a.handleBatchCreate`, not `handleInference`.

  The create handler carries the identity but never asks it anything:

  ```go
  out, err := a.Batch.Create(rq.Context(), batch.CreateRequest{
      InputFileID:      body.InputFileID,
      ...
      OwnerKeyID:       principalID(rq),
  })
  ```

  The scheduler resolves the row's model against the *global* target table with no
  reference to the owner (`internal/batch/scheduler.go:431`):

  ```go
  tgt, ok := s.cfg.Models.ResolveModel(row.Model)
  ```

  And the budget gate is absent: `grep -n "udget" internal/batch/*.go internal/app/batch.go
  internal/app/batchstore.go` returns nothing. `budgetGate.reserve` is called from exactly
  one place, `internal/app/dispatch.go:117`, which the batch executor never reaches
  (`internal/app/batch.go:212-278` dispatches directly, bypassing `dispatcher.Dispatch`).
- **Fix:** in `handleBatchCreate`, and again in the row validator that already walks every
  JSONL line (`internal/batch/validate.go`), reject any row whose `model` fails
  `rq.Principal.AllowsModel(row.Model)`; and give the batch executor the same
  `budgetGate.reserve`/`settle` pair `dispatcher.Dispatch` uses, keyed on the batch's
  `OwnerKeyID`.

---

### [HIGH] Three per-subject rate/concurrency limits are stored, imported, administered, documented — and never enforced

- **Location:** `internal/auth/principal.go:46-52` and `:165-170` (the checks),
  `internal/app/authn.go:131` and `internal/server/server.go:412` (the only two
  constructors of `auth.Access`), `internal/app/build.go:95-97` (capacity wiring)
- **Attack:** an operator sets `rpm_limit: 60` on a key (via import from an incumbent
  gateway, or `/key/update`). The key issues 60,000 requests per minute. Same for
  `tpm_limit` and `max_parallel_requests`.
- **Impact:** the per-key, per-user and per-team request-rate, token-rate and concurrency
  ceilings do not exist. DESIGN §11.2 states "Team, user, and key limits all apply; the most
  restrictive wins"; three of them apply to nothing. Cost control on this gateway reduces to
  the budget gate (which has its own defect, below) and the per-credential quota.
- **Evidence:** the enforcement branch reads two fields of `auth.Access`:

  ```go
  if l.RPMLimit != nil && int64(a.ObservedRPM) >= *l.RPMLimit {
      return refuse(ReasonRateLimited, subject, "rpm")
  }
  if l.TPMLimit != nil && int64(a.ObservedTPM) >= *l.TPMLimit {
  ```

  `ObservedRPM` and `ObservedTPM` are never assigned anywhere outside tests. The two
  production constructors are:

  ```go
  // internal/app/authn.go:131
  err := p.p.Authorize(auth.Access{Now: p.now(), Model: a.Model, Route: a.Route})
  // internal/server/server.go:412
  rq.Principal.Authorize(Access{Model: rq.Model, Route: rq.Path})
  ```

  Both leave the two counters at zero, so the comparison is `0 >= limit`, false for every
  positive limit. `MaxParallel` is worse: `internal/auth/principal.go:50-52` says "It is
  enforced by internal/capacity", but `MaxParallel` appears nowhere in `internal/capacity`.
  The broker's per-principal ceiling comes only from static YAML
  (`internal/app/build.go:95-97`):

  ```go
  for name, l := range cfg.Capacity.Principals {
      c.Principals[name] = l.MaxConcurrent
  }
  ```

  The `max_parallel_requests` column on keys, users, teams and deployments never reaches it.
- **Fix:** either (a) populate `Access.ObservedRPM`/`ObservedTPM` from `internal/quota`'s
  per-key meter at `internal/app/authn.go:131` and feed `Limits.MaxParallel` into
  `capacity.Request` at `internal/app/dispatch.go`, or (b) delete the three fields from
  `auth.Limits` and the admin/store schema so nothing promises enforcement that does not
  happen. Shipping them inert is the worst of the three options.

---

### [HIGH] A team or user budget ceiling is enforced per key, so N keys under one team each get the full ceiling

- **Location:** `internal/app/budget.go:176-205` (`principal.budget`),
  `internal/app/budget.go:102-103` (the durable key)
- **Attack:** a team is given `max_budget: 100 USD/month`. The team's ten keys carry no
  key-level budget of their own. Each key independently reserves against its own durable
  counter with the *team's* limit as its ceiling. The team spends 1000 USD.
- **Impact:** the team and user budget ceilings — the ones an operator sets to bound a
  department — are multiplied by the number of keys under them. On a team with many keys
  this is unbounded in practice.
- **Evidence:** `budget()` takes the *minimum* limit across key, user and team, which is the
  right direction:

  ```go
  for _, l := range []*auth.Limits{&p.p.Key, p.p.User, p.p.Team} {
      if l == nil || l.MaxBudgetNanoUSD == nil { continue }
      if v := *l.MaxBudgetNanoUSD; !ok || v < limit { limit, ok = v, true }
      if period == "" { period = l.BudgetPeriod }
  }
  ```

  but the counter that limit is applied to is keyed on the **key id** only
  (`internal/app/budget.go:102-103`):

  ```go
  hold, err := g.ledger.Reserve(ctx,
      cluster.BudgetKey(budgetSubjectKind, p.KeyID(), window, g.now()), limit, amount)
  ```

  with `budgetSubjectKind = "key"` (`internal/app/budget.go:33`). The comment at
  `internal/app/budget.go:173-175` asserts the safety property that does not hold: "a user or
  team ceiling lower than the key's still binds the request, which is the direction that
  cannot fail open." It binds each key to the team ceiling *separately*, which is precisely
  failing open by a factor of N.
- **Secondary defect in the same function:** `limit` is the minimum across subjects but
  `period` is the *first non-empty* period among subjects that carry a budget. A key with a
  monthly 10 USD budget under a team with a daily 1 USD budget yields `limit = 1 USD,
  period = monthly` — the daily ceiling applied over a monthly window, which is the exact
  "a limit carried without its period" hazard `internal/auth/principal.go:36-41` documents.
- **Fix:** take a hold per binding subject, not one hold at the minimum: reserve against
  `BudgetKey("key", keyID, …)`, `BudgetKey("user", userID, …)` and
  `BudgetKey("team", teamID, …)` for every subject that declares a ceiling, each with its
  own limit *and its own period*, releasing all of them together on failure.

---

### [HIGH] The upstream's error message is copied verbatim into the client-facing envelope, contradicting COMPATIBILITY §11.3 and exposing the provider credential

- **Location:** `internal/server/errors.go:269`, `:287`, `:302`, `:315`, `:358`;
  reached from `internal/backend/errors.go` (`upstreamError` → `server.Normalize`)
- **Status: partially closed.** The scrubber half is now on the shipped path. When this was
  written the dispatch path made its own HTTP call in `internal/app` and had no scrubber;
  that copy has been deleted and `internal/app` now calls `internal/backend`, whose
  `upstreamError` collects what was actually put on the outbound credential headers
  (`collectSecrets`, which reads the headers rather than the credential table so an OAuth
  token applied by code it does not own is caught too) and scrubs it from the message, the
  native type and the code before anything renders them. The *other* half of the finding
  stands: `Normalize` still puts the upstream's own message in the response body, which
  COMPATIBILITY §11.3 says it must not, and the four structured branches are still unbounded.
- **Attack:** two variants, neither needing access to dorang.
  1. A provider that echoes the offending key in its 401 body. Several
     OpenAI-compatible servers do (`{"error":{"message":"Invalid API key: sk-…"}}`), and
     OpenAI itself echoes a partially redacted form. Any authenticated caller sends one
     request while a provider credential is invalid, revoked, or mid-rotation, and reads the
     provider's message straight out of dorang's 401 body.
  2. A compromised or hostile backend — including a self-hosted vLLM/SGLang node, which
     DESIGN §4.4 makes a first-class provider class — simply answers any request with
     `{"error":{"message":"<the x-api-key header it just received>"}}`.
- **Impact:** exfiltration of the operator's provider credential (or a substantial prefix of
  it) to any tenant with a valid `sk-` key. This is the same class as the redirect defect
  already fixed — a hostile upstream turning the response path into a credential channel —
  and it is not closed on the response *body*, only on response headers in the passthrough
  engine.
- **Evidence:** COMPATIBILITY §11.3 is unambiguous:

  > The upstream's own `type`, `code`, and message are recorded in the ledger and surfaced in
  > `x-dorang-native-error-type` / `x-dorang-native-error-code`. They are **not** put in the
  > response body.

  `Normalize` puts them in the response body on every branch:

  ```go
  e.Message = s              // errors.go:269  — SGLang bare-string envelope
  e.Message = in.Message     // errors.go:287  — OpenAI-nested and Anthropic
  e.Message = s              // errors.go:302  — FastAPI detail
  e.Message = probe.Message  // errors.go:315  — SGLang flat envelope
  e.Message = http.StatusText(status) + ": " + string(b)  // errors.go:358 — opaque
  ```

  and `appendEnvelope` (`internal/server/errors.go:434`) writes `e.Message` into the
  `{"error":{"message":…}}` the client reads. Only the `opaqueError` branch is bounded, at
  `excerptLimit = 256` (`internal/server/errors.go:215`) — and the comment there ("so that a
  stack trace or a credential echoed into a 500 page cannot be relayed wholesale") is wrong
  about the consequence: a `sk-` key is 51–164 characters and fits in 256 bytes with room to
  spare. The four structured branches have no bound at all, limited only by the 1 MiB read
  at `internal/app/dispatch.go:296`.
- **Fix:** honour §11.3 — put the upstream message in `NativeMessage` (ledger + header) and
  emit dorang's own canonical message for the condition in the body, exactly as
  `canonicalType`/`canonicalCode` already do for the other two fields. If an excerpt must be
  relayed for debuggability, gate it behind an operator flag that is off by default, and
  scrub every known provider-credential value from it before it is written.
- **The codebase already knows this rule and implements it correctly elsewhere.**
  `internal/probe/doc.go:30-34` states it outright — "a provider's error body can echo the
  key back, and wrapping it would put the key straight into the message… Every error and
  every recorded reason additionally passes a scrubber holding every secret the credential
  has presented" — and `internal/probe/scrub.go` is a working implementation that also
  handles the URL-escaped spelling of a key arriving in a query parameter.
  `internal/probe/http.go` applies it at every error site (`:94`, `:111`) and bounds every
  read (`:98`, `:106`). That package has no importers. The shipped dispatch path had no
  scrubber at all. This is the fixed-defect shape once more: the correct code exists, and
  the path that runs is not the path that has it.

  That last sentence was the whole defect class, and it recurred a third time before it was
  caught: `internal/backend` was written as an extraction out of `internal/app`, four
  tool-call defects were fixed in it, its tests passed — and nothing imported it, so
  production kept all four. The extraction has since been completed and `internal/app`'s
  copy deleted. The lesson the two occurrences share is that a package test proves the code
  works, never that anything calls it; the check that would have caught both is an importer
  count, and after it, a test that fails when the fix is reverted.

---

### [HIGH] An unauthenticated caller can drive an O(loaded-keys) map copy under a global write lock, plus one database query, per request

- **Location:** `internal/auth/authenticator.go:553-588` (`insert`, `mergeLocked`), reached
  from `internal/auth/authenticator.go:433-438` via `internal/server/server.go:381`
- **Attack:** send `POST /v1/chat/completions` with `Authorization: Bearer sk-<random>` and
  no body. Vary the random suffix on every request. Authentication runs *before* the body is
  read (`internal/server/server.go:375-409`), so each request is a few hundred bytes on the
  wire.
- **Impact:** each request is a cache miss with a fresh lookup key, so (a) it costs one
  `LoadByLookup` store query — the negative-TTL cache cannot coalesce distinct keys — and
  (b) from the 64th distinct unknown key onward, every single one triggers a full copy of
  the credential snapshot map while holding `a.mu` exclusively. On a deployment with 50,000
  loaded keys that is a 50,000-entry map allocation and copy per request, serialized against
  every other authentication miss. The mitigation the code documents ("a flood of unknown
  keys must not make every miss cost O(keys)") does not fire.
- **Evidence:** `insert` folds the overlay once it reaches `mergeThreshold = 64`:

  ```go
  a.overlay[l] = e
  switch {
  case len(a.overlay) >= maxOverlay:   // 8192
      a.overlay = nil
  case len(a.overlay) >= mergeThreshold:  // 64
      a.mergeLocked()
  }
  ```

  but `mergeLocked` promotes only *positive* entries and hands the negatives straight back
  as the new overlay:

  ```go
  cur := *a.snap.Load()
  m := make(map[Lookup]*entry, len(cur)+len(a.overlay))
  for k, v := range cur { m[k] = v }        // O(loaded keys), every call
  keep := make(map[Lookup]*entry)
  for k, v := range a.overlay {
      if v.found { m[k] = v } else { keep[k] = v }
  }
  a.snap.Store(&m)
  a.overlay = keep                          // still >= 64 negatives
  ```

  An unknown-key flood produces only negative entries, so `len(a.overlay)` never falls back
  below 64 and `mergeLocked` runs on essentially every subsequent miss until the 8192-entry
  reset — roughly 8128 full snapshot copies per 8192 attacker requests. The hot path also
  takes `a.mu.RLock()` at `internal/auth/authenticator.go:425`, so legitimate misses queue
  behind the writer.
- **Fix:** do not call `mergeLocked` when the overlay contains no promotable entries — track
  a `positives` count in `insert` and merge on that, not on `len(a.overlay)`. Bound the
  negative set separately (a small ring or a second map with its own cap). Independently,
  rate-limit or coalesce store lookups for unknown keys per source, since one query per
  unauthenticated request is its own amplification factor against SQLite.

---

### [HIGH] The prefix-affinity table is not tenant-scoped, giving any tenant a byte-exact oracle over other tenants' prompt prefixes and a routing-poisoning primitive

- **Location:** `internal/prefix/chain.go:81-93` (`NewChain`), `internal/prefix/chain.go:169`
  (`Compute`), `internal/app/dispatch.go:210-215` (the only interactive call site),
  `internal/app/app.go:174` (one process-wide table), `internal/router/router.go:852-863`
  and `:1191-1193` (lookup and record), `internal/router/strategy.go:52-53` (it decides
  routing)
- **Attack:** tenants A and B share a model group. Coding-agent clients send a large,
  byte-identical system prompt on every request, well inside the 4 KiB first checkpoint.
  B sends a request with the candidate prefix bytes and `X-Dorang-Detail: full`, then reads
  `X-Dorang-Route-Reason: prefix_hit:depth=N` off its own response
  (`internal/app/dispatch.go:607` → `internal/server/headers.go:227-228`). A hit means some
  other tenant recently sent those exact bytes on that model. Varying the guessed bytes and
  watching `depth` walks the prefix out byte by byte. Separately, because `prefix_sticky` is
  first in the default strategy chain and outranks `lowest_cost`, B can *plant* an entry for
  A's prefix by sending it first, pinning A's next request onto whatever deployment served
  B's.
- **Impact:** a cross-tenant confirmation oracle over request prefixes (system prompts,
  project names, anything early in the body), and the ability to steer another tenant's
  traffic onto a chosen deployment — cost inflation, targeted degradation, or steering onto
  a deployment the attacker knows is about to fail.
- **Evidence:** the chain's only seed is the client-facing model group:

  ```go
  func NewChain(group string, baseSegment int) *Chain {
      ...
      c.state = sha256.Sum256([]byte(group))
  ```

  and the only interactive construction site passes the model name and nothing else:

  ```go
  if st.prefixOn && st.chunk > 0 {
      c.rreq.Digests = prefix.Compute(rq.Model, body, st.chunk)
  }
  ```

  `prefix.Digest` is `[16]byte` (`internal/prefix/table.go`) with no tenant component in the
  key type at all.
- **Honest qualification:** the implementation matches the design. DESIGN §7.4b line 1007
  specifies `h₀ = H(group_id)`. So this is a gap in the design, not a deviation from it —
  but the review brief's premise that "cache-affinity keys are tenant-scoped by design" is
  true only of §7.4a (session stickiness), not §7.4b. On a shared gateway the two need the
  same rule.
- **Fix:** seed the chain with the tenant as the leading component —
  `h₀ = H(tenant ‖ 0x00 ‖ group)` — taking the tenant from `rq.Principal.TeamID()` (falling
  back to `KeyID()`) at `internal/app/dispatch.go:214`. Until then, suppressing
  `X-Dorang-Route-Reason`'s prefix detail removes the oracle but not the poisoning.

---

### [HIGH] The session-stickiness tenant scope is implemented, tested, and never populated

- **Location:** `internal/router/request.go:82-84` (the field),
  `internal/router/router.go:638-643` (the key), `internal/app/dispatch.go:198-206` (the
  only production constructor)
- **Attack:** two tenants that present the same `X-Dorang-Session` value — a default or
  predictable session id, or an attacker deliberately mirroring a victim's — collide onto
  the same pin.
- **Impact:** DESIGN §7.4a line 925 states the property this breaks verbatim: "Key is
  `(tenant, group, session)` with tenant as the leading component so two tenants never share
  a pin." In the shipped binary the leading component is always `""`. Tenant B can be handed
  the `(deployment, credential)` pin tenant A holds — which §7.4a2 calls a *correctness*
  constraint for stateful conversations, not an optimization — and can learn whether a given
  session id is pinned by anyone.
- **Evidence:** the key is built correctly:

  ```go
  return stickyKey{tenant: req.Tenant, group: g.Name, session: req.Session}
  ```

  and `Request.Tenant` is assigned in exactly one place in the entire tree:

  ```
  $ grep -rn "Tenant:" --include=*.go .
  testing/scenario/harness.go:421:		Tenant:        c.Tenant,
  ```

  `internal/app/dispatch.go:198-206` sets `Model`, `Principal`, `Session`, `AllowLossy`,
  `InputTokens`, `MaxOutputTokens`, `Stream` — and not `Tenant`. The identity is right there:
  `server.Principal` exposes `KeyID()`, `UserID()` and `TeamID()`
  (`internal/server/deps.go:32-40`), all populated at `internal/app/authn.go:120-127`.
- **Fix:** one line — add `Tenant: tenantOf(rq.Principal),` to the `router.Request` literal
  at `internal/app/dispatch.go:198`, and the same at the batch call site
  (`internal/app/batch.go:141`). Add a test that fails when the field is zero on a request
  that carried a principal.

---

### [HIGH] Passthrough routes dispatch without the model allow-list

- **Location:** `internal/server/passthrough.go:133` (`NeedsBody: false`),
  `internal/server/server.go:389-416` (the gate), `internal/server/handlers.go:100` (the
  check that is skipped)
- **Attack:** a deployment with `passthrough.enabled: true` and a route
  `{ prefix: /anthropic, provider: anthropic-main, auth: dorang }`. A key restricted to
  `gpt-4o-mini` sends `POST /anthropic/v1/messages` with `{"model":"claude-opus-4",…}`. The
  request is authenticated, the route allow-list is checked, the model allow-list is not.
- **Impact:** the per-key model allow-list is bypassed for every model reachable behind a
  configured passthrough prefix, spending the operator's provider credential (which
  `passthroughRoutes` attaches for `auth: dorang`, `internal/app/build.go:668-673`).
- **Evidence:** the model is only ever peeked for routes that declare `NeedsBody`:

  ```go
  if rt.NeedsBody {
      ...
      model, stream, ok := peekRequest(b)
      rq.Model, rq.Stream = model, stream
  }
  if rq.Principal != nil {
      if err := rq.Principal.Authorize(Access{Model: rq.Model, Route: rq.Path}); err != nil {
  ```

  Passthrough sets `NeedsBody: false` deliberately ("step 3: relay without parsing,
  streaming"), so `rq.Model` is `""` and `Limits.authorize` skips the check
  (`internal/auth/principal.go:156`). `servePassthrough`
  (`internal/server/passthrough.go:210-266`) never consults the principal again.
- **Fix:** the honest options are (a) refuse to compile a passthrough route when any
  configured key carries a non-empty `models` allow-list, so the incompatibility is a
  startup error rather than a silent bypass, or (b) accept a bounded peek on passthrough
  bodies whose content type is JSON, reusing `peekRequest` against the same cap the relay
  already applies, and enforce the allow-list on the result. Option (a) is smaller and
  fails closed.

---

### [HIGH] The non-streaming upstream response is read with an unbounded `io.ReadAll`

- **Location:** `internal/app/dispatch.go:332`
- **Attack:** a hostile, compromised, or merely misbehaving backend — including a
  self-hosted engine — answers `200 application/json` with a multi-gigabyte body. Any
  authenticated caller that routes to it triggers the read.
- **Impact:** the whole body is buffered, then `convertResponse` unmarshals it
  (`internal/app/dispatch.go:341`) and re-marshals the result, so peak resident memory is
  roughly three to four times the response size, per concurrent request. This is a remote
  OOM driven entirely from the upstream side of the trust boundary.
- **Evidence:** the error path on the same function is bounded and the success path is not:

  ```go
  if resp.StatusCode >= 400 {
      body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))   // :296 — bounded
  ...
  body, err := io.ReadAll(resp.Body)                             // :332 — unbounded
  ```

  DESIGN §15.5 prohibits full-body buffering and §10.6 makes the bound "part of the contract
  rather than an implementation detail" for the passthrough relay
  (`internal/server/passthrough.go:192-201`, `relayBufferSize` and `usageScanLimit`). Two
  paths, one rule, one enforcement. There is a documented process-wide replay budget for
  *request* bodies (`internal/server/server.go:25`) and nothing at all for response bodies.
- **Fix:** `io.ReadAll(io.LimitReader(resp.Body, cfg.maxResponseBytes))` with a configurable
  ceiling defaulting to the request cap, and a `502 upstream_response_too_large` when the
  limit is hit.

---

### [MEDIUM] Request bodies are fully buffered before the concurrency gate, so the per-key ceiling does not bound memory

- **Location:** `internal/server/server.go:389-409` (buffer), `internal/app/dispatch.go:103`
  → `internal/capacity/broker.go:381-385` (the gate, downstream)
- **Attack:** one valid key, N simultaneous connections, each `POST /v1/chat/completions`
  with a body just under the 32 MiB cap.
- **Impact:** every connection's body is read into memory before the 32-concurrent-per-key
  capacity check is consulted, and requests that then queue for a slot hold their buffer for
  up to `MaxQueueWait` (30 s default). The per-key concurrency ceiling bounds *dispatched*
  requests, not *buffered* ones. There is no `netutil.LimitListener` or equivalent
  (`cmd/dorang/main.go:173` is a plain `net.Listen`). **The cap itself is configurable now** —
  `server.max_body_bytes` (CONFIG §2) reaches `server.Options.MaxBodyBytes`, so an operator can
  lower it; that closes the half of this finding that said no operator could. The admission
  half stands: the buffer is still taken before the concurrency gate, and lowering the cap
  reduces the per-connection cost rather than bounding the number of concurrent buffers.
- **Evidence:** `serve` reads the body at `internal/server/server.go:391` and only calls
  `rt.Handler` at `:418`; the capacity acquisition is inside `st.router.Route` at
  `internal/app/dispatch.go:103`, which `handleInference` reaches after the read.
- **Fix:** acquire a cheap per-principal admission token before `rq.body.read`, released when
  the request finishes. Surfacing `server.max_body_bytes` in configuration is **done**
  (`TestConfiguredBodyCapIsHonoured`).

---

### [MEDIUM] No `ReadTimeout` or `IdleTimeout`, and the body read has no deadline — slow-POST

- **Location:** `internal/server/drain.go:36-43`, `internal/server/request.go:256-308`
- **Attack:** an authenticated caller sends complete headers within the 30 s
  `ReadHeaderTimeout`, declares `Content-Length: 33554432`, then sends one byte every 25
  seconds.
- **Impact:** the handler goroutine blocks in `r.Body.Read` with no deadline — once
  `ReadHeaderTimeout`'s window has passed, Go clears the read deadline because `ReadTimeout`
  is unset. Repeated across connections this pins goroutines, pooled `Request` structs and
  file descriptors indefinitely.
- **Evidence:**

  ```go
  hs := &http.Server{
      Handler: s,
      ReadHeaderTimeout: 30 * time.Second,
  }
  ```

  The comment above it claims "the gateway's own request timeout bounds the handler", but
  the request context built at `internal/server/server.go:328-332` is never threaded into
  `Body.read`, which takes `*http.Request` and an `int64` and loops on a blocking
  `r.Body.Read`. `DefaultRequestTimeout` is 6000 s (`internal/server/server.go:29`) and does
  not apply here regardless.
- **Fix:** set `ReadTimeout` and `IdleTimeout` on the `http.Server`, and use
  `http.NewResponseController(w).SetReadDeadline` inside `Body.read`.

---

### [MEDIUM] `/v1/files` has a per-file cap but no per-key quota, no file count limit, and no expiry

- **Location:** `internal/app/batch.go:319-328` (route), `internal/batch/files.go:37-61`
  (per-file cap), `internal/app/batch.go:403-441` (upload handler)
- **Attack:** an authenticated caller repeatedly `POST /v1/files` with
  `purpose=user_data` and a ~200 MiB payload.
- **Impact:** disk exhaustion under the blob directory, which also starves the SQLite or
  Postgres store and every other tenant on the same volume. The per-upload cap
  (`DefaultMaxFileBytes = 200 << 20`) bounds one file; nothing bounds the count or the
  total. `handleFileUpload` never sets `ExpiresAfter`, so `rec.ExpiresAt` stays zero
  (`internal/batch/files.go:82-83`) and files persist until explicitly deleted. Purposes
  other than `batch` skip JSONL validation entirely (`internal/batch/files.go:63-68`), so
  the payload need not even be well-formed.
- **Fix:** a per-`OwnerKeyID` total-bytes and file-count quota checked in `UploadFile`, and a
  default `ExpiresAfter` for uploads that do not name one.

---

### [MEDIUM] Transport error text — internal hostnames, ports and URLs — is relayed to the caller on the dispatch path, while the passthrough path deliberately refuses to

- **Location:** `internal/app/dispatch.go:286-287`, versus
  `internal/server/passthrough.go:373-376`
- **Attack:** any authenticated caller sends a request that routes to a deployment which is
  down, slow, or has a DNS failure.
- **Impact:** the 502 body carries the upstream URL and the transport error, e.g.
  `upstream request failed: Post "http://vllm-a.internal.svc:8000/v1/chat/completions": dial
  tcp 10.0.3.14:8000: connect: connection refused`. That maps the operator's internal network
  from outside it. Go's `url.Error` redacts an embedded password but keeps the username, host,
  port and path.
- **Evidence:** the two paths state opposite rules for the same condition. Passthrough:

  ```go
  // The transport error text can name internal hosts and ports. It is
  // deliberately not relayed.
  return NewError(http.StatusBadGateway, TypeAPIError,
      "could not reach the upstream provider").WithCode("passthrough_unreachable")
  ```

  Dispatch:

  ```go
  res.err = server.NewError(status, server.TypeAPIError,
      "upstream request failed: "+err.Error()).WithCode("upstream_unreachable")
  ```
- **Fix:** use the passthrough wording at `internal/app/dispatch.go:286` and log the detailed
  error via `d.logf` instead. The same applies to `internal/app/dispatch.go:460`, which
  appends a decode error that can quote upstream body bytes.

---

### [MEDIUM] `x-dorang-native-error-type` is set from an unvalidated, unbounded upstream string, in violation of the rule the same package states elsewhere

- **Location:** `internal/server/errors.go:478`, versus `internal/server/server.go:469-478`
- **Attack:** a hostile backend returns `{"error":{"type":"a\r\n\r\n<injected>","message":…}}`.
  `json.Unmarshal` decodes the escapes into real CR LF, `canonicalType` passes the string
  through as `NativeType`, and `WriteError` writes it into a response header.
- **Impact:** with `net/http`'s own `ResponseWriter` this is neutralized —
  `Header.writeSubset` replaces CR and LF with spaces — so it is not exploitable today. But
  the package explicitly rejects that reasoning 100 lines away:

  ```go
  // The path is client-controlled and is about to become a header value.
  // net/http drops an invalid one at write time, but a response writer that
  // is not net/http's will not, so the check happens here — CRLF in a header
  // value is response splitting, and "the framework probably catches it" is
  // not a security argument.
  if safeHeaderValue(rq.Path) {
      rw.Header().Set(HeaderUnimplemented, rq.Path)
  }
  ```

  `WriteError` applies no such check to an *upstream*-controlled value, which is a strictly
  less trusted source than the client-controlled path that does get checked. The value is
  also unbounded — up to the 1 MiB error-body read — so a backend can emit a 1 MiB response
  header.
- **Fix:** `if safeHeaderValue(e.NativeType) { h.Set(HeaderNativeErrorType, clamp(e.NativeType, 128)) }`.
- **Related latent hazard:** `stampHeaders` builds header *names* from map keys with
  `quotaPrefix+textproto.CanonicalMIMEHeaderKey(window)+quotaSuffix`
  (`internal/server/headers.go:258-261`). `CanonicalMIMEHeaderKey` returns its input
  unchanged when it contains invalid bytes. `Result.QuotaUsedPct` is currently never
  populated by anything (see the unreachable-controls section), so this is inert — but if it
  is ever fed from provider-reported window names it becomes header-*name* injection, which
  is a worse position than the value case above.

---

### [MEDIUM] The embeddings relay forwards the client body upstream without the strict fold-collision filter

- **Location:** `internal/app/dispatch.go:370-376` and `:723-737` (`replaceModel`), versus
  `internal/canonical/strictjson.go:12-46`
- **Attack:** `POST /v1/embeddings` with `{"model":"allowed","Model":"other"}`.
- **Impact:** `strictjson` removes fold-colliding keys rather than renaming or preserving
  them, and states exactly why: "an adapter that kept `Model` in its pass-through Extra map
  would hand it to the next hop, and any hop that parses with `encoding/json` — the next
  dorang, a Go-based OpenAI-compatible server — re-creates the bypass one link further
  down." The embeddings path does not apply that filter: `replaceModel` decodes into
  `map[string]json.RawMessage`, overwrites `obj["model"]`, and re-marshals — carrying
  `"Model"` intact to the upstream.
- **Honest qualification:** I could not construct a working exploit. `json.Marshal` sorts map
  keys, and every ASCII case variant of `model` sorts before `model` (uppercase bytes are
  lower), so the correct key is always last in the emitted object and last-wins at a Go
  upstream. The defence is accidental — it depends on Go's map-key sort order and on the
  fold-collision alphabet for this one field — not on the rule the design states. Reported as
  a hardening gap, not a live vulnerability.
- **Fix:** run the body through `canonical.StrictBytes` (or an equivalent key filter) in
  `replaceModel` before re-marshalling, so the one path that relays a caller's raw body
  obeys the same rule as every path that decodes it.

---

### [MEDIUM] `dorangctl import config` writes plaintext provider keys to stdout, because `SecretRef`'s redaction is structurally bypassed by its own embedding tag

- **Location:** `internal/config/secret.go:33` (the `Inline` field), `internal/config/secret.go:88`
  (`MarshalYAML`), `internal/config/config.go:168` and `:267` (the `yaml:",inline"` embeddings),
  `internal/config/import.go:471`, `cmd/dorangctl/config.go:114-120`
- **Attack:** an operator migrating from an incumbent proxy runs the documented workflow,
  `dorangctl import config old.yaml > dorang.yaml`.
- **Impact:** every literal `api_key` in the source file is re-emitted as a plaintext
  `key:` value on stdout — into the new config file, into terminal scrollback, and into CI
  logs if the migration is scripted. DESIGN §4.1 states "Secrets never appear in
  configuration"; this is the one tool whose job is to produce a configuration, and it
  writes them there.
- **Evidence:** reproduced against the real binary with a fabricated key and no network:

  ```
  $ go run ./cmd/dorangctl import config in.yaml
  ...
  credentials:
      - id: openai-key-1
        provider: openai
        key: sk-FAKE-NOT-A-REAL-KEY-000000000000000000
  ```

  The redaction that should have stopped it never runs. `SecretRef.MarshalYAML`
  (`internal/config/secret.go:88`) deliberately emits only the reference — but both uses of
  `SecretRef` embed it with `yaml:",inline"` (`internal/config/config.go:168`, `:267`), and
  `gopkg.in/yaml.v3` flattens an inline-embedded struct's exported fields rather than calling
  its `MarshalYAML`. The doc comment on the method admits this in passing ("the real
  guarantee is that `resolve` moves an inline literal out of the exported field") — and
  `ImportProxyConfig` is precisely the path that never calls `resolve`, being "deliberately
  not validated". So the server-startup path is safe by accident of ordering, not by the
  redaction, and the one path that skips that ordering leaks.
- **Honest qualification:** the plaintext was already in the operator's source file, so this
  propagates a secret rather than disclosing one that was otherwise protected, and the
  importer does warn on stderr (`internal/config/import.go:472-475`). That is why this is
  MEDIUM and not HIGH. The structural point is the interesting one: a redaction that exists
  and is never invoked is the same defect class as a check that exists and is never called.
- **Fix:** have `credential()` emit `SecretRef{Env: suggestedEnvName}` and print the literal
  values to stderr as an `export FOO=…` block for the operator to place, so the generated
  configuration never contains a secret. Failing that, give `Credential` an explicit
  `MarshalYAML` that calls through to `SecretRef.MarshalYAML` instead of relying on `,inline`,
  and add a test that marshals a `Config` holding an unresolved inline secret and asserts the
  plaintext is absent from the output.

---

### [MEDIUM] Any escaped byte in any top-level key forces a full unmarshal of the whole body on the routing hot path

- **Location:** `internal/server/peek.go:32`, `:108-142`
- **Attack:** every request includes an unrelated top-level key with one escaped character,
  e.g. `{"x":0,"model":"gpt-4o-mini","messages":[…32 MiB…]}`.
- **Impact:** `sawEscapedKey` is set by *any* escaped key, not only one that could spell
  `model` or `stream`, and the fallback then runs
  `json.Unmarshal(b, &map[string]json.RawMessage)` over the entire body. A caller can force
  full-body JSON decoding on every single request — precisely the work DESIGN §15.2.2 says is
  "the single most expensive thing a gateway can do before it has even decided where to send
  the request". Bounded by the 32 MiB cap, so this is CPU amplification, not memory
  exhaustion.
- **Evidence:** the flag is set unconditionally on any escape (`internal/server/peek.go:55`)
  and the fallback is entered on the flag alone (`:108`).
- **Fix:** set `sawEscapedKey` only when the *decoded* key could be `model` or `stream` —
  cheapest correct test is a length check plus an unescape of keys whose escaped length could
  produce a 5- or 6-byte result.

---

### [MEDIUM] Two independent lists of accepted authentication headers, with no cross-check

- **Location:** `internal/auth/header.go:33-40` (`accepted`, used by `Extract` to
  *authenticate*) and `internal/server/headers.go:33-40` (`authHeaders`, used by
  `StripAuthHeaders` to *strip before forwarding*)
- **Attack:** none today — the two lists currently hold the same six names, and both match
  case-insensitively.
- **Impact:** the two lists are maintained separately and nothing asserts they agree. Adding
  a seventh accepted header to `internal/auth/header.go` without the matching edit to
  `internal/server/headers.go` produces a header that *authenticates the caller* and is then
  *forwarded to the provider* — the client-credential leak COMPATIBILITY §7.3 exists to
  prevent. `internal/auth/header.go:124` (`auth.Strip`) has no non-test caller at all; the
  server uses its own copy, which is how the two came to be able to drift.
- **Evidence:** `grep -rn "auth\.Strip"` returns only the definition.
  `internal/server/handlers_test.go:33` asserts `len(AuthHeaders()) != 6` but never compares
  the two packages' lists.
- **Fix:** delete `internal/server/headers.go`'s copy and have the server call
  `auth.Headers()`/`auth.Strip`, or add a test that asserts set equality between
  `auth.Headers()` and `server.AuthHeaders()`. One list, or a test that fails when there are
  two.

---

### [MEDIUM] The passthrough and WebSocket relays forward upstream `Set-Cookie` onto dorang's own origin

- **Location:** `internal/server/passthrough.go:415-423` (`copyResponseHeaders`),
  `internal/server/websocket.go:160-171` (`writeSwitchingProtocols`)
- **Attack:** a compromised backend behind a passthrough prefix answers with
  `Set-Cookie: dorang_admin_ui=…; Path=/`.
- **Impact:** `copyResponseHeaders` copies every upstream header, then strips hop-by-hop and
  authentication headers — `Set-Cookie` is in neither set. An upstream can therefore set
  arbitrary cookies on dorang's origin, including overwriting or clearing the operator UI's
  session cookie. Session *fixation* does not work (the session id is minted server-side and
  looked up in a local map, `internal/admin/ui.go:417-457`), so the concrete impact is
  denial of the operator's UI session and general cookie injection, not takeover. It becomes
  materially worse if the admin surface is ever mounted on the same origin *and* gains a
  mutating endpoint.
- **Fix:** add `Set-Cookie` (and `Set-Cookie2`) to the response-side strip set in
  `copyResponseHeaders`, on the same reasoning §10.6's rule 4 already applies to
  authentication headers: a backend that leaks state into the response must not have it
  relayed.

---

### [LOW] `shadow.Options` holds the reference gateway's credential in an unredacted field

- **Location:** `internal/shadow/options.go:64-66`
- **Attack:** none today.
- **Impact:** `ReferenceKey string` is a plain field on a struct with no `String`,
  `GoString`, `Format` or `MarshalJSON` override. Every other secret-bearing type in the
  tree redacts itself under every `fmt` verb — `auth.Config`
  (`internal/auth/authenticator.go:114-120`), `auth.Hasher`
  (`internal/auth/scheme.go:217-223`), `auth.Token`, `auth.OAuthConfig`,
  `auth.OAuthCredential` (`internal/auth/oauth.go:318`), `auth.OAuthManager`
  (`internal/auth/oauth.go:715-725`), `config.SecretRef`
  (`internal/config/secret.go:79-85`). This one does not. I checked every call site and
  none formats `Options`, so it is latent — but it is the single weakest link of its kind,
  and one future `logf("shadow: starting with %+v", opts)` turns it into a credential in
  the log.
- **Fix:** three lines, matching the pattern the rest of the tree already uses.

---

### [LOW] The route table is enumerable before authentication

- **Location:** `internal/server/server.go:362-371`, `:469-487`;
  `internal/admin/api.go:252-279`
- **Attack:** an unauthenticated caller probes paths and reads the status and headers.
- **Impact:** `serve` returns `501 route_unknown` / `501 route_not_implemented` (with the
  requested path echoed in `X-Dorang-Unimplemented`) and `405` with an `Allow` header, all
  before `cfg.auth.AuthenticateHeader` is reached. The admin API does the same — route
  lookup, `OPTIONS` handling and method dispatch all precede `a.authenticate(r)`. A caller
  learns which passthrough prefixes are configured, which routes exist, and which methods
  they accept, without a credential.
- **Fix:** authenticate before answering 405/501 for non-public routes, or collapse both to a
  single uninformative status for unauthenticated callers. Low value either way; recorded
  for completeness.

---

### [LOW] A relayed WebSocket has no timeout, no metering, and no per-key limit

- **Location:** `internal/server/websocket.go:36-38`, `:49-157`, `:178-194`
- **Attack:** with `AllowWebSocket` enabled on a passthrough route, an authenticated caller
  opens many upgrades and holds them.
- **Impact:** "Once the upgrade completes there is no timeout" is stated as intentional. Each
  relay holds two goroutines, two connections, two 32 KiB buffers and one `inflight` count
  that never decrements, and `rq.Result.Tokens` is never set so nothing is metered or
  charged. During a drain, `waitInFlight` cannot reach zero and the grace always expires.
- **Fix:** an idle deadline on both halves of `pipe`, and a per-principal cap on concurrent
  relays.

---

## Controls that exist and are not reachable

This class has now appeared far more than once. It is the dominant defect shape in this
codebase, and it comes in four grades.

### 1. The entire administrative control plane is never mounted

`internal/admin` — 20 files, roughly 7,000 lines, heavily unit-tested: key issuance,
`/key/block` and `/key/unblock`, budgets, users, teams, spend reporting, the audit trail,
`/admin/config/reload`, and the embedded operator UI with its session handling. It has
**zero importers anywhere in the tree**:

```
$ grep -rln "dorang/internal/admin" --include=*.go .
(no output)
$ grep -rn "admin\.New(" --include=*.go .
(no output)
```

`cmd/dorang/main.go` builds an `*app.App` and serves it; `internal/app/app.go` never
imports `internal/admin`. The live route set is `internal/server/handlers.go:baseRoutes()`
plus `internal/app/batch.go:batchRoutes()` (`internal/app/app.go:317`). `/key/`, `/user/`,
`/team/`, `/model/`, `/budget/`, `/spend/` and `/global/spend/` are listed in
`internal/server/server.go:492-497` as *planned* prefixes that answer
`501 route_not_implemented`.

The operational consequence is worth stating plainly rather than as a category: **there is
no way to revoke a leaked API key through the API.** An operator following the documented
incident-response step — `POST /key/block` — gets a 501. The only remaining levers are a
direct `UPDATE` against `api_keys` followed by a cache invalidation the API also cannot
trigger, or a process restart.

Everything I found inside that package is therefore latent rather than live, and I am
reporting it here rather than as findings above:

- Route dispatch, `OPTIONS` handling and the 405/501 answers all precede
  `a.authenticate(r)` (`internal/admin/api.go:252-281`).
- Authorization is a single global bit with no team scoping — the master credential, or any
  user whose role is in `knownRoles` (`internal/admin/api.go:300-320`,
  `internal/admin/users.go:90-96`). There is no team-admin concept.
- Every listing and info endpoint takes the subject id **from the request** rather than
  from the authenticated principal: `spendLogs` (`internal/admin/spend.go:299-309`),
  `teamInfo` (`internal/admin/teams.go:188-199`), `keyInfo`/`keyList`
  (`internal/admin/keys.go:392-543`). Combined with the previous point, mounting this
  as-is gives every `admin_viewer` read access to every tenant's spend, keys and budgets.
  The comment at `internal/admin/api.go:301-303` frames the absent permission model as a
  considered choice; it is defensible only while every admin is a full admin, which is not
  what `admin_viewer` implies.
- `admin.Hasher.Label` (`internal/admin/deps.go:163-173`) documents a non-reversibility
  contract for key display labels and has **no production implementation** — only a test
  fake (`internal/admin/fakes_test.go:105-107`). The wired-up equivalent,
  `store.LabelFor`/`labelFromLookup` (`internal/store/keys.go:138-151`), is correct: it
  derives the label from the first 8 hex characters of the lookup digest, never from the
  secret's own characters, with an explicit comment naming the incumbent mistake DESIGN
  §2.4 warns about.

Fix the wiring first, then the three points above, in that order. Wiring it up without
them converts three latent findings into live ones on the same commit.

### 2. Enforcement fields that reach no enforcement

> ⚠️ **Re-derived 2026-08-03: eight of these nine rows are closed and only one carried a
> closure marker.** The `usage_probe` row was struck when it was fixed; the other seven were
> fixed and left reading as open, which is the direction this document's own correction section
> calls the worse one — a reader who spot-checks one row, finds it stale, cannot tell which of
> the rest are real. Each row below now carries its own disposition and the call site that
> settles it. **`Result.QuotaUsedPct` is the one that is still open**, and it is stated as such
> with the grep that establishes it.

| Control | Defined | Read by | Consequence |
|---|---|---|---|
| `Limits.RPMLimit` | `internal/auth/principal.go:47` | ~~`principal.go:165` only, and `Access.ObservedRPM` is never assigned~~ → `internal/app/rates.go` | ~~per-key/user/team request rate is unlimited~~ **Closed.** `rates.go` is the missing producer — a rolling minute keyed by `kind+":"+id`, so a team ceiling counts across the team rather than against one of its keys. Its own doc comment records that the auth package's tests passed by setting the field themselves, which is §17.1 rule 1 |
| `Limits.TPMLimit` | `internal/auth/principal.go:49` | ~~`principal.go:168`, same~~ → `internal/app/rates.go` | ~~per-key/user/team token rate is unlimited~~ **Closed**, same producer |
| `Limits.MaxParallel` | `internal/auth/principal.go:52` | ~~nothing — the identifier does not occur in `internal/capacity`~~ → `internal/app/principal_policy.go:35` sets `rr.PrincipalMax` from `principal.maxParallel()` | ~~per-subject concurrency ceiling is unlimited~~ **Closed.** The ceiling reaches the broker as the principal axis's per-request maximum |
| `capacity.{global,provider_groups,credential_groups,models,principals}.{rpm,tpm,max_queue,max_queue_wait}` | `internal/config/config.go:193-198`, `:235-241` | ~~only `MaxConcurrency`/`MaxConcurrent` is copied~~ → split in two | ~~four ceilings on every capacity axis load cleanly, warn about nothing, and enforce nothing~~ **Closed in both directions, which is why the row is worth keeping.** `rpm`/`tpm` are a **load error** now (`refuseRate` in `internal/config/validate.go`), naming `deployments[].limits[]` and the key's own `rpm_limit`/`tpm_limit`; `max_queue`/`max_queue_wait` are **wired** — `internal/app/build.go:95-108` fills `capacity.Config.Queues` and `internal/capacity/broker.go:380-400` reads it. A ceiling that loads and enforces nothing was the finding; neither half is that any more |
| `providers[].usage_probe.*` | validated in `internal/config/validate.go` | ~~`quota.NewRegistry` and `Meter.AttachTracker` have no non-test callers~~ → `internal/app/usageprobe.go` | ~~`usage_probe.enabled: true` is accepted and inert; every quota decision silently runs local-only, understating usage for any credential also used outside dorang (DESIGN §6.2)~~ **Closed 2026-07-29.** A prober per enabled provider, a `quota.Tracker` per credential with a rule to gate, polled off the request path; a `fetcher` no prober exists for is a start-up refusal. `TestProviderReportedQuotaReachesTheMeter` in `internal/app` drives a provider-reported figure to a credential's own refusal with no local traffic at all |
| `router.Request.Tenant` | `internal/router/request.go:82-84` | ~~`router.go:642`; assigned only in `testing/scenario/harness.go:421`~~ → `internal/app/dispatch.go:478` assigns it on the request path | ~~session pins are shared across tenants~~ **Closed** — finding 5 in the table below |
| `budgetGate` on the batch path | `internal/app/budget.go` | ~~`internal/app/dispatch.go:117` only~~ → `internal/app/batch.go:290` calls `budget.reserveBatch` per row | ~~batch spend is unbudgeted~~ **Closed** — finding 1 below, pinned by `TestBatchExecutionIsBudgeted`. The grep this finding quoted (`grep -n "udget" internal/batch/*.go internal/app/batch.go …` returning nothing) now returns the hold, the settle and the row pricer |
| `Result.QuotaUsedPct` | `internal/server/deps.go` | `headers.go` — **and nothing anywhere assigns the map outside `internal/server`'s own tests** | **STILL OPEN, re-derived at `5447598` rather than carried.** `x-dorang-quota-*-used-pct` has never been emitted by any response. Two documents depend on it and neither says so: DESIGN §10.4 lists it as an extension header, and OPERATIONS §11.3 step 3 instructs an operator debugging a probe to *read it off a response* — a troubleshooting step that cannot succeed. It is the third header family in `stampHeaders` whose renderer is reached and whose value has no producer, beside `Result.RateLimit` and `Result.RetryAfterSeconds` (DESIGN §10.4's box, §17.1). The latent header-*name* injection noted originally is unchanged and stays latent for the same reason: nothing feeds it |
| `config.KeyRotation.Strategy` | validated at `internal/config/validate.go` | ~~nothing outside `internal/config`~~ → `internal/app/build.go` parses it onto `router.Config.Rotation`; `Router.rotate` applies all four names | ~~key-pool rotation strategy is inert~~ **Closed**; CONFIG §23.1a, and CONFIG §9's own row asserted the opposite until 2026-08-03 |

Two of these deserve a note on how the gap survived. The RPM/TPM check is *unit-tested* —
`internal/auth/auth_test.go:478-479` injects `a.ObservedRPM, a.ObservedTPM = 10, 100`
directly to make it trip — while `internal/app/authn_test.go` contains no reference to
either field. The test proves the check works given its input and nothing proves the input
arrives. That is the same test topology the already-fixed budget bug had, and it will keep
producing this defect until an integration test asserts enforcement end to end rather than
asserting the enforcing function in isolation.

### 3. Whole subsystems with no callers

- **`internal/auth`'s OAuth subsystem.** `NewOAuthCredential`, `OAuthManager`, the
  single-flight refresh, the atomic token-store write, the 401 backoff gate — every
  mechanic DESIGN §11.2b spends eighty lines specifying — has no caller outside
  `internal/auth` and its tests, and `internal/config.Credential` has no field that could
  turn it on. `internal/app/build.go:129-166` (`collectCredentials`) reads only a static
  `Key.Value()`. OAuth credentials cannot be configured, so §11.2b describes a feature the
  binary does not have.
- **`internal/backend`** (`client.go`, `credential.go`, `endpoint.go`, `provider.go`) —
  no importer outside itself.
- **`internal/probe`** — untracked in git (`?? internal/probe/`), so this is
  work in progress rather than a defect. Noted because it is the best secret-handling code
  in the tree and nothing uses it (see the §11.3 finding above).
- **`internal/luaext`** — skeleton, no importers; `extensions.lua.*` is validated and inert.
- **`quota.Ranker` / `Meter.Allowance`** (`internal/quota/urgency.go:262`, `:353-391`) —
  DESIGN §7.5a(c)'s expiring-quota preference. Callers only in `urgency_test.go`. Not a
  security control; the cost is a bill that is quietly higher than it needs to be.
- **`store.ImportKeys`** (`internal/store/import.go:236-296`) — the identifier hygiene
  (`validIdent` + `quoteIdent`) is correctly built and has never been exercised by a real
  caller, because no CLI subcommand or HTTP route reaches it. Re-audit it the moment
  `opts.Table` or the `src *sql.DB` becomes a flag or a request value.
- **`quota.Budget`** (`internal/quota/budget.go`) — no production callers, and this one is
  *correct*: it was deliberately superseded by the durable `cluster.Ledger`, which is
  properly wired at `internal/app/dispatch.go:117-149`. Listed only because
  `internal/quota/doc.go:87-106` still describes it as "the" budget mechanism, which will
  mislead the next person doing this sweep.

### 4. Rules implemented twice, where only one copy runs

- `auth.Strip` (`internal/auth/header.go:124`) has no non-test caller. The stripping that
  actually runs uses a separate implementation, `server.StripAuthHeaders`
  (`internal/server/headers.go:55`), over a separately maintained list. Both packages'
  documentation — `internal/auth/doc.go:9`, `internal/auth/authenticator.go:352-353`,
  `internal/auth/oauth.go:362` — describes `auth.Strip` as the function called at the
  forwarding point. It is not. A change made in `auth.Strip` on the strength of those
  comments would do nothing. See the MEDIUM finding on the two header lists.
- Four separate `http.Client` constructors each independently set `CheckRedirect`:
  `internal/server/passthrough.go:147`, `internal/app/upstream.go:115`,
  `internal/shadow/reference.go:54`, `internal/backend/client.go:19`, plus a fifth in
  `internal/probe/http.go:22`. All five are currently correct. The security property is
  restated five times rather than held in one place, which is how the original redirect
  defect became possible and how the sixth one will.
- `SecretRef.MarshalYAML` versus `SecretRef.resolve`: two mechanisms for the same
  invariant, and the one the reader would expect to be load-bearing is the one that never
  runs (see the `dorangctl import config` finding).

---

## What a reviewer could not verify

Stated plainly rather than implied by omission.

- **The working tree changed during the review.** `internal/probe` (seven files, ~55 KB)
  did not exist when I enumerated `internal/` at the start and was present, untracked, by
  the end. I read `doc.go`, `scrub.go` and the error/read sites in `http.go`; I did **not**
  read `probe.go` (26 KB), `decode.go`, `deepseek.go` or `zai.go`. Any finding about that
  package is partial, and other files may have moved under me without my noticing.
- **No code was executed except one CLI reproduction.** I built and ran
  `go run ./cmd/dorangctl import config` against a locally authored file containing a
  fabricated key, offline, to confirm the config-import finding. Nothing else was run: no
  test suite, no fuzzer, no server instance, no request against a live gateway, and no call
  to any external service. Every other finding is derived from reading source and
  documents; the attack paths are constructed from control flow, not demonstrated.
- **Two findings I would want confirmed on a running instance before acting.** The
  authenticator merge amplification — I have reasoned about the constant factor and not
  measured it, and the practical severity depends on how many keys a deployment loads via
  `Load()`. And the batch model-allow-list bypass: I read the handler, the scheduler's
  dispatch and the call graph, but did not exhaustively read `internal/batch/validate.go`'s
  row-validation path, so there is some chance of a check I missed. The budget half of that
  finding is certain — `grep` for "udget" across `internal/batch/*.go`,
  `internal/app/batch.go` and `internal/app/batchstore.go` returns nothing.
- **`internal/auth/oauth.go` (870 lines) and `internal/auth/tokenstore.go` (554 lines) were
  spot-checked, not audited.** The redaction discipline holds where I looked: `scrub`/
  `scrubLocked` (`oauth.go:651-671`) is applied on the refresh path, `execStore.Load`
  (`tokenstore.go:476-501`) discards command output and builds errors from the exit status
  alone, and every secret-bearing type redacts under every `fmt` verb. I did not verify the
  §11.2b claim that "every token the credential has held is scrubbed from anything
  recorded" against every recording site, nor the atomic-write and single-flight
  correctness. Since the subsystem has no caller (above), this is lower priority than it
  would otherwise be.
- **`internal/cluster` was read only for SQL construction.** Distributed-lock, leader-election
  and overshoot semantics were not reviewed. DESIGN §5.6 publishes an overshoot bound of
  `block × (nodes−1)`; a budget ledger that is correct in-process and wrong across nodes
  would not have been caught here.
- **The Lua extension surface (§11.5) and the email path were not reviewed.** Config
  validation requires instruction, memory and wall-clock ceilings
  (`internal/config/validate.go:740-746`), which is a good sign, but a sandbox needs its own
  review rather than a passing glance. The package is currently a skeleton with no importers.
- **Wire-adapter fuzzing was not attempted.** `internal/canonical/gatediff_test.go` and
  `internal/server/fuzz_test.go` appear to target exactly the gate/adapter divergence class;
  I read the production code they exercise but did not run them or extend their corpus. Under
  manual case analysis the gate and the adapters agree on `model` and `stream` for duplicate
  keys, `null` values, escaped keys and invalid UTF-8, and the one divergence I could
  construct — trailing content after the top-level object — is rejected by both. I would not
  call that class closed on manual analysis alone.

### Areas checked that produced no finding

Recorded so the coverage claim is falsifiable, not as reassurance.

- **SQL injection: none, and the guarantee is structural.** Every query in `internal/store`
  and `internal/cluster` goes through `Store.exec/query/queryRow/txExec`
  (`internal/store/store.go:265-298`) or `cluster.conn/tx.exec/query/queryRow`
  (`internal/cluster/sql.go:33-61`), which pass values as `args ...any` and rewrite
  placeholders per dialect. No package outside those two touches `database/sql`. Dynamic
  identifiers appear only for rollup tables and partitions and come from closed, hardcoded
  literal sets. The closest thing to a problem is
  `internal/store/partition.go:202-207`, the one `fmt.Sprintf`-into-SQL in the tree, whose
  two identifier arguments are safe today only because its three call sites pass literals —
  and which, unlike its neighbour at `partition.go:236`, carries no allow-list assertion to
  catch a future regression.
- **Metrics cardinality: no caller-controlled labels anywhere.** `internal/server/metrics.go`,
  `internal/admin/metrics.go` and `internal/shadow/metrics.go` are fixed-cardinality
  `atomic.Uint64` fields. There is no Prometheus client library in the tree at all, so no
  `WithLabelValues(callerString)` pattern exists to exploit.

  ⚠️ **This bullet named three files and there were four.** `internal/metrics` is labelled, and
  its `model` label was taken from the request body (or the deployment path segment) and metered
  verbatim on every request *including refused ones* — so a key permitted exactly one model could
  mint label values by asking for models it may not have. Memory and scrape size were bounded, by
  a series cap; **resolution was not**. Entries are never evicted, so 128 fabricated names spent
  the per-model table for the life of the process and every model first seen afterwards —
  including a real one a config reload had just added — folded into `__overflow__` and lost its
  duration, TTFT and prefix-hit series until a restart. A permanent, remotely triggerable denial
  of resolution on the operator's dashboard; serving was never affected.

  Closed by structural admission: `metrics.Requests.SetAdmittedModels` is handed the configured
  model set (`models[]` plus `aliases`) at assembly **and on every reload**, and a name outside it
  becomes `__unknown__` — one series regardless of how many names are asked for, so a flood
  occupies nothing. The per-model cap is now a floor raised to hold the whole configured set,
  which is what makes the reload case work. `TestTheConfiguredSetBoundsTheModelLabel` and
  `TestAModelAddedAfterAFloodStillGetsItsOwnSeries` (`internal/metrics`) and
  `TestModelLabelAdmissionFollowsAReload` (`internal/app`) are the guards; OPERATIONS.md §6 carries
  the operator-facing version and the migration note.

  The rule the bullet should have stated, and now does: **a label value is drawn from
  configuration, never from a message** — not a request body, not a response body, not a URL path
  segment, not an upstream error string. It is written where it is enforced, in
  `SetAdmittedModels`, so the next label added has to answer it.
- **Metering queues cannot become backpressure.** `internal/meter/ring.go:83-110` and
  `internal/meter/spool.go:55-69,372-402` are fixed-size and fail by dropping, never
  blocking; drops are counted and flip a `metering_degraded` flag.
- **The prefix table's byte budget holds.** `internal/prefix/table.go:125-188` evicts
  synchronously past `MaxBytes` (64 MiB default). A caller flooding unique prefixes churns
  the cache and burns eviction CPU but cannot grow memory. (Its *keying* is the separate
  HIGH finding above.)
- **Batch object ownership is correct.** Ids are 96 bits of `crypto/rand`
  (`internal/batch/service.go:380-391`), and `Retrieve`, `Cancel`, `List`, `ListFiles`,
  `FileContent` and `DeleteFile` all filter on `principalID(rq)` taken from the
  authenticated principal, never from a request parameter (`internal/app/batch.go:344-486`).
  No IDOR. The bypass in the CRITICAL finding is about *what model a batch may name*, not
  about whose batch a caller may read.
- **The client's credential never reaches a provider on the dispatch path**, because
  `attempt` builds a fresh `http.Request` and copies no client headers
  (`internal/app/dispatch.go:262-276`). The passthrough path copies headers and strips all
  six accepted names in both directions (`internal/server/passthrough.go:396-397`, `:422`).
- **The shadow comparator is careful.** An allow-list of forwarded headers rather than a
  deny-list (`internal/shadow/reference.go:39-47`), a belt-and-braces `stripAuth` after it,
  a loop guard header, and a diff report that records header presence but never header
  values (`internal/shadow/diff.go:76-81`).
- **`peekRequest`'s W10 hardening is sound as far as manual analysis goes.** The escaped-key
  fallback runs whenever an escaped key was seen rather than only when no model was found,
  and it decodes into a map rather than a tagged struct — both of which are the right
  choices and both of which the comments explain. The only defect I found there is a
  performance one (the MEDIUM above).
- **The passthrough path traversal defence holds.** Validation runs on the decoded path so
  one rule covers both spellings (`internal/server/passthrough.go:322-344`), the joined path
  is re-checked against the base after normalization (`:296`), `/anthropicX` cannot be served
  by the `/anthropic` prefix (`:280-283`), and hop-by-hop stripping follows the `Connection`
  header's own list rather than a fixed one (`:175-186`). I could not construct a traversal
  or an unmapped-prefix reach through any encoding I tried.

---

# Dispositions

Written by the engineer who acted on the findings above. The findings themselves
are unedited; this section records what was done, what was not, and what the
review missed.

Baseline: commit `207d622`, on the branch the fixes were written on.

> **This section was, for a time, false of `main`.** The document was copied to
> `main` without the code — see "# Dispositions, third pass" at the end, which is
> the authoritative statement. Every claim below has since been **re-verified
> against the merged tree** by reverting the fix in place and watching the named
> test fail; where the merge changed where a fix lives or what it does, the text
> below has been corrected to match the binary rather than left as written. The
> third pass carries the full revert-and-fail table and the list of what the
> first two passes got wrong.

A test that passes in both directions is recorded as such rather than counted.

## The unifying diagnosis, applied

The review's headline — *a control that exists and is not reached* — was treated
as the specification. Three of the nine instances were closed by making the
omission impossible to express rather than by adding a call site:

- **`server.Route.ModelAuth` is a required field.** Its zero value,
  `ModelAuthUnset`, is refused by `newRouteTable`, as is `ModelAuthGate` on a
  route with no body (the shape that let passthrough look authorized while
  enforcing nothing). A route cannot reach the mux without an answer, and
  `TestEveryRouteDeclaresAModelAuthMode` walks the real route set — base routes,
  batch routes and compiled passthrough prefixes — rather than a fixture.
- **`ModelAuthHandler` carries a post-condition.** `Server.serve` fails a
  request whose handler declared it would name its models and did not. The
  omission is now a 500 with `model_auth_missing` and a log line, not a silent
  dispatch.
- **`execIdentity` cannot be constructed without the allow-list decision.** The
  batch executor needs one to reach its upstream call, and
  `ownerResolver.identity` refuses to return one for a model the owner may not
  use. There is no ordering in which the call happens and the check does not.

Two more were made *structural* in a weaker sense — the type no longer has a
field the defect can live in:

- `batch.UploadFile` refuses a `PurposeBatch` upload that carries no
  `ModelAuthorizer`. A nil authorizer is a 400, not a permissive default.
- `Credential`/`RotationKey` marshal through a shape with no field an inline
  literal can land in, so `MarshalYAML` cannot leak by forgetting to omit one.

The rest are honest patches, marked as such below.

## Finding-by-finding

Status is as of the merge. "Closed" means a named test fails when the fix is
reverted in the merged tree — the third pass lists the exact edit for each.

| # | Finding | Status | Kind |
|---|---|---|---|
| 1 | [CRITICAL] batch bypasses the model allow-list and the budget gate | **Closed**, verified | structural |
| 2 | [HIGH] `rpm_limit`, `tpm_limit`, `max_parallel_requests` enforce nothing | **Closed**, verified — and it was NOT closed on `main` until the merge; the per-team half was a live defect, see the third pass | patched + wired |
| 3 | [HIGH] upstream error message copied into the client envelope | **Closed**, verified | structural |
| 4 | [HIGH] a team budget enforced per key | **Closed**, verified | patched |
| 5 | [HIGH] tenant isolation of cache affinity is false as shipped | **Closed**, verified | patched |
| 6 | [HIGH] unauthenticated key flood drives an O(keys) copy per request | **Closed**, verified — first half only; the store-lookup half is open | patched |
| 7 | [HIGH] non-streaming upstream read is an unbounded `io.ReadAll` | **Closed**, verified — in `internal/backend`, and the test was strengthened to bound the READ | patched |
| 8 | [HIGH] passthrough dispatches with no model check | **Closed**, verified | structural |
| 9 | [MEDIUM] `SecretRef.MarshalYAML` never runs | **Closed**, verified — by `Credential`/`RotationKey`, not by the method the finding names | structural |
| — | [MEDIUM] transport error text names internal hosts | **Closed**, verified | patched (bundled) |
| — | [MEDIUM] `x-dorang-native-error-type` unvalidated and unbounded | **Closed**, verified | patched (bundled) |
| — | [MEDIUM] passthrough relays upstream `Set-Cookie` | **Closed**, verified | patched (bundled) |
| — | The administrative control plane is not mounted | **Fixed** — mounted, with its three latent defects closed first | see the third pass |
| — | [MEDIUM] `batch.ownedBy` treats an unowned record as public (found while fixing) | **Closed**, verified | patched |

### 1 — CRITICAL, batch

Two independent enforcement points, one of which every dispatched row must pass.

- **Upload time**, synchronous: `batch.UploadRequest.Authorize` is threaded into
  `validateConfig` and consulted per row by `validateRow`, with its own code
  `model_not_allowed` (distinct from `model_not_found`, so a refusal does not
  tell the caller which models exist). `internal/app` supplies a closure over
  `server.Request.AuthorizeModel`, so batch rows and interactive requests reach
  the same implementation.
- **Dispatch time**, per row: `batchExecutor.Execute` resolves the batch's
  `OwnerKeyID` back to an `*auth.Principal` through the same
  `recordFromAPIKey` conversion `internal/auth` uses, then checks the allow-list
  *and* `Authorize` (so a blocked or expired credential stops a running batch)
  *and* takes the budget hold. The resolution is cached for 30 s and capped at
  1024 owners.

The budget half is the same gate the interactive path uses:
`budgetGate.reserveFor` is now reached from both, and a refusal is wrapped
terminal so `internal/batch` does not retry a spent ceiling through its backoff.

Tests: `internal/batch/modelauth_test.go`,
`internal/app/security_test.go` (`TestBatchExecutorRefusesAModelTheOwnerMayNotUse`,
`TestBatchExecutorRefusesABlockedOwner`, `TestBatchExecutionIsBudgeted`).
Pre-fix: 200 rows ran against a 1 USD budget with 200 upstream calls and no
refusal.

**Not closed by this:** a batch already validated under one allow-list, then
dispatched after the key's `models` column is *widened*, uses the widened list —
the executor reads current limits, by design. Narrowing is enforced; that is the
direction that matters.

### 2 — HIGH, rate and concurrency ceilings

`auth.Access` gained `Rates auth.RateSource`, and `Limits.authorize` now takes
the subject's **id** so a team ceiling is compared against the team's counter.
The single `ObservedRPM`/`ObservedTPM` pair is kept as the one-subject fallback
and is what the pre-existing unit test still exercises — that test passes before
and after and closes nothing, which is precisely the topology the review named.
The tests that close it are end-to-end through `app.principal.Authorize`.

`internal/app/rates.go` is the counter. As merged it is a **rolling** minute —
sixty stamped one-second buckets per subject, sharded sixteen ways, capped at
100 000 subjects with cold-entry eviction — keyed `kind:id`, so all three
subjects are counted per request and a flood of distinct subject ids is bounded
rather than merely reset. (The branch's version was a tumbling minute with the
map dropped whole on rollover; `main` had independently written the rolling one
for the per-key sweep, and the merge kept the rolling window and re-derived it
per subject. The third pass records why.) Tokens are added at settlement, from
`App.recordMetrics`, which is the one production site every finished request
passes.

`max_parallel_requests` reaches the broker through
`capacity.Request.PrincipalMax`, combined with the static
`capacity.principals` table by taking the more restrictive — a per-key ceiling
can tighten a deployment default and never raise it.

Tests: `TestRPMLimitIsEnforced`, `TestTPMLimitIsEnforced`,
`TestTeamRPMCountsEveryKeyUnderTheTeam`, `TestRateWindowRolls`,
`TestSettledTokensReachEverySubjectOfTheRequest`,
`TestRPMLimitRefusesThroughTheWholeStack` and `TestTPMLimitCountsFinishedTokens`
(both through the assembled stack, against a real upstream),
`TestMaxParallelReachesThePrincipal`, and `TestPrincipalMaxTightensTheAxis` /
`TestPrincipalMaxNeverWidens` in `internal/capacity/queue_limits_test.go`.

**Mitigated, not closed:** `tpm_limit` bounds the *next* request, because the
token count does not exist until settlement. A single enormous request can
exceed the ceiling once. The rate counters are also per process; a multi-node
deployment enforces N times the ceiling. The durable ledger exists and would
close that, at a store write per request — not taken.

### 3 — HIGH, §11.3

`Normalize` has no assignment from a decoded body to `Error.Message` on any
branch. The upstream's text goes to `Error.NativeMessage`, bounded at 512 bytes,
and the body gets `canonicalMessage(status)`, a function whose only input is the
status. A new branch cannot leak by forgetting a rule; it would have to write
the leak.

The scrubber runs in `internal/backend`, which is the layer that makes the HTTP
call after the L5 extraction. `upstreamError` collects what was actually put on
the outbound credential headers — `collectSecrets`, which reads the headers
rather than the credential table, so an OAuth token applied by code it does not
own is caught too — and scrubs it out of `NativeMessage`, the native type and the
code before anything renders or records them, so the ledger copy is scrubbed too.
`internal/redact` exists as the shared implementation; `internal/app` no longer
applies it, because `internal/app` no longer touches an upstream response.
`internal/probe`'s own `scrubber` is a third copy and was left alone; it should
be switched to `internal/redact` when that work lands.

The bundled MEDIUMs went with it: `WriteError` applies `safeHeaderValue` and a
128-byte clamp to the upstream-controlled `NativeType`, and `transportError` logs
the transport failure instead of relaying `err.Error()` — which for a dial
failure names the operator's internal host, port and resolved address.

Tests: `internal/server/errorleak_test.go` (all seven envelope shapes),
`TestAHostileUpstreamCannotEchoTheCredentialBack` (end to end, a backend
answering with the `Authorization` header it was handed),
`TestUnreachableUpstreamDoesNotNameInternalHosts`,
`TestNativeErrorTypeHeaderIsCheckedAndClamped`.

**A consequence, found by the merge:** `Error.Message` is now
`canonicalMessage(status)` on every branch, so anything that *classified* on the
upstream's words had to move to `NativeMessage` with it.
`internal/app/estimate.go`'s `upstreamCause` did not, and §10.5a's "route to a
larger window" had silently stopped being reachable from an upstream signal.
`TestAnUpstreamOverflowIsRecognisedFromTheBody` pins it.

**Deliberately not done:** the review suggested an operator flag to relay an
excerpt for debuggability. Not added — it is a flag whose only effect is to
re-enable the vulnerability, and the native text is in the ledger and the log.

### 4 — HIGH, budget subject

`budgetSubjectsOf` returns one subject per declaring subject, each with its own
limit *and its own period*, and `reserveFor` takes one hold per subject,
releasing everything already taken if a later one refuses. The secondary defect
— minimum limit paired with the first non-empty period — is closed by the same
change, and has its own test.

Tests: `TestTeamBudgetIsNotMultipliedByTheNumberOfKeys` (100 requests across 10
keys under one 1 USD team ceiling; pre-fix, none refused),
`TestEachBudgetSubjectKeepsItsOwnPeriod`.

### 5 — HIGH, tenant scoping

`router.Request.Tenant` is assigned in production, from
`tenantOf(rq.Principal)`: team, else user, else key, each prefixed so a team id
and a user id cannot collide. `prefix.NewChain`/`Compute` take the tenant and
seed `h₀ = H(tenant ‖ 0x00 ‖ group)`; the separator is tested, because without
it a tenant could choose a group name that seeds like another tenant's.

`internal/batch`'s `groupHash` passes an empty tenant deliberately — that digest
orders one batch's own rows and never reaches the routing table.

This is a **deviation from DESIGN §7.4b as written** (`h₀ = H(group_id)`). The
review is right that the design is the defect. §7.4b should be amended.

Tests: `internal/prefix/tenant_test.go`, `TestRouterRequestCarriesTheTenant`.

### 6 — HIGH, authenticator flood

`insert` counts promotable entries and merges on those; negatives have their own
cap (`maxNegative = 1024`) and are dropped alone, so a flood costs the
attacker's own cache and not the legitimate credentials learned beside it.

Measured on the pre-fix code by the new test: **1937 snapshot merges per 2000
unknown keys**, which matches the review's arithmetic. After: single digits.

Tests: `internal/auth/flood_test.go`.

**Not closed:** the review's second recommendation — rate-limit or coalesce
*store lookups* for unknown keys per source — was not implemented. One
`LoadByLookup` per unauthenticated request remains, and against SQLite that is
its own amplification factor. The negative TTL bounds repeats of the same key
and nothing bounds distinct ones. **This is the largest thing left open in this
pass.**

### 7 — HIGH, unbounded read

`backend.readUpstreamBody` reads one byte past `Options.MaxResponseBytes`
(default `backend.DefaultMaxResponseBytes`, 32 MiB, matching the request cap) and
answers `502 upstream_response_too_large`, not retryable — a fail-back hop would
buffer another one. It is in `internal/backend` because that is the package that
makes the call; the batch executor reaches it through the same `backend.Do`, so
the second unbounded read the review did not mention is closed by the same code
rather than by a second copy of it.

`MaxResponseBytes` is an option on the backend but is **not yet surfaced in
YAML**. `server.max_body_bytes` had the same gap and no longer does (CONFIG §2),
which leaves this one on its own.

Tests: `TestOversizedUpstreamResponseIsRefused` — which now counts what the
reader was ASKED FOR, because the first version of it passed against an
implementation that read the whole body and measured it afterwards.

### 8 — HIGH, passthrough

Option (b), bounded peek, rather than the review's preferred option (a): a
startup refusal cannot be computed, because keys live in the database and their
`models` column changes at runtime, so "no configured key carries an allow-list"
is not a fact `compilePassthrough` can know.

`authorizePassthroughModel` reads up to 1 MiB, hands the bytes straight back to
the relay via `io.MultiReader` (the forwarded request is byte-identical — there
is a test), and calls `Request.AuthorizeModel` on what it finds. When the model
cannot be established — non-JSON body, body past the peek window, JSON that is
not an object — the answer comes from the principal: **refused if it restricts
any model, allowed if it restricts none.** A WebSocket upgrade is refused
outright for a restricted key, because frames are never decoded.

`server.ModelRestricted` is the optional interface that answers "is there a
list". A `Principal` that does not implement it is treated as restricted:
fail-closed, so a future implementation that merely forgets a method costs
availability rather than enforcement.

Both refusals are **403 `permission_error`**, per COMPATIBILITY §11.2 — the
credential authenticated and is simply not permitted this model, and a 401 tells
a client to re-authenticate over a body it should instead re-encode. The branch
answered 401 citing §7.2, which is about tag-routing misses; the shared
authorization gate has always answered 403 for the same refusal, so one rule had
two answers and only the unreachable one was wrong.

Tests: `internal/server/passthrough_security_test.go`.

**Mitigated, not closed:** a restricted key with a legitimate non-JSON
passthrough body (audio upload, image edit) is now refused. That is a functional
regression on a path that previously bypassed the allow-list entirely, and it is
the fail-closed direction, but an operator will notice it.

### 9 — MEDIUM, `dorangctl import config`

Two changes, and the finding's own title names the wrong method.
`SecretRef.MarshalYAML` **never runs** — that is the finding — and it still never
runs: it can be renamed out of existence and every marshalling test passes. The
fix is one layer out. `Credential` and `RotationKey` have explicit `MarshalYAML`
methods whose target shape has no field an inline literal can occupy, so the
`,inline` flattening cannot reach it. And `internal/config/import.go` no longer *creates*
inline literals: a literal `api_key` in the source becomes
`key_env: DORANG_<PROVIDER>_API_KEY` plus a warning naming the variable. The
secret is not echoed into the warning either — the fix for "a secret ended up
somewhere it should not" is not to print it somewhere else.

Tests: `internal/config/secretmarshal_test.go`,
`TestImportWarnsAboutALiteralKey` (rewritten to assert the literal is absent).

**Behaviour change for operators:** `dorangctl import config` output no longer
starts a server without the operator setting the named environment variables
first. That is intended.

## `internal/admin` — mounted

**This section said "Left unwired", and that is no longer true.** It is mounted,
with its three latent defects closed first — read-before-auth, a team-scoped
administrator derived from `api_keys.team_id`, and request-supplied id filters
checked rather than trusted. The reasoning for deferring it stands as written:
wiring it without those three would have converted three latent findings into
live ones on one commit. They were fixed, then it was wired. The third pass has
the detail and the tests.

### Revoking a leaked key

`POST /key/block` serves. `TestALeakedKeyCanBeRevokedThroughTheAPI`
(`cmd/dorang/revoke_test.go`) runs the incident end to end over HTTP: a working
key, one administrative call, and the next request from that key refused, with
the audit row read back from the database. The refusal is observed within
`DefaultEntryTTL` (60 s), because `internal/auth` caches a loaded credential —
the test waits for it rather than asserting instantly, since the operator's
question is "how long until it stops working" and the answer has to be a bounded
number rather than "never".

`POST /key/regenerate` replaces the secret outright, and `POST /key/rotate`
replaces it with a grace window (§11.2c). Both go through `store.RotateKey`;
regenerate follows it with `EndGrace`. There is no second write path, because the
one that existed wrote `api_keys`' denormalized verifier columns while
authentication resolves through `api_key_secrets` — a regeneration that
regenerated nothing, behind a 200.

The SQL break-glass path still works and is still the right answer in one case:
when the administrative credential itself is what leaked.

```sql
UPDATE api_keys SET blocked = 1 WHERE id = '<key id>';
```

`internal/auth` caches a loaded credential for `DefaultEntryTTL` (60 s), so the
key stops working within a minute of the write, and `Authenticator.use` enforces
`Blocked` on every request independently of `Authorize`. Find the key id without
the secret: the label is derived from the first 8 hex characters of the lookup
digest (`store.LabelFor`), so
`SELECT id, key_label, user_id, team_id FROM api_keys WHERE key_label = ?`
identifies it from what a leak report usually contains.

**In-flight batches:** blocking the row also stops batches that key submitted,
within the owner-resolution TTL (30 s) — `batchExecutor` reaches `Authorize` per
row. Before this change, a batch submitted before a block kept spending until it
finished.

## What the review missed

Found while fixing, not in the findings above.

1. **`.gitignore` untracks the entire `cmd/` tree.** The patterns `dorang` and
   `dorangctl` are unanchored, so they match the *directories* `cmd/dorang` and
   `cmd/dorangctl` as well as the built binaries. `cmd/dorang/main.go` — the
   gateway's entry point — has never been in version control; `git status`
   shows a clean tree and a fresh clone does not build. Fixed by anchoring both
   patterns to the repository root. This is why the review could cite
   `cmd/dorang/main.go:173` for a file `git ls-files` does not list.

2. **The batch executor had the same unbounded `io.ReadAll`** as finding 7
   (`internal/app/batch.go`, pre-fix line 266). The review found the interactive
   one only. Both now use `readUpstreamBody`.

3. **`batch.ownedBy` treated an unowned record as public.**
   `ownedBy(recordOwner, owner)` returned true when `recordOwner == ""`, so a
   file or batch created with an empty `OwnerKeyID` was readable, usable and
   deletable by *every* key. A master-credential upload produced exactly such a
   record. **Fixed**, in both halves, because either alone is incomplete:
   `principalID` gives the master credential a reserved, non-empty owner
   (`app.MasterOwnerID`), and `ownedBy` no longer reads an empty `recordOwner` as
   everyone's. The alternative — "an unowned record is the master credential's" —
   was rejected because it leaves the WRITE path producing ownerless rows and
   relabels every legacy row as something the master credential did, which is a
   lie in an audit sense. The residue is the fail-closed direction: rows that
   already have an empty owner are now visible only to an administrative caller.
   Tests: `TestAnUnownedRecordIsNotVisibleToEveryKey`,
   `TestTheMasterCredentialOwnsWhatItCreates`.

4. **A batch row's body could name a different model than the row.** The
   scheduler carries `row.Model` (extracted at validation) and the executor
   decoded `req.Body` independently; the allow-list check would have been
   against one and the dispatch against the other. `batchExecutor.Execute` now
   refuses the mismatch. This was not exploitable pre-fix because nothing was
   checked at all, and would have become exploitable the moment a check was
   added to the wrong one of the two.

5. **A latent nil dereference introduced by the budget refactor, caught by its
   own test:** `estimate` reads the gate's clock, so an unconfigured gate must
   not reach it. Both entry points check `g == nil || g.ledger == nil` first.
   Recorded because it is the argument for the tests: the refactor was reviewed
   twice by eye and the panic was found by `TestBatchExecutorRunsAnAllowedModel`.

## Not addressed

Findings from the review left open, deliberately, in priority order for the next
pass:

- **Store lookups for unknown keys are unbounded** (finding 6, second half).
  The largest remaining item.
- [MEDIUM] request bodies buffered before the concurrency gate, and
  `max_response_bytes` not exposed in YAML. `server.max_body_bytes` is exposed now.
- [MEDIUM] no `ReadTimeout`/`IdleTimeout`, no body-read deadline — slow POST.
- [MEDIUM] `/v1/files` has no per-key quota, no file count limit, no expiry.
- [MEDIUM] embeddings relay does not apply the strict fold-collision filter.
- [MEDIUM] any escaped top-level key forces a full unmarshal on the hot path.
- [MEDIUM] two independent lists of accepted authentication headers.
- [LOW] `shadow.Options.ReferenceKey` unredacted; route table enumerable before
  authentication; WebSocket relays unmetered and untimed.

---

# The "configured but never applied" sweep

A separate pass over the twelve remaining controls that were validated, loaded,
and read by nothing — the defect class DESIGN §17.1 names as this codebase's
dominant one. Nothing here edits a finding above; where an earlier disposition
turned out to be wrong, it is corrected below by name.

## The correction that has to come first

**Finding 2 was recorded as closed and was not.** The table above says
`rpm_limit`, `tpm_limit`, `max_parallel_requests` — **Closed | patched + wired**,
and the prose describes `auth.Access.Rates auth.RateSource`, an
`internal/app/rates.go` holding a tumbling minute, `capacity.Request.PrincipalMax`,
and five named tests. None of it existed. `grep` for `RateSource` or
`PrincipalMax` returned nothing, `internal/app/rates.go` was not a file, and
`TestRPMLimitIsEnforced` appeared only inside this document.

That is worse than the defect it claimed to fix, because a closed finding is not
re-checked. It is also the same failure shape one level up: a disposition
satisfied on paper and connected to nothing. The rule that follows is the one
§17.1 already states for code, applied to the review — **a disposition that names
a file or a test is only closed once that file or test exists**, and the way to
hold it is to cite something a reader can run.

The control is closed now, and the implementation differs from what the
disposition described. It is written out below rather than left to match by
coincidence.

## What was wired

| # | Control | What it does now |
|---|---|---|
| 1 | per-key `rpm_limit` / `tpm_limit` | `internal/app/rates.go` keeps a rolling minute: sixty stamped one-second buckets, sharded sixteen ways, capped at 100 000 subjects with cold-entry eviction. `app.principal.Authorize` supplies the observed counters at the one production construction site of `auth.Access`, which omitted them, so every positive ceiling compared against a hard-coded zero. The request is counted at the gate (a ceiling enforced only on finished requests cannot refuse a burst) and the tokens at settlement. **Corrected by the merge:** as written here the window was keyed by api key ALONE and `auth.Access` carried one observed pair for three subjects, so a TEAM's ceiling was compared against one KEY's counter — a team limit multiplied by the number of keys under it. It is now keyed `kind:id` and read per subject through `auth.RateSource`; see the third pass |
| 1 | per-key `max_parallel_requests` | `capacity.Request.PrincipalMax`, combined with the static `capacity.principals` table by taking the **more restrictive**: a key's own ceiling can tighten a configured one and never widen it. It travels with the request because it is not in the file and changes when the key is edited |
| 2 | `capacity.*.max_queue` | A per-axis queue-depth ceiling in `internal/capacity`. Past it `Acquire` returns `ErrQueueFull` and leaves nothing enqueued. Before this the wait queue was an unbounded heap |
| 2 | `capacity.principals.<id>.max_queue_wait` | A wait budget on `Acquire`, returning `ErrQueueTimeout`. The router uses it for a **pinned** request, which is the request that genuinely has to wait — an unpinned one spills or falls back (§7.4a2, §7.6), and queueing it instead would trade a fast hop onto a healthy backend for a slow wait on a saturated one. It does **not** apply to batch: batch is the work that is supposed to wait (§11.1), and the thirty-second default every principal carries would have turned ordinary contention into failed rows |
| 3 | `metering_degraded` | `server.HealthReporter`, implemented by `app.meterHealth`. `GET /health` now carries `"metering":{"degraded":…,"reason":…,"dropped":…,"spool_bytes":…}` on every response, degraded or not — reporting only on failure leaves an operator unable to tell "not degraded" from "this build does not report it". It never changes the status code: losing traces is a data-quality failure, not a serving failure |
| 4 | `observability.prometheus` | False removes the `/metrics` route entirely, so it answers 501 like any other unserved route rather than 200 with an empty body |
| 4 | `/metrics` authentication | The route is `Admin` by default: the master credential or a principal implementing the new optional `server.AdminPrincipal`. `observability.metrics.public: true` opens it deliberately. It was `Public` alongside the container probes, which put per-key spend, per-credential quota state and the whole configured model list on an unauthenticated port |
| 5 | `key_rotation.strategy` | `router.Rotation`, applied in `Router.eligible` as the preferred credential. `failover` is the old behaviour, `round_robin` advances a per-deployment cursor, `least_used` asks the broker (`Broker.LeastUsedKey`, one lock for the whole pool), `random` picks uniformly. A credential pin and a sticky entry both outrank it: those are statements about a conversation, a rotation is a statement about load |
| 6 | `quota_urgency` | `router.StrategyQuotaUrgency`, added to **both** name lists, with a `compare` case ranking descending and a `Deps.Urgency` fed by `quota.Ranker`. The scorer, the occupancy damping and the per-node jitter all already existed and had no caller |
| 7 | `client_priority` / `range` | `capacity.principals.<id>.client_priority: allow` with a `range` of two class names, compiled into `router.PriorityConfig.Grants` and applied by `CanonicalFor(principal, …)`. The inbound hint is read from `X-Request-Priority`; an ungranted hint is reported in `x-dorang-dropped-params`, which §10.5 requires and which was silent before. The key's `priority_class` now reaches the router too — it was loaded, carried on the principal, and read by nothing but the Lua hook view |
| 8 | `DORANG_STATE_DIR` | `config.ExpandPath` resolves a leading `~` to it. Every shipped state path is `~/.dorang/…`, so under the image's `nonroot` user the database, the spool and the **generated key pepper** were written to `/home/nonroot` — outside the declared volume, lost on restart. Losing the pepper makes every issued api key unverifiable |
| 8 | the image's `HEALTHCHECK` | `dorangctl health` now exists. The image invoked it and the CLI answered `unknown command "health"` and exited 2, so every container reported unhealthy after `start-period + 3 × interval`, forever — and being distroless, nothing else in it could have probed either |
| 10 | model allow-list status | 403 `permission_error`, per COMPATIBILITY §11.2. The branch cited §7.2, which is about **tag routing**; the shared authorization gate has always answered 403 for the same refusal, so one rule had two answers and only the unreachable one was wrong. Since the merge the check runs at the GATE for every `ModelAuthGate` route rather than inside `handleInference`, which was reachable by six patterns and by nothing else — so the 403 now covers the routes the check could not previously reach, passthrough included |
| 12 | inline `notional_rate` | Accepted in `pricing.rules[]` with mandatory `source` and `as_of`, and carried across the bridge into the catalog's spelling. Adding the class name alone would not have been a fix: `internal/pricing` refuses a notional rule without provenance, so the translation had to carry it |
| 12 | `cached_read` / `cache_read` | Both spellings are accepted in both files and resolved to one component. The same component under both names in one rule is refused as pricing one thing twice |
| — | `routing.prefix.ttl` per backend | Not on the list; see below |

## What was made to refuse

Each refusal names the setting and what to use instead. That is the whole
difference between a refusal and a wall.

| Setting | Refusal |
|---|---|
| `capacity.*.rpm`, `.tpm` on every group, on `global` and on `models[]` | "a rate ceiling is not enforced on a capacity axis… put the ceiling on the deployment instead — `models[].deployments[].limits[]` with `metric: rpm` or `tpm`" |
| `capacity.principals.<id>.rpm`, `.tpm` | the same, naming the api key instead: `dorangctl key create --rpm N --tpm N`, which is what item 1 above now enforces |
| `key_ref` everywhere it appears | "key_ref is not resolved by this build… Use `key_env` or `key_file` — a vault agent that writes a file or exports a variable satisfies both" |
| `pricing.rules[].rates.images` | now a **load** error rather than an assembly error. It used to pass `dorangctl config lint` and then stop the server from starting |
| a `notional_rate` rule with no `source` or `as_of`, and provenance on any other class | both directions, per §8.5 |
| `client_priority: allow` with no `range`, a `range` with no grant, a `range` naming an undeclared class | a half-written grant is the shape this sweep exists to remove |

### Why `rpm`/`tpm` are refused on capacity axes rather than wired

The question the task asked — which package is the right home — has one answer.
A capacity axis counts **concurrent reservations**, released when a request
finishes; a rate counts **events over a window**, which are never released. The
broker has no clock and no window, and giving it one would duplicate
`internal/quota`, which owns exactly that and already has both configuration
surfaces: `models[].deployments[].limits[]` for the credential axis and the api
key's own `rpm_limit`/`tpm_limit` for the caller axis. Wiring `capacity.*.rpm`
would have produced a **third** spelling of a ceiling that two other spellings
already enforce, which is the defect class rather than a fix for it.

`tpm` has a second, decisive problem on that axis: a token count does not exist
at admission. Enforcing it there would mean charging an estimate and settling
later — the mechanism `internal/quota` already implements, one package over.

## Not on the list, found while sweeping

1. **`routing.prefix.ttl` was one number for a thing that is not global.** The
   affinity table's entry lifetime models how long the **backend** still holds
   the KV blocks for a prefix, and that differs by more than an order of
   magnitude: roughly five minutes for OpenAI's automatic caching, five minutes
   on Anthropic's default tier against an hour on its extended one, an
   operator-set TTL for Gemini's explicit caching — and for vLLM and SGLang it is
   not a duration at all, because blocks live until LRU eviction under memory
   pressure. One hour was wrong in both directions at once: too long for a hosted
   backend, where it pins a conversation to a node that no longer holds the
   prefix and costs load balance for nothing; too short for a self-hosted one,
   where it discards hits that were still there.

   Now settable per provider and per deployment, inheriting the global, with
   `until_evicted` for the class that has no clock. **The claim was checked before
   it was made**: `internal/prefix` evicts in two passes, expired first and then
   coldest-by-last-use against the byte budget, so an entry with no lifetime is
   still bounded — by capacity, which is exactly the contract a self-hosted
   engine's prefix cache has. `TestUntilEvictedIsStillBoundedByBytes` asserts it,
   and asserts that the budget was actually reached, so it cannot pass vacuously.

2. **`dorang_passthrough_requests_total` does have an increment site**, at
   `internal/server/passthrough.go:211`, since the file was written.
   `docs/OPERATIONS.md` and its Korean mirror said it had none. The counter and
   the claim about it drifted because nothing asserted either. Docs corrected;
   `TestPassthroughCounterIsIncrementedAndExported` now holds them together.

3. **`metering_degraded` already had a metric.** `docs/CONFIG.md` §23.1 and
   `docs/OPERATIONS.md` said "no metric, no health field, no admin field"; the
   metric landed with the §12.3 surface and the docs were not revisited. Only the
   health field was still missing. Same shape as (2): the prose was the only
   record, and prose does not fail a build.

4. **Eleven further settings that load and do nothing**, found by the recurrence
   guard on its first run and listed in `knownUnwired` in
   `internal/config/consumed_test.go`: `providers[].usage_probe` (§6.2, the
   fetchers exist and nothing constructs one — **closed 2026-07-29**, see the
   control table above), `providers[].metrics.interval` (§12.4 — **the whole
   `metrics` block is a load error now**: nothing scrapes a backend, so it is
   refused rather than left inert, naming the strategies that need no scrape),
   `providers[].params.drop` and `.drop_unsupported` (§10.3 — **both closed,
   in opposite directions**: `params.drop` is wired end to end, and
   `drop_unsupported: false` is a load error naming it, because dorang converts
   rather than relaying and there is nothing to forward an unmodelled parameter
   INTO. `params.drop` was the longer of the two: the mechanism — the validated
   list, the neutral-request removal, the `x-dorang-dropped-params` report — was
   complete and `internal/app/upstream.go` never passed `p.Params.Drop` into
   `backend.Spec`, so the feature was live and no configuration file could reach
   it. That is this document's own dominant class committed by the change that
   built the feature, and it is now held by
   `TestParamsDropReachesTheUpstreamFromYAML` in `internal/app`, which asserts
   the parameter's absence from the body a real socket received),
   `routing.prefix.checkpoints` (§7.4b), `models[].deployments[].stream_timeout`,
   `key_rotation.providers[].affinity_group` (not even validated),
   `cluster.redis_url_env` (§13 — required for `capacity_mode: shared-redis` and
   no Redis client is constructed anywhere), `observability.otlp_endpoint`,
   `.log_level` and `.log_format`. None was in scope here; each is now recorded
   in a place that fails a test rather than in prose.

5. **`router.Config.PinnedWait` is never set from configuration.** It gates the
   only interactive path that can block, so that path was dead. The principal's
   `max_queue_wait` now supplies the budget when `PinnedWait` is zero, which is
   what gave that setting an interactive consumer at all.

## The recurrence guard, and what it cannot catch

`TestEveryConfiguredFieldIsReadSomewhere` (`internal/config/consumed_test.go`)
walks `config.Config` by reflection, collects every field carrying a `yaml` tag,
and requires each one's Go name to appear as an identifier in non-test source
**outside** `internal/config`. Two escape hatches, both of which are claims
rather than silencers: `readExempt` for fields whose whole effect is inside this
package (a secret source is resolved here and leaves as a value) or that exist to
carry a refusal, and `knownUnwired` for the ledger above. The ledger is checked in
**both** directions — a new setting with no consumer fails, and an entry that has
since been wired also fails, so the list cannot go stale.

It is a floor, not a proof:

- **It cannot see a value that is read and then dropped.** `Decision.PriorityTier`
  was computed and discarded, and every name in that chain was "referenced".
  Reference is necessary, not sufficient. The per-setting tests beside it are what
  assert behaviour.
- **It is vacuous for short field names.** `Enabled`, `Path`, `Drop`, `Interval`
  and `Timeout` occur everywhere, so four settings this sweep confirmed to be
  unwired — `providers[].metrics.interval`, `providers[].params.drop`,
  `providers[].usage_probe.interval`, `models[].deployments[].stream_timeout` —
  are invisible to it and are tracked in `docs/CONFIG.md` §23.1 instead.
  `params.drop` is the one of the four that has since been wired, and the guard
  played no part in noticing either its absence or its arrival — it reported
  `Drop` as consumed throughout, which is precisely the vacuity this bullet
  names. What caught it was a per-setting behavioural test in `internal/app`
  driving `config.LoadBytes` through an assembled gateway, which is the only
  instrument that covers the half the guard cannot. It does
  hold for the distinctive names, which is where new settings land:
  `MaxQueueWait`, `ClientPriority`, `PrefixTTL`, `AffinityGroup`.
- **It says nothing about semantics.** Reading `MaxQueue` and comparing it against
  the wrong quantity passes.
- **It only covers `config.Config`.** A control that is not a configuration field
  — `Decision.PriorityTier`, `auth.Access.ObservedRPM` — is outside its reach
  entirely. That half of the defect class has no automated guard, and the honest
  answer is that the only thing which finds it is an assembled-stack test that
  asserts an observable the owning package cannot produce alone.

## On the tests

For this defect class a test that proves a check works proves nothing: the
existing suites already did that for the rate limits, for the urgency scorer and
for the priority clamp, and all three were unreachable. So every test added here
either asserts an observable outside the package that owns the check — an HTTP
status, a header, the health body, a `Snapshot` counter — or asserts the joint
itself, that a configured value reached the subsystem that acts on it
(`Router.Rotation()`, `Broker.WaitBudget()`, `PriorityConfig.GrantsHint()`).

Two are structural rather than behavioural, and deliberately so.
`TestDockerfileHealthcheckInvokesARealSubcommand` reads the image's own
`HEALTHCHECK` line and feeds its arguments to the real CLI dispatcher, because
"the CLI has a health command" was never the thing that was wrong — the two
halves disagreeing was. `TestStateDirIsDeclaredAndRead` requires the image's
`ENV DORANG_STATE_DIR` to name the directory its `VOLUME` declares.

---

# Dispositions, third pass — the merge, and what was verified

The nine fixes were written on a branch, together with the "# Dispositions"
section above. **Only the document reached `main`**, inside a commit whose
subject is *"gitignore: cmd/ was never in version control"*; the 5,221 lines of
fix code stayed on the branch, which is not an ancestor of `main`. So for the
life of that commit `main` shipped a security document asserting protections
that were not in the binary — nine findings marked **Closed**, naming files that
did not exist and tests that appeared only inside this document.

An audit checked all twelve disposition claims and found **ten false on `main`**.
This section is written after merging the branch, and it is the authoritative
statement: **every claim below was checked by reverting the fix in place, running
the named test, and observing it fail** — then restoring it and observing it
pass. Nothing is recorded as closed on the strength of the code reading
correctly. A fix that passes with and without it has not landed.

Read the two sections above as history. Where they disagree with this one, this
one is what the binary does; the specific corrections are listed at the end.

## The merge: which implementation was kept, and why

`main` had moved a long way — the L5 extraction into `internal/backend`
completed, `internal/app` no longer makes an HTTP call, the "configured but never
applied" sweep landed, key rotation grew a second table. Twelve files conflicted
and several of them held **two implementations of one thing**. This codebase has
been bitten three times by exactly that, so in every case one was kept and the
other deleted. None is behind a flag.

| Duplicate | Kept | Deleted | Why |
|---|---|---|---|
| **The rate window** — `internal/app/rates.go` existed on both sides: the branch's `rateMeter` (tumbling minute, map dropped whole on rollover, per subject) and the sweep's `keyRates` (sixteen shards, sixty stamped one-second buckets, 100 000-subject cap with cold eviction, per **key**) | `keyRates` | `rateMeter` | A rolling minute is the number an operator can reconcile against a provider's own 429s; a tumbling one refuses a burst that straddles the boundary and admits one that does not. But `keyRates` was keyed by api key alone, which is the live defect below — so it was **re-derived per subject**, keyed `kind:id`, and made to serve `auth.RateSource`. One data structure, three subjects |
| **`MostRestrictiveParallel` / `strictest` / `mostRestrictive`** — the same "smaller of two ceilings, zero means none" written three times | `auth.MostRestrictiveParallel` (the shared rule) and `capacity.strictest` (the broker's own, which also carries the queue ceiling the sweep added) | `capacity.mostRestrictive`, and the inline loop in `app.principal.maxParallel` | The branch's `mostRestrictive` was byte-for-byte `strictest` without queue support. `principal.maxParallel` now calls `auth.MostRestrictiveParallel` rather than spelling §11.2's rule a fourth time |
| **The concurrency ceiling's path to the broker** — the branch set `capacity.Request.PrincipalMax` in the routing-request literal via `principalMaxParallel(rq.Principal)`; the sweep set it in `applyPrincipalPolicy` alongside the priority class and the client hint | `applyPrincipalPolicy` | `principalMaxParallel`, and the literal's field | Two writers of one field, and the later one silently won. One place now reads the caller's policy into the routing request |
| **`internal/capacity/principalmax_test.go`** (branch) vs `queue_limits_test.go`'s `TestPrincipalMaxTightensTheAxis` / `TestPrincipalMaxNeverWidens` (sweep) | the sweep's | the branch's file | Same three properties, and the sweep's exercise the broker end to end rather than the helper |
| **The bounded upstream read** — `readUpstreamBody` + `DefaultMaxResponseBytes` + `errUpstreamTooLarge` in `internal/app/dispatch.go` (branch) and in `internal/backend/backend.go` (branch, re-derived) | `internal/backend`'s | `internal/app`'s, and the `internal/app` copy of `TestOversizedUpstreamResponseIsRefused` | `internal/app` no longer makes the HTTP call. Its copy had no production caller — it was already the version nothing runs, which is the defect this review is named after |
| **The §11.3 scrubber's application site** — `internal/app/dispatch.go` scrubbing with `internal/redact` (branch) and `internal/backend/errors.go` scrubbing with `collectSecrets` (branch, re-derived) | `internal/backend`'s | the `internal/app` site and its `redact` import | Same reason. `collectSecrets` is also stronger: it reads what was actually put on the outbound headers, so an OAuth token applied by code it does not own is caught too |
| **The batch row's allow-list check** — `ownerResolver.identity` asked `principalAllowsModel(p, model)` **and** `p.Authorize(auth.Access{Model: model})`, and `auth.Limits.authorize` already consults the allow-list when `Access.Model` is non-empty | `p.Authorize` (the shared gate) | `principalAllowsModel` | Removing either alone changed nothing observable, so "revert the fix and watch a test fail" passed against a defect the other copy silently covered. That is the exact failure mode this pass exists to remove — recorded in the first pass as "one control with two implementations", and now it is one control with one |
| **Replacing a key's secret** — `store.ReplaceKeyVerifier` (branch) and `store.RotateKey` + `EndGrace` (`secrets.go`, current `main`) | `RotateKey` + `EndGrace` | `ReplaceKeyVerifier` | Not a stylistic choice. `ReplaceKeyVerifier` wrote `api_keys.lookup`, `.token_hash` and `.hash_scheme` — the **denormalized** copy. Authentication resolves through `api_key_secrets` (`resolveKeyQuery`), so on current `main`'s schema the leaked secret would have gone on authenticating and the freshly minted one would have been unknown to the gateway, behind a `200 OK` telling the operator the credential had been replaced. `/key/regenerate` is now `RotateKey` with a zero grace followed by `EndGrace`: the difference between a planned rotation and an incident is a parameter, not a second write path |
| **The model-allow-list refusal status** — the branch's `Request.AuthorizeModel` answered **401** citing §7.2; the sweep had just corrected the same refusal from 401 to **403 `permission_error`** citing §11.2 | 403 | 401 | §7.2 is about **tag routing** misses, a different condition. The shared authorization gate has always answered 403 for this refusal, so one rule had two answers. Both passthrough refusals moved with it (`model_not_allowed` and `model_not_authorizable`), because a caller cannot be told to re-authenticate over a body it should re-encode |
| **The upstream context-overflow classifier** — `internal/app/estimate.go` and a second copy in `testing/scenario/harness.go`, both reading `server.Error.Message` | both, corrected to read `NativeMessage` | — | The two copies remain (the harness is another agent's file and is mid-flight), but both were silently broken by finding 3: `Message` is now `canonicalMessage(status)`, a function of the status line alone, so scanning it for a vendor's overflow phrase finds nothing. §10.5a's "route to a larger window" had stopped being reachable from an upstream signal. **This is the one duplicate this pass did not collapse**, and it is named here so the next pass does |

### Re-derived, because the code the fix guarded had moved

`main` completed the L5 extraction while the branch sat unmerged: `internal/app`
now delegates every upstream call to `internal/backend`. Four fixes therefore
land one package down, and one lands in a place neither side anticipated.

- **The §11.3 leak → `internal/backend/errors.go`.** `upstreamError` scrubs the
  provider credential out of `Error.NativeMessage`, which is where the upstream's
  text lives now — `Normalize` writes nothing from a decoded body into
  `Error.Message` on any branch.
- **The unbounded read → `internal/backend/backend.go`.** The L5 layer shipped
  the same asymmetry the interactive dispatcher had: 1 MiB on the error branch,
  `io.ReadAll` on the success branch. `Options.MaxResponseBytes`,
  `DefaultMaxResponseBytes` and a non-retryable `upstream_response_too_large`.
- **Transport error text → `internal/backend/errors.go`.** `transportError`
  relayed `err.Error()` under a comment arguing it was safe because it cannot
  carry a credential. True, and not the finding: it carries the operator's
  internal hostname, port and IP.
- **The batch executor → `backend.Do`.** The branch's version made its own HTTP
  call. The budget hold, the row/body model-mismatch check and the settlement
  now wrap `backend.Do` instead, so the batch path and the interactive path share
  one client, one scrubber and one bounded read.
- **The batch owner's envelope → `cluster.AuthPrincipal`.** The branch had its
  own `recordFromAPIKey`; `main` had moved that conversion to
  `cluster.AuthRecord`, which also resolves the key's **tier** and applies it to
  the limits. `AuthRecord` was split so both callers derive from one conversion —
  otherwise a batch row would have been authorized against an untiered envelope,
  which is a second view of one credential's limits and the shape R1-A named.

### The live defect the document claimed was closed

**A team's `rpm_limit` was multiplied by the number of keys under the team.**
`auth.Access` carried ONE observed pair for three subjects, and `internal/app`'s
window was keyed by api key alone, so `internal/auth/principal.go` compared the
**team's** ceiling against one **key's** counter. Demonstrated before the fix:
team `rpm_limit: 4`, ten keys × four requests → **zero refused**; ten requests on
one key → six refused. It is the same N-multiplication as finding 4, in the rate
dimension, and it was live on `main` while this document said finding 2 was
closed.

Closed by `auth.RateSource`: `Limits.authorize` takes the subject's **id** and
reads that subject's counter, `keyRates` is keyed `kind:id`, and the window is
entered once per request — read and increment together under the subject's shard
lock, so two concurrent requests cannot both observe the same pre-count. Tokens
land on all three subjects at settlement, from `App.recordMetrics`, which is the
one production settlement site and the only point every finished request passes.

## Verification: revert the fix, watch the named test fail

Every row was produced mechanically: apply the edit in the "Reverted" column, run
only the named test, record the outcome, restore. A row is present only if the
test **failed** with the fix reverted and passes with it.

| # | Control | Reverted | Named test |
|---|---|---|---|
| 1 | batch: dispatch-time allow-list and kill switches | `ownerResolver.identity` no longer calls `p.Authorize` | `TestBatchExecutorRefusesAModelTheOwnerMayNotUse`, `TestBatchExecutorRefusesABlockedOwner` |
| 1 | batch: a budget hold on every row | `reserveBatch` replaced by a nil hold | `TestBatchExecutionIsBudgeted` |
| 1 | batch: upload-time allow-list, per row | `validateRow` skips `vc.authorize` | `TestUploadRefusesARowNamingADisallowedModel` |
| 2 | the observed window reaches the check at all | `principal.Authorize` no longer calls `rates.observe` | `TestRPMLimitIsEnforced`, `TestTPMLimitIsEnforced`, `TestRateWindowRolls`, `TestRPMLimitRefusesThroughTheWholeStack`, `TestTPMLimitCountsFinishedTokens` |
| 2 | the window counts every SUBJECT, not only the key | `subjectsOf` returns the key alone | `TestTeamRPMCountsEveryKeyUnderTheTeam` |
| 2 | `auth` compares each subject against its own counter | `Limits.authorize` reads `Access.ObservedRPM/TPM` again | `TestRPMLimitIsEnforced`, `TestTeamRPMCountsEveryKeyUnderTheTeam` |
| 2 | the token half is settled from a real finished request | `App.recordMetrics` drops `recordTokens` | `TestTPMLimitCountsFinishedTokens` |
| 2 | settled tokens land on every subject | `recordTokens` given the key id only | `TestSettledTokensReachEverySubjectOfTheRequest` |
| 2 | `max_parallel_requests` reaches the routing request | `applyPrincipalPolicy` sets `PrincipalMax = 0` | `TestMaxParallelReachesThePrincipal` |
| 2 | the broker applies the carried ceiling | `Broker.needs` drops `strictest(…, req.PrincipalMax)` | `TestPrincipalMaxTightensTheAxis`, `TestPrincipalMaxNeverWidens` |
| 3 | the envelope carries dorang's words, not the upstream's | `Normalize` sets `e.Message = e.NativeMessage` | `TestNormalizeEveryUpstreamShape` (and `internal/server/errorleak_test.go`) |
| 3 | the RECORDED native text is scrubbed of the sent secret | `upstreamError` drops `scrub(e.NativeMessage, secrets)` | `TestAHostileUpstreamCannotEchoTheCredentialBack` |
| 3 | transport error text does not name internal hosts | `transportError` relays `err.Error()` | `TestUnreachableUpstreamDoesNotNameInternalHosts` |
| 3 | `x-dorang-native-error-type` is clamped and header-safe | `WriteError` emits the raw `NativeType` | `TestNativeErrorTypeHeaderIsCheckedAndClamped` |
| 4 | one budget hold per declaring subject | `budgetSubjectsOf` returns the key alone | `TestTeamBudgetIsNotMultipliedByTheNumberOfKeys`, `TestEachBudgetSubjectKeepsItsOwnPeriod` |
| 5 | the prefix chain is seeded with the tenant | `NewChain` seeds `H(group)` again | `TestChainIsTenantScoped`, `TestTenantAndGroupCannotBeConfused` |
| 5 | `router.Request.Tenant` is assigned in production | `decode` sets `Tenant: ""` | `TestRouterRequestCarriesTheTenant` |
| 6 | an unknown-key flood cannot drive a snapshot merge | `insert` merges on `positives+negatives` | `TestUnknownKeyFloodDoesNotMergeOnEveryMiss` |
| 7 | the non-streaming upstream body is BOUNDED | `readUpstreamBody` returns `io.ReadAll(r)` | `TestOversizedUpstreamResponseIsRefused` |
| 8 | passthrough consults the allow-list | `authorizePassthroughModel` marks the request authorized instead | `TestPassthroughEnforcesTheModelAllowList` |
| 8 | passthrough fails CLOSED when it cannot determine the model | `principalRestrictsModels` returns false | `TestPassthroughRefusesWhenTheModelCannotBeDetermined` |
| 8 | a route cannot reach the mux without a `ModelAuth` answer | `newRouteTable` stops refusing `ModelAuthUnset` | `TestRouteTableRefusesAnUndeclaredModelAuth`, `TestEveryRouteDeclaresAModelAuthMode` |
| 8 | a `ModelAuthHandler` route that never asked fails the request | `Server.serve` drops the post-condition | `TestModelAuthHandlerRouteThatSkipsTheCheckFails` |
| 9 | `Credential.MarshalYAML` leaves the inline literal nowhere to land | method renamed out of the interface | `TestMarshalingNeverEmitsAPlaintextSecret` |
| 9 | `RotationKey.MarshalYAML`, the same for the rotation pool | method renamed out of the interface | `TestMarshalingNeverEmitsAPlaintextSecret` |
| 9 | `import config` writes `key_env`, never an inline literal | `internal/config/import.go` emits `SecretRef{Inline: raw}` for a literal again | `TestImportWarnsAboutALiteralKey` |
| — | passthrough does not relay upstream `Set-Cookie` | the two `dst.Del` calls removed | `TestPassthroughDoesNotRelaySetCookie` |
| — | an unowned batch record is not everyone's | `ownedBy` admits an empty `recordOwner` again | `TestAnUnownedRecordIsNotVisibleToEveryKey` |
| — | the master credential owns what it creates | `principalID` returns `""` for the master again | `TestTheMasterCredentialOwnsWhatItCreates` |
| — | `/key/regenerate` actually retires the leaked secret | `ReplaceVerifier` made a no-op | `TestRegeneratingAKeyRetiresTheOldSecret` |
| — | an upstream overflow is classified from the NATIVE text | `upstreamCause` reads `e.Message` | `TestAnUpstreamOverflowIsRecognisedFromTheBody` |
| — | an ungranted priority hint is REPORTED as dropped (§10.5) | `droppedParams` stops naming the header | `TestGrantedClientPriorityIsHonouredAndADropIsReported` |

### Four tests that did not survive the check as written, and were fixed

- **`TestOversizedUpstreamResponseIsRefused` proved the refusal, not the bound.**
  With `readUpstreamBody` reverted to `io.ReadAll(r)` it still returned the
  too-large error, because the length check followed the read — so the test
  passed against an implementation with none of the safety. The finding is a
  remote OOM: the bytes must not be READ. The surviving copy counts what the
  reader was asked for.
- **`TestTPMLimitCountsFinishedTokens` fed the counter itself.** It called
  `rates.record(keyID, 500)` directly and then asserted a 429, which proves the
  comparison works and says nothing about whether anything in production feeds
  it — the exact topology that let `rpm_limit` and `tpm_limit` ship enforcing
  nothing, unit-tested, for the life of the project. It now drives a real chat
  completion against a real upstream that reports usage, and the second request
  is refused.
- **`TestGrantedClientPriorityIsHonouredAndADropIsReported` named itself end to
  end and called `CanonicalFor` directly.** §10.5's requirement is that a dropped
  hint be *reported* in `x-dorang-dropped-params`, and nothing asserted the
  header. It now sends `X-Request-Priority` from an ungranted key through the
  assembled stack and reads the response header.
- **Finding 9's fix is not where the first pass said it was.**
  `SecretRef.MarshalYAML` can be renamed out of existence and every marshalling
  test still passes: both uses embed `SecretRef` with `yaml:",inline"`, and
  gopkg.in/yaml.v3 flattens an inline-embedded struct's exported fields rather
  than calling its `MarshalYAML`. That method is **dead code**, correctly
  written and never called. The load-bearing halves are `Credential.MarshalYAML`,
  `RotationKey.MarshalYAML` and the importer's refusal to create an inline
  literal, and those are the three rows in the table above.

## `internal/admin` — mounted

The first pass left it unwired on the grounds that mounting it would convert
three latent findings into live ones. All three are closed and it is mounted, so
**there is an API path to revoke a leaked key** — which the review's plainest
sentence said there was not.

- **Read-before-auth.** `API.ServeHTTP` authenticates FIRST: before the route
  lookup, before `OPTIONS`, before the 405 and before the 501. Everything above
  it was an oracle. Test: `TestTheRouteTableIsNotReadableBeforeAuthentication`.
- **Team-scoped administration.** `admin.Scope`, derived from the key's
  `team_id` rather than from a role nobody can forget to set; the zero `Scope`
  admits nothing. Tests: `TestATeamAdminCannotReachAnotherTeam`,
  `TestATeamAdminCannotMintOrMoveKeysAcrossTeams`,
  `TestDeploymentWideEndpointsRefuseAScopedAdmin`, `TestTheZeroScopeAdmitsNothing`.
- **Request-supplied id filters.** A parameter naming another team is
  `403 out_of_scope`; an OBJECT outside the scope is `404`, identical to an id
  that does not exist, so no id becomes an existence oracle. Listings are
  filtered after the store returns, not only before. Tests:
  `TestListingFiltersCannotWidenTheScope`, `TestSpendLogsCannotReachAnotherTeam`,
  `TestBudgetSubjectsAreScoped`.

`cmd/dorang/revoke_test.go` runs the incident end to end over HTTP —
`TestALeakedKeyCanBeRevokedThroughTheAPI` blocks a working key with one call and
watches the next request from it be refused, reading the audit row back;
`TestAnOrdinaryKeyCannotAdminister` pins 403 for a tenant key and 401 for none.

`internal/store/admin.go` supplies what the mounted surface calls: list, update,
delete, `users.role`, and the first `INSERT INTO audit_logs` in the repository —
against a table that had had a schema, two indexes and a retention sweep that
DELETES from it since the first migration. Rotation and pend are wired to
`store.RotateKey`, `EndGrace`, `ListKeySecrets`, `PendKey` and `ReleaseKey`, so
`/key/rotate`, `/key/rotate/cut`, `/key/secrets`, `/key/pend` and `/key/release`
serve rather than answering 501. Seven dependencies are still absent and answer
`501 dependency_not_configured` naming the missing piece; the list is in
`docs/OPERATIONS.md` §3.2.

## What the first two passes got wrong

The two sections above have been **corrected in place** so that no false claim
stands anywhere in this document. What they said before is recorded here, because
each error is an instance of the defect class the review is about and deleting it
silently would repeat the mistake that made a second audit necessary.

1. **"Closed" was asserted for code that was not on `main` at all.** Nine
   findings, twelve disposition claims, ten of them false — not fabricated, but
   copied ahead of the code they described. A disposition that names a file or a
   test is only closed once that file or test exists **on the branch it is
   claimed for**, and the check that would have caught this is `git merge-base
   --is-ancestor`, not a reading of the diff.
2. **Finding 2 was closed twice and was wrong both times.** The first pass
   described a `rates.go` that did not exist. The sweep then wrote a real one and
   recorded finding 2 closed — with the window keyed by api key alone, so a team
   ceiling was still multiplied by the number of keys under the team. Two
   independent "closed"s and the defect survived both.
3. **Finding 3's fix was described in the wrong package.** `internal/redact`
   applied in `internal/app`; the L5 extraction moved the upstream call and the
   scrubber had to move with it. The same relocation broke
   `internal/app/estimate.go`'s overflow classifier, which nothing noticed
   because it read a field that still existed and had merely stopped carrying the
   text.
4. **Finding 7's fix was described on `dispatchState`**, which no longer makes an
   HTTP call, and its test proved a refusal rather than a bound.
5. **Finding 9's fix was described as `SecretRef.MarshalYAML`**, which is dead
   code — `yaml:",inline"` flattens the embedded struct's exported fields and
   never calls its marshaller. The method the finding names still never runs.
6. **`internal/admin` was recorded as deliberately unwired** with "there is no
   API path to revoke a key" as the operational consequence. That was true when
   written and is not now.
7. **`batch.ownedBy` was recorded as "not fixed in this pass"** and had in fact
   been fixed on the branch the pass was written from.

## Still open

Named, because a control that exists and is not called is this codebase's
dominant defect and it has now been counted three times.

**As of `8e6016d`, 2026-07-29.** This list is dated because an undated backlog
is the thing this document was written about: every entry below was re-checked
against that commit's blobs, and three entries that were *not* re-checked before
had been false for days. When you read it, check the date against `git log`
first — a backlog is a measurement of a tree, and a measurement without a stamp
cannot be audited or trusted, only believed.

1. **Store lookups for unknown keys are still unbounded** (finding 6's second
   half). One `LoadByLookup` per unauthenticated request; the negative TTL bounds
   repeats of the same key and nothing bounds distinct ones. The first pass
   called this the largest thing left open and it still is.
2. **`internal/store` has no model registry.** `deployments` and `model_aliases`
   are tables with no Go code, so `/model/*` and `/model_group/info` answer
   `501 dependency_not_configured`. Note the reason has to be stated more
   carefully than "no store layer": those two tables also have no *reader*, since
   routing is compiled from the configuration file, so writing them would change
   no routing decision.

   *(This entry also named `users`, `teams`, `team_members` and the budget
   ceilings, and closing that half found something worse than a missing store
   layer. The tables were only the smaller obstacle: `cluster.AuthPrincipal`
   built an `auth.Principal` with the KEY's limits and left `User` and `Team`
   nil, so `users.blocked`, `teams.blocked`, the ceilings and the team rate
   limits reached no decision on any node at any latency — a **team-level**
   `rpm_limit` could not be put on a stored row, and would not have been enforced
   if it had been. It was invisible for the same reason it was harmless-looking:
   every guard that reads an owner envelope is nil-guarded, correctly, because an
   unowned key has none. The credential read now LEFT-joins both owners in the
   same statement, `AuthPrincipal` takes them as a required argument, and the
   directory and budget routes are wired — see DESIGN W12. The per-subject rate
   ceiling is still proved at the gate — `TestTeamRPMCountsEveryKeyUnderTheTeam`,
   `TestSettledTokensReachEverySubjectOfTheRequest` — and is now also reachable
   from a stored row.)*
3. **`/audit/list`**: rows are written by every administrative mutation and still
   cannot be read over the API. `internal/admin/routes.go` names it as the one
   stub whose reason says "until this ships" rather than describing a refusal.
   *(This entry said "**No range-aggregating ledger query**, so
   `/global/spend/report` and the three daily-activity endpoints have nothing to
   call." Both halves were false — see the fourth-pass corrections below.)*
4. **`quota.Registry`, `internal/probe`, `pricing.Catalog` and `App.Reload`** are
   not reachable from the administrative surface, so `/admin/credentials/health`,
   `/admin/quota`, `/spend/calculate`, `/admin/pricing/preview` and
   `/admin/config/reload` have no adapter. `SIGHUP` works.
5. **Two copies of the upstream-overflow classifier** remain, in
   `internal/app/estimate.go` and `testing/scenario/harness.go` — but the
   duplication is now near-nothing and this entry is **downgraded, not carried**:
   both are three-line adapters delegating to the single `router.ClassifyBody`,
   so the rule is single-sourced and only the argument shuffling differs. What
   this entry described — two independent implementations of one classification —
   no longer exists.
6. **Rate ceilings are per process.** An N-node deployment enforces N times every
   `rpm_limit` and `tpm_limit`, and `tpm_limit` bounds the NEXT request because
   the token count does not exist until settlement.
7. **`auth.Strip` and `store.ImportKeys`** — unchanged from the first pass.
   `auth.Strip` (`internal/auth/header.go`) has only test callers; the production
   strip is `server.StripAuthHeaders`. `store.ImportKeys` has no CLI or HTTP
   entry point. *(This entry also named **the OAuth subsystem**, **`internal/luaext`**
   and **`quota.Ranker`**. All three are wired — see the fourth-pass corrections
   below. Three of the five names on one line were false.)*
8. Everything under "## Not addressed" above that is not corrected in the list
   before this one, or in the fourth-pass corrections below.

## What the third pass got wrong — 2026-07-29, at `8e6016d`

The "Still open" list above has been **corrected in place**, and what each entry
said before is quoted where it stood. This section exists for the same reason
"## What the first two passes got wrong" does: each error is an instance of the
defect class this review is about, and deleting one silently is the mistake that
made the second audit necessary.

The shape repeated exactly. The first two passes marked things **closed** that
were not. The third pass marked things **open** that were. Both are the same
error — a disposition copied forward without re-reading the code — and the second
is not the harmless direction. A backlog carrying dead entries stops being read,
and the live items in it go with them. Five of six documents in this repository
were asserting something the code had closed when this pass began.

| # | The third pass said | The code says | Evidence |
|---|---|---|---|
| 1 | "Still open" #3: **no range-aggregating ledger query**, so `/global/spend/report` has nothing to call | It exists and is wired | `store.RollupQuery` and `Store.ReadRollupRange` (`internal/store/rollup.go`); the adapter is `(*adminLedger).Report` (`internal/app/admin.go`), whose own comment says *"It answered 501 until now"*. Pinned by `TestGlobalSpendReportReadsTheRollups` |
| 2 | Same entry, second half: the three daily-activity endpoints have **nothing to call** | **Outcome right, reason wrong** | They still refuse, but from a *live* ledger that names what is missing — `internal/app/admin.go`'s `unsupportedLedger` says the ledger has no per-user index, and `rollupPlan` refuses two non-`day` dimensions at once because §9.4 materializes purpose-built rollups and not a cube. That is a deliberate refusal with the alternative named, not an absent dependency. "Nothing to call" is false; "no cube to answer it from" is true, and that is what OPERATIONS §3.2 now says |
| 3 | "Still open" #7: **`internal/luaext`** is unreachable, "unchanged from the first pass" | Reachable, on the request path | `go.mod` requires `github.com/yuin/gopher-lua v1.1.2` since `a0d5871`; `internal/app/extensions.go` builds the engine from configuration and `internal/app/filter.go` compiles every declared `filters.plugins[]` entry. `94de856`, `bb67140` and `241f73b` hardened the sandbox — three commits of work on a subsystem this document called a skeleton |
| 4 | Same entry: **`quota.Ranker`** is unreachable | Constructed on the production path | `internal/app/build.go` calls `quota.NewRanker`; the join is pinned by `TestUrgencySourceIsTheQuotaRanker` (`internal/router/wiring_test.go`). Its sibling `Meter.Allowance`, named beside it in "### 3. Whole subsystems with no callers", is read by `internal/metrics/collect_quota.go` |
| 5 | Same entry: **the OAuth subsystem** is unreachable, and "OAuth credentials cannot be configured, so §11.2b describes a feature the binary does not have" | Configured, built, started and closed | `internal/app/oauthcred.go`'s `buildOAuth` turns every `auth: oauth` credential into a refreshing one; `internal/app/app.go` calls it, starts the refresh loops and closes them. Its own doc comment records that this was §17.1's defect class and that the two halves had to land together |
| 6 | "Still open" #5: **two copies of the upstream-overflow classifier**, nothing keeping them in step | Narrowed to argument shuffling | Both delegate to the single `router.ClassifyBody`. Downgraded above rather than deleted, because the mirror is still a mirror |
| 7 | "## Not addressed", LOW: **route table enumerable before authentication** | Closed, in this same document | The "## `internal/admin` — mounted" section three headings above states it and names `TestTheRouteTableIsNotReadableBeforeAuthentication`. A document that closes a finding in one section and forwards it as open in another is the single-file version of the cross-file contradiction this pass found twice |
| 8 | "### 3. Whole subsystems with no callers": **`internal/backend`** — no importer outside itself | The backend layer is the request path | Historical, and true when written; kept for the record. §17.1's "Extracting the backend layer" is the change that closed it |

> ⚠️ **The paragraph that stood here claimed three of that section's entries were "still true,
> re-verified rather than carried". All three are now false, and the third was false in a way
> re-verification could not miss.** Quoted as it stood: *"`internal/probe` has zero importers
> (`grep` returns two comments and no import), `store.ImportKeys` has no non-test caller, and
> `quota.Budget` is deliberately superseded by the durable `cluster.Ledger` (DESIGN §18 W9)."*
>
> | Claim | The code, at `5447598` |
> |---|---|
> | `internal/probe` has zero importers | `grep -rl dorang/internal/probe --include=*.go` returns `internal/app/usageprobe.go`. A prober is built per enabled provider and polled off the request path; CONFIG §23.1a records `providers[].usage_probe` as wired |
> | `store.ImportKeys` has no non-test caller | `cmd/dorangctl/importkeys.go:113` calls it. That file's own opening comment describes the gap this closed, so the caller and the correction shipped together |
> | `quota.Budget` is superseded by `cluster.Ledger` | `quota.Budget` **does not exist**; `internal/app/budget.go:64-68` says it "has since been deleted". "Superseded" describes a type that is still there |
>
> **This is the sharpest instance of the class on this page, because of where it sits.** It is
> the paragraph immediately below a correction table about carrying dispositions forward without
> re-reading the code, and it says *re-verified rather than carried* in its own text. A claim
> that asserts its own freshness is not evidence of freshness — it is the same disposition
> copied forward with a stronger adjective. What the section below asks for ("`git grep` for an
> import outside the package, run at the commit the claim is made for") is exactly what would
> have caught all three, and exactly what was not done.
>
> Of that section's original list, what remains genuinely unreached is nothing this paragraph
> named. The list is not restated here, because restating it would recreate the defect: an
> absence claim is only worth writing next to the grep that establishes it.

**One method to check the next time.** Every false entry above shares a tell: the
disposition names a *package* rather than a symbol and a call site. "`internal/luaext`
— skeleton, no importers" cannot be falsified by reading `internal/luaext`; it is
a claim about the rest of the tree, and the only thing that settles it is
`git grep` for an import outside the package, run at the commit the claim is made
for. That is the same rule the first correction on this page states for closures —
*a disposition that names a file or a test is only closed once that file or test
exists on the branch it is claimed for* — applied in the other direction. An
**open** disposition needs the same evidence a **closed** one does.

