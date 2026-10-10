package completions

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"
)

func TestFilterPrefersExactBasenameStem(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetItems([]FileCompletionValue{
		{Path: "internal/ui/chat/search.go"},
		{Path: "internal/ui/chat/user.go"},
	}, nil, nil)

	c.Filter("user")

	filtered := c.filtered
	require.NotEmpty(t, filtered)
	first, ok := filtered[0].(*CompletionItem)
	require.True(t, ok)
	require.Equal(t, "internal/ui/chat/user.go", first.Text())
	require.NotEmpty(t, first.match.MatchedIndexes)
}

func TestFilterPrefersBasenamePrefix(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetItems([]FileCompletionValue{
		{Path: "internal/ui/chat/mcp.go"},
		{Path: "internal/ui/model/chat.go"},
	}, nil, nil)

	c.Filter("chat.g")

	filtered := c.filtered
	require.NotEmpty(t, filtered)
	first, ok := filtered[0].(*CompletionItem)
	require.True(t, ok)
	require.Equal(t, "internal/ui/model/chat.go", first.Text())
	require.NotEmpty(t, first.match.MatchedIndexes)
}

func TestNamePriorityTier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		query    string
		wantTier int
	}{
		{
			name:     "exact stem",
			path:     "internal/ui/chat/user.go",
			query:    "user",
			wantTier: tierExactName,
		},
		{
			name:     "basename prefix",
			path:     "internal/ui/model/chat.go",
			query:    "chat.g",
			wantTier: tierPrefixName,
		},
		{
			name:     "path segment exact",
			path:     "internal/ui/chat/mcp.go",
			query:    "chat",
			wantTier: tierPathSegment,
		},
		{
			name:     "fallback",
			path:     "internal/ui/chat/search.go",
			query:    "user",
			wantTier: tierFallback,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := namePriorityTier(tt.path, tt.query)
			require.Equal(t, tt.wantTier, got)
		})
	}
}

func TestFilterPrefersPathSegmentExact(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetItems([]FileCompletionValue{
		{Path: "internal/ui/model/xychat.go"},
		{Path: "internal/ui/chat/mcp.go"},
	}, nil, nil)

	c.Filter("chat")

	filtered := c.filtered
	require.NotEmpty(t, filtered)
	first, ok := filtered[0].(*CompletionItem)
	require.True(t, ok)
	require.Equal(t, "internal/ui/chat/mcp.go", first.Text())
}

// Live dispatched agents ride the @ popup alongside files (#313): agent
// rows lead, the handle is the primary text, and the detail tail carries
// role, status, and current todo.
func TestSetItemsIncludesLiveAgents(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetItems([]FileCompletionValue{{Path: "internal/ui/chat/search.go"}}, nil, []AgentCompletionValue{
		{Handle: "tester", Detail: "writes tests · ● working · wiring form validation"},
	})
	c.Filter("tester")

	filtered := c.filtered
	require.NotEmpty(t, filtered)
	first, ok := filtered[0].(*CompletionItem)
	require.True(t, ok)
	require.Equal(t, "@tester", first.SortKey())
	require.Contains(t, first.Text(), "@tester")
	require.Contains(t, first.Text(), "writes tests")

	sel, ok := first.Value().(AgentCompletionValue)
	require.True(t, ok)
	require.Equal(t, "tester", sel.Handle)
}

// TestSelectAgentEmitsSelectionMsg pins the agent-completion enter path
// (#313 regression): selecting a live agent's row must emit a
// SelectionMsg[AgentCompletionValue] and close the popup. Before the fix,
// Enter fell through selectCurrent's type switch, so the popup half-closed
// — the component stopped rendering while the model still believed it was
// open, and no @handle was inserted.
func TestSelectAgentEmitsSelectionMsg(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetItems(nil, nil, []AgentCompletionValue{
		{Handle: "go-review", Detail: "reviewer · working"},
		{Handle: "sec-review", Detail: "security · working"},
	})

	msg, handled := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, handled, "enter must be consumed while the popup is open")
	selection, ok := msg.(SelectionMsg[AgentCompletionValue])
	require.True(t, ok, "enter on an agent row must emit an agent selection, got %T", msg)
	require.Equal(t, "go-review", selection.Value.Handle)
	require.False(t, selection.KeepOpen)
	require.False(t, c.IsOpen(), "the popup must be fully closed after selection")

	// The glyph prefix must not leak into the inserted value or the sort
	// key: the model inserts the bare handle and name-priority tiering
	// matches what the user typed after the @.
	require.Equal(t, "go-review", selection.Value.Handle)
	first := c.allItems[0].(*CompletionItem)
	require.Equal(t, "@go-review", first.SortKey())
}
