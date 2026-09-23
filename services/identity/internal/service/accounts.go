package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/tbilol/dxrl/services/identity/internal/auth"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

// CreateAccount creates the account for a first sign-in: ACTIVE, ROLE_RESELLER,
// the Default plan, version 1. Internal: auth-gateway only. A retry with the
// same request id returns the same account.
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
		acct, created, err = q.CreateAccount(ctx, domain.NewAccount(id, s.clock()), reqID)
		// TODO(events): publish AccountEvent (account created) on
		// agrogram.identity.account.v1 once events/v1/account.proto exists.
		return err
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

// present fills the fields computed from the clock.
//
// TODO(billing): purchase and renewal of Premium are not designed. Nothing in
// identity changes `plan` / `premium_until` yet (only SQL by hand); plan
// changes will also need the AccountEvent stream (see the AccountEvent TODOs).
func (s *Service) present(a domain.Account) domain.Account {
	a.IsPremium = a.PremiumActive(s.clock())
	return a
}
