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

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
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

	// Empty disables the outbox relay and the stream consumer (with a
	// warning): ListingCreated events accumulate in the outbox table, and no
	// escrow transaction is seen, so remaining quantities never drop, no
	// listing becomes SOLD_OUT and transaction parties cannot read listings
	// that are not ACTIVE.
	KafkaBrokers  []string `env:"KAFKA_BROKERS" envSeparator:","`
	KafkaClientID string   `env:"KAFKA_CLIENT_ID" envDefault:"catalog"`
	// The consumer group of order-escrow's transaction stream, also
	// DeadLetter.consumer_group.
	KafkaConsumerGroup string `env:"KAFKA_CONSUMER_GROUP" envDefault:"catalog"`
	// The transaction stream's topic (events.TopicTransaction by default).
	KafkaTransactionTopic string `env:"KAFKA_TRANSACTION_TOPIC" envDefault:"agrogram.orderescrow.transaction.v1"`

	GRPCAddr       string `env:"GRPC_ADDR" envDefault:":50051"`
	HTTPAddr       string `env:"HTTP_ADDR" envDefault:":8081"`
	GRPCReflection bool   `env:"GRPC_REFLECTION" envDefault:"false"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	// Plan quotas: ACTIVE listings per owner and kind (market.proto).
	OfferQuotaDefault      int `env:"OFFER_QUOTA_DEFAULT" envDefault:"5"`
	OfferQuotaPremium      int `env:"OFFER_QUOTA_PREMIUM" envDefault:"20"`
	BuyRequestQuotaDefault int `env:"BUY_REQUEST_QUOTA_DEFAULT" envDefault:"5"`
	BuyRequestQuotaPremium int `env:"BUY_REQUEST_QUOTA_PREMIUM" envDefault:"25"`

	CreateRateLimit  int           `env:"CREATE_RATE_LIMIT" envDefault:"30"`
	CreateRateWindow time.Duration `env:"CREATE_RATE_WINDOW" envDefault:"1h"`

	ConsumerMaxAttempts     int           `env:"CONSUMER_MAX_ATTEMPTS" envDefault:"5"`
	ConsumerRetryBackoff    time.Duration `env:"CONSUMER_RETRY_BACKOFF" envDefault:"500ms"`
	ConsumerRetryBackoffMax time.Duration `env:"CONSUMER_RETRY_BACKOFF_MAX" envDefault:"30s"`
	ConsumerHandleTimeout   time.Duration `env:"CONSUMER_HANDLE_TIMEOUT" envDefault:"10s"`
	ConsumerBatchSize       int           `env:"CONSUMER_BATCH_SIZE" envDefault:"100"`
	InboxRetention          time.Duration `env:"INBOX_RETENTION" envDefault:"720h"`

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

// Quotas returns the plan quotas.
func (c Config) Quotas() domain.Quotas {
	return domain.Quotas{
		OfferDefault:      c.OfferQuotaDefault,
		OfferPremium:      c.OfferQuotaPremium,
		BuyRequestDefault: c.BuyRequestQuotaDefault,
		BuyRequestPremium: c.BuyRequestQuotaPremium,
	}
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
		"CREATE_RATE_WINDOW":         c.CreateRateWindow,
		"CONSUMER_RETRY_BACKOFF":     c.ConsumerRetryBackoff,
		"CONSUMER_RETRY_BACKOFF_MAX": c.ConsumerRetryBackoffMax,
		"CONSUMER_HANDLE_TIMEOUT":    c.ConsumerHandleTimeout,
		"VIEWS_FLUSH_INTERVAL":       c.ViewsFlushInterval,
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
		"INBOX_RETENTION":    c.InboxRetention,
		"OUTBOX_RETENTION":   c.OutboxRetention,
		"VIEWS_DEDUP_WINDOW": c.ViewsDedupWindow,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s%s must not be negative (0 turns it off)", Prefix, name))
		}
	}
	for name, n := range map[string]int{
		"OFFER_QUOTA_DEFAULT":       c.OfferQuotaDefault,
		"OFFER_QUOTA_PREMIUM":       c.OfferQuotaPremium,
		"BUY_REQUEST_QUOTA_DEFAULT": c.BuyRequestQuotaDefault,
		"BUY_REQUEST_QUOTA_PREMIUM": c.BuyRequestQuotaPremium,
		"CREATE_RATE_LIMIT":         c.CreateRateLimit,
		"CONSUMER_MAX_ATTEMPTS":     c.ConsumerMaxAttempts,
		"CONSUMER_BATCH_SIZE":       c.ConsumerBatchSize,
		"VIEWS_BUFFER_SIZE":         c.ViewsBufferSize,
		"OUTBOX_BATCH_SIZE":         c.OutboxBatchSize,
		"DATABASE_MAX_CONNS":        int(c.DatabaseMaxConns),
	} {
		if n < 1 {
			errs = append(errs, fmt.Errorf("%s%s must be at least 1, got %d", Prefix, name, n))
		}
	}
	if c.ConsumerRetryBackoffMax < c.ConsumerRetryBackoff {
		errs = append(errs, fmt.Errorf("%sCONSUMER_RETRY_BACKOFF_MAX must be >= %sCONSUMER_RETRY_BACKOFF", Prefix, Prefix))
	}
	if len(c.KafkaBrokers) > 0 {
		for name, v := range map[string]string{
			"KAFKA_CONSUMER_GROUP":    c.KafkaConsumerGroup,
			"KAFKA_TRANSACTION_TOPIC": c.KafkaTransactionTopic,
		} {
			if strings.TrimSpace(v) == "" {
				errs = append(errs, fmt.Errorf("%s%s must not be empty", Prefix, name))
			}
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
