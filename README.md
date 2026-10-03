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

More run instructions will be added as the server grows.

## Documentation
- API for client developers: `docs/API.md` (coming in M0)

## License
MIT, see [LICENSE](LICENSE).
