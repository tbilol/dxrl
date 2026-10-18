package service_test

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/events"
	"github.com/tbilol/dxrl/services/identity/internal/service"
)

// fakeDB is an in-memory database with the semantics of the Postgres
// repository (the CreateAccount idempotency key).
type fakeDB struct {
	accounts   map[uuid.UUID]domain.Account
	createReqs map[uuid.UUID]uuid.UUID
	outbox     []events.Message
	// outboxErr, when set, makes EnqueueOutbox fail (the transaction then
	// rolls back, like a failed INSERT INTO outbox).
	outboxErr error
}

func (d *fakeDB) clone() *fakeDB {
	c := &fakeDB{
		accounts:   maps.Clone(d.accounts),
		createReqs: maps.Clone(d.createReqs),
		outbox:     slices.Clone(d.outbox),
		outboxErr:  d.outboxErr,
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
		accounts:   map[uuid.UUID]domain.Account{},
		createReqs: map[uuid.UUID]uuid.UUID{},
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

// snapshot returns a copy of the committed state.
func (s *fakeStore) snapshot() *fakeDB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.clone()
}

// update changes the committed state directly (test set-up).
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

func (q *fakeQueries) GetAccount(_ context.Context, id uuid.UUID) (domain.Account, error) {
	a, ok := q.db.accounts[id]
	if !ok {
		return domain.Account{}, domain.ErrNotFound
	}
	a.Roles = slices.Clone(a.Roles)
	slices.Sort(a.Roles)
	return a, nil
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
