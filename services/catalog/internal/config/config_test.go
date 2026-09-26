package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/events"
)

// clearEnv unsets every CATALOG_* variable for the rest of the test, so the
// defaults are checked even when the caller's environment sets some (make
// test-integration loads ./.env for every package).
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, Prefix) {
			t.Setenv(name, "") // restores the original value after the test
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("CATALOG_DATABASE_URL", "postgres://catalog:x@localhost:5432/catalog")
	t.Setenv("CATALOG_KAFKA_BROKERS", "a:9092,b:9092")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":50051" || cfg.HTTPAddr != ":8081" || cfg.GRPCReflection || cfg.LogFormat != "json" ||
		len(cfg.KafkaBrokers) != 2 || cfg.RedisURL != "" || cfg.KafkaClientID != "catalog" ||
		cfg.CreateRateLimit != 30 || cfg.CreateRateWindow != time.Hour || cfg.ViewsFlushInterval != 2*time.Second ||
		cfg.ViewsBufferSize != 10000 || cfg.ViewsDedupWindow != 24*time.Hour || cfg.OutboxRetention != 168*time.Hour {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.KafkaConsumerGroup != "catalog" || cfg.ConsumerMaxAttempts != 5 ||
		cfg.ConsumerRetryBackoff != 500*time.Millisecond || cfg.ConsumerRetryBackoffMax != 30*time.Second ||
		cfg.ConsumerHandleTimeout != 10*time.Second || cfg.ConsumerBatchSize != 100 || cfg.InboxRetention != 720*time.Hour {
		t.Fatalf("consumer defaults = %+v", cfg)
	}
	// The default topic is the contract's (events.TopicTransaction); the
	// variable only renames it.
	if cfg.KafkaTransactionTopic != events.TopicTransaction {
		t.Fatalf("transaction topic = %q, want %q", cfg.KafkaTransactionTopic, events.TopicTransaction)
	}
	if cfg.Quotas() != domain.DefaultQuotas {
		t.Fatalf("quotas = %+v, want the contract's %+v", cfg.Quotas(), domain.DefaultQuotas)
	}
}

func TestConsumerSettings(t *testing.T) {
	clearEnv(t)
	t.Setenv("CATALOG_DATABASE_URL", "postgres://x")
	t.Setenv("CATALOG_KAFKA_BROKERS", "kafka:9092")
	t.Setenv("CATALOG_KAFKA_CONSUMER_GROUP", "catalog-blue")
	t.Setenv("CATALOG_KAFKA_TRANSACTION_TOPIC", "staging.orderescrow.transaction.v1")
	t.Setenv("CATALOG_CONSUMER_MAX_ATTEMPTS", "3")
	t.Setenv("CATALOG_CONSUMER_RETRY_BACKOFF", "1s")
	t.Setenv("CATALOG_CONSUMER_RETRY_BACKOFF_MAX", "1s")
	t.Setenv("CATALOG_CONSUMER_HANDLE_TIMEOUT", "5s")
	t.Setenv("CATALOG_CONSUMER_BATCH_SIZE", "10")
	t.Setenv("CATALOG_INBOX_RETENTION", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KafkaConsumerGroup != "catalog-blue" || cfg.KafkaTransactionTopic != "staging.orderescrow.transaction.v1" ||
		cfg.ConsumerMaxAttempts != 3 || cfg.ConsumerRetryBackoff != time.Second || cfg.ConsumerRetryBackoffMax != time.Second ||
		cfg.ConsumerHandleTimeout != 5*time.Second || cfg.ConsumerBatchSize != 10 || cfg.InboxRetention != 0 {
		t.Fatalf("consumer settings = %+v", cfg)
	}

	// Without brokers there is no consumer, so its group and topic may be blank.
	clearEnv(t)
	t.Setenv("CATALOG_DATABASE_URL", "postgres://x")
	t.Setenv("CATALOG_KAFKA_CONSUMER_GROUP", " ")
	t.Setenv("CATALOG_KAFKA_TRANSACTION_TOPIC", " ")
	if _, err := Load(); err != nil {
		t.Fatalf("no brokers: %v", err)
	}
}

func TestQuotasAreConfigurable(t *testing.T) {
	t.Setenv("CATALOG_DATABASE_URL", "postgres://x")
	t.Setenv("CATALOG_OFFER_QUOTA_DEFAULT", "2")
	t.Setenv("CATALOG_OFFER_QUOTA_PREMIUM", "7")
	t.Setenv("CATALOG_BUY_REQUEST_QUOTA_DEFAULT", "3")
	t.Setenv("CATALOG_BUY_REQUEST_QUOTA_PREMIUM", "9")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Quotas{OfferDefault: 2, OfferPremium: 7, BuyRequestDefault: 3, BuyRequestPremium: 9}
	if cfg.Quotas() != want {
		t.Fatalf("quotas = %+v, want %+v", cfg.Quotas(), want)
	}
}

func TestLoadListsEveryProblem(t *testing.T) {
	t.Setenv("CATALOG_DATABASE_URL", "")
	t.Setenv("CATALOG_CREATE_RATE_WINDOW", "soon")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"CATALOG_DATABASE_URL", "CreateRateWindow", `"soon"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadValidates(t *testing.T) {
	t.Setenv("CATALOG_DATABASE_URL", "postgres://x")
	t.Setenv("CATALOG_OFFER_QUOTA_PREMIUM", "0")
	t.Setenv("CATALOG_CREATE_RATE_LIMIT", "0")
	t.Setenv("CATALOG_LOG_FORMAT", "xml")
	t.Setenv("CATALOG_VIEWS_DEDUP_WINDOW", "-1h")
	t.Setenv("CATALOG_VIEWS_FLUSH_INTERVAL", "0s")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"OFFER_QUOTA_PREMIUM", "CREATE_RATE_LIMIT", "LOG_FORMAT", "VIEWS_DEDUP_WINDOW", "VIEWS_FLUSH_INTERVAL"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadValidatesConsumerSettings(t *testing.T) {
	t.Setenv("CATALOG_DATABASE_URL", "postgres://x")
	t.Setenv("CATALOG_KAFKA_BROKERS", "a:9092")
	// An empty value means the default (caarlos0/env); only blanks are empty.
	t.Setenv("CATALOG_KAFKA_CONSUMER_GROUP", " ")
	t.Setenv("CATALOG_KAFKA_TRANSACTION_TOPIC", "\t")
	t.Setenv("CATALOG_CONSUMER_MAX_ATTEMPTS", "0")
	t.Setenv("CATALOG_CONSUMER_BATCH_SIZE", "0")
	t.Setenv("CATALOG_CONSUMER_RETRY_BACKOFF", "10s")
	t.Setenv("CATALOG_CONSUMER_RETRY_BACKOFF_MAX", "1s")
	t.Setenv("CATALOG_CONSUMER_HANDLE_TIMEOUT", "0s")
	t.Setenv("CATALOG_INBOX_RETENTION", "-1h")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"CATALOG_KAFKA_CONSUMER_GROUP", "CATALOG_KAFKA_TRANSACTION_TOPIC", "CONSUMER_MAX_ATTEMPTS",
		"CONSUMER_BATCH_SIZE", "CONSUMER_RETRY_BACKOFF_MAX must be >=", "CONSUMER_HANDLE_TIMEOUT", "INBOX_RETENTION"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestHTTPAddr(t *testing.T) {
	t.Setenv("CATALOG_HTTP_ADDR", "127.0.0.1:18054")
	if got, err := HTTPAddr(); err != nil || got != "127.0.0.1:18054" {
		t.Fatalf("HTTPAddr = %q, %v", got, err)
	}
}
