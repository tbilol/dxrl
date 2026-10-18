package service_test

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
	"github.com/tbilol/dxrl/services/profile-social/internal/events"
	"github.com/tbilol/dxrl/services/profile-social/internal/service"
)

type edge struct{ follower, followee uuid.UUID }

// fakeDB is an in-memory database with the semantics of the Postgres
// repository (upserts, ON CONFLICT DO NOTHING, guarded counters).
type fakeDB struct {
	profiles  map[uuid.UUID]domain.Profile
	follows   map[edge]time.Time
	processed map[uuid.UUID]domain.ProcessedEvent
	outbox    []events.Message
	// failWith, when set, makes every statement fail (the database is down).
	failWith error
}

func (d *fakeDB) clone() *fakeDB {
	c := &fakeDB{
		profiles:  maps.Clone(d.profiles),
		follows:   maps.Clone(d.follows),
		processed: maps.Clone(d.processed),
		outbox:    slices.Clone(d.outbox),
		failWith:  d.failWith,
	}
	for id, p := range c.profiles {
		p.Account.Roles = slices.Clone(p.Account.Roles)
		c.profiles[id] = p
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
		profiles:  map[uuid.UUID]domain.Profile{},
		follows:   map[edge]time.Time{},
		processed: map[uuid.UUID]domain.ProcessedEvent{},
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

func (q *fakeQueries) GetProfile(_ context.Context, id, viewer uuid.UUID) (domain.Profile, error) {
	if q.db.failWith != nil {
		return domain.Profile{}, q.db.failWith
	}
	p, ok := q.db.profiles[id]
	if !ok {
		return domain.Profile{}, domain.ErrNotFound
	}
	p.Account.Roles = slices.Clone(p.Account.Roles)
	_, p.IsFollowedByMe = q.db.follows[edge{viewer, id}]
	return p, nil
}

func (q *fakeQueries) EnsureProfile(_ context.Context, id uuid.UUID, now time.Time) error {
	if q.db.failWith != nil {
		return q.db.failWith
	}
	if _, ok := q.db.profiles[id]; !ok {
		q.db.profiles[id] = domain.NewPendingProfile(id, now)
	}
	return nil
}

func (q *fakeQueries) LockProfiles(ctx context.Context, ids ...uuid.UUID) (map[uuid.UUID]domain.Profile, error) {
	out := map[uuid.UUID]domain.Profile{}
	for _, id := range ids {
		p, err := q.GetProfile(ctx, id, uuid.Nil)
		if err == nil {
			out[id] = p
		} else if err != domain.ErrNotFound {
			return nil, err
		}
	}
	return out, nil
}

func (q *fakeQueries) LockFollowGraph(ctx context.Context, id uuid.UUID) (domain.Profile, error) {
	return q.GetProfile(ctx, id, uuid.Nil)
}

func (q *fakeQueries) SaveProfile(_ context.Context, p domain.Profile) error {
	cur, ok := q.db.profiles[p.ID]
	if !ok {
		return domain.ErrNotFound
	}
	cur.Status, cur.ProfileFields, cur.CreateRequestID, cur.Version = p.Status, p.ProfileFields, p.CreateRequestID, p.Version
	cur.CreatedAt, cur.UpdatedAt, cur.DeletedAt = p.CreatedAt, p.UpdatedAt, p.DeletedAt
	q.db.profiles[p.ID] = cur
	return nil
}

func (q *fakeQueries) bump(id uuid.UUID, fn func(p *domain.Profile)) {
	p := q.db.profiles[id]
	fn(&p)
	q.db.profiles[id] = p
}

func (q *fakeQueries) InsertFollow(_ context.Context, follower, followee uuid.UUID, now time.Time) (bool, error) {
	e := edge{follower, followee}
	if _, ok := q.db.follows[e]; ok {
		return false, nil
	}
	q.db.follows[e] = now
	q.bump(followee, func(p *domain.Profile) { p.FollowerCount++ })
	q.bump(follower, func(p *domain.Profile) { p.FollowingCount++ })
	return true, nil
}

func (q *fakeQueries) DeleteFollow(_ context.Context, follower, followee uuid.UUID) (bool, error) {
	e := edge{follower, followee}
	if _, ok := q.db.follows[e]; !ok {
		return false, nil
	}
	delete(q.db.follows, e)
	q.bump(followee, func(p *domain.Profile) { p.FollowerCount = max(p.FollowerCount-1, 0) })
	q.bump(follower, func(p *domain.Profile) { p.FollowingCount = max(p.FollowingCount-1, 0) })
	return true, nil
}

func (q *fakeQueries) RemoveFollows(ctx context.Context, id uuid.UUID) error {
	for e := range q.db.follows {
		if e.follower == id || e.followee == id {
			if _, err := q.DeleteFollow(ctx, e.follower, e.followee); err != nil {
				return err
			}
		}
	}
	q.bump(id, func(p *domain.Profile) { p.FollowerCount, p.FollowingCount = 0, 0 })
	return nil
}

func (q *fakeQueries) ListFollowers(ctx context.Context, id, viewer uuid.UUID, after *domain.Cursor, limit int) ([]domain.FollowEntry, error) {
	return q.list(ctx, id, viewer, after, limit, func(e edge) (uuid.UUID, bool) { return e.follower, e.followee == id })
}

func (q *fakeQueries) ListFollowing(ctx context.Context, id, viewer uuid.UUID, after *domain.Cursor, limit int) ([]domain.FollowEntry, error) {
	return q.list(ctx, id, viewer, after, limit, func(e edge) (uuid.UUID, bool) { return e.followee, e.follower == id })
}

func (q *fakeQueries) list(ctx context.Context, _, viewer uuid.UUID, after *domain.Cursor, limit int,
	side func(edge) (uuid.UUID, bool),
) ([]domain.FollowEntry, error) {
	var out []domain.FollowEntry
	for e, at := range q.db.follows {
		other, ok := side(e)
		if !ok {
			continue
		}
		p, err := q.GetProfile(ctx, other, viewer)
		if err != nil || !p.Public() {
			continue
		}
		if after != nil && !before(at, other, after.At, after.ID) {
			continue
		}
		out = append(out, domain.FollowEntry{Profile: p, FollowedAt: at})
	}
	slices.SortFunc(out, func(a, b domain.FollowEntry) int {
		return newestFirst(a.FollowedAt, a.Profile.ID, b.FollowedAt, b.Profile.ID)
	})
	return out[:min(limit, len(out))], nil
}

// before reports (at, id) < (cAt, cID) as Postgres compares the row values.
func before(at time.Time, id uuid.UUID, cAt time.Time, cID uuid.UUID) bool {
	return at.Before(cAt) || (at.Equal(cAt) && bytes.Compare(id[:], cID[:]) < 0)
}

func newestFirst(aAt time.Time, aID uuid.UUID, bAt time.Time, bID uuid.UUID) int {
	if c := bAt.Compare(aAt); c != 0 {
		return c
	}
	return bytes.Compare(bID[:], aID[:])
}

func (q *fakeQueries) MarkProcessed(_ context.Context, e domain.ProcessedEvent) (bool, error) {
	if q.db.failWith != nil {
		return false, q.db.failWith
	}
	if _, ok := q.db.processed[e.EventID]; ok {
		return false, nil
	}
	q.db.processed[e.EventID] = e
	return true, nil
}

func (q *fakeQueries) ApplyAccount(_ context.Context, userID uuid.UUID, a domain.Account, now time.Time) (bool, error) {
	p, ok := q.db.profiles[userID]
	if !ok {
		p = domain.NewPendingProfile(userID, now)
	} else if p.Account.Version >= a.Version {
		return false, nil
	}
	a.Roles = slices.Clone(a.Roles)
	p.Account = a
	q.db.profiles[userID] = p
	return true, nil
}

func (q *fakeQueries) EnqueueOutbox(_ context.Context, m events.Message) error {
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

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeLimiter answers from its fields and counts calls.
type fakeLimiter struct {
	mu         sync.Mutex
	deny       bool
	retryAfter time.Duration
	err        error
	keys       []string
}

func (l *fakeLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	if l.err != nil {
		return false, 0, l.err
	}
	if l.deny {
		return false, l.retryAfter, nil
	}
	return true, 0, nil
}

// fakeListings stands in for catalog.
type fakeListings struct {
	mu    sync.Mutex
	page  domain.ListingPage
	err   error
	got   domain.ListingQuery
	gotP  auth.Principal
	calls int
}

func (f *fakeListings) ListUserListings(_ context.Context, p auth.Principal, q domain.ListingQuery) (domain.ListingPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.got, f.gotP = q, p
	return f.page, f.err
}
