package uz.agrogram.orderescrow.events;

import java.time.Instant;
import java.util.List;

/** The relay's view of the outbox table. */
public interface OutboxStore {

    /** Publishes a batch; throws when any record of it was not acknowledged. */
    @FunctionalInterface
    interface Publisher {
        void publish(List<OutboxMessage> batch) throws Exception;
    }

    /**
     * Locks up to {@code limit} unpublished messages in outbox order, skipping
     * rows another relay holds (FOR UPDATE SKIP LOCKED), and hands them to
     * {@code publisher} while the locks are held. When it succeeds the messages
     * are marked published; otherwise their attempt count and last error are
     * recorded and a {@link PublishException} is thrown.
     *
     * @return how many messages were handed to the publisher
     */
    int processOutbox(int limit, Publisher publisher);

    /** Deletes messages published before {@code before}; returns how many. */
    long purgePublished(Instant before);

    /** The publisher failed; the batch stays unpublished. */
    final class PublishException extends RuntimeException {
        private static final long serialVersionUID = 1L;

        private final int batchSize;

        public PublishException(int batchSize, Throwable cause) {
            super("publish outbox batch of " + batchSize + ": " + cause, cause);
            this.batchSize = batchSize;
        }

        public int batchSize() {
            return batchSize;
        }
    }
}
