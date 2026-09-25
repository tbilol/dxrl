package service_test

import (
	"testing"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/service"
)

func TestGetListingVisibility(t *testing.T) {
	e := newEnv()
	owner := farmer()
	active := e.create(t, owner, domain.KindOffer)
	closed := e.create(t, owner, domain.KindOffer)
	e.setStatus(closed.ID, domain.StatusClosed)

	for name, p := range map[string]auth.Principal{
		"anonymous": {RequestID: "r"}, "other user": reseller(), "owner": owner, "staff": admin(),
	} {
		if got, err := e.svc.GetListing(ctx, p, active.ID.String()); err != nil || got.ID != active.ID {
			t.Fatalf("%s reads ACTIVE: %v", name, err)
		}
	}
	for name, p := range map[string]auth.Principal{
		"owner": owner, "staff": admin(), "order-escrow": internalCaller("order-escrow"),
	} {
		if got, err := e.svc.GetListing(ctx, p, closed.ID.String()); err != nil || got.Status != domain.StatusClosed {
			t.Fatalf("%s reads CLOSED: %+v, %v", name, got, err)
		}
	}
	for name, p := range map[string]auth.Principal{
		"anonymous": {RequestID: "r"}, "other user": reseller(), "profile-social": internalCaller("profile-social"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.GetListing(ctx, p, closed.ID.String())
			wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
		})
	}
	_, err := e.svc.GetListing(ctx, owner, uuid.NewString())
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	_, err = e.svc.GetListing(ctx, owner, "42")
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
}

func TestGetListingCountsViews(t *testing.T) {
	e := newEnv()
	owner := farmer()
	l := e.create(t, owner, domain.KindOffer)
	viewer := reseller()
	for _, p := range []auth.Principal{
		{RequestID: "anon"}, viewer, // counted
		owner, admin(), internalCaller("order-escrow"), internalCaller("profile-social"), // not counted
	} {
		if _, err := e.svc.GetListing(ctx, p, l.ID.String()); err != nil {
			t.Fatal(err)
		}
	}
	got := e.views.recorded()
	if len(got) != 2 || got[0] != [2]uuid.UUID{l.ID, uuid.Nil} || got[1] != [2]uuid.UUID{l.ID, viewer.UserID} {
		t.Fatalf("views = %v", got)
	}
	// Views never change the version.
	if cur, _ := e.svc.GetListing(ctx, owner, l.ID.String()); cur.Version != 1 {
		t.Fatalf("version = %d", cur.Version)
	}
	// Without a recorder nothing breaks.
	svc := service.New(e.store, nil, nil)
	if _, err := svc.GetListing(ctx, viewer, l.ID.String()); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateListing(t *testing.T) {
	e := newEnv()
	owner := farmer()
	l := e.create(t, owner, domain.KindOffer)

	f := domain.ListingFields{
		Price: &domain.Price{AmountMinor: 4_000_000, Currency: domain.CurrencyUZS, Unit: domain.UnitTonne},
		Notes: "  Yangi hosil  ",
		Phone: "+998000000000", // not in the mask: ignored
	}
	got, err := e.svc.UpdateListing(ctx, owner, l.ID.String(), f, []string{"price", "notes"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Notes != "Yangi hosil" || got.Price.AmountMinor != 4_000_000 || got.Price.Unit != domain.UnitTonne ||
		got.Phone != l.Phone || got.Product != l.Product || got.Kind != l.Kind || !got.UpdatedAt.After(l.UpdatedAt) ||
		!got.CreatedAt.Equal(l.CreatedAt) {
		t.Fatalf("updated = %+v", got)
	}
	db := e.store.state()
	if db.docs[l.ID].Notes != "yangi hosil" || len(db.outbox) != 1 { // TODO(events): no ListingUpdated yet
		t.Fatalf("doc = %+v, outbox = %d", db.docs[l.ID], len(db.outbox))
	}

	// The same values again change nothing.
	same, err := e.svc.UpdateListing(ctx, owner, l.ID.String(), f, []string{"notes", "price", "notes"}, 0)
	if err != nil || same.Version != 2 {
		t.Fatalf("no-op update = %+v, %v", same, err)
	}
	// Clearing the price makes it negotiable; quantity drives the remaining quantity.
	got, err = e.svc.UpdateListing(ctx, owner, l.ID.String(), domain.ListingFields{
		Quantity: domain.Mass{Grams: 5_000_000, Unit: domain.UnitTonne},
	}, []string{"price", "quantity"}, 2)
	if err != nil || got.Price != nil || got.Quantity.Grams != 5_000_000 || got.RemainingGrams != 5_000_000 || got.Version != 3 {
		t.Fatalf("cleared price = %+v, %v", got, err)
	}
	// Stale version.
	_, err = e.svc.UpdateListing(ctx, owner, l.ID.String(), domain.ListingFields{Notes: "x"}, []string{"notes"}, 2)
	if ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_CONFLICT); ae.Metadata["current_version"] != "3" {
		t.Fatalf("metadata = %v", ae.Metadata)
	}
	// A product change re-indexes the product names.
	got, err = e.svc.UpdateListing(ctx, owner, l.ID.String(), domain.ListingFields{
		Product: commonv1.ProductType_PRODUCT_TYPE_ACHCHIQ_QALAMPIR,
	}, []string{"product"}, 0)
	if err != nil || got.Product != commonv1.ProductType_PRODUCT_TYPE_ACHCHIQ_QALAMPIR {
		t.Fatalf("product change = %+v, %v", got, err)
	}
	if doc := e.store.state().docs[l.ID]; doc.Product != "achchiq qalampir острый перец achchiq qalampir achchiqqalampir" {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestUpdateListingRules(t *testing.T) {
	e := newEnv()
	owner := farmer()
	l := e.create(t, owner, domain.KindOffer)
	notes := domain.ListingFields{Notes: "boshqa"}

	_, err := e.svc.UpdateListing(ctx, reseller(), l.ID.String(), notes, []string{"notes"}, 0)
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
	_, err = e.svc.UpdateListing(ctx, admin(), l.ID.String(), notes, []string{"notes"}, 0)
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
	_, err = e.svc.UpdateListing(ctx, auth.Principal{RequestID: "r"}, l.ID.String(), notes, []string{"notes"}, 0)
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED)
	_, err = e.svc.UpdateListing(ctx, owner, uuid.NewString(), notes, []string{"notes"}, 0)
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	for _, mask := range [][]string{nil, {"status"}, {"kind"}} {
		_, err = e.svc.UpdateListing(ctx, owner, l.ID.String(), notes, mask, 0)
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
	}
	// Required fields cannot be cleared (direct callers; protovalidate checks it on the wire).
	_, err = e.svc.UpdateListing(ctx, owner, l.ID.String(), domain.ListingFields{}, []string{"phone"}, 0)
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
	// A retired product cannot be chosen...
	_, err = e.svc.UpdateListing(ctx, owner, l.ID.String(), domain.ListingFields{Product: commonv1.ProductType_PRODUCT_TYPE_BEDA},
		[]string{"product"}, 0)
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
	// ...but a listing whose product was retired later stays editable.
	e.store.update(func(d *fakeDB) {
		cur := d.listings[l.ID]
		cur.Product = commonv1.ProductType_PRODUCT_TYPE_BEDA
		d.listings[l.ID] = cur
	})
	if _, err := e.svc.UpdateListing(ctx, owner, l.ID.String(), notes, []string{"notes"}, 0); err != nil {
		t.Fatalf("edit of a listing with a retired product: %v", err)
	}

	// Only ACTIVE listings are editable; hidden ones do not leak to others.
	e.setStatus(l.ID, domain.StatusClosed)
	_, err = e.svc.UpdateListing(ctx, owner, l.ID.String(), notes, []string{"notes"}, 0)
	if ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_LISTING_NOT_ACTIVE); ae.Metadata["status"] != "CLOSED" {
		t.Fatalf("metadata = %v", ae.Metadata)
	}
	_, err = e.svc.UpdateListing(ctx, reseller(), l.ID.String(), notes, []string{"notes"}, 0)
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
}

func TestDeleteListing(t *testing.T) {
	e := newEnv()
	owner := farmer()
	l := e.create(t, owner, domain.KindOffer)

	wantReason(t, e.svc.DeleteListing(ctx, reseller(), l.ID.String(), 0), commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
	wantReason(t, e.svc.DeleteListing(ctx, auth.Principal{}, l.ID.String(), 0), commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED)
	wantReason(t, e.svc.DeleteListing(ctx, owner, uuid.NewString(), 0), commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	ae := wantReason(t, e.svc.DeleteListing(ctx, owner, l.ID.String(), 7), commonv1.ErrorReason_ERROR_REASON_CONFLICT)
	if ae.Metadata["current_version"] != "1" {
		t.Fatalf("metadata = %v", ae.Metadata)
	}

	if err := e.svc.DeleteListing(ctx, owner, l.ID.String(), 1); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.GetListing(ctx, owner, l.ID.String())
	if err != nil || got.Status != domain.StatusDeleted || got.DeletedAt == nil || got.Version != 2 {
		t.Fatalf("deleted = %+v, %v", got, err)
	}
	// A retry (same, now stale, version) is a no-op, not a conflict.
	if err := e.svc.DeleteListing(ctx, owner, l.ID.String(), 1); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if cur, _ := e.svc.GetListing(ctx, owner, l.ID.String()); cur.Version != 2 {
		t.Fatalf("version after a repeated delete = %d", cur.Version)
	}
	// Others no longer see it; the search document is untouched; no event yet.
	_, err = e.svc.GetListing(ctx, reseller(), l.ID.String())
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	wantReason(t, e.svc.DeleteListing(ctx, reseller(), l.ID.String(), 0), commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	if db := e.store.state(); db.docs[l.ID].Product == "" || len(db.outbox) != 1 { // TODO(events): no ListingDeleted
		t.Fatalf("doc = %+v, outbox = %d", db.docs[l.ID], len(db.outbox))
	}
	// Listings in other statuses can be deleted too.
	blocked := e.create(t, owner, domain.KindOffer)
	e.setStatus(blocked.ID, domain.StatusBlocked)
	if err := e.svc.DeleteListing(ctx, owner, blocked.ID.String(), 0); err != nil {
		t.Fatal(err)
	}
}

func TestListProductTypes(t *testing.T) {
	e := newEnv()
	all, err := e.svc.ListProductTypes(ctx, auth.Principal{}, 0)
	if err != nil || len(all) != len(testProducts) || all[0].Key != "bugdoy" || !all[1].Retired {
		t.Fatalf("all = %+v, %v", all, err)
	}
	veg, err := e.svc.ListProductTypes(ctx, auth.Principal{}, commonv1.ProductCategory_PRODUCT_CATEGORY_VEGETABLE)
	if err != nil || len(veg) != 2 || veg[0].Type != commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA {
		t.Fatalf("vegetables = %+v, %v", veg, err)
	}
}
