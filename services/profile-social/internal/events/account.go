package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// AccountApplier applies one account update to the projection (the service).
type AccountApplier interface {
	ApplyAccountEvent(ctx context.Context, u domain.AccountUpdate) (domain.ApplyOutcome, error)
}

// AccountHandler is the RecordHandler of agrogram.identity.account.v1: it
// decodes and validates each AccountEvent and hands its snapshot to the
// applier. Every event type takes the same path (the snapshot, applied when
// its version is newer); the oneof details are not needed, so an event type
// added after this code was generated is applied like any other.
type AccountHandler struct {
	applier   AccountApplier
	validator protovalidate.Validator
	log       *slog.Logger
}

// NewAccountHandler returns the account-stream handler.
func NewAccountHandler(applier AccountApplier, validator protovalidate.Validator, log *slog.Logger) *AccountHandler {
	return &AccountHandler{applier: applier, validator: validator, log: log}
}

// HandleRecord implements RecordHandler.
func (h *AccountHandler) HandleRecord(ctx context.Context, r *kgo.Record) error {
	u, err := DecodeAccountRecord(r, h.validator)
	if err != nil {
		return err
	}
	if key := string(r.Key); key != u.UserID.String() {
		h.log.WarnContext(ctx, "account event keyed by something else than its user id; applied anyway",
			"key", key, "user_id", u.UserID, "event_id", u.EventID)
	}
	outcome, err := h.applier.ApplyAccountEvent(ctx, u)
	if err != nil {
		return fmt.Errorf("apply account event %s: %w", u.EventID, err)
	}
	h.log.DebugContext(ctx, "account event consumed", "event_id", u.EventID, "event_type", u.EventType,
		"user_id", u.UserID, "version", u.Account.Version, "outcome", outcome.String(),
		"partition", r.Partition, "offset", r.Offset)
	return nil
}

// DecodeAccountRecord decodes one record of the account stream. A record
// that is not an application/x-protobuf AccountEvent, fails protovalidate, or
// whose snapshot contradicts its metadata is poison: the error is Permanent.
// An unknown oneof case (a newer producer) is not an error.
func DecodeAccountRecord(r *kgo.Record, v protovalidate.Validator) (domain.AccountUpdate, error) {
	if ct := header(r, HeaderContentType); ct != "" && ct != ContentType {
		return domain.AccountUpdate{}, Permanent(fmt.Errorf("unsupported content-type %q", ct))
	}
	var ev eventsv1.AccountEvent
	if err := proto.Unmarshal(r.Value, &ev); err != nil {
		return domain.AccountUpdate{}, Permanent(fmt.Errorf("decode AccountEvent: %w", err))
	}
	if err := v.Validate(&ev); err != nil {
		return domain.AccountUpdate{}, Permanent(fmt.Errorf("invalid AccountEvent: %w", err))
	}
	md, snap := ev.GetMetadata(), ev.GetAccount()
	switch {
	case md.GetAggregateType() != commonv1.ResourceType_RESOURCE_TYPE_USER:
		return domain.AccountUpdate{}, Permanent(fmt.Errorf("aggregate type %s, want RESOURCE_TYPE_USER", md.GetAggregateType()))
	case snap.GetId() != md.GetAggregateId():
		return domain.AccountUpdate{}, Permanent(errors.New("account.id differs from metadata.aggregate_id"))
	case snap.GetVersion() != md.GetAggregateVersion():
		return domain.AccountUpdate{}, Permanent(fmt.Errorf("account.version %d differs from metadata.aggregate_version %d",
			snap.GetVersion(), md.GetAggregateVersion()))
	}
	// Both ids passed protovalidate's uuid rule.
	eventID, err := uuid.Parse(md.GetEventId())
	if err != nil {
		return domain.AccountUpdate{}, Permanent(fmt.Errorf("event id: %w", err))
	}
	userID, err := uuid.Parse(snap.GetId())
	if err != nil {
		return domain.AccountUpdate{}, Permanent(fmt.Errorf("account id: %w", err))
	}
	// ADMIN is the only role left. RESELLER and FARMER (granted before the
	// listings-only pivot) carry nothing and are dropped.
	roles := slices.DeleteFunc(slices.Clone(snap.GetRoles()), func(r commonv1.Role) bool { return r != domain.RoleAdmin })
	slices.Sort(roles)
	u := domain.AccountUpdate{
		EventID:    eventID,
		EventType:  md.GetEventType(),
		Topic:      r.Topic,
		OccurredAt: md.GetOccurredAt().AsTime(),
		UserID:     userID,
		Account: domain.Account{
			Roles:   slices.Compact(roles),
			Version: snap.GetVersion(),
		},
	}
	return u, nil
}
