// Package config loads identity's settings from IDENTITY_* environment
// variables. There are no config files and no secrets in code.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Prefix of every variable.
const Prefix = "IDENTITY_"

// Config is the full service configuration. Tags name the variables without
// the prefix; README.md documents each one.
type Config struct {
	DatabaseURL      string `env:"DATABASE_URL,required,notEmpty"`
	DatabaseMaxConns int32  `env:"DATABASE_MAX_CONNS" envDefault:"10"`

	// Empty disables rate limiting (with a warning at startup).
	RedisURL string `env:"REDIS_URL"`

	// Empty disables the outbox relay: events accumulate in the outbox table.
	KafkaBrokers  []string `env:"KAFKA_BROKERS" envSeparator:","`
	KafkaClientID string   `env:"KAFKA_CLIENT_ID" envDefault:"identity"`

	GRPCAddr       string `env:"GRPC_ADDR" envDefault:":50051"`
	HTTPAddr       string `env:"HTTP_ADDR" envDefault:":8081"`
	GRPCReflection bool   `env:"GRPC_REFLECTION" envDefault:"false"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	EIjaraTimeout   time.Duration `env:"EIJARA_TIMEOUT" envDefault:"3s"`
	EIjaraStubDelay time.Duration `env:"EIJARA_STUB_DELAY" envDefault:"0s"`

	VerificationRateLimit  int           `env:"VERIFICATION_RATE_LIMIT" envDefault:"5"`
	VerificationRateWindow time.Duration `env:"VERIFICATION_RATE_WINDOW" envDefault:"1h"`

	WorkerInterval  time.Duration `env:"WORKER_INTERVAL" envDefault:"30s"`
	WorkerBatchSize int           `env:"WORKER_BATCH_SIZE" envDefault:"50"`
	RetryBackoff    time.Duration `env:"VERIFICATION_RETRY_BACKOFF" envDefault:"30s"`
	RetryBackoffMax time.Duration `env:"VERIFICATION_RETRY_BACKOFF_MAX" envDefault:"1h"`

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

// HTTPAddr reads only IDENTITY_HTTP_ADDR (for the healthcheck subcommand,
// which must not need the rest of the configuration).
func HTTPAddr() (string, error) {
	c, err := env.ParseAsWithOptions[struct {
		HTTPAddr string `env:"HTTP_ADDR" envDefault:":8081"`
	}](env.Options{Prefix: Prefix})
	return c.HTTPAddr, err
}

func (c Config) validate() error {
	var errs []error
	positive := map[string]time.Duration{
		"EIJARA_TIMEOUT":                 c.EIjaraTimeout,
		"VERIFICATION_RATE_WINDOW":       c.VerificationRateWindow,
		"WORKER_INTERVAL":                c.WorkerInterval,
		"VERIFICATION_RETRY_BACKOFF":     c.RetryBackoff,
		"VERIFICATION_RETRY_BACKOFF_MAX": c.RetryBackoffMax,
		"OUTBOX_INTERVAL":                c.OutboxInterval,
		"OUTBOX_MAX_BACKOFF":             c.OutboxMaxBackoff,
		"HEALTH_INTERVAL":                c.HealthInterval,
		"SHUTDOWN_TIMEOUT":               c.ShutdownTimeout,
	}
	for name, d := range positive {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s%s must be positive, got %s", Prefix, name, d))
		}
	}
	if c.EIjaraStubDelay < 0 {
		errs = append(errs, fmt.Errorf("%sEIJARA_STUB_DELAY must not be negative", Prefix))
	}
	if c.OutboxRetention < 0 {
		errs = append(errs, fmt.Errorf("%sOUTBOX_RETENTION must not be negative (0 keeps rows forever)", Prefix))
	}
	for name, n := range map[string]int{
		"VERIFICATION_RATE_LIMIT": c.VerificationRateLimit,
		"WORKER_BATCH_SIZE":       c.WorkerBatchSize,
		"OUTBOX_BATCH_SIZE":       c.OutboxBatchSize,
		"DATABASE_MAX_CONNS":      int(c.DatabaseMaxConns),
	} {
		if n < 1 {
			errs = append(errs, fmt.Errorf("%s%s must be at least 1, got %d", Prefix, name, n))
		}
	}
	if c.RetryBackoffMax < c.RetryBackoff {
		errs = append(errs, fmt.Errorf("%sVERIFICATION_RETRY_BACKOFF_MAX must be >= %sVERIFICATION_RETRY_BACKOFF", Prefix, Prefix))
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
