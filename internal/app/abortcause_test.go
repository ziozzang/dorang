package app

import (
	"testing"

	"github.com/ziozzang/dorang/internal/backend"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
)

// A caller who hung up, or a stream already flowing that hit the provider time
// limit, is classed aborted — which counts for nothing against the deployment —
// and is not offered to another deployment (board #1623).
func TestAbortedExchangeIsNotADeploymentFailure(t *testing.T) {
	e := server.NewError(502, server.TypeAPIError, "the upstream stream ended abnormally")
	br := backend.Result{Err: e, Status: 200, Aborted: true, Timeout: true, Retryable: true}
	if got := backendCause(br); got != router.CauseAborted {
		t.Fatalf("cause %v, want aborted", got)
	}
	if backendResult(br).retryable {
		t.Error("an aborted exchange was offered to another deployment")
	}
	// The same stream cut by the upstream itself stays a failure.
	br.Aborted, br.Timeout = false, false
	if got := backendCause(br); got != router.CauseUpstream5xx {
		t.Fatalf("cause %v, want upstream_5xx", got)
	}
}
