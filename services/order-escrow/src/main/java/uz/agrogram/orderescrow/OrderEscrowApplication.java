package uz.agrogram.orderescrow;

import java.time.Duration;
import java.util.Arrays;
import org.slf4j.LoggerFactory;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.context.ApplicationContextInitializer;
import org.springframework.context.ConfigurableApplicationContext;
import uz.agrogram.orderescrow.config.DatabaseUrl;
import uz.agrogram.orderescrow.config.HostPort;
import uz.agrogram.orderescrow.config.ServiceConfig;
import uz.agrogram.orderescrow.health.HealthProbe;
import uz.agrogram.orderescrow.migrate.Migrator;

/**
 * order-escrow: the escrow transaction (tranzaksiya) service.
 *
 * <pre>
 *   order-escrow [serve]               gRPC (OrderEscrowService, health, reflection), HTTP /healthz /readyz,
 *                                      outbox relay to Kafka
 *   order-escrow migrate up|status|down
 *                                      schema migrations (needs only ORDER_ESCROW_DATABASE_URL)
 *   order-escrow healthcheck           GET /readyz on ORDER_ESCROW_HTTP_ADDR; exit 0 when ready
 * </pre>
 *
 * The same commands as the Go services' binaries. Configuration comes from
 * ORDER_ESCROW_* environment variables ({@link ServiceConfig}).
 */
@SpringBootApplication
public class OrderEscrowApplication {

    private static final String USAGE = """
            usage: order-escrow [command]

              serve                  run the service (default)
              migrate up             apply all pending migrations
              migrate status         show migration status
              migrate down           roll back the latest migration
              healthcheck            exit 0 when GET /readyz answers 200
            """;

    public static void main(String[] args) {
        String command = args.length == 0 ? "serve" : args[0];
        String[] rest = args.length == 0 ? args : Arrays.copyOfRange(args, 1, args.length);
        switch (command) {
            case "serve":
                serve(rest);
                return;
            case "migrate":
                System.exit(migrate(rest));
                return;
            case "healthcheck":
                System.exit(healthcheck());
                return;
            case "help":
            case "-h":
            case "--help":
                System.out.print(USAGE);
                return;
            default:
                System.err.print("order-escrow: unknown command \"" + command + "\"\n" + USAGE);
                System.exit(2);
        }
    }

    private static void serve(String[] args) {
        ServiceConfig config;
        try {
            config = ServiceConfig.load(System.getenv());
        } catch (ServiceConfig.ConfigException e) {
            System.err.println("order-escrow: " + e.getMessage());
            System.exit(1);
            return;
        }
        SpringApplication app = new SpringApplication(OrderEscrowApplication.class);
        app.setDefaultProperties(config.springProperties());
        ApplicationContextInitializer<ConfigurableApplicationContext> registerConfig =
                ctx -> ctx.getBeanFactory().registerSingleton("serviceConfig", config);
        app.addInitializers(registerConfig);
        app.run(args);
    }

    private static int migrate(String[] args) {
        quietLibraries();
        if (args.length != 1) {
            System.err.print("order-escrow: migrate needs one of up, status, down\n" + USAGE);
            return 2;
        }
        DatabaseUrl db;
        try {
            db = ServiceConfig.loadDatabase(System.getenv(), "DATABASE_URL");
        } catch (ServiceConfig.ConfigException e) {
            System.err.println("order-escrow: " + e.getMessage());
            return 1;
        }
        Migrator m = new Migrator(Migrator.flyway(db, null), System.out);
        try {
            switch (args[0]) {
                case "up":
                    m.up();
                    return 0;
                case "status":
                    m.status();
                    return 0;
                case "down":
                    m.down();
                    return 0;
                default:
                    System.err.print("order-escrow: unknown migrate command \"" + args[0] + "\"\n" + USAGE);
                    return 2;
            }
        } catch (RuntimeException e) {
            System.err.println("order-escrow: migrate " + args[0] + ": " + e.getMessage());
            return 1;
        }
    }

    private static int healthcheck() {
        HostPort http;
        try {
            http = ServiceConfig.loadHttpAddr(System.getenv());
        } catch (ServiceConfig.ConfigException e) {
            System.err.println("order-escrow: " + e.getMessage());
            return 1;
        }
        String failure = HealthProbe.probe(HealthProbe.readyUrl(http), Duration.ofSeconds(3));
        if (failure != null) {
            System.err.println("order-escrow: not ready: " + failure);
            return 1;
        }
        return 0;
    }

    /** Outside Spring, Logback would log everything at DEBUG; keep the CLI commands readable. */
    private static void quietLibraries() {
        if (LoggerFactory.getLogger(org.slf4j.Logger.ROOT_LOGGER_NAME)
                instanceof ch.qos.logback.classic.Logger root) {
            root.setLevel(ch.qos.logback.classic.Level.INFO);
        }
    }
}
