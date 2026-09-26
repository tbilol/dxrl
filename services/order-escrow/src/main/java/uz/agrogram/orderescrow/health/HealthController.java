package uz.agrogram.orderescrow.health;

import org.springframework.http.HttpStatus;
import org.springframework.http.MediaType;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

/**
 * GET /healthz (liveness: the process is up) and GET /readyz (readiness:
 * Postgres reachable and not shutting down), on ORDER_ESCROW_HTTP_ADDR, with
 * the same plain-text answers as the Go services.
 */
@RestController
public class HealthController {

    private final ReadinessMonitor readiness;

    public HealthController(ReadinessMonitor readiness) {
        this.readiness = readiness;
    }

    @GetMapping(path = "/healthz", produces = MediaType.TEXT_PLAIN_VALUE)
    public String healthz() {
        return "ok\n";
    }

    @GetMapping(path = "/readyz", produces = MediaType.TEXT_PLAIN_VALUE)
    public ResponseEntity<String> readyz() {
        if (!readiness.ready()) {
            return ResponseEntity.status(HttpStatus.SERVICE_UNAVAILABLE).body("not ready\n");
        }
        return ResponseEntity.ok("ready\n");
    }
}
