-- name: GetFile :one
SELECT *
FROM files
WHERE id = ? LIMIT 1;

-- name: GetFileByPathAndSession :one
SELECT *
FROM files
WHERE path = ? AND session_id = ?
ORDER BY version DESC, created_at DESC
LIMIT 1;

-- name: ListFilesBySession :many
SELECT *
FROM files
WHERE session_id = ?
ORDER BY version ASC, created_at ASC;

-- name: GetNextFileVersionByPath :one
SELECT COALESCE(MAX(version) + 1, 0) AS next_version
FROM files
WHERE path = ?;

-- name: CreateFile :one
INSERT INTO files (
    id,
    session_id,
    path,
    content,
    version,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, strftime('%s', 'now'), strftime('%s', 'now')
)
RETURNING *;

-- name: DeleteFile :exec
DELETE FROM files
WHERE id = ?;

-- name: DeleteSessionFiles :exec
DELETE FROM files
WHERE session_id = ?;

-- name: ListLatestSessionFiles :many
SELECT f.*
FROM files f
WHERE f.session_id = ?
  AND NOT EXISTS (
      SELECT 1
      FROM files newer
      WHERE newer.session_id = f.session_id
        AND newer.path = f.path
        AND (
            newer.version > f.version
            OR (newer.version = f.version AND newer.created_at > f.created_at)
            OR (newer.version = f.version AND newer.created_at = f.created_at AND newer.id > f.id)
        )
  )
ORDER BY f.path;
