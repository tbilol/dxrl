package service_test

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/eijara"
	"github.com/tbilol/dxrl/services/identity/internal/events"
	"github.com/tbilol/dxrl/services/identity/internal/service"
)

// fakeDB is an in-memory database with the semantics of the Postgres
// repository (idempotency keys, the partial unique index, guarded updates).
type fakeDB struct {
	accounts      map[uuid.UUID]domain.Account
	createReqs    map[uuid.UUID]uuid.UUID
	verifications map[uuid.UUID]domain.Verification
	lastErrors    map[uuid.UUID]string
	outbox        []events.Message
	// outboxErr, when set, makes EnqueueOutbox fail (the transaction then
	// rolls back, like a failed INSERT INTO outbox).
	outboxErr error
}

func (d *fakeDB) clone() *fakeDB {
	c := &fakeDB{
		accounts:      maps.Clone(d.accounts),
		createReqs:    maps.Clone(d.createReqs),
		verifications: maps.Clone(d.verifications),
		lastErrors:    maps.Clone(d.lastErrors),
		outbox:        slices.Clone(d.outbox),
		outboxErr:     d.outboxErr,
	}
	for id, a := range c.accounts {
		a.Roles = slices.Clone(a.Roles)
		c.accounts[id] = a
	}
	return c
}

// fakeStore serializes everything; InTx works on a copy that replaces the
// state only when fn succeeds (rollback otherwise).
type fakeStore struct {
	mu sync.Mutex
	db *fakeDB
}

func newFakeStore() *fakeStore {
	return &fakeStore{db: &fakeDB{
		accounts:      map[uuid.UUID]domain.Account{},
		createReqs:    map[uuid.UUID]uuid.UUID{},
		verifications: map[uuid.UUID]domain.Verification{},
		lastErrors:    map[uuid.UUID]string{},
	}}
}

func (s *fakeStore) Run(_ context.Context, fn func(service.Queries) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&fakeQueries{db: s.db})
}

func (s *fakeStore) InTx(_ context.Context, fn func(service.Queries) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	work := s.db.clone()
	if err := fn(&fakeQueries{db: work}); err != nil {
		return err
	}
	s.db = work
	return nil
}

// snapshot returns a copy of the state for assertions.
func (s *fakeStore) snapshot() *fakeDB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.clone()
}

func (s *fakeStore) update(fn func(db *fakeDB)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.db)
}

type fakeQueries struct{ db *fakeDB }

var _ service.Queries = (*fakeQueries)(nil)

func (q *fakeQueries) CreateAccount(ctx context.Context, a domain.Account, requestID uuid.UUID) (domain.Account, bool, error) {
	if id, ok := q.db.createReqs[requestID]; ok {
		acct, err := q.GetAccount(ctx, id)
		return acct, false, err
	}
	a.Roles = slices.Clone(a.Roles)
	q.db.accounts[a.ID] = a
	q.db.createReqs[requestID] = a.ID
	acct, err := q.GetAccount(ctx, a.ID)
	return acct, true, err
}

func (q *fakeQueries) GetAccount(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	a, ok := q.db.accounts[id]
	if !ok {
		return domain.Account{}, domain.ErrNotFound
	}
	a.Roles = slices.Clone(a.Roles)
	slices.Sort(a.Roles)
	if v, err := q.LatestVerification(ctx, id); err == nil {
		a.FarmerVerificationStatus = v.Status
	}
	return a, nil
}

func (q *fakeQueries) LockAccount(_ context.Context, id uuid.UUID) error {
	if _, ok := q.db.accounts[id]; !ok {
		return domain.ErrNotFound
	}
	return nil
}

func (q *fakeQueries) GrantRole(_ context.Context, id uuid.UUID, role commonv1.Role, now time.Time) (bool, error) {
	a, ok := q.db.accounts[id]
	if !ok || a.HasRole(role) {
		return false, nil
	}
	a.Roles = append(slices.Clone(a.Roles), role)
	a.Version++
	a.UpdatedAt = now
	q.db.accounts[id] = a
	return true, nil
}

func (q *fakeQueries) RevokeFarmerRoleUnlessVerified(_ context.Context, id uuid.UUID, now time.Time) (bool, error) {
	a, ok := q.db.accounts[id]
	if !ok || !a.HasRole(domain.RoleFarmer) {
		return false, nil
	}
	for _, v := range q.db.verifications {
		if v.UserID == id && v.Status == domain.VerificationVerified {
			return false, nil
		}
	}
	a.Roles = slices.DeleteFunc(slices.Clone(a.Roles), func(r commonv1.Role) bool { return r == domain.RoleFarmer })
	a.Version++
	a.UpdatedAt = now
	q.db.accounts[id] = a
	return true, nil
}

func (q *fakeQueries) SetPlan(_ context.Context, id uuid.UUID, plan identityv1.Plan, now time.Time) error {
	a, ok := q.db.accounts[id]
	if !ok {
		return domain.ErrNotFound
	}
	a.Plan, a.PremiumUntil, a.UpdatedAt = plan, nil, now
	a.Version++
	q.db.accounts[id] = a
	return nil
}

func (q *fakeQueries) GetVerification(_ context.Context, id uuid.UUID) (domain.Verification, error) {
	v, ok := q.db.verifications[id]
	if !ok {
		return domain.Verification{}, domain.ErrNotFound
	}
	return v, nil
}

func (q *fakeQueries) VerificationByRequest(_ context.Context, userID, requestID uuid.UUID) (domain.Verification, error) {
	for _, v := range q.db.verifications {
		if v.UserID == userID && v.RequestID == requestID {
			return v, nil
		}
	}
	return domain.Verification{}, domain.ErrNotFound
}

func (q *fakeQueries) LatestVerification(_ context.Context, userID uuid.UUID) (domain.Verification, error) {
	var (
		latest domain.Verification
		found  bool
	)
	for _, v := range q.db.verifications {
		if v.UserID != userID {
			continue
		}
		if !found || v.SubmittedAt.After(latest.SubmittedAt) ||
			(v.SubmittedAt.Equal(latest.SubmittedAt) && v.ID.String() > latest.ID.String()) {
			latest, found = v, true
		}
	}
	if !found {
		return domain.Verification{}, domain.ErrNotFound
	}
	return latest, nil
}

func (q *fakeQueries) ContractClaimed(_ context.Context, contract string, userID uuid.UUID) (bool, error) {
	for _, v := range q.db.verifications {
		if v.ContractNumber == contract && v.Status == domain.VerificationVerified && v.UserID != userID {
			return true, nil
		}
	}
	return false, nil
}

func (q *fakeQueries) InsertVerification(ctx context.Context, v domain.Verification) (domain.Verification, bool, error) {
	if existing, err := q.VerificationByRequest(ctx, v.UserID, v.RequestID); err == nil {
		return existing, false, nil
	}
	v.Status = domain.VerificationPending
	q.db.verifications[v.ID] = v
	return v, true, nil
}

func (q *fakeQueries) DecideVerification(_ context.Context, d domain.Decision) (bool, error) {
	v, ok := q.db.verifications[d.VerificationID]
	if !ok || v.Status != domain.VerificationPending {
		return false, nil
	}
	if d.Status == domain.VerificationVerified {
		for _, o := range q.db.verifications {
			if o.ID != v.ID && o.Status == domain.VerificationVerified && o.ContractNumber == v.ContractNumber {
				return false, domain.ErrContractClaimed // the partial unique index
			}
		}
	}
	q.db.verifications[v.ID] = v.Apply(d)
	return true, nil
}

func (q *fakeQueries) ClaimPendingVerifications(_ context.Context, now time.Time, limit int, backoff, maxBackoff time.Duration) ([]domain.Verification, error) {
	var due []domain.Verification
	for _, v := range q.db.verifications {
		if v.Status == domain.VerificationPending && !v.NextAttemptAt.After(now) {
			due = append(due, v)
		}
	}
	slices.SortFunc(due, func(a, b domain.Verification) int { return a.NextAttemptAt.Compare(b.NextAttemptAt) })
	if len(due) > limit {
		due = due[:limit]
	}
	for i, v := range due {
		v.NextAttemptAt = now.Add(min(backoff*time.Duration(1<<min(v.Attempts, 20)), maxBackoff))
		v.Attempts++
		q.db.verifications[v.ID] = v
		due[i] = v
	}
	return due, nil
}

func (q *fakeQueries) RecordVerificationError(_ context.Context, id uuid.UUID, msg string) error {
	q.db.lastErrors[id] = msg
	return nil
}

func (q *fakeQueries) ExpiryCandidates(_ context.Context, today time.Time, limit int) ([]domain.Verification, error) {
	var due []domain.Verification
	for _, v := range q.db.verifications {
		if v.Status == domain.VerificationVerified && v.Lease != nil && v.Lease.ValidUntil.Before(today) {
			due = append(due, v)
		}
	}
	if len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

func (q *fakeQueries) ExpireVerification(_ context.Context, id uuid.UUID, today, _ time.Time) (domain.Verification, bool, error) {
	v, ok := q.db.verifications[id]
	if !ok || v.Status != domain.VerificationVerified || v.Lease == nil || !v.Lease.ValidUntil.Before(today) {
		return domain.Verification{}, false, nil
	}
	v.Status = domain.VerificationExpired
	q.db.verifications[id] = v
	return v, true, nil
}

func (q *fakeQueries) EnqueueOutbox(_ context.Context, m events.Message) error {
	if q.db.outboxErr != nil {
		return q.db.outboxErr
	}
	q.db.outbox = append(q.db.outbox, m)
	return nil
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// fakeLimiter answers from its fields and counts calls.
type fakeLimiter struct {
	mu         sync.Mutex
	deny       bool
	retryAfter time.Duration
	err        error
	calls      int
}

func (l *fakeLimiter) Allow(context.Context, string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return false, 0, l.err
	}
	if l.deny {
		return false, l.retryAfter, nil
	}
	return true, 0, nil
}

// checkerFunc adapts a function to service.EIjaraChecker.
type checkerFunc func(ctx context.Context, contractNumber string) (eijara.Result, error)

func (f checkerFunc) Check(ctx context.Context, n string) (eijara.Result, error) { return f(ctx, n) }

// blockingChecker never answers; it returns when the service's timeout fires.
var blockingChecker = checkerFunc(func(ctx context.Context, _ string) (eijara.Result, error) {
	<-ctx.Done()
	return eijara.Result{}, ctx.Err()
})
