package tools

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

// TestMCPPromptTools_Info pins the tool names and that automatic schema
// generation handles the tools' inputs — in particular the call tool's
// free-form map argument, which must render as an object parameter.
func TestMCPPromptTools_Info(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{})
	perms := permission.NewPermissionService(cfg.WorkingDir(), false, nil)

	list := NewListMCPPromptsTool(cfg, perms).Info()
	require.Equal(t, ListMCPPromptsToolName, list.Name)
	require.Contains(t, list.Parameters, "mcp_name")
	require.Contains(t, list.Required, "mcp_name")

	call := NewCallMCPPromptTool(cfg, perms).Info()
	require.Equal(t, CallMCPPromptToolName, call.Name)
	require.Contains(t, call.Parameters, "mcp_name")
	require.Contains(t, call.Parameters, "prompt_name")
	require.Contains(t, call.Parameters, "arguments")
	require.Subset(t, call.Required, []string{"mcp_name", "prompt_name"})
	require.NotContains(t, call.Required, "arguments", "arguments is optional")
}
