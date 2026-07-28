// Package shadow is the replacement decision criterion.
//
// DESIGN §0.3 makes migration "run alongside, then take over", and §14.1 says
// what makes taking over defensible: an empty structural diff report over real
// traffic. This package produces that report. It sends a sampled fraction of
// live requests to a reference gateway a second time, compares the *shape* of
// the two responses, and writes every difference — and every comparison it
// could not complete — as a structured record.
//
// # What is compared, and what is not
//
// Model output is not deterministic, so content is never compared. What is
// compared is everything a client branches on:
//
//   - the HTTP status;
//   - the set of JSON field paths present, and the type at each path;
//   - the header key set;
//   - for a stream, the sequence of frame kinds and the terminator;
//   - finish_reason / stop_reason values, which are a bounded vocabulary;
//   - the presence and shape of usage;
//   - the error envelope shape, and its type and code, when either side errors.
//
// Explicitly ignored: ids, timestamps, system_fingerprint, model output text,
// token counts, and whatever compare.ignore_fields adds. See [defaultIgnore].
//
// # The report has to be trustworthy
//
// An empty report is the completion criterion, which means the report is the
// thing a production cutover is decided on. A comparison that silently skipped
// a case would make an empty report a lie — so a comparison this package cannot
// complete writes an *inconclusive* record naming the dimension and the reason,
// and inconclusive records are counted separately from clean ones. "Zero diffs"
// and "zero diffs and zero inconclusive" are different answers, and only the
// second one is the gate.
//
// # What is not compared at all
//
// §14.1 says to send the same request to a reference gateway. Taken literally
// that includes `DELETE /key/…`, which on a reference gateway deletes a key. So
// replay is deny-by-default: GET, HEAD and OPTIONS always, POST only for the
// inference families, nothing else. See [replayable].
//
// The consequence is that the compared surface is smaller than the served
// surface — and COMPATIBILITY §0 measures the served surface as 80% control
// plane. A clean report therefore proves something about inference traffic and
// the read-only routes, and nothing about the rest. [Stats.SkippedUnsafe]
// counts what was passed over, and it is reported beside the verdict for
// exactly that reason.
//
// # Nothing is on the request path
//
// [Shadower.Sample] is one hash of the request id. [Shadower.Observe] copies a
// bounded capture and pushes it onto a bounded queue; a full queue is a counted
// drop, never a wait (DESIGN §9.6 rule 3). The reference call itself happens on
// a worker goroutine and can hang, fail, time out or refuse without the client
// ever learning that it happened.
//
// # Cost, which is the sharp edge
//
// Both modes send every sampled request twice, so both cost twice. Three things
// follow, and each of them is a place a plausible design goes wrong:
//
//  1. The ceiling is a hard stop, not a warning. When a reservation does not
//     fit, shadowing stops for the rest of the UTC day and says so in metrics
//     and in the health body.
//
//  2. It is a stop, not a skip. The tempting alternative — refuse the requests
//     that do not fit and keep taking the ones that do — biases coverage toward
//     cheap traffic while the counter still reads under the limit. The report
//     would then look complete while the expensive paths, which are the ones
//     worth comparing, were never compared at all.
//
//  3. Cost is reserved before the call and settled after. Charging on
//     completion lets any number of concurrent calls each see the pre-spend
//     balance and pass — the same failure §9.6 refuses to accept for budget,
//     for the same reason.
//
// Sampling is deterministic in the request id, so a retry of one logical call
// is sampled the same way both times rather than charged twice, and it looks at
// nothing but the id — not the model, the body size or the price — so it cannot
// prefer cheap requests and make both the ceiling and the coverage look better
// than they are.
//
// # How the reference call is accounted
//
// It is not in dorang's ledger, and that is deliberate.
//
// A shadowed request produces exactly one row (DESIGN §12): dorang meters the
// request it served. The reference call is issued against the reference
// gateway's own credential, never re-enters dorang's request path, and touches
// no capacity axis, quota window or budget. If it were metered, every cost and
// quota figure would be wrong by the sample rate — and wrong in the direction
// that makes a gateway look more expensive than it is, right at the moment
// somebody is deciding whether to adopt it.
//
// The money is real, and it is spent on the reference gateway's account. The
// only place it appears is this package's own daily ceiling and its counters.
// [Stats.SpentNanoUSD] is that number.
package shadow
