package tools

import (
	"context"
	_ "embed"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/joestump-agent/a2tea"
)

const SidekickUpdateToolName = "sidekick_update"

//go:embed sidekick_update.md
var sidekickUpdateDescription []byte

// SidekickSurface is one dashboard push from the sidekick_update tool: an
// A2UI surface bound for the pinned dashboard slot at the top of the
// Sidekick panel (#56). Content is the payload wrapped in
// <a2ui-json>...</a2ui-json> tags, ready for the shared a2tea render
// pipeline.
type SidekickSurface struct {
	Content string
}

type SidekickUpdateParams struct {
	Surface string `json:"surface" description:"The A2UI updateComponents payload as a JSON string — the same single object you would emit inside an inline <a2ui-json> block (e.g. {\"version\":\"...\",\"updateComponents\":{...}}). Do not include the <a2ui-json> tags."`
}

// NewSidekickUpdateTool builds the main coder agent's dashboard push
// channel (#57). It validates the payload against the same a2tea scanner
// the renderer uses, publishes it to the Sidekick dashboard broker, and
// returns immediately — the surface never enters the chat message stream,
// and a slow (or absent) UI can never block the agent's turn. The tool is
// only ever given to the main coder agent, never to sub-agents or the
// Sidekick agent itself.
func NewSidekickUpdateTool(dashboard pubsub.Publisher[SidekickSurface]) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		SidekickUpdateToolName,
		string(sidekickUpdateDescription),
		func(ctx context.Context, params SidekickUpdateParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			// Be forgiving about the wrapper: the inline emission habit
			// dies hard, so accept a payload already wrapped in tags.
			payload := strings.TrimSpace(params.Surface)
			payload = strings.TrimPrefix(payload, "<a2ui-json>")
			payload = strings.TrimSuffix(payload, "</a2ui-json>")
			payload = strings.TrimSpace(payload)
			if payload == "" {
				return fantasy.NewTextErrorResponse("surface is required: pass an A2UI updateComponents JSON payload"), nil
			}

			content := "<a2ui-json>" + payload + "</a2ui-json>"
			parts, err := a2tea.Scan(content)
			if err != nil {
				return fantasy.NewTextErrorResponse("invalid A2UI payload: " + err.Error()), nil
			}
			messages := 0
			for _, p := range parts {
				messages += len(p.Messages)
			}
			if messages == 0 {
				return fantasy.NewTextErrorResponse("invalid A2UI payload: no A2UI messages found; emit a single updateComponents object"), nil
			}

			if dashboard == nil {
				return fantasy.NewTextErrorResponse("sidekick dashboard is not available"), nil
			}
			// Publish is non-blocking (lossy under back-pressure): a
			// replaced-anyway intermediate frame may drop, the turn never
			// stalls.
			dashboard.Publish(pubsub.CreatedEvent, SidekickSurface{Content: content})
			return fantasy.NewTextResponse("rendered"), nil
		},
	)
}
