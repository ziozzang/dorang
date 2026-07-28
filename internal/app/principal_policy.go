package app

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
)

// HeaderClientPriority is the inbound priority hint (§10.5).
//
// It is the same spelling dorang EMITS to a backend, deliberately: a client that
// already speaks the header to an engine speaks it here, and an operator reading
// a trace sees one name rather than two.
const HeaderClientPriority = "X-Request-Priority"

// applyPrincipalPolicy carries what the AUTHENTICATED CALLER decides into the
// routing request: their priority class, their priority hint if an operator
// granted them one, and their own concurrency ceiling.
//
// All three were loaded and dropped before this. The key's priority_class
// reached auth.Principal and stopped there, so every request routed as the
// default class; the client's hint was never read from anywhere, so §10.5's
// "allow" branch had no input even once a range could be configured; and
// max_parallel_requests reached auth.Limits with a comment claiming
// internal/capacity enforced it, which internal/capacity had never heard of.
func applyPrincipalPolicy(rq *server.Request, rr *router.Request) {
	p, ok := rq.Principal.(*principal)
	if !ok || p == nil {
		return
	}
	rr.PriorityClass = p.priorityClass()
	rr.PrincipalMax = p.maxParallel()
	rr.PriorityHint = clientPriorityHint(rq.HTTP.Header)
}

// clientPriorityHint reads the inbound hint, or nil when there is none.
//
// A malformed value is nil rather than an error: the hint is advisory, the
// router ignores it unless a grant exists, and refusing a whole request over an
// unparseable advisory header would turn a courtesy into an outage. It is
// reported as dropped either way, which is what §10.5 requires.
func clientPriorityHint(h http.Header) *int {
	v := strings.TrimSpace(h.Get(HeaderClientPriority))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}
