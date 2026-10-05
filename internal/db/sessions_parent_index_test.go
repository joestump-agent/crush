package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChildSessionsUseParentIndex pins why idx_sessions_parent exists:
// without it, listing child sessions scans the whole sessions table and
// sorts in a temp B-tree (#408).
func TestChildSessionsUseParentIndex(t *testing.T) {
	t.Parallel()

	conn, err := Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	explain := func(t *testing.T, query string, args ...any) string {
		t.Helper()
		var plan strings.Builder
		rows, err := conn.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var id, parent, notused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
			plan.WriteString(detail)
			plan.WriteString("\n")
		}
		require.NoError(t, rows.Err())
		return plan.String()
	}

	got := explain(t, `SELECT * FROM sessions WHERE parent_session_id = ? ORDER BY updated_at ASC`, "parent")
	require.Contains(t, got, "idx_sessions_parent",
		"listing one parent's children must seek the parent index, not walk the table")

	got = explain(t,
		`SELECT * FROM sessions WHERE parent_session_id IS NOT NULL ORDER BY parent_session_id, created_at, rowid`)
	require.Contains(t, got, "idx_sessions_parent",
		"the grouped child query must use the parent index")
	// parent_session_id first, created_at second, so the grouped query
	// arrives already ordered within each parent.
	require.NotContains(t, got, "TEMP B-TREE",
		"the grouped child query should need no sort")
}
