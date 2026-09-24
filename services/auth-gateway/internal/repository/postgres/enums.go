package postgres

import (
	"fmt"

	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
)

// enumMap maps proto enum values to the readable text the CHECK constraints
// in migrations/ allow (the value name without the enum prefix).
type enumMap[E comparable] struct {
	name   string
	toDB   map[E]string
	fromDB map[string]E
}

func newEnumMap[E comparable](name string, m map[E]string) enumMap[E] {
	rev := make(map[string]E, len(m))
	for k, v := range m {
		rev[v] = k
	}
	return enumMap[E]{name: name, toDB: m, fromDB: rev}
}

func (m enumMap[E]) text(v E) (string, error) {
	s, ok := m.toDB[v]
	if !ok {
		return "", fmt.Errorf("%s %v cannot be stored", m.name, v)
	}
	return s, nil
}

func (m enumMap[E]) value(s string) (E, error) {
	v, ok := m.fromDB[s]
	if !ok {
		var zero E
		return zero, fmt.Errorf("unknown %s %q in database", m.name, s)
	}
	return v, nil
}

var (
	methods = newEnumMap("sign-in method", map[authgatewayv1.SignInMethod]string{
		domain.MethodGoogle:        "GOOGLE",
		domain.MethodApple:         "APPLE",
		domain.MethodPhonePassword: "PHONE_PASSWORD",
	})
	purposes = newEnumMap("phone code purpose", map[authgatewayv1.PhoneCodePurpose]string{
		domain.PurposeSignUp:        "SIGN_UP",
		domain.PurposeResetPassword: "RESET_PASSWORD",
		domain.PurposeLinkPhone:     "LINK_PHONE",
	})
	platforms = newEnumMap("client platform", map[authgatewayv1.ClientPlatform]string{
		authgatewayv1.ClientPlatform_CLIENT_PLATFORM_UNSPECIFIED: "UNSPECIFIED",
		authgatewayv1.ClientPlatform_CLIENT_PLATFORM_WEB:         "WEB",
		authgatewayv1.ClientPlatform_CLIENT_PLATFORM_ANDROID:     "ANDROID",
		authgatewayv1.ClientPlatform_CLIENT_PLATFORM_IOS:         "IOS",
	})
)
