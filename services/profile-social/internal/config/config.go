// Package config loads profile-social's settings from PROFILE_SOCIAL_*
// environment variables. There are no config files and no secrets in code.
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
const Prefix = "PROFILE_SOCIAL_"

// Config is the full service configuration. Tags name the variables without
// the prefix; README.md documents each one.
type Config struct {
	DatabaseURL      string `env:"DATABASE_URL,required,notEmpty"`
	DatabaseMaxConns int32  `env:"DATABASE_MAX_CONNS" envDefault:"10"`

	// Empty disables the follow rate limit (with a warning at startup).
	RedisURL string `env:"REDIS_URL"`

	// Empty disables the stream consumer and the outbox relay (with a
	// warning): no account updates are applied, no completed transactions are
	// recorded (RateUser stays RATING_NOT_ALLOWED) and events accumulate in
	// the outbox table. The consumer group reads both consumed streams,
	// identity's account stream and order-escrow's transaction stream, and is
	// also DeadLetter.consumer_group.
	KafkaBrokers       []string `env:"KAFKA_BROKERS" envSeparator:","`
	KafkaClientID      string   `env:"KAFKA_CLIENT_ID" envDefault:"profile-social"`
	KafkaConsumerGroup string   `env:"KAFKA_CONSUMER_GROUP" envDefault:"profilesocial"`

	// catalog gRPC address for ListUserListings; the connection is lazy.
	// catalog's local binary or container listens on 127.0.0.1:15054
	// (.env.example), catalog:50051 in compose (compose.yaml). Empty, or
	// catalog unreachable: an empty page (with a warning).
	CatalogAddr    string        `env:"CATALOG_ADDR"`
	CatalogTimeout time.Duration `env:"CATALOG_TIMEOUT" envDefault:"2s"`

	GRPCAddr       string `env:"GRPC_ADDR" envDefault:":50051"`
	HTTPAddr       string `env:"HTTP_ADDR" envDefault:":8081"`
	GRPCReflection bool   `env:"GRPC_REFLECTION" envDefault:"false"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	FollowRateLimit  int           `env:"FOLLOW_RATE_LIMIT" envDefault:"100"`
	FollowRateWindow time.Duration `env:"FOLLOW_RATE_WINDOW" envDefault:"1h"`

	ConsumerMaxAttempts     int           `env:"CONSUMER_MAX_ATTEMPTS" envDefault:"5"`
	ConsumerRetryBackoff    time.Duration `env:"CONSUMER_RETRY_BACKOFF" envDefault:"500ms"`
	ConsumerRetryBackoffMax time.Duration `env:"CONSUMER_RETRY_BACKOFF_MAX" envDefault:"30s"`
	ConsumerHandleTimeout   time.Duration `env:"CONSUMER_HANDLE_TIMEOUT" envDefault:"10s"`
	ConsumerBatchSize       int           `env:"CONSUMER_BATCH_SIZE" envDefault:"100"`
	InboxRetention          time.Duration `env:"INBOX_RETENTION" envDefault:"720h"`

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

// HTTPAddr reads only PROFILE_SOCIAL_HTTP_ADDR (for the healthcheck
// subcommand, which must not need the rest of the configuration).
func HTTPAddr() (string, error) {
	c, err := env.ParseAsWithOptions[struct {
		HTTPAddr string `env:"HTTP_ADDR" envDefault:":8081"`
	}](env.Options{Prefix: Prefix})
	return c.HTTPAddr, err
}

func (c Config) validate() error {
	var errs []error
	positive := map[string]time.Duration{
		"CATALOG_TIMEOUT":            c.CatalogTimeout,
		"FOLLOW_RATE_WINDOW":         c.FollowRateWindow,
		"CONSUMER_RETRY_BACKOFF":     c.ConsumerRetryBackoff,
		"CONSUMER_RETRY_BACKOFF_MAX": c.ConsumerRetryBackoffMax,
		"CONSUMER_HANDLE_TIMEOUT":    c.ConsumerHandleTimeout,
		"OUTBOX_INTERVAL":            c.OutboxInterval,
		"OUTBOX_MAX_BACKOFF":         c.OutboxMaxBackoff,
		"HEALTH_INTERVAL":            c.HealthInterval,
		"SHUTDOWN_TIMEOUT":           c.ShutdownTimeout,
	}
	for name, d := range positive {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s%s must be positive, got %s", Prefix, name, d))
		}
	}
	for name, d := range map[string]time.Duration{
		"OUTBOX_RETENTION": c.OutboxRetention,
		"INBOX_RETENTION":  c.InboxRetention,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s%s must not be negative (0 keeps rows forever)", Prefix, name))
		}
	}
	for name, n := range map[string]int{
		"FOLLOW_RATE_LIMIT":     c.FollowRateLimit,
		"CONSUMER_MAX_ATTEMPTS": c.ConsumerMaxAttempts,
		"CONSUMER_BATCH_SIZE":   c.ConsumerBatchSize,
		"OUTBOX_BATCH_SIZE":     c.OutboxBatchSize,
		"DATABASE_MAX_CONNS":    int(c.DatabaseMaxConns),
	} {
		if n < 1 {
			errs = append(errs, fmt.Errorf("%s%s must be at least 1, got %d", Prefix, name, n))
		}
	}
	if c.ConsumerRetryBackoffMax < c.ConsumerRetryBackoff {
		errs = append(errs, fmt.Errorf("%sCONSUMER_RETRY_BACKOFF_MAX must be >= %sCONSUMER_RETRY_BACKOFF", Prefix, Prefix))
	}
	if len(c.KafkaBrokers) > 0 && strings.TrimSpace(c.KafkaConsumerGroup) == "" {
		errs = append(errs, fmt.Errorf("%sKAFKA_CONSUMER_GROUP must not be empty", Prefix))
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
