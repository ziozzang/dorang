package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/store"
)

// A request through the HTTP surface attributes itself to the calling
// credential's user and team, all the way to the ledger row and the daily team
// rollup.
//
// This is the assembly's version of a table nobody writes. DESIGN §9.4
// materializes usage_by_team_day precisely so a team's spend can be answered
// without scanning the ledger — but the team is only knowable at the gate, and
// server.Principal used to expose nothing but a key id. Everything downstream
// then had an empty team: the ledger's team_id was NULL and the rollup stayed
// empty forever, with no error anywhere to say so.
func TestTeamAndUserReachTheLedgerAndTheTeamRollup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "round-trip-pepper")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamAnswer)
	}))
	defer up.Close()

	cfg := loadRoundTripConfig(t, dir, up.URL)
	ctx := context.Background()
	a, err := app.New(ctx, app.Options{Config: cfg, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)

	const (
		token  = "sk-team-attribution-token" // pragma: allowlist secret — test fixture
		userID = "user-ada"
		teamID = "team-research"
	)
	k := &store.APIKey{KeyAlias: "team-attribution", UserID: userID, TeamID: teamID}
	if err := a.Store.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.InsertAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}

	front := httptest.NewServer(a.Server)
	defer front.Close()

	resp, err := postErr(front.URL+"/v1/chat/completions", token, chatRequest, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.status, resp.body)
	}
	requestID := resp.header.Get("X-Dorang-Request-Id")
	if requestID == "" {
		t.Fatal("no request id; the ledger cannot be joined against")
	}

	// The ledger row. Metering is asynchronous by design (§9.1), so the flush is
	// driven rather than waited on.
	var row store.RequestLog
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := a.Meter.Flush(ctx); err != nil {
			t.Fatalf("meter flush: %v", err)
		}
		page, err := a.Store.ListRequestsByKey(ctx, k.ID,
			store.TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour)},
			store.Page{Limit: 10})
		if err != nil {
			t.Fatalf("ledger query: %v", err)
		}
		if len(page.Rows) > 0 {
			row = page.Rows[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no ledger row was ever written")
		}
		time.Sleep(25 * time.Millisecond)
	}

	if row.TeamID != teamID {
		t.Errorf("ledger team_id = %q, want %q — usage_by_team_day is written from this",
			row.TeamID, teamID)
	}
	if row.UserID != userID {
		t.Errorf("ledger user_id = %q, want %q — §9.3's per-user index indexes nothing without it",
			row.UserID, userID)
	}

	// The rollup §9.4 exists for. It is keyed by the UTC day, which is the same
	// flooring the metering sink applies.
	day := time.Now().UTC().Truncate(24 * time.Hour)
	var rollup store.UsageDelta
	deadline = time.Now().Add(10 * time.Second)
	for {
		if err := a.Meter.Flush(ctx); err != nil {
			t.Fatalf("meter flush: %v", err)
		}
		rollup, err = a.Store.ReadTeamDay(ctx, store.TeamDayKey{Day: day, TeamID: teamID})
		if err != nil {
			t.Fatalf("ReadTeamDay: %v", err)
		}
		if rollup.Requests > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage_by_team_day has no row for %s on %s; the table is written by "+
				"nothing on the HTTP path", teamID, day.Format("2006-01-02"))
		}
		time.Sleep(25 * time.Millisecond)
	}
	if rollup.TotalTokens != 14 {
		t.Errorf("team rollup total_tokens = %d, want 14", rollup.TotalTokens)
	}
	const wantCost = 11*3_000 + 3*15_000
	if rollup.CostNano != wantCost {
		t.Errorf("team rollup cost = %d nano-USD, want %d", rollup.CostNano, wantCost)
	}
}
