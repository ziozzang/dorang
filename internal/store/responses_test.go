package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The Responses API's server-side state (DESIGN §9.2 [R1-C7]).
//
// Two properties are load-bearing and both are asserted here rather than
// commented: a stored exchange round-trips byte for byte, and it is scoped to
// the credential that created it.

func newStoredResponse(id, owner string) *StoredResponse {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	return &StoredResponse{
		ID:                 id,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
		OwnerKeyID:         owner,
		PreviousResponseID: "",
		ModelGroup:         "qwen3.5:397b",
		Items:              []byte(`{"input":[{"type":"message","role":"user"}]}`),
		ReasoningBlobs:     []byte(`["sig-a","sig-b"]`),
	}
}

func TestStoredResponseRoundTrip(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		want := newStoredResponse("resp_a", "key-1")
		if err := s.PutStoredResponse(ctx, want); err != nil {
			t.Fatalf("PutStoredResponse: %v", err)
		}
		got, err := s.GetStoredResponse(ctx, "resp_a", "key-1")
		if err != nil {
			t.Fatalf("GetStoredResponse: %v", err)
		}
		if string(got.Items) != string(want.Items) {
			t.Errorf("items\n got %s\nwant %s", got.Items, want.Items)
		}
		if string(got.ReasoningBlobs) != string(want.ReasoningBlobs) {
			t.Errorf("reasoning blobs\n got %s\nwant %s", got.ReasoningBlobs, want.ReasoningBlobs)
		}
		if got.ModelGroup != want.ModelGroup {
			// The model group is an opaque name with a ':' in it (DESIGN §2.1).
			t.Errorf("model group %q, want %q", got.ModelGroup, want.ModelGroup)
		}
		if !got.CreatedAt.Equal(want.CreatedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
			t.Errorf("timestamps %v/%v, want %v/%v",
				got.CreatedAt, got.ExpiresAt, want.CreatedAt, want.ExpiresAt)
		}
	})
}

// TestStoredResponseIsScopedToItsOwner is the security property of the endpoint.
//
// `previous_response_id` is a client-supplied identifier for server-side state.
// If another credential's row were readable, the endpoint would be a
// cross-tenant read primitive, and a 403 rather than a 404 would still leave it
// an existence oracle. Both reads answer the same way: not found.
func TestStoredResponseIsScopedToItsOwner(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		if err := s.PutStoredResponse(ctx, newStoredResponse("resp_a", "key-1")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetStoredResponse(ctx, "resp_a", "key-2"); !errors.Is(err, ErrNotFound) {
			t.Errorf("another key read the row: %v, want ErrNotFound", err)
		}
		if _, err := s.GetStoredResponse(ctx, "resp_missing", "key-1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a missing row: %v, want ErrNotFound", err)
		}
		ok, err := s.DeleteStoredResponse(ctx, "resp_a", "key-2")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("another key deleted the row")
		}
		ok, err = s.DeleteStoredResponse(ctx, "resp_a", "key-1")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("the owner could not delete its own row")
		}
		if _, err := s.GetStoredResponse(ctx, "resp_a", "key-1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("the row survived its delete: %v", err)
		}
	})
}

// TestStoredResponseUpsert asserts that writing the same id twice replaces
// rather than failing. One logical request writes its id once; a retry of that
// request must not collide with itself.
func TestStoredResponseUpsert(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		r := newStoredResponse("resp_a", "key-1")
		if err := s.PutStoredResponse(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.Items = []byte(`{"input":[],"response":{"id":"resp_a"}}`)
		if err := s.PutStoredResponse(ctx, r); err != nil {
			t.Fatalf("second write: %v", err)
		}
		got, err := s.GetStoredResponse(ctx, "resp_a", "key-1")
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Items) != string(r.Items) {
			t.Errorf("items %s, want %s", got.Items, r.Items)
		}
	})
}

// TestStoredResponseNeedsAnID refuses the one write that cannot be read back.
func TestStoredResponseNeedsAnID(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		if err := s.PutStoredResponse(context.Background(), &StoredResponse{}); err == nil {
			t.Error("an id-less response was stored")
		}
	})
}
