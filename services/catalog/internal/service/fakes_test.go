package service_test

import (
	"bytes"
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/events"
	"github.com/tbilol/dxrl/services/catalog/internal/service"
)

// fakeDB is an in-memory database with the semantics the service relies on
// from the Postgres repository.
type fakeDB struct {
	listings map[uuid.UUID]domain.Listing
	docs     map[uuid.UUID]domain.SearchDocument
	products map[commonv1.ProductType]domain.ProductType
	outbox   []events.Message
	// lockedOwners records LockOwner calls.
	lockedOwners []uuid.UUID
	// queries records FindListings calls; hits, when set, answers them
	// instead of the listings (search tests).
	queries []domain.ListingQuery
	hits    []domain.Hit
	// failWith, when set, makes every statement fail (the database is down).
	failWith error
}

func (d *fakeDB) clone() *fakeDB {
	c := *d
	c.listings = maps.Clone(d.listings)
	c.docs = maps.Clone(d.docs)
	c.outbox = slices.Clone(d.outbox)
	c.lockedOwners = slices.Clone(d.lockedOwners)
	c.queries = slices.Clone(d.queries)
	return &c
}

// fakeStore serializes everything; InTx works on a copy that replaces the
// state only when fn succeeds (rollback otherwise).
type fakeStore struct {
	mu sync.Mutex
	db *fakeDB
}

// testProducts is a slice of the reference data: two active products and a
// retired one.
var testProducts = []domain.ProductType{
	{Type: commonv1.ProductType_PRODUCT_TYPE_BUGDOY, Key: "bugdoy", NameUZ: "Bug'doy", NameRU: "Пшеница",
		Category: commonv1.ProductCategory_PRODUCT_CATEGORY_GRAIN_INDUSTRIAL, SortOrder: 10},
	{Type: commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA, Key: "kartoshka", NameUZ: "Kartoshka", NameRU: "Картофель",
		Category: commonv1.ProductCategory_PRODUCT_CATEGORY_VEGETABLE, SortOrder: 90},
	{Type: commonv1.ProductType_PRODUCT_TYPE_ACHCHIQ_QALAMPIR, Key: "achchiq-qalampir", NameUZ: "Achchiq qalampir",
		NameRU: "Острый перец", Category: commonv1.ProductCategory_PRODUCT_CATEGORY_VEGETABLE, SortOrder: 170},
	{Type: commonv1.ProductType_PRODUCT_TYPE_BEDA, Key: "beda", NameUZ: "Beda", NameRU: "Люцерна",
		Category: commonv1.ProductCategory_PRODUCT_CATEGORY_GRAIN_INDUSTRIAL, SortOrder: 80, Retired: true},
}

func newFakeStore() *fakeStore {
	db := &fakeDB{
		listings: map[uuid.UUID]domain.Listing{},
		docs:     map[uuid.UUID]domain.SearchDocument{},
		products: map[commonv1.ProductType]domain.ProductType{},
	}
	for _, p := range testProducts {
		db.products[p.Type] = p
	}
	return &fakeStore{db: db}
}

func (s *fakeStore) Run(_ context.Context, fn func(service.Queries) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&fakeQueries{db: s.db})
}

func (s *fakeStore) InTx(_ context.Context, fn func(service.Queries) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := s.db.clone()
	if err := fn(&fakeQueries{db: tx}); err != nil {
		return err
	}
	s.db = tx
	return nil
}

// state returns a snapshot of the committed database.
func (s *fakeStore) state() *fakeDB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.clone()
}

// update changes the committed database directly (test set-up).
func (s *fakeStore) update(fn func(d *fakeDB)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.db)
}

type fakeQueries struct{ db *fakeDB }

var _ service.Queries = (*fakeQueries)(nil)

func (q *fakeQueries) LockOwner(_ context.Context, owner uuid.UUID) error {
	if q.db.failWith != nil {
		return q.db.failWith
	}
	q.db.lockedOwners = append(q.db.lockedOwners, owner)
	return nil
}

func (q *fakeQueries) ListingByRequest(_ context.Context, owner, requestID uuid.UUID) (domain.Listing, error) {
	if q.db.failWith != nil {
		return domain.Listing{}, q.db.failWith
	}
	for _, l := range q.db.listings {
		if l.OwnerID == owner && l.CreateRequestID == requestID {
			return l, nil
		}
	}
	return domain.Listing{}, domain.ErrNotFound
}

func (q *fakeQueries) CountActiveListings(_ context.Context, owner uuid.UUID, kind commonv1.ListingKind) (int, error) {
	if q.db.failWith != nil {
		return 0, q.db.failWith
	}
	n := 0
	for _, l := range q.db.listings {
		if l.OwnerID == owner && l.Kind == kind && l.Status == domain.StatusActive {
			n++
		}
	}
	return n, nil
}

func (q *fakeQueries) InsertListing(_ context.Context, l domain.Listing, doc domain.SearchDocument) error {
	if q.db.failWith != nil {
		return q.db.failWith
	}
	l.MediaIDs = slices.Clone(l.MediaIDs)
	q.db.listings[l.ID] = l
	q.db.docs[l.ID] = doc
	return nil
}

func (q *fakeQueries) GetListing(_ context.Context, id uuid.UUID) (domain.Listing, error) {
	if q.db.failWith != nil {
		return domain.Listing{}, q.db.failWith
	}
	l, ok := q.db.listings[id]
	if !ok {
		return l, domain.ErrNotFound
	}
	l.MediaIDs = slices.Clone(l.MediaIDs)
	return l, nil
}

func (q *fakeQueries) LockListing(ctx context.Context, id uuid.UUID) (domain.Listing, error) {
	return q.GetListing(ctx, id)
}

func (q *fakeQueries) SaveListing(_ context.Context, l domain.Listing, doc *domain.SearchDocument) error {
	if q.db.failWith != nil {
		return q.db.failWith
	}
	prev, ok := q.db.listings[l.ID]
	if !ok {
		return domain.ErrNotFound
	}
	l.ViewsCount = prev.ViewsCount // never written by SaveListing
	l.MediaIDs = slices.Clone(l.MediaIDs)
	q.db.listings[l.ID] = l
	if doc != nil {
		q.db.docs[l.ID] = *doc
	}
	return nil
}

// FindListings answers the canned hits when set; otherwise it filters the
// listings by the criteria the fake understands (statuses, kind, owner,
// products, regions) and orders them newest first, keyset on (created_at,
// id).
func (q *fakeQueries) FindListings(_ context.Context, lq domain.ListingQuery) ([]domain.Hit, error) {
	if q.db.failWith != nil {
		return nil, q.db.failWith
	}
	q.db.queries = append(q.db.queries, lq)
	if q.db.hits != nil {
		return q.db.hits[:min(len(q.db.hits), lq.Limit)], nil
	}
	c := lq.Criteria
	var out []domain.Hit
	for _, l := range q.db.listings {
		if !slices.Contains(c.Statuses, l.Status) || (c.Kind != 0 && l.Kind != c.Kind) ||
			(c.OwnerID != uuid.Nil && l.OwnerID != c.OwnerID) ||
			(len(c.Products) > 0 && !slices.Contains(c.Products, l.Product)) ||
			(len(c.Regions) > 0 && !slices.Contains(c.Regions, l.Address.Region)) {
			continue
		}
		if a := lq.After; a != nil && newer(l, a.CreatedAt, a.ID) >= 0 {
			continue
		}
		out = append(out, domain.Hit{Listing: l})
	}
	slices.SortFunc(out, func(a, b domain.Hit) int { return -newer(a.Listing, b.CreatedAt, b.ID) })
	return out[:min(len(out), lq.Limit)], nil
}

// newer compares l with the position (at, id) in (created_at, id) order: 1
// when l comes after it in ascending order.
func newer(l domain.Listing, at time.Time, id uuid.UUID) int {
	return cmp.Or(l.CreatedAt.Compare(at), bytes.Compare(l.ID[:], id[:]))
}

func (q *fakeQueries) GetProductType(_ context.Context, t commonv1.ProductType) (domain.ProductType, error) {
	if q.db.failWith != nil {
		return domain.ProductType{}, q.db.failWith
	}
	p, ok := q.db.products[t]
	if !ok {
		return p, domain.ErrNotFound
	}
	return p, nil
}

func (q *fakeQueries) ListProductTypes(_ context.Context, category commonv1.ProductCategory) ([]domain.ProductType, error) {
	if q.db.failWith != nil {
		return nil, q.db.failWith
	}
	var out []domain.ProductType
	for _, p := range q.db.products {
		if category == 0 || p.Category == category {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b domain.ProductType) int { return cmp.Compare(a.SortOrder, b.SortOrder) })
	return out, nil
}

func (q *fakeQueries) EnqueueOutbox(_ context.Context, m events.Message) error {
	if q.db.failWith != nil {
		return q.db.failWith
	}
	q.db.outbox = append(q.db.outbox, m)
	return nil
}

// fakeLimiter answers Allow from its fields and counts the calls.
type fakeLimiter struct {
	mu         sync.Mutex
	allowed    bool
	retryAfter time.Duration
	err        error
	keys       []string
}

func (l *fakeLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	return l.allowed, l.retryAfter, l.err
}

// fakeViews records views.
type fakeViews struct {
	mu    sync.Mutex
	views [][2]uuid.UUID
}

func (v *fakeViews) Record(listing, viewer uuid.UUID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.views = append(v.views, [2]uuid.UUID{listing, viewer})
}

func (v *fakeViews) recorded() [][2]uuid.UUID {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.views)
}
