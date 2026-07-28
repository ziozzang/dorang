package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StoredResponse is one row of DESIGN §9.2's `responses_store`.
//
// The table is not optional [R1-C7]. The Responses API carries server-side
// conversation state — `store: true` plus `previous_response_id` — and a
// gateway that promises that endpoint without somewhere to keep it has only two
// runtime options, both of which break the promise: reject the request, or
// ignore the reference and answer from a truncated conversation.
type StoredResponse struct {
	ID        string
	CreatedAt time.Time
	// ExpiresAt is when the sweep may delete the row. Entries expire; retention
	// is configurable per tier (§9.2).
	ExpiresAt time.Time
	// OwnerKeyID is the credential that created the response. It is the
	// authorization key for every read: one caller's conversation must not be
	// resolvable by another's credential, and a lookup that ignored it would
	// make previous_response_id a cross-tenant read primitive.
	OwnerKeyID         string
	PreviousResponseID string
	ModelGroup         string
	// Items is the stored exchange as JSON, verbatim. It is the answer bytes
	// rather than a re-encoding: a retrieved response has to be the response
	// that was served, not dorang's second rendering of it.
	Items []byte
	// ReasoningBlobs holds the opaque integrity-bearing reasoning handles of
	// §10.2 as raw bytes, so they can be replayed byte-identically.
	ReasoningBlobs []byte
}

const storedResponseColumns = `response_id, created_at, expires_at, owner_key_id,
	previous_response_id, model_group, items, reasoning_blobs`

// PutStoredResponse writes or replaces a stored response.
//
// It is an upsert rather than an insert because the same response id is written
// exactly once per request and a retry of the same logical request must not
// fail on a primary-key collision it caused itself.
func (s *Store) PutStoredResponse(ctx context.Context, r *StoredResponse) error {
	if r == nil || r.ID == "" {
		return errors.New("store: a stored response needs an id")
	}
	args := []any{
		r.ID, Micros(r.CreatedAt), Micros(r.ExpiresAt), nullStr(r.OwnerKeyID),
		nullStr(r.PreviousResponseID), r.ModelGroup, string(orJSONList(r.Items)),
		r.ReasoningBlobs,
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := s.txExec(ctx, tx, `UPDATE responses_store SET
				created_at = ?, expires_at = ?, owner_key_id = ?,
				previous_response_id = ?, model_group = ?, items = ?, reasoning_blobs = ?
			 WHERE response_id = ?`,
			append(append([]any{}, args[1:]...), r.ID)...)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n > 0 {
			return nil
		}
		_, err = s.txExec(ctx, tx, `INSERT INTO responses_store (`+storedResponseColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, args...)
		return err
	})
}

// GetStoredResponse returns one stored response, or [ErrNotFound].
//
// ownerKeyID is part of the lookup, not a check applied afterwards: a row that
// belongs to another credential must be indistinguishable from a row that does
// not exist, or the endpoint becomes an oracle for other callers' response ids.
// An empty ownerKeyID matches any owner and exists for the unauthenticated
// single-tenant case only.
func (s *Store) GetStoredResponse(ctx context.Context, id, ownerKeyID string) (*StoredResponse, error) {
	q := `SELECT ` + storedResponseColumns + ` FROM responses_store WHERE response_id = ?`
	args := []any{id}
	if ownerKeyID != "" {
		q += ` AND owner_key_id = ?`
		args = append(args, ownerKeyID)
	}
	return scanStoredResponse(s.queryRow(ctx, q, args...))
}

// DeleteStoredResponse removes one, reporting whether it existed.
func (s *Store) DeleteStoredResponse(ctx context.Context, id, ownerKeyID string) (bool, error) {
	q := `DELETE FROM responses_store WHERE response_id = ?`
	args := []any{id}
	if ownerKeyID != "" {
		q += ` AND owner_key_id = ?`
		args = append(args, ownerKeyID)
	}
	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func scanStoredResponse(row *sql.Row) (*StoredResponse, error) {
	var (
		r        StoredResponse
		created  int64
		expires  int64
		owner    sql.NullString
		previous sql.NullString
		items    string
	)
	err := row.Scan(&r.ID, &created, &expires, &owner, &previous, &r.ModelGroup,
		&items, &r.ReasoningBlobs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: response %s", ErrNotFound, r.ID)
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = TimeAt(created)
	r.ExpiresAt = TimeAt(expires)
	r.OwnerKeyID = str(owner)
	r.PreviousResponseID = str(previous)
	r.Items = []byte(items)
	return &r, nil
}
