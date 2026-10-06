-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS a2a_tasks (
    id TEXT PRIMARY KEY,
    context_id TEXT NOT NULL,
    "user" TEXT NOT NULL DEFAULT '',
    host_id TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    version INTEGER NOT NULL,
    task_json TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_a2a_tasks_context_id ON a2a_tasks (context_id);
CREATE INDEX IF NOT EXISTS idx_a2a_tasks_state_updated ON a2a_tasks (state, updated_at);
CREATE INDEX IF NOT EXISTS idx_a2a_tasks_host_state ON a2a_tasks (host_id, state);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_a2a_tasks_host_state;
DROP INDEX IF EXISTS idx_a2a_tasks_state_updated;
DROP INDEX IF EXISTS idx_a2a_tasks_context_id;
DROP TABLE IF EXISTS a2a_tasks;
-- +goose StatementEnd
