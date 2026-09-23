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

	"github.com/tbilol/dxrl/services/identity/internal/config"
	"github.com/tbilol/dxrl/services/identity/internal/eijara"
	"github.com/tbilol/dxrl/services/identity/internal/events"
	"github.com/tbilol/dxrl/services/identity/internal/grpcserver"
	"github.com/tbilol/dxrl/services/identity/internal/health"
	applog "github.com/tbilol/dxrl/services/identity/internal/log"
	"github.com/tbilol/dxrl/services/identity/internal/ratelimit"
	"github.com/tbilol/dxrl/services/identity/internal/repository/postgres"
	"github.com/tbilol/dxrl/services/identity/internal/service"
)

// serve runs identity until SIGINT/SIGTERM, then shuts down gracefully:
// health NOT_SERVING, drain gRPC, stop HTTP, stop the worker and the relay,
// flush the Kafka producer, close Redis and the Postgres pool.
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

	// Postgres. The pool connects lazily: readiness reports the database.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("IDENTITY_DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = cfg.DatabaseMaxConns
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("postgres pool: %w", err)
	}
	defer pool.Close()
	store := postgres.New(pool)

	// Redis (optional): the farmer verification rate limit.
	var (
		rdb     *redis.Client
		limiter service.RateLimiter
	)
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("IDENTITY_REDIS_URL: %w", err)
		}
		rdb = redis.NewClient(opt)
		defer rdb.Close()
		limiter = ratelimit.New(rdb, cfg.VerificationRateLimit, cfg.VerificationRateWindow)
	} else {
		log.Warn("IDENTITY_REDIS_URL is empty: farmer verification rate limiting is disabled")
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
		log.Warn("IDENTITY_KAFKA_BROKERS is empty: outbox relay disabled, events stay in the outbox table")
	}

	// STUB: e-ijara does not exist yet; eijara.Stub answers locally.
	checker := eijara.Stub{Delay: cfg.EIjaraStubDelay}
	svc := service.New(store, checker, limiter, service.Config{
		EIjaraTimeout:   cfg.EIjaraTimeout,
		RetryBackoff:    cfg.RetryBackoff,
		RetryBackoffMax: cfg.RetryBackoffMax,
		WorkerInterval:  cfg.WorkerInterval,
		WorkerBatchSize: cfg.WorkerBatchSize,
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
	bg.Go(func() { svc.RunWorker(bgCtx) })
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
	log.Info("identity started", "grpc_addr", grpcLis.Addr().String(), "http_addr", httpLis.Addr().String(),
		"grpc_reflection", cfg.GRPCReflection, "rate_limit", limiter != nil, "outbox_relay", kafka != nil)

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
	log.Info("identity stopped")
	return runErr
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
