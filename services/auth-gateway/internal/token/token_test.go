package token

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var start = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

func ecPEM(t *testing.T, curve elliptic.Curve) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), key
}

func publicPEM(t *testing.T, pub any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func newIssuer(t *testing.T, privatePEM string, previous ...string) *Issuer {
	t.Helper()
	keys, err := LoadKeySet(privatePEM, previous)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewIssuer(keys, Config{
		Issuer: "https://auth.agrogram.local", Audience: "agrogram-api",
		TTL: 15 * time.Minute, Leeway: 0,
	}, WithClock(func() time.Time { return start }))
	if err != nil {
		t.Fatal(err)
	}
	return issuer
}

func sampleClaims() Claims {
	return Claims{
		UserID:    uuid.MustParse("0198e4d0-0000-7000-8000-000000000001"),
		SessionID: uuid.MustParse("0198e4d0-0000-7000-8000-000000000002"),
		Roles:     []string{"RESELLER", "FARMER"},
		Premium:   true,
	}
}

func TestIssueAndVerify(t *testing.T) {
	keyPEM, _ := ecPEM(t, elliptic.P256())
	issuer := newIssuer(t, keyPEM)

	raw, issued, err := issuer.Issue(sampleClaims())
	if err != nil {
		t.Fatal(err)
	}
	if !issued.ExpiresAt.Equal(start.Add(15*time.Minute)) || issued.ID == "" {
		t.Fatalf("issued claims = %+v", issued)
	}
	got, err := issuer.Verify(raw)
	if err != nil {
		t.Fatalf("Verify = %v", err)
	}
	want := sampleClaims()
	if got.UserID != want.UserID || got.SessionID != want.SessionID || !got.Premium ||
		strings.Join(got.Roles, ",") != "RESELLER,FARMER" || got.ID != issued.ID ||
		!got.ExpiresAt.Equal(issued.ExpiresAt) {
		t.Fatalf("Verify = %+v", got)
	}
}

func TestVerifyRejects(t *testing.T) {
	keyPEM, _ := ecPEM(t, elliptic.P256())
	issuer := newIssuer(t, keyPEM)
	raw, _, err := issuer.Issue(sampleClaims())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("expired", func(t *testing.T) {
		late, err := NewIssuer(mustKeySet(t, keyPEM), Config{
			Issuer: "https://auth.agrogram.local", Audience: "agrogram-api", TTL: 15 * time.Minute,
		}, WithClock(func() time.Time { return start.Add(16 * time.Minute) }))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := late.Verify(raw); !errors.Is(err, ErrExpired) {
			t.Fatalf("Verify = %v, want ErrExpired", err)
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		parts := strings.Split(raw, ".")
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		tampered := strings.Replace(string(payload), `"premium":true`, `"premium":true,"x":1`, 1)
		parts[1] = base64.RawURLEncoding.EncodeToString([]byte(tampered))
		if _, err := issuer.Verify(strings.Join(parts, ".")); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Verify = %v, want ErrInvalid", err)
		}
	})

	t.Run("signed with another key", func(t *testing.T) {
		otherPEM, _ := ecPEM(t, elliptic.P256())
		other := newIssuer(t, otherPEM)
		foreign, _, err := other.Issue(sampleClaims())
		if err != nil {
			t.Fatal(err)
		}
		// Unknown kid: the key set has never seen that key.
		if _, err := issuer.Verify(foreign); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Verify = %v, want ErrInvalid", err)
		}
	})

	t.Run("known kid, wrong signature", func(t *testing.T) {
		parts := strings.Split(raw, ".")
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}
		sig[0] ^= 0xff
		parts[2] = base64.RawURLEncoding.EncodeToString(sig)
		if _, err := issuer.Verify(strings.Join(parts, ".")); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Verify = %v, want ErrInvalid", err)
		}
	})

	t.Run("wrong issuer and audience", func(t *testing.T) {
		stranger, err := NewIssuer(mustKeySet(t, keyPEM), Config{
			Issuer: "https://evil.example", Audience: "someone-else", TTL: time.Minute,
		}, WithClock(func() time.Time { return start }))
		if err != nil {
			t.Fatal(err)
		}
		foreign, _, err := stranger.Issue(sampleClaims())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := issuer.Verify(foreign); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Verify = %v, want ErrInvalid", err)
		}
	})

	t.Run("garbage", func(t *testing.T) {
		for _, raw := range []string{"", "not.a.token", "a.b"} {
			if _, err := issuer.Verify(raw); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Verify(%q) = %v, want ErrInvalid", raw, err)
			}
		}
	})
}

// A retired key keeps verifying its tokens during a rotation, and its public
// key stays in the JWKS, but it never signs again.
func TestRotationKeepsPreviousKeys(t *testing.T) {
	oldPEM, oldKey := ecPEM(t, elliptic.P256())
	oldIssuer := newIssuer(t, oldPEM)
	oldToken, _, err := oldIssuer.Issue(sampleClaims())
	if err != nil {
		t.Fatal(err)
	}

	newPEM, newKey := ecPEM(t, elliptic.P256())
	rotated := newIssuer(t, newPEM, publicPEM(t, &oldKey.PublicKey))

	if _, err := rotated.Verify(oldToken); err != nil {
		t.Fatalf("a token from the retired key must still verify: %v", err)
	}
	fresh, _, err := rotated.Issue(sampleClaims())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldIssuer.Verify(fresh); !errors.Is(err, ErrInvalid) {
		t.Fatal("the old key set must not accept tokens from the new key")
	}
	jwks := rotated.JWKS()
	if len(jwks) != 2 {
		t.Fatalf("JWKS has %d keys, want 2", len(jwks))
	}
	newJWK := mustJWK(t, Key{Public: &newKey.PublicKey, Alg: AlgES256})
	if jwks[0].Kid != newJWK.Kid {
		t.Fatal("the active key must come first in the JWKS")
	}
}

func TestJWKSFields(t *testing.T) {
	keyPEM, key := ecPEM(t, elliptic.P256())
	issuer := newIssuer(t, keyPEM)
	jwks := issuer.JWKS()
	if len(jwks) != 1 {
		t.Fatalf("JWKS = %+v", jwks)
	}
	jwk := jwks[0]
	if jwk.Kty != "EC" || jwk.Crv != "P-256" || jwk.Alg != AlgES256 || jwk.Use != "sig" {
		t.Fatalf("JWK = %+v", jwk)
	}
	if jwk.N != "" || jwk.E != "" {
		t.Fatalf("an EC key must not carry RSA fields: %+v", jwk)
	}
	x, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil || len(x) != 32 {
		t.Fatalf("x = %q (%d bytes), %v: want 32 fixed-length octets", jwk.X, len(x), err)
	}
	y, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil || len(y) != 32 {
		t.Fatalf("y = %q (%d bytes), %v", jwk.Y, len(y), err)
	}

	// RFC 7638: kid is the base64url SHA-256 of the canonical JWK.
	canonical := `{"crv":"P-256","kty":"EC","x":"` + jwk.X + `","y":"` + jwk.Y + `"}`
	sum := sha256.Sum256([]byte(canonical))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); jwk.Kid != want {
		t.Fatalf("kid = %q, want the RFC 7638 thumbprint %q", jwk.Kid, want)
	}
	// The thumbprint only depends on the public key, so it survives a restart
	// and can be recomputed from the public PEM alone.
	again, err := ParsePublicKey(publicPEM(t, &key.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != jwk.Kid {
		t.Fatalf("kid from the public key = %q, want %q", again.ID, jwk.Kid)
	}

	// The token names the key it was signed with.
	raw, _, err := issuer.Issue(sampleClaims())
	if err != nil {
		t.Fatal(err)
	}
	header, err := base64.RawURLEncoding.DecodeString(strings.Split(raw, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(header), `"kid":"`+jwk.Kid+`"`) || !strings.Contains(string(header), `"alg":"ES256"`) {
		t.Fatalf("token header = %s", header)
	}
}

func TestRSAKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
	issuer := newIssuer(t, keyPEM)

	jwk := issuer.JWKS()[0]
	if jwk.Kty != "RSA" || jwk.Alg != AlgRS256 || jwk.N == "" || jwk.E == "" || jwk.Crv != "" {
		t.Fatalf("JWK = %+v", jwk)
	}
	raw, _, err := issuer.Issue(sampleClaims())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Verify(raw); err != nil {
		t.Fatalf("Verify = %v", err)
	}
}

func TestLoadKeySetRejects(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	smallPEM := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(small),
	}))
	ecKeyPEM, ecKey := ecPEM(t, elliptic.P256())
	for name, tc := range map[string]struct{ key, want string }{
		"empty":             {"", "empty"},
		"not a key":         {"hello", "neither PEM nor base64"},
		"a public key":      {publicPEM(t, &ecKey.PublicKey), "unsupported PEM block"},
		"rsa too small":     {smallPEM, "at least 2048"},
		"unusable ec curve": {mustP224PEM(t), "unsupported EC curve"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadKeySet(tc.key, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadKeySet = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	// A previous key must be a public key, and a bad one names its position.
	if _, err := LoadKeySet(ecKeyPEM, []string{"nonsense"}); err == nil ||
		!strings.Contains(err.Error(), "previous public key 1") {
		t.Fatalf("LoadKeySet = %v", err)
	}
}

// Containers pass the PEM base64-encoded; both forms must load identically.
func TestLoadKeySetAcceptsBase64PEM(t *testing.T) {
	keyPEM, _ := ecPEM(t, elliptic.P256())
	plain, err := LoadKeySet(keyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := LoadKeySet(base64.StdEncoding.EncodeToString([]byte(keyPEM)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Active().ID != encoded.Active().ID {
		t.Fatal("the base64 form must produce the same key")
	}
}

func TestRefreshTokenEntropy(t *testing.T) {
	a, ha, err := NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	b, hb, err := NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two refresh tokens are identical")
	}
	if raw, err := base64.RawURLEncoding.DecodeString(a); err != nil || len(raw) != RefreshTokenBytes {
		t.Fatalf("token decodes to %d bytes, %v", len(raw), err)
	}
	if len(ha) != 32 || EqualHash(ha, hb) {
		t.Fatal("digests must be 32 bytes and differ")
	}
	if !EqualHash(ha, HashRefreshToken(a)) {
		t.Fatal("HashRefreshToken is not stable")
	}
}

func mustKeySet(t *testing.T, privatePEM string, previous ...string) *KeySet {
	t.Helper()
	ks, err := LoadKeySet(privatePEM, previous)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func mustJWK(t *testing.T, k Key) JSONWebKey {
	t.Helper()
	jwk, err := k.JWK()
	if err != nil {
		t.Fatal(err)
	}
	return jwk
}

func mustP224PEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
