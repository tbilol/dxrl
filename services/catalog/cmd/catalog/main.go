// Command catalog is the AGROGRAM catalog service: the farmers' offer board,
// the resellers' buy-request board, listing search and the product reference
// data.
//
//	catalog [serve]                  run the gRPC and HTTP servers, the outbox relay, the transaction-stream consumer and the view counter (default)
//	catalog migrate up|down|status   manage the database schema (and the product seed)
//	catalog healthcheck              GET /readyz; exit 0 when ready (container HEALTHCHECK)
//
// Configuration comes from CATALOG_* environment variables (README.md).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for goose

	"github.com/tbilol/dxrl/services/catalog/internal/config"
	"github.com/tbilol/dxrl/services/catalog/internal/health"
	"github.com/tbilol/dxrl/services/catalog/migrations"
)

const usage = `usage:
  catalog [serve]                  run the gRPC and HTTP servers, the relay, the consumer and the view counter (default)
  catalog migrate up|down|status   apply all, roll back one, or list migrations
  catalog healthcheck              exit 0 when GET /readyz answers 200
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd, rest := "serve", args
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = migrate(ctx, rest, stdout)
	case "healthcheck":
		err = healthcheck(ctx)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "catalog: unknown command %q\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "catalog:", err)
		return 1
	}
	return 0
}

func migrate(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("migrate needs exactly one of: up, down, status")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	provider, err := migrations.NewProvider(db)
	if err != nil {
		return err
	}
	defer provider.Close()

	switch args[0] {
	case "up":
		results, err := provider.Up(ctx)
		for _, r := range results {
			fmt.Fprintln(out, r)
		}
		if err == nil && len(results) == 0 {
			fmt.Fprintln(out, "no pending migrations")
		}
		return err
	case "down":
		r, err := provider.Down(ctx)
		if r != nil {
			fmt.Fprintln(out, r)
		}
		return err
	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range statuses {
			applied := "-"
			if !s.AppliedAt.IsZero() {
				applied = s.AppliedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(out, "%-8s %-25s %s\n", s.State, applied, s.Source.Path)
		}
		return nil
	default:
		return fmt.Errorf("unknown migrate command %q: want up, down or status", args[0])
	}
}

func healthcheck(ctx context.Context) error {
	addr, err := config.HTTPAddr()
	if err != nil {
		return err
	}
	url, err := health.ProbeURL(addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return health.Probe(ctx, url)
}
