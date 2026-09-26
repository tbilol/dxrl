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

	"github.com/tbilol/dxrl/services/catalog/internal/config"
	"github.com/tbilol/dxrl/services/catalog/internal/events"
	"github.com/tbilol/dxrl/services/catalog/internal/grpcserver"
	"github.com/tbilol/dxrl/services/catalog/internal/health"
	applog "github.com/tbilol/dxrl/services/catalog/internal/log"
	"github.com/tbilol/dxrl/services/catalog/internal/ratelimit"
	"github.com/tbilol/dxrl/services/catalog/internal/repository/postgres"
	"github.com/tbilol/dxrl/services/catalog/internal/service"
	"github.com/tbilol/dxrl/services/catalog/internal/views"
)

// serve runs catalog until SIGINT/SIGTERM, then shuts down gracefully:
// health NOT_SERVING, drain gRPC, stop HTTP, stop the transaction-stream
// consumer (committing what it processed), the relay and the view counter
// (which writes what it has queued), leave the consumer group, flush the
// Kafka producer, close Redis and the Postgres pool.
//
// catalog calls no other service. It consumes one stream, order-escrow's
// transaction stream (the escrow commitments behind the remaining quantity,
// SOLD_OUT and transaction-party visibility), and publishes the listing
// stream through its outbox.
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
		return fmt.Errorf("CATALOG_DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = cfg.DatabaseMaxConns
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("postgres pool: %w", err)
	}
	defer pool.Close()
	store := postgres.New(pool)

	// Redis (optional): the listing-creation rate limit and view
	// de-duplication. Both fail open.
	var (
		limiter service.RateLimiter
		dedup   views.Deduper
	)
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("CATALOG_REDIS_URL: %w", err)
		}
		rdb := redis.NewClient(opt)
		defer rdb.Close()
		limiter = ratelimit.New(rdb, cfg.CreateRateLimit, cfg.CreateRateWindow)
		if cfg.ViewsDedupWindow > 0 {
			dedup = views.NewRedisDeduper(rdb, cfg.ViewsDedupWindow)
		}
	} else {
		log.Warn("CATALOG_REDIS_URL is empty: listing creation is not rate-limited and every view counts")
	}

	counter := views.NewCounter(store, dedup, views.Config{
		FlushInterval: cfg.ViewsFlushInterval,
		BufferSize:    cfg.ViewsBufferSize,
	}, log)
	svc := service.New(store, limiter, counter, service.WithLogger(log), service.WithQuotas(cfg.Quotas()))

	validator, err := protovalidate.New()
	if err != nil {
		return fmt.Errorf("protovalidate: %w", err)
	}

	// Kafka (optional): the outbox relay publishing the listing stream and the
	// consumer of order-escrow's transaction stream. The producer also
	// publishes the consumer's dead letters; the consumer commits offsets
	// itself.
	var (
		producer *kgo.Client
		consumer *kgo.Client
	)
	if len(cfg.KafkaBrokers) > 0 {
		producer, err = kgo.NewClient(
			kgo.SeedBrokers(cfg.KafkaBrokers...),
			kgo.ClientID(cfg.KafkaClientID),
			kgo.RequiredAcks(kgo.AllISRAcks()), // idempotent producer (franz-go default) needs acks=all
		)
		if err != nil {
			return fmt.Errorf("kafka producer: %w", err)
		}
		consumer, err = kgo.NewClient(
			kgo.SeedBrokers(cfg.KafkaBrokers...),
			kgo.ClientID(cfg.KafkaClientID),
			kgo.ConsumerGroup(cfg.KafkaConsumerGroup),
			kgo.ConsumeTopics(cfg.KafkaTransactionTopic),
			kgo.DisableAutoCommit(), // offsets are committed after processing (events.Consumer)
			// A new group starts at the beginning of the topic, which backfills
			// the commitments of every transaction still retained in it.
			kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		)
		if err != nil {
			producer.Close()
			return fmt.Errorf("kafka consumer: %w", err)
		}
	} else {
		log.Warn("CATALOG_KAFKA_BROKERS is empty: the outbox relay and the transaction-stream consumer are disabled; " +
			"ListingCreated events stay in the outbox table, escrow transactions do not reduce remaining quantities " +
			"(no listing becomes SOLD_OUT) and transaction parties cannot read listings that are not ACTIVE")
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
		closeKafka(context.Background(), producer, consumer, log)
		return fmt.Errorf("grpc listen: %w", err)
	}
	httpLis, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		grpcLis.Close()
		closeKafka(context.Background(), producer, consumer, log)
		return fmt.Errorf("http listen: %w", err)
	}

	// Background work outlives the signal context until it is stopped below.
	bgCtx, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBG()
	var bg sync.WaitGroup
	bg.Go(func() { monitor.Run(bgCtx) })
	bg.Go(func() { counter.Run(bgCtx) })
	if producer != nil {
		relay := events.NewRelay(store, producer, events.RelayConfig{
			Interval:   cfg.OutboxInterval,
			BatchSize:  cfg.OutboxBatchSize,
			MaxBackoff: cfg.OutboxMaxBackoff,
			Retention:  cfg.OutboxRetention,
		}, log)
		bg.Go(func() { relay.Run(bgCtx) })
		transactions := events.NewConsumer(consumer, producer, map[string]events.RecordHandler{
			cfg.KafkaTransactionTopic: events.NewTransactionHandler(svc, validator, log),
		}, store, events.ConsumerConfig{
			Group:          cfg.KafkaConsumerGroup,
			MaxAttempts:    cfg.ConsumerMaxAttempts,
			RetryBackoff:   cfg.ConsumerRetryBackoff,
			MaxBackoff:     cfg.ConsumerRetryBackoffMax,
			HandleTimeout:  cfg.ConsumerHandleTimeout,
			BatchSize:      cfg.ConsumerBatchSize,
			Transient:      postgres.IsTransient,
			InboxRetention: cfg.InboxRetention,
		}, log)
		bg.Go(func() { transactions.Run(bgCtx) })
	}

	errc := make(chan error, 2)
	go func() { errc <- fmt.Errorf("grpc server: %w", grpcSrv.Serve(grpcLis)) }()
	go func() {
		if err := httpSrv.Serve(httpLis); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("http server: %w", err)
		}
	}()
	log.Info("catalog started", "grpc_addr", grpcLis.Addr().String(), "http_addr", httpLis.Addr().String(),
		"grpc_reflection", cfg.GRPCReflection, "rate_limit", limiter != nil, "view_dedup", dedup != nil,
		"kafka", producer != nil, "consumer_group", cfg.KafkaConsumerGroup, "transaction_topic", cfg.KafkaTransactionTopic,
		"quotas", cfg.Quotas())

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
	stopBG() // the view counter writes what it has queued before it returns
	waitGroup(shutdownCtx, &bg, log)
	closeKafka(shutdownCtx, producer, consumer, log)
	log.Info("catalog stopped")
	return runErr
}

// closeKafka leaves the consumer group (the consumer has committed what it
// processed) and flushes and closes the producer. Both may be nil.
func closeKafka(ctx context.Context, producer, consumer *kgo.Client, log *slog.Logger) {
	if consumer != nil {
		consumer.Close()
	}
	if producer != nil {
		if err := producer.Flush(ctx); err != nil {
			log.Warn("kafka flush", "error", err)
		}
		producer.Close()
	}
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
