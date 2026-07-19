package tools

import (
	"context"
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/permission"
)

type ListMCPPromptsParams struct {
	MCPName string `json:"mcp_name" description:"The MCP server name"`
}

type ListMCPPromptsPermissionsParams struct {
	MCPName string `json:"mcp_name"`
}

const ListMCPPromptsToolName = "list_mcp_prompts"

//go:embed list_mcp_prompts.md
var listMCPPromptsDescription string

func NewListMCPPromptsTool(cfg *config.ConfigStore, permissions permission.Service) fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		ListMCPPromptsToolName,
		listMCPPromptsDescription,
		func(ctx context.Context, params ListMCPPromptsParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			params.MCPName = strings.TrimSpace(params.MCPName)
			if params.MCPName == "" {
				return fantasy.NewTextErrorResponse("mcp_name parameter is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for listing MCP prompts")
			}

			relPath := filepathext.SmartJoin(cfg.WorkingDir(), params.MCPName)
			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        relPath,
					ToolCallID:  call.ID,
					ToolName:    ListMCPPromptsToolName,
					Action:      "list",
					Description: fmt.Sprintf("List MCP prompts from %s", params.MCPName),
					Params:      ListMCPPromptsPermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			prompts, err := mcp.ListPrompts(ctx, cfg, params.MCPName)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(prompts) == 0 {
				return fantasy.NewTextResponse("No prompts found"), nil
			}

			blocks := make([]string, 0, len(prompts))
			for _, prompt := range prompts {
				if prompt == nil {
					continue
				}
				var b strings.Builder
				fmt.Fprintf(&b, "- %s", prompt.Name)
				if prompt.Description != "" {
					fmt.Fprintf(&b, ": %s", prompt.Description)
				}
				for _, arg := range prompt.Arguments {
					if arg == nil {
						continue
					}
					required := "optional"
					if arg.Required {
						required = "required"
					}
					fmt.Fprintf(&b, "\n    - %s (%s)", arg.Name, required)
					if arg.Description != "" {
						fmt.Fprintf(&b, ": %s", arg.Description)
					}
				}
				blocks = append(blocks, b.String())
			}

			sort.Strings(blocks)
			return fantasy.NewTextResponse(strings.Join(blocks, "\n")), nil
		},
	)
}
