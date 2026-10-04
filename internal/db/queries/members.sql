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

-- name: SetDisplayName :one
UPDATE users SET display_name = $2 WHERE id = $1 RETURNING *;

-- name: SetAvatarKey :one
-- Returns the updated user. (The old avatar file is removed by the caller / the cleanup.)
UPDATE users SET avatar_key = $2 WHERE id = $1 RETURNING *;

-- name: ExistingAvatarKeys :many
-- Which of these avatar files on disk still belong to a user.
SELECT avatar_key::text FROM users WHERE avatar_key = ANY(sqlc.arg(keys)::text[]);
