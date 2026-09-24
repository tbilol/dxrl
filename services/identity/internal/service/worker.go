package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/events"
)

// RunWorker retries PENDING verifications and expires ended leases every
// WorkerInterval until ctx is canceled. Several instances may run: rows are
// claimed with FOR UPDATE SKIP LOCKED and every change is guarded by status.
func (s *Service) RunWorker(ctx context.Context) {
	t := time.NewTicker(s.cfg.WorkerInterval)
	defer t.Stop()
	for {
		if n, err := s.RetryPending(ctx); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "retrying pending verifications failed", "error", err)
		} else if n > 0 {
			s.log.InfoContext(ctx, "pending verifications decided", "count", n)
		}
		if n, err := s.ExpireVerifications(ctx); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "expiring verifications failed", "error", err)
		} else if n > 0 {
			s.log.InfoContext(ctx, "verifications expired", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RetryPending asks e-ijara again about PENDING verifications that are due
// and returns how many got decided. Each claim counts an attempt and pushes
// the next one out with exponential backoff.
func (s *Service) RetryPending(ctx context.Context) (int, error) {
	var due []domain.Verification
	err := s.store.Run(ctx, func(q Queries) error {
		var err error
		due, err = q.ClaimPendingVerifications(ctx, s.clock(), s.cfg.WorkerBatchSize, s.cfg.RetryBackoff, s.cfg.RetryBackoffMax)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("claim pending verifications: %w", err)
	}
	decided := 0
	var errs []error
	for _, v := range due {
		if ctx.Err() != nil {
			break
		}
		res, err := s.check(ctx, v.ContractNumber)
		if err != nil {
			s.log.WarnContext(ctx, "e-ijara retry gave no answer", "verification_id", v.ID,
				"attempts", v.Attempts, "next_attempt_at", v.NextAttemptAt, "error", err)
			if rerr := s.store.Run(ctx, func(q Queries) error {
				return q.RecordVerificationError(ctx, v.ID, err.Error())
			}); rerr != nil {
				errs = append(errs, rerr)
			}
			continue
		}
		if _, err := s.decide(ctx, v, res, workerTrigger()); err != nil {
			errs = append(errs, err)
			continue
		}
		decided++
	}
	return decided, errors.Join(errs...)
}

// ExpireVerifications marks VERIFIED verifications whose lease ended before
// today (Asia/Tashkent) EXPIRED, revokes ROLE_FARMER unless another VERIFIED
// verification remains, and requests a notification, one transaction each.
// Each transaction also writes FarmerVerificationExpired and, when the role
// was revoked, AccountRoleChanged to the outbox.
func (s *Service) ExpireVerifications(ctx context.Context) (int, error) {
	today := domain.TashkentDate(s.clock())
	var due []domain.Verification
	err := s.store.Run(ctx, func(q Queries) error {
		var err error
		due, err = q.ExpiryCandidates(ctx, today, s.cfg.WorkerBatchSize)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("list expired verifications: %w", err)
	}
	expired := 0
	var errs []error
	for _, c := range due {
		if ctx.Err() != nil {
			break
		}
		now, tr := s.clock(), workerTrigger()
		var done, revoked bool
		err := s.store.InTx(ctx, func(q Queries) error {
			if err := q.LockAccount(ctx, c.UserID); err != nil {
				return err
			}
			v, ok, err := q.ExpireVerification(ctx, c.ID, today, now)
			if err != nil || !ok {
				return err
			}
			// Expiry itself leaves the account row (and its version) as it is.
			acct, err := q.GetAccount(ctx, v.UserID)
			if err != nil {
				return err
			}
			expiredEvent, err := events.VerificationEvent(v)
			if err != nil {
				return err
			}
			if err := s.enqueueAccountEvent(ctx, q, acct, expiredEvent, now, tr); err != nil {
				return err
			}
			if revoked, err = q.RevokeFarmerRoleUnlessVerified(ctx, v.UserID, now); err != nil {
				return err
			}
			if revoked {
				if err := s.enqueueRoleChange(ctx, q, acct, v.ID, now, tr); err != nil {
					return err
				}
			}
			done = true
			return s.enqueueNotification(ctx, q, v, events.TemplateFarmerVerificationExpired,
				map[string]string{"lease_contract_number": v.ContractNumber}, now, tr)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("expire verification %s: %w", c.ID, err))
			continue
		}
		if done {
			expired++
			s.log.InfoContext(ctx, "farmer verification expired", "verification_id", c.ID,
				"user_id", c.UserID, "farmer_role_revoked", revoked)
		}
	}
	return expired, errors.Join(errs...)
}
