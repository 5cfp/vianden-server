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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/5cfp/vianden-server/internal/db"
)

// New connects to the test database, applies all migrations, and empties every table.
// The pool is closed automatically when the test ends.
//
// Tests using it must not run in parallel with each other (they share one database).
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
	if err := db.Migrate(ctx, url, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	// Start every test from empty tables. RESTART IDENTITY resets the id counters to 1.
	if _, err := pool.Exec(ctx, "TRUNCATE users, sessions, invites RESTART IDENTITY CASCADE"); err != nil {
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
