package uz.agrogram.orderescrow.catalog;

import java.util.UUID;
import uz.agrogram.catalog.v1.Listing;
import uz.agrogram.orderescrow.auth.Principal;

/** Reads a listing from catalog to snapshot it into a transaction. */
@FunctionalInterface
public interface ListingLookup {

    /**
     * The listing, in any status (catalog lets x-agrogram-caller: order-escrow
     * read them all).
     *
     * @throws uz.agrogram.orderescrow.error.AppException NOT_FOUND when catalog
     *     has no such listing, DEPENDENCY_UNAVAILABLE when catalog cannot answer
     */
    Listing getListing(Principal principal, UUID listingId);
}
