package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// §2.3: everything is audited, with actor, action, before and after.
//
// This drives every mutating endpoint in sequence and then checks the whole
// trail at once. Checking it as a trail rather than endpoint by endpoint is the
// point: the failure this guards against is a *new* mutating endpoint that
// forgets to audit, and a per-endpoint test would simply not exist for it.
func TestEveryMutationIsAudited(t *testing.T) {
	h := newHarness(t, withReporters)

	keyID, _ := h.newKey(map[string]any{"key_alias": "a"})
	steps := []struct {
		action string
		method string
		path   string
		body   any
	}{
		{"user.new", http.MethodPost, "/user/new", map[string]any{"user_id": "u1", "user_email": "u1@x.test"}},
		{"user.update", http.MethodPost, "/user/update", map[string]any{"user_id": "u1", "user_name": "U One"}},
		{"team.new", http.MethodPost, "/team/new", map[string]any{"team_id": "t1", "team_name": "T"}},
		{"team.update", http.MethodPost, "/team/update", map[string]any{"team_id": "t1", "team_name": "T2"}},
		{"team.member_add", http.MethodPost, "/team/member_add", map[string]any{"team_id": "t1", "user_id": "u1"}},
		{"team.member_delete", http.MethodPost, "/team/member_delete", map[string]any{"team_id": "t1", "user_id": "u1"}},
		{"model.new", http.MethodPost, "/model/new", map[string]any{
			"id": "d1", "model_name": "g", "dorang_params": map[string]any{"provider": "p", "model": "m"}}},
		{"model.update", http.MethodPost, "/model/update", map[string]any{"id": "d1", "enabled": false}},
		{"budget.new", http.MethodPost, "/budget/new", map[string]any{
			"subject_kind": "team", "subject_id": "t1", "max_budget": 10}},
		{"budget.update", http.MethodPost, "/budget/update", map[string]any{
			"budget_id": "team:t1", "max_budget": 20}},
		{"budget.delete", http.MethodPost, "/budget/delete", map[string]any{"budget_id": "team:t1"}},
		{"key.update", http.MethodPost, "/key/update", map[string]any{"key_id": keyID, "key_alias": "b"}},
		{"key.block", http.MethodPost, "/key/block", map[string]any{"key_id": keyID}},
		{"key.unblock", http.MethodPost, "/key/unblock", map[string]any{"key_id": keyID}},
		{"key.regenerate", http.MethodPost, "/key/regenerate", map[string]any{"key_id": keyID}},
		{"key.delete", http.MethodPost, "/key/delete", map[string]any{"keys": []string{keyID}}},
		{"model.delete", http.MethodPost, "/model/delete", map[string]any{"id": "d1"}},
		{"team.delete", http.MethodPost, "/team/delete", map[string]any{"team_ids": []string{"t1"}}},
		{"user.delete", http.MethodPost, "/user/delete", map[string]any{"user_ids": []string{"u1"}}},
		{"config.reload", http.MethodPost, "/admin/config/reload", nil},
	}

	want := []string{"key.generate"}
	for _, s := range steps {
		rec := h.do(s.method, s.path, s.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d\n%s", s.action, rec.Code, rec.Body.String())
		}
		want = append(want, s.action)
	}

	trail := h.store.auditLog()
	if len(trail) != len(want) {
		var got []string
		for _, e := range trail {
			got = append(got, e.Action)
		}
		t.Fatalf("audit trail has %d rows, want %d\ngot:  %v\nwant: %v",
			len(trail), len(want), got, want)
	}
	for i, e := range trail {
		if e.Action != want[i] {
			t.Errorf("row %d action = %q, want %q", i, e.Action, want[i])
		}
		if e.ID == "" || e.TS.IsZero() {
			t.Errorf("row %d has no id or timestamp: %+v", i, e)
		}
		if e.ActorKind == "" {
			t.Errorf("row %d has no actor kind", i)
		}
		if e.ObjectKind == "" || e.ObjectID == "" {
			t.Errorf("row %d does not name its object: %+v", i, e)
		}
		if e.Before == "" && e.After == "" {
			t.Errorf("row %d records neither a before nor an after state", i)
		}
		for _, state := range []string{e.Before, e.After} {
			if state == "" {
				continue
			}
			if !json.Valid([]byte(state)) {
				t.Errorf("row %d state is not valid JSON: %q", i, state)
			}
		}
	}

	// Creations carry no before; deletions carry no after. That asymmetry is
	// what makes a trail reconstructable.
	byAction := map[string]AuditEntry{}
	for _, e := range trail {
		byAction[e.Action] = e
	}
	for _, a := range []string{"key.generate", "user.new", "team.new", "model.new"} {
		if byAction[a].Before != "" {
			t.Errorf("%s recorded a before state", a)
		}
		if byAction[a].After == "" {
			t.Errorf("%s recorded no after state", a)
		}
	}
	if byAction["model.delete"].After != "" {
		t.Error("model.delete recorded an after state")
	}
	for _, a := range []string{"user.update", "team.update", "model.update", "key.update", "budget.update"} {
		e := byAction[a]
		if e.Before == "" || e.After == "" {
			t.Errorf("%s does not carry both states", a)
		}
		if e.Before == e.After {
			t.Errorf("%s recorded an unchanged state on both sides", a)
		}
	}
}

// The actor is recorded from the authenticated principal, and the address from
// the connection rather than from a header the caller controls.
func TestAuditActorAndAddress(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodPost, "/key/generate", map[string]any{}, asToken(adminToken),
		func(r *http.Request) {
			r.RemoteAddr = "203.0.113.9:44321"
			r.Header.Set("X-Forwarded-For", "198.51.100.1")
		})
	e, ok := h.store.lastAudit()
	if !ok {
		t.Fatal("no audit row")
	}
	if e.ActorKind != "key" || e.ActorID != "key-admin" {
		t.Errorf("actor = %s/%s", e.ActorKind, e.ActorID)
	}
	if e.IP != "203.0.113.9" {
		t.Errorf("ip = %q, want the peer address", e.IP)
	}
	if strings.Contains(e.IP, "198.51.100.1") {
		t.Error("the audit trail trusted a caller-supplied X-Forwarded-For")
	}
}

func TestAuditUserAgentIsBounded(t *testing.T) {
	h := newHarness(t)
	long := strings.Repeat("x", 4096)
	h.do(http.MethodPost, "/key/generate", map[string]any{}, func(r *http.Request) {
		r.Header.Set("User-Agent", long)
	})
	e, _ := h.store.lastAudit()
	if len(e.UserAgent) != maxUserAgent {
		t.Fatalf("user agent length = %d, want %d", len(e.UserAgent), maxUserAgent)
	}
}

// The surface is served concurrently, so it is exercised concurrently. This is
// the test the -race flag is for.
func TestConcurrentUse(t *testing.T) {
	h := newHarness(t, withReporters)
	seedLedger(h)
	h.newKey(map[string]any{"key_alias": "seed"})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				h.do(http.MethodPost, "/key/generate", map[string]any{"key_alias": "c"})
			case 1:
				h.do(http.MethodGet, "/key/list", nil)
			case 2:
				h.do(http.MethodGet,
					"/spend/logs?start_date=2026-07-25&end_date=2026-07-29", nil)
			case 3:
				h.do(http.MethodGet, "/ui/usage", nil)
			}
		}(i)
	}
	wg.Wait()

	if got := h.api.Metrics().Requests; got == 0 {
		t.Error("no requests were counted")
	}
	if got := h.api.Metrics().KeysIssued; got != 5 {
		t.Errorf("keys issued = %d, want 5", got)
	}
}

func TestMoneyRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		in   string
		nano int64
		out  string
	}{
		{"0", 0, "0"},
		{"1", 1_000_000_000, "1"},
		{"1.5", 1_500_000_000, "1.5"},
		{"0.123456789", 123456789, "0.123456789"},
		{"-0.25", -250_000_000, "-0.25"},
		{"1000000000", 1_000_000_000_000_000_000, "1000000000"},
		{".5", 500_000_000, "0.5"},
	} {
		got, err := parseNano(tc.in)
		if err != nil {
			t.Fatalf("parseNano(%q): %v", tc.in, err)
		}
		if got != tc.nano {
			t.Errorf("parseNano(%q) = %d, want %d", tc.in, got, tc.nano)
		}
		if back := formatNano(got); back != tc.out {
			t.Errorf("formatNano(%d) = %q, want %q", got, back, tc.out)
		}
	}
	for _, bad := range []string{"", "abc", "1e9", "0.1234567891", "1..2", "2000000000", "-"} {
		if _, err := parseNano(bad); err == nil {
			t.Errorf("parseNano(%q) was accepted", bad)
		}
	}
}

func TestStampRendersZeroAsNull(t *testing.T) {
	b, err := json.Marshal(struct {
		At Stamp `json:"at"`
	}{})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"at":null}` {
		t.Fatalf("zero instant rendered as %s; never and the epoch are different answers", b)
	}
}
