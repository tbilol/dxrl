package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// TransactionRecorder records completed escrow transactions (the service).
type TransactionRecorder interface {
	RecordCompletedTransaction(ctx context.Context, t domain.CompletedTransaction) (bool, error)
}

// TransactionHandler is the RecordHandler for order-escrow's transaction
// stream, agrogram.orderescrow.transaction.v1 (TransactionEvent,
// proto/events/v1/transaction.proto). It records a transaction once its
// snapshot reaches TRANSACTION_STATE_COMPLETED, which PayoutReleased
// ("escrow paid the farmer; the transaction is complete. The two parties may
// now rate each other") introduces and later events such as BonusAwarded
// repeat. Recording is idempotent by transaction id: it unlocks RateUser for
// both parties and adds one to each party's deals_count exactly once.
//
// TODO(phase3): order-escrow is not built yet, so nothing subscribes this
// handler: cmd/profile-social/serve.go registers only the account stream.
// Once order-escrow publishes the stream, register it there under
// TopicTransaction (the consumer then subscribes the topic too) and add
// agrogram.orderescrow.transaction.v1 to the kfake topics of the consumer
// tests.
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
	t, completed, err := DecodeTransactionRecord(r, h.validator)
	if err != nil || !completed {
		return err
	}
	recorded, err := h.recorder.RecordCompletedTransaction(ctx, t)
	if err != nil {
		return fmt.Errorf("record completed transaction %s: %w", t.ID, err)
	}
	h.log.DebugContext(ctx, "completed transaction consumed", "transaction_id", t.ID, "new", recorded)
	return nil
}

// DecodeTransactionRecord decodes one record of the transaction stream and
// reports whether its snapshot is COMPLETED. Undecodable or invalid records
// are poison (Permanent).
func DecodeTransactionRecord(r *kgo.Record, v protovalidate.Validator) (domain.CompletedTransaction, bool, error) {
	if ct := header(r, HeaderContentType); ct != "" && ct != ContentType {
		return domain.CompletedTransaction{}, false, Permanent(fmt.Errorf("unsupported content-type %q", ct))
	}
	var ev eventsv1.TransactionEvent
	if err := proto.Unmarshal(r.Value, &ev); err != nil {
		return domain.CompletedTransaction{}, false, Permanent(fmt.Errorf("decode TransactionEvent: %w", err))
	}
	if err := v.Validate(&ev); err != nil {
		return domain.CompletedTransaction{}, false, Permanent(fmt.Errorf("invalid TransactionEvent: %w", err))
	}
	snap := ev.GetTransaction()
	if snap.GetState() != eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED {
		return domain.CompletedTransaction{}, false, nil
	}
	ids := make([]uuid.UUID, 3)
	for i, s := range []string{snap.GetId(), snap.GetFarmerId(), snap.GetResellerId()} {
		id, err := uuid.Parse(s)
		if err != nil {
			return domain.CompletedTransaction{}, false, Permanent(fmt.Errorf("transaction ids: %w", err))
		}
		ids[i] = id
	}
	completedAt := snap.GetCompletedAt()
	if completedAt == nil {
		completedAt = ev.GetMetadata().GetOccurredAt()
	}
	if completedAt == nil {
		return domain.CompletedTransaction{}, false, Permanent(errors.New("completed transaction without a completion time"))
	}
	return domain.CompletedTransaction{
		ID:          ids[0],
		FarmerID:    ids[1],
		ResellerID:  ids[2],
		CompletedAt: completedAt.AsTime(),
	}, true, nil
}
