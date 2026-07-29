# Changelog

Notable changes. Dates are the day the work landed; the git history is the
record of record.

The format is not "features added". This project's history is mostly *defects
found in its own claims*, and the entries that matter are the ones where a
published number, a documented control or a passing test turned out not to mean
what it said. Those are kept rather than tidied away — see `docs/DESIGN.md`
§17.1, which is the ledger of the patterns behind them.

## Unreleased

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
