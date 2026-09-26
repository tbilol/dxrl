package uz.agrogram.orderescrow.domain;

import uz.agrogram.common.v1.Role;

/** A side of an escrow transaction. */
public enum Party {
    /** Seller (dehqon): owns an offer, or answers a reseller's buy-request. Receives the payout. */
    FARMER(Role.ROLE_FARMER),
    /** Buyer (olib sotuvchi): owns a buy-request, or starts a transaction on an offer. Pays in. */
    RESELLER(Role.ROLE_RESELLER);

    private final Role role;

    Party(Role role) {
        this.role = role;
    }

    /** Transaction.my_role for this side. */
    public Role role() {
        return role;
    }
}
