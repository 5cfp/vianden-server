-- Roles and bans (milestone M5). Every user has exactly one role; "is_owner" becomes role = 'owner'.

-- +goose Up

ALTER TABLE users
    ADD COLUMN role TEXT NOT NULL DEFAULT 'member'
        CHECK (role IN ('owner', 'admin', 'moderator', 'member'));

UPDATE users SET role = 'owner' WHERE is_owner;

DROP INDEX users_single_owner_idx;
ALTER TABLE users DROP COLUMN is_owner;

-- Still at most one owner.
CREATE UNIQUE INDEX users_single_owner_idx ON users (role) WHERE role = 'owner';

-- A banned user cannot log in (NULL = not banned). The reason is shown to admins only.
ALTER TABLE users
    ADD COLUMN banned_at  TIMESTAMPTZ,
    ADD COLUMN ban_reason TEXT NOT NULL DEFAULT '' CHECK (char_length(ban_reason) <= 200),
    -- The owner can never be banned (defense in depth: the code checks this too).
    ADD CONSTRAINT users_owner_not_banned CHECK (role <> 'owner' OR banned_at IS NULL);

-- +goose Down

ALTER TABLE users DROP CONSTRAINT users_owner_not_banned;
ALTER TABLE users DROP COLUMN ban_reason, DROP COLUMN banned_at;
DROP INDEX users_single_owner_idx;
ALTER TABLE users ADD COLUMN is_owner BOOLEAN NOT NULL DEFAULT false;
UPDATE users SET is_owner = (role = 'owner');
CREATE UNIQUE INDEX users_single_owner_idx ON users (is_owner) WHERE is_owner;
ALTER TABLE users DROP COLUMN role;
