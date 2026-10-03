-- name: OwnerExists :one
SELECT EXISTS (SELECT 1 FROM users WHERE is_owner);

-- name: CreateUser :one
INSERT INTO users (username, display_name, password_hash, is_owner)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: CreateSession :exec
INSERT INTO sessions (user_id, token_hash, expires_at)
VALUES ($1, $2, $3);

-- name: LockUsableInvite :one
-- Finds an invite that is not expired or used up, and LOCKS its row until the
-- transaction ends. Two people registering with the same last-use invite at the
-- same moment then cannot both succeed: the second one waits, re-checks, and finds nothing.
SELECT * FROM invites
WHERE code_hash = $1 AND uses < max_uses AND expires_at > now()
FOR UPDATE;

-- name: UseInvite :exec
UPDATE invites SET uses = uses + 1 WHERE id = $1;
