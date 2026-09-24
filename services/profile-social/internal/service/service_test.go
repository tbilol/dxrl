package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	profilesocialv1 "github.com/tbilol/dxrl/gen/go/profilesocial/v1"
	"google.golang.org/grpc/codes"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
	"github.com/tbilol/dxrl/services/profile-social/internal/service"
)

var (
	ctx   = context.Background()
	start = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
)

type harness struct {
	t        *testing.T
	store    *fakeStore
	clock    *fakeClock
	limiter  *fakeLimiter
	listings *fakeListings
	svc      *service.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, store: newFakeStore(), clock: &fakeClock{t: start}, limiter: &fakeLimiter{}, listings: &fakeListings{}}
	h.svc = service.New(h.store, h.limiter, h.listings, service.WithClock(h.clock.Now))
	return h
}

// user is a signed-in principal for a fresh user id.
func user(roles ...commonv1.Role) auth.Principal {
	if len(roles) == 0 {
		roles = []commonv1.Role{domain.RoleReseller}
	}
	return auth.Principal{UserID: uuid.Must(uuid.NewV7()), Roles: roles, RequestID: uuid.NewString()}
}

func fieldsNamed(name string) domain.ProfileFields {
	return domain.ProfileFields{DisplayName: name, Region: commonv1.Region_REGION_ANDIJON, District: "Asaka"}
}

// create creates p's profile and fails the test on error.
func (h *harness) create(p auth.Principal, name string) domain.Profile {
	h.t.Helper()
	prof, err := h.svc.CreateProfile(ctx, p, uuid.NewString(), fieldsNamed(name))
	if err != nil {
		h.t.Fatalf("CreateProfile: %v", err)
	}
	return prof
}

// row is the stored profile row of id (any status).
func (h *harness) row(id uuid.UUID) domain.Profile {
	h.t.Helper()
	p, ok := h.store.snapshot().profiles[id]
	if !ok {
		h.t.Fatalf("no profile row for %s", id)
	}
	return p
}

// wantErr fails unless err is an apperr with the code and reason.
func wantErr(t *testing.T, err error, code codes.Code, reason commonv1.ErrorReason) *apperr.Error {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code || ae.Reason != reason {
		t.Fatalf("err = %v, want %s / %s", err, code, reason)
	}
	return ae
}

func TestCreateProfile(t *testing.T) {
	h := newHarness(t)
	p := user()
	reqID := uuid.NewString()
	f := domain.ProfileFields{DisplayName: "  Dilnoza  ", Bio: " Olma bog'i ", Region: commonv1.Region_REGION_FARGONA,
		District: " Quva ", AvatarMediaID: uuid.New()}

	prof, err := h.svc.CreateProfile(ctx, p, reqID, f)
	if err != nil {
		t.Fatal(err)
	}
	if prof.ID != p.UserID || !prof.Public() || prof.DisplayName != "Dilnoza" || prof.Bio != "Olma bog'i" ||
		prof.District != "Quva" || prof.Version != 1 || !prof.CreatedAt.Equal(start) || prof.AvatarMediaID != f.AvatarMediaID ||
		prof.Account.Version != 0 || prof.Account.IsVerifiedFarmer {
		t.Fatalf("profile = %+v", prof)
	}

	// A retry with the same request id returns the same profile.
	h.clock.Advance(time.Minute)
	again, err := h.svc.CreateProfile(ctx, p, reqID, fieldsNamed("Other"))
	if err != nil || again.DisplayName != "Dilnoza" || again.Version != 1 {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	// Another request id: the profile exists.
	_, err = h.svc.CreateProfile(ctx, p, uuid.NewString(), fieldsNamed("Other"))
	wantErr(t, err, codes.AlreadyExists, commonv1.ErrorReason_ERROR_REASON_ALREADY_EXISTS)

	// Validation the contract cannot express, and authentication.
	_, err = h.svc.CreateProfile(ctx, user(), uuid.NewString(), fieldsNamed("   x   "))
	ae := wantErr(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
	if len(ae.Violations) != 1 || ae.Violations[0].GetField() != "profile.display_name" {
		t.Fatalf("violations = %v", ae.Violations)
	}
	_, err = h.svc.CreateProfile(ctx, auth.Principal{}, uuid.NewString(), fieldsNamed("Anon"))
	wantErr(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED)
}

func TestGetProfileVisibility(t *testing.T) {
	h := newHarness(t)
	owner := user()
	// Known from the account stream only: PENDING, not public, not even to its owner.
	if _, err := h.svc.ApplyAccountEvent(ctx, accountUpdate(owner.UserID, 1)); err != nil {
		t.Fatal(err)
	}
	_, err := h.svc.GetProfile(ctx, owner, owner.UserID.String())
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)

	h.create(owner, "Bobur")
	got, err := h.svc.GetProfile(ctx, auth.Principal{}, owner.UserID.String()) // anonymous
	if err != nil || got.DisplayName != "Bobur" || got.IsFollowedByMe {
		t.Fatalf("anonymous read = %+v, %v", got, err)
	}

	if err := h.svc.DeleteProfile(ctx, owner, ""); err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.GetProfile(ctx, auth.Principal{}, owner.UserID.String())
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	_, err = h.svc.GetProfile(ctx, auth.Principal{}, uuid.NewString())
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
}

func TestUpdateProfile(t *testing.T) {
	h := newHarness(t)
	p := user()
	created := h.create(p, "Sardor")
	avatar := uuid.New()

	h.clock.Advance(time.Minute)
	upd, err := h.svc.UpdateProfile(ctx, p, p.UserID.String(),
		domain.ProfileFields{DisplayName: "ignored", Bio: "  Bug'doy  ", AvatarMediaID: avatar, Region: commonv1.Region_REGION_XORAZM},
		[]string{domain.FieldBio, domain.FieldAvatar, domain.FieldRegion}, created.Version)
	if err != nil {
		t.Fatal(err)
	}
	if upd.DisplayName != "Sardor" || upd.Bio != "Bug'doy" || upd.AvatarMediaID != avatar || upd.Region != commonv1.Region_REGION_XORAZM ||
		upd.District != "Asaka" || upd.Version != 2 || !upd.UpdatedAt.Equal(start.Add(time.Minute)) || !upd.CreatedAt.Equal(start) {
		t.Fatalf("updated = %+v", upd)
	}

	// Stale expected_version.
	_, err = h.svc.UpdateProfile(ctx, p, p.UserID.String(), fieldsNamed("New"), []string{domain.FieldDisplayName}, 1)
	ae := wantErr(t, err, codes.Aborted, commonv1.ErrorReason_ERROR_REASON_CONFLICT)
	if ae.Metadata["current_version"] != "2" {
		t.Fatalf("metadata = %v", ae.Metadata)
	}

	// Same values: no change, no version bump. Clearing the avatar is a change.
	same, err := h.svc.UpdateProfile(ctx, p, p.UserID.String(), domain.ProfileFields{Bio: "Bug'doy"}, []string{domain.FieldBio}, 0)
	if err != nil || same.Version != 2 {
		t.Fatalf("no-op update = %+v, %v", same, err)
	}
	cleared, err := h.svc.UpdateProfile(ctx, p, p.UserID.String(), domain.ProfileFields{}, []string{domain.FieldAvatar}, 2)
	if err != nil || cleared.AvatarMediaID != uuid.Nil || cleared.Version != 3 {
		t.Fatalf("clear avatar = %+v, %v", cleared, err)
	}

	// Only the owner, only created profiles.
	other := user()
	_, err = h.svc.UpdateProfile(ctx, other, p.UserID.String(), fieldsNamed("Hijack"), []string{domain.FieldDisplayName}, 0)
	wantErr(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
	_, err = h.svc.UpdateProfile(ctx, other, other.UserID.String(), fieldsNamed("Nobody"), []string{domain.FieldDisplayName}, 0)
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
}

func TestDeleteProfile(t *testing.T) {
	h := newHarness(t)
	a, b, c := user(), user(), user()
	h.create(a, "Aziz")
	h.create(b, "Barno")
	h.create(c, "Chori")
	for _, f := range []struct{ from, to auth.Principal }{{a, b}, {b, a}, {c, a}, {a, c}} {
		if _, err := h.svc.FollowUser(ctx, f.from, f.to.UserID.String()); err != nil {
			t.Fatal(err)
		}
	}

	// Another user may not delete it; staff may; nobody may delete a PENDING one.
	err := h.svc.DeleteProfile(ctx, b, a.UserID.String())
	wantErr(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
	err = h.svc.DeleteProfile(ctx, user(), "")
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)

	admin := user(domain.RoleReseller, domain.RoleAdmin)
	if err := h.svc.DeleteProfile(ctx, admin, a.UserID.String()); err != nil {
		t.Fatal(err)
	}
	db := h.store.snapshot()
	if len(db.follows) != 0 {
		t.Fatalf("follow edges left: %v", db.follows)
	}
	gone := db.profiles[a.UserID]
	if gone.Status != domain.ProfileDeleted || gone.DeletedAt == nil || gone.Version != 2 || gone.DisplayName != "Aziz" ||
		gone.FollowerCount != 0 || gone.FollowingCount != 0 {
		t.Fatalf("deleted row = %+v", gone)
	}
	for _, id := range []uuid.UUID{b.UserID, c.UserID} {
		if p := db.profiles[id]; p.FollowerCount != 0 || p.FollowingCount != 0 {
			t.Fatalf("counters of %s not adjusted: %+v", id, p)
		}
	}
	// Deleting again is a no-op; the user can create the profile anew.
	if err := h.svc.DeleteProfile(ctx, a, ""); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	again := h.create(a, "Aziz 2")
	if !again.Public() || again.Version != 3 || again.DeletedAt != nil {
		t.Fatalf("re-created = %+v", again)
	}
}

func TestFollowAndUnfollow(t *testing.T) {
	h := newHarness(t)
	a, b := user(), user()
	h.create(a, "Aziz")
	h.create(b, "Barno")

	followed, err := h.svc.FollowUser(ctx, a, b.UserID.String())
	if err != nil {
		t.Fatal(err)
	}
	if followed.ID != b.UserID || followed.FollowerCount != 1 || !followed.IsFollowedByMe {
		t.Fatalf("followed = %+v", followed)
	}
	// Following again is a no-op: no double count.
	if again, err := h.svc.FollowUser(ctx, a, b.UserID.String()); err != nil || again.FollowerCount != 1 {
		t.Fatalf("second follow = %+v, %v", again, err)
	}
	if got := h.row(a.UserID).FollowingCount; got != 1 {
		t.Fatalf("following count = %d", got)
	}
	if seen, _ := h.svc.GetProfile(ctx, a, b.UserID.String()); !seen.IsFollowedByMe {
		t.Fatal("is_followed_by_me must be true for the follower")
	}

	_, err = h.svc.FollowUser(ctx, a, a.UserID.String())
	wantErr(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_SELF_ACTION_NOT_ALLOWED)
	_, err = h.svc.FollowUser(ctx, a, uuid.NewString())
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	_, err = h.svc.FollowUser(ctx, user(), b.UserID.String()) // the caller has no profile
	wantErr(t, err, codes.FailedPrecondition, commonv1.ErrorReason_ERROR_REASON_FAILED_PRECONDITION)
	_, err = h.svc.FollowUser(ctx, auth.Principal{}, b.UserID.String())
	wantErr(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED)

	unfollowed, err := h.svc.UnfollowUser(ctx, a, b.UserID.String())
	if err != nil || unfollowed.FollowerCount != 0 || unfollowed.IsFollowedByMe {
		t.Fatalf("unfollowed = %+v, %v", unfollowed, err)
	}
	if again, err := h.svc.UnfollowUser(ctx, a, b.UserID.String()); err != nil || again.FollowerCount != 0 {
		t.Fatalf("second unfollow = %+v, %v", again, err)
	}
	if got := h.row(a.UserID).FollowingCount; got != 0 {
		t.Fatalf("following count = %d", got)
	}
	_, err = h.svc.UnfollowUser(ctx, a, a.UserID.String())
	wantErr(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_SELF_ACTION_NOT_ALLOWED)
}

func TestFollowRateLimit(t *testing.T) {
	h := newHarness(t)
	a, b := user(), user()
	h.create(a, "Aziz")
	h.create(b, "Barno")

	h.limiter.deny, h.limiter.retryAfter = true, 90*time.Second
	_, err := h.svc.FollowUser(ctx, a, b.UserID.String())
	ae := wantErr(t, err, codes.ResourceExhausted, commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED)
	if ae.Metadata["retry_after_seconds"] != "90" || h.limiter.keys[0] != "follow:"+a.UserID.String() {
		t.Fatalf("metadata = %v, keys = %v", ae.Metadata, h.limiter.keys)
	}
	// Redis down: fail open.
	h.limiter.deny, h.limiter.err = false, errors.New("redis: connection refused")
	if _, err := h.svc.FollowUser(ctx, a, b.UserID.String()); err != nil {
		t.Fatalf("rate limiter down must fail open: %v", err)
	}
}

func TestListFollowersPagination(t *testing.T) {
	h := newHarness(t)
	star := user()
	h.create(star, "Star")
	var fans []auth.Principal
	for i := range 5 {
		f := user()
		h.create(f, "Fan "+string(rune('A'+i)))
		h.clock.Advance(time.Second)
		if _, err := h.svc.FollowUser(ctx, f, star.UserID.String()); err != nil {
			t.Fatal(err)
		}
		fans = append(fans, f)
	}
	viewer := fans[0]
	if _, err := h.svc.FollowUser(ctx, viewer, fans[4].UserID.String()); err != nil {
		t.Fatal(err)
	}

	var got []uuid.UUID
	token := ""
	for page := 0; ; page++ {
		res, err := h.svc.ListFollowers(ctx, viewer, star.UserID.String(), domain.PageRequest{Size: 2, Token: token})
		if err != nil {
			t.Fatal(err)
		}
		if res.TotalSize != 5 {
			t.Fatalf("total size = %d", res.TotalSize)
		}
		for _, p := range res.Profiles {
			got = append(got, p.ID)
			if want := p.ID == fans[4].UserID; p.IsFollowedByMe != want {
				t.Fatalf("is_followed_by_me of %s = %v", p.DisplayName, p.IsFollowedByMe)
			}
		}
		if token = res.NextPageToken; token == "" {
			if page != 2 {
				t.Fatalf("pages = %d, want 3", page+1)
			}
			break
		}
	}
	for i, id := range got { // newest first
		if id != fans[4-i].UserID {
			t.Fatalf("order = %v", got)
		}
	}

	following, err := h.svc.ListFollowing(ctx, auth.Principal{}, viewer.UserID.String(), domain.PageRequest{})
	if err != nil || len(following.Profiles) != 2 || following.Profiles[0].ID != fans[4].UserID || following.TotalSize != 2 {
		t.Fatalf("following = %+v, %v", following, err)
	}

	// Tokens belong to one list of one user.
	first, _ := h.svc.ListFollowers(ctx, viewer, star.UserID.String(), domain.PageRequest{Size: 1})
	for name, req := range map[string]func() error{
		"garbage": func() error {
			_, err := h.svc.ListFollowers(ctx, viewer, star.UserID.String(), domain.PageRequest{Token: "%%%"})
			return err
		},
		"other list": func() error {
			_, err := h.svc.ListFollowing(ctx, viewer, star.UserID.String(), domain.PageRequest{Token: first.NextPageToken})
			return err
		},
		"other user": func() error {
			_, err := h.svc.ListFollowers(ctx, viewer, viewer.UserID.String(), domain.PageRequest{Token: first.NextPageToken})
			return err
		},
	} {
		ae := wantErr(t, req(), codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
		if ae.Violations[0].GetField() != "page.page_token" {
			t.Fatalf("%s: violations = %v", name, ae.Violations)
		}
	}

	// A deleted follower leaves the list and the count.
	if err := h.svc.DeleteProfile(ctx, fans[2], ""); err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.ListFollowers(ctx, auth.Principal{}, star.UserID.String(), domain.PageRequest{Size: 10})
	if err != nil || len(res.Profiles) != 4 || res.TotalSize != 4 {
		t.Fatalf("after delete: %d profiles, total %d, %v", len(res.Profiles), res.TotalSize, err)
	}
	_, err = h.svc.ListFollowers(ctx, auth.Principal{}, fans[2].UserID.String(), domain.PageRequest{})
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
}

func TestListUserListings(t *testing.T) {
	h := newHarness(t)
	owner, stranger := user(), user()
	h.create(owner, "Owner")
	active := &profilesocialv1.ListingCard{ListingId: uuid.NewString(), Status: domain.ListingActive}
	closed := &profilesocialv1.ListingCard{ListingId: uuid.NewString(), Status: eventsv1.ListingStatus_LISTING_STATUS_CLOSED}
	h.listings.page = domain.ListingPage{Listings: []*profilesocialv1.ListingCard{active, closed}, NextPageToken: "catalog-token"}
	closedOnly := []eventsv1.ListingStatus{eventsv1.ListingStatus_LISTING_STATUS_CLOSED}

	// The owner may ask for any status; the query goes to catalog unchanged.
	page, err := h.svc.ListUserListings(ctx, owner, owner.UserID.String(), commonv1.ListingKind_LISTING_KIND_OFFER, closedOnly,
		domain.PageRequest{Size: 5, Token: "t"})
	if err != nil || len(page.Listings) != 2 || page.NextPageToken != "catalog-token" {
		t.Fatalf("owner page = %+v, %v", page, err)
	}
	if q := h.listings.got; q.OwnerID != owner.UserID || q.Kind != commonv1.ListingKind_LISTING_KIND_OFFER ||
		len(q.Statuses) != 1 || q.Page.Size != 5 || q.Page.Token != "t" || h.listings.gotP.UserID != owner.UserID {
		t.Fatalf("catalog query = %+v", q)
	}

	// Others: ACTIVE only (asking for more is denied; anything else is filtered).
	_, err = h.svc.ListUserListings(ctx, stranger, owner.UserID.String(), 0, closedOnly, domain.PageRequest{})
	wantErr(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
	page, err = h.svc.ListUserListings(ctx, auth.Principal{}, owner.UserID.String(), 0, nil, domain.PageRequest{})
	if err != nil || len(page.Listings) != 1 || page.Listings[0] != active {
		t.Fatalf("public page = %+v, %v", page, err)
	}
	// ... and only of a public profile.
	_, err = h.svc.ListUserListings(ctx, stranger, uuid.NewString(), 0, nil, domain.PageRequest{})
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)

	// catalog unavailable or not configured: an empty page, not an error.
	h.listings.err = apperr.New(commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE, "catalog is unavailable")
	h.listings.err.(*apperr.Error).Cause = domain.ErrDependencyUnavailable
	page, err = h.svc.ListUserListings(ctx, owner, owner.UserID.String(), 0, nil, domain.PageRequest{})
	if err != nil || len(page.Listings) != 0 || page.NextPageToken != "" {
		t.Fatalf("degraded page = %+v, %v", page, err)
	}
	h.listings.err = apperr.New(commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED, "bad page token")
	_, err = h.svc.ListUserListings(ctx, owner, owner.UserID.String(), 0, nil, domain.PageRequest{Token: "x"})
	wantErr(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)

	noCatalog := service.New(h.store, nil, nil, service.WithClock(h.clock.Now))
	page, err = noCatalog.ListUserListings(ctx, owner, owner.UserID.String(), 0, nil, domain.PageRequest{})
	if err != nil || page.Listings == nil || len(page.Listings) != 0 {
		t.Fatalf("no catalog = %+v, %v", page, err)
	}
}
