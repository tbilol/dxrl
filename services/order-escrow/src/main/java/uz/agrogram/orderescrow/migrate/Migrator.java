package uz.agrogram.orderescrow.migrate;

import java.io.IOException;
import java.io.InputStream;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.SQLException;
import java.sql.Statement;
import org.flywaydb.core.Flyway;
import org.flywaydb.core.api.MigrationInfo;
import org.flywaydb.core.api.configuration.FluentConfiguration;
import org.flywaydb.core.api.output.MigrateResult;
import uz.agrogram.orderescrow.config.DatabaseUrl;

/**
 * Schema migrations with Flyway: {@code db/migration/V<n>__<name>.sql} on the
 * classpath, history in {@code flyway_schema_history}.
 *
 * <p>Flyway OSS has no undo, so "down" (parity with the Go services' goose
 * migrate-down) is explicit: every migration has a reverting script
 * {@code db/down/V<n>.down.sql}; {@link #down} runs the one of the latest
 * applied migration and deletes its history row, in one transaction.
 */
public final class Migrator {

    private final Flyway flyway;
    private final PrintStream out;

    public Migrator(Flyway flyway, PrintStream out) {
        this.flyway = flyway;
        this.out = out;
    }

    /**
     * Flyway for the service's migrations.
     *
     * @param schema the schema to migrate, or null for the connection's default
     *     (public); integration tests pass a temporary schema that already exists
     */
    public static Flyway flyway(DatabaseUrl db, String schema) {
        FluentConfiguration c = Flyway.configure()
                .dataSource(db.jdbcUrl(), db.username(), db.password())
                .locations("classpath:db/migration")
                .validateMigrationNaming(true)
                .cleanDisabled(true);
        if (schema != null) {
            c = c.schemas(schema).defaultSchema(schema).createSchemas(false);
        }
        return c.load();
    }

    /** Applies all pending migrations. */
    public void up() {
        MigrateResult result = flyway.migrate();
        out.printf("migrate: %d migration(s) applied; schema version %s%n", result.migrationsExecuted,
                result.targetSchemaVersion == null ? "unchanged" : result.targetSchemaVersion);
    }

    /** Prints every migration and its state. */
    public void status() {
        out.printf("%-8s %-30s %-10s %s%n", "VERSION", "DESCRIPTION", "STATE", "INSTALLED ON");
        for (MigrationInfo i : flyway.info().all()) {
            out.printf("%-8s %-30s %-10s %s%n", i.getVersion() == null ? "" : i.getVersion().getVersion(),
                    i.getDescription(), i.getState().getDisplayName(),
                    i.getInstalledOn() == null ? "" : i.getInstalledOn().toInstant());
        }
    }

    /** Rolls back the latest applied migration with its db/down script. */
    public void down() {
        MigrationInfo current = flyway.info().current();
        if (current == null || current.getVersion() == null) {
            out.println("migrate: nothing to roll back");
            return;
        }
        String version = current.getVersion().getVersion();
        String resource = "db/down/V" + version + ".down.sql";
        String script;
        try (InputStream in = Migrator.class.getClassLoader().getResourceAsStream(resource)) {
            if (in == null) {
                throw new IllegalStateException("no down script " + resource + " for migration " + version);
            }
            script = new String(in.readAllBytes(), StandardCharsets.UTF_8);
        } catch (IOException e) {
            throw new IllegalStateException("cannot read " + resource, e);
        }
        String table = flyway.getConfiguration().getTable();
        try (Connection c = flyway.getConfiguration().getDataSource().getConnection()) {
            c.setAutoCommit(false);
            try {
                try (Statement s = c.createStatement()) {
                    String[] schemas = flyway.getConfiguration().getSchemas();
                    if (schemas.length > 0) {
                        s.execute("SET LOCAL search_path TO " + quoteIdent(schemas[0]));
                    }
                    s.execute(script);
                }
                try (PreparedStatement d = c.prepareStatement(
                        "DELETE FROM " + quoteIdent(table) + " WHERE version = ?")) {
                    d.setString(1, version);
                    if (d.executeUpdate() != 1) {
                        throw new IllegalStateException("migration " + version + " is not in " + table);
                    }
                }
                c.commit();
            } catch (SQLException | RuntimeException e) {
                c.rollback();
                throw e;
            }
        } catch (SQLException e) {
            throw new IllegalStateException("rolling back migration " + version + " failed: " + e.getMessage(), e);
        }
        out.printf("migrate: rolled back version %s (%s)%n", version, current.getDescription());
    }

    private static String quoteIdent(String s) {
        return "\"" + s.replace("\"", "\"\"") + "\"";
    }
}
