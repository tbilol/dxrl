// Package config loads auth-gateway's settings from AUTH_GATEWAY_*
// environment variables. There are no config files and no secrets in code.
//
// The signing key is the one setting that is never invented: without
// AUTH_GATEWAY_JWT_PRIVATE_KEY_FILE or AUTH_GATEWAY_JWT_PRIVATE_KEY the
// service refuses to start (see `make dev-key`). A silently generated key
// would mean every restart invalidates every token in the field, and two
// replicas would sign with different keys.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/password"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/sms"
)

// Prefix of every variable.
const Prefix = "AUTH_GATEWAY_"

// Config is the full service configuration. Tags name the variables without
// the prefix; README.md documents each one.
type Config struct {
	DatabaseURL      string `env:"DATABASE_URL,required,notEmpty"`
	DatabaseMaxConns int32  `env:"DATABASE_MAX_CONNS" envDefault:"10"`

	// identity owns accounts; auth-gateway cannot issue a token without it.
	IdentityAddr    string        `env:"IDENTITY_ADDR,required,notEmpty"`
	IdentityTimeout time.Duration `env:"IDENTITY_TIMEOUT" envDefault:"3s"`

	// Empty disables rate limiting (with a warning at startup).
	RedisURL string `env:"REDIS_URL"`

	// Empty disables the outbox relay: events accumulate in the outbox table.
	KafkaBrokers  []string `env:"KAFKA_BROKERS" envSeparator:","`
	KafkaClientID string   `env:"KAFKA_CLIENT_ID" envDefault:"auth-gateway"`

	GRPCAddr       string `env:"GRPC_ADDR" envDefault:":50051"`
	HTTPAddr       string `env:"HTTP_ADDR" envDefault:":8081"`
	GRPCReflection bool   `env:"GRPC_REFLECTION" envDefault:"false"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	// Access token signing. Exactly one source of the private key is needed:
	// a PEM file (host) or the PEM itself, raw or base64 (containers).
	JWTPrivateKeyFile  string        `env:"JWT_PRIVATE_KEY_FILE"`
	JWTPrivateKey      string        `env:"JWT_PRIVATE_KEY"`
	JWTPreviousPubKeys []string      `env:"JWT_PREVIOUS_PUBLIC_KEYS" envSeparator:","`
	JWTIssuer          string        `env:"JWT_ISSUER" envDefault:"https://auth.agrogram.local"`
	JWTAudience        string        `env:"JWT_AUDIENCE" envDefault:"agrogram-api"`
	AccessTokenTTL     time.Duration `env:"ACCESS_TOKEN_TTL" envDefault:"15m"`
	RefreshTokenTTL    time.Duration `env:"REFRESH_TOKEN_TTL" envDefault:"720h"`
	TokenClockSkew     time.Duration `env:"TOKEN_CLOCK_SKEW" envDefault:"30s"`

	PhoneCodeTTL         time.Duration `env:"PHONE_CODE_TTL" envDefault:"10m"`
	PhoneCodeResend      time.Duration `env:"PHONE_CODE_RESEND" envDefault:"60s"`
	PhoneCodeLength      int           `env:"PHONE_CODE_LENGTH" envDefault:"6"`
	PhoneCodeMaxAttempts int           `env:"PHONE_CODE_MAX_ATTEMPTS" envDefault:"5"`

	// STUB: a fixed SMS code, and whether codes may be logged. Development
	// only; both must stay unset everywhere else.
	DevPhoneCode     string `env:"DEV_PHONE_CODE"`
	DevLogPhoneCodes bool   `env:"DEV_LOG_PHONE_CODES" envDefault:"false"`

	PhoneCodeRateLimit  int           `env:"PHONE_CODE_RATE_LIMIT" envDefault:"5"`
	PhoneCodeRateWindow time.Duration `env:"PHONE_CODE_RATE_WINDOW" envDefault:"1h"`
	SignInRateLimit     int           `env:"SIGN_IN_RATE_LIMIT" envDefault:"10"`
	SignInRateWindow    time.Duration `env:"SIGN_IN_RATE_WINDOW" envDefault:"15m"`

	Argon2MemoryKiB   uint32 `env:"ARGON2_MEMORY_KIB" envDefault:"65536"`
	Argon2Time        uint32 `env:"ARGON2_TIME" envDefault:"3"`
	Argon2Parallelism uint8  `env:"ARGON2_PARALLELISM" envDefault:"2"`

	// VerifyToken is internal (api-gateway). Empty disables the check.
	VerifyTokenCaller string `env:"VERIFY_TOKEN_CALLER" envDefault:"api-gateway"`

	OutboxInterval   time.Duration `env:"OUTBOX_INTERVAL" envDefault:"1s"`
	OutboxBatchSize  int           `env:"OUTBOX_BATCH_SIZE" envDefault:"100"`
	OutboxMaxBackoff time.Duration `env:"OUTBOX_MAX_BACKOFF" envDefault:"30s"`
	OutboxRetention  time.Duration `env:"OUTBOX_RETENTION" envDefault:"168h"`

	HealthInterval  time.Duration `env:"HEALTH_INTERVAL" envDefault:"5s"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"20s"`
}

// Load reads and checks the configuration. The error lists every missing or
// invalid variable at once.
func Load() (Config, error) {
	cfg, err := env.ParseAsWithOptions[Config](env.Options{Prefix: Prefix})
	if err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// HTTPAddr reads only AUTH_GATEWAY_HTTP_ADDR (for the healthcheck subcommand,
// which must not need the rest of the configuration).
func HTTPAddr() (string, error) {
	c, err := env.ParseAsWithOptions[struct {
		HTTPAddr string `env:"HTTP_ADDR" envDefault:":8081"`
	}](env.Options{Prefix: Prefix})
	return c.HTTPAddr, err
}

// Argon2Params is the password hashing cost from the configuration.
func (c Config) Argon2Params() password.Params {
	p := password.DefaultParams()
	p.MemoryKiB, p.Time, p.Parallelism = c.Argon2MemoryKiB, c.Argon2Time, c.Argon2Parallelism
	return p
}

// PrivateKeyPEM returns the signing key: the file when
// AUTH_GATEWAY_JWT_PRIVATE_KEY_FILE is set, otherwise the inline value.
func (c Config) PrivateKeyPEM() (string, error) {
	if c.JWTPrivateKeyFile != "" {
		pem, err := os.ReadFile(c.JWTPrivateKeyFile)
		if err != nil {
			return "", fmt.Errorf("%sJWT_PRIVATE_KEY_FILE: %w (generate one with `make dev-key`)", Prefix, err)
		}
		return string(pem), nil
	}
	return c.JWTPrivateKey, nil
}

func (c Config) validate() error {
	var errs []error
	positive := map[string]time.Duration{
		"IDENTITY_TIMEOUT":       c.IdentityTimeout,
		"ACCESS_TOKEN_TTL":       c.AccessTokenTTL,
		"REFRESH_TOKEN_TTL":      c.RefreshTokenTTL,
		"PHONE_CODE_TTL":         c.PhoneCodeTTL,
		"PHONE_CODE_RESEND":      c.PhoneCodeResend,
		"PHONE_CODE_RATE_WINDOW": c.PhoneCodeRateWindow,
		"SIGN_IN_RATE_WINDOW":    c.SignInRateWindow,
		"OUTBOX_INTERVAL":        c.OutboxInterval,
		"OUTBOX_MAX_BACKOFF":     c.OutboxMaxBackoff,
		"HEALTH_INTERVAL":        c.HealthInterval,
		"SHUTDOWN_TIMEOUT":       c.ShutdownTimeout,
	}
	for name, d := range positive {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s%s must be positive, got %s", Prefix, name, d))
		}
	}
	if c.TokenClockSkew < 0 {
		errs = append(errs, fmt.Errorf("%sTOKEN_CLOCK_SKEW must not be negative", Prefix))
	}
	if c.OutboxRetention < 0 {
		errs = append(errs, fmt.Errorf("%sOUTBOX_RETENTION must not be negative (0 keeps rows forever)", Prefix))
	}
	if c.RefreshTokenTTL <= c.AccessTokenTTL {
		errs = append(errs, fmt.Errorf("%sREFRESH_TOKEN_TTL must be longer than %sACCESS_TOKEN_TTL", Prefix, Prefix))
	}
	if c.PhoneCodeResend > c.PhoneCodeTTL {
		errs = append(errs, fmt.Errorf("%sPHONE_CODE_RESEND must not exceed %sPHONE_CODE_TTL", Prefix, Prefix))
	}
	for name, n := range map[string]int{
		"PHONE_CODE_MAX_ATTEMPTS": c.PhoneCodeMaxAttempts,
		"PHONE_CODE_RATE_LIMIT":   c.PhoneCodeRateLimit,
		"SIGN_IN_RATE_LIMIT":      c.SignInRateLimit,
		"OUTBOX_BATCH_SIZE":       c.OutboxBatchSize,
		"DATABASE_MAX_CONNS":      int(c.DatabaseMaxConns),
	} {
		if n < 1 {
			errs = append(errs, fmt.Errorf("%s%s must be at least 1, got %d", Prefix, name, n))
		}
	}
	if c.PhoneCodeLength < sms.MinCodeLength || c.PhoneCodeLength > sms.MaxCodeLength {
		errs = append(errs, fmt.Errorf("%sPHONE_CODE_LENGTH must be between %d and %d, got %d",
			Prefix, sms.MinCodeLength, sms.MaxCodeLength, c.PhoneCodeLength))
	}
	if c.DevPhoneCode != "" && !sms.ValidCode(c.DevPhoneCode) {
		errs = append(errs, fmt.Errorf("%sDEV_PHONE_CODE must be %d-%d digits",
			Prefix, sms.MinCodeLength, sms.MaxCodeLength))
	}
	if c.JWTPrivateKeyFile == "" && c.JWTPrivateKey == "" {
		errs = append(errs, fmt.Errorf(
			"%sJWT_PRIVATE_KEY_FILE or %sJWT_PRIVATE_KEY is required: auth-gateway never generates a signing key. Run `make dev-key` and point %sJWT_PRIVATE_KEY_FILE at secrets/jwt-es256.pem",
			Prefix, Prefix, Prefix))
	}
	if c.JWTIssuer == "" {
		errs = append(errs, fmt.Errorf("%sJWT_ISSUER must not be empty", Prefix))
	}
	if c.JWTAudience == "" {
		errs = append(errs, fmt.Errorf("%sJWT_AUDIENCE must not be empty", Prefix))
	}
	if err := c.Argon2Params().Validate(); err != nil {
		errs = append(errs, fmt.Errorf("%sARGON2_*: %w", Prefix, err))
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.LogLevel)); err != nil {
		errs = append(errs, fmt.Errorf("%sLOG_LEVEL: %w", Prefix, err))
	}
	if f := strings.ToLower(c.LogFormat); f != "json" && f != "text" {
		errs = append(errs, fmt.Errorf("%sLOG_FORMAT must be json or text, got %q", Prefix, c.LogFormat))
	}
	return errors.Join(errs...)
}
