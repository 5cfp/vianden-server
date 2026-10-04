-- name: ListUsers :many
SELECT * FROM users ORDER BY username;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: SetUserRole :one
UPDATE users SET role = $2 WHERE id = $1 RETURNING *;

-- name: BanUser :exec
UPDATE users SET banned_at = now(), ban_reason = $2 WHERE id = $1;

-- name: UnbanUser :exec
UPDATE users SET banned_at = NULL, ban_reason = '' WHERE id = $1;

-- name: RevokeUserSessions :execrows
-- Signs a user out everywhere (kick, ban).
UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;
