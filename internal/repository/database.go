// Package repository persists bot state in PostgreSQL.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/idan/secretmediabot/migrations"
	"github.com/pressly/goose/v3"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	ErrNotFound = errors.New("repository record not found")
	ErrConflict = errors.New("repository conflict")
)

type DatabaseOptions struct {
	URL             string
	MaxOpenConns    int
	MinIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

type Database struct {
	db    *gorm.DB
	sqlDB *sql.DB
}

func Open(ctx context.Context, options DatabaseOptions) (*Database, error) {
	if options.URL == "" {
		return nil, errors.New("database URL is required")
	}

	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN:                  options.URL,
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		// SQL parameter logging could disclose ciphertext, Telegram identifiers,
		// and opaque hashes. Application-level structured logs are used instead.
		Logger:                                   logger.Discard,
		DisableForeignKeyConstraintWhenMigrating: true,
		SkipDefaultTransaction:                   true,
		TranslateError:                           true,
	})
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("access PostgreSQL pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(options.MaxOpenConns)
	sqlDB.SetMaxIdleConns(options.MinIdleConns)
	sqlDB.SetConnMaxLifetime(options.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(options.ConnMaxIdleTime)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	return &Database{db: db, sqlDB: sqlDB}, nil
}

// Migrate runs the embedded, reviewable Goose migrations. GORM AutoMigrate is
// intentionally not used because partial indexes, constraints, and leases are
// part of the application's correctness model.
func (d *Database) Migrate(ctx context.Context) error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("configure migrations: %w", err)
	}
	if err := goose.UpContext(ctx, d.sqlDB, "."); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

func (d *Database) Ping(ctx context.Context) error {
	return d.sqlDB.PingContext(ctx)
}

func (d *Database) Close() error {
	return d.sqlDB.Close()
}

func (d *Database) GORM() *gorm.DB {
	return d.db
}
