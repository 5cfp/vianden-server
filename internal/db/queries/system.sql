-- name: PostgresVersion :one
-- Returns the PostgreSQL version text. Logged at startup.
SELECT version()::text AS version;
