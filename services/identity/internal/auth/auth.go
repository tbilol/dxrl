// Package auth decodes the metadata the edge forwards (see
// proto/common/v1/auth.proto) into a Principal and holds identity's
// authorization rules. Authentication happens at the edge; identity trusts
// these headers because the edge strips client-supplied x-agrogram-* values.
package auth

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/grpc/metadata"

	"github.com/tbilol/dxrl/services/identity/internal/apperr"
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

// Internal callers (x-agrogram-caller) that internal-only RPCs accept.
const (
	// CallerAuthGateway is the only caller allowed to create accounts.
	CallerAuthGateway = "auth-gateway"
)

// ServiceName is identity's own name, used as the actor of background work.
const ServiceName = "identity"

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
	SessionID string
	Caller    string // x-agrogram-caller; set on internal calls only

	// RequestID is x-request-id, or a fresh UUIDv7 when absent or unusable.
	RequestID string
	// Traceparent and Tracestate are kept only when well formed, so they can
	// go into EventMetadata unchanged.
	Traceparent string
	Tracestate  string
}

// FromMetadata decodes request metadata. A present but malformed user id is
// UNAUTHENTICATED; unknown role names are ignored.
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
	if s, err := uuid.Parse(first(md, HeaderSessionID)); err == nil {
		p.SessionID = s.String()
	}
	if raw := first(md, HeaderUserID); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil {
			return p, apperr.New(commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED,
				HeaderUserID+" is not a valid UUID")
		}
		p.UserID = id
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

// HasRole reports whether the token-time role set holds r.
func (p Principal) HasRole(r commonv1.Role) bool { return slices.Contains(p.Roles, r) }

// Actor is who performs the request, for events and audit trails: the
// signed-in user; without one, the calling service; otherwise identity.
func (p Principal) Actor() *commonv1.Actor {
	switch {
	case p.Authenticated():
		return UserActor(p.UserID)
	case p.Internal():
		return &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: p.Caller}}
	}
	return ServiceActor()
}

// UserActor is a user acting through the app.
func UserActor(id uuid.UUID) *commonv1.Actor {
	return &commonv1.Actor{Kind: &commonv1.Actor_UserId{UserId: id.String()}}
}

// ServiceActor is identity acting on its own (the background worker).
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

// RequireCaller allows internal calls from the named service only.
func RequireCaller(p Principal, service string) error {
	if p.Caller != service {
		return apperr.New(commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED,
			"only "+service+" may call this method (x-agrogram-caller)")
	}
	return nil
}

// RequireUser returns the signed-in user, or UNAUTHENTICATED.
func RequireUser(p Principal) (uuid.UUID, error) {
	if !p.Authenticated() {
		return uuid.Nil, apperr.New(commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED,
			"a signed-in user is required ("+HeaderUserID+")")
	}
	return p.UserID, nil
}

// ReadTarget resolves whose data a read addresses. An empty userID means the
// caller. Users read their own data; reading another user's needs ROLE_ADMIN
// or an internal caller.
func ReadTarget(p Principal, userID string) (uuid.UUID, error) {
	if userID == "" {
		return RequireUser(p)
	}
	id, err := uuid.Parse(userID)
	if err != nil {
		return uuid.Nil, apperr.Validation(&commonv1.FieldViolation{
			Field: "user_id", Description: "value must be a valid UUID", Rule: "string.uuid",
		})
	}
	switch {
	case p.Authenticated() && id == p.UserID, p.Internal(), p.Authenticated() && p.HasRole(commonv1.Role_ROLE_ADMIN):
		return id, nil
	case !p.Authenticated():
		return uuid.Nil, apperr.New(commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED,
			"a signed-in user or an internal caller is required")
	default:
		return uuid.Nil, apperr.New(commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED,
			"reading another user's data requires ROLE_ADMIN")
	}
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
