package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/permission"
)

type CallMCPPromptParams struct {
	MCPName    string            `json:"mcp_name" description:"The MCP server name"`
	PromptName string            `json:"prompt_name" description:"The name of the prompt to invoke"`
	Arguments  map[string]string `json:"arguments,omitempty" description:"Arguments to pass to the prompt, as a map of argument name to value"`
}

type CallMCPPromptPermissionsParams struct {
	MCPName    string            `json:"mcp_name"`
	PromptName string            `json:"prompt_name"`
	Arguments  map[string]string `json:"arguments,omitempty"`
}

const CallMCPPromptToolName = "call_mcp_prompt"

//go:embed call_mcp_prompt.md
var callMCPPromptDescription string

func NewCallMCPPromptTool(cfg *config.ConfigStore, permissions permission.Service) fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		CallMCPPromptToolName,
		callMCPPromptDescription,
		func(ctx context.Context, params CallMCPPromptParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			params.MCPName = strings.TrimSpace(params.MCPName)
			params.PromptName = strings.TrimSpace(params.PromptName)
			if params.MCPName == "" {
				return fantasy.NewTextErrorResponse("mcp_name parameter is required"), nil
			}
			if params.PromptName == "" {
				return fantasy.NewTextErrorResponse("prompt_name parameter is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for calling MCP prompts")
			}

			relPath := filepathext.SmartJoin(cfg.WorkingDir(), params.MCPName)
			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        relPath,
					ToolCallID:  call.ID,
					ToolName:    CallMCPPromptToolName,
					Action:      "run",
					Description: fmt.Sprintf("Call MCP prompt %q from %s", params.PromptName, params.MCPName),
					Params:      CallMCPPromptPermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			messages, err := mcp.GetPromptMessages(ctx, cfg, params.MCPName, params.PromptName, params.Arguments)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(messages) == 0 {
				return fantasy.NewTextResponse("Prompt returned no content"), nil
			}

			return fantasy.NewTextResponse(strings.Join(messages, "\n")), nil
		},
	)
}
