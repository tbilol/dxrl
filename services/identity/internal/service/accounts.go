package service

import (
	"context"
	"errors"
	"fmt"

	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/identity/internal/auth"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

// CreateAccount creates the account for a first sign-in: ACTIVE, no roles,
// version 1, and AccountCreated in the same transaction.
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
	return acct, nil
}

// GetAccount returns an account's status and roles.
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
	return acct, nil
}
