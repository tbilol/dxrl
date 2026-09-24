// Package provider turns a Google ID token or an Apple identity token into
// the account identity behind it.
//
// STUB: neither verifier talks to Google or Apple. Both accept a documented
// development token instead, so the sign-in flows can be exercised offline
// and in tests; every real token is rejected. The comments on GoogleStub and
// AppleStub describe the verification each one owes.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Identity is what a provider knows about the user signing in.
type Identity struct {
	// Subject is the provider's stable user id (the `sub` claim). It is the
	// only field a credential is keyed by: e-mail addresses change.
	Subject string
	// Email is used for the masked display hint only; it may be empty.
	Email string
}

// ErrInvalidToken is returned for a token the provider does not accept. The
// service turns it into INVALID_CREDENTIALS, never leaking the reason.
var ErrInvalidToken = errors.New("provider token is not valid")

// Verifier checks a provider credential. Implementations must honour ctx: the
// service calls them with a short timeout.
type Verifier interface {
	// Verify checks the token (and the nonce the app sent, when the provider
	// echoes it) and returns the identity behind it.
	Verify(ctx context.Context, token, nonce string) (Identity, error)
}

// Dev token prefixes, accepted by the stubs only.
const (
	DevGooglePrefix = "dev-google:"
	DevApplePrefix  = "dev-apple:"
)

// GoogleStub accepts development tokens of the form
// "dev-google:<subject>" or "dev-google:<subject>:<email>".
//
// STUB: the real implementation fetches Google's JWKS
// (https://www.googleapis.com/oauth2/v3/certs, cached by its Cache-Control
// max-age), verifies the ID token's RS256 signature against the key named by
// `kid`, and checks `iss` in {accounts.google.com,
// https://accounts.google.com}, `aud` == AGROGRAM's OAuth client id (one per
// platform: web, Android, iOS), `exp`/`iat` within the allowed skew, and
// `nonce` == the value the app generated. `sub` is the account key, and
// `email` is only used for the display hint (`email_verified` must be true).
type GoogleStub struct{}

// Verify implements Verifier.
func (GoogleStub) Verify(_ context.Context, token, _ string) (Identity, error) {
	return parseDevToken(DevGooglePrefix, "google", token)
}

// AppleStub accepts development tokens of the form
// "dev-apple:<subject>" or "dev-apple:<subject>:<email>".
//
// STUB: the real implementation fetches Apple's JWKS
// (https://appleid.apple.com/auth/keys), verifies the identity token's RS256
// signature, and checks `iss` == https://appleid.apple.com, `aud` ==
// AGROGRAM's Services ID / bundle id, `exp`, and `nonce` == SHA-256 of the raw
// nonce the app sent (Apple hashes it). It also exchanges
// `authorization_code` at https://appleid.apple.com/auth/token for the Apple
// refresh token, which is what lets us revoke the link when the account is
// deleted - Apple requires that. `sub` is the account key; the e-mail (often
// a private relay address) arrives only on the first sign-in, so it is stored
// then or never.
type AppleStub struct{}

// Verify implements Verifier.
func (AppleStub) Verify(_ context.Context, token, _ string) (Identity, error) {
	return parseDevToken(DevApplePrefix, "apple", token)
}

// parseDevToken reads "<prefix><subject>[:<email>]". Anything else is
// rejected, so a real provider token cannot slip through a stub.
func parseDevToken(prefix, name, token string) (Identity, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(token), prefix)
	if !ok {
		return Identity{}, fmt.Errorf("%w: the %s verifier is a stub and only accepts %q tokens",
			ErrInvalidToken, name, prefix)
	}
	subject, email, _ := strings.Cut(rest, ":")
	if subject = strings.TrimSpace(subject); subject == "" {
		return Identity{}, fmt.Errorf("%w: %s token has no subject", ErrInvalidToken, name)
	}
	return Identity{Subject: subject, Email: strings.TrimSpace(email)}, nil
}
