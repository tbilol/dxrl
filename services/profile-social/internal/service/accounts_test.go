package service_test

import (
	"errors"
	"testing"
	"time"

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
		Account:    domain.Account{Roles: []commonv1.Role{domain.RoleReseller}, Version: version},
	}
	for _, m := range mutate {
		m(&u)
	}
	return u
}

func eventType(name string) func(*domain.AccountUpdate) {
	return func(u *domain.AccountUpdate) { u.EventType = "agrogram.events.v1." + name }
}

func farmer(u *domain.AccountUpdate) {
	u.Account.Roles = []commonv1.Role{domain.RoleReseller, domain.RoleFarmer}
	u.Account.IsVerifiedFarmer = true
}

func premium(until *time.Time) func(*domain.AccountUpdate) {
	return func(u *domain.AccountUpdate) { u.Account.IsPremium, u.Account.PremiumUntil = true, until }
}

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

// AccountCreated creates the PENDING row; CreateProfile then keeps the badges.
func TestAccountCreatedThenCreateProfile(t *testing.T) {
	h := newHarness(t)
	p := user()
	h.apply(accountUpdate(p.UserID, 1), domain.Applied)
	row := h.row(p.UserID)
	if row.Status != domain.ProfilePending || row.Account.Version != 1 || row.Version != 0 || row.DisplayName != "" {
		t.Fatalf("pending row = %+v", row)
	}
	h.apply(accountUpdate(p.UserID, 2, eventType("AccountRoleChanged"), farmer), domain.Applied)

	prof := h.create(p, "Dehqon")
	if !prof.Public() || !prof.Account.IsVerifiedFarmer || prof.Account.Version != 2 || prof.Version != 1 {
		t.Fatalf("created = %+v", prof)
	}
}

// CreateProfile before any account event: the row starts at account version
// 0 and the events fill in the badges without touching the user's fields.
func TestCreateProfileBeforeAccountEvents(t *testing.T) {
	h := newHarness(t)
	p := user()
	prof := h.create(p, "Early Bird")
	if prof.Account.Version != 0 || prof.Account.IsVerifiedFarmer || prof.IsPremium {
		t.Fatalf("created = %+v", prof)
	}
	h.apply(accountUpdate(p.UserID, 1), domain.Applied)
	h.apply(accountUpdate(p.UserID, 2, eventType("AccountPremiumChanged"), premium(nil)), domain.Applied)

	got, err := h.svc.GetProfile(ctx, p, p.UserID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Early Bird" || got.District != "Asaka" || got.Version != 1 || got.Account.Version != 2 ||
		!got.IsPremium || got.CreateRequestID != prof.CreateRequestID {
		t.Fatalf("after events = %+v (user fields and profile version must be untouched)", got)
	}
}

func TestAccountEventDuplicatesAreIgnored(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	created := accountUpdate(id, 1)
	h.apply(created, domain.Applied)
	promoted := accountUpdate(id, 2, eventType("AccountRoleChanged"), farmer)
	h.apply(promoted, domain.Applied)

	h.apply(created, domain.Duplicate)
	h.apply(promoted, domain.Duplicate)
	if row := h.row(id); row.Account.Version != 2 || !row.Account.IsVerifiedFarmer {
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
	until := start.Add(24 * time.Hour)
	h.apply(accountUpdate(id, 3, eventType("AccountPremiumChanged"), farmer, premium(&until)), domain.Applied)
	h.apply(accountUpdate(id, 2, eventType("AccountRoleChanged"), farmer), domain.Stale)
	h.apply(accountUpdate(id, 1), domain.Stale) // AccountCreated last

	row := h.row(id)
	if row.Account.Version != 3 || !row.Account.IsPremium || row.Account.PremiumUntil == nil || !row.Account.IsVerifiedFarmer ||
		len(row.Account.Roles) != 2 {
		t.Fatalf("row = %+v, want the version 3 snapshot", row)
	}
	// Each event was still recorded once (so a redelivery is a duplicate).
	if n := len(h.store.snapshot().processed); n != 3 {
		t.Fatalf("inbox rows = %d, want 3", n)
	}
}

// FarmerVerified / FarmerVerificationExpired repeat the current version: they
// are no-ops; the badge changes with the AccountRoleChanged after them.
func TestAccountEventsThatRepeatTheVersion(t *testing.T) {
	h := newHarness(t)
	p := user()
	h.create(p, "Farmer To Be")
	h.apply(accountUpdate(p.UserID, 1), domain.Applied)
	h.apply(accountUpdate(p.UserID, 1, eventType("FarmerVerified")), domain.Stale)
	if prof, _ := h.svc.GetProfile(ctx, p, p.UserID.String()); prof.Account.IsVerifiedFarmer {
		t.Fatal("FarmerVerified alone must not set the badge")
	}
	h.apply(accountUpdate(p.UserID, 2, eventType("AccountRoleChanged"), farmer), domain.Applied)
	if prof, _ := h.svc.GetProfile(ctx, p, p.UserID.String()); !prof.Account.IsVerifiedFarmer {
		t.Fatal("AccountRoleChanged must set the badge")
	}
	h.apply(accountUpdate(p.UserID, 2, eventType("FarmerVerificationExpired"), farmer), domain.Stale)
	h.apply(accountUpdate(p.UserID, 3, eventType("AccountRoleChanged")), domain.Applied) // FARMER revoked
	prof, _ := h.svc.GetProfile(ctx, p, p.UserID.String())
	if prof.Account.IsVerifiedFarmer || prof.Account.Version != 3 || len(prof.Account.Roles) != 1 {
		t.Fatalf("after expiry = %+v", prof.Account)
	}
}

// An event type newer than this code applies its snapshot like any other.
func TestUnknownAccountEventTypeAppliesSnapshot(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	h.apply(accountUpdate(id, 1), domain.Applied)
	h.apply(accountUpdate(id, 2, eventType("AccountFrozen"), premium(nil)), domain.Applied)
	if row := h.row(id); row.Account.Version != 2 || !row.Account.IsPremium {
		t.Fatalf("row = %+v", row)
	}
}

func TestPremiumBadgeEndsWithPremiumUntil(t *testing.T) {
	h := newHarness(t)
	p := user()
	h.create(p, "Premium")
	until := start.Add(time.Hour)
	h.apply(accountUpdate(p.UserID, 2, eventType("AccountPremiumChanged"), premium(&until)), domain.Applied)
	if prof, _ := h.svc.GetProfile(ctx, p, p.UserID.String()); !prof.IsPremium {
		t.Fatal("premium badge must show before premium_until")
	}
	h.clock.Advance(2 * time.Hour)
	if prof, _ := h.svc.GetProfile(ctx, p, p.UserID.String()); prof.IsPremium {
		t.Fatal("premium badge must disappear after premium_until (identity publishes no event then)")
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
