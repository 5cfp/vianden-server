// Package db connects to PostgreSQL and applies database migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for database/sql (used by goose)
	"github.com/pressly/goose/v3"
)

// migrationFiles holds every .sql file in migrations/, compiled into the binary.
// Hosts never need to copy migration files next to the server.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// Connect opens a connection pool and checks that the database answers.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid database URL: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("cannot reach the database (is `docker compose up -d` running?): %w", err)
	}
	return pool, nil
}

// Migrate applies every migration that has not been applied yet.
// goose records applied migrations in the goose_db_version table.
func Migrate(ctx context.Context, databaseURL string, logger *slog.Logger) error {
	// goose works with Go's standard database/sql, so it gets its own short-lived connection.
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, files)
	if err != nil {
		return fmt.Errorf("loading migrations: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	for _, r := range results {
		logger.Info("applied migration", "file", r.Source.Path, "duration", r.Duration)
	}

	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return err
	}
	logger.Info("database is up to date", "schema_version", version)
	return nil
}
