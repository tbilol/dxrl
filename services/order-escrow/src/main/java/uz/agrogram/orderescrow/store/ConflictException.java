package uz.agrogram.orderescrow.store;

/**
 * A write lost a race against a concurrent transaction: a unique key already
 * taken (idempotency record, one open transaction per caller and listing, one
 * payment or payout per transaction), a stale version, a serialization failure
 * or a deadlock. The database transaction is rolled back; the unit of work can
 * be retried from the start, and the retry sees the winner's committed result.
 */
public final class ConflictException extends RuntimeException {

    private static final long serialVersionUID = 1L;

    public ConflictException(String message) {
        super(message);
    }

    public ConflictException(String message, Throwable cause) {
        super(message, cause);
    }
}
