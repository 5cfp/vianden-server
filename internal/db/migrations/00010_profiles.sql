-- Profiles: avatars (milestone M6, step 4). Display names already exist.

-- +goose Up

-- Random file name of the user's avatar (a 256x256 PNG made by the server); NULL = none.
ALTER TABLE users ADD COLUMN avatar_key TEXT UNIQUE CHECK (avatar_key ~ '^[0-9a-f]{64}$');

-- +goose Down

ALTER TABLE users DROP COLUMN avatar_key;
