package uz.agrogram.orderescrow.store.jdbc;

import uz.agrogram.common.v1.ListingKind;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.events.v1.ReceiptVerificationStatus;

/** Proto enums as stored text: the value name without the enum prefix. */
final class DbEnums {

    private DbEnums() {}

    static String kind(ListingKind k) {
        return strip(k.name(), "LISTING_KIND_");
    }

    static ListingKind kind(String s) {
        return ListingKind.valueOf("LISTING_KIND_" + s);
    }

    static String unit(MassUnit u) {
        return u == MassUnit.UNRECOGNIZED ? "UNSPECIFIED" : strip(u.name(), "MASS_UNIT_");
    }

    static MassUnit unit(String s) {
        return MassUnit.valueOf("MASS_UNIT_" + s);
    }

    static String verification(ReceiptVerificationStatus v) {
        return strip(v.name(), "RECEIPT_VERIFICATION_STATUS_");
    }

    static ReceiptVerificationStatus verification(String s) {
        return ReceiptVerificationStatus.valueOf("RECEIPT_VERIFICATION_STATUS_" + s);
    }

    private static String strip(String name, String prefix) {
        if (!name.startsWith(prefix)) {
            throw new IllegalArgumentException(name + " has no prefix " + prefix);
        }
        return name.substring(prefix.length());
    }
}
