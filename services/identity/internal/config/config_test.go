package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("IDENTITY_DATABASE_URL", "postgres://identity:x@localhost:5432/identity")
	t.Setenv("IDENTITY_KAFKA_BROKERS", "a:9092,b:9092")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":50051" || cfg.HTTPAddr != ":8081" || cfg.EIjaraTimeout != 3*time.Second ||
		cfg.VerificationRateLimit != 5 || cfg.VerificationRateWindow != time.Hour || cfg.GRPCReflection ||
		cfg.LogFormat != "json" || len(cfg.KafkaBrokers) != 2 || cfg.RedisURL != "" {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestLoadListsEveryProblem(t *testing.T) {
	t.Setenv("IDENTITY_DATABASE_URL", "")
	t.Setenv("IDENTITY_EIJARA_TIMEOUT", "soon")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"IDENTITY_DATABASE_URL", "EIjaraTimeout", `"soon"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadValidates(t *testing.T) {
	t.Setenv("IDENTITY_DATABASE_URL", "postgres://x")
	t.Setenv("IDENTITY_VERIFICATION_RATE_LIMIT", "0")
	t.Setenv("IDENTITY_LOG_FORMAT", "xml")
	t.Setenv("IDENTITY_WORKER_INTERVAL", "-1s")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"VERIFICATION_RATE_LIMIT", "LOG_FORMAT", "WORKER_INTERVAL"} {
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
