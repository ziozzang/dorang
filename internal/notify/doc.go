// Package notify delivers the operational notifications of DESIGN §11.5 —
// key_created, budget_80pct, budget_exceeded, quota_exhausted,
// credential_unhealthy, batch_completed and invite — over one of four drivers:
// smtp, http, the on_email extension hook, or none.
//
// Three properties shape everything here, and each of them is the answer to a
// specific way a notification system goes wrong.
//
// # Never on the request path
//
// A budget crossing 80% is discovered while serving a request. Connecting to a
// mail server there would put an SMTP handshake inside the gateway-overhead
// budget of §15.1, and a mail server that stops answering would become a
// gateway that stops answering. So the request path does two things and no
// more: it asks whether this alert is already accounted for ([Notifier.Admit],
// which is a sharded map lookup and allocates nothing), and if it is not, it
// hands over a notification ([Notifier.Send], a non-blocking enqueue).
// Everything after that — rendering, redaction, the connection, the retry —
// happens on a worker.
//
// The queue is bounded and a full queue drops, visibly, per §9.6 rule 1 and
// rule 3: back-pressure surfaces as a counted drop rather than as a request
// that waits. [Notifier.Stats] and [Notifier.Degraded] report it.
//
// # One alert per subject per period
//
// budget_80pct is true on *every* request past the threshold. Without
// deduplication the alert is emitted once per request, which is the difference
// between something an operator reads and something an operator filters. The
// deduper keys on (event, subject, period bucket): the first request past the
// line sends, and every other request in that period is suppressed and counted.
//
// The period is per event, because the events do not have the same shape.
// budget_80pct repeats until the budget window rolls over; key_created happens
// once by construction and is not deduplicated at all.
//
// # No secret in an email
//
// A notification is assembled from named fields, and three independent things
// have to agree before one reaches a message:
//
//  1. The field name is on the event's allow-list ([EventFields]). Anything
//     else is dropped and counted — a caller cannot add a field by accident.
//  2. The field name is not on the refusal list ([DeniedField]) regardless of
//     what any allow-list says.
//  3. The value does not look like key material. A value that starts like a
//     provider token, or that is long and opaque, is replaced with
//     [Redacted] and counted.
//
// So a key_created notification carries key_id and key_name and cannot carry
// the key, whether or not the caller tried. TestKeyCreatedCannotCarryTheKey
// asserts it from the caller's side and TestRenderRedactsSecretShapedValues
// from the renderer's.
//
// # A failing mail server must not retry hard
//
// Delivery is attempted a bounded number of times with exponential backoff, and
// then the notification is dropped and counted. Consecutive failures open a
// breaker, after which delivery is not even attempted until a cooldown elapses
// — so a mail server that is down for an hour costs one connection attempt per
// cooldown rather than one per notification, and the queue drains instead of
// filling.
package notify
