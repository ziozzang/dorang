package cluster

import (
	"errors"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/store"
)

// [AuthPrincipal] carries all three subjects of DESIGN §11.2, not one.
//
// This is the conversion that decides what the gateway enforces. It built an
// [auth.Principal] with `Key:` limits only, and because [auth.Principal] is
// nil-guarded on `User` and `Team` — as it must be, since an unowned key really
// has neither — nothing failed and nothing was enforced. `users.blocked`
// refused no request, a team ceiling bound no budget, and a team's rate and
// concurrency limits reached no gate.
//
// Every assertion here is on what Authorize DECIDES rather than on which field
// was copied, because a field-copy assertion is what a caller that never
// supplies the owners still passes.
func TestAuthPrincipalCarriesTheOwnersEnvelope(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	key := &store.APIKey{ID: "k1", UserID: "u1", TeamID: "t1", Tier: "commercial"}

	t.Run("an unowned key has no owner envelope", func(t *testing.T) {
		p, err := AuthPrincipal(&store.APIKey{ID: "k0"}, store.Owners{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p.User != nil || p.Team != nil {
			t.Fatalf("an unowned key produced %+v / %+v; nil is what says 'no such owner', "+
				"and it is what makes RequireOwner mean anything", p.User, p.Team)
		}
		if err := p.Authorize(auth.Access{Now: now}); err != nil {
			t.Errorf("an unowned key was refused: %v", err)
		}
	})

	t.Run("a blocked user refuses", func(t *testing.T) {
		p, err := AuthPrincipal(key, store.Owners{
			User: &store.User{ID: "u1", Blocked: true},
			Team: &store.Team{ID: "t1"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = p.Authorize(auth.Access{Now: now})
		if err == nil {
			t.Fatal("a blocked user's key was authorized: users.blocked reaches no decision")
		}
		if !isReason(err, auth.ReasonBlocked) {
			t.Errorf("a blocked user refused with %v, want ReasonBlocked", err)
		}
		// And the refusal has to be visible to the authenticator's cache
		// lifetime rule too, or a blocked user's entry is held for the SERVING
		// TTL and re-checked a minute later (§11.2c rule 3).
		if !p.Refusing(now) {
			t.Error("a blocked user's principal does not report itself as refusing, so its " +
				"cache entry gets the serving lifetime")
		}
	})

	t.Run("a blocked team refuses", func(t *testing.T) {
		p, err := AuthPrincipal(key, store.Owners{Team: &store.Team{ID: "t1", Blocked: true}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Authorize(auth.Access{Now: now}); !isReason(err, auth.ReasonBlocked) {
			t.Errorf("a blocked team refused with %v, want ReasonBlocked", err)
		}
	})

	t.Run("owner limits are carried whole", func(t *testing.T) {
		userBudget, teamBudget := int64(1_000), int64(2_000)
		p, err := AuthPrincipal(key, store.Owners{
			User: &store.User{
				ID: "u1", MaxBudgetNano: &userBudget, BudgetPeriod: "monthly",
				RPMLimit: auth.Limit(7), TPMLimit: auth.Limit(8), Models: []string{"model-a"},
			},
			Team: &store.Team{
				ID: "t1", MaxBudgetNano: &teamBudget, BudgetPeriod: "daily",
				RPMLimit: auth.Limit(9), TPMLimit: auth.Limit(10), MaxParallel: auth.Limit(3),
				Models: []string{"model-a", "model-b"},
			},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Fatal rather than a dereference: a conversion that drops the owners
		// must fail this test BY NAME, not crash the package and take every
		// other named failure with it.
		if p.User == nil || p.Team == nil {
			t.Fatalf("the owners were dropped: User=%v Team=%v", p.User, p.Team)
		}
		// A ceiling separated from its period is a ceiling whose reset date the
		// gate has to guess, and a monthly budget guessed as daily is thirty
		// times too permissive.
		if p.User.BudgetPeriod != "monthly" || p.Team.BudgetPeriod != "daily" {
			t.Errorf("periods = %q / %q, want the ones each subject declared",
				p.User.BudgetPeriod, p.Team.BudgetPeriod)
		}
		if p.User.MaxBudgetNanoUSD == nil || *p.User.MaxBudgetNanoUSD != userBudget {
			t.Errorf("user ceiling = %v", p.User.MaxBudgetNanoUSD)
		}
		if p.Team.MaxBudgetNanoUSD == nil || *p.Team.MaxBudgetNanoUSD != teamBudget {
			t.Errorf("team ceiling = %v", p.Team.MaxBudgetNanoUSD)
		}
		// The concurrency ceiling internal/capacity is handed. The team column
		// is the only one of the three tables that has it.
		if got := auth.MostRestrictiveParallel(&p.Key, p.User, p.Team); got != 3 {
			t.Errorf("MostRestrictiveParallel = %d, want the team's 3", got)
		}
		// The most restrictive allow-list wins across subjects, which only works
		// if both are present.
		if err := p.Authorize(auth.Access{Now: now, Model: "model-b"}); err == nil {
			t.Error("model-b was authorized: the USER's allow-list, which excludes it, was not read")
		}
		if err := p.Authorize(auth.Access{Now: now, Model: "model-a"}); err != nil {
			t.Errorf("model-a, which both subjects allow, was refused: %v", err)
		}
	})

	t.Run("a tier narrows the key and not its owners", func(t *testing.T) {
		tiers, err := auth.NewTierSet([]auth.Tier{
			{Name: "commercial", Rank: 0, RPMLimit: auth.Limit(10)},
		}, "commercial")
		if err != nil {
			t.Fatal(err)
		}
		p, err := AuthPrincipal(key, store.Owners{
			User: &store.User{ID: "u1", RPMLimit: auth.Limit(1_000)},
			Team: &store.Team{ID: "t1"},
		}, tiers)
		if err != nil {
			t.Fatal(err)
		}
		if p.User == nil || p.Team == nil {
			t.Fatalf("the owners were dropped: User=%v Team=%v", p.User, p.Team)
		}
		if p.Key.RPMLimit == nil || *p.Key.RPMLimit != 10 {
			t.Errorf("the key's rpm = %v, want the tier's ceiling", p.Key.RPMLimit)
		}
		// A tier is granted to a CREDENTIAL (§11.6). Applying it to the user
		// and the team as well would narrow one grant three times and cap three
		// independent counters at the same number, which is not what "the most
		// restrictive wins" means.
		if p.User.RPMLimit == nil || *p.User.RPMLimit != 1_000 {
			t.Errorf("the tier narrowed the USER's rpm to %v; a tier belongs to the key",
				p.User.RPMLimit)
		}
		if p.Team.RPMLimit != nil {
			t.Errorf("the tier gave the team an rpm ceiling of %v that it never declared",
				p.Team.RPMLimit)
		}
	})
}

// AuthRecord carries the owners too, so the credential CACHE holds all three
// subjects. The snapshot is what the hot path answers from, and an entry short
// one subject is an entry that fails open on that subject for its whole life.
func TestAuthRecordCarriesTheOwnersIntoTheCachedEntry(t *testing.T) {
	rec, err := AuthRecord(
		&store.APIKey{ID: "k1", UserID: "u1", TeamID: "t1"},
		&store.KeySecret{
			ID: "k1.1", KeyID: "k1", Generation: 1,
			Lookup:     "0123456789abcdef0123456789abcdef",
			TokenHash:  "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
			HashScheme: store.SchemeDorangV1,
		},
		store.Owners{
			User: &store.User{ID: "u1", Blocked: true},
			Team: &store.Team{ID: "t1", MaxBudgetNano: auth.Limit(5)},
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Principal.User == nil || !rec.Principal.User.Blocked {
		t.Error("the cached entry does not carry the owning user's block")
	}
	if rec.Principal.Team == nil || rec.Principal.Team.MaxBudgetNanoUSD == nil {
		t.Error("the cached entry does not carry the owning team's ceiling")
	}
}

func isReason(err error, want auth.Reason) bool {
	var ae *auth.Error
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Reason == want
}
