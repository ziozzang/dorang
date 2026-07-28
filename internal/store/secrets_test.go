package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// DESIGN §11.2c. The identity is durable and the secret is not: a rotation
// mints a new secret and touches nothing else. Every assertion below is about
// something surviving that a re-provisioning would have reset.

func newRotationKey(t *testing.T, s *Store, token string, mutate func(*APIKey)) *APIKey {
	t.Helper()
	k := &APIKey{
		ID:             NewID(),
		KeyAlias:       "the-key",
		UserID:         "u1",
		TeamID:         "t1",
		Models:         []string{"small", "medium"},
		AllowedRoutes:  []string{"/v1/chat/completions"},
		MaxBudgetNano:  ptrOf(int64(500_000)),
		SoftBudgetNano: ptrOf(int64(400_000)),
		BudgetPeriod:   "monthly",
		SpendNano:      123_456,
		RPMLimit:       ptrOf(int64(60)),
		TPMLimit:       ptrOf(int64(9_000)),
		MaxParallel:    ptrOf(int64(4)),
		PriorityClass:  "interactive",
		Tier:           "commercial",
		Tags:           []string{"team-a"},
		ExpiresAt:      TimeAt(Micros(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))),
	}
	if mutate != nil {
		mutate(k)
	}
	if err := s.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAPIKey(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	return k
}

func ptrOf[T any](v T) *T { return &v }

func verifierFor(t *testing.T, s *Store, token string) KeySecret {
	t.Helper()
	hash, err := HashDorangV1(s.cfg.Pepper, token)
	if err != nil {
		t.Fatal(err)
	}
	return KeySecret{
		Lookup: KeyLookup(token), TokenHash: hash,
		HashScheme: SchemeDorangV1, KeyLabel: LabelFor(token),
	}
}

func TestRotationPreservesEveryIDScopedField(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		old := "sk-rot-preserve-1" // pragma: allowlist secret — test fixture
		neu := "sk-rot-preserve-2" // pragma: allowlist secret — test fixture
		k := newRotationKey(t, s, old, nil)
		before, err := s.GetAPIKey(ctx, k.ID)
		if err != nil {
			t.Fatal(err)
		}

		rot, err := s.RotateKey(ctx, k.ID, verifierFor(t, s, neu), RotationPolicy{Grace: 24 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		after, err := s.GetAPIKey(ctx, k.ID)
		if err != nil {
			t.Fatal(err)
		}

		if after.ID != before.ID {
			t.Fatal("the key id changed; the identity is supposed to be the durable half")
		}
		// A rotation that also reset the limits would be a re-provisioning, and
		// an operator facing that puts it off. Every one of these is a field an
		// operator would have had to re-enter.
		for _, tc := range []struct {
			name       string
			got, want  any
			comparable bool
		}{
			{name: "tier", got: after.Tier, want: before.Tier, comparable: true},
			{name: "spend", got: after.SpendNano, want: before.SpendNano, comparable: true},
			{name: "budget_period", got: after.BudgetPeriod, want: before.BudgetPeriod, comparable: true},
			{name: "priority_class", got: after.PriorityClass, want: before.PriorityClass, comparable: true},
			{name: "user", got: after.UserID, want: before.UserID, comparable: true},
			{name: "team", got: after.TeamID, want: before.TeamID, comparable: true},
			{name: "alias", got: after.KeyAlias, want: before.KeyAlias, comparable: true},
			{name: "blocked", got: after.Blocked, want: before.Blocked, comparable: true},
			{name: "expires_at", got: after.ExpiresAt, want: before.ExpiresAt, comparable: true},
			{name: "created_at", got: after.CreatedAt, want: before.CreatedAt, comparable: true},
		} {
			if tc.comparable && tc.got != tc.want {
				t.Errorf("rotation changed %s: %v -> %v", tc.name, tc.want, tc.got)
			}
		}
		wantInts(t, "max_budget", after.MaxBudgetNano, before.MaxBudgetNano)
		wantInts(t, "soft_budget", after.SoftBudgetNano, before.SoftBudgetNano)
		wantInts(t, "rpm", after.RPMLimit, before.RPMLimit)
		wantInts(t, "tpm", after.TPMLimit, before.TPMLimit)
		wantInts(t, "max_parallel", after.MaxParallel, before.MaxParallel)
		wantStrings(t, "models", after.Models, before.Models)
		wantStrings(t, "allowed_routes", after.AllowedRoutes, before.AllowedRoutes)
		wantStrings(t, "tags", after.Tags, before.Tags)

		// And the verifier DID change, or nothing was rotated.
		if after.Lookup == before.Lookup || after.TokenHash == before.TokenHash {
			t.Fatal("the verifier did not change; nothing was rotated")
		}
		if rot.New.Generation != 2 || rot.Previous.Generation != 1 {
			t.Errorf("generations are %d/%d, want 2 replacing 1",
				rot.New.Generation, rot.Previous.Generation)
		}
	})
}

func TestBothSecretsAuthenticateDuringGraceAndTheLedgerRecordsWhich(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		old := "sk-rot-grace-1" // pragma: allowlist secret — test fixture
		neu := "sk-rot-grace-2" // pragma: allowlist secret — test fixture
		k := newRotationKey(t, s, old, nil)

		rot, err := s.RotateKey(ctx, k.ID, verifierFor(t, s, neu), RotationPolicy{Grace: 24 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if rot.PreviousExpiresAt.IsZero() {
			t.Fatal("the rotation did not report when the old secret expires; " +
				"a caller cannot infer that from a policy plus a clock")
		}

		// Both authenticate, to the SAME principal, with DIFFERENT secret ids.
		seen := map[string]string{}
		for _, tok := range []string{old, neu} {
			got, err := s.AuthenticateKey(ctx, tok)
			if err != nil {
				t.Fatalf("%s did not authenticate during the grace period: %v", tok, err)
			}
			if got.Key.ID != k.ID {
				t.Errorf("%s authenticated to key %s, want %s", tok, got.Key.ID, k.ID)
			}
			if got.Secret == nil {
				t.Fatalf("%s authenticated without reporting which secret verified", tok)
			}
			seen[tok] = got.Secret.ID
		}
		if seen[old] == seen[neu] {
			t.Fatal("both secrets report the same id; an operator cannot tell whether the client rolled")
		}

		// Ledger continuity: rows written before and after the rotation belong
		// to the same key and name different secrets.
		rows := []RequestLog{
			{TS: s.now().Add(-time.Hour), APIKeyID: k.ID, SecretID: seen[old], ModelGroup: "m", TotalTokens: 10},
			{TS: s.now(), APIKeyID: k.ID, SecretID: seen[neu], ModelGroup: "m", TotalTokens: 20},
		}
		if err := s.InsertRequestLogs(ctx, rows); err != nil {
			t.Fatal(err)
		}
		page, err := s.ListRequestsByKey(ctx, k.ID,
			TimeRange{Start: s.now().Add(-2 * time.Hour), End: s.now().Add(time.Hour)}, Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Rows) != 2 {
			t.Fatalf("the ledger holds %d rows for the key, want both sides of the rotation", len(page.Rows))
		}
		got := map[string]bool{}
		for _, r := range page.Rows {
			if r.APIKeyID != k.ID {
				t.Errorf("a ledger row moved to key %s", r.APIKeyID)
			}
			got[r.SecretID] = true
		}
		for tok, id := range seen {
			if !got[id] {
				t.Errorf("the ledger does not record that %s was used (secret %s)", tok, id)
			}
		}
	})
}

func TestTheGracePeriodEndsOnItsOwn(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		clk := newClock(s.now())
		s.cfg.Now = clk.Now

		old := "sk-rot-lapse-1" // pragma: allowlist secret — test fixture
		neu := "sk-rot-lapse-2" // pragma: allowlist secret — test fixture
		k := newRotationKey(t, s, old, nil)
		if _, err := s.RotateKey(ctx, k.ID, verifierFor(t, s, neu), RotationPolicy{Grace: time.Hour}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AuthenticateKey(ctx, old); err != nil {
			t.Fatalf("the old secret stopped working inside the grace period: %v", err)
		}
		clk.Add(time.Hour)
		if _, err := s.AuthenticateKey(ctx, old); !errors.Is(err, ErrSecretRetired) {
			t.Fatalf("the old secret was refused with %v after the grace period, want ErrSecretRetired", err)
		}
		if _, err := s.AuthenticateKey(ctx, neu); err != nil {
			t.Fatalf("the current secret stopped working when the grace period ended: %v", err)
		}
	})
}

func TestEndingTheGraceEarlyIsImmediate(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		old := "sk-rot-cut-1" // pragma: allowlist secret — test fixture
		neu := "sk-rot-cut-2" // pragma: allowlist secret — test fixture
		k := newRotationKey(t, s, old, nil)
		if _, err := s.RotateKey(ctx, k.ID, verifierFor(t, s, neu),
			RotationPolicy{Grace: 30 * 24 * time.Hour}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AuthenticateKey(ctx, old); err != nil {
			t.Fatal(err)
		}

		// This is what a suspected compromise needs: rotate now, cut the old
		// secret immediately, keep everything else. No clock is advanced.
		lookups, err := s.EndGrace(ctx, k.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(lookups) != 1 || lookups[0] != KeyLookup(old) {
			t.Fatalf("the cut reported %v, want exactly the old secret's index key", lookups)
		}
		if _, err := s.AuthenticateKey(ctx, old); !errors.Is(err, ErrSecretRetired) {
			t.Fatalf("the cut secret still authenticates: %v", err)
		}
		// "keep everything else": the caller who already rolled is not locked
		// out by the control that protects them.
		if _, err := s.AuthenticateKey(ctx, neu); err != nil {
			t.Fatalf("the current secret was cut too: %v", err)
		}
		got, err := s.GetAPIKey(ctx, k.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.SpendNano == 0 || got.Tier != "commercial" {
			t.Error("the early cut reset id-scoped state")
		}
	})
}

func TestMaxSecretsCutsTheOldestRatherThanRefusingTheRotation(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		toks := []string{
			"sk-rot-max-1", // pragma: allowlist secret — test fixture
			"sk-rot-max-2", // pragma: allowlist secret — test fixture
			"sk-rot-max-3", // pragma: allowlist secret — test fixture
		}
		k := newRotationKey(t, s, toks[0], nil)
		p := RotationPolicy{Grace: 30 * 24 * time.Hour, MaxSecrets: 2}

		if _, err := s.RotateKey(ctx, k.ID, verifierFor(t, s, toks[1]), p); err != nil {
			t.Fatal(err)
		}
		// A second rotation in a hurry is exactly what a suspected compromise
		// looks like. It must not be refused by a bookkeeping limit.
		rot, err := s.RotateKey(ctx, k.ID, verifierFor(t, s, toks[2]), p)
		if err != nil {
			t.Fatalf("a second rotation was refused: %v", err)
		}
		if len(rot.Retired) != 1 || rot.Retired[0].Generation != 1 {
			t.Fatalf("retired %+v, want the oldest generation", rot.Retired)
		}
		if _, err := s.AuthenticateKey(ctx, toks[0]); !errors.Is(err, ErrSecretRetired) {
			t.Errorf("generation 1 still authenticates with max_secrets 2: %v", err)
		}
		for _, tok := range toks[1:] {
			if _, err := s.AuthenticateKey(ctx, tok); err != nil {
				t.Errorf("%s should still authenticate: %v", tok, err)
			}
		}
	})
}

func TestPendIsReversibleInOneActionAndStopsTheKey(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		tok := "sk-pend-store" // pragma: allowlist secret — test fixture
		k := newRotationKey(t, s, tok, nil)

		if _, err := s.PendKey(ctx, k.ID, "token guard: 10x"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AuthenticateKey(ctx, tok); !errors.Is(err, ErrKeyPended) {
			t.Fatalf("a pended key authenticated with %v", err)
		}
		got, err := s.GetAPIKey(ctx, k.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Pended() || got.PendReason == "" {
			t.Error("the pend recorded neither an instant nor a reason")
		}
		if got.Blocked {
			t.Error("a pend set the blocked flag; the reversible control must stay distinguishable")
		}

		// ONE action.
		if _, err := s.ReleaseKey(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AuthenticateKey(ctx, tok); err != nil {
			t.Fatalf("a released key did not resume: %v", err)
		}
		// The caller kept the credential it already had. That is the whole
		// difference between a pend and a revocation.
		if after, _ := s.GetAPIKey(ctx, k.ID); after.Lookup != k.Lookup {
			t.Error("the release changed the key's secret")
		}
	})
}

func TestPendingIsIdempotentAndKeepsTheOriginalInstant(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		clk := newClock(s.now())
		s.cfg.Now = clk.Now
		tok := "sk-pend-twice" // pragma: allowlist secret — test fixture
		k := newRotationKey(t, s, tok, nil)

		if _, err := s.PendKey(ctx, k.ID, "first"); err != nil {
			t.Fatal(err)
		}
		first, _ := s.GetAPIKey(ctx, k.ID)
		clk.Add(time.Hour)
		if _, err := s.PendKey(ctx, k.ID, "second"); err != nil {
			t.Fatal(err)
		}
		second, _ := s.GetAPIKey(ctx, k.ID)
		if !second.PendedAt.Equal(first.PendedAt) {
			t.Error("a repeated pend moved the instant; 'when was this pended' has to survive a second alert")
		}
	})
}

func TestMaxAgeWarnsAndDoesNotExecute(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		clk := newClock(s.now())
		s.cfg.Now = clk.Now
		tok := "sk-max-age" // pragma: allowlist secret — test fixture
		k := newRotationKey(t, s, tok, nil)

		if overdue, err := s.KeysOverdueForRotation(ctx, 90*24*time.Hour); err != nil || len(overdue) != 0 {
			t.Fatalf("a fresh key is overdue: %v %v", overdue, err)
		}
		clk.Add(100 * 24 * time.Hour)
		overdue, err := s.KeysOverdueForRotation(ctx, 90*24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := overdue[k.ID]; !ok {
			t.Fatal("a 100-day-old secret is not reported against a 90-day policy")
		}
		// And nothing happened to it. max_age does not silently break a working
		// integration on a timer.
		if _, err := s.AuthenticateKey(ctx, tok); err != nil {
			t.Fatalf("an overdue key stopped working: %v", err)
		}
	})
}

func TestEveryExistingKeyGetsAGenerationOneSecret(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		tok := "sk-gen-one" // pragma: allowlist secret — test fixture
		k := newRotationKey(t, s, tok, nil)
		secs, err := s.ListKeySecrets(ctx, k.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(secs) != 1 || secs[0].Generation != 1 || !secs[0].Current {
			t.Fatalf("a newly issued key has secrets %+v, want one current generation 1", secs)
		}
		if secs[0].Lookup != k.Lookup {
			t.Error("the secrets table disagrees with the key's denormalized copy")
		}
	})
}

func TestAuthenticationIsStillOneStatementAfterARotation(t *testing.T) {
	// §2.4's rule survives §11.2c's shape: one lookup selects the row, and only
	// verification branches. A key with two live secrets must not cost two
	// round trips.
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		old := "sk-one-stmt-1" // pragma: allowlist secret — test fixture
		neu := "sk-one-stmt-2" // pragma: allowlist secret — test fixture
		k := newRotationKey(t, s, old, nil)
		if _, err := s.RotateKey(ctx, k.ID, verifierFor(t, s, neu), RotationPolicy{Grace: time.Hour}); err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{old, neu} {
			before := s.StatementCount()
			if _, err := s.AuthenticateKey(ctx, tok); err != nil {
				t.Fatal(err)
			}
			if n := s.StatementCount() - before; n != 1 {
				t.Errorf("authenticating %s issued %d statements, want exactly 1", tok, n)
			}
		}
	})
}

func TestInvalidationBusCarriesMessagesInOrderAndPrunes(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		clk := newClock(s.now())
		s.cfg.Now = clk.Now

		for i, cause := range []string{"revoked", "pended", "grace_cut"} {
			if _, err := s.PublishInvalidation(ctx, KeyInvalidation{
				KeyID: "k" + string(rune('1'+i)), Lookups: []string{"aa", "bb"}, Cause: cause,
			}); err != nil {
				t.Fatal(err)
			}
		}
		msgs, err := s.InvalidationsSince(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 {
			t.Fatalf("read %d messages, want 3", len(msgs))
		}
		for i := 1; i < len(msgs); i++ {
			if msgs[i].Seq <= msgs[i-1].Seq {
				t.Fatal("the sequence is not monotonic; a watermark over it would skip messages")
			}
		}
		if len(msgs[0].Lookups) != 2 {
			t.Error("the index keys did not survive the round trip")
		}
		head, err := s.LatestInvalidationSeq(ctx)
		if err != nil || head != msgs[len(msgs)-1].Seq {
			t.Fatalf("head = %d/%v, want %d", head, err, msgs[len(msgs)-1].Seq)
		}
		// A watermark reads only what follows it.
		rest, err := s.InvalidationsSince(ctx, msgs[0].Seq, 0)
		if err != nil || len(rest) != 2 {
			t.Fatalf("a watermarked read returned %d messages", len(rest))
		}

		clk.Add(2 * time.Hour)
		n, err := s.PruneInvalidations(ctx, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Errorf("pruned %d rows, want 3", n)
		}
		// Pruning must not move the sequence backwards: a node whose watermark
		// is past the pruned rows must not re-read them, and a node behind them
		// reloads (rule 4) rather than replaying.
		if _, err := s.LatestInvalidationSeq(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func wantInts(t *testing.T, name string, got, want *int64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("rotation changed %s: %v -> %v", name, want, got)
	case *got != *want:
		t.Errorf("rotation changed %s: %d -> %d", name, *want, *got)
	}
}

func wantStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("rotation changed %s: %v -> %v", name, want, got)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("rotation changed %s: %v -> %v", name, want, got)
			return
		}
	}
}
