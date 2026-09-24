// Package auth decodes the metadata the edge forwards (see
// proto/common/v1/auth.proto) into a Principal and holds profile-social's
// authorization helpers. Authentication happens at the edge; profile-social
// trusts these headers because the edge strips client-supplied x-agrogram-*
// values.
package auth

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/grpc/metadata"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
)

// Metadata keys (gRPC metadata keys are lower case).
const (
	HeaderUserID         = "x-agrogram-user-id"
	HeaderRoles          = "x-agrogram-roles"
	HeaderPremium        = "x-agrogram-premium"
	HeaderSessionID      = "x-agrogram-session-id"
	HeaderCaller         = "x-agrogram-caller"
	HeaderRequestID      = "x-request-id"
	HeaderTraceparent    = "traceparent"
	HeaderTracestate     = "tracestate"
	HeaderAcceptLanguage = "accept-language"
)

// ServiceName is profile-social's own name: the x-agrogram-caller of its
// outgoing calls and the actor of work it does on its own.
const ServiceName = "profile-social"

var (
	callerPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	traceparentPattern = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)
	requestIDPattern   = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`) // printable ASCII, no spaces
	languagePattern    = regexp.MustCompile(`^[A-Za-z0-9,;=.* -]{1,64}$`)
)

// Principal is the in-process form of the request metadata: the signed-in
// user (common.v1.Principal), the internal caller and the request context.
type Principal struct {
	UserID    uuid.UUID // uuid.Nil: anonymous
	Roles     []commonv1.Role
	Premium   bool
	SessionID string
	Caller    string // x-agrogram-caller; set on internal calls only

	// RequestID is x-request-id, or a fresh UUIDv7 when absent or unusable.
	RequestID string
	// Traceparent and Tracestate are kept only when well formed, so they can
	// go into EventMetadata and outgoing calls unchanged.
	Traceparent string
	Tracestate  string
	// AcceptLanguage is forwarded on calls made on the user's behalf.
	AcceptLanguage string
}

// FromMetadata decodes request metadata. A present but malformed user id is
// UNAUTHENTICATED; unknown role names are ignored.
func FromMetadata(md metadata.MD) (Principal, error) {
	p := Principal{
		Premium:        strings.EqualFold(first(md, HeaderPremium), "true"),
		RequestID:      first(md, HeaderRequestID),
		Traceparent:    first(md, HeaderTraceparent),
		Tracestate:     first(md, HeaderTracestate),
		AcceptLanguage: first(md, HeaderAcceptLanguage),
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
	if !languagePattern.MatchString(p.AcceptLanguage) {
		p.AcceptLanguage = ""
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

// Staff reports whether the signed-in user is platform staff (ROLE_ADMIN).
func (p Principal) Staff() bool { return p.Authenticated() && p.HasRole(commonv1.Role_ROLE_ADMIN) }

// Actor is who performs the request, for events and audit trails: the
// signed-in user; without one, the calling service; otherwise profile-social.
func (p Principal) Actor() *commonv1.Actor {
	switch {
	case p.Authenticated():
		return UserActor(p.UserID)
	case p.Internal():
		return &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: p.Caller}}
	}
	return ServiceActor()
}

// OutgoingMetadata is the metadata of a call profile-social makes on the
// user's behalf (catalog ListListings): its own caller name, the request
// context, and the user headers forwarded unchanged (auth.proto: "they
// forward the user headers unchanged on calls made on the user's behalf").
func (p Principal) OutgoingMetadata() metadata.MD {
	md := metadata.Pairs(HeaderCaller, ServiceName, HeaderRequestID, p.RequestID)
	if p.Traceparent != "" {
		md.Set(HeaderTraceparent, p.Traceparent)
		if p.Tracestate != "" {
			md.Set(HeaderTracestate, p.Tracestate)
		}
	}
	if p.AcceptLanguage != "" {
		md.Set(HeaderAcceptLanguage, p.AcceptLanguage)
	}
	if !p.Authenticated() {
		return md
	}
	md.Set(HeaderUserID, p.UserID.String())
	names := make([]string, 0, len(p.Roles))
	for _, r := range p.Roles {
		names = append(names, strings.TrimPrefix(r.String(), "ROLE_"))
	}
	if len(names) > 0 {
		md.Set(HeaderRoles, strings.Join(names, ","))
	}
	md.Set(HeaderPremium, strconv.FormatBool(p.Premium))
	if p.SessionID != "" {
		md.Set(HeaderSessionID, p.SessionID)
	}
	return md
}

// UserActor is a user acting through the app.
func UserActor(id uuid.UUID) *commonv1.Actor {
	return &commonv1.Actor{Kind: &commonv1.Actor_UserId{UserId: id.String()}}
}

// ServiceActor is profile-social acting on its own.
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

// RequireUser returns the signed-in user, or UNAUTHENTICATED.
func RequireUser(p Principal) (uuid.UUID, error) {
	if !p.Authenticated() {
		return uuid.Nil, apperr.New(commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED,
			"a signed-in user is required ("+HeaderUserID+")")
	}
	return p.UserID, nil
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
