package uz.agrogram.orderescrow.config;

import java.net.URLDecoder;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.Objects;

/**
 * A libpq-style database URL, the form every AGROGRAM service is configured
 * with ({@code postgres://user:password@host:port/db?sslmode=disable}),
 * converted for the PostgreSQL JDBC driver. The user and password are kept out
 * of the JDBC URL so that it can be logged.
 *
 * @param jdbcUrl {@code jdbc:postgresql://host:port/db?...}
 * @param username the user, percent-decoded
 * @param password the password, percent-decoded ("" when absent)
 */
public record DatabaseUrl(String jdbcUrl, String username, String password) {

    public DatabaseUrl {
        Objects.requireNonNull(jdbcUrl);
        Objects.requireNonNull(username);
        Objects.requireNonNull(password);
    }

    /**
     * Parses {@code postgres://} or {@code postgresql://} URLs. Query parameters
     * pass through, except the libpq names the JDBC driver spells differently
     * (application_name, connect_timeout).
     *
     * @throws IllegalArgumentException with a message that never contains the password
     */
    public static DatabaseUrl parse(String url) {
        if (url == null || url.isBlank()) {
            throw new IllegalArgumentException("is empty");
        }
        String rest;
        if (url.startsWith("postgres://")) {
            rest = url.substring("postgres://".length());
        } else if (url.startsWith("postgresql://")) {
            rest = url.substring("postgresql://".length());
        } else {
            throw new IllegalArgumentException("must start with postgres:// or postgresql://");
        }
        String query = "";
        int q = rest.indexOf('?');
        if (q >= 0) {
            query = rest.substring(q + 1);
            rest = rest.substring(0, q);
        }
        int slash = rest.indexOf('/');
        String authority = slash >= 0 ? rest.substring(0, slash) : rest;
        String database = slash >= 0 ? decode(rest.substring(slash + 1)) : "";
        String user = "";
        String password = "";
        int at = authority.lastIndexOf('@');
        if (at >= 0) {
            String userInfo = authority.substring(0, at);
            authority = authority.substring(at + 1);
            int colon = userInfo.indexOf(':');
            user = decode(colon >= 0 ? userInfo.substring(0, colon) : userInfo);
            password = colon >= 0 ? decode(userInfo.substring(colon + 1)) : "";
        }
        if (authority.isEmpty()) {
            throw new IllegalArgumentException("has no host");
        }
        if (database.isEmpty() || database.contains("/")) {
            throw new IllegalArgumentException("must name one database (postgres://user:password@host:port/db)");
        }
        if (user.isEmpty()) {
            throw new IllegalArgumentException("has no user");
        }
        Map<String, String> params = new LinkedHashMap<>();
        if (!query.isEmpty()) {
            for (String pair : query.split("&")) {
                if (pair.isEmpty()) {
                    continue;
                }
                int eq = pair.indexOf('=');
                String key = decode(eq >= 0 ? pair.substring(0, eq) : pair);
                String value = eq >= 0 ? decode(pair.substring(eq + 1)) : "";
                switch (key) {
                    case "application_name":
                        params.put("ApplicationName", value);
                        break;
                    case "connect_timeout":
                        params.put("connectTimeout", value);
                        break;
                    default:
                        params.put(key, value);
                }
            }
        }
        StringBuilder jdbc = new StringBuilder("jdbc:postgresql://").append(authority).append('/')
                .append(encode(database));
        char sep = '?';
        for (Map.Entry<String, String> e : params.entrySet()) {
            jdbc.append(sep).append(encode(e.getKey())).append('=').append(encode(e.getValue()));
            sep = '&';
        }
        return new DatabaseUrl(jdbc.toString(), user, password);
    }

    /** The same database with {@code currentSchema} set (integration tests use a temporary schema). */
    public DatabaseUrl withSchema(String schema) {
        String sep = jdbcUrl.contains("?") ? "&" : "?";
        return new DatabaseUrl(jdbcUrl + sep + "currentSchema=" + encode(schema), username, password);
    }

    private static String decode(String s) {
        try {
            return URLDecoder.decode(s.replace("+", "%2B"), StandardCharsets.UTF_8);
        } catch (IllegalArgumentException e) {
            throw new IllegalArgumentException("has an invalid percent-encoding");
        }
    }

    private static String encode(String s) {
        return URLEncoder.encode(s, StandardCharsets.UTF_8).replace("+", "%20");
    }

    @Override
    public String toString() {
        return "DatabaseUrl{" + jdbcUrl + ", user=" + username + ", password=" + (password.isEmpty() ? "" : "***")
                + "}";
    }
}
