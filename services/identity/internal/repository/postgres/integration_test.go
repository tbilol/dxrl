//go:build integration

// Integration tests against a real PostgreSQL. They run only with
// `-tags=integration` and IDENTITY_TEST_DATABASE_URL set (make
// test-integration loads it from .env). Each run migrates a fresh temporary
// schema (selected through search_path) and drops it afterwards, so the
// target database is left as it was.
package postgres_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/identity/internal/auth"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/events"
	"github.com/tbilol/dxrl/services/identity/internal/repository/postgres"
	"github.com/tbilol/dxrl/services/identity/internal/service"
	"github.com/tbilol/dxrl/services/identity/migrations"
)

var ctx = context.Background()

// setup creates a temporary schema, migrates it and returns a pool whose
// search_path points at it.
func setup(t *testing.T) (*pgxpool.Pool, *goose.Provider) {
	t.Helper()
	url := os.Getenv("IDENTITY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("IDENTITY_TEST_DATABASE_URL is not set")
	}
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "identity_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		_ = admin.Close(context.Background())
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return pool, provider
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func createAccount(t *testing.T, store *postgres.Store, now time.Time) domain.Account {
	t.Helper()
	var a domain.Account
	err := store.InTx(ctx, func(q service.Queries) (err error) {
		a, _, err = q.CreateAccount(ctx, domain.NewAccount(uuid.Must(uuid.NewV7()), now), uuid.New())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestIntegrationRepository(t *testing.T) {
	pool, provider := setup(t)
	store := postgres.New(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	var q service.Queries
	_ = store.Run(ctx, func(qq service.Queries) error { q = qq; return nil }) // autocommit queries

	t.Run("create account is idempotent", func(t *testing.T) {
		reqID := uuid.New()
		create := func() (domain.Account, bool) {
			t.Helper()
			var (
				a       domain.Account
				created bool
			)
			err := store.InTx(ctx, func(q service.Queries) (err error) {
				a, created, err = q.CreateAccount(ctx, domain.NewAccount(uuid.Must(uuid.NewV7()), now), reqID)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			return a, created
		}
		first, created1 := create()
		second, created2 := create()
		if !created1 || created2 || first.ID != second.ID || first.Version != 1 ||
			len(first.Roles) != 0 || first.Status != domain.AccountActive {
			t.Fatalf("first=%+v (%v) second=%+v (%v)", first, created1, second, created2)
		}
	})

	a := createAccount(t, store, now)
	t.Run("outbox", func(t *testing.T) {
		msg, err := events.NewAccountEvent(events.AccountChange{
			Account: a, Event: &eventsv1.AccountCreated{}, OccurredAt: now, CorrelationID: "it", Actor: auth.ServiceActor(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.EnqueueOutbox(ctx, msg); err != nil {
			t.Fatal(err)
		}
		n, err := store.ProcessOutbox(ctx, 10, func(context.Context, []events.Message) error { return errors.New("kafka down") })
		if err == nil || n != 1 {
			t.Fatalf("failed publish: n=%d err=%v", n, err)
		}
		var got []events.Message
		n, err = store.ProcessOutbox(ctx, 10, func(_ context.Context, ms []events.Message) error { got = ms; return nil })
		if err != nil || n != 1 || got[0].ID != msg.ID || got[0].Headers["event-id"] != msg.ID.String() ||
			string(got[0].Value) != string(msg.Value) {
			t.Fatalf("publish: n=%d err=%v got=%+v", n, err, got)
		}
		if n, err := store.ProcessOutbox(ctx, 10, func(context.Context, []events.Message) error { return nil }); err != nil || n != 0 {
			t.Fatalf("published twice: n=%d err=%v", n, err)
		}
		if purged, err := store.PurgePublished(ctx, time.Now().Add(time.Hour)); err != nil || purged != 1 {
			t.Fatalf("purge: %d %v", purged, err)
		}
	})

	t.Run("service end to end", func(t *testing.T) {
		svc := service.New(store)
		acct, err := svc.CreateAccount(ctx, auth.Principal{Caller: auth.CallerAuthGateway, RequestID: "it"}, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		p := auth.Principal{UserID: acct.ID, RequestID: "it"}
		got, err := svc.GetAccount(ctx, p, "")
		if err != nil || len(got.Roles) != 0 || got.Version != 1 || got.Status != domain.AccountActive {
			t.Fatalf("account: %+v %v", got, err)
		}
		var topic, eventType string
		if err := pool.QueryRow(ctx, `SELECT topic, event_type FROM outbox
WHERE published_at IS NULL AND record_key = $1 ORDER BY seq`, acct.ID.String()).Scan(&topic, &eventType); err != nil ||
			topic != events.TopicAccount || eventType != events.AccountCreatedType {
			t.Fatalf("outbox = %s %s, %v; want AccountCreated", topic, eventType, err)
		}
	})

	t.Run("migration 00002 drops verification and premium", func(t *testing.T) {
		exists := func(table string) bool {
			t.Helper()
			var found bool
			if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&found); err != nil {
				t.Fatal(err)
			}
			return found
		}
		if exists("farmer_verifications") {
			t.Fatal("00002 must drop farmer_verifications")
		}
		legacy := createAccount(t, store, now)
		res, err := provider.Down(ctx)
		if err != nil || res.Source.Version != 2 {
			t.Fatalf("down = %v, %v; want migration 2 rolled back", res, err)
		}
		if !exists("farmer_verifications") {
			t.Fatal("down must restore the 00001 schema")
		}
		// Roles granted before the pivot are removed on the way up; ADMIN stays.
		if _, err := pool.Exec(ctx, `INSERT INTO account_roles (account_id, role, granted_at)
VALUES ($1, 'RESELLER', now()), ($1, 'FARMER', now()), ($1, 'ADMIN', now())`, legacy.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up again: %v", err)
		}
		acct := must[domain.Account](t)(q.GetAccount(ctx, legacy.ID))
		if len(acct.Roles) != 1 || acct.Roles[0] != domain.RoleAdmin {
			t.Fatalf("roles after 00002 = %v; want [ADMIN]", acct.Roles)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO account_roles (account_id, role, granted_at) VALUES ($1, 'FARMER', now())`,
			legacy.ID); err == nil {
			t.Fatal("FARMER must violate account_roles_role_check")
		}
	})

	t.Run("migrations roll back and forward", func(t *testing.T) {
		if _, err := provider.Down(ctx); err != nil {
			t.Fatalf("down: %v", err)
		}
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up again: %v", err)
		}
		statuses, err := provider.Status(ctx)
		if err != nil || len(statuses) == 0 || statuses[len(statuses)-1].State != goose.StateApplied {
			t.Fatalf("status: %v %v", statuses, err)
		}
	})
}
