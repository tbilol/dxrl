package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every IDENTITY_* variable for the rest of the test, so the
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
	t.Setenv("IDENTITY_DATABASE_URL", "postgres://identity:x@localhost:5432/identity")
	t.Setenv("IDENTITY_KAFKA_BROKERS", "a:9092,b:9092")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":50051" || cfg.HTTPAddr != ":8081" || cfg.GRPCReflection || cfg.LogFormat != "json" ||
		len(cfg.KafkaBrokers) != 2 || cfg.OutboxInterval != time.Second || cfg.OutboxRetention != 168*time.Hour {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestLoadListsEveryProblem(t *testing.T) {
	t.Setenv("IDENTITY_DATABASE_URL", "")
	t.Setenv("IDENTITY_OUTBOX_INTERVAL", "soon")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"IDENTITY_DATABASE_URL", "OutboxInterval", `"soon"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadValidates(t *testing.T) {
	t.Setenv("IDENTITY_DATABASE_URL", "postgres://x")
	t.Setenv("IDENTITY_OUTBOX_BATCH_SIZE", "0")
	t.Setenv("IDENTITY_LOG_FORMAT", "xml")
	t.Setenv("IDENTITY_HEALTH_INTERVAL", "-1s")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"OUTBOX_BATCH_SIZE", "LOG_FORMAT", "HEALTH_INTERVAL"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestHTTPAddr(t *testing.T) {
	t.Setenv("IDENTITY_HTTP_ADDR", "127.0.0.1:18051")
	if got, err := HTTPAddr(); err != nil || got != "127.0.0.1:18051" {
		t.Fatalf("HTTPAddr = %q, %v", got, err)
	}
}
