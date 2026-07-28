package prefix

import "testing"

// Two tenants sending byte-identical bodies to the same model group produce
// different prefix digests.
//
// DESIGN §7.4b specified h₀ = H(group_id), and on a shared gateway that is not
// enough. A prefix table keyed on the group alone is a byte-exact confirmation
// oracle over other tenants' prompt prefixes — send candidate bytes, read
// prefix_hit:depth=N off your own routing header, learn that somebody else
// recently sent exactly those bytes — and a routing-poisoning primitive,
// because prefix affinity outranks cost in the default strategy chain, so
// planting an entry steers a victim's next request onto a deployment of the
// attacker's choosing.
//
// §7.4a already required the tenant to lead its key. The two now share the rule.
func TestChainIsTenantScoped(t *testing.T) {
	body := []byte("a long shared system prompt, byte for byte identical")

	a := Compute("team:alice", "gpt-4o", body, 16)
	b := Compute("team:bob", "gpt-4o", body, 16)

	if len(a) == 0 || len(b) == 0 {
		t.Fatal("no digests were produced")
	}
	if len(a) != len(b) {
		t.Fatalf("digest counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] == b[i] {
			t.Fatalf("tenants alice and bob share prefix digest %d for identical bytes: "+
				"one tenant's cache entry answers another tenant's lookup", i)
		}
	}
}

// The same tenant and group still match, or affinity would do nothing.
func TestChainMatchesWithinOneTenant(t *testing.T) {
	body := []byte("a long shared system prompt, byte for byte identical")
	a := Compute("team:alice", "gpt-4o", body, 16)
	b := Compute("team:alice", "gpt-4o", body, 16)
	if len(a) != len(b) {
		t.Fatalf("digest counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("one tenant's own identical request diverged at digest %d", i)
		}
	}
}

// The group still separates, so the tenant component did not replace it.
func TestChainStillSeparatesGroups(t *testing.T) {
	body := []byte("identical bytes")
	a := Compute("team:alice", "gpt-4o", body, 16)
	b := Compute("team:alice", "claude-opus-4", body, 16)
	if a[0] == b[0] {
		t.Fatal("two model groups share a prefix entry")
	}
}

// The separator prevents the tenant and group from running together: a tenant
// "ab" with group "c" must not seed like tenant "a" with group "bc".
func TestTenantAndGroupCannotBeConfused(t *testing.T) {
	body := []byte("identical bytes")
	a := Compute("ab", "c", body, 16)
	b := Compute("a", "bc", body, 16)
	if a[0] == b[0] {
		t.Fatal("the tenant and group components run together, so one tenant can " +
			"impersonate another by choosing a group name")
	}
}
