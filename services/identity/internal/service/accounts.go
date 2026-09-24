package service

import (
	"context"
	"errors"
	"fmt"

	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/identity/internal/auth"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

// CreateAccount creates the account for a first sign-in: ACTIVE, ROLE_RESELLER,
// the Default plan, version 1, and AccountCreated in the same transaction.
// Internal: auth-gateway only. A retry with the same request id returns the
// same account and publishes nothing.
func (s *Service) CreateAccount(ctx context.Context, p auth.Principal, requestID string) (domain.Account, error) {
	if err := auth.RequireCaller(p, auth.CallerAuthGateway); err != nil {
		return domain.Account{}, err
	}
	reqID, err := parseRequestID(requestID)
	if err != nil {
		return domain.Account{}, err
	}
	id, err := newID()
	if err != nil {
		return domain.Account{}, err
	}
	var (
		acct    domain.Account
		created bool
	)
	err = s.store.InTx(ctx, func(q Queries) error {
		var err error
		acct, created, err = q.CreateAccount(ctx, domain.NewAccount(id, s.clock()), reqID)
		if err != nil || !created {
			return err // a retry: the first call published AccountCreated
		}
		// The person signing in is the actor; auth-gateway relays the sign-in.
		tr := requestTrigger(p)
		tr.actor = auth.UserActor(acct.ID)
		return s.enqueueAccountEvent(ctx, q, acct, &eventsv1.AccountCreated{}, acct.CreatedAt, tr)
	})
	if err != nil {
		return domain.Account{}, fmt.Errorf("create account: %w", err)
	}
	if created {
		s.log.InfoContext(ctx, "account created", "user_id", acct.ID, "request_id", p.RequestID)
	}
	return s.present(acct), nil
}

// GetAccount returns an account's roles, farmer verification status and plan.
// Empty userID means the caller.
func (s *Service) GetAccount(ctx context.Context, p auth.Principal, userID string) (domain.Account, error) {
	target, err := auth.ReadTarget(p, userID)
	if err != nil {
		return domain.Account{}, err
	}
	var acct domain.Account
	err = s.store.Run(ctx, func(q Queries) error {
		acct, err = q.GetAccount(ctx, target)
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return domain.Account{}, notFound("account", target)
	case err != nil:
		return domain.Account{}, fmt.Errorf("get account: %w", err)
	}
	return s.present(acct), nil
}

// SetAccountPremium turns Premium on (plan PREMIUM, no end date) or off (plan
// DEFAULT). Internal: order-escrow only. The plan, the version bump and
// AccountPremiumChanged commit together. Asking for the value the account
// already has (Premium active or not, right now) is a no-op: no version bump
// and no event.
func (s *Service) SetAccountPremium(ctx context.Context, p auth.Principal, userID string, premium bool) (domain.Account, error) {
	if err := auth.RequireCaller(p, auth.CallerOrderEscrow); err != nil {
		return domain.Account{}, err
	}
	target, err := parseID("user_id", userID)
	if err != nil {
		return domain.Account{}, err
	}
	var (
		acct    domain.Account
		changed bool
	)
	err = s.store.InTx(ctx, func(q Queries) error {
		if err := q.LockAccount(ctx, target); err != nil {
			return err
		}
		cur, err := q.GetAccount(ctx, target)
		if err != nil {
			return err
		}
		now := s.clock()
		if cur.PremiumActive(now) == premium {
			acct = cur
			return nil
		}
		plan := domain.PlanDefault
		if premium {
			plan = domain.PlanPremium
		}
		if err := q.SetPlan(ctx, target, plan, now); err != nil {
			return err
		}
		if acct, err = q.GetAccount(ctx, target); err != nil {
			return err
		}
		changed = true
		return s.enqueueAccountEvent(ctx, q, acct, &eventsv1.AccountPremiumChanged{IsPremium: premium}, now, requestTrigger(p))
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return domain.Account{}, notFound("account", target)
	case err != nil:
		return domain.Account{}, fmt.Errorf("set account premium: %w", err)
	}
	if changed {
		s.log.InfoContext(ctx, "premium changed", "user_id", target, "is_premium", premium, "caller", p.Caller)
	}
	return s.present(acct), nil
}

// present fills the fields computed from the clock.
//
// TODO(billing): purchase and renewal of Premium are not designed.
// SetAccountPremium is the on/off switch they will use; nothing in identity
// sets `premium_until` (only SQL by hand), and no event is published when a
// hand-set `premium_until` passes.
func (s *Service) present(a domain.Account) domain.Account {
	a.IsPremium = a.PremiumActive(s.clock())
	return a
}
