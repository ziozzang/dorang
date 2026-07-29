# testing/

Harnesses that exercise the assembled gateway. Everything here is a test tool —
nothing in this directory is deployed.

`deploy/` is the other half of that split, and it is deliberate: two of these
harnesses talk to real providers and spend real money, and they used to live
under `deploy/` where an operator reading a release would reasonably run them.

| | what it is | cost to run |
|---|---|---|
| `fake/` | a controllable upstream: fixed delays, injected failures, recorded requests | free |
| `scenario/` | the design's numbered scenarios, end to end through a real socket | free |
| `workload/` | generated request mixes | free |
| `perf/` | the §15.1 overhead instrument — handler time minus every nanosecond blocked on the upstream | free; `-short` skips it |
| `parity/` | dorang and the incumbent LiteLLM answering the same requests, compared field by field | **real keys, real money** |
| `providers/` | every provider in the operator's `~/env` brought up, plus the dual-account capacity proof | **real keys, real money**, except the dual-key proof, which uses `fake_upstream.py` and spends nothing |

## The two that spend money

`parity/` and `providers/` are not run by `go test ./...` and never will be.
They exist because several defects were only ever going to be found against real
vendors — a base URL that lost its version segment and broke every Anthropic
coding plan; an upstream answering `200` with a body that is not a response;
embeddings priced at zero because one vendor reports its count only in
`total_tokens`.

Each carries a `REPORT.md`, and those are **dated measurements, not living
documents**. A measurement edited in place stops being evidence. When a claim in
one is superseded, the new run is appended and the old one stays.

Read the script before running either. They use the smallest viable prompts and
one call per case on purpose; the plans they authenticate against are the
operator's own.
