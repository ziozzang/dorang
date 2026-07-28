package batch

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// MemStore is an in-memory [Store].
//
// It is a reference implementation and the one the tests run against. It is
// deliberately strict where a sloppy fake would be permissive: every record is
// cloned on the way in and on the way out, so no caller can hold a pointer into
// stored state and no in-memory aliasing can smuggle a value across what is
// supposed to be a process boundary. A restart test against a fake that handed
// out its own pointers would pass whether or not the state was ever persisted.
//
// It is not durable. Production runs against the SQL store (DESIGN §9.2); this
// is for tests and for a single-process notebook run that accepts losing batches
// when the process ends.
type MemStore struct {
	mu      sync.Mutex
	batches map[string]*BatchRecord
	rows    map[string][]*RowRecord
	files   map[string]*FileRecord
	seq     map[string]int // id → insertion order, for stable listing
	next    int
}

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{
		batches: make(map[string]*BatchRecord),
		rows:    make(map[string][]*RowRecord),
		files:   make(map[string]*FileRecord),
		seq:     make(map[string]int),
	}
}

// CreateBatch inserts a batch.
func (m *MemStore) CreateBatch(ctx context.Context, b *BatchRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.batches[b.ID]; ok {
		return fmt.Errorf("%w: batch %s", ErrExists, b.ID)
	}
	m.batches[b.ID] = b.Clone()
	m.seq[b.ID] = m.next
	m.next++
	return nil
}

// SaveBatch overwrites a batch.
func (m *MemStore) SaveBatch(ctx context.Context, b *BatchRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.batches[b.ID]; !ok {
		return fmt.Errorf("%w: batch %s", ErrNotFound, b.ID)
	}
	m.batches[b.ID] = b.Clone()
	return nil
}

// GetBatch returns one batch.
func (m *MemStore) GetBatch(ctx context.Context, id string) (*BatchRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.batches[id]
	if !ok {
		return nil, fmt.Errorf("%w: batch %s", ErrNotFound, id)
	}
	return b.Clone(), nil
}

// ListBatches returns a page, newest first.
func (m *MemStore) ListBatches(ctx context.Context, q BatchQuery) ([]*BatchRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	all := make([]*BatchRecord, 0, len(m.batches))
	for _, b := range m.batches {
		if q.OwnerKeyID != "" && b.OwnerKeyID != q.OwnerKeyID {
			continue
		}
		all = append(all, b)
	}
	sort.Slice(all, func(i, j int) bool { return m.seq[all[i].ID] > m.seq[all[j].ID] })

	start := 0
	if q.After != "" {
		for i, b := range all {
			if b.ID == q.After {
				start = i + 1
				break
			}
		}
	}
	all = all[start:]
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	more := len(all) > limit
	if more {
		all = all[:limit]
	}
	out := make([]*BatchRecord, 0, len(all))
	for _, b := range all {
		out = append(out, b.Clone())
	}
	return out, more, nil
}

// ActiveBatches returns every non-terminal batch, oldest first.
func (m *MemStore) ActiveBatches(ctx context.Context) ([]*BatchRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*BatchRecord, 0, 8)
	for _, b := range m.batches {
		if !b.Status.Terminal() {
			out = append(out, b.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return m.seq[out[i].ID] < m.seq[out[j].ID] })
	return out, nil
}

// PutRows inserts rows, leaving any that already exist untouched. That is what
// makes re-validating a batch after a crash safe: a row that already ran keeps
// its result instead of being reset to queued and paid for twice.
func (m *MemStore) PutRows(ctx context.Context, rows []*RowRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rows {
		existing := m.rows[r.BatchID]
		dup := false
		for _, e := range existing {
			if e.CustomID == r.CustomID {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		m.rows[r.BatchID] = append(existing, r.Clone())
	}
	for id, rs := range m.rows {
		sort.Slice(rs, func(i, j int) bool { return rs[i].Seq < rs[j].Seq })
		m.rows[id] = rs
	}
	return nil
}

// SaveRow overwrites one row.
func (m *MemStore) SaveRow(ctx context.Context, r *RowRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := m.rows[r.BatchID]
	for i, e := range rs {
		if e.CustomID == r.CustomID {
			rs[i] = r.Clone()
			return nil
		}
	}
	return fmt.Errorf("%w: row %s of batch %s", ErrNotFound, r.CustomID, r.BatchID)
}

// EachRow visits every row of a batch in seq order.
func (m *MemStore) EachRow(ctx context.Context, batchID string, fn func(*RowRecord) error) error {
	m.mu.Lock()
	rs := make([]*RowRecord, 0, len(m.rows[batchID]))
	for _, r := range m.rows[batchID] {
		rs = append(rs, r.Clone())
	}
	m.mu.Unlock()
	for _, r := range rs {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// CreateFile inserts a file record.
func (m *MemStore) CreateFile(ctx context.Context, f *FileRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[f.ID]; ok {
		return fmt.Errorf("%w: file %s", ErrExists, f.ID)
	}
	m.files[f.ID] = f.Clone()
	m.seq[f.ID] = m.next
	m.next++
	return nil
}

// GetFile returns one file record.
func (m *MemStore) GetFile(ctx context.Context, id string) (*FileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[id]
	if !ok {
		return nil, fmt.Errorf("%w: file %s", ErrNotFound, id)
	}
	return f.Clone(), nil
}

// ListFiles returns a page of file records, newest first.
func (m *MemStore) ListFiles(ctx context.Context, q FileQuery) ([]*FileRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	all := make([]*FileRecord, 0, len(m.files))
	for _, f := range m.files {
		if q.OwnerKeyID != "" && f.OwnerKeyID != q.OwnerKeyID {
			continue
		}
		if q.Purpose != "" && f.Purpose != q.Purpose {
			continue
		}
		all = append(all, f)
	}
	sort.Slice(all, func(i, j int) bool { return m.seq[all[i].ID] > m.seq[all[j].ID] })

	start := 0
	if q.After != "" {
		for i, f := range all {
			if f.ID == q.After {
				start = i + 1
				break
			}
		}
	}
	all = all[start:]
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	more := len(all) > limit
	if more {
		all = all[:limit]
	}
	out := make([]*FileRecord, 0, len(all))
	for _, f := range all {
		out = append(out, f.Clone())
	}
	return out, more, nil
}

// DeleteFile removes a file record.
func (m *MemStore) DeleteFile(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[id]; !ok {
		return fmt.Errorf("%w: file %s", ErrNotFound, id)
	}
	delete(m.files, id)
	return nil
}
