-- name: CreateInvite :one
INSERT INTO invites (code_hash, created_by, max_uses, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListInvites :many
SELECT * FROM invites ORDER BY created_at DESC, id DESC;

-- name: DeleteInvite :execrows
DELETE FROM invites WHERE id = $1;
