package provider

import (
	"context"
	"errors"
	"testing"
)

func TestStubsAcceptDevTokensOnly(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		verifier Verifier
		token    string
		subject  string
		email    string
	}{
		"google":            {GoogleStub{}, "dev-google:104711920733", "104711920733", ""},
		"google with email": {GoogleStub{}, "dev-google:104711920733:alice@gmail.com", "104711920733", "alice@gmail.com"},
		"apple":             {AppleStub{}, "dev-apple:000123.abcdef", "000123.abcdef", ""},
		"apple with email":  {AppleStub{}, " dev-apple:000123.abcdef:relay@privaterelay.appleid.com ", "000123.abcdef", "relay@privaterelay.appleid.com"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := tc.verifier.Verify(ctx, tc.token, "nonce")
			if err != nil {
				t.Fatal(err)
			}
			if got.Subject != tc.subject || got.Email != tc.email {
				t.Fatalf("identity = %+v", got)
			}
		})
	}
}

// A real provider token must never be accepted by a stub.
func TestStubsRejectEverythingElse(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		verifier Verifier
		token    string
	}{
		"real google id token": {GoogleStub{}, "eyJhbGciOiJSUzI1NiIsImtpZCI6IjEyMyJ9.e30.sig"},
		"real apple token":     {AppleStub{}, "eyJraWQiOiJXNldjT0tCIn0.e30.sig"},
		"empty":                {GoogleStub{}, ""},
		"no subject":           {GoogleStub{}, "dev-google:"},
		"wrong provider":       {AppleStub{}, "dev-google:104711920733"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.verifier.Verify(ctx, tc.token, "")
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("Verify = %v, want ErrInvalidToken", err)
			}
		})
	}
}
