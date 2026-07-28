package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/app"
)

// TestUpstream5xxKeepsItsOwnStatus settles COMPATIBILITY §11.2's upstream-5xx
// row, which said "→ 502" while dorang has always passed the status through.
//
// The document and the code disagreed and the code is right, for two reasons.
// A `502` is a statement about the HOP — dorang could not get a usable answer —
// and folding a provider's `500` into one tells a client that dorang failed
// when the provider did. And the passthrough is what keeps dorang
// status-identical to the incumbent on an upstream failure: the reproduction
// that found this disagreement had nine such rows, and every one of them
// matched only because the status survived.
//
// A retired model answering `500 "<model> was retired"` must reach the client as
// a `500`. A client that retries on `502` and gives up on `500` is told the
// truth either way only if the status is not rewritten.
func TestUpstream5xxKeepsItsOwnStatus(t *testing.T) {
	for _, upstreamStatus := range []int{500, 502, 503} {
		t.Run(strconv.Itoa(upstreamStatus), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("DORANG_KEY_PEPPER", "upstream-status-pepper")

			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(upstreamStatus)
				_, _ = io.WriteString(w,
					`{"error":{"message":"model-x was retired at 2026-07-15","type":"server_error"}}`)
			}))
			defer up.Close()

			ctx := context.Background()
			a, err := app.New(ctx, app.Options{
				Config: loadRoundTripConfig(t, dir, up.URL),
				Logf:   t.Logf,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close(ctx)

			token := issueKey(t, ctx, a.Store)
			front := httptest.NewServer(a.Server)
			defer front.Close()

			resp, err := postErr(front.URL+"/v1/chat/completions", token, chatRequest, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if resp.status != upstreamStatus {
				t.Errorf("upstream answered %d, client saw %d: the provider's own status was "+
					"rewritten, so a client cannot tell a retired model from a gateway that "+
					"could not reach one\nbody = %s", upstreamStatus, resp.status, resp.body)
			}
		})
	}
}
