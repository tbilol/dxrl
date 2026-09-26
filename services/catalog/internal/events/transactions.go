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

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// TransactionApplier applies one transaction-stream event to catalog's
// projection of the escrow transactions on listings (the service).
type TransactionApplier interface {
	ApplyTransactionEvent(ctx context.Context, u domain.TransactionUpdate) (domain.ApplyOutcome, error)
}

// TransactionHandler is the RecordHandler of order-escrow's transaction
// stream, agrogram.orderescrow.transaction.v1 (TransactionEvent,
// proto/events/v1/transaction.proto). Every state matters to catalog: the
// first event of a transaction it sees commits the transaction's quantity
// against the listing, and the parties it names may read the listing in any
// status.
//
// It reads the snapshot, never the oneof, so every event type takes the same
// path, including one added after this code was generated. The applier
// de-duplicates on the event id (inbox) and applies a snapshot only when its
// version is newer. An event that contradicts catalog's data
// (domain.ErrInconsistentEvent) is poison like an undecodable record: it is
// dead-lettered at once.
type TransactionHandler struct {
	applier   TransactionApplier
	validator protovalidate.Validator
	log       *slog.Logger
}

// NewTransactionHandler returns the transaction-stream handler.
func NewTransactionHandler(applier TransactionApplier, validator protovalidate.Validator, log *slog.Logger) *TransactionHandler {
	return &TransactionHandler{applier: applier, validator: validator, log: log}
}

// HandleRecord implements RecordHandler.
func (h *TransactionHandler) HandleRecord(ctx context.Context, r *kgo.Record) error {
	u, err := DecodeTransactionRecord(r, h.validator)
	if err != nil {
		return err
	}
	if key := string(r.Key); key != u.TransactionID.String() {
		h.log.WarnContext(ctx, "transaction event keyed by something else than its transaction id; applied anyway",
			"key", key, "transaction_id", u.TransactionID, "event_id", u.EventID)
	}
	outcome, err := h.applier.ApplyTransactionEvent(ctx, u)
	if err != nil {
		err = fmt.Errorf("apply transaction event %s (transaction %s): %w", u.EventID, u.TransactionID, err)
		if errors.Is(err, domain.ErrInconsistentEvent) {
			return Permanent(err)
		}
		return err
	}
	h.log.DebugContext(ctx, "transaction event consumed", "event_id", u.EventID, "event_type", u.EventType,
		"transaction_id", u.TransactionID, "listing_id", u.ListingID, "version", u.Version,
		"state", u.State.String(), "outcome", outcome.String(), "partition", r.Partition, "offset", r.Offset)
	return nil
}

// DecodeTransactionRecord decodes one record of the transaction stream into
// the update it carries, whatever the transaction's state. A record that is
// not an application/x-protobuf TransactionEvent, fails protovalidate, has a
// snapshot contradicting its metadata (aggregate type, id or version), or
// commits no quantity is poison: the error is Permanent. An unknown oneof case
// (a newer producer) is not an error.
func DecodeTransactionRecord(r *kgo.Record, v protovalidate.Validator) (domain.TransactionUpdate, error) {
	if ct := header(r, HeaderContentType); ct != "" && ct != ContentType {
		return domain.TransactionUpdate{}, Permanent(fmt.Errorf("unsupported content-type %q", ct))
	}
	var ev eventsv1.TransactionEvent
	if err := proto.Unmarshal(r.Value, &ev); err != nil {
		return domain.TransactionUpdate{}, Permanent(fmt.Errorf("decode TransactionEvent: %w", err))
	}
	if err := v.Validate(&ev); err != nil {
		return domain.TransactionUpdate{}, Permanent(fmt.Errorf("invalid TransactionEvent: %w", err))
	}
	md, snap := ev.GetMetadata(), ev.GetTransaction()
	switch {
	case md.GetAggregateType() != commonv1.ResourceType_RESOURCE_TYPE_TRANSACTION:
		return domain.TransactionUpdate{}, Permanent(fmt.Errorf("aggregate type %s, want RESOURCE_TYPE_TRANSACTION",
			md.GetAggregateType()))
	case snap.GetId() != md.GetAggregateId():
		return domain.TransactionUpdate{}, Permanent(errors.New("transaction.id differs from metadata.aggregate_id"))
	case snap.GetVersion() != md.GetAggregateVersion():
		return domain.TransactionUpdate{}, Permanent(fmt.Errorf("transaction.version %d differs from metadata.aggregate_version %d",
			snap.GetVersion(), md.GetAggregateVersion()))
	case snap.GetQuantity().GetGrams() <= 0: // common.v1.Mass allows 0; a transaction must commit something
		return domain.TransactionUpdate{}, Permanent(fmt.Errorf("transaction.quantity.grams must be positive, got %d",
			snap.GetQuantity().GetGrams()))
	}
	// Every id passed protovalidate's uuid rule.
	eventID, err := uuid.Parse(md.GetEventId())
	if err != nil {
		return domain.TransactionUpdate{}, Permanent(fmt.Errorf("event id: %w", err))
	}
	ids := make([]uuid.UUID, 4)
	for i, s := range []string{snap.GetId(), snap.GetListingId(), snap.GetFarmerId(), snap.GetResellerId()} {
		id, err := uuid.Parse(s)
		if err != nil {
			return domain.TransactionUpdate{}, Permanent(fmt.Errorf("transaction ids: %w", err))
		}
		ids[i] = id
	}
	return domain.TransactionUpdate{
		EventID:       eventID,
		EventType:     md.GetEventType(),
		Topic:         r.Topic,
		OccurredAt:    md.GetOccurredAt().AsTime(),
		Version:       snap.GetVersion(),
		TransactionID: ids[0],
		ListingID:     ids[1],
		ListingKind:   snap.GetListingKind(),
		FarmerID:      ids[2],
		ResellerID:    ids[3],
		QuantityGrams: snap.GetQuantity().GetGrams(),
		State:         snap.GetState(),
		CreatedAt:     snap.GetCreatedAt().AsTime(),
	}, nil
}
