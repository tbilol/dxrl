package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"buf.build/go/protovalidate"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
	grpchealth "google.golang.org/grpc/health"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/config"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/events"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/grpcserver"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/health"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/identityclient"
	applog "github.com/tbilol/dxrl/services/auth-gateway/internal/log"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/password"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/provider"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/ratelimit"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/repository/postgres"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/service"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/sms"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/token"
)

// serve runs auth-gateway until SIGINT/SIGTERM, then shuts down gracefully:
// health NOT_SERVING, drain gRPC, stop HTTP, stop the relay, flush the Kafka
// producer, close Redis, the identity connection and the Postgres pool.
func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log, err := applog.New(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Signing keys first: without one the service has nothing to offer, and
	// failing here is far better than failing at the first sign-in.
	pemKey, err := cfg.PrivateKeyPEM()
	if err != nil {
		return err
	}
	keys, err := token.LoadKeySet(pemKey, cfg.JWTPreviousPubKeys)
	if err != nil {
		return fmt.Errorf("%sJWT_PRIVATE_KEY: %w (generate one with `make dev-key`)", config.Prefix, err)
	}
	issuer, err := token.NewIssuer(keys, token.Config{
		Issuer:   cfg.JWTIssuer,
		Audience: cfg.JWTAudience,
		TTL:      cfg.AccessTokenTTL,
		Leeway:   cfg.TokenClockSkew,
	})
	if err != nil {
		return fmt.Errorf("token issuer: %w", err)
	}

	// Postgres. The pool connects lazily: readiness reports the database.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("%sDATABASE_URL: %w", config.Prefix, err)
	}
	poolCfg.MaxConns = cfg.DatabaseMaxConns
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("postgres pool: %w", err)
	}
	defer pool.Close()
	store := postgres.New(pool)

	// identity: the account store. The connection is lazy, so identity being
	// down delays sign-ins (DEPENDENCY_UNAVAILABLE) but not the start-up.
	accounts, err := identityclient.Dial(cfg.IdentityAddr, cfg.IdentityTimeout)
	if err != nil {
		return err
	}
	defer accounts.Close()

	// Redis (optional): sign-in and phone code rate limits.
	var (
		rdb         *redis.Client
		limiter     service.RateLimiter
		phoneLimits *ratelimit.Limiter
	)
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("%sREDIS_URL: %w", config.Prefix, err)
		}
		rdb = redis.NewClient(opt)
		defer rdb.Close()
		phoneLimits = ratelimit.New(rdb, cfg.PhoneCodeRateLimit, cfg.PhoneCodeRateWindow)
		limiter = &splitLimiter{
			phone:  phoneLimits,
			signIn: ratelimit.New(rdb, cfg.SignInRateLimit, cfg.SignInRateWindow),
		}
	} else {
		log.Warn(config.Prefix + "REDIS_URL is empty: sign-in and phone code rate limiting is disabled")
	}

	// Kafka (optional): the outbox relay.
	var kafka *kgo.Client
	if len(cfg.KafkaBrokers) > 0 {
		kafka, err = kgo.NewClient(
			kgo.SeedBrokers(cfg.KafkaBrokers...),
			kgo.ClientID(cfg.KafkaClientID),
			kgo.RequiredAcks(kgo.AllISRAcks()), // idempotent producer (franz-go default) needs acks=all
		)
		if err != nil {
			return fmt.Errorf("kafka client: %w", err)
		}
	} else {
		log.Warn(config.Prefix + "KAFKA_BROKERS is empty: outbox relay disabled, events stay in the outbox table")
	}

	if cfg.DevPhoneCode != "" || cfg.DevLogPhoneCodes {
		log.Warn("development SMS settings are on: codes are fixed and/or logged. Never do this outside local development",
			"fixed_code", cfg.DevPhoneCode != "", "logged", cfg.DevLogPhoneCodes)
	}
	svc := service.New(service.Deps{
		Store:    store,
		Accounts: accounts,
		Issuer:   issuer,
		Hasher:   password.New(cfg.Argon2Params()),
		// STUB: neither verifier calls Google or Apple; see internal/provider.
		Google: provider.GoogleStub{},
		Apple:  provider.AppleStub{},
		// STUB: no SMS is ever sent; see internal/sms.
		Sender:  sms.StubSender{Log: log, LogCodes: cfg.DevLogPhoneCodes},
		Limiter: limiter,
	}, service.Config{
		RefreshTokenTTL:      cfg.RefreshTokenTTL,
		ProviderTimeout:      cfg.IdentityTimeout,
		PhoneCodeTTL:         cfg.PhoneCodeTTL,
		PhoneCodeResend:      cfg.PhoneCodeResend,
		PhoneCodeLength:      cfg.PhoneCodeLength,
		PhoneCodeMaxAttempts: cfg.PhoneCodeMaxAttempts,
		DevPhoneCode:         cfg.DevPhoneCode,
		VerifyTokenCaller:    cfg.VerifyTokenCaller,
	}, service.WithLogger(log))

	validator, err := protovalidate.New()
	if err != nil {
		return fmt.Errorf("protovalidate: %w", err)
	}
	grpcHealth := grpchealth.NewServer()
	monitor := health.NewMonitor(pool, grpcHealth, cfg.HealthInterval, log, "", grpcserver.ServiceName)
	grpcSrv := grpcserver.New(svc, grpcHealth, grpcserver.Options{
		Logger:     log,
		Validator:  validator,
		Reflection: cfg.GRPCReflection,
	})
	httpSrv := &http.Server{Handler: monitor.Handler(), ReadHeaderTimeout: 5 * time.Second}

	// Listen first, so a taken port fails before background work starts.
	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	httpLis, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		grpcLis.Close()
		return fmt.Errorf("http listen: %w", err)
	}

	// Background work outlives the signal context until it is stopped below.
	bgCtx, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBG()
	var bg sync.WaitGroup
	bg.Go(func() { monitor.Run(bgCtx) })
	if kafka != nil {
		relay := events.NewRelay(store, kafka, events.RelayConfig{
			Interval:   cfg.OutboxInterval,
			BatchSize:  cfg.OutboxBatchSize,
			MaxBackoff: cfg.OutboxMaxBackoff,
			Retention:  cfg.OutboxRetention,
		}, log)
		bg.Go(func() { relay.Run(bgCtx) })
	}

	errc := make(chan error, 2)
	go func() { errc <- fmt.Errorf("grpc server: %w", grpcSrv.Serve(grpcLis)) }()
	go func() {
		if err := httpSrv.Serve(httpLis); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("http server: %w", err)
		}
	}()
	log.Info("auth-gateway started", "grpc_addr", grpcLis.Addr().String(), "http_addr", httpLis.Addr().String(),
		"grpc_reflection", cfg.GRPCReflection, "identity_addr", cfg.IdentityAddr,
		"signing_key", keys.Active().ID, "signing_alg", keys.Active().Alg, "jwks_keys", len(keys.JWKS()),
		"rate_limit", limiter != nil, "outbox_relay", kafka != nil)

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down", "signal", context.Cause(ctx).Error())
	case runErr = <-errc:
		log.Error("server failed; shutting down", "error", runErr)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	monitor.Shutdown()
	grpcserver.GracefulStop(shutdownCtx, grpcSrv)
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "error", err)
	}
	stopBG()
	waitGroup(shutdownCtx, &bg, log)
	if kafka != nil {
		if err := kafka.Flush(shutdownCtx); err != nil {
			log.Warn("kafka flush", "error", err)
		}
		kafka.Close()
	}
	log.Info("auth-gateway stopped")
	return runErr
}

// splitLimiter routes each rate-limit key to the window it belongs to: phone
// code requests and failed sign-ins have different limits, but the service
// sees one RateLimiter.
type splitLimiter struct {
	phone  *ratelimit.Limiter
	signIn *ratelimit.Limiter
}

func (l *splitLimiter) pick(key string) *ratelimit.Limiter {
	if len(key) >= 11 && key[:11] == "phone-code:" {
		return l.phone
	}
	return l.signIn
}

func (l *splitLimiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	return l.pick(key).Allow(ctx, key)
}

func (l *splitLimiter) Exceeded(ctx context.Context, key string) (bool, time.Duration, error) {
	return l.pick(key).Exceeded(ctx, key)
}

func waitGroup(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Warn("background workers did not stop before the shutdown timeout")
	}
}
