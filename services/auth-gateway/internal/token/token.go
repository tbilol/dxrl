package token

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Verification failures, mapped by the service to TOKEN_EXPIRED and
// TOKEN_INVALID.
var (
	ErrExpired = errors.New("access token has expired")
	ErrInvalid = errors.New("access token is not valid")
)

// Claims are the access token claims. They are what the api-gateway turns
// into `x-agrogram-*` metadata, so they mirror common.v1.Principal: `sub` is
// the user, `sid` the session, `roles` the Role names without the `ROLE_`
// prefix (exactly the `x-agrogram-roles` form). There is no plan claim: one
// flat tier. Tokens issued before carry a `premium` claim, which is ignored.
type Claims struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
	Roles     []string
	ID        string // jti
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// registeredClaims is the wire form of Claims.
type registeredClaims struct {
	jwt.RegisteredClaims
	SessionID string   `json:"sid"`
	Roles     []string `json:"roles,omitempty"`
}

// Config tunes issuing and verification.
type Config struct {
	Issuer   string        // `iss`
	Audience string        // `aud`
	TTL      time.Duration // access token lifetime
	Leeway   time.Duration // clock skew allowed when verifying
}

// Issuer signs access tokens with the key set's active key and verifies them
// against every key in the set.
type Issuer struct {
	keys   *KeySet
	cfg    Config
	now    func() time.Time
	parser *jwt.Parser
}

// Option customizes an Issuer.
type Option func(*Issuer)

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(i *Issuer) { i.now = now } }

// NewIssuer returns an Issuer. The parser accepts only the algorithms the key
// set actually holds, so a token cannot pick its own (alg confusion).
func NewIssuer(keys *KeySet, cfg Config, opts ...Option) (*Issuer, error) {
	if keys == nil {
		return nil, errors.New("no signing keys")
	}
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("issuer and audience are required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 15 * time.Minute
	}
	i := &Issuer{keys: keys, cfg: cfg, now: time.Now}
	for _, o := range opts {
		o(i)
	}
	algs := make([]string, 0, len(keys.order))
	for _, id := range keys.order {
		k, _ := keys.ByID(id)
		algs = append(algs, k.Alg)
	}
	i.parser = jwt.NewParser(
		jwt.WithValidMethods(algs),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithAudience(cfg.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(cfg.Leeway),
		jwt.WithTimeFunc(func() time.Time { return i.now() }),
	)
	return i, nil
}

// TTL is the configured access token lifetime.
func (i *Issuer) TTL() time.Duration { return i.cfg.TTL }

// JWKS returns the public keys for GetJwks.
func (i *Issuer) JWKS() []JSONWebKey { return i.keys.JWKS() }

// Issue signs an access token for c. IssuedAt, ExpiresAt and ID are filled in
// when unset, and returned in the Claims so the caller can report the expiry.
func (i *Issuer) Issue(c Claims) (string, Claims, error) {
	if c.IssuedAt.IsZero() {
		c.IssuedAt = i.now().UTC()
	}
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = c.IssuedAt.Add(i.cfg.TTL)
	}
	if c.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return "", Claims{}, fmt.Errorf("token id: %w", err)
		}
		c.ID = id.String()
	}
	key := i.keys.Active()
	method := jwt.GetSigningMethod(key.Alg)
	if method == nil {
		return "", Claims{}, fmt.Errorf("unknown signing algorithm %q", key.Alg)
	}
	tok := jwt.NewWithClaims(method, registeredClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.cfg.Issuer,
			Subject:   c.UserID.String(),
			Audience:  jwt.ClaimStrings{i.cfg.Audience},
			IssuedAt:  jwt.NewNumericDate(c.IssuedAt),
			ExpiresAt: jwt.NewNumericDate(c.ExpiresAt),
			ID:        c.ID,
		},
		SessionID: c.SessionID.String(),
		Roles:     c.Roles,
	})
	tok.Header["kid"] = key.ID
	signed, err := tok.SignedString(key.Private)
	if err != nil {
		return "", Claims{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, c, nil
}

// Verify checks signature, `iss`, `aud` and expiry locally and returns the
// claims. It returns ErrExpired or ErrInvalid; the session is checked by the
// service, not here.
func (i *Issuer) Verify(raw string) (Claims, error) {
	var rc registeredClaims
	_, err := i.parser.ParseWithClaims(raw, &rc, i.keyFunc)
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return Claims{}, fmt.Errorf("%w: %w", ErrExpired, err)
	case err != nil:
		return Claims{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	userID, err := uuid.Parse(rc.Subject)
	if err != nil || userID == uuid.Nil {
		return Claims{}, fmt.Errorf("%w: sub is not a user id", ErrInvalid)
	}
	sessionID, err := uuid.Parse(rc.SessionID)
	if err != nil || sessionID == uuid.Nil {
		return Claims{}, fmt.Errorf("%w: sid is not a session id", ErrInvalid)
	}
	c := Claims{
		UserID:    userID,
		SessionID: sessionID,
		Roles:     rc.Roles,
		ID:        rc.ID,
	}
	if rc.IssuedAt != nil {
		c.IssuedAt = rc.IssuedAt.Time.UTC()
	}
	if rc.ExpiresAt != nil {
		c.ExpiresAt = rc.ExpiresAt.Time.UTC()
	}
	return c, nil
}

// keyFunc looks the signing key up by the token's `kid` and refuses a token
// whose algorithm does not match that key.
func (i *Issuer) keyFunc(t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)
	if kid == "" {
		return nil, errors.New("token has no kid header")
	}
	key, ok := i.keys.ByID(kid)
	if !ok {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	if t.Method.Alg() != key.Alg {
		return nil, fmt.Errorf("token alg %q does not match key alg %q", t.Method.Alg(), key.Alg)
	}
	return key.Public, nil
}
