package config

import (
	"strings"
	"testing"
	"time"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

func TestLoadDefaults(t *testing.T) {
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
	if cfg.Quotas() != domain.DefaultQuotas {
		t.Fatalf("quotas = %+v, want the contract's %+v", cfg.Quotas(), domain.DefaultQuotas)
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

func TestHTTPAddr(t *testing.T) {
	t.Setenv("CATALOG_HTTP_ADDR", "127.0.0.1:18054")
	if got, err := HTTPAddr(); err != nil || got != "127.0.0.1:18054" {
		t.Fatalf("HTTPAddr = %q, %v", got, err)
	}
}
