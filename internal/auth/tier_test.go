package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// DESIGN §11.6: the tier decides what a key may claim, a per-key setting may
// narrow it and never widen it, and batch sits below every tier's interactive
// work.

func tiersForTest(t *testing.T) *TierSet {
	t.Helper()
	s, err := NewTierSet([]Tier{
		{
			Name: "free", Rank: 0, PriorityClass: "batch",
			MaxBudgetNanoUSD: Limit(10_000), RPMLimit: Limit(10), TPMLimit: Limit(1000),
			MaxParallel: Limit(2), Models: []string{"small", "medium"},
		},
		{
			Name: "commercial", Rank: 1, PriorityClass: "interactive",
			MaxBudgetNanoUSD: Limit(1_000_000), RPMLimit: Limit(600), TPMLimit: Limit(100_000),
			MaxParallel: Limit(32),
		},
		{Name: "unlimited", Rank: 2, PriorityClass: "realtime"},
	}, "free")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTierDefaultsNarrowButNeverWiden(t *testing.T) {
	tiers := tiersForTest(t)
	free, _ := tiers.Get("free")

	t.Run("a key that asks for more than its tier gets its tier's", func(t *testing.T) {
		got := free.Apply(Limits{
			MaxBudgetNanoUSD: Limit(999_999_999),
			RPMLimit:         Limit(100_000),
			TPMLimit:         Limit(9_000_000),
			MaxParallel:      Limit(512),
			Models:           []string{"small", "medium", "enormous"},
		})
		wantInt(t, "max_budget", got.MaxBudgetNanoUSD, 10_000)
		wantInt(t, "rpm", got.RPMLimit, 10)
		wantInt(t, "tpm", got.TPMLimit, 1000)
		wantInt(t, "max_parallel", got.MaxParallel, 2)
		wantModels(t, got.Models, []string{"small", "medium"})
	})

	t.Run("a key that asks for less than its tier keeps its own", func(t *testing.T) {
		got := free.Apply(Limits{
			MaxBudgetNanoUSD: Limit(500),
			RPMLimit:         Limit(1),
			TPMLimit:         Limit(10),
			MaxParallel:      Limit(1),
			Models:           []string{"small"},
		})
		wantInt(t, "max_budget", got.MaxBudgetNanoUSD, 500)
		wantInt(t, "rpm", got.RPMLimit, 1)
		wantInt(t, "tpm", got.TPMLimit, 10)
		wantInt(t, "max_parallel", got.MaxParallel, 1)
		wantModels(t, got.Models, []string{"small"})
	})

	t.Run("a key with nothing of its own inherits the tier", func(t *testing.T) {
		got := free.Apply(Limits{})
		wantInt(t, "max_budget", got.MaxBudgetNanoUSD, 10_000)
		wantModels(t, got.Models, []string{"small", "medium"})
	})

	t.Run("an unlimited tier imposes nothing and does not erase the key's own", func(t *testing.T) {
		unl, _ := tiers.Get("unlimited")
		got := unl.Apply(Limits{MaxBudgetNanoUSD: Limit(42), Models: []string{"x"}})
		wantInt(t, "max_budget", got.MaxBudgetNanoUSD, 42)
		wantModels(t, got.Models, []string{"x"})
		if unl.Apply(Limits{}).MaxBudgetNanoUSD != nil {
			t.Error("an unlimited tier invented a budget ceiling")
		}
	})

	t.Run("a model outside the tier cannot be granted by the key", func(t *testing.T) {
		got := free.Apply(Limits{Models: []string{"enormous"}})
		// Not nil: nil is "unrestricted", which is the opposite of what an
		// empty intersection means.
		if len(got.Models) == 0 {
			t.Fatal("an empty intersection became an unrestricted model list, which widens the key")
		}
		if allowedIn(got.Models, "enormous", false) {
			t.Error("the key kept a model its tier does not grant")
		}
		if allowedIn(got.Models, "small", false) {
			t.Error("the key gained a model it did not ask for")
		}
	})

	t.Run("applying twice changes nothing", func(t *testing.T) {
		in := Limits{MaxBudgetNanoUSD: Limit(999_999), Models: []string{"small", "medium", "enormous"}}
		once := free.Apply(in)
		twice := free.Apply(once)
		wantInt(t, "max_budget", twice.MaxBudgetNanoUSD, *once.MaxBudgetNanoUSD)
		wantModels(t, twice.Models, once.Models)
	})

	t.Run("a tier ceiling of zero means zero, not absent", func(t *testing.T) {
		suspended := Tier{Name: "suspended", MaxBudgetNanoUSD: Limit(0)}
		got := suspended.Apply(Limits{MaxBudgetNanoUSD: Limit(1_000)})
		wantInt(t, "max_budget", got.MaxBudgetNanoUSD, 0)
		if !got.BudgetExceeded(time.Now()) {
			t.Error("a zero ceiling did not refuse")
		}
	})
}

func TestTierPriorityClassNarrowsButNeverWidens(t *testing.T) {
	tiers := tiersForTest(t)
	classes := map[string]int{"realtime": 0, "interactive": 2, "batch": 10}
	comm, _ := tiers.Get("commercial")

	if got := comm.NarrowClass("realtime", classes); got != "interactive" {
		t.Errorf("a commercial key claimed realtime and got %q; the tier is the ceiling", got)
	}
	if got := comm.NarrowClass("batch", classes); got != "batch" {
		t.Errorf("a commercial key asked to be scheduled lower and got %q", got)
	}
	if got := comm.NarrowClass("", classes); got != "interactive" {
		t.Errorf("a key naming no class got %q, want its tier's", got)
	}
	if got := comm.NarrowClass("nonsense", classes); got != "interactive" {
		t.Errorf("a key naming an unknown class got %q, want its tier's", got)
	}
}

func TestBatchRanksBelowEveryTiersInteractiveWork(t *testing.T) {
	for _, tiers := range []*TierSet{DefaultTiers(), tiersForTest(t), oneTierSet(t), fiveTierSet(t)} {
		names := tiers.Names()

		// The property, stated as the design states it: ANY tier's batch
		// traffic yields to ANY tier's interactive traffic. Lower canonical is
		// more urgent (§7.5), so every batch value must exceed every
		// interactive one.
		for _, b := range names {
			for _, i := range names {
				batch := tiers.Canonical(b, true)
				inter := tiers.Canonical(i, false)
				if batch <= inter {
					t.Errorf("%d tiers: batch(%s)=%d does not yield to interactive(%s)=%d",
						len(names), b, batch, i, inter)
				}
			}
		}

		// The most privileged tier's batch work still yields to the least
		// privileged tier's interactive work, which is the case the design
		// singles out and the one a naive per-tier scale gets wrong.
		top, bottom := names[len(names)-1], names[0]
		if tiers.Canonical(top, true) <= tiers.Canonical(bottom, false) {
			t.Errorf("a paying customer's batch job (%s) outranks a free user's interactive request (%s)",
				top, bottom)
		}

		if tiers.BatchFloor() <= tiers.InteractiveCeiling() {
			t.Errorf("the published separation is wrong: batch floor %d is not above interactive ceiling %d",
				tiers.BatchFloor(), tiers.InteractiveCeiling())
		}

		// More privileged is more urgent within a mode.
		for i := 1; i < len(names); i++ {
			lo, hi := names[i-1], names[i]
			if tiers.Canonical(hi, false) >= tiers.Canonical(lo, false) {
				t.Errorf("%s does not outrank %s for interactive work", hi, lo)
			}
			if tiers.Canonical(hi, true) >= tiers.Canonical(lo, true) {
				t.Errorf("%s does not outrank %s for batch work", hi, lo)
			}
		}
	}
}

func TestDefaultTiersReproduceTheDesignsOwnScale(t *testing.T) {
	// §7.5's example is {realtime: 0, interactive: 2, batch: 10}. The default
	// three-tier set has to land on it, or the two sections describe different
	// schedulers.
	s := DefaultTiers()
	for _, tc := range []struct {
		tier  string
		batch bool
		want  int
	}{
		{"unlimited", false, 0},
		{"commercial", false, 2},
		{"free", false, 4},
		{"unlimited", true, 6},
		{"commercial", true, 8},
		{"free", true, 10},
	} {
		if got := s.Canonical(tc.tier, tc.batch); got != tc.want {
			t.Errorf("Canonical(%s, batch=%t) = %d, want %d", tc.tier, tc.batch, got, tc.want)
		}
	}
}

func TestATierIsOperatorAssignedAndACallerCannotClaimOne(t *testing.T) {
	// §10.5's rule, applied to tiers: an operator can grant, a caller cannot
	// claim. The only path a tier reaches a principal by is the stored record.
	tiers := tiersForTest(t)
	tok := "sk-tier-claim-test" // pragma: allowlist secret — test fixture
	a := newAuth(t, Config{Tiers: tiers})
	loadOne(t, a, tok, func(r *Record) {
		r.Principal.Tier = "free"
		r.Principal.KeyID = "k1"
	})

	// Every shape a caller has to say something in: a header, a header in the
	// same family the gateway reads, and a query-ish free-form value. None of
	// them is consulted by anything in this package.
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	h.Set("x-dorang-tier", "unlimited")
	h.Set("x-dorang-priority", "realtime")
	h.Set("x-dorang-priority-class", "realtime")

	p, err := a.AuthenticateHeader(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tier != "free" {
		t.Fatalf("a caller claimed a tier through a header and got %q", p.Tier)
	}

	// The authenticator exposes the tier SET, which is configuration, and no
	// setter through which a request could reach one.
	if a.Tiers() != tiers {
		t.Error("the authenticator is not using the configured tier set")
	}

	// And the grant path: the tier only changes when the stored record changes,
	// which is an operator action.
	loadOne(t, a, tok, func(r *Record) {
		r.Principal.Tier = "unlimited"
		r.Principal.KeyID = "k1"
	})
	p, err = a.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tier != "unlimited" {
		t.Fatalf("an operator granted a tier and the principal reports %q", p.Tier)
	}
}

func TestTierSetRefusesConfigurationsThatCannotBeHonoured(t *testing.T) {
	if _, err := NewTierSet(nil, "free"); err == nil {
		t.Error("an empty tier set was accepted")
	}
	if _, err := NewTierSet([]Tier{{Name: "a"}, {Name: "a"}}, "a"); err == nil {
		t.Error("a duplicate tier name was accepted")
	}
	if _, err := NewTierSet([]Tier{{Name: "a"}}, "b"); err == nil {
		t.Error("a default naming no tier was accepted")
	}
	if _, err := NewTierSet([]Tier{{Name: "  "}}, ""); err == nil {
		t.Error("a nameless tier was accepted")
	}
	s, err := NewTierSet([]Tier{{Name: "a"}, {Name: "b"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	// An unnamed default takes the LEAST privileged tier. Defaulting upward is
	// how an unassigned key becomes unlimited.
	if s.Default() != "a" {
		t.Errorf("the implicit default is %q, want the least privileged tier", s.Default())
	}
}

func TestAnUnknownTierIsAnErrorAndNotADefault(t *testing.T) {
	s := DefaultTiers()
	if _, err := s.Resolve("gold"); err == nil {
		t.Error("a row naming a tier the configuration does not have resolved silently")
	}
	got, err := s.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "free" {
		t.Errorf("a row naming NO tier resolved to %q, want the default", got.Name)
	}
}

// --- helpers -----------------------------------------------------------------

func oneTierSet(t *testing.T) *TierSet {
	t.Helper()
	s, err := NewTierSet([]Tier{{Name: "only"}}, "only")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fiveTierSet(t *testing.T) *TierSet {
	t.Helper()
	var tiers []Tier
	for i, n := range []string{"trial", "free", "starter", "commercial", "unlimited"} {
		tiers = append(tiers, Tier{Name: n, Rank: i})
	}
	s, err := NewTierSet(tiers, "trial")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func wantInt(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil, want %d", name, want)
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", name, *got, want)
	}
}

func wantModels(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
}
