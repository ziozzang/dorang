// Package loadsignal reads a self-hosted engine's own report of how busy it was while it
// served a request, so that price can be a function of occupancy (DESIGN §8.6).
//
// It is one file's worth of parsing and a refusal taxonomy, and the taxonomy is the part
// that matters. Every function here answers one question — "did we observe this backend's
// occupancy, and if not, why not" — and the second half of that question is what the
// package exists for.
//
// # The sampling point, and why there is only one
//
// vLLM returns an ORCA load report on the response to the request it describes, when the
// request carries `endpoint-load-metrics-format: JSON` (VLLM.md §3.2). No server flag is
// needed. That report is an observation OF THIS REQUEST rather than a gauge of unknown
// age, and it is the only sampling point this package accepts.
//
// The alternative — scraping `/metrics` — is deliberately **not built**, and the reason is
// a pricing reason rather than an engineering one. A gauge has an AGE, not an interval. To
// price a request with one you must pick an instant: admission, which is the occupancy the
// request arrived into and says nothing about the minutes it then ran for; or settlement,
// which is the occupancy after it left. Neither instant is a property of the request, so
// neither is defensible on an invoice — and a poll is node-local, so two dorang nodes at
// different phases of the same interval charge different amounts for the same request into
// one shared ledger. R17's scraper is a good ROUTING signal and remains worth building for
// that: a misrouted request is cheap to be wrong about and self-corrects. A wrong invoice
// is neither. See [RefusalStreamed] for what this costs.
//
// # The zero that is not idle, twice
//
// VLLM.md §3.1 warns that `--disable-log-stats` leaves `/metrics` answering 200 with zero
// `vllm:` series, and that dorang must tell "reachable, no series" from "idle" because they
// look identical and mean opposite things. For routing that misdirects traffic. For
// pricing it means free.
//
// The load header does not dissolve that trap. It DISGUISES it. vLLM builds the report as
//
//	kv_cache_utilization=(last_req_metrics.gpu_kv_cache_utilisation
//	                      if last_req_metrics is not None else 0.0)
//
// so an engine that had no metrics to report emits a well-formed header claiming an empty
// machine. A scrape at least fails visibly; this succeeds and is wrong. So [Parse] refuses
// an exact zero ([RefusalZeroIsAmbiguous]) rather than believing it.
//
// That refusal is free, which is why it is not a trade-off. At an occupancy of zero the
// factor is 1 + slope x 0 = 1.0 and the charge is the base rate — the same charge the
// fallback produces. The one reading dorang cannot trust is the one reading that does not
// move the price, so refusing it costs nobody anything and replaces a claim ("this ran on
// an empty machine") with the truth ("we do not know what this ran on").
//
// # What this package does not do
//
//   - No smoothing, no EWMA, no jitter. internal/quota has damping and jitter for its
//     ranker and they are right there; they are wrong here. Both are node-local state, and
//     a price computed from node-local state is a price that differs between two nodes
//     serving the same caller — which is the objection to the scraper, reintroduced. A
//     ranking is a choice and may be perturbed freely because nobody is billed for it; a
//     price is published, disputed, and has to be re-derivable from the row.
//   - No queue depth in the factor. The report carries more than occupancy, and waiting
//     requests are a LATENCY signal rather than an occupancy one. One signal, one
//     arithmetic, one published ceiling.
package loadsignal
