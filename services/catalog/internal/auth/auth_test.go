package auth

import (
	"slices"
	"testing"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
)

const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

func TestFromMetadata(t *testing.T) {
	user, session := uuid.New(), uuid.New()
	p, err := FromMetadata(metadata.New(map[string]string{
		HeaderUserID:         user.String(),
		HeaderRoles:          " admin, ADMIN ,bogus,,ADMIN",
		HeaderSessionID:      session.String(),
		HeaderCaller:         "api-gateway",
		HeaderRequestID:      "req-123",
		HeaderTraceparent:    traceparent,
		HeaderTracestate:     "vendor=1",
		HeaderAcceptLanguage: "ru",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != user || !slices.Equal(p.Roles, []commonv1.Role{commonv1.Role_ROLE_ADMIN}) ||
		p.SessionID != session.String() || p.Caller != "api-gateway" || p.RequestID != "req-123" ||
		p.Traceparent != traceparent || p.Tracestate != "vendor=1" || p.AcceptLanguage != "ru" {
		t.Fatalf("got %+v", p)
	}
	if !p.Authenticated() || !p.Internal() || !p.HasRole(commonv1.Role_ROLE_ADMIN) || !p.Staff() {
		t.Fatalf("helpers disagree: %+v", p)
	}
	if p.Actor().GetUserId() != user.String() {
		t.Fatalf("actor = %v", p.Actor())
	}
}

func TestFromMetadataAnonymousAndSanitized(t *testing.T) {
	p, err := FromMetadata(metadata.New(map[string]string{
		HeaderTraceparent:    "not-a-traceparent",
		HeaderTracestate:     "vendor=1",
		HeaderCaller:         "Not A Service",
		HeaderRequestID:      "has spaces",
		HeaderSessionID:      "nope",
		HeaderAcceptLanguage: "uz\n<script>",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Authenticated() || p.Internal() || p.Traceparent != "" || p.Tracestate != "" || p.SessionID != "" ||
		p.AcceptLanguage != "" {
		t.Fatalf("got %+v", p)
	}
	if uuid.Validate(p.RequestID) != nil {
		t.Fatalf("request id %q was not regenerated", p.RequestID)
	}
	if p.Actor().GetService() != ServiceName {
		t.Fatalf("anonymous actor = %v", p.Actor())
	}
}

func TestFromMetadataRejectsBadUserID(t *testing.T) {
	for _, bad := range []string{"42", uuid.Nil.String()} {
		_, err := FromMetadata(metadata.Pairs(HeaderUserID, bad))
		if ae := apperr.From(err); ae == nil || ae.Code != codes.Unauthenticated {
			t.Fatalf("%q: err = %v", bad, err)
		}
	}
}

func TestStaffAndRequireUser(t *testing.T) {
	admin := Principal{UserID: uuid.New(), Roles: []commonv1.Role{commonv1.Role_ROLE_RESELLER, commonv1.Role_ROLE_ADMIN}}
	if !admin.Staff() {
		t.Fatal("ROLE_ADMIN is staff")
	}
	if (Principal{Roles: []commonv1.Role{commonv1.Role_ROLE_ADMIN}}).Staff() {
		t.Fatal("an anonymous call is never staff, whatever the roles header says")
	}
	if id, err := RequireUser(admin); err != nil || id != admin.UserID {
		t.Fatalf("RequireUser = %v, %v", id, err)
	}
	if _, err := RequireUser(Principal{}); apperr.From(err).Code != codes.Unauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
}

func TestActorOfInternalCall(t *testing.T) {
	if got := (Principal{Caller: "order-escrow"}).Actor().GetService(); got != "order-escrow" {
		t.Fatalf("actor = %q", got)
	}
}
