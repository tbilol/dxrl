package sms

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
)

func TestNewCode(t *testing.T) {
	seen := map[string]int{}
	for range 50 {
		code, err := NewCode(6)
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 6 || !ValidCode(code) {
			t.Fatalf("code = %q", code)
		}
		seen[code]++
	}
	if len(seen) < 40 {
		t.Fatalf("only %d distinct codes out of 50: the generator is not random enough", len(seen))
	}
	for _, n := range []int{0, 3, 9} {
		if _, err := NewCode(n); err == nil {
			t.Fatalf("NewCode(%d) must fail", n)
		}
	}
}

func TestValidCode(t *testing.T) {
	for code, want := range map[string]bool{
		"1234": true, "12345678": true, "000000": true,
		"123": false, "123456789": false, "12a456": false, "": false, "12345 ": false,
	} {
		if got := ValidCode(code); got != want {
			t.Fatalf("ValidCode(%q) = %v, want %v", code, got, want)
		}
	}
}

// The digest is salted with the challenge id, so the same code stored twice
// looks different and the table cannot be scanned for a known code.
func TestHashCode(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ha, hb := HashCode(a, "123456"), HashCode(b, "123456")
	if len(ha) != 32 || bytes.Equal(ha, hb) {
		t.Fatal("digests must be 32 bytes and salted by the challenge id")
	}
	if !EqualCode(a, ha, "123456") {
		t.Fatal("the right code must match")
	}
	if EqualCode(a, ha, "123457") || EqualCode(b, ha, "123456") {
		t.Fatal("a wrong code, or the right code under another challenge, must not match")
	}
}

func TestStubSenderLogging(t *testing.T) {
	ctx := context.Background()
	const code = "424242"

	var quiet bytes.Buffer
	sender := StubSender{Log: slog.New(slog.NewTextHandler(&quiet, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	if err := sender.Send(ctx, "+998901234567", code, authgatewayv1.PhoneCodePurpose_PHONE_CODE_PURPOSE_SIGN_UP); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(quiet.String(), code) {
		t.Fatalf("the code must not be logged without the development flag: %s", quiet.String())
	}

	var loud bytes.Buffer
	dev := StubSender{Log: slog.New(slog.NewTextHandler(&loud, &slog.HandlerOptions{Level: slog.LevelDebug})), LogCodes: true}
	if err := dev.Send(ctx, "+998901234567", code, authgatewayv1.PhoneCodePurpose_PHONE_CODE_PURPOSE_SIGN_UP); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loud.String(), code) {
		t.Fatalf("with the development flag the code is printed: %s", loud.String())
	}
}
