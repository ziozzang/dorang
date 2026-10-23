package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExperientialCredits(t *testing.T) {
	called := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/api/v1/credits" || r.Header.Get("Authorization") != "Bearer test" {
			t.Error("incorrect credits request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"total_credits":46.0,"total_usage":0.0000384}}`))
	}))
	defer up.Close()
	s := experientialSource{}
	req, err := s.request(context.Background(), up.URL, "test", "dorang-test")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !called || s.endpoint("") != "https://api.experientiallabs.ai/api/v1/credits" {
		t.Fatal("incorrect source endpoint")
	}
	reading, err := s.decode([]byte(`{"data":{"total_credits":46.0,"total_usage":0.0000384}}`))
	if err != nil || len(reading.balances) != 1 {
		t.Fatal("balance not decoded", err)
	}
	if got := reading.balances[0].RemainingNano; got != 45999961600 {
		t.Fatalf("remaining nano USD %d", got)
	}
	if _, err := s.decode([]byte(`{"data":{"total_credits":46.0}}`)); err == nil {
		t.Error("missing usage accepted")
	}
}

// Two provider policies have separate credential sets in the registry. Their
// fetcher identities must remain distinct although the account endpoint agrees.
func TestExperientialPolicyProbeIdentities(t *testing.T) {
	for _, name := range []string{"experiential", "experiential-zdr"} {
		p, err := New(name, Config{})
		if err != nil {
			t.Fatal(err)
		}
		if p.ProviderID() != name || p.Endpoint() != "https://api.experientiallabs.ai/api/v1/credits" {
			t.Fatalf("incorrect policy probe %s", name)
		}
	}
}
