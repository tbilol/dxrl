package password

import (
	"strings"
	"testing"
)

// Small parameters keep the tests fast; production values come from config.
func testHasher() *Hasher {
	return New(Params{MemoryKiB: 8 * 1024, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
}

func TestHashVerifyRoundTrip(t *testing.T) {
	h := testHasher()
	const pw = "ozbekiston-2026!"
	phc, err := h.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("PHC string = %q", phc)
	}
	if err := Verify(phc, pw); err != nil {
		t.Fatalf("Verify = %v", err)
	}
	// The salt is random, so the same password hashes differently every time.
	other, err := h.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	if other == phc {
		t.Fatal("two hashes of the same password are identical: the salt is not random")
	}
	if err := Verify(other, pw); err != nil {
		t.Fatalf("Verify (second hash) = %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	h := testHasher()
	phc, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	for name, cases := range map[string]struct {
		hash, password string
	}{
		"wrong password": {phc, "correct horse battery stapl"},
		"empty password": {phc, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Verify(cases.hash, cases.password); err != ErrMismatch {
				t.Fatalf("Verify = %v, want ErrMismatch", err)
			}
		})
	}

	// A hash this package did not write is a parse error, not a mismatch:
	// something is wrong with the row, and the service must not read that as
	// "wrong password".
	for name, broken := range map[string]string{
		"empty":            "",
		"bcrypt":           "$2y$10$abcdefghijklmnopqrstuv",
		"wrong algorithm":  "$argon2i$v=19$m=8192,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
		"wrong version":    "$argon2id$v=16$m=8192,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
		"bad parameters":   "$argon2id$v=19$m=x,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
		"bad base64 salt":  "$argon2id$v=19$m=8192,t=1,p=1$!!!$aGFzaA",
		"truncated fields": "$argon2id$v=19$m=8192,t=1,p=1$c2FsdA",
	} {
		t.Run(name, func(t *testing.T) {
			err := Verify(broken, "whatever")
			if err == nil || err == ErrMismatch {
				t.Fatalf("Verify = %v, want a parse error", err)
			}
		})
	}
}

func TestTamperedHashFails(t *testing.T) {
	h := testHasher()
	phc, err := h.Hash("s3cret-pass")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(phc, "$")
	// Flip the last character of the digest.
	last := parts[5]
	flipped := last[:len(last)-1] + string(rune('A'+(last[len(last)-1]+1)%26))
	parts[5] = flipped
	if err := Verify(strings.Join(parts, "$"), "s3cret-pass"); err != ErrMismatch {
		t.Fatalf("Verify of a tampered hash = %v, want ErrMismatch", err)
	}
}

func TestNeedsRehash(t *testing.T) {
	weak := New(Params{MemoryKiB: 8 * 1024, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	strong := New(Params{MemoryKiB: 16 * 1024, Time: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	phc, err := weak.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	if weak.NeedsRehash(phc) {
		t.Fatal("a hash with the current parameters does not need a rehash")
	}
	if !strong.NeedsRehash(phc) {
		t.Fatal("a hash with weaker parameters needs a rehash")
	}
	if !strong.NeedsRehash("not a hash") {
		t.Fatal("an unreadable hash needs a rehash")
	}
}

func TestParamsValidate(t *testing.T) {
	if err := DefaultParams().Validate(); err != nil {
		t.Fatalf("the defaults must be valid: %v", err)
	}
	err := Params{MemoryKiB: 1, Time: 0, Parallelism: 0, SaltLength: 1, KeyLength: 1}.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"memory", "time", "parallelism", "salt", "key length"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestNewFillsZeroParams(t *testing.T) {
	if got, want := New(Params{}).Params(), DefaultParams(); got != want {
		t.Fatalf("New(Params{}) = %+v, want %+v", got, want)
	}
}
