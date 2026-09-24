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
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/identity/internal/auth"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/eijara"
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
	today := domain.TashkentDate(now)

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
			len(first.Roles) != 1 || first.Roles[0] != domain.RoleReseller || first.Status != domain.AccountActive {
			t.Fatalf("first=%+v (%v) second=%+v (%v)", first, created1, second, created2)
		}
	})

	a, b := createAccount(t, store, now), createAccount(t, store, now)
	lease := &domain.Lease{ContractNumber: "IT-1", ValidFrom: today, ValidUntil: today.AddDate(1, 0, 0)}

	pending := func(user uuid.UUID, contract string, next time.Time) domain.Verification {
		t.Helper()
		v, inserted, err := q.InsertVerification(ctx, domain.Verification{
			ID: uuid.Must(uuid.NewV7()), UserID: user, RequestID: uuid.New(), ContractNumber: contract,
			Status: domain.VerificationPending, NextAttemptAt: next, SubmittedAt: now,
		})
		if err != nil || !inserted {
			t.Fatalf("insert: inserted=%v err=%v", inserted, err)
		}
		return v
	}

	t.Run("verification lifecycle and the partial unique index", func(t *testing.T) {
		va := pending(a.ID, "IT-1", now.Add(time.Hour))
		again, inserted, err := q.InsertVerification(ctx, domain.Verification{
			ID: uuid.New(), UserID: a.ID, RequestID: va.RequestID, ContractNumber: "IT-1", NextAttemptAt: now, SubmittedAt: now,
		})
		if err != nil || inserted || again.ID != va.ID {
			t.Fatalf("idempotent insert: %+v inserted=%v err=%v", again, inserted, err)
		}
		if acct := must[domain.Account](t)(q.GetAccount(ctx, a.ID)); acct.FarmerVerificationStatus != domain.VerificationPending {
			t.Fatalf("account status = %s", acct.FarmerVerificationStatus)
		}

		err = store.InTx(ctx, func(q service.Queries) error {
			if err := q.LockAccount(ctx, a.ID); err != nil {
				return err
			}
			ok, err := q.DecideVerification(ctx, domain.Decision{VerificationID: va.ID, Status: domain.VerificationVerified, Lease: lease, DecidedAt: now})
			if err != nil || !ok {
				return fmt.Errorf("decide: ok=%v err=%v", ok, err)
			}
			granted, err := q.GrantRole(ctx, a.ID, domain.RoleFarmer, now)
			if err != nil || !granted {
				return fmt.Errorf("grant: %v %v", granted, err)
			}
			granted, err = q.GrantRole(ctx, a.ID, domain.RoleFarmer, now)
			if err != nil || granted {
				return fmt.Errorf("second grant: %v %v", granted, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		acct := must[domain.Account](t)(q.GetAccount(ctx, a.ID))
		if !acct.HasRole(domain.RoleFarmer) || acct.Version != 2 || acct.FarmerVerificationStatus != domain.VerificationVerified {
			t.Fatalf("account = %+v", acct)
		}
		got := must[domain.Verification](t)(q.GetVerification(ctx, va.ID))
		if got.Lease == nil || !got.Lease.ValidFrom.Equal(lease.ValidFrom) || !got.Lease.ValidUntil.Equal(lease.ValidUntil) || got.DecidedAt == nil {
			t.Fatalf("verified = %+v", got)
		}
		if ok, err := q.DecideVerification(ctx, domain.Decision{VerificationID: va.ID, Status: domain.VerificationRejected,
			RejectionReason: domain.ReasonContractInvalid, DecidedAt: now}); err != nil || ok {
			t.Fatalf("second decision: ok=%v err=%v", ok, err)
		}

		vb := pending(b.ID, "IT-1", now.Add(time.Hour))
		if claimed := must[bool](t)(q.ContractClaimed(ctx, "IT-1", b.ID)); !claimed {
			t.Fatal("IT-1 should be claimed for b")
		}
		if claimed := must[bool](t)(q.ContractClaimed(ctx, "IT-1", a.ID)); claimed {
			t.Fatal("IT-1 is a's own contract")
		}
		_, err = q.DecideVerification(ctx, domain.Decision{VerificationID: vb.ID, Status: domain.VerificationVerified, Lease: lease, DecidedAt: now})
		if !errors.Is(err, domain.ErrContractClaimed) {
			t.Fatalf("partial unique index: %v", err)
		}
		if ok, err := q.DecideVerification(ctx, domain.Decision{VerificationID: vb.ID, Status: domain.VerificationRejected,
			RejectionReason: domain.ReasonContractClaimed, DecidedAt: now}); err != nil || !ok {
			t.Fatalf("reject b: ok=%v err=%v", ok, err)
		}
		if got := must[domain.Verification](t)(q.LatestVerification(ctx, b.ID)); got.RejectionReason != domain.ReasonContractClaimed {
			t.Fatalf("b = %+v", got)
		}
	})

	t.Run("claim pending with backoff", func(t *testing.T) {
		c := createAccount(t, store, now)
		v := pending(c.ID, "IT-9", now.Add(-time.Minute))
		claimed := must[[]domain.Verification](t)(q.ClaimPendingVerifications(ctx, now, 10, 30*time.Second, time.Hour))
		if len(claimed) != 1 || claimed[0].ID != v.ID || claimed[0].Attempts != 1 || !claimed[0].NextAttemptAt.Equal(now.Add(30*time.Second)) {
			t.Fatalf("first claim = %+v", claimed)
		}
		if again := must[[]domain.Verification](t)(q.ClaimPendingVerifications(ctx, now, 10, 30*time.Second, time.Hour)); len(again) != 0 {
			t.Fatalf("claimed twice: %+v", again)
		}
		later := now.Add(31 * time.Second)
		claimed = must[[]domain.Verification](t)(q.ClaimPendingVerifications(ctx, later, 10, 30*time.Second, time.Hour))
		if len(claimed) != 1 || claimed[0].Attempts != 2 || !claimed[0].NextAttemptAt.Equal(later.Add(time.Minute)) {
			t.Fatalf("second claim = %+v", claimed)
		}
		if err := q.RecordVerificationError(ctx, v.ID, "e-ijara: timeout"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("expiry revokes the farmer role", func(t *testing.T) {
		after := lease.ValidUntil.AddDate(0, 0, 1)
		due := must[[]domain.Verification](t)(q.ExpiryCandidates(ctx, after, 10))
		if len(due) != 1 || due[0].UserID != a.ID {
			t.Fatalf("candidates = %+v", due)
		}
		if none := must[[]domain.Verification](t)(q.ExpiryCandidates(ctx, lease.ValidUntil, 10)); len(none) != 0 {
			t.Fatalf("expired on the last day: %+v", none)
		}
		err := store.InTx(ctx, func(q service.Queries) error {
			if err := q.LockAccount(ctx, a.ID); err != nil {
				return err
			}
			v, ok, err := q.ExpireVerification(ctx, due[0].ID, after, now)
			if err != nil || !ok || v.Status != domain.VerificationExpired {
				return fmt.Errorf("expire: %+v %v %v", v, ok, err)
			}
			if revoked, err := q.RevokeFarmerRoleUnlessVerified(ctx, a.ID, now); err != nil || !revoked {
				return fmt.Errorf("revoke: %v %v", revoked, err)
			}
			if revoked, err := q.RevokeFarmerRoleUnlessVerified(ctx, a.ID, now); err != nil || revoked {
				return fmt.Errorf("second revoke: %v %v", revoked, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		acct := must[domain.Account](t)(q.GetAccount(ctx, a.ID))
		if acct.HasRole(domain.RoleFarmer) || acct.Version != 3 || acct.FarmerVerificationStatus != domain.VerificationExpired {
			t.Fatalf("account = %+v", acct)
		}
		if _, ok, err := q.ExpireVerification(ctx, due[0].ID, after, now); err != nil || ok {
			t.Fatalf("expired twice: %v %v", ok, err)
		}
	})

	t.Run("set plan", func(t *testing.T) {
		c := createAccount(t, store, now)
		later := now.Add(time.Minute)
		if err := q.SetPlan(ctx, c.ID, domain.PlanPremium, later); err != nil {
			t.Fatal(err)
		}
		got := must[domain.Account](t)(q.GetAccount(ctx, c.ID))
		if got.Plan != domain.PlanPremium || got.PremiumUntil != nil || got.Version != 2 || !got.UpdatedAt.Equal(later) {
			t.Fatalf("account = %+v", got)
		}
		if err := q.SetPlan(ctx, uuid.New(), domain.PlanDefault, now); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing account: %v", err)
		}
	})

	t.Run("outbox", func(t *testing.T) {
		msg, err := events.NewNotificationRequest(events.Notification{
			RecipientID: a.ID, Category: eventsv1.NotificationCategory_NOTIFICATION_CATEGORY_ACCOUNT,
			TemplateKey: events.TemplateFarmerVerified, Params: map[string]string{"lease_contract_number": "IT-1"},
			OccurredAt: now, CorrelationID: "it", Actor: auth.ServiceActor(),
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
		svc := service.New(store, eijara.Stub{}, nil, service.Config{EIjaraTimeout: time.Second})
		acct, err := svc.CreateAccount(ctx, auth.Principal{Caller: auth.CallerAuthGateway, RequestID: "it"}, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		p := auth.Principal{UserID: acct.ID, RequestID: "it"}
		v, err := svc.SubmitFarmerVerification(ctx, p, uuid.NewString(), "it-e2e-1")
		if err != nil || v.Status != domain.VerificationVerified {
			t.Fatalf("submit: %+v %v", v, err)
		}
		v, err = svc.SubmitFarmerVerification(ctx, p, uuid.NewString(), "INVALID-1")
		if err == nil {
			t.Fatalf("already verified accepted: %+v", v)
		}
		got, err := svc.GetAccount(ctx, p, "")
		if err != nil || !got.HasRole(commonv1.Role_ROLE_FARMER) || got.Version != 2 {
			t.Fatalf("account: %+v %v", got, err)
		}
		// The outbox, in commit order, all keyed by the user: the account
		// stream and the notification request of the VERIFIED decision.
		type outboxRow struct{ Topic, EventType string }
		outboxRows := func() []outboxRow {
			t.Helper()
			rows, err := pool.Query(ctx, `SELECT topic, event_type FROM outbox
WHERE published_at IS NULL AND record_key = $1 ORDER BY seq`, acct.ID.String())
			if err != nil {
				t.Fatal(err)
			}
			got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[outboxRow])
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		want := []outboxRow{
			{events.TopicAccount, events.AccountCreatedType},
			{events.TopicAccount, events.FarmerVerifiedType},
			{events.TopicAccount, events.AccountRoleChangedType},
			{events.TopicNotificationRequest, events.NotificationRequestedType},
		}
		if got := outboxRows(); !slices.Equal(got, want) {
			t.Fatalf("outbox = %v, want %v", got, want)
		}

		// Premium on, then the same value again: one change, one event.
		escrow := auth.Principal{Caller: auth.CallerOrderEscrow, RequestID: "it"}
		on, err := svc.SetAccountPremium(ctx, escrow, acct.ID.String(), true)
		if err != nil || !on.IsPremium || on.Plan != domain.PlanPremium || on.PremiumUntil != nil || on.Version != 3 {
			t.Fatalf("premium on: %+v %v", on, err)
		}
		again, err := svc.SetAccountPremium(ctx, escrow, acct.ID.String(), true)
		if err != nil || !again.IsPremium || again.Version != 3 {
			t.Fatalf("premium on again: %+v %v", again, err)
		}
		want = append(want, outboxRow{events.TopicAccount, events.AccountPremiumChangedType})
		if got := outboxRows(); !slices.Equal(got, want) {
			t.Fatalf("outbox = %v, want %v", got, want)
		}
		if _, err := svc.SetAccountPremium(ctx, escrow, uuid.NewString(), true); err == nil {
			t.Fatal("unknown account accepted")
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
