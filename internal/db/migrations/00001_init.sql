-- Baseline migration: from here on, the database schema is managed by goose.
-- The first real tables (users, sessions, invites) arrive in milestone M1.
--
-- Rules for migrations:
--   * Never edit a migration that has been committed; add a new numbered file instead.
--   * Each file has an "Up" part (apply) and a "Down" part (undo).

-- +goose Up
SELECT 1;

-- +goose Down
SELECT 1;
