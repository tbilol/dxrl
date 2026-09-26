package uz.agrogram.orderescrow.domain;

import java.time.Instant;
import java.util.Objects;
import java.util.UUID;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.events.v1.ReceiptVerificationStatus;

/**
 * The weighbridge receipt (tarozi cheki) the reseller submitted (table
 * weighbridge_receipts, one per transaction): a ticket number, a photo, or both.
 *
 * <p>STUB: TODO(weighbridge) there is no weighbridge integration or OCR
 * comparison, so {@link #stubVerified} accepts every receipt as VERIFIED the
 * moment it is submitted and RECEIPT_REJECTED never happens. TODO(media): the
 * photo is not checked with media-pipeline (purpose
 * MEDIA_PURPOSE_WEIGHBRIDGE_RECEIPT, attached to this transaction).
 *
 * @param ticketNumber ticket number as printed, or ""
 * @param photoMediaId media-pipeline upload of the ticket photo, or null
 * @param netMassGrams net weight typed in, or 0 when not given
 * @param netMassUnit display unit of the net weight
 * @param verificationStatus outcome of the check
 * @param submittedBy the reseller
 * @param submittedAt when the receipt was submitted
 * @param verifiedAt when the check finished, or null while PENDING
 */
public record ReceiptRecord(
        String ticketNumber,
        UUID photoMediaId,
        long netMassGrams,
        MassUnit netMassUnit,
        ReceiptVerificationStatus verificationStatus,
        UUID submittedBy,
        Instant submittedAt,
        Instant verifiedAt) {

    public ReceiptRecord {
        Objects.requireNonNull(ticketNumber, "ticketNumber");
        Objects.requireNonNull(netMassUnit, "netMassUnit");
        Objects.requireNonNull(verificationStatus, "verificationStatus");
        Objects.requireNonNull(submittedBy, "submittedBy");
        Objects.requireNonNull(submittedAt, "submittedAt");
        if (ticketNumber.isEmpty() && photoMediaId == null) {
            throw new IllegalArgumentException("a receipt needs a ticket number or a photo");
        }
        if (netMassGrams < 0) {
            throw new IllegalArgumentException("net mass must not be negative");
        }
    }

    /** STUB: TODO(weighbridge) a receipt accepted without any check. */
    public static ReceiptRecord stubVerified(String ticketNumber, UUID photoMediaId, long netMassGrams,
            MassUnit netMassUnit, UUID submittedBy, Instant now) {
        return new ReceiptRecord(ticketNumber, photoMediaId, netMassGrams, netMassUnit,
                ReceiptVerificationStatus.RECEIPT_VERIFICATION_STATUS_VERIFIED, submittedBy, now, now);
    }
}
