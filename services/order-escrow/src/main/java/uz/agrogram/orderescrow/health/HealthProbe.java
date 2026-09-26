package uz.agrogram.orderescrow.health;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import uz.agrogram.orderescrow.config.HostPort;

/**
 * The container healthcheck ({@code order-escrow healthcheck}): GET /readyz on
 * ORDER_ESCROW_HTTP_ADDR and succeed only on 200. Runs without Spring, so it
 * is cheap enough for a HEALTHCHECK every few seconds.
 */
public final class HealthProbe {

    private HealthProbe() {}

    /** The local /readyz URL for a listen address (all interfaces probe 127.0.0.1). */
    public static URI readyUrl(HostPort http) {
        return URI.create("http://" + http.probeHost() + ":" + http.port() + "/readyz");
    }

    /** @return null when ready, else the reason */
    public static String probe(URI url, Duration timeout) {
        HttpClient client = HttpClient.newBuilder().connectTimeout(timeout).build();
        HttpRequest request = HttpRequest.newBuilder(url).timeout(timeout).GET().build();
        try {
            HttpResponse<String> response = client.send(request, HttpResponse.BodyHandlers.ofString());
            return response.statusCode() == 200 ? null : url + ": HTTP " + response.statusCode();
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return url + ": interrupted";
        } catch (Exception e) {
            return url + ": " + e;
        }
    }
}
