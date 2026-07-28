package cluster

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// TestGuardRefusesClusteredLocal is risk W1, closed. DESIGN 5.6: revision 1
// called this a recommendation, and it is not. Silently exceeding a provider's
// plan limit produces upstream 429s, which cascade into the fallback chain and
// consume the capacity of unrelated models in the same class -- so the failure
// surfaces far from its cause and the refusal has to be at start-up.
func TestGuardRefusesClusteredLocal(t *testing.T) {
	if err := Guard(true, ModeLocal); !errors.Is(err, ErrLocalInCluster) {
		t.Fatalf("Guard(clustered, local) = %v, want ErrLocalInCluster", err)
	}
	// Every other pairing is allowed, so the guard is a guard and not a ban.
	if err := Guard(false, ModeLocal); err != nil {
		t.Fatalf("Guard(single node, local) = %v, want nil", err)
	}
	for _, m := range []Mode{ModeSharedPG, ModeSharedRedis, ModeLeased} {
		if err := Guard(true, m); err != nil {
			t.Fatalf("Guard(clustered, %s) = %v, want nil", m, err)
		}
	}
}

// TestEveryEntryPointRefusesClusteredLocal walks the ways into this package. A
// guard with one entry point is a guard that a second entry point walks around,
// which is exactly how a "validated at load" check stops protecting anything
// once a second construction path appears.
func TestEveryEntryPointRefusesClusteredLocal(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		t.Run("New", func(t *testing.T) {
			_, err := New(Config{Enabled: true, Mode: ModeLocal, Store: s, Now: clk.Now})
			if !errors.Is(err, ErrLocalInCluster) {
				t.Fatalf("New = %v, want ErrLocalInCluster", err)
			}
		})

		t.Run("Publish", func(t *testing.T) {
			_, err := Publish(ModeLocal, Params{Limit: 100, Nodes: 4, Clustered: true})
			if !errors.Is(err, ErrLocalInCluster) {
				t.Fatalf("Publish = %v, want ErrLocalInCluster", err)
			}
		})

		t.Run("PublishAll", func(t *testing.T) {
			ok, refused := PublishAll(Params{Limit: 100, Nodes: 4, Clustered: true})
			for _, a := range ok {
				if a.Mode == ModeLocal {
					t.Fatal("PublishAll offered local as usable in a cluster")
				}
			}
			if !errors.Is(refused[ModeLocal], ErrLocalInCluster) {
				t.Fatalf("PublishAll refused local with %v", refused[ModeLocal])
			}
		})

		t.Run("Coordinator cannot ask for local", func(t *testing.T) {
			n := testNode(t, s, "node-a", clk, func(c *Config) { c.Mode = ModeLeased })
			// A caller hands over a config that says local. The node must use
			// its own mode and ignore the request entirely.
			c, err := n.Coordinator(quota.CoordinatorConfig{Mode: ModeLocal, Clustered: false})
			if err != nil {
				t.Fatalf("Coordinator: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			if c.Mode() != ModeLeased {
				t.Fatalf("the coordinator came back in %s mode after the caller asked for local", c.Mode())
			}
		})

		t.Run("quota refuses it too", func(t *testing.T) {
			// The same refusal exists one layer down. Two independent guards is
			// the point, not duplication: they are reached by different paths.
			_, err := quota.NewCoordinator(quota.CoordinatorConfig{Mode: quota.ModeLocal, Clustered: true})
			if err == nil {
				t.Fatal("quota.NewCoordinator accepted clustered local")
			}
		})
	})
}

// TestNodeModeCannotBeChangedAtRuntime is the structural half of the guard.
// [New] refusing is worth nothing if the mode can be set to local afterwards,
// so the type is checked for the shape that would allow it.
func TestNodeModeCannotBeChangedAtRuntime(t *testing.T) {
	nt := reflect.TypeOf(&Node{})
	for i := 0; i < nt.NumMethod(); i++ {
		name := nt.Method(i).Name
		if strings.HasPrefix(name, "Set") {
			t.Fatalf("Node has a setter %q; the mode guard only holds while the mode is immutable", name)
		}
	}
	st := reflect.TypeOf(Node{})
	for i := 0; i < st.NumField(); i++ {
		if f := st.Field(i); f.IsExported() {
			t.Fatalf("Node.%s is exported and therefore assignable from outside this package", f.Name)
		}
	}
}

// TestModeVocabularyMatchesConfig pins this package's mode names to
// internal/config's. DESIGN 6.3: quota and concurrency use one accuracy
// vocabulary, not two -- and a vocabulary that drifts between the validator and
// the implementation is two vocabularies wearing one name.
func TestModeVocabularyMatchesConfig(t *testing.T) {
	cases := []struct {
		spelling string
		want     Mode
	}{
		{config.CapacityModeLocal, ModeLocal},
		{config.CapacityModeSharedRedis, ModeSharedRedis},
		{config.CapacityModeSharedPG, ModeSharedPG},
		{config.CapacityModeLeased, ModeLeased},
	}
	for _, tc := range cases {
		got, err := ParseMode(tc.spelling)
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", tc.spelling, err)
		}
		if got != tc.want {
			t.Fatalf("ParseMode(%q) = %s, want %s", tc.spelling, got, tc.want)
		}
		if got.String() != tc.spelling {
			t.Fatalf("%v renders as %q, but configuration spells it %q", got, got.String(), tc.spelling)
		}
	}
	if len(Modes()) != len(cases) {
		t.Fatalf("this package implements %d modes and configuration accepts %d", len(Modes()), len(cases))
	}
}

// TestConfigStillRefusesClusteredLocal checks the load-time half is intact, so
// that the two guards cannot both be removed by someone who found only one.
func TestConfigStillRefusesClusteredLocal(t *testing.T) {
	var c config.Config
	c.Version = 1
	c.Cluster = config.Cluster{Enabled: true, CapacityMode: config.CapacityModeLocal, MinLeasable: 16}
	err := c.Validate()
	if err == nil {
		t.Fatal("internal/config accepted cluster.enabled with capacity_mode local")
	}
	if !strings.Contains(err.Error(), "capacity_mode") {
		t.Fatalf("the refusal does not mention capacity_mode: %v", err)
	}
}

// TestClosedNodeRefusesWork checks that a drained node stays drained rather
// than quietly resuming leadership work after Close.
func TestClosedNodeRefusesWork(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		n := testNode(t, s, "node-a", clk, nil)
		tick(t, n)
		if !n.IsLeader() {
			t.Fatal("the only node did not become leader")
		}
		if err := n.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if n.IsLeader() {
			t.Fatal("a closed node still believes it leads")
		}
		if err := n.Tick(ctx); !errors.Is(err, ErrClosed) {
			t.Fatalf("Tick after Close = %v, want ErrClosed", err)
		}
		// Closing twice is the normal shape of a deferred shutdown.
		if err := n.Close(ctx); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})
}

// TestSharedRedisIsNotSilentlySharedPG is the second guard on the mode that
// this build cannot honour.
//
// The two shared modes are one coordinator over different stores, so the
// substitution is one line and produces something that works. That is exactly
// what makes it dangerous: the result reports Mode() == "shared-redis" and
// publishes "exact, one round trip to Redis per acquire" (DESIGN 5.6) while
// every acquire goes to the SQL lease table. An operator picks a mode by
// reading those figures, and picks a store size and a latency budget from the
// hot-path cost. internal/config refuses the configuration at load; this is the
// second entry point, because one guard is not a guard.
func TestSharedRedisIsNotSilentlySharedPG(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		n := testNode(t, s, "node-a", clk, func(c *Config) { c.Mode = ModeSharedRedis })

		c, err := n.Coordinator(quota.CoordinatorConfig{})
		if err == nil {
			_ = c.Close()
			t.Fatal("shared-redis built a coordinator with no Redis client; it is " +
				"coordinating through the store while claiming to use Redis")
		}
		if !errors.Is(err, ErrNoRedisClient) {
			t.Fatalf("Coordinator refused with %v, want ErrNoRedisClient", err)
		}

		// A caller that HAS a client is served. The refusal is about the
		// missing dependency, not about the mode being unimplemented — the
		// protocol ships, and MemRedis is a complete implementation of it.
		shared, err := NewRedisShared(NewMemRedis(clk.Now))
		if err != nil {
			t.Fatal(err)
		}
		c, err = n.Coordinator(quota.CoordinatorConfig{Shared: shared})
		if err != nil {
			t.Fatalf("Coordinator with a Redis client: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if c.Mode() != ModeSharedRedis {
			t.Fatalf("the coordinator came back in %s mode", c.Mode())
		}

		// And the units really went to Redis rather than to the lease table.
		// Mode() is a label; this is the thing the label is about.
		ctx := context.Background()
		key := quota.NewKey("credential", "c1", quota.Daily, quota.MetricRequests, clk.Now())
		if _, err := c.Charge(ctx, key, 64, 8, clk.Now()); err != nil {
			t.Fatal(err)
		}
		held, err := n.Leases().Held(ctx, key.String())
		if err != nil {
			t.Fatal(err)
		}
		if held != 0 {
			t.Errorf("a shared-redis charge put %d units in the SQL lease table", held)
		}
	})
}
