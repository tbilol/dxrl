//go:build integration

// Integration tests against a real PostgreSQL. They run only with
// `-tags=integration` and PROFILE_SOCIAL_TEST_DATABASE_URL set (make
// test-integration loads it from .env). Each run migrates a fresh temporary
// schema (selected through search_path) and drops it afterwards, so the
// target database is left as it was.
package postgres_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
	"github.com/tbilol/dxrl/services/profile-social/internal/events"
	"github.com/tbilol/dxrl/services/profile-social/internal/repository/postgres"
	"github.com/tbilol/dxrl/services/profile-social/internal/service"
	"github.com/tbilol/dxrl/services/profile-social/migrations"
)

var ctx = context.Background()

// setup creates a temporary schema, migrates it and returns a pool whose
// search_path points at it.
func setup(t *testing.T) (*pgxpool.Pool, *goose.Provider) {
	t.Helper()
	url := os.Getenv("PROFILE_SOCIAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PROFILE_SOCIAL_TEST_DATABASE_URL is not set")
	}
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "profile_social_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
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
	cfg.MaxConns = 20
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

func principal() auth.Principal {
	return auth.Principal{UserID: uuid.Must(uuid.NewV7()), Roles: []commonv1.Role{domain.RoleReseller}, RequestID: uuid.NewString()}
}

func mustCreate(t *testing.T, svc *service.Service, p auth.Principal, name string) domain.Profile {
	t.Helper()
	prof, err := svc.CreateProfile(ctx, p, uuid.NewString(), domain.ProfileFields{
		DisplayName: name, Region: commonv1.Region_REGION_QORAQALPOGISTON, District: "Nukus", AvatarMediaID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	return prof
}

func account(user uuid.UUID, version int64, farmer bool) domain.AccountUpdate {
	roles := []commonv1.Role{domain.RoleReseller}
	if farmer {
		roles = append(roles, domain.RoleFarmer)
	}
	return domain.AccountUpdate{
		EventID: uuid.Must(uuid.NewV7()), EventType: "agrogram.events.v1.AccountRoleChanged", Topic: events.TopicAccount,
		OccurredAt: time.Now(), UserID: user,
		Account: domain.Account{Roles: roles, IsVerifiedFarmer: farmer, Version: version},
	}
}

// completion is a decoded COMPLETED transaction-stream record for tx:
// eventType at version, with a fresh event id.
func completion(tx domain.CompletedTransaction, eventType string, version int64) domain.TransactionUpdate {
	return domain.TransactionUpdate{
		EventID: uuid.Must(uuid.NewV7()), EventType: "agrogram.events.v1." + eventType, Topic: events.TopicTransaction,
		OccurredAt: time.Now(), Version: version, Transaction: tx,
	}
}

func wantReason(t *testing.T, err error, reason commonv1.ErrorReason) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Reason != reason {
		t.Fatalf("err = %v, want %s", err, reason)
	}
}

func TestIntegrationRepository(t *testing.T) {
	pool, provider := setup(t)
	store := postgres.New(pool)
	svc := service.New(store, nil, nil)

	t.Run("account projection: dedupe, version guard, pending rows", func(t *testing.T) {
		p := principal()
		first := account(p.UserID, 1, false)
		for i, want := range []domain.ApplyOutcome{domain.Applied, domain.Duplicate} {
			if got, err := svc.ApplyAccountEvent(ctx, first); err != nil || got != want {
				t.Fatalf("delivery %d = %s, %v; want %s", i+1, got, err, want)
			}
		}
		if got, err := svc.ApplyAccountEvent(ctx, account(p.UserID, 3, true)); err != nil || got != domain.Applied {
			t.Fatalf("v3 = %s, %v", got, err)
		}
		if got, err := svc.ApplyAccountEvent(ctx, account(p.UserID, 2, false)); err != nil || got != domain.Stale {
			t.Fatalf("v2 after v3 = %s, %v", got, err)
		}
		_, err := svc.GetProfile(ctx, p, p.UserID.String())
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND) // PENDING
		prof := mustCreate(t, svc, p, "Qoraqalpoq")
		if !prof.Public() || prof.Account.Version != 3 || !prof.Account.IsVerifiedFarmer || prof.Version != 1 {
			t.Fatalf("created = %+v", prof)
		}

		// CreateProfile before any event, then the event.
		early := principal()
		mustCreate(t, svc, early, "Early")
		if _, err := svc.ApplyAccountEvent(ctx, account(early.UserID, 1, true)); err != nil {
			t.Fatal(err)
		}
		got, err := svc.GetProfile(ctx, early, early.UserID.String())
		if err != nil || got.DisplayName != "Early" || !got.Account.IsVerifiedFarmer || got.Version != 1 {
			t.Fatalf("after event = %+v, %v", got, err)
		}
		if n, err := store.PurgeProcessedEvents(ctx, time.Now().Add(time.Hour)); err != nil || n < 4 {
			t.Fatalf("purged %d, %v", n, err)
		}
	})

	t.Run("follows: counters, concurrency, delete", func(t *testing.T) {
		star := principal()
		mustCreate(t, svc, star, "Star")
		fans := make([]auth.Principal, 12)
		for i := range fans {
			fans[i] = principal()
			mustCreate(t, svc, fans[i], "Fan")
		}
		// Every fan follows twice, concurrently, and the star follows back:
		// counters must be exact and nothing may deadlock.
		var wg sync.WaitGroup
		errs := make(chan error, 3*len(fans))
		for _, f := range fans {
			for range 2 {
				wg.Go(func() {
					_, err := svc.FollowUser(ctx, f, star.UserID.String())
					errs <- err
				})
			}
			wg.Go(func() {
				_, err := svc.FollowUser(ctx, star, f.UserID.String())
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("follow: %v", err)
			}
		}
		got, err := svc.GetProfile(ctx, fans[0], star.UserID.String())
		if err != nil || got.FollowerCount != 12 || got.FollowingCount != 12 || !got.IsFollowedByMe {
			t.Fatalf("star = %+v, %v", got, err)
		}

		// Keyset pagination over real timestamps.
		seen := map[uuid.UUID]bool{}
		token := ""
		for {
			page, err := svc.ListFollowers(ctx, star, star.UserID.String(), domain.PageRequest{Size: 5, Token: token})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range page.Profiles {
				if seen[p.ID] {
					t.Fatalf("%s listed twice", p.ID)
				}
				seen[p.ID] = true
			}
			if token = page.NextPageToken; token == "" {
				break
			}
		}
		if len(seen) != 12 {
			t.Fatalf("listed %d followers, want 12", len(seen))
		}

		if _, err := svc.UnfollowUser(ctx, fans[1], star.UserID.String()); err != nil {
			t.Fatal(err)
		}
		if err := svc.DeleteProfile(ctx, star, ""); err != nil {
			t.Fatal(err)
		}
		for _, f := range fans {
			got, err := svc.GetProfile(ctx, f, f.UserID.String())
			if err != nil || got.FollowerCount != 0 || got.FollowingCount != 0 {
				t.Fatalf("fan after the star's deletion = %+v, %v", got, err)
			}
		}
		var edges int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM follows WHERE follower_id = $1 OR followee_id = $1`,
			star.UserID).Scan(&edges); err != nil || edges != 0 {
			t.Fatalf("edges left = %d, %v", edges, err)
		}
		_, err = svc.FollowUser(ctx, fans[0], star.UserID.String())
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	})

	t.Run("ratings: eligibility, once per transaction, aggregate and outbox", func(t *testing.T) {
		farmer, reseller := principal(), principal()
		mustCreate(t, svc, farmer, "Farmer")
		mustCreate(t, svc, reseller, "Reseller")
		_, err := svc.RateUser(ctx, reseller, uuid.NewString(), uuid.NewString(), 5, "")
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_RATING_NOT_ALLOWED)

		// PayoutReleased records the transaction; its redelivery is a
		// duplicate and BonusAwarded (a new event id) is stale.
		tx := domain.CompletedTransaction{ID: uuid.New(), FarmerID: farmer.UserID, ResellerID: reseller.UserID, CompletedAt: time.Now()}
		payout := completion(tx, "PayoutReleased", 6)
		for i, step := range []struct {
			u    domain.TransactionUpdate
			want domain.ApplyOutcome
		}{
			{payout, domain.Applied},
			{payout, domain.Duplicate},
			{completion(tx, "BonusAwarded", 7), domain.Stale},
		} {
			if got, err := svc.RecordCompletedTransaction(ctx, step.u); err != nil || got != step.want {
				t.Fatalf("record %d = %s, %v; want %s", i+1, got, err, step.want)
			}
		}
		var (
			topic, eventType string
			aggregateID      uuid.UUID
			aggregateVersion int64
			inboxRows        int
		)
		// event_id is the inbox's primary key: one row, however many deliveries.
		if err := pool.QueryRow(ctx, `
SELECT topic, event_type, aggregate_id, aggregate_version FROM processed_events WHERE event_id = $1`, payout.EventID).
			Scan(&topic, &eventType, &aggregateID, &aggregateVersion); err != nil || topic != events.TopicTransaction ||
			eventType != payout.EventType || aggregateID != tx.ID || aggregateVersion != 6 {
			t.Fatalf("inbox row of PayoutReleased = %q, %q, %s, %d, %v", topic, eventType, aggregateID, aggregateVersion, err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM processed_events WHERE aggregate_id = $1`, tx.ID).
			Scan(&inboxRows); err != nil || inboxRows != 2 {
			t.Fatalf("inbox rows of the transaction = %d, %v; want 2 (PayoutReleased once, BonusAwarded)", inboxRows, err)
		}
		for _, p := range []auth.Principal{farmer, reseller} {
			if prof, err := svc.GetProfile(ctx, p, p.UserID.String()); err != nil || prof.DealsCount != 1 {
				t.Fatalf("deals of %s = %d, %v; want 1", p.UserID, prof.DealsCount, err)
			}
		}
		reqID := uuid.NewString()
		r, err := svc.RateUser(ctx, reseller, reqID, tx.ID.String(), 4, "yaxshi")
		if err != nil {
			t.Fatal(err)
		}
		if again, err := svc.RateUser(ctx, reseller, reqID, tx.ID.String(), 1, ""); err != nil || again.ID != r.ID {
			t.Fatalf("retry = %+v, %v", again, err)
		}
		_, err = svc.RateUser(ctx, reseller, uuid.NewString(), tx.ID.String(), 3, "")
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_ALREADY_RATED)

		prof, err := svc.GetProfile(ctx, reseller, farmer.UserID.String())
		if err != nil || prof.RatingCount != 1 || prof.RatingAverage() != 4 || prof.DealsCount != 1 || prof.Version != 2 {
			t.Fatalf("farmer = %+v, %v", prof, err)
		}
		page, err := svc.ListRatings(ctx, auth.Principal{}, farmer.UserID.String(), domain.PageRequest{})
		if err != nil || len(page.Ratings) != 1 || page.Ratings[0].Comment != "yaxshi" || page.Count != 1 {
			t.Fatalf("ratings = %+v, %v", page, err)
		}

		// The outbox holds one RatingUpdated for the farmer; the relay's store publishes it.
		var published []events.Message
		n, err := store.ProcessOutbox(ctx, 100, func(_ context.Context, msgs []events.Message) error {
			published = append(published, msgs...)
			return nil
		})
		if err != nil || n != 1 || published[0].Topic != events.TopicProfile || published[0].Key != farmer.UserID.String() {
			t.Fatalf("outbox = %d, %v, %+v", n, err, published)
		}
		var env eventsv1.ProfileEvent
		if err := proto.Unmarshal(published[0].Value, &env); err != nil || env.GetRatingUpdated().GetScore() != 4 ||
			env.GetMetadata().GetAggregateVersion() != 2 {
			t.Fatalf("event = %v, %v", &env, err)
		}
		if n, err := store.ProcessOutbox(ctx, 100, func(context.Context, []events.Message) error { return nil }); err != nil || n != 0 {
			t.Fatalf("second pass = %d, %v", n, err)
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

	t.Run("an unreachable database is transient", func(t *testing.T) {
		cfg := pool.Config().Copy()
		cfg.ConnConfig.Port = 1 // nothing listens there
		cfg.ConnConfig.Fallbacks = nil
		cfg.ConnConfig.ConnectTimeout = time.Second
		dead, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer dead.Close()
		deadSvc := service.New(postgres.New(dead), nil, nil)
		_, err = deadSvc.ApplyAccountEvent(ctx, account(uuid.New(), 1, false))
		if err == nil || !postgres.IsTransient(err) {
			t.Fatalf("account event: err = %v, want a transient error", err)
		}
		_, err = deadSvc.RecordCompletedTransaction(ctx, completion(domain.CompletedTransaction{
			ID: uuid.New(), FarmerID: uuid.New(), ResellerID: uuid.New(), CompletedAt: time.Now(),
		}, "PayoutReleased", 6))
		if err == nil || !postgres.IsTransient(err) {
			t.Fatalf("transaction event: err = %v, want a transient error", err)
		}
	})
}
