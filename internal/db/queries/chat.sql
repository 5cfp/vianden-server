-- name: ListChannels :many
-- All channels in room-list order, each with its newest message (if any) for the preview.
SELECT
    c.id, c.name, c.topic, c.type, c.position,
    -- sqlc cannot tell that a LATERAL join may find nothing, so make "no message" explicit.
    (lm.created_at IS NOT NULL)::boolean        AS has_last_message,
    COALESCE(lm.content, '')::text            AS last_content,
    COALESCE(lm.created_at, now())::timestamptz AS last_created_at,
    u.display_name AS last_author
FROM channels c
LEFT JOIN LATERAL (
    SELECT m.content, m.created_at, m.author_id
    FROM messages m
    WHERE m.channel_id = c.id
    ORDER BY m.id DESC
    LIMIT 1
) lm ON true
LEFT JOIN users u ON u.id = lm.author_id
ORDER BY c.position, c.id;

-- name: CreateChannel :one
-- New channels go to the end of the list.
INSERT INTO channels (name, topic, position)
VALUES ($1, $2, (SELECT COALESCE(MAX(position), -1) + 1 FROM channels))
RETURNING *;

-- name: UpdateChannel :one
UPDATE channels SET name = $2, topic = $3 WHERE id = $1
RETURNING *;

-- name: GetChannel :one
SELECT * FROM channels WHERE id = $1;

-- name: DeleteChannel :execrows
DELETE FROM channels WHERE id = $1;

-- name: CreateMessage :one
INSERT INTO messages (channel_id, author_id, content)
VALUES ($1, $2, $3)
RETURNING id, channel_id, content, created_at;

-- name: ListMessages :many
-- One page of history, NEWEST first. "before" is the id of the oldest message the
-- client already has (NULL for the newest page). Keyset pagination: see docs.
SELECT
    m.id, m.channel_id, m.content, m.created_at, m.author_id,
    u.username     AS author_username,
    u.display_name AS author_display_name
FROM messages m
LEFT JOIN users u ON u.id = m.author_id
WHERE m.channel_id = sqlc.arg(channel_id)
  AND (sqlc.narg(before)::bigint IS NULL OR m.id < sqlc.narg(before)::bigint)
ORDER BY m.id DESC
LIMIT sqlc.arg(row_limit);
