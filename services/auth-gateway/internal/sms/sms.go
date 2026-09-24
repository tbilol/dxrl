// Package sms generates one-time phone codes and "delivers" them.
//
// STUB: nothing is ever sent. Codes go to the notification service, over its
// SendNotification RPC and never through Kafka (see
// proto/authgateway/v1/auth_gateway.proto and proto/events/v1/notification.proto),
// and that service does not exist yet. Sender below therefore only logs - at
// debug level, behind an explicit development flag - so a developer can
// complete the flow offline. A fixed development code (config
// AUTH_GATEWAY_DEV_PHONE_CODE) makes it work without reading logs at all.
//
// A code is a secret: it is hashed before it is stored, it never reaches
// Kafka, the outbox or a production log, and it is not echoed in any response.
package sms

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"math/big"
	"strings"

	"github.com/google/uuid"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
)

// Code length bounds; the proto accepts 4 to 8 digits.
const (
	MinCodeLength = 4
	MaxCodeLength = 8
)

// NewCode returns a random numeric code of the given length, including
// leading zeros.
func NewCode(length int) (string, error) {
	if length < MinCodeLength || length > MaxCodeLength {
		return "", fmt.Errorf("phone code length %d is outside %d-%d", length, MinCodeLength, MaxCodeLength)
	}
	digits := make([]byte, length)
	for i := range digits {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", fmt.Errorf("phone code: %w", err)
		}
		digits[i] = byte('0' + n.Int64())
	}
	return string(digits), nil
}

// HashCode returns the digest stored for a code. The challenge id salts it,
// so two challenges with the same code have different digests and the table
// cannot be scanned for a known code.
func HashCode(challengeID uuid.UUID, code string) []byte {
	h := sha256.New()
	id := challengeID // arrays are values; Sum wants a slice
	h.Write(id[:])
	h.Write([]byte(code))
	return h.Sum(nil)
}

// EqualCode compares a code with a stored digest in constant time.
func EqualCode(challengeID uuid.UUID, hash []byte, code string) bool {
	return subtle.ConstantTimeCompare(HashCode(challengeID, code), hash) == 1
}

// ValidCode reports whether s looks like a code at all (digits only, right
// length), which lets a malformed input be rejected without hashing.
func ValidCode(s string) bool {
	if len(s) < MinCodeLength || len(s) > MaxCodeLength {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// Sender delivers a code to a phone number.
type Sender interface {
	// Send must not block on anything external: it is called inline in
	// RequestPhoneCode.
	Send(ctx context.Context, phone, code string, purpose authgatewayv1.PhoneCodePurpose) error
}

// StubSender is the offline stand-in for notification's SendNotification.
//
// STUB: it sends nothing. With LogCodes it writes the code at debug level
// (development only; the log is then as sensitive as an SMS inbox).
type StubSender struct {
	Log      *slog.Logger
	LogCodes bool
}

// Send implements Sender.
//
// TODO(notification): call notification's SendNotification RPC
// (agrogram.notification.v1.NotificationService) with template
// "security.phone_code", channel SMS and the code as a parameter, once that
// service exists. The code must keep going over gRPC only: NotificationRequested
// on Kafka is the wrong channel for a secret.
func (s StubSender) Send(ctx context.Context, phone, code string, purpose authgatewayv1.PhoneCodePurpose) error {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	if s.LogCodes {
		log.DebugContext(ctx, "STUB sms: phone code not sent, printed for development only",
			"phone_number", phone, "purpose", purpose.String(), "code", code)
		return nil
	}
	// Without the development flag the code never appears anywhere.
	log.DebugContext(ctx, "STUB sms: phone code not sent (no SMS provider configured)",
		"phone_number", phone, "purpose", purpose.String())
	return nil
}
