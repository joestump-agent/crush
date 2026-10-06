package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// agentCrushrc is the #431 acceptance script, with the model pin swapped
// for one the test provider list contains.
const agentCrushrc = `agent defaults --nudge-after 4 --kill-after-nudges off
agent set coder --deny-tool sourcegraph
agent set task --nudge false
agent add worker --role dispatch --model openai/gpt-4 --prompt builtin:dispatch --prompt-append file:PROMPT_PATH --tools @read @write job_output job_kill lsp_diagnostics --mcp github:get_issue --skill go-patterns --workspace worktree --kill-after-nudges 2 --stall 5m --timeout 30m
agent add reviewer --role dispatch --runtime a2a --card https://reviewer.example.net/.well-known/agent-card.json --bearer '$REVIEWER_TOKEN' --workspace none --idle-timeout 2m
mcp add github --command glow`

// agentCrushrcJSON is the equivalent agents block for crush.json.
const agentCrushrcJSON = `{
	"$defaults": {
		"todos": {"nudge_after_tool_calls": 4},
		"kill": {"after_ignored_nudges": "off"}
	},
	"coder": {"tools": {"deny": ["sourcegraph"]}},
	"task": {"todos": {"nudge": false}},
	"worker": {
		"role": "dispatch",
		"model": {"provider": "openai", "model": "gpt-4"},
		"prompt": "builtin:dispatch",
		"prompt_append": "file:PROMPT_PATH",
		"tools": {"allow": ["@read", "@write", "job_output", "job_kill", "lsp_diagnostics"]},
		"mcp": {"allow": ["github:get_issue"]},
		"skills": ["go-patterns"],
		"workspace": "worktree",
		"kill": {"after_ignored_nudges": 2, "stall": "5m", "timeout": "30m"}
	},
	"reviewer": {
		"role": "dispatch",
		"runtime": "a2a",
		"card": "https://reviewer.example.net/.well-known/agent-card.json",
		"auth": {"type": "bearer", "token": "$REVIEWER_TOKEN"},
		"workspace": "none",
		"transport": {"idle_timeout": "2m"}
	}
}`

// TestAgentDefinitionsCrushrcMatchesJson verifies that a crushrc using the
// agent builtin and the equivalent crush.json resolve to identical
// Config.Agents through the real load pipeline (#431).
func TestAgentDefinitionsCrushrcMatchesJson(t *testing.T) {
	// The prompt_append file must exist for load-time validation to pass;
	// both variants reference the same absolute path.
	promptPath := filepath.Join(t.TempDir(), "worker.md")
	require.NoError(t, os.WriteFile(promptPath, []byte("Be brief."), 0o644))
	// Forward slashes survive both substitution targets: the bash
	// interpreter eats backslash escapes in crushrc, and JSON rejects
	// them as invalid escapes. Windows accepts slashed paths.
	refPath := filepath.ToSlash(promptPath)

	load := func(useCrushrc bool) map[string]config.Agent {
		t.Helper()
		workDir, dataDir := isolateReloadEnv(t)
		globalDir := os.Getenv("CRUSH_GLOBAL_CONFIG")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))

		body := replaceAll(agentsBaseConfig, `"mcp": {`, `"mcp": {
		"github": {"type": "stdio", "command": "glow"},`)
		globalName := "crush.json"
		if !useCrushrc {
			agentsJSON := replaceAll(agentCrushrcJSON, "PROMPT_PATH", refPath)
			body = replaceAll(body, `"providers": {`, `"agents": `+agentsJSON+`,"providers": {`)
		}
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, globalName), []byte(body), 0o600))

		if useCrushrc {
			rc := replaceAll(agentCrushrc, "PROMPT_PATH", refPath)
			require.NoError(t, os.WriteFile(filepath.Join(workDir, "crushrc"), []byte(rc), 0o644))
		}

		store, err := config.Load(workDir, dataDir, false)
		require.NoError(t, err)
		return store.Config().Agents
	}

	rcAgents := load(true)
	jsonAgents := load(false)

	require.Len(t, rcAgents, 5, "$defaults is not an agent; coder, plan, task, worker, reviewer are")
	require.Equal(t, jsonAgents, rcAgents)
}

func replaceAll(s, old, new string) string {
	return strings.ReplaceAll(s, old, new)
}
