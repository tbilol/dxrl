// Package config loads catalog's settings from CATALOG_* environment
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
const Prefix = "CATALOG_"

// Config is the full service configuration. Tags name the variables without
// the prefix; README.md documents each one.
type Config struct {
	DatabaseURL      string `env:"DATABASE_URL,required,notEmpty"`
	DatabaseMaxConns int32  `env:"DATABASE_MAX_CONNS" envDefault:"10"`

	// Empty disables the creation rate limit and view de-duplication (with a
	// warning at startup).
	RedisURL string `env:"REDIS_URL"`

	// Empty disables the outbox relay (with a warning): ListingCreated events
	// accumulate in the outbox table.
	KafkaBrokers  []string `env:"KAFKA_BROKERS" envSeparator:","`
	KafkaClientID string   `env:"KAFKA_CLIENT_ID" envDefault:"catalog"`

	GRPCAddr       string `env:"GRPC_ADDR" envDefault:":50051"`
	HTTPAddr       string `env:"HTTP_ADDR" envDefault:":8081"`
	GRPCReflection bool   `env:"GRPC_REFLECTION" envDefault:"false"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	// ACTIVE listings per user on each board (market.proto): one flat tier.
	ListingQuotaPerBoard int `env:"LISTING_QUOTA_PER_BOARD" envDefault:"10"`

	CreateRateLimit  int           `env:"CREATE_RATE_LIMIT" envDefault:"30"`
	CreateRateWindow time.Duration `env:"CREATE_RATE_WINDOW" envDefault:"1h"`

	ViewsFlushInterval time.Duration `env:"VIEWS_FLUSH_INTERVAL" envDefault:"2s"`
	ViewsBufferSize    int           `env:"VIEWS_BUFFER_SIZE" envDefault:"10000"`
	// A viewer counts once per listing per window (needs Redis); 0 counts
	// every view.
	ViewsDedupWindow time.Duration `env:"VIEWS_DEDUP_WINDOW" envDefault:"24h"`

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

// HTTPAddr reads only CATALOG_HTTP_ADDR (for the healthcheck subcommand,
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
		"CREATE_RATE_WINDOW":   c.CreateRateWindow,
		"VIEWS_FLUSH_INTERVAL": c.ViewsFlushInterval,
		"OUTBOX_INTERVAL":      c.OutboxInterval,
		"OUTBOX_MAX_BACKOFF":   c.OutboxMaxBackoff,
		"HEALTH_INTERVAL":      c.HealthInterval,
		"SHUTDOWN_TIMEOUT":     c.ShutdownTimeout,
	}
	for name, d := range positive {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s%s must be positive, got %s", Prefix, name, d))
		}
	}
	for name, d := range map[string]time.Duration{
		"OUTBOX_RETENTION":   c.OutboxRetention,
		"VIEWS_DEDUP_WINDOW": c.ViewsDedupWindow,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s%s must not be negative (0 turns it off)", Prefix, name))
		}
	}
	for name, n := range map[string]int{
		"LISTING_QUOTA_PER_BOARD": c.ListingQuotaPerBoard,
		"CREATE_RATE_LIMIT":       c.CreateRateLimit,
		"VIEWS_BUFFER_SIZE":       c.ViewsBufferSize,
		"OUTBOX_BATCH_SIZE":       c.OutboxBatchSize,
		"DATABASE_MAX_CONNS":      int(c.DatabaseMaxConns),
	} {
		if n < 1 {
			errs = append(errs, fmt.Errorf("%s%s must be at least 1, got %d", Prefix, name, n))
		}
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
