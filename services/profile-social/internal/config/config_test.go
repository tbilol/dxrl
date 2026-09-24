package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("PROFILE_SOCIAL_DATABASE_URL", "postgres://profile_social:x@localhost:5432/profile_social")
	t.Setenv("PROFILE_SOCIAL_KAFKA_BROKERS", "a:9092,b:9092")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":50051" || cfg.HTTPAddr != ":8081" || cfg.GRPCReflection || cfg.LogFormat != "json" ||
		len(cfg.KafkaBrokers) != 2 || cfg.RedisURL != "" || cfg.CatalogAddr != "" || cfg.CatalogTimeout != 2*time.Second ||
		cfg.KafkaConsumerGroup != "profilesocial" || cfg.KafkaClientID != "profile-social" ||
		cfg.FollowRateLimit != 100 || cfg.FollowRateWindow != time.Hour || cfg.ConsumerMaxAttempts != 5 ||
		cfg.ConsumerRetryBackoff != 500*time.Millisecond || cfg.InboxRetention != 720*time.Hour {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestLoadListsEveryProblem(t *testing.T) {
	t.Setenv("PROFILE_SOCIAL_DATABASE_URL", "")
	t.Setenv("PROFILE_SOCIAL_CATALOG_TIMEOUT", "soon")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"PROFILE_SOCIAL_DATABASE_URL", "CatalogTimeout", `"soon"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadValidates(t *testing.T) {
	t.Setenv("PROFILE_SOCIAL_DATABASE_URL", "postgres://x")
	t.Setenv("PROFILE_SOCIAL_FOLLOW_RATE_LIMIT", "0")
	t.Setenv("PROFILE_SOCIAL_LOG_FORMAT", "xml")
	t.Setenv("PROFILE_SOCIAL_CONSUMER_RETRY_BACKOFF", "10s")
	t.Setenv("PROFILE_SOCIAL_CONSUMER_RETRY_BACKOFF_MAX", "1s")
	t.Setenv("PROFILE_SOCIAL_INBOX_RETENTION", "-1h")
	t.Setenv("PROFILE_SOCIAL_KAFKA_BROKERS", "a:9092")
	t.Setenv("PROFILE_SOCIAL_KAFKA_CONSUMER_GROUP", " ")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"FOLLOW_RATE_LIMIT", "LOG_FORMAT", "CONSUMER_RETRY_BACKOFF_MAX", "INBOX_RETENTION", "KAFKA_CONSUMER_GROUP"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestHTTPAddr(t *testing.T) {
	t.Setenv("PROFILE_SOCIAL_HTTP_ADDR", "127.0.0.1:18053")
	if got, err := HTTPAddr(); err != nil || got != "127.0.0.1:18053" {
		t.Fatalf("HTTPAddr = %q, %v", got, err)
	}
}
