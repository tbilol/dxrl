package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"buf.build/go/protovalidate"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
	grpchealth "google.golang.org/grpc/health"

	"github.com/tbilol/dxrl/services/profile-social/internal/catalogclient"
	"github.com/tbilol/dxrl/services/profile-social/internal/config"
	"github.com/tbilol/dxrl/services/profile-social/internal/events"
	"github.com/tbilol/dxrl/services/profile-social/internal/grpcserver"
	"github.com/tbilol/dxrl/services/profile-social/internal/health"
	applog "github.com/tbilol/dxrl/services/profile-social/internal/log"
	"github.com/tbilol/dxrl/services/profile-social/internal/ratelimit"
	"github.com/tbilol/dxrl/services/profile-social/internal/repository/postgres"
	"github.com/tbilol/dxrl/services/profile-social/internal/service"
)

// serve runs profile-social until SIGINT/SIGTERM, then shuts down
// gracefully: health NOT_SERVING, drain gRPC, stop HTTP, stop the consumer
// (committing what it processed) and the relay, leave the consumer group,
// flush the Kafka producer, close the catalog connection, Redis and the
// Postgres pool.
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
		return fmt.Errorf("PROFILE_SOCIAL_DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = cfg.DatabaseMaxConns
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("postgres pool: %w", err)
	}
	defer pool.Close()
	store := postgres.New(pool)

	// Redis (optional): the follow/unfollow rate limit.
	var limiter service.RateLimiter
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("PROFILE_SOCIAL_REDIS_URL: %w", err)
		}
		rdb := redis.NewClient(opt)
		defer rdb.Close()
		limiter = ratelimit.New(rdb, cfg.FollowRateLimit, cfg.FollowRateWindow)
	} else {
		log.Warn("PROFILE_SOCIAL_REDIS_URL is empty: follow rate limiting is disabled")
	}

	// catalog (optional): ListUserListings. The connection is lazy, so catalog
	// may start after profile-social or restart; without an address, or while
	// catalog is unreachable, ListUserListings answers empty pages (with a
	// warning).
	var listings service.ListingSource
	if cfg.CatalogAddr != "" {
		catalog, err := catalogclient.Dial(cfg.CatalogAddr, cfg.CatalogTimeout)
		if err != nil {
			return fmt.Errorf("PROFILE_SOCIAL_CATALOG_ADDR: %w", err)
		}
		defer catalog.Close()
		listings = catalog
	} else {
		log.Warn("PROFILE_SOCIAL_CATALOG_ADDR is empty: ListUserListings answers empty pages")
	}

	svc := service.New(store, limiter, listings, service.WithLogger(log))

	validator, err := protovalidate.New()
	if err != nil {
		return fmt.Errorf("protovalidate: %w", err)
	}

	// Kafka (optional): the outbox relay and the consumer of identity's
	// account stream (the account projection). The producer also publishes
	// dead letters; the consumer commits offsets itself.
	var (
		producer *kgo.Client
		consumer *kgo.Client
		handlers = map[string]events.RecordHandler{
			events.TopicAccount: events.NewAccountHandler(svc, validator, log),
		}
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
			kgo.ConsumeTopics(slices.Sorted(maps.Keys(handlers))...),
			kgo.DisableAutoCommit(), // offsets are committed after processing (events.Consumer)
			// A new group starts at the beginning of the stream, so profiles
			// catch up with every account identity ever published.
			kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		)
		if err != nil {
			producer.Close()
			return fmt.Errorf("kafka consumer: %w", err)
		}
	} else {
		log.Warn("PROFILE_SOCIAL_KAFKA_BROKERS is empty: the account-stream consumer and the outbox relay are " +
			"disabled; account roles will not update and events stay in the outbox table")
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
	if producer != nil {
		relay := events.NewRelay(store, producer, events.RelayConfig{
			Interval:   cfg.OutboxInterval,
			BatchSize:  cfg.OutboxBatchSize,
			MaxBackoff: cfg.OutboxMaxBackoff,
			Retention:  cfg.OutboxRetention,
		}, log)
		bg.Go(func() { relay.Run(bgCtx) })
		streams := events.NewConsumer(consumer, producer, handlers, store, events.ConsumerConfig{
			Group:          cfg.KafkaConsumerGroup,
			MaxAttempts:    cfg.ConsumerMaxAttempts,
			RetryBackoff:   cfg.ConsumerRetryBackoff,
			MaxBackoff:     cfg.ConsumerRetryBackoffMax,
			HandleTimeout:  cfg.ConsumerHandleTimeout,
			BatchSize:      cfg.ConsumerBatchSize,
			Transient:      postgres.IsTransient,
			InboxRetention: cfg.InboxRetention,
		}, log)
		bg.Go(func() { streams.Run(bgCtx) })
	}

	errc := make(chan error, 2)
	go func() { errc <- fmt.Errorf("grpc server: %w", grpcSrv.Serve(grpcLis)) }()
	go func() {
		if err := httpSrv.Serve(httpLis); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("http server: %w", err)
		}
	}()
	log.Info("profile-social started", "grpc_addr", grpcLis.Addr().String(), "http_addr", httpLis.Addr().String(),
		"grpc_reflection", cfg.GRPCReflection, "rate_limit", limiter != nil, "catalog", listings != nil,
		"kafka", producer != nil, "consumer_group", cfg.KafkaConsumerGroup, "topics", slices.Sorted(maps.Keys(handlers)))

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
	closeKafka(shutdownCtx, producer, consumer, log)
	log.Info("profile-social stopped")
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
