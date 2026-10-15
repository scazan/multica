-- =====================
-- Asana Connection
-- =====================

-- name: ListAsanaConnectionsByWorkspace :many
SELECT * FROM asana_connection
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: GetAsanaConnectionByID :one
SELECT * FROM asana_connection
WHERE id = $1;

-- name: UpsertAsanaConnection :one
-- Reconnecting the same Asana workspace rotates the stored token and identity
-- in place rather than creating a duplicate row, so existing project bindings
-- keep working under the new token.
INSERT INTO asana_connection (
    workspace_id, asana_workspace_gid, asana_workspace_name,
    asana_user_gid, asana_user_name, asana_user_email,
    access_token_encrypted, connected_by_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, sqlc.narg('connected_by_id')
)
ON CONFLICT (workspace_id, asana_workspace_gid) DO UPDATE SET
    asana_workspace_name   = EXCLUDED.asana_workspace_name,
    asana_user_gid         = EXCLUDED.asana_user_gid,
    asana_user_name        = EXCLUDED.asana_user_name,
    asana_user_email       = EXCLUDED.asana_user_email,
    access_token_encrypted = EXCLUDED.access_token_encrypted,
    connected_by_id        = EXCLUDED.connected_by_id,
    updated_at             = now()
RETURNING *;

-- name: DeleteAsanaConnection :exec
-- No FKs, so the bindings, task links, and comment links that hang off the
-- connection are swept here in one statement so they commit or roll back
-- atomically with the connection row. The target CTE scopes every child delete
-- to a connection that belongs to the workspace, so a wrong workspace_id is a
-- no-op rather than deleting another tenant's rows. Mirrored issues are left in
-- place: they are real Multica issues the team may still be working on.
WITH target AS (
    SELECT asana_connection.id FROM asana_connection
    WHERE asana_connection.id = $1 AND asana_connection.workspace_id = $2
),
target_bindings AS (
    SELECT asana_project_binding.id FROM asana_project_binding
    WHERE asana_project_binding.connection_id IN (SELECT target.id FROM target)
),
target_links AS (
    SELECT asana_task_link.id FROM asana_task_link
    WHERE asana_task_link.binding_id IN (SELECT target_bindings.id FROM target_bindings)
),
cleared_comment_links AS (
    DELETE FROM asana_comment_link
    WHERE task_link_id IN (SELECT target_links.id FROM target_links)
),
cleared_task_links AS (
    DELETE FROM asana_task_link
    WHERE id IN (SELECT target_links.id FROM target_links)
),
cleared_bindings AS (
    DELETE FROM asana_project_binding
    WHERE id IN (SELECT target_bindings.id FROM target_bindings)
)
DELETE FROM asana_connection
WHERE asana_connection.id = $1 AND asana_connection.workspace_id = $2;

-- =====================
-- Asana Project Binding
-- =====================

-- name: ListAsanaProjectBindingsByWorkspace :many
SELECT * FROM asana_project_binding
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: ListAsanaProjectBindingsByConnection :many
SELECT * FROM asana_project_binding
WHERE connection_id = $1
ORDER BY created_at ASC;
