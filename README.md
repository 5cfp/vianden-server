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
```powershell
go test ./...
```

## Project layout
| Folder | What it contains |
|---|---|
| `cmd/server/` | Entry point (`main.go`): loads config, starts services |
| `internal/api/` | REST API handlers |
| `internal/config/` | Loads settings from environment variables / `.env` |
| `internal/supervisor/` | Restarts a service if it crashes |
| `internal/buildinfo/` | Server and protocol version numbers |
| `docs/API.md` | Full API documentation for client developers |

## Documentation
- API for client developers: [docs/API.md](docs/API.md)

## License
MIT, see [LICENSE](LICENSE).
