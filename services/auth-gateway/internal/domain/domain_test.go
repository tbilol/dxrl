package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Display hints are shown in the app, so they must never be the raw value.
func TestMasking(t *testing.T) {
	for name, tc := range map[string]struct{ got, want string }{
		"phone":          {MaskPhone("+998901234567"), "+998 90 *** ** 67"},
		"short phone":    {MaskPhone("+99890123"), "***"},
		"foreign phone":  {MaskPhone("+7900123456789"), "***"},
		"email":          {MaskEmail("alice@gmail.com"), "a***@gmail.com"},
		"email no local": {MaskEmail("@gmail.com"), "***"},
		"not an email":   {MaskEmail("alice"), "***"},
		"subject":        {MaskSubject("104711920733"), "1***33"},
		"short subject":  {MaskSubject("abc"), "***"},
		"google hint":    {DisplayHint(MethodGoogle, "104711920733", "alice@gmail.com"), "a***@gmail.com"},
		"apple hint":     {DisplayHint(MethodApple, "000123.abcdef", ""), "0***ef"},
		"phone hint":     {DisplayHint(MethodPhonePassword, "+998901234567", ""), "+998 90 *** ** 67"},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestSessionLive(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	revoked := now.Add(-time.Minute)
	live := Session{ID: uuid.New(), ExpiresAt: now.Add(time.Hour)}
	if !live.Live(now) {
		t.Fatal("a fresh session is live")
	}
	if (Session{ExpiresAt: now.Add(-time.Second)}).Live(now) {
		t.Fatal("an expired session is not live")
	}
	if (Session{ExpiresAt: now.Add(time.Hour), RevokedAt: &revoked}).Live(now) {
		t.Fatal("a revoked session is not live")
	}
}

func TestChallengeUsable(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	consumed := now.Add(-time.Minute)
	base := PhoneChallenge{ExpiresAt: now.Add(10 * time.Minute), MaxAttempts: 3}
	if !base.Usable(now) {
		t.Fatal("a fresh challenge is usable")
	}
	for name, c := range map[string]PhoneChallenge{
		"expired":      {ExpiresAt: now.Add(-time.Second), MaxAttempts: 3},
		"consumed":     {ExpiresAt: now.Add(time.Minute), MaxAttempts: 3, ConsumedAt: &consumed},
		"out of tries": {ExpiresAt: now.Add(time.Minute), MaxAttempts: 3, Attempts: 3},
	} {
		t.Run(name, func(t *testing.T) {
			if c.Usable(now) {
				t.Fatal("must not be usable")
			}
		})
	}
}
