package service_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/events"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/identityclient"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/service"
)

// fakeDB is an in-memory database with the semantics of the Postgres
// repository (unique constraints, guarded updates).
type fakeDB struct {
	creds      map[uuid.UUID]domain.Credential
	sessions   map[uuid.UUID]domain.Session
	refresh    map[uuid.UUID]domain.RefreshToken
	challenges map[uuid.UUID]domain.PhoneChallenge
	outbox     []events.Message
}

func (d *fakeDB) clone() *fakeDB {
	return &fakeDB{
		creds:      maps.Clone(d.creds),
		sessions:   maps.Clone(d.sessions),
		refresh:    maps.Clone(d.refresh),
		challenges: maps.Clone(d.challenges),
		outbox:     slices.Clone(d.outbox),
	}
}

// fakeStore serializes everything; InTx works on a copy that replaces the
// state only when fn succeeds (rollback otherwise).
type fakeStore struct {
	mu sync.Mutex
	db *fakeDB
}

func newFakeStore() *fakeStore {
	return &fakeStore{db: &fakeDB{
		creds:      map[uuid.UUID]domain.Credential{},
		sessions:   map[uuid.UUID]domain.Session{},
		refresh:    map[uuid.UUID]domain.RefreshToken{},
		challenges: map[uuid.UUID]domain.PhoneChallenge{},
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

// snapshot copies the state for assertions.
func (s *fakeStore) snapshot() *fakeDB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.clone()
}

type fakeQueries struct{ db *fakeDB }

var _ service.Queries = (*fakeQueries)(nil)

func (q *fakeQueries) CredentialBySubject(_ context.Context, method authgatewayv1.SignInMethod, subject string) (domain.Credential, error) {
	for _, c := range q.db.creds {
		if c.Method == method && c.Subject == subject {
			return c, nil
		}
	}
	return domain.Credential{}, domain.ErrNotFound
}

func (q *fakeQueries) CredentialsByUser(_ context.Context, userID uuid.UUID) ([]domain.Credential, error) {
	var out []domain.Credential
	for _, c := range q.db.creds {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b domain.Credential) int {
		if a.LinkedAt.Equal(b.LinkedAt) {
			return int(a.Method - b.Method)
		}
		return a.LinkedAt.Compare(b.LinkedAt)
	})
	return out, nil
}

func (q *fakeQueries) InsertCredential(_ context.Context, c domain.Credential) error {
	for _, other := range q.db.creds {
		switch {
		case other.UserID == c.UserID && other.Method == c.Method:
			return domain.ErrMethodLinked
		case other.Method == c.Method && other.Subject == c.Subject:
			return domain.ErrSubjectTaken
		}
	}
	q.db.creds[c.ID] = c
	return nil
}

func (q *fakeQueries) UpdatePasswordHash(_ context.Context, id uuid.UUID, hash string) error {
	c, ok := q.db.creds[id]
	if !ok {
		return domain.ErrNotFound
	}
	c.PasswordHash = hash
	q.db.creds[id] = c
	return nil
}

func (q *fakeQueries) TouchCredential(_ context.Context, id uuid.UUID, now time.Time) error {
	c, ok := q.db.creds[id]
	if !ok {
		return domain.ErrNotFound
	}
	c.LastUsedAt = &now
	q.db.creds[id] = c
	return nil
}

func (q *fakeQueries) DeleteCredential(_ context.Context, userID uuid.UUID, method authgatewayv1.SignInMethod) (bool, error) {
	for id, c := range q.db.creds {
		if c.UserID == userID && c.Method == method {
			delete(q.db.creds, id)
			return true, nil
		}
	}
	return false, nil
}

func (q *fakeQueries) InsertSession(_ context.Context, s domain.Session) error {
	q.db.sessions[s.ID] = s
	return nil
}

func (q *fakeQueries) GetSession(_ context.Context, id uuid.UUID) (domain.Session, error) {
	s, ok := q.db.sessions[id]
	if !ok {
		return domain.Session{}, domain.ErrNotFound
	}
	return s, nil
}

func (q *fakeQueries) TouchSession(_ context.Context, id uuid.UUID, now time.Time) error {
	s, ok := q.db.sessions[id]
	if !ok {
		return domain.ErrNotFound
	}
	s.LastSeenAt = now
	q.db.sessions[id] = s
	return nil
}

func (q *fakeQueries) RevokeSession(_ context.Context, id, userID uuid.UUID, reason string, now time.Time) (bool, error) {
	s, ok := q.db.sessions[id]
	if !ok || s.UserID != userID || s.RevokedAt != nil {
		return false, nil
	}
	s.RevokedAt, s.RevokedReason = &now, reason
	q.db.sessions[id] = s
	return true, nil
}

func (q *fakeQueries) RevokeUserSessions(_ context.Context, userID uuid.UUID, reason string, now time.Time) (int64, error) {
	var n int64
	for id, s := range q.db.sessions {
		if s.UserID == userID && s.RevokedAt == nil {
			s.RevokedAt, s.RevokedReason = &now, reason
			q.db.sessions[id] = s
			n++
		}
	}
	return n, nil
}

func (q *fakeQueries) InsertRefreshToken(_ context.Context, rt domain.RefreshToken) error {
	q.db.refresh[rt.ID] = rt
	return nil
}

func (q *fakeQueries) RefreshTokenByHash(_ context.Context, hash []byte) (domain.RefreshToken, error) {
	for _, rt := range q.db.refresh {
		if bytes.Equal(rt.Hash, hash) {
			return rt, nil
		}
	}
	return domain.RefreshToken{}, domain.ErrNotFound
}

func (q *fakeQueries) UseRefreshToken(_ context.Context, id, replacedBy uuid.UUID, now time.Time) (bool, error) {
	// refresh_tokens.replaced_by is a foreign key: the replacement must exist.
	if _, ok := q.db.refresh[replacedBy]; !ok {
		return false, fmt.Errorf("replaced_by %s does not exist (foreign key)", replacedBy)
	}
	rt, ok := q.db.refresh[id]
	if !ok || rt.UsedAt != nil {
		return false, nil
	}
	rt.UsedAt, rt.ReplacedBy = &now, &replacedBy
	q.db.refresh[id] = rt
	return true, nil
}

func (q *fakeQueries) InsertChallenge(_ context.Context, c domain.PhoneChallenge) error {
	q.db.challenges[c.ID] = c
	return nil
}

func (q *fakeQueries) GetChallenge(_ context.Context, id uuid.UUID) (domain.PhoneChallenge, error) {
	c, ok := q.db.challenges[id]
	if !ok {
		return domain.PhoneChallenge{}, domain.ErrNotFound
	}
	return c, nil
}

func (q *fakeQueries) LatestChallenge(_ context.Context, phone string, purpose authgatewayv1.PhoneCodePurpose) (domain.PhoneChallenge, error) {
	var (
		latest domain.PhoneChallenge
		found  bool
	)
	for _, c := range q.db.challenges {
		if c.PhoneNumber != phone || c.Purpose != purpose {
			continue
		}
		if !found || c.CreatedAt.After(latest.CreatedAt) {
			latest, found = c, true
		}
	}
	if !found {
		return domain.PhoneChallenge{}, domain.ErrNotFound
	}
	return latest, nil
}

func (q *fakeQueries) CountChallengeAttempt(_ context.Context, id uuid.UUID) error {
	c, ok := q.db.challenges[id]
	if !ok {
		return domain.ErrNotFound
	}
	c.Attempts++
	q.db.challenges[id] = c
	return nil
}

func (q *fakeQueries) ConsumeChallenge(_ context.Context, id uuid.UUID, now time.Time) (bool, error) {
	c, ok := q.db.challenges[id]
	if !ok || c.ConsumedAt != nil || !now.Before(c.ExpiresAt) || c.Attempts >= c.MaxAttempts {
		return false, nil
	}
	c.ConsumedAt = &now
	q.db.challenges[id] = c
	return true, nil
}

func (q *fakeQueries) EnqueueOutbox(_ context.Context, m events.Message) error {
	q.db.outbox = append(q.db.outbox, m)
	return nil
}

// fakeAccounts stands in for identity: idempotent CreateAccount, GetAccount,
// and a switch to make every call fail the way the real client would.
type fakeAccounts struct {
	mu        sync.Mutex
	byRequest map[uuid.UUID]uuid.UUID
	accounts  map[uuid.UUID]identityclient.Account
	err       error
	creates   int
	gets      int
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{
		byRequest: map[uuid.UUID]uuid.UUID{},
		accounts:  map[uuid.UUID]identityclient.Account{},
	}
}

// unavailable makes identity answer the way identityclient translates a dead
// connection or a timeout.
func (a *fakeAccounts) unavailable() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = apperr.New(commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE,
		"identity is unavailable", "dependency", "identity")
}

func (a *fakeAccounts) setStatus(id uuid.UUID, status identityv1.AccountStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	acct := a.accounts[id]
	acct.Status = status
	a.accounts[id] = acct
}

func (a *fakeAccounts) setPremium(id uuid.UUID, premium bool, roles ...commonv1.Role) {
	a.mu.Lock()
	defer a.mu.Unlock()
	acct := a.accounts[id]
	acct.IsPremium = premium
	if len(roles) > 0 {
		acct.Roles = roles
	}
	a.accounts[id] = acct
}

func (a *fakeAccounts) CreateAccount(_ context.Context, _ auth.Principal, requestID uuid.UUID) (identityclient.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return identityclient.Account{}, a.err
	}
	a.creates++
	if id, ok := a.byRequest[requestID]; ok {
		return a.accounts[id], nil
	}
	id := uuid.New()
	acct := identityclient.Account{
		ID:     id,
		Roles:  []commonv1.Role{commonv1.Role_ROLE_RESELLER},
		Status: identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE,
	}
	a.byRequest[requestID] = id
	a.accounts[id] = acct
	return acct, nil
}

func (a *fakeAccounts) GetAccount(_ context.Context, _ auth.Principal, userID uuid.UUID) (identityclient.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return identityclient.Account{}, a.err
	}
	a.gets++
	acct, ok := a.accounts[userID]
	if !ok {
		return identityclient.Account{}, apperr.New(commonv1.ErrorReason_ERROR_REASON_NOT_FOUND, "account not found")
	}
	return acct, nil
}

// fakeLimiter counts hits per key with a limit, and records the calls.
type fakeLimiter struct {
	mu      sync.Mutex
	limit   int
	hits    map[string]int
	fail    error
	allowed int
	peeked  int
}

func newFakeLimiter(limit int) *fakeLimiter {
	return &fakeLimiter{limit: limit, hits: map[string]int{}}
}

func (l *fakeLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.allowed++
	if l.fail != nil {
		return false, 0, l.fail
	}
	l.hits[key]++
	if l.hits[key] > l.limit {
		return false, 42 * time.Second, nil
	}
	return true, 0, nil
}

func (l *fakeLimiter) Exceeded(_ context.Context, key string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.peeked++
	if l.fail != nil {
		return false, 0, l.fail
	}
	if l.hits[key] > l.limit {
		return true, 42 * time.Second, nil
	}
	return false, 0, nil
}

func (l *fakeLimiter) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hits[key]
}

// fakeSender records the codes instead of sending them, which is how tests
// (and the local stack) complete the phone flow.
type fakeSender struct {
	mu   sync.Mutex
	sent []sentCode
	fail error
}

type sentCode struct {
	phone   string
	code    string
	purpose authgatewayv1.PhoneCodePurpose
}

func (s *fakeSender) Send(_ context.Context, phone, code string, purpose authgatewayv1.PhoneCodePurpose) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.sent = append(s.sent, sentCode{phone, code, purpose})
	return nil
}

func (s *fakeSender) last(t *testing.T) sentCode {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		t.Fatal("no code was sent")
	}
	return s.sent[len(s.sent)-1]
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

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// logBuffer is a synchronized buffer, so the logger can be read from a test.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func debugLogger(w *logBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
