-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS a2a_dispatches (
    dispatch_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    parent_session_id TEXT NOT NULL DEFAULT '',
    handle TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    workspace_path TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    result_json TEXT NOT NULL DEFAULT '',
    delivered_at INTEGER NOT NULL DEFAULT 0,
    host_id TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_a2a_dispatches_task_id ON a2a_dispatches (task_id);
CREATE INDEX IF NOT EXISTS idx_a2a_dispatches_parent_session_id ON a2a_dispatches (parent_session_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_a2a_dispatches_parent_session_id;
DROP INDEX IF EXISTS idx_a2a_dispatches_task_id;
DROP TABLE IF EXISTS a2a_dispatches;
-- +goose StatementEnd
