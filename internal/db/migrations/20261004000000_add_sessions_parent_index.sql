-- +goose Up
-- +goose StatementBegin
-- The sessions picker and the child-sessions routes read children by
-- parent. Without this, SQLite walks the whole sessions table and sorts
-- in a temp B-tree. parent_session_id first so it can seek; created_at
-- second so the grouped child listing arrives already ordered.
CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions (parent_session_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_parent;
-- +goose StatementEnd
