package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every dependency is faked here, which is the point of deps.go: the whole
// administration surface is exercised without a database, a router, a pricing
// catalog or a capacity broker. None of these fakes is clever; each is the
// smallest thing that can tell a correct handler from an incorrect one.

// ---------------------------------------------------------------------------
// Clock and ids
// ---------------------------------------------------------------------------

var testNow = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testNow }

// seqIDs mints deterministic ids so that golden files are stable.
type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (s *seqIDs) next() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("id-%04d", s.n)
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

type fakePrincipal struct {
	kind  string
	id    string
	admin bool
	scope Scope
}

func (p fakePrincipal) ActorKind() string { return p.kind }
func (p fakePrincipal) ActorID() string   { return p.id }
func (p fakePrincipal) IsAdmin() bool     { return p.admin }
func (p fakePrincipal) AdminScope() Scope { return p.scope }

const (
	masterToken = "master-secret"
	adminToken  = "sk-admin-key"  // pragma: allowlist secret — test fixture
	userToken   = "sk-plain-user" // pragma: allowlist secret — test fixture
	// teamAToken is an administrative key that belongs to team-a. It is a full
	// administrator by role and a team administrator by scope, which is the
	// combination every scope test is about.
	teamAToken = "sk-team-a-admin" // pragma: allowlist secret — test fixture
)

type fakeAuth struct{}

func (fakeAuth) AuthenticateHeader(_ context.Context, h http.Header) (Principal, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(h.Get("Authorization"), "Bearer "))
	if tok == "" {
		tok = h.Get("X-Dorang-Key")
	}
	switch tok {
	case masterToken:
		return fakePrincipal{kind: "master", id: "", admin: true, scope: GlobalScope()}, nil
	case adminToken:
		return fakePrincipal{kind: "key", id: "key-admin", admin: true, scope: GlobalScope()}, nil
	case teamAToken:
		return fakePrincipal{kind: "key", id: "key-team-a", admin: true,
			scope: TeamScope("team-a")}, nil
	case userToken:
		return fakePrincipal{kind: "key", id: "key-user", admin: false}, nil
	}
	return nil, ErrUnauthenticated
}

// ---------------------------------------------------------------------------
// Hashing
// ---------------------------------------------------------------------------

type fakeHasher struct{ broken bool }

func (h fakeHasher) Lookup(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

func (h fakeHasher) Hash(token string) (string, string, error) {
	if h.broken {
		return "", "", fmt.Errorf("no pepper configured")
	}
	sum := sha256.Sum256([]byte("pepper|" + token))
	return hex.EncodeToString(sum[:]), "dorang_v1", nil
}

// Label mirrors the real rule: derived from the lookup, which is itself a
// truncated digest, so no part of the secret survives.
func (h fakeHasher) Label(token string) string {
	return "dk-" + h.Lookup(token)[:8]
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type fakeStore struct {
	mu sync.Mutex

	keys      map[string]*Key
	verifiers map[string]Verifier
	keyOrder  []string
	// secrets models §11.2c: a key has an id and one or more secrets, and
	// everything except the verifier hangs off the id.
	secrets map[string][]KeySecret

	users     map[string]*User
	userOrder []string

	teams     map[string]*Team
	teamOrder []string
	members   map[string][]TeamMember

	deployments map[string]*Deployment
	depOrder    []string
	aliases     []Alias

	budgets map[BudgetSubject]Budget

	logs []LogRow

	audits []AuditEntry
	// auditErr makes the audit write fail, so the "applied but not audited"
	// path is testable.
	auditErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		keys:        map[string]*Key{},
		verifiers:   map[string]Verifier{},
		users:       map[string]*User{},
		teams:       map[string]*Team{},
		members:     map[string][]TeamMember{},
		deployments: map[string]*Deployment{},
		budgets:     map[BudgetSubject]Budget{},
	}
}

// --- keys ---

func (s *fakeStore) CreateKey(_ context.Context, k *Key, v Verifier) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[k.ID]; ok {
		return ErrConflict
	}
	for _, existing := range s.verifiers {
		if existing.Lookup == v.Lookup {
			return ErrConflict
		}
	}
	cp := *k
	s.keys[k.ID] = &cp
	s.verifiers[k.ID] = v
	s.keyOrder = append(s.keyOrder, k.ID)
	if s.secrets == nil {
		s.secrets = map[string][]KeySecret{}
	}
	s.secrets[k.ID] = []KeySecret{{
		ID: k.ID + ".1", Generation: 1, KeyLabel: k.KeyLabel,
		Current: true, CreatedAt: k.CreatedAt,
	}}
	return nil
}

func (s *fakeStore) GetKey(_ context.Context, id string) (*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *k
	return &cp, nil
}

func (s *fakeStore) UpdateKey(_ context.Context, k *Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[k.ID]; !ok {
		return ErrNotFound
	}
	cp := *k
	s.keys[k.ID] = &cp
	return nil
}

func (s *fakeStore) DeleteKeys(_ context.Context, ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range ids {
		if _, ok := s.keys[id]; ok {
			delete(s.keys, id)
			delete(s.verifiers, id)
			n++
		}
	}
	kept := s.keyOrder[:0]
	for _, id := range s.keyOrder {
		if _, ok := s.keys[id]; ok {
			kept = append(kept, id)
		}
	}
	s.keyOrder = kept
	return n, nil
}

func (s *fakeStore) ListKeys(_ context.Context, f KeyFilter) ([]*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Key
	for _, id := range s.keyOrder {
		k := s.keys[id]
		if f.UserID != "" && k.UserID != f.UserID {
			continue
		}
		if f.TeamID != "" && k.TeamID != f.TeamID {
			continue
		}
		if f.Blocked != nil && k.Blocked != *f.Blocked {
			continue
		}
		cp := *k
		out = append(out, &cp)
	}
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			return nil, nil
		}
		out = out[f.Offset:]
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (s *fakeStore) ReplaceVerifier(_ context.Context, id string, v Verifier, label string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return ErrNotFound
	}
	s.verifiers[id] = v
	k.KeyLabel = label
	k.HashScheme = v.HashScheme
	k.UpdatedAt = now
	return nil
}

// --- rotation and pend (§11.2c, §11.6) ---

var _ RotatingKeyStore = (*fakeStore)(nil)
var _ PendableKeyStore = (*fakeStore)(nil)

func (s *fakeStore) Rotate(_ context.Context, id string, v Verifier, label string, grace time.Duration, now time.Time) (RotationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return RotationResult{}, ErrNotFound
	}
	list := s.secrets[id]
	var (
		prev    KeySecret
		prevIdx = -1
		maxGen  int64
	)
	for i, sec := range list {
		if sec.Generation > maxGen {
			maxGen = sec.Generation
		}
		if sec.Current {
			prev, prevIdx = sec, i
		}
	}
	if prevIdx < 0 {
		return RotationResult{}, ErrNotFound
	}
	graceEnd := now.Add(grace)
	list[prevIdx].Current = false
	list[prevIdx].ExpiresAt = graceEnd
	prev = list[prevIdx]

	next := KeySecret{
		ID: fmt.Sprintf("%s.%d", id, maxGen+1), Generation: maxGen + 1,
		KeyLabel: label, Current: true, CreatedAt: now,
	}
	s.secrets[id] = append(list, next)

	// The verifier follows the current secret; nothing else about the key is
	// written, which is what makes the "rotation preserves everything" test an
	// assertion about behaviour rather than about this fake.
	s.verifiers[id] = v
	k.KeyLabel = label
	k.HashScheme = v.HashScheme
	k.UpdatedAt = now
	return RotationResult{Previous: prev, New: next, PreviousExpiresAt: graceEnd}, nil
}

func (s *fakeStore) EndGrace(_ context.Context, id string, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, ok := s.secrets[id]
	if !ok {
		return 0, ErrNotFound
	}
	n := 0
	for i := range list {
		if list[i].Current || !list[i].RevokedAt.IsZero() {
			continue
		}
		list[i].RevokedAt = now
		n++
	}
	return n, nil
}

func (s *fakeStore) ListSecrets(_ context.Context, id string) ([]KeySecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, ok := s.secrets[id]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]KeySecret, len(list))
	copy(out, list)
	sort.Slice(out, func(i, j int) bool { return out[i].Generation > out[j].Generation })
	return out, nil
}

func (s *fakeStore) Pend(_ context.Context, id, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return ErrNotFound
	}
	if k.PendedAt.IsZero() {
		k.PendedAt = now
	}
	k.PendReason = reason
	k.UpdatedAt = now
	return nil
}

func (s *fakeStore) Release(_ context.Context, id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return ErrNotFound
	}
	k.PendedAt = time.Time{}
	k.PendReason = ""
	k.UpdatedAt = now
	return nil
}

// verifierFor is a test-only reach into what was stored, so that a test can
// assert the plaintext was never kept.
func (s *fakeStore) verifierFor(id string) Verifier {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifiers[id]
}

// --- users ---

func (s *fakeStore) CreateUser(_ context.Context, u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[u.ID]; ok {
		return ErrConflict
	}
	for _, existing := range s.users {
		if existing.Email == u.Email {
			return ErrConflict
		}
	}
	cp := *u
	s.users[u.ID] = &cp
	s.userOrder = append(s.userOrder, u.ID)
	return nil
}

func (s *fakeStore) GetUser(_ context.Context, id string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *fakeStore) UpdateUser(_ context.Context, u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[u.ID]; !ok {
		return ErrNotFound
	}
	cp := *u
	s.users[u.ID] = &cp
	return nil
}

func (s *fakeStore) DeleteUsers(_ context.Context, ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range ids {
		if _, ok := s.users[id]; ok {
			delete(s.users, id)
			n++
		}
	}
	kept := s.userOrder[:0]
	for _, id := range s.userOrder {
		if _, ok := s.users[id]; ok {
			kept = append(kept, id)
		}
	}
	s.userOrder = kept
	return n, nil
}

func (s *fakeStore) ListUsers(_ context.Context, o ListOptions) ([]*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*User
	for _, id := range s.userOrder {
		cp := *s.users[id]
		out = append(out, &cp)
	}
	return pageSlice(out, o), nil
}

// --- teams ---

func (s *fakeStore) CreateTeam(_ context.Context, t *Team) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.teams[t.ID]; ok {
		return ErrConflict
	}
	cp := *t
	s.teams[t.ID] = &cp
	s.teamOrder = append(s.teamOrder, t.ID)
	return nil
}

func (s *fakeStore) GetTeam(_ context.Context, id string) (*Team, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.teams[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (s *fakeStore) UpdateTeam(_ context.Context, t *Team) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.teams[t.ID]; !ok {
		return ErrNotFound
	}
	cp := *t
	s.teams[t.ID] = &cp
	return nil
}

func (s *fakeStore) DeleteTeams(_ context.Context, ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range ids {
		if _, ok := s.teams[id]; ok {
			delete(s.teams, id)
			delete(s.members, id)
			n++
		}
	}
	kept := s.teamOrder[:0]
	for _, id := range s.teamOrder {
		if _, ok := s.teams[id]; ok {
			kept = append(kept, id)
		}
	}
	s.teamOrder = kept
	return n, nil
}

func (s *fakeStore) ListTeams(_ context.Context, o ListOptions) ([]*Team, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Team
	for _, id := range s.teamOrder {
		cp := *s.teams[id]
		out = append(out, &cp)
	}
	return pageSlice(out, o), nil
}

func (s *fakeStore) AddTeamMember(_ context.Context, m TeamMember) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.teams[m.TeamID]; !ok {
		return ErrNotFound
	}
	if _, ok := s.users[m.UserID]; !ok {
		return ErrNotFound
	}
	for _, existing := range s.members[m.TeamID] {
		if existing.UserID == m.UserID {
			return ErrConflict
		}
	}
	s.members[m.TeamID] = append(s.members[m.TeamID], m)
	return nil
}

func (s *fakeStore) RemoveTeamMember(_ context.Context, teamID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.members[teamID]
	for i, m := range list {
		if m.UserID == userID {
			s.members[teamID] = append(append([]TeamMember{}, list[:i]...), list[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (s *fakeStore) ListTeamMembers(_ context.Context, teamID string) ([]TeamMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TeamMember(nil), s.members[teamID]...), nil
}

// --- deployments ---

func (s *fakeStore) CreateDeployment(_ context.Context, d *Deployment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.deployments[d.ID]; ok {
		return ErrConflict
	}
	cp := *d
	s.deployments[d.ID] = &cp
	s.depOrder = append(s.depOrder, d.ID)
	return nil
}

func (s *fakeStore) GetDeployment(_ context.Context, id string) (*Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deployments[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *d
	return &cp, nil
}

func (s *fakeStore) UpdateDeployment(_ context.Context, d *Deployment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.deployments[d.ID]; !ok {
		return ErrNotFound
	}
	cp := *d
	s.deployments[d.ID] = &cp
	return nil
}

func (s *fakeStore) DeleteDeployment(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.deployments[id]; !ok {
		return ErrNotFound
	}
	delete(s.deployments, id)
	kept := s.depOrder[:0]
	for _, v := range s.depOrder {
		if v != id {
			kept = append(kept, v)
		}
	}
	s.depOrder = kept
	return nil
}

func (s *fakeStore) ListDeployments(_ context.Context) ([]*Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Deployment
	for _, id := range s.depOrder {
		cp := *s.deployments[id]
		out = append(out, &cp)
	}
	return out, nil
}

func (s *fakeStore) ListAliases(_ context.Context) ([]Alias, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Alias(nil), s.aliases...), nil
}

// --- budgets ---

func (s *fakeStore) SetBudget(_ context.Context, b Budget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.budgets[b.Subject]
	b.SpentNano = cur.SpentNano
	b.ReservedNano = cur.ReservedNano
	b.UpdatedAt = testNow
	s.budgets[b.Subject] = b
	return nil
}

func (s *fakeStore) GetBudget(_ context.Context, sub BudgetSubject) (Budget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.budgets[sub]
	if !ok {
		return Budget{}, ErrNotFound
	}
	return b, nil
}

func (s *fakeStore) ClearBudget(_ context.Context, sub BudgetSubject) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.budgets[sub]
	if !ok {
		return ErrNotFound
	}
	b.MaxBudgetNano = nil
	b.SoftBudgetNano = nil
	s.budgets[sub] = b
	return nil
}

func (s *fakeStore) ListBudgets(_ context.Context, o ListOptions) ([]Budget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Budget
	for _, b := range s.budgets {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return budgetID(out[i].Subject) < budgetID(out[j].Subject) })
	return pageSlice(out, o), nil
}

// --- ledger ---

func (s *fakeStore) addLog(r LogRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, r)
	sort.Slice(s.logs, func(i, j int) bool {
		if !s.logs[i].TS.Equal(s.logs[j].TS) {
			return s.logs[i].TS.After(s.logs[j].TS)
		}
		return s.logs[i].ID > s.logs[j].ID
	})
}

// ListRequests enforces the bounded-range rule the real store enforces, so the
// handler's surfacing of it is exercised end to end rather than only at the
// handler's own guard.
func (s *fakeStore) ListRequests(_ context.Context, q LogQuery) (LogPage, error) {
	if q.Range.Start.IsZero() || q.Range.End.IsZero() || !q.Range.End.After(q.Range.Start) {
		return LogPage{}, ErrUnboundedRange
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	var out []LogRow
	for _, r := range s.logs {
		if r.TS.Before(q.Range.Start) || !r.TS.Before(q.Range.End) {
			continue
		}
		if q.KeyID != "" && r.APIKeyID != q.KeyID {
			continue
		}
		if q.TeamID != "" && r.TeamID != q.TeamID {
			continue
		}
		if q.UserID != "" && r.UserID != q.UserID {
			continue
		}
		if q.TraceID != "" && r.TraceID != q.TraceID {
			continue
		}
		if q.Tag != "" && !containsString(r.Tags, q.Tag) {
			continue
		}
		if q.ErrorsOnly && r.Status < 400 {
			continue
		}
		if q.After != nil {
			if r.TS.After(q.After.TS) || (r.TS.Equal(q.After.TS) && r.ID >= q.After.ID) {
				continue
			}
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	page := LogPage{Rows: out}
	if len(out) == limit && limit > 0 {
		last := out[len(out)-1]
		page.Next = &Cursor{TS: last.TS, ID: last.ID}
	}
	return page, nil
}

func (s *fakeStore) Report(_ context.Context, q ReportQuery) (Report, error) {
	if q.Range.Start.IsZero() || q.Range.End.IsZero() || !q.Range.End.After(q.Range.Start) {
		return Report{}, ErrUnboundedRange
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	type key struct{ day, model, k, team, user, tag, provider string }
	buckets := map[key]*ReportRow{}
	total := Usage{NotionalKnown: true}

	for _, r := range s.logs {
		if r.TS.Before(q.Range.Start) || !r.TS.Before(q.Range.End) {
			continue
		}
		tags := r.Tags
		if !hasDimension(q.GroupBy, GroupByTag) {
			tags = []string{""}
		} else if len(tags) == 0 {
			tags = []string{""}
		}
		for _, tag := range tags {
			var kk key
			for _, g := range q.GroupBy {
				switch g {
				case GroupByDay:
					kk.day = r.TS.UTC().Format("2006-01-02")
				case GroupByModel:
					kk.model = r.ModelGroup
				case GroupByKey:
					kk.k = r.APIKeyID
				case GroupByTeam:
					kk.team = r.TeamID
				case GroupByUser:
					kk.user = r.UserID
				case GroupByTag:
					kk.tag = tag
				case GroupByProvider:
					kk.provider = r.ProviderID
				}
			}
			row := buckets[kk]
			if row == nil {
				row = &ReportRow{ModelGroup: kk.model, KeyID: kk.k, TeamID: kk.team,
					UserID: kk.user, Tag: kk.tag, ProviderID: kk.provider}
				if kk.day != "" {
					row.Day, _ = time.Parse("2006-01-02", kk.day)
				}
				row.Usage.NotionalKnown = true
				buckets[kk] = row
			}
			u := usageOf(r)
			row.Usage.Add(u)
			total.Add(u)
		}
	}

	rows := make([]ReportRow, 0, len(buckets))
	for _, r := range buckets {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Day.Equal(rows[j].Day) {
			return rows[i].Day.After(rows[j].Day)
		}
		return reportKeyString(rows[i]) < reportKeyString(rows[j])
	})
	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return Report{Range: q.Range, Rows: rows, Total: total}, nil
}

func reportKeyString(r ReportRow) string {
	return r.ModelGroup + "|" + r.KeyID + "|" + r.TeamID + "|" + r.UserID + "|" + r.Tag + "|" + r.ProviderID
}

func usageOf(r LogRow) Usage {
	u := Usage{
		Requests:             1,
		PromptTokens:         r.PromptTokens,
		CompletionTokens:     r.CompletionTokens,
		CachedTokens:         r.CachedTokens,
		ReasoningTokens:      r.ReasoningTokens,
		TotalTokens:          r.TotalTokens,
		CostNano:             r.CostNano,
		MarginalCostNano:     r.MarginalCostNano,
		SubscriptionCostNano: r.SubscriptionCostNano,
		NotionalNano:         r.NotionalNano,
		NotionalKnown:        r.NotionalKnown,
		LatencyMSSum:         r.LatencyMS,
	}
	if r.Status >= 400 {
		u.Errors = 1
	}
	return u
}

func hasDimension(gs []GroupBy, want GroupBy) bool {
	for _, g := range gs {
		if g == want {
			return true
		}
	}
	return false
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// --- audit ---

func (s *fakeStore) Record(_ context.Context, e AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auditErr != nil {
		return s.auditErr
	}
	s.audits = append(s.audits, e)
	return nil
}

func (s *fakeStore) auditLog() []AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditEntry(nil), s.audits...)
}

func (s *fakeStore) lastAudit() (AuditEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.audits) == 0 {
		return AuditEntry{}, false
	}
	return s.audits[len(s.audits)-1], true
}

func pageSlice[T any](in []T, o ListOptions) []T {
	if o.Offset > 0 {
		if o.Offset >= len(in) {
			return nil
		}
		in = in[o.Offset:]
	}
	if o.Limit > 0 && len(in) > o.Limit {
		in = in[:o.Limit]
	}
	return in
}

// ---------------------------------------------------------------------------
// Reporters
// ---------------------------------------------------------------------------

type fakeCredentials struct{ list []CredentialStatus }

func (f fakeCredentials) Credentials(context.Context) ([]CredentialStatus, error) {
	return append([]CredentialStatus(nil), f.list...), nil
}

type fakeCapacity struct{ occ CapacityOccupancy }

func (f fakeCapacity) Occupancy(context.Context) (CapacityOccupancy, error) { return f.occ, nil }

type fakeCatalog struct {
	explanation ModelExplanation
	unverified  []string
}

func (f fakeCatalog) ExplainModel(kind, model string) (ModelExplanation, error) {
	ex := f.explanation
	ex.Kind, ex.Model = kind, model
	return ex, nil
}

func (f fakeCatalog) UnverifiedModels() []string { return append([]string(nil), f.unverified...) }

type fakePricer struct {
	ex  PriceExplanation
	err error
}

func (f fakePricer) Explain(context.Context, PriceRequest) (PriceExplanation, error) {
	return f.ex, f.err
}

type fakeReloader struct{ res ReloadResult }

func (f fakeReloader) Reload(context.Context) (ReloadResult, error) { return f.res, nil }

// fakeConfigWriter accepts any structured config edit without touching a file,
// so a route that goes through ConfigWriter reaches its handler in a test that
// has no config file to write.
type fakeConfigWriter struct{}

func (fakeConfigWriter) SetDeploymentEnabled(context.Context, string, string, string, int, bool) error {
	return nil
}

// fakeSpend answers "what has this key spent" from the SAME ledger rows the
// report endpoints aggregate.
//
// It sums them rather than returning a number a test typed, because the defect
// this fake exists to catch is a surface that disagrees with the ledger. A
// fixture that carried its own answer could not tell the two apart.
//
// A key with no rows is ABSENT from the map rather than present at zero, which
// is what [SpendReporter] requires: absent means "the counter has nothing to
// say", and the caller then keeps the stored column and knows it.
type fakeSpend struct {
	st  *fakeStore
	err error
	// calls counts batched lookups, so a test can assert a page of keys costs
	// one call rather than one per row.
	calls atomic.Int64
}

func (f *fakeSpend) KeySpend(_ context.Context, refs []KeySpendRef) (map[string]int64, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	want := make(map[string]bool, len(refs))
	for _, r := range refs {
		want[r.ID] = true
	}
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	out := map[string]int64{}
	for _, l := range f.st.logs {
		if l.APIKeyID != "" && want[l.APIKeyID] {
			out[l.APIKeyID] += l.CostNano
		}
	}
	return out, nil
}

// withSpend wires the spend reporter over the harness's own ledger, so that
// "what the page shows" and "what the ledger holds" are comparable in one test.
func withSpend(c *Config) { c.Spend = &fakeSpend{st: c.Ledger.(*fakeStore)} }

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	t     *testing.T
	api   *API
	store *fakeStore
	ids   *seqIDs
	// tokens records the plaintext secrets the API returned, so a test can
	// assert none of them is ever stored or returned again.
	tokens []string
	// tokenSeq drives the deterministic token generator. It is atomic because
	// the concurrency test mints keys from several goroutines, and a generator
	// that repeats itself would make every concurrent creation collide on the
	// lookup and hide the thing being tested.
	tokenSeq atomic.Int64
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	st := newFakeStore()
	ids := &seqIDs{}
	h := &harness{t: t, store: st, ids: ids}

	cfg := Config{
		Auth:      fakeAuth{},
		Keys:      st,
		Hasher:    fakeHasher{},
		Directory: st,
		Models:    st,
		Budgets:   st,
		Ledger:    st,
		Audit:     st,
		Now:       fixedNow,
		NewID:     ids.next,
		NewToken: func() (string, error) {
			return keyPrefix + fmt.Sprintf("test-token-%04d", h.tokenSeq.Add(1)), nil
		},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	api, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.api = api
	return h
}

// do issues a request as the master credential unless overridden.
func (h *harness) do(method, path string, body any, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal request: %v", err)
		}
		rdr = strings.NewReader(string(buf))
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+masterToken)
	req.Header.Set("User-Agent", "admin-test")
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	return rec
}

func asToken(tok string) func(*http.Request) {
	return func(r *http.Request) {
		if tok == "" {
			r.Header.Del("Authorization")
			return
		}
		r.Header.Set("Authorization", "Bearer "+tok)
	}
}

// decode reads a JSON body into a map, failing the test on a non-JSON body.
func (h *harness) decode(rec *httptest.ResponseRecorder) map[string]any {
	h.t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatalf("response is not a JSON object (status %d): %v\nbody: %s",
			rec.Code, err, rec.Body.String())
	}
	return out
}

// expectStatus asserts a status and returns the decoded body.
func (h *harness) expectStatus(rec *httptest.ResponseRecorder, want int) map[string]any {
	h.t.Helper()
	if rec.Code != want {
		h.t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, want, rec.Body.String())
	}
	return h.decode(rec)
}

// expectFault asserts a refusal's status and machine-readable code.
func (h *harness) expectFault(rec *httptest.ResponseRecorder, status int, code string) map[string]any {
	h.t.Helper()
	body := h.expectStatus(rec, status)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		h.t.Fatalf("no error envelope in body: %s", rec.Body.String())
	}
	if got := errObj["code"]; got != code {
		h.t.Fatalf("error code = %v, want %q\nbody: %s", got, code, rec.Body.String())
	}
	if _, ok := errObj["code"].(string); !ok {
		h.t.Fatalf("error code must be a string, got %T", errObj["code"])
	}
	if got := rec.Header().Get("X-Dorang-Error-Code"); got != code {
		h.t.Errorf("X-Dorang-Error-Code = %q, want %q", got, code)
	}
	return body
}

// newKey issues a credential and records the returned plaintext.
func (h *harness) newKey(spec map[string]any) (id, token string) {
	h.t.Helper()
	rec := h.do(http.MethodPost, "/key/generate", spec)
	body := h.expectStatus(rec, http.StatusOK)
	token, _ = body["key"].(string)
	id, _ = body["token_id"].(string)
	if token == "" || id == "" {
		h.t.Fatalf("key/generate returned no key or id: %s", rec.Body.String())
	}
	h.tokens = append(h.tokens, token)
	return id, token
}
