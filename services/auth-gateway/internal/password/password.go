// Package password hashes and verifies account passwords with argon2id
// (OWASP's first recommendation for new applications), and stores the result
// as a PHC string:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<base64 salt>$<base64 hash>
//
// The parameters travel with every hash, so raising them later keeps old
// hashes verifiable and NeedsRehash reports which ones to upgrade on the next
// successful sign-in.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrMismatch is returned when the password does not match the hash. It is a
// normal outcome, not a failure; callers turn it into INVALID_CREDENTIALS.
var ErrMismatch = errors.New("password does not match")

// Params are the argon2id cost parameters (configurable, see config).
type Params struct {
	MemoryKiB   uint32
	Time        uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultParams follow the OWASP argon2id recommendation (64 MiB, t=3, p=2)
// and hash in roughly 40-60 ms on a developer laptop.
func DefaultParams() Params {
	return Params{MemoryKiB: 64 * 1024, Time: 3, Parallelism: 2, SaltLength: 16, KeyLength: 32}
}

// Validate checks the parameters, so a misconfiguration fails at start-up
// rather than at the first sign-up.
func (p Params) Validate() error {
	var errs []error
	if p.MemoryKiB < 8*1024 {
		errs = append(errs, fmt.Errorf("argon2 memory must be at least 8192 KiB, got %d", p.MemoryKiB))
	}
	if p.Time < 1 {
		errs = append(errs, errors.New("argon2 time must be at least 1"))
	}
	if p.Parallelism < 1 {
		errs = append(errs, errors.New("argon2 parallelism must be at least 1"))
	}
	if p.SaltLength < 8 {
		errs = append(errs, fmt.Errorf("argon2 salt length must be at least 8, got %d", p.SaltLength))
	}
	if p.KeyLength < 16 {
		errs = append(errs, fmt.Errorf("argon2 key length must be at least 16, got %d", p.KeyLength))
	}
	return errors.Join(errs...)
}

// Hasher hashes with one set of parameters.
type Hasher struct{ params Params }

// New returns a Hasher. Zero-valued parameters fall back to DefaultParams.
func New(p Params) *Hasher {
	d := DefaultParams()
	if p.MemoryKiB == 0 {
		p.MemoryKiB = d.MemoryKiB
	}
	if p.Time == 0 {
		p.Time = d.Time
	}
	if p.Parallelism == 0 {
		p.Parallelism = d.Parallelism
	}
	if p.SaltLength == 0 {
		p.SaltLength = d.SaltLength
	}
	if p.KeyLength == 0 {
		p.KeyLength = d.KeyLength
	}
	return &Hasher{params: p}
}

// Params returns the hasher's parameters.
func (h *Hasher) Params() Params { return h.params }

// Hash returns the PHC string for password, with a fresh random salt.
func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password salt: %w", err)
	}
	sum := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.MemoryKiB, h.params.Parallelism, h.params.KeyLength)
	return encode(h.params, salt, sum), nil
}

// Verify checks password against a PHC string. It returns ErrMismatch when
// they differ, and a parse error when the stored value is not a hash this
// package wrote.
func Verify(phc, password string) error {
	params, salt, want, err := decode(phc)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(password), salt, params.Time, params.MemoryKiB, params.Parallelism, params.KeyLength)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether phc was made with weaker parameters than the
// hasher's, so the caller can re-hash on the next successful sign-in.
func (h *Hasher) NeedsRehash(phc string) bool {
	params, _, _, err := decode(phc)
	if err != nil {
		return true
	}
	return params.MemoryKiB < h.params.MemoryKiB || params.Time < h.params.Time ||
		params.Parallelism < h.params.Parallelism || params.KeyLength < h.params.KeyLength
}

func encode(p Params, salt, sum []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum))
}

func decode(phc string) (Params, []byte, []byte, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, errors.New("password hash is not an argon2id PHC string")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, fmt.Errorf("password hash version: %w", err)
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("unsupported argon2 version %d", version)
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Parallelism); err != nil {
		return Params{}, nil, nil, fmt.Errorf("password hash parameters: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("password hash salt: %w", err)
	}
	sum, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("password hash: %w", err)
	}
	p.SaltLength, p.KeyLength = uint32(len(salt)), uint32(len(sum))
	if p.SaltLength == 0 || p.KeyLength == 0 {
		return Params{}, nil, nil, errors.New("password hash is empty")
	}
	return p, salt, sum, nil
}
