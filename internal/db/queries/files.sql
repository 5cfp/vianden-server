-- name: CreateAttachment :one
INSERT INTO attachments (uploader_id, filename, content_type, size, width, height, storage_key)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: CountPendingUploads :one
-- Uploads of a user not attached to a message yet (limits how much one user can park).
SELECT COUNT(*) FROM attachments WHERE uploader_id = $1 AND message_id IS NULL;

-- name: GetAttachmentForDownload :one
-- The attachment, plus where its message lives (NULL if not attached yet).
SELECT a.*, m.channel_id, (m.deleted_at IS NOT NULL)::boolean AS message_deleted
FROM attachments a
LEFT JOIN messages m ON m.id = a.message_id
WHERE a.id = $1;

-- name: LinkAttachments :many
-- Attaches the user's own, still unattached uploads to a message. The caller checks that
-- every requested id came back (anything else is someone else's, used, or unknown).
UPDATE attachments SET message_id = sqlc.arg(message_id)
WHERE id = ANY(sqlc.arg(ids)::bigint[])
  AND uploader_id = sqlc.arg(uploader_id)
  AND message_id IS NULL
RETURNING id;

-- name: ListAttachments :many
-- The attachments of the given messages, for message objects.
SELECT id, message_id, filename, content_type, size, width, height
FROM attachments
WHERE message_id = ANY(sqlc.arg(message_ids)::bigint[])
ORDER BY id;

-- name: DeleteMessageAttachments :exec
-- A deleted message loses its files (the files on disk are removed by the cleanup).
DELETE FROM attachments WHERE message_id = $1;

-- name: DeleteStaleUploads :execrows
-- Uploads never attached to a message within an hour.
DELETE FROM attachments WHERE message_id IS NULL AND created_at < now() - interval '1 hour';

-- name: ExistingStorageKeys :many
-- Which of these files on disk still belong to an attachment.
SELECT storage_key FROM attachments WHERE storage_key = ANY(sqlc.arg(keys)::text[]);
