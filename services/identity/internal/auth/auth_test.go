package auth

import (
	"slices"
	"testing"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/tbilol/dxrl/services/identity/internal/apperr"
)

const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

func TestFromMetadata(t *testing.T) {
	user, session := uuid.New(), uuid.New()
	p, err := FromMetadata(metadata.New(map[string]string{
		HeaderUserID:      user.String(),
		HeaderRoles:       " reseller, FARMER ,bogus,,FARMER",
		HeaderPremium:     "TRUE",
		HeaderSessionID:   session.String(),
		HeaderCaller:      "auth-gateway",
		HeaderRequestID:   "req-123",
		HeaderTraceparent: traceparent,
		HeaderTracestate:  "vendor=1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Principal{
		UserID: user, Roles: []commonv1.Role{commonv1.Role_ROLE_RESELLER, commonv1.Role_ROLE_FARMER},
		Premium: true, SessionID: session.String(), Caller: "auth-gateway", RequestID: "req-123",
		Traceparent: traceparent, Tracestate: "vendor=1",
	}
	if p.UserID != want.UserID || !slices.Equal(p.Roles, want.Roles) || p.Premium != want.Premium ||
		p.SessionID != want.SessionID || p.Caller != want.Caller || p.RequestID != want.RequestID ||
		p.Traceparent != want.Traceparent || p.Tracestate != want.Tracestate {
		t.Fatalf("got %+v\nwant %+v", p, want)
	}
	if !p.Authenticated() || !p.Internal() || !p.HasRole(commonv1.Role_ROLE_FARMER) || p.HasRole(commonv1.Role_ROLE_ADMIN) {
		t.Fatalf("helpers disagree: %+v", p)
	}
	if p.Actor().GetUserId() != user.String() {
		t.Fatalf("actor = %v", p.Actor())
	}
}

func TestFromMetadataAnonymousAndSanitized(t *testing.T) {
	p, err := FromMetadata(metadata.New(map[string]string{
		HeaderTraceparent: "not-a-traceparent",
		HeaderTracestate:  "vendor=1",
		HeaderCaller:      "Not A Service",
		HeaderRequestID:   "has spaces",
		HeaderSessionID:   "nope",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Authenticated() || p.Internal() || p.Traceparent != "" || p.Tracestate != "" || p.SessionID != "" || p.Premium {
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
			t.Fatalf("%q: got %v", bad, err)
		}
	}
}

func TestAuthorizationRules(t *testing.T) {
	self, other := uuid.New(), uuid.New()
	user := Principal{UserID: self}
	admin := Principal{UserID: self, Roles: []commonv1.Role{commonv1.Role_ROLE_ADMIN}}
	internal := Principal{Caller: "order-escrow"}
	adminWithoutUser := Principal{Roles: []commonv1.Role{commonv1.Role_ROLE_ADMIN}}

	tests := []struct {
		name   string
		p      Principal
		userID string
		want   uuid.UUID
		code   codes.Code
	}{
		{"own, implicit", user, "", self, codes.OK},
		{"own, explicit", user, self.String(), self, codes.OK},
		{"other as user", user, other.String(), uuid.Nil, codes.PermissionDenied},
		{"other as admin", admin, other.String(), other, codes.OK},
		{"other as internal caller", internal, other.String(), other, codes.OK},
		{"anonymous implicit", Principal{}, "", uuid.Nil, codes.Unauthenticated},
		{"anonymous explicit", Principal{}, other.String(), uuid.Nil, codes.Unauthenticated},
		{"admin role without user", adminWithoutUser, other.String(), uuid.Nil, codes.Unauthenticated},
		{"internal caller, implicit", internal, "", uuid.Nil, codes.Unauthenticated},
		{"bad id", user, "x", uuid.Nil, codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadTarget(tt.p, tt.userID)
			code := codes.OK
			if err != nil {
				code = apperr.From(err).Code
			}
			if code != tt.code || got != tt.want {
				t.Fatalf("ReadTarget = %s, %s; want %s, %s", got, code, tt.want, tt.code)
			}
		})
	}

	if err := RequireCaller(Principal{Caller: CallerAuthGateway}, CallerAuthGateway); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Principal{user, admin, internal} {
		if ae := apperr.From(RequireCaller(p, CallerAuthGateway)); ae == nil || ae.Code != codes.PermissionDenied {
			t.Fatalf("RequireCaller(%+v) = %v", p, ae)
		}
	}
}
