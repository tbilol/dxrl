// Package auth decodes the metadata the edge forwards (see
// proto/common/v1/auth.proto) into a Principal and holds auth-gateway's
// authorization rules.
//
// auth-gateway is what turns a token into these headers in the first place
// (VerifyToken), but it is itself behind the edge: SignOut, ListSignInMethods,
// LinkSignInMethod and UnlinkSignInMethod read the caller from
// `x-agrogram-user-id` / `x-agrogram-session-id`, which the edge sets after
// calling VerifyToken. Everything else on this service is callable
// anonymously (SignIn, RequestPhoneCode, SignUpWithPhone, ResetPassword,
// RefreshToken, GetJwks) or internal only (VerifyToken).
package auth

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/grpc/metadata"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
)

// Metadata keys (gRPC metadata keys are lower case).
const (
	HeaderUserID      = "x-agrogram-user-id"
	HeaderRoles       = "x-agrogram-roles"
	HeaderSessionID   = "x-agrogram-session-id"
	HeaderCaller      = "x-agrogram-caller"
	HeaderRequestID   = "x-request-id"
	HeaderTraceparent = "traceparent"
	HeaderTracestate  = "tracestate"
)

// ServiceName is auth-gateway's own name: the `x-agrogram-caller` it sends to
// identity and the actor of its own background work.
const ServiceName = "auth-gateway"

// CallerAPIGateway is the default caller allowed to call VerifyToken
// (configurable, see config.VerifyTokenCaller).
const CallerAPIGateway = "api-gateway"

var (
	callerPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	traceparentPattern = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)
	requestIDPattern   = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`) // printable ASCII, no spaces
)

// Principal is the in-process form of the request metadata: the signed-in
// user (common.v1.Principal), the internal caller and the request context.
type Principal struct {
	UserID    uuid.UUID // uuid.Nil: anonymous
	Roles     []commonv1.Role
	SessionID uuid.UUID // uuid.Nil: no session header
	Caller    string    // x-agrogram-caller; set on internal calls only

	// RequestID is x-request-id, or a fresh UUIDv7 when absent or unusable.
	RequestID string
	// Traceparent and Tracestate are kept only when well formed, so they can
	// go into EventMetadata and outgoing identity calls unchanged.
	Traceparent string
	Tracestate  string
}

// FromMetadata decodes request metadata. A present but malformed user id or
// session id is UNAUTHENTICATED; unknown role names are ignored.
func FromMetadata(md metadata.MD) (Principal, error) {
	p := Principal{
		RequestID:   first(md, HeaderRequestID),
		Traceparent: first(md, HeaderTraceparent),
		Tracestate:  first(md, HeaderTracestate),
	}
	if !requestIDPattern.MatchString(p.RequestID) {
		p.RequestID = newID()
	}
	if !traceparentPattern.MatchString(p.Traceparent) {
		p.Traceparent, p.Tracestate = "", ""
	}
	if len(p.Tracestate) > 512 {
		p.Tracestate = ""
	}
	if c := first(md, HeaderCaller); callerPattern.MatchString(c) {
		p.Caller = c
	}
	if raw := first(md, HeaderUserID); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil {
			return p, apperr.New(commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED,
				HeaderUserID+" is not a valid UUID")
		}
		p.UserID = id
	}
	if raw := first(md, HeaderSessionID); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil {
			return p, apperr.New(commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED,
				HeaderSessionID+" is not a valid UUID")
		}
		p.SessionID = id
	}
	for _, name := range strings.Split(first(md, HeaderRoles), ",") {
		name = strings.ToUpper(strings.TrimSpace(name))
		if v, ok := commonv1.Role_value["ROLE_"+name]; ok && v != 0 && !slices.Contains(p.Roles, commonv1.Role(v)) {
			p.Roles = append(p.Roles, commonv1.Role(v))
		}
	}
	return p, nil
}

// Authenticated reports whether a user is signed in.
func (p Principal) Authenticated() bool { return p.UserID != uuid.Nil }

// Internal reports whether another backend service is calling.
func (p Principal) Internal() bool { return p.Caller != "" }

// Actor is the user performing the request, for events and audit trails.
func (p Principal) Actor() *commonv1.Actor {
	if p.Authenticated() {
		return &commonv1.Actor{Kind: &commonv1.Actor_UserId{UserId: p.UserID.String()}}
	}
	return ServiceActor()
}

// ServiceActor is auth-gateway acting on its own (anonymous flows such as
// ResetPassword, where no access token identifies the caller).
func ServiceActor() *commonv1.Actor {
	return &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: ServiceName}}
}

type ctxKey struct{}

// NewContext returns ctx carrying p.
func NewContext(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the Principal stored by NewContext.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// RequireCaller allows internal calls from the named service only. An empty
// service disables the check (local development).
func RequireCaller(p Principal, service string) error {
	if service == "" || p.Caller == service {
		return nil
	}
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED,
		"only "+service+" may call this method ("+HeaderCaller+")")
}

// RequireUser returns the signed-in user, or UNAUTHENTICATED.
func RequireUser(p Principal) (uuid.UUID, error) {
	if !p.Authenticated() {
		return uuid.Nil, apperr.New(commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED,
			"a signed-in user is required ("+HeaderUserID+")")
	}
	return p.UserID, nil
}

// RequireSession returns the caller's user and session, or UNAUTHENTICATED.
// SignOut of the current session needs both.
func RequireSession(p Principal) (uuid.UUID, uuid.UUID, error) {
	userID, err := RequireUser(p)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if p.SessionID == uuid.Nil {
		return uuid.Nil, uuid.Nil, apperr.New(commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED,
			"the current session is required ("+HeaderSessionID+")")
	}
	return userID, p.SessionID, nil
}

// OutgoingMetadata is the metadata auth-gateway sends to identity: its own
// caller name plus the request context it was called with.
func (p Principal) OutgoingMetadata() metadata.MD {
	md := metadata.Pairs(HeaderCaller, ServiceName, HeaderRequestID, p.RequestID)
	if p.Traceparent != "" {
		md.Set(HeaderTraceparent, p.Traceparent)
		if p.Tracestate != "" {
			md.Set(HeaderTracestate, p.Tracestate)
		}
	}
	return md
}

func first(md metadata.MD, key string) string {
	for _, v := range md.Get(key) {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
