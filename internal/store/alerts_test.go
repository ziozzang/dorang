package store

import (
	"context"
	"testing"
	"time"
)

// TestAlertStateAnnouncesEachCrossingOnce walks one condition through its life:
// the first observation announces, a second (another node, a restart) does not,
// the reminder is due after remind, recovery re-arms it, and every transport's
// delivery is recorded so only the missing one is retried.
func TestAlertStateAnnouncesEachCrossingOnce(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		t0 := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
		const key = "remaining_low|vendor|openrouter|openrouter-1|credits"
		remind := 6 * time.Hour
		raise := func(at time.Time) (bool, int64) {
			t.Helper()
			ok, seq, err := s.AlertRaise(ctx, key, "remaining_low", at, at.Add(-remind), "smtp,http", `{"x":1}`)
			if err != nil {
				t.Fatal(err)
			}
			return ok, seq
		}

		if ok, seq := raise(t0); !ok || seq != 1 {
			t.Fatalf("first observation: announce=%v seq=%d, want true 1", ok, seq)
		}
		if ok, _ := raise(t0.Add(time.Minute)); ok {
			t.Fatal("a second observation of the same crossing announced again")
		}

		// One transport delivered; the other is still owed and is retried.
		if err := s.AlertDelivered(ctx, key, 1, "http", t0); err != nil {
			t.Fatal(err)
		}
		pend, err := s.AlertsPending(ctx, t0.Add(time.Minute), 10)
		if err != nil || len(pend) != 1 || len(pend[0].Pending) != 1 || pend[0].Pending[0] != "smtp" {
			t.Fatalf("pending = %+v, %v; want smtp alone", pend, err)
		}
		if owed, ok, _ := s.AlertRetryClaim(ctx, key, 1, pend[0].LastSent, t0.Add(2*time.Minute)); !ok ||
			len(owed) != 1 || owed[0] != "smtp" {
			t.Fatalf("the retry was not claimed for smtp: %v %v", owed, ok)
		}
		if _, ok, _ := s.AlertRetryClaim(ctx, key, 1, pend[0].LastSent, t0.Add(2*time.Minute)); ok {
			t.Fatal("the same retry was claimed twice")
		}
		if act, err := s.AlertsActive(ctx, 10); err != nil || len(act) != 1 || act[0].Key != key {
			t.Fatalf("active = %+v, %v", act, err)
		}
		_ = s.AlertDelivered(ctx, key, 1, "smtp", t0)
		if pend, _ := s.AlertsPending(ctx, t0.Add(time.Hour), 10); len(pend) != 0 {
			t.Fatalf("delivered everywhere and still pending: %+v", pend)
		}
		// A listing taken before the last delivery cannot be claimed into a duplicate.
		if _, ok, _ := s.AlertRetryClaim(ctx, key, 1, t0.Add(2*time.Minute), t0.Add(3*time.Minute)); ok {
			t.Fatal("a retry was claimed with nothing owed")
		}

		// Still low past remind: one reminder.
		if ok, seq := raise(t0.Add(remind + 3*time.Minute)); !ok || seq != 2 {
			t.Fatalf("reminder: announce=%v seq=%d, want true 2", ok, seq)
		}
		// A delivery of a superseded announcement changes nothing.
		_ = s.AlertDelivered(ctx, key, 1, "smtp", t0)
		if pend, _ := s.AlertsPending(ctx, t0.Add(24*time.Hour), 10); len(pend) != 1 || len(pend[0].Pending) != 2 {
			t.Fatalf("pending after reminder = %+v", pend)
		}

		// Recovery re-arms: the next crossing is news at once.
		if was, _ := s.AlertClear(ctx, key, t0.Add(7*time.Hour)); !was {
			t.Fatal("clearing a low condition reported it was not low")
		}
		if was, _ := s.AlertClear(ctx, key, t0.Add(7*time.Hour)); was {
			t.Fatal("clearing twice reported a second recovery")
		}
		if ok, seq := raise(t0.Add(7*time.Hour + time.Minute)); !ok || seq != 3 {
			t.Fatalf("after recovery: announce=%v seq=%d, want true 3", ok, seq)
		}
	})
}
