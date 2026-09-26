package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// TransactionRecorder records completed escrow transactions (the service).
type TransactionRecorder interface {
	RecordCompletedTransaction(ctx context.Context, u domain.TransactionUpdate) (domain.ApplyOutcome, error)
}

// TransactionHandler is the RecordHandler of order-escrow's transaction
// stream, agrogram.orderescrow.transaction.v1 (TransactionEvent,
// proto/events/v1/transaction.proto). profile-social needs one fact from it:
// that a transaction is complete. PayoutReleased ("escrow paid the farmer;
// the transaction is complete. The two parties may now rate each other")
// moves the snapshot to TRANSACTION_STATE_COMPLETED, which is final, and
// later events such as BonusAwarded repeat that state.
//
// Like the account handler it reads the snapshot, never the oneof, so an
// event type added after this code was generated is handled like any other.
// Records whose snapshot is not COMPLETED (TransactionCreated up to
// ReceiptConfirmed) are skipped without touching the database. A COMPLETED
// one goes to the recorder, which de-duplicates on the event id (inbox) and
// records the transaction once per transaction id: that unlocks RateUser for
// both parties and adds one to each party's deals_count exactly once.
type TransactionHandler struct {
	recorder  TransactionRecorder
	validator protovalidate.Validator
	log       *slog.Logger
}

// NewTransactionHandler returns the transaction-stream handler.
func NewTransactionHandler(recorder TransactionRecorder, validator protovalidate.Validator, log *slog.Logger) *TransactionHandler {
	return &TransactionHandler{recorder: recorder, validator: validator, log: log}
}

// HandleRecord implements RecordHandler.
func (h *TransactionHandler) HandleRecord(ctx context.Context, r *kgo.Record) error {
	u, completed, err := DecodeTransactionRecord(r, h.validator)
	if err != nil {
		return err
	}
	if !completed {
		h.log.DebugContext(ctx, "transaction event skipped: the transaction is not completed",
			"event_id", header(r, HeaderEventID), "event_type", header(r, HeaderEventType), "key", string(r.Key),
			"partition", r.Partition, "offset", r.Offset)
		return nil
	}
	t := u.Transaction
	if key := string(r.Key); key != t.ID.String() {
		h.log.WarnContext(ctx, "transaction event keyed by something else than its transaction id; recorded anyway",
			"key", key, "transaction_id", t.ID, "event_id", u.EventID)
	}
	outcome, err := h.recorder.RecordCompletedTransaction(ctx, u)
	if err != nil {
		return fmt.Errorf("record transaction event %s (transaction %s): %w", u.EventID, t.ID, err)
	}
	h.log.DebugContext(ctx, "transaction event consumed", "event_id", u.EventID, "event_type", u.EventType,
		"transaction_id", t.ID, "version", u.Version, "outcome", outcome.String(),
		"partition", r.Partition, "offset", r.Offset)
	return nil
}

// DecodeTransactionRecord decodes one record of the transaction stream and
// reports whether its snapshot is COMPLETED; only then is the update filled
// in. A record that is not an application/x-protobuf TransactionEvent, fails
// protovalidate, or whose snapshot contradicts its metadata is poison,
// whatever its state: the error is Permanent. An unknown oneof case (a newer
// producer) is not an error.
func DecodeTransactionRecord(r *kgo.Record, v protovalidate.Validator) (domain.TransactionUpdate, bool, error) {
	if ct := header(r, HeaderContentType); ct != "" && ct != ContentType {
		return domain.TransactionUpdate{}, false, Permanent(fmt.Errorf("unsupported content-type %q", ct))
	}
	var ev eventsv1.TransactionEvent
	if err := proto.Unmarshal(r.Value, &ev); err != nil {
		return domain.TransactionUpdate{}, false, Permanent(fmt.Errorf("decode TransactionEvent: %w", err))
	}
	if err := v.Validate(&ev); err != nil {
		return domain.TransactionUpdate{}, false, Permanent(fmt.Errorf("invalid TransactionEvent: %w", err))
	}
	md, snap := ev.GetMetadata(), ev.GetTransaction()
	switch {
	case md.GetAggregateType() != commonv1.ResourceType_RESOURCE_TYPE_TRANSACTION:
		return domain.TransactionUpdate{}, false, Permanent(fmt.Errorf("aggregate type %s, want RESOURCE_TYPE_TRANSACTION",
			md.GetAggregateType()))
	case snap.GetId() != md.GetAggregateId():
		return domain.TransactionUpdate{}, false, Permanent(errors.New("transaction.id differs from metadata.aggregate_id"))
	case snap.GetVersion() != md.GetAggregateVersion():
		return domain.TransactionUpdate{}, false, Permanent(fmt.Errorf("transaction.version %d differs from metadata.aggregate_version %d",
			snap.GetVersion(), md.GetAggregateVersion()))
	}
	if snap.GetState() != eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED {
		return domain.TransactionUpdate{}, false, nil
	}
	// Every id passed protovalidate's uuid rule.
	eventID, err := uuid.Parse(md.GetEventId())
	if err != nil {
		return domain.TransactionUpdate{}, false, Permanent(fmt.Errorf("event id: %w", err))
	}
	ids := make([]uuid.UUID, 3)
	for i, s := range []string{snap.GetId(), snap.GetFarmerId(), snap.GetResellerId()} {
		id, err := uuid.Parse(s)
		if err != nil {
			return domain.TransactionUpdate{}, false, Permanent(fmt.Errorf("transaction ids: %w", err))
		}
		ids[i] = id
	}
	completedAt := snap.GetCompletedAt()
	if completedAt == nil {
		completedAt = md.GetOccurredAt()
	}
	if completedAt == nil {
		return domain.TransactionUpdate{}, false, Permanent(errors.New("completed transaction without a completion time"))
	}
	return domain.TransactionUpdate{
		EventID:    eventID,
		EventType:  md.GetEventType(),
		Topic:      r.Topic,
		OccurredAt: md.GetOccurredAt().AsTime(),
		Version:    snap.GetVersion(),
		Transaction: domain.CompletedTransaction{
			ID:          ids[0],
			FarmerID:    ids[1],
			ResellerID:  ids[2],
			CompletedAt: completedAt.AsTime(),
		},
	}, true, nil
}
