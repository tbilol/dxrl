package service_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// accountUpdate is a decoded account-stream event for user at version, with
// a fresh event id; mutate adjusts the snapshot.
func accountUpdate(user uuid.UUID, version int64, mutate ...func(*domain.AccountUpdate)) domain.AccountUpdate {
	u := domain.AccountUpdate{
		EventID:    uuid.Must(uuid.NewV7()),
		EventType:  "agrogram.events.v1.AccountCreated",
		Topic:      "agrogram.identity.account.v1",
		OccurredAt: start,
		UserID:     user,
		Account:    domain.Account{Version: version},
	}
	for _, m := range mutate {
		m(&u)
	}
	return u
}

func eventType(name string) func(*domain.AccountUpdate) {
	return func(u *domain.AccountUpdate) { u.EventType = "agrogram.events.v1." + name }
}

// admin grants ROLE_ADMIN, the only role left.
func admin(u *domain.AccountUpdate) { u.Account.Roles = []commonv1.Role{domain.RoleAdmin} }

func (h *harness) apply(u domain.AccountUpdate, want domain.ApplyOutcome) {
	h.t.Helper()
	got, err := h.svc.ApplyAccountEvent(ctx, u)
	if err != nil {
		h.t.Fatalf("ApplyAccountEvent(%s v%d): %v", u.EventType, u.Account.Version, err)
	}
	if got != want {
		h.t.Fatalf("ApplyAccountEvent(%s v%d) = %s, want %s", u.EventType, u.Account.Version, got, want)
	}
}

// AccountCreated creates the PENDING row; CreateProfile then keeps the
// projection.
func TestAccountCreatedThenCreateProfile(t *testing.T) {
	h := newHarness(t)
	p := user()
	h.apply(accountUpdate(p.UserID, 1), domain.Applied)
	row := h.row(p.UserID)
	if row.Status != domain.ProfilePending || row.Account.Version != 1 || row.Version != 0 || row.DisplayName != "" {
		t.Fatalf("pending row = %+v", row)
	}
	h.apply(accountUpdate(p.UserID, 2, eventType("AccountRoleChanged"), admin), domain.Applied)

	prof := h.create(p, "Dehqon")
	if !prof.Public() || !prof.HasRole(domain.RoleAdmin) || prof.Account.Version != 2 || prof.Version != 1 {
		t.Fatalf("created = %+v", prof)
	}
}

// CreateProfile before any account event: the row starts at account version
// 0 and the events fill in the projection without touching the user's
// fields.
func TestCreateProfileBeforeAccountEvents(t *testing.T) {
	h := newHarness(t)
	p := user()
	prof := h.create(p, "Early Bird")
	if prof.Account.Version != 0 || len(prof.Account.Roles) != 0 {
		t.Fatalf("created = %+v", prof)
	}
	h.apply(accountUpdate(p.UserID, 1), domain.Applied)
	h.apply(accountUpdate(p.UserID, 2, eventType("AccountRoleChanged"), admin), domain.Applied)

	got, err := h.svc.GetProfile(ctx, p, p.UserID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Early Bird" || got.District != "Asaka" || got.Version != 1 || got.Account.Version != 2 ||
		!got.HasRole(domain.RoleAdmin) || got.CreateRequestID != prof.CreateRequestID {
		t.Fatalf("after events = %+v (user fields and profile version must be untouched)", got)
	}
}

func TestAccountEventDuplicatesAreIgnored(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	created := accountUpdate(id, 1)
	h.apply(created, domain.Applied)
	promoted := accountUpdate(id, 2, eventType("AccountRoleChanged"), admin)
	h.apply(promoted, domain.Applied)

	h.apply(created, domain.Duplicate)
	h.apply(promoted, domain.Duplicate)
	if row := h.row(id); row.Account.Version != 2 || !row.HasRole(domain.RoleAdmin) {
		t.Fatalf("row = %+v", row)
	}
	if n := len(h.store.snapshot().processed); n != 2 {
		t.Fatalf("inbox rows = %d, want 2", n)
	}
}

// An older snapshot arriving after a newer one never overwrites it.
func TestAccountEventsOutOfOrder(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	h.apply(accountUpdate(id, 3, eventType("AccountRoleChanged"), admin), domain.Applied)
	h.apply(accountUpdate(id, 2, eventType("AccountRoleChanged")), domain.Stale) // ADMIN not granted yet
	h.apply(accountUpdate(id, 1), domain.Stale)                                  // AccountCreated last

	row := h.row(id)
	if row.Account.Version != 3 || !row.HasRole(domain.RoleAdmin) || len(row.Account.Roles) != 1 {
		t.Fatalf("row = %+v, want the version 3 snapshot", row)
	}
	// Each event was still recorded once (so a redelivery is a duplicate).
	if n := len(h.store.snapshot().processed); n != 3 {
		t.Fatalf("inbox rows = %d, want 3", n)
	}
}

// An event that repeats the current version is a no-op.
func TestAccountEventsThatRepeatTheVersion(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	h.apply(accountUpdate(id, 1), domain.Applied)
	h.apply(accountUpdate(id, 1, eventType("AccountRoleChanged"), admin), domain.Stale)
	if row := h.row(id); row.HasRole(domain.RoleAdmin) {
		t.Fatalf("row = %+v, want version 1 unchanged", row)
	}
}

// An event type newer than this code applies its snapshot like any other.
func TestUnknownAccountEventTypeAppliesSnapshot(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	h.apply(accountUpdate(id, 1), domain.Applied)
	h.apply(accountUpdate(id, 2, eventType("AccountFrozen"), admin), domain.Applied)
	if row := h.row(id); row.Account.Version != 2 || !row.HasRole(domain.RoleAdmin) {
		t.Fatalf("row = %+v", row)
	}
}

// A failed transaction records nothing: the event is not marked processed,
// so the consumer's retry applies it.
func TestAccountEventDatabaseDown(t *testing.T) {
	h := newHarness(t)
	down := errors.New("connection refused")
	h.store.update(func(db *fakeDB) { db.failWith = down })
	u := accountUpdate(uuid.New(), 1)
	if _, err := h.svc.ApplyAccountEvent(ctx, u); !errors.Is(err, down) {
		t.Fatalf("err = %v, want the database error", err)
	}
	h.store.update(func(db *fakeDB) { db.failWith = nil })
	h.apply(u, domain.Applied)
}
