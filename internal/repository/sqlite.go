// Package repository persists and retrieves Link entities using GORM.
//
// The implementation uses GORM with the pure-Go SQLite driver (github.com/glebarez/sqlite)
// so that CGO is not required. All database types and constraints are explicitly defined.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/mmuqiitf/url-shortener/internal/model"
)

// LinkModel defines the explicit database schema for the links table.
type LinkModel struct {
	ID        uint       `gorm:"column:id;primaryKey;autoIncrement;type:integer"`
	Code      string     `gorm:"column:code;type:text;size:64;uniqueIndex:idx_links_code;not null"`
	LongURL   string     `gorm:"column:long_url;type:text;not null"`
	CreatedAt time.Time  `gorm:"column:created_at;type:datetime;not null"`
	ExpiresAt *time.Time `gorm:"column:expires_at;type:datetime;index:idx_links_expires_at"`
	Clicks    int64      `gorm:"column:clicks;type:integer;not null;default:0"`
	IsActive  bool       `gorm:"column:is_active;type:boolean;not null;default:true;index:idx_links_is_active"`
}

// TableName explicitly overrides the table name.
func (LinkModel) TableName() string {
	return "links"
}

// Repository is a GORM-backed implementation of link storage.
type Repository struct {
	db *gorm.DB
}

// Open initializes a SQLite database via GORM, configures production pragmas,
// and applies AutoMigrate for schema creation.
func Open(ctx context.Context, dsn string) (*Repository, error) {
	gormDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		TranslateError: true, // Translates SQLite constraint violations into gorm.ErrDuplicatedKey etc.
		Logger:         logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("repository: open: %w", err)
	}

	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("repository: get sql.DB: %w", err)
	}

	// SQLite is a single-writer DB; setting MaxOpenConns=1 avoids "database is locked" errors under load.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("repository: ping: %w", err)
	}

	// Production SQLite PRAGMAs
	const pragmas = `
		PRAGMA journal_mode = WAL;
		PRAGMA busy_timeout = 5000;
		PRAGMA foreign_keys = ON;
	`
	if err := gormDB.Exec(pragmas).Error; err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("repository: set pragmas: %w", err)
	}

	// AutoMigrate tables using explicit LinkModel schema
	if err := gormDB.AutoMigrate(&LinkModel{}); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("repository: auto migrate: %w", err)
	}

	return &Repository{db: gormDB}, nil
}

// OpenWithDB wraps an externally-managed *gorm.DB.
func OpenWithDB(ctx context.Context, db *gorm.DB) (*Repository, error) {
	if err := db.AutoMigrate(&LinkModel{}); err != nil {
		return nil, fmt.Errorf("repository: auto migrate: %w", err)
	}
	return &Repository{db: db}, nil
}

// DB returns the underlying *gorm.DB handle.
func (r *Repository) DB() *gorm.DB {
	return r.db
}

// Close releases the database connection pool.
func (r *Repository) Close() error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Create inserts a new link.
// Returns model.ErrCodeExists if the code collides with an existing row.
func (r *Repository) Create(ctx context.Context, l model.Link) error {
	createdAt := l.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	m := LinkModel{
		Code:      l.Code,
		LongURL:   l.LongURL,
		CreatedAt: createdAt.UTC(),
		ExpiresAt: l.ExpiresAt,
		Clicks:    l.Clicks,
		IsActive:  true,
	}

	result := r.db.WithContext(ctx).Create(&m)
	if errors.Is(result.Error, gorm.ErrDuplicatedKey) {
		return fmt.Errorf("repository: create: %w", model.ErrCodeExists)
	}
	if result.Error != nil {
		return fmt.Errorf("repository: create: %w", result.Error)
	}
	return nil
}

// GetByCode fetches a link by its short code.
// Returns model.ErrNotFound when no row matches.
func (r *Repository) GetByCode(ctx context.Context, code string) (model.Link, error) {
	var m LinkModel
	err := r.db.WithContext(ctx).Where("code = ?", code).Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.Link{}, fmt.Errorf("repository: get %q: %w", code, model.ErrNotFound)
	}
	if err != nil {
		return model.Link{}, fmt.Errorf("repository: get: %w", err)
	}

	return model.Link{
		Code:      m.Code,
		LongURL:   m.LongURL,
		CreatedAt: m.CreatedAt,
		ExpiresAt: m.ExpiresAt,
		Clicks:    m.Clicks,
		IsActive:  m.IsActive,
	}, nil
}

// List returns links ordered by id desc, paginated by limit/offset.
func (r *Repository) List(ctx context.Context, limit, offset int) ([]model.Link, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var records []LinkModel
	err := r.db.WithContext(ctx).
		Order("id DESC").
		Limit(limit).
		Offset(offset).
		Find(&records).Error
	if err != nil {
		return nil, fmt.Errorf("repository: list: %w", err)
	}

	out := make([]model.Link, len(records))
	for i, m := range records {
		out[i] = model.Link{
			Code:      m.Code,
			LongURL:   m.LongURL,
			CreatedAt: m.CreatedAt,
			ExpiresAt: m.ExpiresAt,
			Clicks:    m.Clicks,
			IsActive:  m.IsActive,
		}
	}
	return out, nil
}

// DeactivateByCode soft-deletes a link (sets is_active = false).
// Returns model.ErrNotFound if no active row was updated.
func (r *Repository) DeactivateByCode(ctx context.Context, code string) error {
	result := r.db.WithContext(ctx).
		Model(&LinkModel{}).
		Where("code = ? AND is_active = ?", code, true).
		Update("is_active", false)
	if result.Error != nil {
		return fmt.Errorf("repository: deactivate: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("repository: deactivate %q: %w", code, model.ErrNotFound)
	}
	return nil
}

// BatchIncrementClicks applies a +N increment to aggregated codes in a single transaction.
func (r *Repository) BatchIncrementClicks(ctx context.Context, codes []string) error {
	if len(codes) == 0 {
		return nil
	}
	counts := make(map[string]int, len(codes))
	for _, c := range codes {
		counts[c]++
	}

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for code, n := range counts {
			err := tx.Model(&LinkModel{}).
				Where("code = ?", code).
				Update("clicks", gorm.Expr("clicks + ?", int64(n))).Error
			if err != nil {
				return fmt.Errorf("repository: batch increment: %w", err)
			}
		}
		return nil
	})
}

// Ping is a thin wrapper used by readiness probes.
func (r *Repository) Ping(ctx context.Context) error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return fmt.Errorf("repository: ping: %w", err)
	}
	return sqlDB.PingContext(ctx)
}
