-- Accounts (milestone M1): users, login sessions, and invite codes.

-- +goose Up

CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Stored in lowercase, so "Osama" and "osama" cannot both exist (prevents look-alike impersonation).
    username      TEXT NOT NULL UNIQUE CHECK (username = lower(username)),
    display_name  TEXT NOT NULL,
    -- Argon2id hash string, including its random salt. Never the password itself.
    password_hash TEXT NOT NULL,
    -- The server owner. Full roles arrive in M5.
    is_owner      BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one owner: a unique index over only the rows where is_owner is true.
CREATE UNIQUE INDEX users_single_owner_idx ON users (is_owner) WHERE is_owner;

CREATE TABLE sessions (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- SHA-256 of the session token. The token itself is only ever known by the client.
    token_hash   BYTEA NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Pushed forward when the session is used (sliding expiry).
    expires_at   TIMESTAMPTZ NOT NULL,
    -- Set on logout or when the session is revoked; a revoked session never works again.
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);

CREATE TABLE invites (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- SHA-256 of the invite code, like session tokens: a leaked database does not reveal usable codes.
    -- The code is shown to its creator once, when it is created.
    code_hash  BYTEA NOT NULL UNIQUE,
    created_by BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    max_uses   INTEGER NOT NULL DEFAULT 1 CHECK (max_uses > 0),
    uses       INTEGER NOT NULL DEFAULT 0 CHECK (uses >= 0 AND uses <= max_uses),
    -- Every invite expires: no forgotten codes that work forever.
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE invites;
DROP TABLE sessions;
DROP TABLE users;
