package app

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/server"
)

// missBudgetYAML is a minimal gateway plus whatever `auth.miss_budget` the test is
// about.
func missBudgetYAML(budget string) string {
	return `
version: 1
auth:
  miss_budget: {` + budget + `}
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`
}

// floodUnknownKeys presents n DISTINCT credentials nobody issued and returns how
// many were refused with 503 auth_unavailable — the answer a request gets when the
// budget is empty and the store was therefore never consulted.
//
// Distinct on purpose: a negative cache entry is keyed by the index key, so
// repeating one key would be answered from cache and would measure nothing.
func floodUnknownKeys(t *testing.T, a *App, n int) (throttled int) {
	t.Helper()
	for i := 0; i < n; i++ {
		secret := "sk-nobody-issued-this-" + strconv.Itoa(i) // pragma: allowlist secret — test fixture
		w := callWith(a, secret, http.MethodGet, "/v1/models", "")
		switch w.Code {
		case http.StatusServiceUnavailable:
			throttled++
		case http.StatusUnauthorized:
		default:
			t.Fatalf("unknown key %d answered %d, which is neither a refusal nor a "+
				"throttle: %s", i, w.Code, w.Body.String())
		}
	}
	return throttled
}

// TestMissBudgetIsReachableFromConfiguration is the wiring, asserted as the status
// an unauthenticated caller gets.
//
// The bucket itself has always worked and internal/auth's own tests prove it. What
// no test covered is the only thing that matters in a deployment: whether the
// numbers an operator writes in the file are the numbers the running authenticator
// bounds with. They were not — auth.Config.MissRate and MissBurst had no path from
// YAML — so every deployment ran the built-in 100/s and 500, whatever the file said,
// because the file could not say anything.
//
// It is asserted as an HTTP status rather than by reading the Config back, and it
// asserts the CONFIGURED number rather than merely "some bound exists": with a burst
// of two, the third distinct unknown key is refused without a store read, and the
// default 500 would let all of them through.
func TestMissBudgetIsReachableFromConfiguration(t *testing.T) {
	// rate 0.001/s so the bucket does not refill inside the test; burst 2, which
	// is far below the built-in 500.
	a := newWiringApp(t, missBudgetYAML(`rate: 0.001, burst: 2`), nil)

	const flood = 12
	throttled := floodUnknownKeys(t, a, flood)
	if want := flood - 2; throttled != want {
		t.Fatalf("%d of %d unknown keys were throttled, want %d: auth.miss_budget.burst "+
			"did not reach the running authenticator, so the process is bounding with "+
			"the built-in %d rather than the configured 2",
			throttled, flood, want, auth.DefaultMissBurst)
	}

	// And the counter an operator watches says the same thing. It is the series
	// that distinguishes "the bound is holding" from "the database is being read"
	// — this one rising while dorang_auth_store_calls_total flattens.
	if got := a.Auth.Stats().LookupsThrottled; got != uint64(flood-2) {
		t.Errorf("dorang_auth_lookup_throttled_total = %d, want %d", got, flood-2)
	}
}

// TestANegativeMissRateRemovesTheBoundFromConfiguration is the other direction, and
// it is not decoration: `0` and "no bound" have to stay different answers, or an
// operator who writes one gets the other. Zero is absence and takes the default;
// negative is the explicit removal.
//
// Removing it restores the amplifier — every distinct unknown key becomes one
// database round trip an unauthenticated caller buys for nothing — so it is a
// deliberate choice, and a deliberate choice has to be expressible.
func TestANegativeMissRateRemovesTheBoundFromConfiguration(t *testing.T) {
	a := newWiringApp(t, missBudgetYAML(`rate: -1`), nil)

	// Well past the built-in burst of 500, so any bound at all would show.
	const flood = auth.DefaultMissBurst + 40
	if throttled := floodUnknownKeys(t, a, flood); throttled != 0 {
		t.Fatalf("%d of %d unknown keys were throttled although auth.miss_budget.rate is "+
			"negative: the explicit \"no bound\" was read as absence and took the default",
			throttled, flood)
	}
	if got := a.Auth.Stats().LookupsThrottled; got != 0 {
		t.Errorf("dorang_auth_lookup_throttled_total = %d with no bound configured", got)
	}
}

// TestTheDefaultMissBudgetSurvivesAnEmptyBlock: an absent block is the built-in
// numbers and not zero, because a rate of zero would refuse every first use of every
// real credential — the whole of a cold node's traffic.
func TestTheDefaultMissBudgetSurvivesAnEmptyBlock(t *testing.T) {
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	cfg, err := config.LoadBytes([]byte(missBudgetYAMLWithoutBudget()))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Auth.MissBudget.Rate != auth.DefaultMissRate ||
		cfg.Auth.MissBudget.Burst != auth.DefaultMissBurst {
		t.Fatalf("an absent auth.miss_budget defaults to %v/%d, want internal/auth's own "+
			"%v/%d: two sets of defaults for one bound is two behaviours nobody chose",
			cfg.Auth.MissBudget.Rate, cfg.Auth.MissBudget.Burst,
			auth.DefaultMissRate, auth.DefaultMissBurst)
	}

	// And the assembled gateway serves ordinary traffic under it: a bound that
	// refused a real key would be worse than no bound.
	a := newWiringApp(t, missBudgetYAMLWithoutBudget(), nil)
	secret := issueKey(t, a, nil)
	if w := callWith(a, secret, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("a real key answered %d under the default miss budget: %s",
			w.Code, w.Body.String())
	}
}

func missBudgetYAMLWithoutBudget() string {
	return `
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`
}

// TestANegativeMissBurstIsALoadError. A negative RATE is the documented "no bound"
// and internal/auth honours it; a negative BURST is read there as absence and
// silently takes the default. Two adjacent fields where a minus sign means opposite
// things is a configuration nobody can read, so one of the two is refused — and the
// refusal names the field that actually removes the bound.
func TestANegativeMissBurstIsALoadError(t *testing.T) {
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	_, err := config.LoadBytes([]byte(missBudgetYAML(`burst: -1`)))
	if err == nil {
		t.Fatal("a negative auth.miss_budget.burst loaded, and would have silently " +
			"taken the default")
	}
	if !strings.Contains(err.Error(), "miss_budget.rate") {
		t.Errorf("the refusal does not name the field that removes the bound: %v", err)
	}
}

// TestConfigMissBudgetDefaultsMatchTheAuths pins the duplication. internal/config
// imports nothing of the gateway — it is the schema, and a schema that needs the
// authenticator cannot be loaded by a tool that does not build one — so the numbers
// are spelled twice and this package is the one that imports both.
func TestConfigMissBudgetDefaultsMatchTheAuths(t *testing.T) {
	cfg := &config.Config{}
	cfg.ApplyDefaults()
	if cfg.Auth.MissBudget.Rate != auth.DefaultMissRate {
		t.Errorf("config default miss rate %v != auth.DefaultMissRate %v",
			cfg.Auth.MissBudget.Rate, auth.DefaultMissRate)
	}
	if cfg.Auth.MissBudget.Burst != auth.DefaultMissBurst {
		t.Errorf("config default miss burst %d != auth.DefaultMissBurst %d",
			cfg.Auth.MissBudget.Burst, auth.DefaultMissBurst)
	}
}

// TestConfigDeadlineDefaultsMatchTheServers pins the other three, the same way and
// for the same reason.
func TestConfigDeadlineDefaultsMatchTheServers(t *testing.T) {
	cfg := &config.Config{}
	cfg.ApplyDefaults()
	for _, p := range []struct {
		name      string
		got, want time.Duration
	}{
		{"read_header_timeout", cfg.Server.ReadHeaderTimeout.Duration(), server.DefaultReadHeaderTimeout},
		{"read_timeout", cfg.Server.ReadTimeout.Duration(), server.DefaultReadTimeout},
		{"idle_timeout", cfg.Server.IdleTimeout.Duration(), server.DefaultIdleTimeout},
	} {
		if p.got != p.want {
			t.Errorf("config default %s = %s, internal/server's is %s", p.name, p.got, p.want)
		}
	}
}
