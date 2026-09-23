// Package migrations embeds identity's SQL migrations (goose format) and runs
// them. Table names are unqualified, so the target schema is whatever the
// connection's search_path selects (the integration tests use a temporary
// schema that way).
package migrations

import (
	"database/sql"
	"embed"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed *.sql
var FS embed.FS

// NewProvider returns a goose provider for the embedded migrations. A
// Postgres advisory lock keeps concurrent `migrate` runs from colliding.
func NewProvider(db *sql.DB) (*goose.Provider, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, db, FS, goose.WithSessionLocker(locker))
}
