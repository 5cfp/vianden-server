# vianden-server

Backend for **Project Vianden** (code name), a self-hosted chat platform with text and voice channels.
One Go binary + PostgreSQL.

> Status: early development (milestone M0). Not usable yet.

## Requirements
- Go 1.27+
- Docker + Docker Compose (for local PostgreSQL)

## Setup
1. Create your local config (never committed):
   ```powershell
   Copy-Item .env.example .env
   ```
   Edit `.env` and replace every `change-me` with a strong password (the same value in `VIANDEN_DB_PASSWORD` and inside `VIANDEN_DATABASE_URL`).
2. Start the local PostgreSQL database:
   ```powershell
   docker compose up -d
   docker compose ps          # should show "healthy"
   ```
   It listens on `127.0.0.1:5432` only (not reachable from other devices).

| Task | Command |
|---|---|
| Stop the database (keeps data) | `docker compose down` |
| Delete all local data | `docker compose down -v` |
| Open a SQL shell | `docker compose exec postgres psql -U vianden -d vianden` |

3. Run the server:
   ```powershell
   go run ./cmd/server
   ```
   Check it: open http://127.0.0.1:8080/api/v1/info in a browser. Stop it with `Ctrl+C`.

## Tests
Some tests need a real PostgreSQL database. They use a separate `vianden_test` database (set by `VIANDEN_TEST_DATABASE_URL` in `.env`) and **delete all data in it**. Create it once:
```powershell
docker compose exec postgres createdb -U vianden vianden_test
```
Then run all tests:
```powershell
go test ./...
```
Without `VIANDEN_TEST_DATABASE_URL`, database tests are skipped (not failed).

## First run: becoming the owner
On a server without an owner, the console shows a one-time setup token (`vo_...`). Register with it as the invite code to become the owner. It works once, and is only printed while the server has no owner.

## Database code (sqlc)
SQL queries live in `internal/db/queries/*.sql`. [sqlc](https://sqlc.dev) turns them into Go functions (files named `sqlc_*.go` and `*.sql.go` in `internal/db/`; never edit those by hand).

After changing a query or adding a migration, regenerate (needs Docker, run in this folder):
```powershell
docker run --rm -v "${PWD}:/src" -w /src sqlc/sqlc:1.31.1 generate
```
Commit the generated files together with the SQL change.

## Migrations
Migrations live in `internal/db/migrations/` as numbered goose files (`00001_init.sql`, `00002_...sql`). They are built into the binary and applied automatically at startup.
**Never edit a migration that is already committed**; add a new one instead.

## License check
This project only allows permissive dependency licenses. Run this whenever dependencies change (`go.mod`):
```powershell
go install github.com/google/go-licenses/v2@latest   # once
go-licenses check ./... --allowed_licenses=MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,Zlib,Unlicense,PostgreSQL
go-licenses report ./...                              # list every dependency and its license
```
No output from `check` means everything is allowed.

## Project layout
| Folder | What it contains |
|---|---|
| `cmd/server/` | Entry point (`main.go`): loads config, starts services |
| `internal/accounts/` | Account logic: owner setup, registration, input validation |
| `internal/api/` | REST API handlers |
| `internal/auth/` | Password hashing (Argon2id) and secret tokens (sessions, invites) |
| `internal/chat/` | Text channels and messages (validation, permissions, history pagination) |
| `internal/config/` | Loads settings from environment variables / `.env` |
| `internal/db/` | PostgreSQL connection + migration runner |
| `internal/db/migrations/` | Database migrations (`.sql`, applied automatically on startup) |
| `internal/db/queries/` | SQL queries; sqlc generates Go code from them |
| `internal/supervisor/` | Restarts a service if it crashes |
| `internal/testdb/` | Test helper: a clean, migrated test database |
| `internal/buildinfo/` | Server and protocol version numbers |
| `docs/API.md` | Full API documentation for client developers |

## Documentation
- API for client developers: [docs/API.md](docs/API.md)

## License
MIT, see [LICENSE](LICENSE).
