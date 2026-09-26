package uz.agrogram.orderescrow.config;

/**
 * A listen address in the Go services' form: {@code host:port}, {@code :port}
 * (all interfaces) or {@code [ipv6]:port}.
 *
 * @param host the host or IP, "" for all interfaces
 * @param port the TCP port (0 picks a free one)
 */
public record HostPort(String host, int port) {

    public HostPort {
        if (port < 0 || port > 65535) {
            throw new IllegalArgumentException("port out of range: " + port);
        }
    }

    /** @throws IllegalArgumentException for anything that is not host:port */
    public static HostPort parse(String s) {
        if (s == null || s.isBlank()) {
            throw new IllegalArgumentException("is empty");
        }
        int colon = s.lastIndexOf(':');
        if (colon < 0) {
            throw new IllegalArgumentException("must be host:port or :port, got \"" + s + "\"");
        }
        String host = s.substring(0, colon);
        if (host.startsWith("[") && host.endsWith("]")) {
            host = host.substring(1, host.length() - 1);
        } else if (host.contains(":")) {
            throw new IllegalArgumentException("an IPv6 host needs brackets: [" + host + "]:port");
        }
        int port;
        try {
            port = Integer.parseInt(s.substring(colon + 1));
        } catch (NumberFormatException e) {
            throw new IllegalArgumentException("has an invalid port in \"" + s + "\"");
        }
        if (port < 0 || port > 65535) {
            throw new IllegalArgumentException("port out of range in \"" + s + "\"");
        }
        return new HostPort(host, port);
    }

    /** Listening on every interface ("" or 0.0.0.0 / ::). */
    public boolean allInterfaces() {
        return host.isEmpty() || host.equals("0.0.0.0") || host.equals("::");
    }

    /** The address a local probe connects to. */
    public String probeHost() {
        if (allInterfaces()) {
            return "127.0.0.1";
        }
        return host.contains(":") ? "[" + host + "]" : host;
    }

    @Override
    public String toString() {
        return (host.contains(":") ? "[" + host + "]" : host) + ":" + port;
    }
}
