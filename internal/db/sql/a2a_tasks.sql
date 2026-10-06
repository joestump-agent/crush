-- name: CreateA2ATask :exec
INSERT INTO a2a_tasks (
    id,
    context_id,
    "user",
    host_id,
    state,
    version,
    task_json,
    created_at,
    updated_at
) VALUES (
    @id,
    @context_id,
    @user,
    @host_id,
    @state,
    1,
    @task_json,
    @created_at,
    @updated_at
);

-- name: GetA2ATask :one
SELECT * FROM a2a_tasks
WHERE id = @id
LIMIT 1;

-- name: UpdateA2ATask :execrows
UPDATE a2a_tasks SET
    context_id = @context_id,
    state = @state,
    version = version + 1,
    task_json = @task_json,
    updated_at = @updated_at
WHERE id = @id;

-- name: UpdateA2ATaskIfVersion :execrows
UPDATE a2a_tasks SET
    context_id = @context_id,
    state = @state,
    version = version + 1,
    task_json = @task_json,
    updated_at = @updated_at
WHERE id = @id AND version = @version;

-- name: CountA2ATasks :one
SELECT count(*) FROM a2a_tasks
WHERE "user" = @user
  AND (@context_id = '' OR context_id = @context_id)
  AND (@state = '' OR state = @state)
  AND (@after = 0 OR updated_at >= @after);

-- name: ListA2ATasks :many
SELECT * FROM a2a_tasks
WHERE "user" = @user
  AND (@context_id = '' OR context_id = @context_id)
  AND (@state = '' OR state = @state)
  AND (@after = 0 OR updated_at >= @after)
  AND (@cursor_at = 0 OR updated_at < @cursor_at OR (updated_at = @cursor_at AND id < @cursor_id))
ORDER BY updated_at DESC, id DESC
LIMIT @page_size;
