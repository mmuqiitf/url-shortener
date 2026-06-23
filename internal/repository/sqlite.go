// Package repository persists and retrieves Link entities.
//
// The public surface is a single Repository struct backed by
// modernc.org/sqlite (pure Go, no CGO). All public methods accept a
// context.Context as the first parameter so callers can cancel or
// apply deadlines — this is the standard Go database pattern.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers itself as "sqlite"

	"github.com/mmuqiitf/url-shortener/internal/model"
)

// Repository is a SQLite-backed implementation of link storage.
//
// The struct wraps *sql.DB and is safe for concurrent use by multiple
// goroutines (the database/sql package guarantees this).
type Repository struct {
	db *sql.DB
}

// Open opens (or creates) a SQLite database at the given path and
// applies all embedded migrations.
//
// For in-memory testing, pass a DSN like "file::memory:?cache=shared"
// or use a per-connection DSN with mode=memory.
func Open(ctx context.Context, dsn string) (*Repository, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("repository: open: %w", err)
	}
	// SQLite is a single-writer DB; setting MaxOpenConns=1 is the
	// standard advice to avoid "database is locked" errors under load.
	// Callers can override via DB_MAX_OPEN_CONNS.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("repository: ping: %w", err)
	}

	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("repository: migrate: %w", err)
	}
	return &Repository{db: db}, nil
}

// OpenWithDB wraps an externally-managed *sql.DB. Useful for tests that
// want to inspect the DB handle directly or share it across packages.
func OpenWithDB(ctx context.Context, db *sql.DB) (*Repository, error) {
	if err := applyMigrations(ctx, db); err != nil {
		return nil, fmt.Errorf("repository: migrate: %w", err)
	}
	return &Repository{db: db}, nil
}

// DB returns the underlying *sql.DB. Provided for the few callers that
// need it (e.g. health checks); do not use it for regular queries.
func (r *Repository) DB() *sql.DB { return r.db }

// Close releases the database connection pool.
func (r *Repository) Close() error { return r.db.Close() }

// Create inserts a new link. The caller is responsible for generating
// the code (via the codec package) and validating it.
//
// Returns model.ErrCodeExists if the code collides with an existing row.
func (r *Repository) Create(ctx context.Context, l model.Link) error {
	const q = `
		INSERT INTO links (code, long_url, created_at, expires_at, clicks, is_active)
		VALUES (?, ?, ?, ?, ?, ?)
	`
	createdAt := l.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	// Default IsActive to true — a freshly created link is active by
	// default. The zero value of bool is false, so callers don't have
	// to set it explicitly.
	isActive := true
	_, err := r.db.ExecContext(ctx, q,
		l.Code, l.LongURL, createdAt.UTC().Format(time.RFC3339Nano),
		nullTime(l.ExpiresAt), l.Clicks, boolToInt(isActive),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("repository: create: %w", model.ErrCodeExists)
		}
		return fmt.Errorf("repository: create: %w", err)
	}
	return nil
}

// GetByCode fetches a link by its short code.
// Returns model.ErrNotFound when no row matches.
func (r *Repository) GetByCode(ctx context.Context, code string) (model.Link, error) {
	const q = `
		SELECT code, long_url, created_at, expires_at, clicks, is_active
		FROM links
		WHERE code = ?
		LIMIT 1
	`
	var (
		lnk       model.Link
		createdAt string
		expiresAt sql.NullString
		isActive  int
	)
	err := r.db.QueryRowContext(ctx, q, code).Scan(
		&lnk.Code, &lnk.LongURL, &createdAt, &expiresAt, &lnk.Clicks, &isActive,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Link{}, fmt.Errorf("repository: get %q: %w", code, model.ErrNotFound)
	}
	if err != nil {
		return model.Link{}, fmt.Errorf("repository: get: %w", err)
	}
	if t, perr := parseTime(createdAt); perr == nil {
		lnk.CreatedAt = t
	}
	if expiresAt.Valid {
		if t, perr := parseTime(expiresAt.String); perr == nil {
			lnk.ExpiresAt = &t
		}
	}
	lnk.IsActive = isActive != 0
	return lnk, nil
}

// List returns links ordered by id desc, paginated by limit/offset.
func (r *Repository) List(ctx context.Context, limit, offset int) ([]model.Link, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	const q = `
		SELECT code, long_url, created_at, expires_at, clicks, is_active
		FROM links
		ORDER BY id DESC
		LIMIT ? OFFSET ?
	`
	rows, err := r.db.QueryContext(ctx, q, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repository: list: %w", err)
	}
	defer rows.Close()

	out := make([]model.Link, 0, limit)
	for rows.Next() {
		var (
			lnk       model.Link
			createdAt string
			expiresAt sql.NullString
			isActive  int
		)
		if err := rows.Scan(&lnk.Code, &lnk.LongURL, &createdAt, &expiresAt, &lnk.Clicks, &isActive); err != nil {
			return nil, fmt.Errorf("repository: list: scan: %w", err)
		}
		if t, perr := parseTime(createdAt); perr == nil {
			lnk.CreatedAt = t
		}
		if expiresAt.Valid {
			if t, perr := parseTime(expiresAt.String); perr == nil {
				lnk.ExpiresAt = &t
			}
		}
		lnk.IsActive = isActive != 0
		out = append(out, lnk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repository: list: rows: %w", err)
	}
	return out, nil
}

// DeactivateByCode soft-deletes a link (sets is_active = 0).
// Returns model.ErrNotFound if no row was updated.
func (r *Repository) DeactivateByCode(ctx context.Context, code string) error {
	const q = `UPDATE links SET is_active = 0 WHERE code = ? AND is_active = 1`
	res, err := r.db.ExecContext(ctx, q, code)
	if err != nil {
		return fmt.Errorf("repository: deactivate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("repository: deactivate: rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("repository: deactivate %q: %w", code, model.ErrNotFound)
	}
	return nil
}

// BatchIncrementClicks applies a +1 increment to every code in the slice
// in a single statement. Codes not found are silently skipped (the
// tracker is best-effort; we don't want a race against link deletion
// to bubble up as a 500 on the redirect path).
//
// We aggregate the input into code -> count first so a batch of N
// events for the same code becomes a single "clicks = clicks + N"
// statement, not N round-trips. All updates run inside one transaction
// so they are atomic (and use a single write lock acquisition).
func (r *Repository) BatchIncrementClicks(ctx context.Context, codes []string) error {
	if len(codes) == 0 {
		return nil
	}
	counts := make(map[string]int, len(codes))
	for _, c := range codes {
		counts[c]++
	}
	const stmt = `UPDATE links SET clicks = clicks + ? WHERE code = ?`

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("repository: batch increment: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for code, n := range counts {
		if _, err := tx.ExecContext(ctx, stmt, int64(n), code); err != nil {
			return fmt.Errorf("repository: batch increment: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("repository: batch increment: commit: %w", err)
	}
	return nil
}

// Ping is a thin wrapper used by readiness probes.
func (r *Repository) Ping(ctx context.Context) error {
	return r.db.PingContext(ctx)
}

// --- helpers ------------------------------------------------------------

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime accepts both RFC3339 (second precision) and RFC3339Nano
// (nanosecond precision) — anything that fits the layout starting at
// the first dash is acceptable.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	return time.Parse(time.RFC3339, s)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUniqueViolation matches SQLite's "UNIQUE constraint failed" error.
// modernc.org/sqlite surfaces this in err.Error(); a regex-free approach
// would require driver-specific error types which we want to avoid.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	const marker = "UNIQUE constraint failed"
	return strings.Contains(err.Error(), marker)
}
