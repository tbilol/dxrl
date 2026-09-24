package auth

import (
	"slices"
	"testing"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
)

const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

func TestFromMetadata(t *testing.T) {
	user, session := uuid.New(), uuid.New()
	p, err := FromMetadata(metadata.New(map[string]string{
		HeaderUserID:      user.String(),
		HeaderRoles:       " reseller, FARMER ,bogus,,FARMER",
		HeaderPremium:     "TRUE",
		HeaderSessionID:   session.String(),
		HeaderCaller:      "api-gateway",
		HeaderRequestID:   "req-123",
		HeaderTraceparent: traceparent,
		HeaderTracestate:  "vendor=1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Principal{
		UserID: user, Roles: []commonv1.Role{commonv1.Role_ROLE_RESELLER, commonv1.Role_ROLE_FARMER},
		Premium: true, SessionID: session, Caller: "api-gateway", RequestID: "req-123",
		Traceparent: traceparent, Tracestate: "vendor=1",
	}
	if p.UserID != want.UserID || !slices.Equal(p.Roles, want.Roles) || p.Premium != want.Premium ||
		p.SessionID != want.SessionID || p.Caller != want.Caller || p.RequestID != want.RequestID ||
		p.Traceparent != want.Traceparent || p.Tracestate != want.Tracestate {
		t.Fatalf("got %+v\nwant %+v", p, want)
	}
	if !p.Authenticated() || !p.Internal() {
		t.Fatalf("helpers disagree: %+v", p)
	}
	if p.Actor().GetUserId() != user.String() {
		t.Fatalf("actor = %v", p.Actor())
	}
	if gotUser, gotSession, err := RequireSession(p); err != nil || gotUser != user || gotSession != session {
		t.Fatalf("RequireSession = %v, %v, %v", gotUser, gotSession, err)
	}
}

func TestFromMetadataAnonymousAndSanitized(t *testing.T) {
	p, err := FromMetadata(metadata.New(map[string]string{
		HeaderTraceparent: "not-a-traceparent",
		HeaderTracestate:  "vendor=1",
		HeaderCaller:      "Not A Service",
		HeaderRequestID:   "has spaces",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Authenticated() || p.Internal() || p.SessionID != uuid.Nil {
		t.Fatalf("want an anonymous principal, got %+v", p)
	}
	if p.Traceparent != "" || p.Tracestate != "" {
		t.Fatalf("a malformed traceparent must be dropped, with its tracestate: %+v", p)
	}
	if _, err := uuid.Parse(p.RequestID); err != nil {
		t.Fatalf("request id = %q, want a generated UUID", p.RequestID)
	}
	// An anonymous caller acts as the service itself (ResetPassword events).
	if p.Actor().GetService() != ServiceName {
		t.Fatalf("actor = %v", p.Actor())
	}
}

func TestFromMetadataRejectsBadIDs(t *testing.T) {
	for name, md := range map[string]metadata.MD{
		"user id":    metadata.Pairs(HeaderUserID, "nope"),
		"nil user":   metadata.Pairs(HeaderUserID, uuid.Nil.String()),
		"session id": metadata.Pairs(HeaderUserID, uuid.NewString(), HeaderSessionID, "nope"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := FromMetadata(md)
			ae := apperr.From(err)
			if ae == nil || ae.Code != codes.Unauthenticated {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRequireUserAndSession(t *testing.T) {
	if _, err := RequireUser(Principal{}); apperr.From(err).Code != codes.Unauthenticated {
		t.Fatalf("RequireUser(anonymous) = %v", err)
	}
	// A user without a session header cannot sign out of "the current session".
	if _, _, err := RequireSession(Principal{UserID: uuid.New()}); apperr.From(err).Code != codes.Unauthenticated {
		t.Fatalf("RequireSession without a session = %v", err)
	}
}

func TestRequireCaller(t *testing.T) {
	if err := RequireCaller(Principal{Caller: CallerAPIGateway}, CallerAPIGateway); err != nil {
		t.Fatalf("RequireCaller = %v", err)
	}
	if err := RequireCaller(Principal{Caller: "catalog"}, CallerAPIGateway); apperr.From(err).Code != codes.PermissionDenied {
		t.Fatalf("RequireCaller(other service) = %v", err)
	}
	if err := RequireCaller(Principal{}, CallerAPIGateway); apperr.From(err).Code != codes.PermissionDenied {
		t.Fatalf("RequireCaller(anonymous) = %v", err)
	}
	// An empty expectation disables the check (local development).
	if err := RequireCaller(Principal{}, ""); err != nil {
		t.Fatalf("RequireCaller with no expectation = %v", err)
	}
}

// Every call to identity must name auth-gateway and carry the request context.
func TestOutgoingMetadata(t *testing.T) {
	p := Principal{RequestID: "req-1", Traceparent: traceparent, Tracestate: "vendor=1"}
	md := p.OutgoingMetadata()
	if got := md.Get(HeaderCaller); len(got) != 1 || got[0] != ServiceName {
		t.Fatalf("%s = %v", HeaderCaller, got)
	}
	if got := md.Get(HeaderRequestID); len(got) != 1 || got[0] != "req-1" {
		t.Fatalf("%s = %v", HeaderRequestID, got)
	}
	if got := md.Get(HeaderTraceparent); len(got) != 1 || got[0] != traceparent {
		t.Fatalf("%s = %v", HeaderTraceparent, got)
	}
	if got := md.Get(HeaderTracestate); len(got) != 1 || got[0] != "vendor=1" {
		t.Fatalf("%s = %v", HeaderTracestate, got)
	}
	// Without trace context, no empty headers are sent.
	if md := (Principal{RequestID: "req-2"}).OutgoingMetadata(); len(md.Get(HeaderTraceparent)) != 0 {
		t.Fatalf("traceparent = %v, want none", md.Get(HeaderTraceparent))
	}
}

func TestContextRoundTrip(t *testing.T) {
	p := Principal{UserID: uuid.New(), RequestID: "req-1"}
	got, ok := FromContext(NewContext(t.Context(), p))
	if !ok || got.UserID != p.UserID {
		t.Fatalf("FromContext = %+v, %v", got, ok)
	}
	if _, ok := FromContext(t.Context()); ok {
		t.Fatal("an empty context must hold no principal")
	}
}
