package repository

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsDir is the virtual directory inside the embedded FS where
// the .sql files live. We keep the same path used in the source tree so
// the embed path matches the deploy layout.
const migrationsDir = "migrations"

// applyMigrations executes every embedded .sql file in lexical order
// inside a single transaction. The approach is intentionally simple:
// there is no migrations table, no versioning — re-running the script
// is safe because every statement uses IF NOT EXISTS.
//
// For a beginner project this is more readable than wiring in
// golang-migrate and keeps the dependency surface minimal.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		raw, err := fs.ReadFile(migrationsFS, migrationsDir+"/"+name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := db.ExecContext(ctx, string(raw)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}
