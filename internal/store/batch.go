package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Batch, BatchRow and File are the persisted shapes of DESIGN §9.2's `batches`,
// `batch_requests` and `files` tables.
//
// They deliberately mirror the tables rather than internal/batch's records. That
// package declares its own Store interface so that it imports nothing (DESIGN
// §1), and it must keep doing so: the translation between these rows and its
// records belongs to the wiring layer, which is the one place allowed to name
// both. What that costs is a struct per table; what it buys is a batch scheduler
// that is still testable without a database.
//
// Two columns are opaque here on purpose. Errors is raw JSON and Metadata is a
// string map: the store has no opinion about what a batch error looks like, and
// giving it one would put a batch vocabulary in the schema layer.
type Batch struct {
	ID          string
	OwnerKeyID  string
	PrincipalID string
	Endpoint    string

	InputFileID  string
	OutputFileID string
	ErrorFileID  string

	CompletionWindowMS int64
	Status             string

	Total     int64
	Completed int64
	Failed    int64

	// Errors is the batch's error list as JSON, or nil. It is what lets a
	// failed batch say WHY after a restart (§9.2).
	Errors   []byte
	Metadata map[string]string

	CreatedAt    time.Time
	UpdatedAt    time.Time
	InProgressAt time.Time
	FinalizingAt time.Time
	CompletedAt  time.Time
	FailedAt     time.Time
	CancellingAt time.Time
	CancelledAt  time.Time
	ExpiredAt    time.Time
	ExpiresAt    time.Time
}

// BatchRow is one row of a batch.
type BatchRow struct {
	BatchID  string
	CustomID string
	Seq      int64

	PrefixHash string
	ModelGroup string
	URL        string

	// InputOffset and InputLength locate the row inside the input blob, so the
	// scheduler seeks to it instead of holding a 200 MiB upload in memory for
	// the life of the batch (§9.2).
	InputOffset int64
	InputLength int64

	Status     string
	Attempts   int64
	StatusCode int64
	// RequestLogID joins the row to the ledger.
	RequestLogID string
	// ResponseBody is the row's answer, verbatim. The ledger stores no bodies,
	// so this is the only thing a resumed batch can rebuild its output file
	// from — and without it every finished row would be paid for twice (§9.2).
	ResponseBody []byte
	ErrorCode    string
	ErrorMessage string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// File is one uploaded or generated file's metadata. The bytes live in blob
// storage, not here.
type File struct {
	ID            string
	OwnerKeyID    string
	Purpose       string
	Filename      string
	Bytes         int64
	SHA256        string
	StorageRef    string
	Status        string
	StatusDetails string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// BatchPage selects a page of batches or files, newest first.
type BatchPage struct {
	// OwnerKeyID scopes the page to one principal. Empty lists everything,
	// which only an administrative caller may ask for.
	OwnerKeyID string
	// Purpose scopes a file listing. Ignored for batches.
	Purpose string
	// After is a cursor id: only records ordered after it are returned.
	After string
	// Limit is the page size. Zero uses the store's default.
	Limit int
}

func (p BatchPage) limit(def int) int {
	if p.Limit > 0 {
		return p.Limit
	}
	if def > 0 {
		return def
	}
	return DefaultPageSize
}

const batchColumns = `id, owner_key_id, principal_id, endpoint,
	input_file_id, output_file_id, error_file_id, completion_window_ms, status,
	total_count, completed_count, failed_count, errors, metadata,
	created_at, updated_at, in_progress_at, finalizing_at, completed_at,
	failed_at, cancelling_at, cancelled_at, expired_at, expires_at`

const batchRowColumns = `batch_id, custom_id, seq, prefix_hash, model_group, url,
	input_offset, input_length, status, attempts, status_code, request_log_id,
	response_body, error_code, error, created_at, updated_at`

const fileColumns = `id, owner_key_id, purpose, filename, bytes, sha256,
	storage_ref, status, status_details, created_at, expires_at`

// CreateBatch inserts a batch, failing with [ErrExists] if the id is taken.
//
// The existence check and the insert share a transaction rather than relying on
// the primary key to raise a driver-specific error, because "is this a duplicate
// key or a broken connection" is not a question a caller should answer by
// matching on message text.
func (s *Store) CreateBatch(ctx context.Context, b *Batch) error {
	if b.ID == "" {
		return errors.New("store: a batch needs an id")
	}
	meta, err := encodeMetadata(b.Metadata)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT 1 FROM batches WHERE id = ?`), b.ID).Scan(&one)
		if err == nil {
			return fmt.Errorf("%w: batch %s", ErrExists, b.ID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = s.txExec(ctx, tx, `INSERT INTO batches (`+batchColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			batchArgs(b, meta)...)
		return err
	})
}

// SaveBatch overwrites an existing batch, or returns [ErrNotFound].
func (s *Store) SaveBatch(ctx context.Context, b *Batch) error {
	meta, err := encodeMetadata(b.Metadata)
	if err != nil {
		return err
	}
	res, err := s.exec(ctx, `UPDATE batches SET
			owner_key_id = ?, principal_id = ?, endpoint = ?,
			input_file_id = ?, output_file_id = ?, error_file_id = ?,
			completion_window_ms = ?, status = ?,
			total_count = ?, completed_count = ?, failed_count = ?,
			errors = ?, metadata = ?,
			created_at = ?, updated_at = ?, in_progress_at = ?, finalizing_at = ?,
			completed_at = ?, failed_at = ?, cancelling_at = ?, cancelled_at = ?,
			expired_at = ?, expires_at = ?
		 WHERE id = ?`,
		append(batchArgs(b, meta)[1:], b.ID)...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: batch %s", ErrNotFound, b.ID)
	}
	return nil
}

func batchArgs(b *Batch, meta string) []any {
	return []any{
		b.ID, nullStr(b.OwnerKeyID), nullStr(b.PrincipalID), b.Endpoint,
		nullStr(b.InputFileID), nullStr(b.OutputFileID), nullStr(b.ErrorFileID),
		b.CompletionWindowMS, b.Status,
		b.Total, b.Completed, b.Failed, string(orJSONList(b.Errors)), meta,
		Micros(b.CreatedAt), Micros(b.UpdatedAt),
		nullMicros(b.InProgressAt), nullMicros(b.FinalizingAt), nullMicros(b.CompletedAt),
		nullMicros(b.FailedAt), nullMicros(b.CancellingAt), nullMicros(b.CancelledAt),
		nullMicros(b.ExpiredAt), nullMicros(b.ExpiresAt),
	}
}

// GetBatch returns one batch, or [ErrNotFound].
func (s *Store) GetBatch(ctx context.Context, id string) (*Batch, error) {
	return scanBatch(s.queryRow(ctx, `SELECT `+batchColumns+` FROM batches WHERE id = ?`, id))
}

// ListBatches returns a page of batches, newest first, and reports whether more
// exist after it.
//
// The order is (created_at DESC, id DESC) rather than created_at alone: two
// batches created in the same microsecond would otherwise page
// non-deterministically, which shows up as a row appearing on two pages or on
// none.
func (s *Store) ListBatches(ctx context.Context, p BatchPage) ([]*Batch, bool, error) {
	q := `SELECT ` + batchColumns + ` FROM batches`
	where, args := []string{}, []any{}
	if p.OwnerKeyID != "" {
		where = append(where, `owner_key_id = ?`)
		args = append(args, p.OwnerKeyID)
	}
	if p.After != "" {
		var at int64
		err := s.queryRow(ctx, `SELECT created_at FROM batches WHERE id = ?`, p.After).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			// An unknown cursor is an empty page, not the first page. Silently
			// restarting a listing is how a client loops forever.
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		where = append(where, `(created_at < ? OR (created_at = ? AND id < ?))`)
		args = append(args, at, at, p.After)
	}
	q += whereClause(where) + ` ORDER BY created_at DESC, id DESC LIMIT ?`
	limit := p.limit(s.cfg.DefaultPageSize)
	args = append(args, limit+1)

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]*Batch, 0, limit)
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// ActiveBatches returns every batch whose status is not one of terminal, oldest
// first. It is what recovery reads after a restart.
//
// The terminal set is a parameter rather than a constant here: which statuses
// end a batch is internal/batch's vocabulary, and duplicating it in the schema
// layer would create two definitions that can drift apart in one release.
func (s *Store) ActiveBatches(ctx context.Context, terminal []string) ([]*Batch, error) {
	q := `SELECT ` + batchColumns + ` FROM batches`
	args := make([]any, 0, len(terminal))
	if len(terminal) > 0 {
		q += ` WHERE status NOT IN (` + placeholders(len(terminal)) + `)`
		for _, t := range terminal {
			args = append(args, t)
		}
	}
	q += ` ORDER BY created_at ASC, id ASC`

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Batch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PutBatchRows inserts rows, leaving any that already exist untouched.
//
// That is what makes re-validating a batch after a crash safe: a row that
// already ran keeps its recorded result instead of being reset to queued and
// executed — and paid for — a second time. ON CONFLICT DO NOTHING is the same
// statement on both dialects.
func (s *Store) PutBatchRows(ctx context.Context, rows []*BatchRow) error {
	if len(rows) == 0 {
		return nil
	}
	const q = `INSERT INTO batch_requests (` + batchRowColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (batch_id, custom_id) DO NOTHING`
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := s.txExec(ctx, tx, q, batchRowArgs(r)...); err != nil {
				return err
			}
		}
		return nil
	})
}

// SaveBatchRow overwrites one row, or returns [ErrNotFound].
func (s *Store) SaveBatchRow(ctx context.Context, r *BatchRow) error {
	res, err := s.exec(ctx, `UPDATE batch_requests SET
			seq = ?, prefix_hash = ?, model_group = ?, url = ?,
			input_offset = ?, input_length = ?, status = ?, attempts = ?,
			status_code = ?, request_log_id = ?, response_body = ?,
			error_code = ?, error = ?, created_at = ?, updated_at = ?
		 WHERE batch_id = ? AND custom_id = ?`,
		append(batchRowArgs(r)[2:], r.BatchID, r.CustomID)...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: row %s of batch %s", ErrNotFound, r.CustomID, r.BatchID)
	}
	return nil
}

func batchRowArgs(r *BatchRow) []any {
	return []any{
		r.BatchID, r.CustomID, r.Seq, r.PrefixHash, r.ModelGroup, r.URL,
		r.InputOffset, r.InputLength, r.Status, r.Attempts, r.StatusCode,
		nullStr(r.RequestLogID), nullBytes(r.ResponseBody), r.ErrorCode,
		nullStr(r.ErrorMessage), Micros(r.CreatedAt), Micros(r.UpdatedAt),
	}
}

// EachBatchRow visits every row of a batch in seq order.
//
// It streams: a batch holds up to fifty thousand rows and their response
// bodies, and materializing all of them to write one output file would put the
// largest customer's batch in the memory profile of every process.
func (s *Store) EachBatchRow(ctx context.Context, batchID string, fn func(*BatchRow) error) error {
	rows, err := s.query(ctx, `SELECT `+batchRowColumns+
		` FROM batch_requests WHERE batch_id = ? ORDER BY seq ASC`, batchID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanBatchRow(rows)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// CreateFile inserts a file record, failing with [ErrExists] if the id is taken.
func (s *Store) CreateFile(ctx context.Context, f *File) error {
	if f.ID == "" {
		return errors.New("store: a file needs an id")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT 1 FROM files WHERE id = ?`), f.ID).Scan(&one)
		if err == nil {
			return fmt.Errorf("%w: file %s", ErrExists, f.ID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = s.txExec(ctx, tx, `INSERT INTO files (`+fileColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, nullStr(f.OwnerKeyID), f.Purpose, f.Filename, f.Bytes, f.SHA256,
			f.StorageRef, f.Status, f.StatusDetails, Micros(f.CreatedAt), nullMicros(f.ExpiresAt))
		return err
	})
}

// GetFile returns one file record, or [ErrNotFound].
func (s *Store) GetFile(ctx context.Context, id string) (*File, error) {
	return scanFile(s.queryRow(ctx, `SELECT `+fileColumns+` FROM files WHERE id = ?`, id))
}

// ListFiles returns a page of file records, newest first.
func (s *Store) ListFiles(ctx context.Context, p BatchPage) ([]*File, bool, error) {
	q := `SELECT ` + fileColumns + ` FROM files`
	where, args := []string{}, []any{}
	if p.OwnerKeyID != "" {
		where = append(where, `owner_key_id = ?`)
		args = append(args, p.OwnerKeyID)
	}
	if p.Purpose != "" {
		where = append(where, `purpose = ?`)
		args = append(args, p.Purpose)
	}
	if p.After != "" {
		var at int64
		err := s.queryRow(ctx, `SELECT created_at FROM files WHERE id = ?`, p.After).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		where = append(where, `(created_at < ? OR (created_at = ? AND id < ?))`)
		args = append(args, at, at, p.After)
	}
	q += whereClause(where) + ` ORDER BY created_at DESC, id DESC LIMIT ?`
	limit := p.limit(s.cfg.DefaultPageSize)
	args = append(args, limit+1)

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]*File, 0, limit)
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// DeleteFile removes a file record, or returns [ErrNotFound].
func (s *Store) DeleteFile(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `DELETE FROM files WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: file %s", ErrNotFound, id)
	}
	return nil
}

func scanBatch(row rowScanner) (*Batch, error) {
	var (
		b                                          Batch
		owner, principal, inFile, outFile, errFile sql.NullString
		errsJSON, metaJSON                         string
		inProg, finalizing, completed, failed      sql.NullInt64
		cancelling, cancelled, expired, expires    sql.NullInt64
		createdAt, updatedAt                       int64
	)
	err := row.Scan(&b.ID, &owner, &principal, &b.Endpoint,
		&inFile, &outFile, &errFile, &b.CompletionWindowMS, &b.Status,
		&b.Total, &b.Completed, &b.Failed, &errsJSON, &metaJSON,
		&createdAt, &updatedAt, &inProg, &finalizing, &completed,
		&failed, &cancelling, &cancelled, &expired, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.OwnerKeyID = str(owner)
	b.PrincipalID = str(principal)
	b.InputFileID = str(inFile)
	b.OutputFileID = str(outFile)
	b.ErrorFileID = str(errFile)
	if errsJSON != "" && errsJSON != "[]" && errsJSON != "null" {
		b.Errors = []byte(errsJSON)
	}
	b.Metadata = decodeMetadata(metaJSON)
	b.CreatedAt = TimeAt(createdAt)
	b.UpdatedAt = TimeAt(updatedAt)
	b.InProgressAt = TimeAt(nullInt(inProg))
	b.FinalizingAt = TimeAt(nullInt(finalizing))
	b.CompletedAt = TimeAt(nullInt(completed))
	b.FailedAt = TimeAt(nullInt(failed))
	b.CancellingAt = TimeAt(nullInt(cancelling))
	b.CancelledAt = TimeAt(nullInt(cancelled))
	b.ExpiredAt = TimeAt(nullInt(expired))
	b.ExpiresAt = TimeAt(nullInt(expires))
	return &b, nil
}

func scanBatchRow(row rowScanner) (*BatchRow, error) {
	var (
		r                    BatchRow
		logID, errMsg        sql.NullString
		body                 []byte
		createdAt, updatedAt int64
	)
	err := row.Scan(&r.BatchID, &r.CustomID, &r.Seq, &r.PrefixHash, &r.ModelGroup, &r.URL,
		&r.InputOffset, &r.InputLength, &r.Status, &r.Attempts, &r.StatusCode,
		&logID, &body, &r.ErrorCode, &errMsg, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.RequestLogID = str(logID)
	r.ErrorMessage = str(errMsg)
	if len(body) > 0 {
		// The driver may reuse its buffer once the row advances, so the body is
		// copied. A row handed to a caller that changes under it is the exact
		// aliasing bug the in-memory store clones to avoid.
		r.ResponseBody = append([]byte(nil), body...)
	}
	r.CreatedAt = TimeAt(createdAt)
	r.UpdatedAt = TimeAt(updatedAt)
	return &r, nil
}

func scanFile(row rowScanner) (*File, error) {
	var (
		f         File
		owner     sql.NullString
		expiresAt sql.NullInt64
		createdAt int64
	)
	err := row.Scan(&f.ID, &owner, &f.Purpose, &f.Filename, &f.Bytes, &f.SHA256,
		&f.StorageRef, &f.Status, &f.StatusDetails, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.OwnerKeyID = str(owner)
	f.CreatedAt = TimeAt(createdAt)
	f.ExpiresAt = TimeAt(nullInt(expiresAt))
	return &f, nil
}

func encodeMetadata(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeMetadata(s string) map[string]string {
	if s == "" || s == "{}" || s == "null" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func orJSONList(b []byte) []byte {
	if len(b) == 0 {
		return []byte("[]")
	}
	return b
}

func nullBytes(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

func whereClause(where []string) string {
	if len(where) == 0 {
		return ""
	}
	out := " WHERE " + where[0]
	for _, w := range where[1:] {
		out += " AND " + w
	}
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
	}
	return string(out)
}
