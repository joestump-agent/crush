-- name: UpsertA2ADispatch :exec
INSERT INTO a2a_dispatches (
    dispatch_id,
    task_id,
    session_id,
    parent_session_id,
    handle,
    branch,
    workspace_path,
    status,
    result_json,
    delivered_at,
    host_id,
    updated_at
) VALUES (
    @dispatch_id,
    @task_id,
    @session_id,
    @parent_session_id,
    @handle,
    @branch,
    @workspace_path,
    @status,
    @result_json,
    @delivered_at,
    @host_id,
    @updated_at
)
ON CONFLICT (dispatch_id) DO UPDATE SET
    task_id = excluded.task_id,
    session_id = excluded.session_id,
    parent_session_id = excluded.parent_session_id,
    handle = excluded.handle,
    branch = excluded.branch,
    workspace_path = excluded.workspace_path,
    status = excluded.status,
    result_json = excluded.result_json,
    delivered_at = excluded.delivered_at,
    host_id = excluded.host_id,
    updated_at = excluded.updated_at;

-- name: SetA2ADispatchTask :exec
UPDATE a2a_dispatches SET
    task_id = @task_id,
    updated_at = @updated_at
WHERE dispatch_id = @dispatch_id;

-- name: FinishA2ADispatch :execrows
UPDATE a2a_dispatches SET
    status = @status,
    result_json = @result_json,
    updated_at = @updated_at
WHERE dispatch_id = @dispatch_id AND status = 'running';

-- name: FailA2ADispatch :execrows
UPDATE a2a_dispatches SET
    status = 'failed',
    result_json = @result_json,
    updated_at = @updated_at
WHERE dispatch_id = @dispatch_id AND status = 'running';

-- name: MarkA2ADispatchDelivered :exec
UPDATE a2a_dispatches SET
    delivered_at = @delivered_at,
    updated_at = @delivered_at
WHERE dispatch_id = @dispatch_id;

-- name: GetA2ADispatch :one
SELECT * FROM a2a_dispatches
WHERE dispatch_id = @dispatch_id
LIMIT 1;

-- name: GetA2ADispatchBySession :one
SELECT * FROM a2a_dispatches
WHERE session_id = @session_id
LIMIT 1;

-- name: ListNonTerminalA2ADispatches :many
SELECT * FROM a2a_dispatches
WHERE status = 'running'
ORDER BY updated_at;

-- name: ListUndeliveredA2ADispatches :many
SELECT * FROM a2a_dispatches
WHERE status IN ('completed', 'failed', 'killed')
  AND delivered_at = 0
ORDER BY updated_at;

-- name: ListNonTerminalA2ATasks :many
SELECT * FROM a2a_tasks
WHERE state NOT IN ('TASK_STATE_COMPLETED', 'TASK_STATE_FAILED', 'TASK_STATE_CANCELED', 'TASK_STATE_REJECTED')
ORDER BY updated_at;
