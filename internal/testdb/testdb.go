// Package testdb gives tests a clean, migrated PostgreSQL database.
//
// It uses VIANDEN_TEST_DATABASE_URL (from the environment or the repo's .env file).
// If that is not set, tests that need a database are skipped, not failed.
package testdb

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/5cfp/vianden-server/internal/db"
)

// testLockID is an arbitrary number naming the advisory lock.
const testLockID = 727_001

// New connects to the test database, applies all migrations, and empties every table.
// The pool is closed automatically when the test ends.
//
// Tests in different packages run in parallel processes but share this one database, so
// New first takes a PostgreSQL "advisory lock": a named, database-wide lock that only one
// connection can hold. Other tests wait until it is released when the test ends.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("VIANDEN_TEST_DATABASE_URL")
	if url == "" {
		if env, err := godotenv.Read(filepath.Join(repoRoot(t), ".env")); err == nil {
			url = env["VIANDEN_TEST_DATABASE_URL"]
		}
	}
	if url == "" {
		t.Skip("VIANDEN_TEST_DATABASE_URL is not set; skipping database test")
	}

	ctx := context.Background()

	lockConn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", testLockID); err != nil {
		t.Fatalf("locking test database: %v", err)
	}
	// Cleanups run last-registered-first, so this unlock runs after the pool is closed.
	t.Cleanup(func() {
		lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", testLockID)
		lockConn.Close(ctx)
	})

	if err := db.Migrate(ctx, url, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	// Start every test from empty tables. RESTART IDENTITY resets the id counters to 1.
	if _, err := pool.Exec(ctx, "TRUNCATE users, sessions, invites, channels, messages RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("emptying test database: %v", err)
	}
	return pool
}

// repoRoot finds the folder containing go.mod, starting from the test's working directory.
func repoRoot(t *testing.T) string {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
