package uz.agrogram.orderescrow.auth;

import io.grpc.Context;
import io.grpc.Metadata;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.Optional;
import java.util.UUID;
import java.util.regex.Pattern;
import uz.agrogram.common.v1.Actor;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.common.v1.Role;
import uz.agrogram.orderescrow.error.AppException;

/**
 * The in-process form of the request metadata the edge forwards
 * (proto/common/v1/auth.proto): the signed-in user, the internal caller and the
 * request context. Authentication happens at the edge; order-escrow trusts these
 * headers because the edge strips client-supplied x-agrogram-* values.
 *
 * <p>Decoding follows the Go services (services/catalog/internal/auth) exactly:
 * a present but malformed user id is UNAUTHENTICATED, unknown role names are
 * ignored, a missing or unusable x-request-id is replaced by a fresh UUIDv7,
 * and malformed trace context is dropped.
 *
 * @param userId the signed-in user; {@code null} for an anonymous call
 * @param roles token-time roles, in header order, without duplicates
 * @param premium Premium plan active when the token was issued
 * @param sessionId sign-in session id (canonical UUID) or ""
 * @param caller x-agrogram-caller: set on internal calls only, else ""
 * @param requestId x-request-id, or a fresh UUIDv7
 * @param traceparent W3C traceparent when well formed, else ""
 * @param tracestate W3C tracestate when present with a valid traceparent, else ""
 * @param acceptLanguage the caller's UI language, informational, or ""
 */
public record Principal(
        UUID userId,
        List<Role> roles,
        boolean premium,
        String sessionId,
        String caller,
        String requestId,
        String traceparent,
        String tracestate,
        String acceptLanguage) {

    /** order-escrow's own name: the producer, the x-agrogram-caller and the actor of system steps. */
    public static final String SERVICE_NAME = "order-escrow";

    public static final String HEADER_USER_ID = "x-agrogram-user-id";
    public static final String HEADER_ROLES = "x-agrogram-roles";
    public static final String HEADER_PREMIUM = "x-agrogram-premium";
    public static final String HEADER_SESSION_ID = "x-agrogram-session-id";
    public static final String HEADER_CALLER = "x-agrogram-caller";
    public static final String HEADER_REQUEST_ID = "x-request-id";
    public static final String HEADER_TRACEPARENT = "traceparent";
    public static final String HEADER_TRACESTATE = "tracestate";
    public static final String HEADER_ACCEPT_LANGUAGE = "accept-language";

    /** The principal of the current gRPC call, set by the principal interceptor. */
    public static final Context.Key<Principal> CONTEXT_KEY = Context.key("agrogram-principal");

    private static final Pattern CALLER = Pattern.compile("^[a-z][a-z0-9-]{1,62}$");
    private static final Pattern TRACEPARENT = Pattern.compile("^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$");
    private static final Pattern REQUEST_ID = Pattern.compile("^[\\x21-\\x7e]{1,128}$");
    private static final Pattern LANGUAGE = Pattern.compile("^[A-Za-z0-9,;=.* -]{1,64}$");

    public Principal {
        roles = List.copyOf(roles);
    }

    /** The result of decoding metadata: always a principal (for the request id), maybe an error. */
    public record Decoded(Principal principal, Optional<AppException> error) {}

    /** Decodes request metadata. */
    public static Decoded decode(Metadata md) {
        String requestId = first(md, HEADER_REQUEST_ID);
        if (!REQUEST_ID.matcher(requestId).matches()) {
            requestId = Uuids.newV7().toString();
        }
        String traceparent = first(md, HEADER_TRACEPARENT);
        String tracestate = first(md, HEADER_TRACESTATE);
        if (!TRACEPARENT.matcher(traceparent).matches()) {
            traceparent = "";
            tracestate = "";
        }
        if (tracestate.length() > 512) {
            tracestate = "";
        }
        String language = first(md, HEADER_ACCEPT_LANGUAGE);
        if (!LANGUAGE.matcher(language).matches()) {
            language = "";
        }
        String caller = first(md, HEADER_CALLER);
        if (!CALLER.matcher(caller).matches()) {
            caller = "";
        }
        String sessionId = Uuids.parse(first(md, HEADER_SESSION_ID)).map(UUID::toString).orElse("");
        boolean premium = first(md, HEADER_PREMIUM).equalsIgnoreCase("true");

        List<Role> roles = new ArrayList<>();
        for (String raw : first(md, HEADER_ROLES).split(",", -1)) {
            Role r = roleByName("ROLE_" + raw.strip().toUpperCase(Locale.ROOT));
            if (r != null && !roles.contains(r)) {
                roles.add(r);
            }
        }

        UUID userId = null;
        Optional<AppException> error = Optional.empty();
        String rawUser = first(md, HEADER_USER_ID);
        if (!rawUser.isEmpty()) {
            Optional<UUID> id = Uuids.parse(rawUser);
            if (id.isEmpty() || (id.get().getMostSignificantBits() == 0 && id.get().getLeastSignificantBits() == 0)) {
                error = Optional.of(AppException.of(ErrorReason.ERROR_REASON_UNAUTHENTICATED,
                        HEADER_USER_ID + " is not a valid UUID"));
            } else {
                userId = id.get();
            }
        }
        Principal p = new Principal(userId, roles, premium, sessionId, caller, requestId, traceparent, tracestate,
                language);
        return new Decoded(p, error);
    }

    /** An anonymous principal with a fresh request id (background work, tests). */
    public static Principal anonymous() {
        return new Principal(null, List.of(), false, "", "", Uuids.newV7().toString(), "", "", "");
    }

    /** The principal of the current gRPC call, or an anonymous one outside a call. */
    public static Principal current() {
        Principal p = CONTEXT_KEY.get();
        return p != null ? p : anonymous();
    }

    public boolean authenticated() {
        return userId != null;
    }

    public boolean internal() {
        return !caller.isEmpty();
    }

    public boolean hasRole(Role r) {
        return roles.contains(r);
    }

    /** Platform staff (ROLE_ADMIN). */
    public boolean staff() {
        return authenticated() && hasRole(Role.ROLE_ADMIN);
    }

    /** The signed-in user, or UNAUTHENTICATED. */
    public UUID requireUser() {
        if (userId == null) {
            throw AppException.of(ErrorReason.ERROR_REASON_UNAUTHENTICATED,
                    "a signed-in user is required (" + HEADER_USER_ID + ")");
        }
        return userId;
    }

    /** Who performs the request: the user; without one, the calling service; otherwise order-escrow. */
    public Actor actor() {
        if (authenticated()) {
            return userActor(userId);
        }
        if (internal()) {
            return Actor.newBuilder().setService(caller).build();
        }
        return serviceActor();
    }

    public static Actor userActor(UUID id) {
        return Actor.newBuilder().setUserId(id.toString()).build();
    }

    /** order-escrow acting on its own (payout release, bonus). */
    public static Actor serviceActor() {
        return Actor.newBuilder().setService(SERVICE_NAME).build();
    }

    /**
     * Metadata for a call made on the user's behalf (auth.proto): x-agrogram-caller,
     * the request context, and the user headers forwarded unchanged.
     */
    public Metadata outgoingMetadata() {
        Metadata md = new Metadata();
        put(md, HEADER_CALLER, SERVICE_NAME);
        put(md, HEADER_REQUEST_ID, requestId);
        if (!traceparent.isEmpty()) {
            put(md, HEADER_TRACEPARENT, traceparent);
            if (!tracestate.isEmpty()) {
                put(md, HEADER_TRACESTATE, tracestate);
            }
        }
        if (!acceptLanguage.isEmpty()) {
            put(md, HEADER_ACCEPT_LANGUAGE, acceptLanguage);
        }
        if (!authenticated()) {
            return md;
        }
        put(md, HEADER_USER_ID, userId.toString());
        if (!roles.isEmpty()) {
            List<String> names = new ArrayList<>(roles.size());
            for (Role r : roles) {
                names.add(r.name().substring("ROLE_".length()));
            }
            put(md, HEADER_ROLES, String.join(",", names));
        }
        put(md, HEADER_PREMIUM, Boolean.toString(premium));
        if (!sessionId.isEmpty()) {
            put(md, HEADER_SESSION_ID, sessionId);
        }
        return md;
    }

    private static Role roleByName(String name) {
        for (Role r : Role.values()) {
            if (r != Role.UNRECOGNIZED && r != Role.ROLE_UNSPECIFIED && r.name().equals(name)) {
                return r;
            }
        }
        return null;
    }

    private static Metadata.Key<String> key(String name) {
        return Metadata.Key.of(name, Metadata.ASCII_STRING_MARSHALLER);
    }

    private static void put(Metadata md, String name, String value) {
        md.put(key(name), value);
    }

    /** The first non-blank value of a header, trimmed, or "". */
    private static String first(Metadata md, String name) {
        Iterable<String> values = md.getAll(key(name));
        if (values == null) {
            return "";
        }
        for (String v : values) {
            String t = v == null ? "" : v.strip();
            if (!t.isEmpty()) {
                return t;
            }
        }
        return "";
    }
}
