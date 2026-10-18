package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
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
	if cfg.ListingQuotaPerBoard != domain.DefaultListingQuota {
		t.Fatalf("listing quota = %d, want the contract's %d", cfg.ListingQuotaPerBoard, domain.DefaultListingQuota)
	}
}

func TestListingQuotaIsConfigurable(t *testing.T) {
	t.Setenv("CATALOG_DATABASE_URL", "postgres://x")
	t.Setenv("CATALOG_LISTING_QUOTA_PER_BOARD", "3")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListingQuotaPerBoard != 3 {
		t.Fatalf("listing quota = %d, want 3", cfg.ListingQuotaPerBoard)
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
	t.Setenv("CATALOG_LISTING_QUOTA_PER_BOARD", "0")
	t.Setenv("CATALOG_CREATE_RATE_LIMIT", "0")
	t.Setenv("CATALOG_LOG_FORMAT", "xml")
	t.Setenv("CATALOG_VIEWS_DEDUP_WINDOW", "-1h")
	t.Setenv("CATALOG_VIEWS_FLUSH_INTERVAL", "0s")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"LISTING_QUOTA_PER_BOARD", "CREATE_RATE_LIMIT", "LOG_FORMAT", "VIEWS_DEDUP_WINDOW", "VIEWS_FLUSH_INTERVAL"} {
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
