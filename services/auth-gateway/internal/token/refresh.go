package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// RefreshTokenBytes is how much entropy an opaque refresh token carries. 32
// random bytes is the floor the design asks for; base64url makes it 43
// characters.
const RefreshTokenBytes = 32

// NewRefreshToken returns a fresh opaque refresh token and the SHA-256 digest
// stored in its place. The plaintext exists only in the response: a database
// dump cannot be replayed, and nothing logs it.
func NewRefreshToken() (string, []byte, error) {
	buf := make([]byte, RefreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("refresh token: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	return secret, HashRefreshToken(secret), nil
}

// HashRefreshToken returns the digest stored for a refresh token. SHA-256 is
// the right primitive here (unlike for passwords): the input is 256 bits of
// uniform randomness, so there is nothing to brute-force.
func HashRefreshToken(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// EqualHash compares two digests in constant time.
func EqualHash(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }
