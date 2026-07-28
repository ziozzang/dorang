// Package probe reads what a provider says is left of a credential's quota.
//
// internal/quota defines [quota.Prober] and combines a provider's figures with
// local metering (DESIGN §6.2); this package is the half that actually asks a
// provider. It is also what makes expiring-quota routing work at all: a rolling
// window has no reset instant of its own, so §7.5a(c) scores **zero** until a
// provider reports one, and that report arrives here.
//
// # What is shipped, and what deliberately is not
//
// The rule that decides is: **a prober authenticates with the credential that
// serves the traffic, and reports what is left of that credential's own
// allowance.** Three providers pass it.
//
//   - z.ai — a consumed percentage and a reset instant per window, for the API
//     key that serves inference. The only source here that supplies everything
//     §7.5a(c) needs.
//   - Anthropic — the five-hour and seven-day utilization of a Claude Pro/Max
//     subscription, for the OAuth token that serves it (§11.2b). An Anthropic
//     API key is refused: there is no endpoint for one.
//   - DeepSeek — a prepaid balance, vendor-documented, for the inference key.
//     A balance is not a window, so it produces no window.
//
// Everything else publishes nothing a serving credential can read, and
// [Supported] carries what was checked for each. [New] returns [ErrNoEndpoint]
// with that reason rather than a prober that reports zeroes: §6.2 makes a
// reported figure authoritative over local metering, so a prober that guesses
// is worse than no prober — it routes traffic into an account it has no
// evidence about, and a silent zero reads as "nothing used".
//
// Three families are excluded by that rule and are easy to mistake for quota
// endpoints. Historical spend read with an organization Admin key (Anthropic's
// and OpenAI's usage and cost reports, xAI's management API) answers what the
// org spent, not what this key has left. Configured ceilings with no
// consumption beside them (Anthropic's rate_limits, GCP's QuotaInfo) cannot say
// how much is left either. And per-response rate-limit headers, which do carry
// remaining balances, arrive only as a side effect of a request — they belong
// to the request path, not to a poller.
//
// # Six rules, none of them about HTTP
//
//  1. A failed fetch never disables a credential. A failed read is not an
//     exhausted quota (§6.2). The prober keeps the last good snapshot with its
//     original fetch instant, so [quota.Tracker] retains it and its staleness
//     grows honestly; a credential that goes dark keeps serving.
//  2. Nothing runs on the request path. [quota.Registry] polls in the
//     background, concurrently, under a per-provider timeout, and this package
//     additionally caps its own in-flight reads per provider so one slow
//     provider delays nobody.
//  3. Figures combine, they do not substitute. This package emits only what the
//     provider said; §6.2's max() lives in [quota.Tracker].
//  4. Credentials never leak. Errors carry a status and a classification and
//     never the response body — a provider's error body can echo the key back,
//     and wrapping it would put the key straight into the message (the same
//     reasoning as §11.2b). Every error and every recorded reason additionally
//     passes a scrubber holding every secret the credential has presented.
//  5. Normalization is honest or absent, never confident and wrong. A
//     percentage out of range becomes an unknown percentage; a window whose
//     length the provider did not describe, or whose meaning is contested,
//     keeps its figures but is filed under no quota key and is named by
//     [Snapshot.Unmapped]; a reset instant in a format that does not name its
//     zone becomes no reset at all. An absent window reads as "no provider
//     opinion" to [quota.Tracker], which falls back to local metering — the
//     safe direction, because an over-optimistic quota reading routes into an
//     exhausted account. A parseable response carrying no figures at all is a
//     failed read rather than an empty success, since adopting it would refresh
//     the staleness of numbers that were never re-read.
//  6. The prober rate-limits itself. Polling an account-status endpoint hard
//     enough to get the account limited is precisely the failure it exists to
//     prevent, so a failed read backs off exponentially with jitter, a
//     Retry-After is obeyed, and reads of one credential have a floor spacing.
//
// # Percent-only providers, and the seam they exposed
//
// The common real shape is a percentage with no absolute ceiling: z.ai reports
// "37% of your 5-hour allowance", never "3,700 of 10,000". §6.2's correction
//
//	effective_used = max(reported, reported_at_last_poll + local_delta_since)
//
// is unit-homogeneous arithmetic, and a percentage is not in the metric's
// units. Adding a token delta to a percentage produces a number that is
// neither, and — because the sum is monotone while the local window is not — it
// grows past any configured limit and parks the credential in a cooldown
// nothing can clear. [quota.Tracker] therefore treats a window with no absolute
// figure as carrying its reset instant only. It reports no usage, which is the
// correct reading of "the provider told us a proportion of a number we do not
// know".
//
// [Allowance] is how an operator closes that gap: declaring the absolute size
// of a provider window turns the percentage into a figure in the rule's units,
// and the full §6.2 combination applies again. Without one the window still
// supplies §7.5a(c)'s reset instant, which is most of its value.
//
// An [Allowance] also re-files a window under the rule key an operator actually
// configured. The tracker is keyed by (window, metric); a provider window that
// lands under a key no rule uses is silently inert, and that is the quiet
// failure mode of this whole feature.
//
// # OAuth
//
// A credential's secret is resolved at probe time through [Auth], not captured
// at registration, because an OAuth credential's access token is replaced by
// its own refresher (§11.2b). A prober holding a stale token would answer every
// poll with a 401 — the rate-limit ban rule 6 exists to avoid.
package probe
