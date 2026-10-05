-- name: ListChannels :many
-- All channels in room-list order, each with its newest message (if any) for the preview.
SELECT
    c.id, c.name, c.topic, c.type, c.position, c.view_role, c.send_role,
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
      AND m.deleted_at IS NULL -- the preview shows the newest message that still has text
    ORDER BY m.id DESC
    LIMIT 1
) lm ON true
LEFT JOIN users u ON u.id = lm.author_id
ORDER BY c.position, c.id;

-- name: CreateChannel :one
-- New channels go to the end of the list.
INSERT INTO channels (name, topic, view_role, send_role, type, position)
VALUES ($1, $2, $3, $4, $5, (SELECT COALESCE(MAX(position), -1) + 1 FROM channels))
RETURNING *;

-- name: UpdateChannel :one
UPDATE channels SET name = $2, topic = $3, view_role = $4, send_role = $5 WHERE id = $1
RETURNING *;

-- name: GetChannel :one
SELECT * FROM channels WHERE id = $1;

-- name: DeleteChannel :execrows
DELETE FROM channels WHERE id = $1;

-- name: CreateMessage :one
INSERT INTO messages (channel_id, author_id, content, reply_to_id, mentions_everyone)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: ListMessages :many
-- One page of history, NEWEST first. "before" is the id of the oldest message the
-- client already has (NULL for the newest page). Keyset pagination: see docs.
-- r / ru = the message this one replies to, and its author (all NULL if not a reply).
-- Keep the column list identical to GetMessage (the Go code converts between the two).
SELECT
    m.id, m.channel_id, m.content, m.created_at, m.edited_at, m.author_id,
    (m.deleted_at IS NOT NULL)::boolean AS deleted, m.mentions_everyone,
    u.username     AS author_username,
    u.display_name AS author_display_name,
    m.reply_to_id,
    r.content      AS reply_content,
    (r.deleted_at IS NOT NULL)::boolean AS reply_deleted,
    r.author_id    AS reply_author_id,
    ru.username     AS reply_author_username,
    ru.display_name AS reply_author_display_name
FROM messages m
LEFT JOIN users u ON u.id = m.author_id
LEFT JOIN messages r ON r.id = m.reply_to_id
LEFT JOIN users ru ON ru.id = r.author_id
WHERE m.channel_id = sqlc.arg(channel_id)
  AND (sqlc.narg(before)::bigint IS NULL OR m.id < sqlc.narg(before)::bigint)
ORDER BY m.id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetMessage :one
-- One message in the same shape as ListMessages.
SELECT
    m.id, m.channel_id, m.content, m.created_at, m.edited_at, m.author_id,
    (m.deleted_at IS NOT NULL)::boolean AS deleted, m.mentions_everyone,
    u.username     AS author_username,
    u.display_name AS author_display_name,
    m.reply_to_id,
    r.content      AS reply_content,
    (r.deleted_at IS NOT NULL)::boolean AS reply_deleted,
    r.author_id    AS reply_author_id,
    ru.username     AS reply_author_username,
    ru.display_name AS reply_author_display_name
FROM messages m
LEFT JOIN users u ON u.id = m.author_id
LEFT JOIN messages r ON r.id = m.reply_to_id
LEFT JOIN users ru ON ru.id = r.author_id
WHERE m.id = $1 AND m.channel_id = $2;

-- name: ReplyTarget :one
-- A live (not deleted) message in this channel that a reply may point at, and its author.
SELECT author_id FROM messages WHERE id = $1 AND channel_id = $2 AND deleted_at IS NULL;

-- name: EditMessage :execrows
-- Only the author can edit, and only a live message.
UPDATE messages SET content = $3, mentions_everyone = $4, edited_at = now()
WHERE id = $1 AND author_id = $2 AND deleted_at IS NULL;

-- name: GetMessageWithAuthor :one
-- A live (not deleted) message in a channel, with its author's role (for the hierarchy rule).
SELECT m.id, m.channel_id, m.author_id, u.role AS author_role
FROM messages m
LEFT JOIN users u ON u.id = m.author_id
WHERE m.id = $1 AND m.channel_id = $2 AND m.deleted_at IS NULL;

-- name: DeleteMessage :execrows
-- Erases the text for real (not just hidden) and keeps a placeholder row.
UPDATE messages SET content = '', deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- ---- mentions (M6) ----

-- name: MentionCandidates :many
-- The users named in a message (@username), plus the author of the message it replies to.
SELECT id, role FROM users
WHERE username = ANY(sqlc.arg(usernames)::text[]) OR id = sqlc.arg(reply_author_id);

-- name: DeleteMentions :exec
DELETE FROM message_mentions WHERE message_id = $1;

-- name: AddMentions :exec
INSERT INTO message_mentions (message_id, user_id)
SELECT sqlc.arg(message_id), unnest(sqlc.arg(user_ids)::bigint[])
ON CONFLICT DO NOTHING;

-- name: ListMentions :many
-- Who the given messages mention (for message objects).
SELECT mm.message_id, u.id, u.username, u.display_name
FROM message_mentions mm
JOIN users u ON u.id = mm.user_id
WHERE mm.message_id = ANY(sqlc.arg(message_ids)::bigint[])
ORDER BY mm.message_id, u.id;

-- ---- unread (M6) ----

-- name: UnreadCounts :many
-- Per channel, for one user: the last message they read, how many newer messages there
-- are, and how many of those mention them. Their own messages, deleted messages, and
-- messages from before their account existed never count. Counts stop at 100 (the
-- client shows "99+"), so a huge backlog stays cheap to count.
SELECT
    c.id AS channel_id,
    COALESCE(rs.last_read_id, 0)::bigint AS last_read_id,
    (SELECT COUNT(*) FROM (
        SELECT 1 FROM messages m
        WHERE m.channel_id = c.id AND m.id > COALESCE(rs.last_read_id, 0)
          AND m.created_at > u.created_at AND m.deleted_at IS NULL
          AND m.author_id IS DISTINCT FROM u.id
        LIMIT 100) x)::int AS unread_count,
    (SELECT COUNT(*) FROM (
        SELECT 1 FROM messages m
        WHERE m.channel_id = c.id AND m.id > COALESCE(rs.last_read_id, 0)
          AND m.created_at > u.created_at AND m.deleted_at IS NULL
          AND m.author_id IS DISTINCT FROM u.id
          AND (m.mentions_everyone OR EXISTS (
              SELECT 1 FROM message_mentions mm WHERE mm.message_id = m.id AND mm.user_id = u.id))
        LIMIT 100) y)::int AS mention_count
FROM channels c
JOIN users u ON u.id = sqlc.arg(user_id)
LEFT JOIN read_states rs ON rs.user_id = u.id AND rs.channel_id = c.id;

-- name: MarkRead :one
-- Moves the read marker forward (never back), and never past the newest message.
INSERT INTO read_states (user_id, channel_id, last_read_id)
VALUES (
    sqlc.arg(user_id), sqlc.arg(channel_id),
    LEAST(sqlc.arg(message_id)::bigint,
          (SELECT COALESCE(MAX(id), 0) FROM messages WHERE channel_id = sqlc.arg(channel_id)))
)
ON CONFLICT (user_id, channel_id)
DO UPDATE SET last_read_id = GREATEST(read_states.last_read_id, EXCLUDED.last_read_id)
RETURNING last_read_id;
