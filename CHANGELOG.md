# Changelog

Notable changes. Dates are the day the work landed; the git history is the
record of record.

The format is not "features added". This project's history is mostly *defects
found in its own claims*, and the entries that matter are the ones where a
published number, a documented control or a passing test turned out not to mean
what it said. Those are kept rather than tidied away — see `docs/DESIGN.md`
§17.1, which is the ledger of the patterns behind them.

## Unreleased

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
  it: warm-local p50 **476 → 372 µs**, allocations per 4 KiB request **391 →
  262**, throughput ceiling **20,489 → 23,532 req/s**.
- §15.1's published 200 µs p50 was found not to hold — it was true only below
  about 512 B — and every figure in that section is now a measurement with the
  concurrency it was taken at. The harness gates came *down* with the
  improvement; a ceiling left where it was has stopped being a ceiling.

### Correctness

- A Gemini deployment was routed *and* encoded against the OpenAI wire shape's
  capability set, while its encoder has no field for `cache_control`, `logprobs`,
  `service_tier`, a thinking block or a structured system prompt. Both §10.1
  gates admitted the request, the encoder dropped the construct, and the client
  got a `200` with no header. Unifying the two capability computations had proved
  only that they agreed — both switched on the same resolved `api` and returned
  one of the same two constants, so no configuration could separate them, and
  both were wrong together for the one wire shape with no constant of its own.
  A capability set is now declared beside the encoder that honours it.
- The backend adapters passed no `Loss` to either encoder, so every located
  downgrade was discarded where it was produced, and two documented promises had
  no writer at all: §10.2's "reasoning is disabled … and `x-dorang-dropped-params`
  says so", and the thinking-block *signature* dropped crossing into the OpenAI
  family — which no capability mask can see, because the bit is held while the
  encoder drops the signature.
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
