package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every AUTH_GATEWAY_* variable for the rest of the test, so
// defaults and missing values are checked even when the caller's environment
// sets some (make test-integration loads ./.env for every package).
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

func setRequired(t *testing.T) {
	t.Helper()
	clearEnv(t)
	t.Setenv("AUTH_GATEWAY_DATABASE_URL", "postgres://auth_gateway:x@localhost:5432/auth_gateway")
	t.Setenv("AUTH_GATEWAY_IDENTITY_ADDR", "127.0.0.1:15051")
	t.Setenv("AUTH_GATEWAY_JWT_PRIVATE_KEY", "-----BEGIN EC PRIVATE KEY-----")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)
	t.Setenv("AUTH_GATEWAY_KAFKA_BROKERS", "a:9092,b:9092")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":50051" || cfg.HTTPAddr != ":8081" || cfg.AccessTokenTTL != 15*time.Minute ||
		cfg.RefreshTokenTTL != 720*time.Hour || cfg.PhoneCodeTTL != 10*time.Minute ||
		cfg.PhoneCodeLength != 6 || cfg.VerifyTokenCaller != "api-gateway" || cfg.GRPCReflection ||
		cfg.LogFormat != "json" || len(cfg.KafkaBrokers) != 2 || cfg.RedisURL != "" ||
		cfg.DevPhoneCode != "" || cfg.DevLogPhoneCodes {
		t.Fatalf("defaults = %+v", cfg)
	}
	if p := cfg.Argon2Params(); p.MemoryKiB != 65536 || p.Time != 3 || p.Parallelism != 2 {
		t.Fatalf("argon2 params = %+v", p)
	}
}

func TestLoadListsEveryProblem(t *testing.T) {
	t.Setenv("AUTH_GATEWAY_DATABASE_URL", "")
	t.Setenv("AUTH_GATEWAY_IDENTITY_ADDR", "")
	t.Setenv("AUTH_GATEWAY_ACCESS_TOKEN_TTL", "soon")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"AUTH_GATEWAY_DATABASE_URL", "AUTH_GATEWAY_IDENTITY_ADDR", `"soon"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadValidates(t *testing.T) {
	setRequired(t)
	t.Setenv("AUTH_GATEWAY_SIGN_IN_RATE_LIMIT", "0")
	t.Setenv("AUTH_GATEWAY_LOG_FORMAT", "xml")
	t.Setenv("AUTH_GATEWAY_PHONE_CODE_TTL", "-1s")
	t.Setenv("AUTH_GATEWAY_PHONE_CODE_LENGTH", "12")
	t.Setenv("AUTH_GATEWAY_DEV_PHONE_CODE", "abc")
	t.Setenv("AUTH_GATEWAY_ARGON2_MEMORY_KIB", "16")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"SIGN_IN_RATE_LIMIT", "LOG_FORMAT", "PHONE_CODE_TTL", "PHONE_CODE_LENGTH",
		"DEV_PHONE_CODE", "ARGON2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

// A missing signing key must stop the start-up and say how to get one.
func TestLoadRequiresASigningKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_GATEWAY_DATABASE_URL", "postgres://x")
	t.Setenv("AUTH_GATEWAY_IDENTITY_ADDR", "127.0.0.1:15051")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"JWT_PRIVATE_KEY", "make dev-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestPrivateKeyPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, []byte("PEM-FROM-FILE"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{JWTPrivateKeyFile: path, JWTPrivateKey: "PEM-INLINE"}
	got, err := cfg.PrivateKeyPEM()
	if err != nil || got != "PEM-FROM-FILE" {
		t.Fatalf("PrivateKeyPEM = %q, %v (the file wins)", got, err)
	}
	if got, err = (Config{JWTPrivateKey: "PEM-INLINE"}).PrivateKeyPEM(); err != nil || got != "PEM-INLINE" {
		t.Fatalf("PrivateKeyPEM = %q, %v", got, err)
	}
	if _, err = (Config{JWTPrivateKeyFile: filepath.Join(t.TempDir(), "missing.pem")}).PrivateKeyPEM(); err == nil ||
		!strings.Contains(err.Error(), "make dev-key") {
		t.Fatalf("missing key file error = %v", err)
	}
}

func TestHTTPAddr(t *testing.T) {
	t.Setenv("AUTH_GATEWAY_HTTP_ADDR", "127.0.0.1:18052")
	if got, err := HTTPAddr(); err != nil || got != "127.0.0.1:18052" {
		t.Fatalf("HTTPAddr = %q, %v", got, err)
	}
}
