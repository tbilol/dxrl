// Package domain holds auth-gateway's business types: what it owns is
// credentials (password hashes and Google/Apple links), phone code challenges
// and refresh-token sessions. Accounts, roles and the plan belong to identity.
//
// Enum fields use the generated proto enums, so the API layer needs no
// translation; the Postgres repository stores them as readable text.
package domain

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
)

// Short names for the enum values auth-gateway works with.
const (
	MethodUnspecified   = authgatewayv1.SignInMethod_SIGN_IN_METHOD_UNSPECIFIED
	MethodGoogle        = authgatewayv1.SignInMethod_SIGN_IN_METHOD_GOOGLE
	MethodApple         = authgatewayv1.SignInMethod_SIGN_IN_METHOD_APPLE
	MethodPhonePassword = authgatewayv1.SignInMethod_SIGN_IN_METHOD_PHONE_PASSWORD

	PurposeSignUp        = authgatewayv1.PhoneCodePurpose_PHONE_CODE_PURPOSE_SIGN_UP
	PurposeResetPassword = authgatewayv1.PhoneCodePurpose_PHONE_CODE_PURPOSE_RESET_PASSWORD
	PurposeLinkPhone     = authgatewayv1.PhoneCodePurpose_PHONE_CODE_PURPOSE_LINK_PHONE

	ScopeCurrentSession = authgatewayv1.SignOutScope_SIGN_OUT_SCOPE_CURRENT_SESSION
	ScopeAllSessions    = authgatewayv1.SignOutScope_SIGN_OUT_SCOPE_ALL_SESSIONS
)

// Reasons a session stops working.
const (
	RevokedSignOut       = "SIGN_OUT"       // SignOut, current session
	RevokedSignOutAll    = "SIGN_OUT_ALL"   // SignOut, every session
	RevokedPasswordReset = "PASSWORD_RESET" // ResetPassword ends every session
	RevokedTokenReuse    = "TOKEN_REUSE"    // a used refresh token came back
)

var (
	// ErrNotFound is returned by repositories when a row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrMethodLinked is returned when the account already has a credential
	// for that method (unique (user_id, method)).
	ErrMethodLinked = errors.New("sign-in method already linked to this account")
	// ErrSubjectTaken is returned when the Google/Apple subject or the phone
	// number belongs to another account (unique (method, subject)).
	ErrSubjectTaken = errors.New("sign-in identity already belongs to an account")
)

// Credential is one sign-in method linked to an account.
type Credential struct {
	ID     uuid.UUID
	UserID uuid.UUID
	Method authgatewayv1.SignInMethod
	// Subject is the provider's stable user id (Google/Apple `sub`) or the
	// phone number in E.164 form.
	Subject string
	// PasswordHash is the argon2id PHC string; PHONE_PASSWORD only.
	PasswordHash string
	// DisplayHint is the masked identifier shown in the app.
	DisplayHint string
	LinkedAt    time.Time
	LastUsedAt  *time.Time
}

// ClientInfo describes the device a session was opened on.
type ClientInfo struct {
	Platform   authgatewayv1.ClientPlatform
	DeviceID   string
	AppVersion string
	DeviceName string
}

// Session is one sign-in on one device: the family a refresh-token chain
// belongs to.
type Session struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	Method        authgatewayv1.SignInMethod
	Client        ClientInfo
	CreatedAt     time.Time
	LastSeenAt    time.Time
	ExpiresAt     time.Time
	RevokedAt     *time.Time
	RevokedReason string
}

// Live reports whether the session still backs tokens at now.
func (s Session) Live(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// RefreshToken is one issued opaque token. Only its SHA-256 digest is stored.
type RefreshToken struct {
	ID         uuid.UUID
	SessionID  uuid.UUID
	Hash       []byte
	IssuedAt   time.Time
	ExpiresAt  time.Time
	UsedAt     *time.Time
	ReplacedBy *uuid.UUID
}

// PhoneChallenge is one outstanding SMS code. The code itself is never stored
// (CodeHash is SHA-256 of the challenge id and the code).
type PhoneChallenge struct {
	ID                uuid.UUID
	PhoneNumber       string
	Purpose           authgatewayv1.PhoneCodePurpose
	CodeHash          []byte
	Attempts          int
	MaxAttempts       int
	CreatedAt         time.Time
	ExpiresAt         time.Time
	ResendAvailableAt time.Time
	ConsumedAt        *time.Time
}

// Usable reports whether the challenge can still be answered at now.
func (c PhoneChallenge) Usable(now time.Time) bool {
	return c.ConsumedAt == nil && now.Before(c.ExpiresAt) && c.Attempts < c.MaxAttempts
}

// MaskPhone masks an E.164 Uzbek number for display:
// "+998901234567" -> "+998 90 *** ** 67". Anything shorter is masked whole.
func MaskPhone(phone string) string {
	if len(phone) != 13 || !strings.HasPrefix(phone, "+998") {
		return "***"
	}
	return phone[:4] + " " + phone[4:6] + " *** ** " + phone[11:]
}

// MaskEmail masks an e-mail address for display: "alice@gmail.com" ->
// "a***@gmail.com". Without an "@" the whole value is masked.
func MaskEmail(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" {
		return "***"
	}
	return local[:1] + "***@" + domain
}

// MaskSubject masks a provider subject that has no e-mail behind it:
// "104711920733" -> "1***33".
func MaskSubject(subject string) string {
	if len(subject) < 4 {
		return "***"
	}
	return subject[:1] + "***" + subject[len(subject)-2:]
}

// DisplayHint is the masked identifier stored with a credential.
func DisplayHint(method authgatewayv1.SignInMethod, subject, email string) string {
	switch {
	case method == MethodPhonePassword:
		return MaskPhone(subject)
	case email != "":
		return MaskEmail(email)
	default:
		return MaskSubject(subject)
	}
}
