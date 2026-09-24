// Package token issues and verifies the access tokens auth-gateway signs, and
// publishes their public keys as a JWKS document (GetJwks).
//
// Keys are ECDSA (ES256 on P-256, the default, because both Envoy and Kong
// verify it and the proto's JsonWebKey documents `kty: "EC"`, `crv: "P-256"`)
// or RSA (RS256, detected from the PEM; JsonWebKey carries n/e for it). EdDSA
// is deliberately not supported: JWKS support for OKP keys at the edge is
// uneven. A key is never generated on the fly - `make dev-key` writes one and
// the service refuses to start without it.
//
// `kid` is the RFC 7638 JWK thumbprint (base64url SHA-256 of the canonical
// JWK), so it is stable across restarts, identical everywhere the same key is
// configured, and computable by anyone holding the public key.
package token

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Signing algorithms auth-gateway can use.
const (
	AlgES256 = "ES256"
	AlgES384 = "ES384"
	AlgES512 = "ES512"
	AlgRS256 = "RS256"
)

// minRSABits is the smallest RSA key accepted (NIST SP 800-57).
const minRSABits = 2048

// Key is one signing key (Private set) or one retired verification key.
type Key struct {
	// ID is the RFC 7638 JWK thumbprint, used as the `kid` header.
	ID string
	// Alg is the JWS algorithm the key type implies.
	Alg     string
	Public  crypto.PublicKey
	Private crypto.Signer // nil on verification-only keys
}

// JSONWebKey is one public key in JWKS form (RFC 7517). Field names match
// authgatewayv1.JsonWebKey; the handler copies them over.
type JSONWebKey struct {
	Kid string
	Kty string
	Alg string
	Use string
	Crv string
	X   string
	Y   string
	N   string
	E   string
}

// KeySet is the active signing key plus every key GetJwks still publishes and
// VerifyToken still accepts (the previous keys of a rotation).
type KeySet struct {
	active Key
	byID   map[string]Key
	order  []string // publication order: active first
}

// NewKeySet returns a key set with active as the signing key. Previous keys
// are verification-only; duplicates of the active key are ignored.
func NewKeySet(active Key, previous ...Key) (*KeySet, error) {
	if active.Private == nil {
		return nil, errors.New("the active key has no private part")
	}
	ks := &KeySet{active: active, byID: map[string]Key{active.ID: active}, order: []string{active.ID}}
	for _, k := range previous {
		if _, seen := ks.byID[k.ID]; seen {
			continue
		}
		k.Private = nil // never sign with a retired key
		ks.byID[k.ID] = k
		ks.order = append(ks.order, k.ID)
	}
	return ks, nil
}

// LoadKeySet parses the configured PEMs: privatePEM signs, previousPEMs are
// public keys kept for verification during a rotation. Each value may be a
// PEM block or that block base64-encoded (containers and secret managers
// dislike newlines).
func LoadKeySet(privatePEM string, previousPEMs []string) (*KeySet, error) {
	active, err := ParsePrivateKey(privatePEM)
	if err != nil {
		return nil, err
	}
	previous := make([]Key, 0, len(previousPEMs))
	for i, p := range previousPEMs {
		k, err := ParsePublicKey(p)
		if err != nil {
			return nil, fmt.Errorf("previous public key %d: %w", i+1, err)
		}
		previous = append(previous, k)
	}
	return NewKeySet(active, previous...)
}

// Active returns the signing key.
func (ks *KeySet) Active() Key { return ks.active }

// ByID returns the key with that `kid`.
func (ks *KeySet) ByID(kid string) (Key, bool) {
	k, ok := ks.byID[kid]
	return k, ok
}

// JWKS returns the public keys, active key first, as a JWKS document body.
func (ks *KeySet) JWKS() []JSONWebKey {
	out := make([]JSONWebKey, 0, len(ks.order))
	for _, id := range ks.order {
		jwk, err := ks.byID[id].JWK()
		if err != nil {
			continue // unreachable: every stored key was parsed successfully
		}
		out = append(out, jwk)
	}
	return out
}

// ParsePrivateKey reads a PEM (or base64 PEM) private key: PKCS#8
// ("PRIVATE KEY"), SEC1 EC ("EC PRIVATE KEY") or PKCS#1 RSA
// ("RSA PRIVATE KEY").
func ParsePrivateKey(data string) (Key, error) {
	block, err := decodePEM(data)
	if err != nil {
		return Key{}, err
	}
	var parsed any
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "ENCRYPTED PRIVATE KEY":
		return Key{}, errors.New("the private key is passphrase-encrypted; decrypt it first (openssl pkcs8 -topk8 -nocrypt)")
	default:
		return Key{}, fmt.Errorf("unsupported PEM block %q: want PRIVATE KEY, EC PRIVATE KEY or RSA PRIVATE KEY", block.Type)
	}
	if err != nil {
		return Key{}, fmt.Errorf("parse private key: %w", err)
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return Key{}, fmt.Errorf("unsupported private key type %T", parsed)
	}
	key, err := newKey(signer.Public())
	if err != nil {
		return Key{}, err
	}
	key.Private = signer
	return key, nil
}

// ParsePublicKey reads a PEM (or base64 PEM) public key ("PUBLIC KEY",
// PKIX/SubjectPublicKeyInfo).
func ParsePublicKey(data string) (Key, error) {
	block, err := decodePEM(data)
	if err != nil {
		return Key{}, err
	}
	if block.Type != "PUBLIC KEY" {
		return Key{}, fmt.Errorf("unsupported PEM block %q: want PUBLIC KEY", block.Type)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return Key{}, fmt.Errorf("parse public key: %w", err)
	}
	return newKey(pub)
}

// newKey fills in the algorithm and the thumbprint for a public key.
func newKey(pub crypto.PublicKey) (Key, error) {
	k := Key{Public: pub}
	switch p := pub.(type) {
	case *ecdsa.PublicKey:
		switch p.Curve {
		case elliptic.P256():
			k.Alg = AlgES256
		case elliptic.P384():
			k.Alg = AlgES384
		case elliptic.P521():
			k.Alg = AlgES512
		default:
			return Key{}, fmt.Errorf("unsupported EC curve %s: want P-256 (ES256), P-384 or P-521", p.Params().Name)
		}
	case *rsa.PublicKey:
		if p.N.BitLen() < minRSABits {
			return Key{}, fmt.Errorf("RSA key is %d bits: want at least %d", p.N.BitLen(), minRSABits)
		}
		k.Alg = AlgRS256
	default:
		return Key{}, fmt.Errorf("unsupported key type %T: want ECDSA (ES256) or RSA (RS256)", pub)
	}
	jwk, err := k.JWK()
	if err != nil {
		return Key{}, err
	}
	k.ID = jwk.Kid
	return k, nil
}

// JWK returns the public key in JWKS form, with the RFC 7638 thumbprint as
// its `kid`.
func (k Key) JWK() (JSONWebKey, error) {
	jwk := JSONWebKey{Alg: k.Alg, Use: "sig"}
	var canonical string
	switch p := k.Public.(type) {
	case *ecdsa.PublicKey:
		size := (p.Curve.Params().BitSize + 7) / 8
		jwk.Kty, jwk.Crv = "EC", p.Curve.Params().Name
		jwk.X = b64(padLeft(p.X, size))
		jwk.Y = b64(padLeft(p.Y, size))
		// RFC 7638 section 3.2: members in lexicographic order, no whitespace.
		canonical = fmt.Sprintf(`{"crv":%q,"kty":"EC","x":%q,"y":%q}`, jwk.Crv, jwk.X, jwk.Y)
	case *rsa.PublicKey:
		jwk.Kty = "RSA"
		jwk.N = b64(p.N.Bytes())
		jwk.E = b64(big.NewInt(int64(p.E)).Bytes())
		canonical = fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`, jwk.E, jwk.N)
	default:
		return JSONWebKey{}, fmt.Errorf("unsupported key type %T", k.Public)
	}
	sum := sha256.Sum256([]byte(canonical))
	jwk.Kid = b64(sum[:])
	return jwk, nil
}

// decodePEM accepts a PEM block, or the same block base64-encoded.
func decodePEM(data string) (*pem.Block, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, errors.New("the key is empty")
	}
	if !strings.Contains(data, "-----BEGIN") {
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(data), ""))
		if err != nil {
			return nil, errors.New("the key is neither PEM nor base64-encoded PEM")
		}
		data = string(raw)
	}
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return nil, errors.New("no PEM block found in the key")
	}
	return block, nil
}

// padLeft renders a coordinate as fixed-length big-endian octets, as JWA
// requires (a short X would otherwise change the thumbprint).
func padLeft(n *big.Int, size int) []byte {
	b := n.Bytes()
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
