package uz.agrogram.orderescrow.auth;

import static org.assertj.core.api.Assertions.assertThat;

import io.grpc.Metadata;
import java.util.UUID;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.common.v1.Role;

/** The x-agrogram-* decoding rules of the Go services (services/catalog/internal/auth). */
class PrincipalTest {

    private static Metadata md(String... kv) {
        Metadata md = new Metadata();
        for (int i = 0; i + 1 < kv.length; i += 2) {
            md.put(Metadata.Key.of(kv[i], Metadata.ASCII_STRING_MARSHALLER), kv[i + 1]);
        }
        return md;
    }

    private static Principal decode(String... kv) {
        Principal.Decoded d = Principal.decode(md(kv));
        assertThat(d.error()).isEmpty();
        return d.principal();
    }

    @Test
    void decodesTheEdgeHeaders() {
        UUID user = UUID.randomUUID();
        UUID session = UUID.randomUUID();
        Principal p = decode("x-agrogram-user-id", user.toString(), "x-agrogram-roles", " reseller , FARMER,admin",
                "x-agrogram-premium", "TRUE", "x-agrogram-session-id", session.toString(), "x-request-id", "req-1",
                "traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "tracestate", "a=b",
                "accept-language", "ru");
        assertThat(p.userId()).isEqualTo(user);
        assertThat(p.roles()).containsExactly(Role.ROLE_RESELLER, Role.ROLE_FARMER, Role.ROLE_ADMIN);
        assertThat(p.premium()).isTrue();
        assertThat(p.sessionId()).isEqualTo(session.toString());
        assertThat(p.requestId()).isEqualTo("req-1");
        assertThat(p.tracestate()).isEqualTo("a=b");
        assertThat(p.acceptLanguage()).isEqualTo("ru");
        assertThat(p.staff()).isTrue();
        assertThat(p.actor().getUserId()).isEqualTo(user.toString());
    }

    @Test
    void anonymousCallsGetAFreshRequestIdAndNoIdentity() {
        Principal p = decode();
        assertThat(p.authenticated()).isFalse();
        assertThat(p.roles()).isEmpty();
        assertThat(Uuids.parseCanonical(p.requestId())).isPresent();
        assertThat(p.actor().getService()).isEqualTo("order-escrow");
    }

    @ParameterizedTest
    @ValueSource(strings = {"not-a-uuid", "00000000-0000-0000-0000-000000000000", "123"})
    void aMalformedOrNilUserIdIsUnauthenticated(String raw) {
        Principal.Decoded d = Principal.decode(md("x-agrogram-user-id", raw, "x-request-id", "r-7"));
        assertThat(d.error()).isPresent();
        assertThat(d.error().get().reason()).isEqualTo(ErrorReason.ERROR_REASON_UNAUTHENTICATED);
        assertThat(d.principal().requestId()).isEqualTo("r-7");
    }

    @Test
    void userIdsAreAcceptedInTheFormsGoParses() {
        UUID u = UUID.randomUUID();
        String hex = u.toString().replace("-", "");
        assertThat(decode("x-agrogram-user-id", u.toString().toUpperCase()).userId()).isEqualTo(u);
        assertThat(decode("x-agrogram-user-id", "{" + u + "}").userId()).isEqualTo(u);
        assertThat(decode("x-agrogram-user-id", "urn:uuid:" + u).userId()).isEqualTo(u);
        assertThat(decode("x-agrogram-user-id", hex).userId()).isEqualTo(u);
    }

    @Test
    void unusableContextValuesAreDroppedOrReplaced() {
        Principal p = decode("x-request-id", "has space", "traceparent", "garbage", "tracestate", "a=b",
                "x-agrogram-caller", "Bad_Caller", "accept-language", "<script>", "x-agrogram-roles", "KING,,",
                "x-agrogram-session-id", "nope");
        assertThat(p.requestId()).isNotEqualTo("has space");
        assertThat(p.traceparent()).isEmpty();
        assertThat(p.tracestate()).isEmpty(); // no tracestate without a valid traceparent
        assertThat(p.caller()).isEmpty();
        assertThat(p.acceptLanguage()).isEmpty();
        assertThat(p.roles()).isEmpty();
        assertThat(p.sessionId()).isEmpty();
        assertThat(decode("x-request-id", "x".repeat(129)).requestId()).hasSize(36);
    }

    @Test
    void internalCallersAreRecognized() {
        Principal p = decode("x-agrogram-caller", "profile-social");
        assertThat(p.internal()).isTrue();
        assertThat(p.actor().getService()).isEqualTo("profile-social");
    }

    @Test
    void outgoingMetadataForwardsTheUserAndNamesOrderEscrow() {
        UUID user = UUID.randomUUID();
        Principal p = decode("x-agrogram-user-id", user.toString(), "x-agrogram-roles", "FARMER,RESELLER",
                "x-request-id", "req-9", "accept-language", "uz");
        Metadata out = p.outgoingMetadata();
        assertThat(get(out, "x-agrogram-caller")).isEqualTo("order-escrow");
        assertThat(get(out, "x-agrogram-user-id")).isEqualTo(user.toString());
        assertThat(get(out, "x-agrogram-roles")).isEqualTo("FARMER,RESELLER");
        assertThat(get(out, "x-agrogram-premium")).isEqualTo("false");
        assertThat(get(out, "x-request-id")).isEqualTo("req-9");
        assertThat(get(out, "accept-language")).isEqualTo("uz");
        assertThat(get(out, "traceparent")).isNull();
        assertThat(get(Principal.anonymous().outgoingMetadata(), "x-agrogram-user-id")).isNull();
    }

    @Test
    void uuidV7IsTimeOrderedAndWellFormed() {
        UUID a = Uuids.newV7(1_000L);
        UUID b = Uuids.newV7(2_000L);
        assertThat(a.version()).isEqualTo(7);
        assertThat(a.variant()).isEqualTo(2);
        assertThat(a.toString().compareTo(b.toString())).isNegative();
        assertThat(Uuids.parse("1-1-1-1-1")).isEmpty(); // UUID.fromString would accept it
        assertThat(Uuids.parseCanonical(a.toString().replace("-", ""))).isEmpty();
    }

    private static String get(Metadata md, String name) {
        return md.get(Metadata.Key.of(name, Metadata.ASCII_STRING_MARSHALLER));
    }
}
