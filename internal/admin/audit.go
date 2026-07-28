package admin

import (
	"encoding/json"
	"net"
	"net/http"
)

// maxUserAgent bounds what a caller can write into the audit table through a
// header it controls entirely.
const maxUserAgent = 256

// requireAudit refuses a mutation that cannot be audited, before it happens.
//
// §2.3 says everything is audited with actor, action, before and after. The
// only way to keep that true is to decline the mutation when the trail is
// unavailable — applying the change and skipping the row would leave the
// deployment in a state nobody can reconstruct, which is precisely what an
// audit table exists to prevent.
func (c *call) requireAudit() error {
	if c.a.cfg.Audit == nil {
		return dependencyOff("audit log", "any administrative mutation")
	}
	return nil
}

// recordAudit writes the trail for one applied mutation.
//
// It runs *after* the change, because the row records what happened rather than
// what was attempted. That ordering has one visible consequence, and it is
// deliberate: if the audit write fails, the change has already taken effect, so
// the caller is told exactly that with [CodeAuditWriteFailed] rather than being
// told the request failed. A caller that retries on a plain 500 would otherwise
// apply the change twice while believing it had applied it zero times.
//
// before is nil for a creation, after is nil for a deletion. Both nil is a
// programming error and is refused here rather than silently written, because a
// row that records neither state records nothing.
func (c *call) recordAudit(action, objectKind, objectID string, before, after any) error {
	if before == nil && after == nil {
		return newFault(http.StatusInternalServerError, CodeInternal, typeAPI,
			"audit entry for %s carries neither a before nor an after state", action)
	}
	beforeJSON, err := auditJSON(before)
	if err != nil {
		return err
	}
	afterJSON, err := auditJSON(after)
	if err != nil {
		return err
	}

	e := AuditEntry{
		ID:         c.a.cfg.NewID(),
		TS:         c.a.now(),
		ActorKind:  c.p.ActorKind(),
		ActorID:    c.p.ActorID(),
		Action:     action,
		ObjectKind: objectKind,
		ObjectID:   objectID,
		Before:     beforeJSON,
		After:      afterJSON,
		IP:         remoteIP(c.r),
		UserAgent:  truncate(c.r.UserAgent(), maxUserAgent),
	}
	c.a.metrics.mutations.Add(1)
	if err := c.a.cfg.Audit.Record(c.ctx(), e); err != nil {
		c.a.metrics.auditFailures.Add(1)
		f := newFault(http.StatusInternalServerError, CodeAuditWriteFailed, typeAPI,
			"%s was applied to %s %q but the audit row could not be written; do not retry, reconcile instead",
			action, objectKind, objectID)
		f.Detail = map[string]any{"action": action, "object_kind": objectKind, "object_id": objectID}
		return f
	}
	return nil
}

// auditJSON renders a state for the trail.
//
// The value handed in is always one of the redacted view types this package
// returns on the wire, never a store row. That is what makes "an audit row
// cannot carry a secret the API would not" a structural property: there is no
// type in scope here that holds one.
func auditJSON(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", newFault(http.StatusInternalServerError, CodeInternal, typeAPI,
			"audit state could not be encoded")
	}
	return string(b), nil
}

// remoteIP is the peer address, taken from the connection and not from a
// header.
//
// X-Forwarded-For is deliberately ignored: it is set by the caller, and an
// audit trail whose actor address can be chosen by the actor is decoration. A
// deployment behind a proxy should record the proxy's own logs alongside these,
// or terminate identity at the proxy.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
