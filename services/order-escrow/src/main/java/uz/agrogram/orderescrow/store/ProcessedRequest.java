package uz.agrogram.orderescrow.store;

import java.time.Instant;
import java.util.Objects;
import java.util.UUID;

/**
 * The idempotency record of a command (table processed_requests): the caller's
 * request_id, the operation and transaction it was used for, a hash of the
 * request, and the response that was returned. A retry with the same
 * request_id returns {@code response} without running the command again.
 *
 * @param callerId the signed-in user who sent the request
 * @param requestId the request's idempotency key
 * @param operation RPC name, e.g. "PayIn"
 * @param transactionId the transaction the request created or acted on
 * @param requestHash SHA-256 of the request without its request_id
 * @param response the serialized response message
 * @param createdAt when the request was processed
 */
public record ProcessedRequest(
        UUID callerId,
        UUID requestId,
        String operation,
        UUID transactionId,
        byte[] requestHash,
        byte[] response,
        Instant createdAt) {

    public ProcessedRequest {
        Objects.requireNonNull(callerId, "callerId");
        Objects.requireNonNull(requestId, "requestId");
        Objects.requireNonNull(operation, "operation");
        Objects.requireNonNull(transactionId, "transactionId");
        Objects.requireNonNull(requestHash, "requestHash");
        Objects.requireNonNull(response, "response");
        Objects.requireNonNull(createdAt, "createdAt");
        requestHash = requestHash.clone();
        response = response.clone();
    }

    @Override
    public byte[] requestHash() {
        return requestHash.clone();
    }

    @Override
    public byte[] response() {
        return response.clone();
    }

    @Override
    public boolean equals(Object o) {
        return o instanceof ProcessedRequest p && callerId.equals(p.callerId) && requestId.equals(p.requestId)
                && operation.equals(p.operation) && transactionId.equals(p.transactionId)
                && java.util.Arrays.equals(requestHash, p.requestHash) && java.util.Arrays.equals(response, p.response)
                && createdAt.equals(p.createdAt);
    }

    @Override
    public int hashCode() {
        return Objects.hash(callerId, requestId);
    }

    @Override
    public String toString() {
        return "ProcessedRequest{caller=" + callerId + ", request=" + requestId + ", operation=" + operation
                + ", transaction=" + transactionId + "}";
    }
}
