package shellconfig_test

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/shellconfig"
	"github.com/stretchr/testify/require"
)

// goldenAgentScript is the acceptance case from #431: every subcommand and
// every flag kind in one script.
const goldenAgentScript = `agent defaults --nudge-after 4 --kill-after-nudges off
agent set coder --deny-tool sourcegraph
agent set task --nudge false
agent add worker --role dispatch --model openai/gpt-5-mini --prompt builtin:dispatch --prompt-append .crush/prompts/worker.md --tools @read @write job_output job_kill lsp_diagnostics --mcp github:get_issue --skill go-patterns --workspace worktree --kill-after-nudges 2 --stall 5m --timeout 30m
agent add reviewer --role dispatch --runtime a2a --card https://reviewer.example.net/.well-known/agent-card.json --bearer '$REVIEWER_TOKEN' --workspace none --idle-timeout 2m`

// goldenAgentJSON is exactly what the golden script must produce.
const goldenAgentJSON = `{
  "$defaults": {"todos": {"nudge_after_tool_calls": 4}, "kill": {"after_ignored_nudges": "off"}},
  "coder": {"tools": {"deny": ["sourcegraph"]}},
  "task": {"todos": {"nudge": false}},
  "worker": {
    "role": "dispatch",
    "model": {"provider": "openai", "model": "gpt-5-mini"},
    "prompt": "builtin:dispatch",
    "prompt_append": "file:.crush/prompts/worker.md",
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

// loadAgentScript runs a crushrc through the real loader and returns the
// agents section as parsed JSON (numbers are float64).
func loadAgentScript(t *testing.T, script string) map[string]any {
	t.Helper()
	path := t.TempDir() + "/crushrc"
	data, err := shellconfig.LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)
	require.NotNil(t, data, "script produced no config")
	var result map[string]any
	require.NoError(t, json.Unmarshal(data, &result))
	agents, ok := result["agents"].(map[string]any)
	require.True(t, ok, "script must populate the agents section")
	return agents
}

// loadAgentScriptErr is loadAgentScript for scripts expected to fail.
func loadAgentScriptErr(t *testing.T, script string) error {
	t.Helper()
	path := t.TempDir() + "/crushrc"
	_, err := shellconfig.LoadShellConfig(t.Context(), path, []byte(script))
	return err
}

// TestAgentGoldenScript pins the #431 acceptance case: the whole flag
// surface produces exactly the documented JSON.
func TestAgentGoldenScript(t *testing.T) {
	t.Parallel()

	agents := loadAgentScript(t, goldenAgentScript)
	out, err := json.Marshal(agents)
	require.NoError(t, err)
	require.JSONEq(t, goldenAgentJSON, string(out))
}

// TestAgentDefaultsOnlyTodosAndKill verifies the $defaults subcommand
// writes the defaults key and rejects every other flag.
func TestAgentDefaultsOnlyTodosAndKill(t *testing.T) {
	t.Parallel()

	agents := loadAgentScript(t, `agent defaults --nudge true --nudge-after 3 --hard-gate on --kill-after-nudges 0 --stall 90s --timeout 1h`)
	defaults := agents["$defaults"].(map[string]any)
	require.Equal(t, map[string]any{
		"nudge":                  true,
		"nudge_after_tool_calls": float64(3),
		"hard_gate":              true,
	}, defaults["todos"])
	require.Equal(t, map[string]any{
		"after_ignored_nudges": float64(0),
		"stall":                "90s",
		"timeout":              "1h",
	}, defaults["kill"])

	err := loadAgentScriptErr(t, `agent defaults --role dispatch`)
	require.ErrorContains(t, err, "unknown flag --role")
}

// TestAgentAddOverlaysExistingID verifies that re-adding an id updates the
// same entry in place, the way mcp add does.
func TestAgentAddOverlaysExistingID(t *testing.T) {
	t.Parallel()

	agents := loadAgentScript(t, `agent add porter --role dispatch
agent add porter --description "moves boxes"`)
	porter := agents["porter"].(map[string]any)
	require.Equal(t, "dispatch", porter["role"], "first add must survive")
	require.Equal(t, "moves boxes", porter["description"], "second add must overlay")
}

// TestAgentSetRejectsRoleAndRuntime verifies `agent set` refuses the flags
// that change what an existing agent is.
func TestAgentSetRejectsRoleAndRuntime(t *testing.T) {
	t.Parallel()

	err := loadAgentScriptErr(t, `agent set coder --role dispatch`)
	require.ErrorContains(t, err, "unknown flag --role")

	err = loadAgentScriptErr(t, `agent set coder --runtime a2a`)
	require.ErrorContains(t, err, "unknown flag --runtime")
}

// TestAgentSetRequiresKnownAgent verifies `agent set` only overlays an
// existing id: a built-in or one the script already defined.
func TestAgentSetRequiresKnownAgent(t *testing.T) {
	t.Parallel()

	agents := loadAgentScript(t, `agent add scribe --role dispatch
agent set scribe --description "writes things"
agent set worker --description "worker definition"`)
	require.Equal(t, "writes things", agents["scribe"].(map[string]any)["description"])
	require.Equal(t, "worker definition", agents["worker"].(map[string]any)["description"])

	err := loadAgentScriptErr(t, `agent set nosuchagent --name x`)
	require.ErrorContains(t, err, "unknown agent")
	require.ErrorContains(t, err, "agent add")
}

// TestAgentRemoveDeletesUserIDs verifies remove and its rm alias delete a
// user-added id, and that built-ins are refused with the disable hint.
func TestAgentRemoveDeletesUserIDs(t *testing.T) {
	t.Parallel()

	agents := loadAgentScript(t, `agent add scribe --role dispatch
agent remove scribe
agent add helper --role dispatch
agent rm helper`)
	require.NotContains(t, agents, "scribe")
	require.NotContains(t, agents, "helper")
}

// TestAgentRemoveBuiltinsGetDisableHint verifies every config built-in is
// refused by `agent remove` with the `agent set --disabled true` hint. The
// ids come from the config package so the two cannot drift.
func TestAgentRemoveBuiltinsGetDisableHint(t *testing.T) {
	t.Parallel()

	for _, id := range []string{config.AgentCoder, config.AgentPlan, config.AgentTask, config.AgentWorker} {
		err := loadAgentScriptErr(t, "agent remove "+id)
		require.ErrorContains(t, err, "built-in agent")
		require.ErrorContains(t, err, "agent set "+id+" --disabled true")
	}
}

// TestAgentModelFlag pins the --model encodings: slots as strings, pins as
// objects split on the first slash only.
func TestAgentModelFlag(t *testing.T) {
	t.Parallel()

	agents := loadAgentScript(t, `agent add a --role dispatch --model large
agent add b --role dispatch --model small
agent add c --role dispatch --model openai/gpt-5-mini
agent add d --role dispatch --model openai/gpt-5/mini`)
	require.Equal(t, "large", agents["a"].(map[string]any)["model"])
	require.Equal(t, "small", agents["b"].(map[string]any)["model"])
	require.Equal(t, map[string]any{"provider": "openai", "model": "gpt-5-mini"}, agents["c"].(map[string]any)["model"])
	require.Equal(t, map[string]any{"provider": "openai", "model": "gpt-5/mini"},
		agents["d"].(map[string]any)["model"], "split on the first slash only")
}

// TestAgentPromptAndAppendFlags pins the prompt encodings: builtin: and
// file: pass through, a bare path gains the file: prefix.
func TestAgentPromptAndAppendFlags(t *testing.T) {
	t.Parallel()

	agents := loadAgentScript(t, `agent add a --role dispatch --prompt builtin:dispatch --prompt-append notes.md
agent add b --role dispatch --prompt file:/etc/motd
agent add c --role dispatch --prompt ./prompts/c.md --prompt-append file:./prompts/tail.md`)
	require.Equal(t, "builtin:dispatch", agents["a"].(map[string]any)["prompt"])
	require.Equal(t, "file:notes.md", agents["a"].(map[string]any)["prompt_append"])
	require.Equal(t, "file:/etc/motd", agents["b"].(map[string]any)["prompt"])
	require.Equal(t, "file:./prompts/c.md", agents["c"].(map[string]any)["prompt"])
	require.Equal(t, "file:./prompts/tail.md", agents["c"].(map[string]any)["prompt_append"])
}

// TestAgentListFlags pins the variadic and repeatable list flags, and that
// a variadic list stops at the next flag instead of swallowing it.
func TestAgentListFlags(t *testing.T) {
	t.Parallel()

	agents := loadAgentScript(t, `agent add a --role dispatch --tools @read bash view --deny-tool sourcegraph --deny-tool web --mcp * --skill go --skill rust --context-path AGENTS.md --context-path NOTES.md
agent add b --role dispatch --tools @read --deny-tool bash`)
	a := agents["a"].(map[string]any)
	require.Equal(t, []any{"@read", "bash", "view"}, a["tools"].(map[string]any)["allow"])
	require.Equal(t, []any{"sourcegraph", "web"}, a["tools"].(map[string]any)["deny"])
	require.Equal(t, []any{"*"}, a["mcp"].(map[string]any)["allow"])
	require.Equal(t, []any{"go", "rust"}, a["skills"])
	require.Equal(t, []any{"AGENTS.md", "NOTES.md"}, a["context_paths"])

	b := agents["b"].(map[string]any)
	require.Equal(t, []any{"@read"}, b["tools"].(map[string]any)["allow"],
		"the list must stop at the next flag, not swallow --deny-tool")
	require.Equal(t, []any{"bash"}, b["tools"].(map[string]any)["deny"])

	err := loadAgentScriptErr(t, `agent add a --role dispatch --tools`)
	require.ErrorContains(t, err, "--tools requires at least one value")
}

// TestAgentBearerFlag pins the auth encoding and the $VAR-only rule: the
// usage shows the single quotes, and a literal token never lands on disk.
func TestAgentBearerFlag(t *testing.T) {
	t.Parallel()

	err := loadAgentScriptErr(t, `agent add a --bearer sk-live-123`)
	require.ErrorContains(t, err, "--bearer")
	require.ErrorContains(t, err, "$VAR")

	agents := loadAgentScript(t, `agent add a --role dispatch --runtime a2a --card https://x.example --bearer '$TOKEN'`)
	require.Equal(t, map[string]any{"type": "bearer", "token": "$TOKEN"}, agents["a"].(map[string]any)["auth"])
}

// TestAgentValueEncodings pins the count and duration encodings: integers
// as JSON numbers, durations as strings, off as "off", and on/off booleans.
func TestAgentValueEncodings(t *testing.T) {
	t.Parallel()

	agents := loadAgentScript(t, `agent add a --role dispatch --nudge off --nudge-after 0 --kill-after-nudges 3 --stall 500ms --timeout 7200 --idle-timeout 90s --hard-gate off`)
	a := agents["a"].(map[string]any)
	todos := a["todos"].(map[string]any)
	require.Equal(t, false, todos["nudge"], "parseBool accepts off")
	require.Equal(t, float64(0), todos["nudge_after_tool_calls"])
	require.Equal(t, false, todos["hard_gate"])
	kill := a["kill"].(map[string]any)
	require.Equal(t, float64(3), kill["after_ignored_nudges"])
	require.Equal(t, "500ms", kill["stall"])
	require.Equal(t, float64(7200), kill["timeout"], "a bare integer is seconds")
	transport := a["transport"].(map[string]any)
	require.Equal(t, "90s", transport["idle_timeout"])
}

// TestAgentErrorsNameTheirFlag covers the error cases caught in crushrc,
// before load-time validation runs.
func TestAgentErrorsNameTheirFlag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		script string
		want   string
	}{
		{"unknown flag", `agent add a --role dispatch --nope x`, "unknown flag --nope"},
		{"missing id", `agent add`, "agent add <id>"},
		{"missing flag value", `agent add a --role`, "--role requires a value"},
		{"bad duration", `agent add a --role dispatch --stall 5x`, "--stall expects a duration"},
		{"negative integer", `agent add a --role dispatch --nudge-after -1`, "--nudge-after must not be negative"},
		{"negative duration", `agent add a --role dispatch --timeout -5m`, "--timeout must not be negative"},
		{"empty provider", `agent add a --role dispatch --model /gpt-5`, "--model expects"},
		{"empty model", `agent add a --role dispatch --model openai/`, "--model expects"},
		{"no slash", `agent add a --role dispatch --model gpt-5`, "--model expects"},
		{"unknown subcommand", `agent config`, "unknown subcommand"},
		{"no subcommand", `agent`, "usage: agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := loadAgentScriptErr(t, tc.script)
			require.Error(t, err)
			require.ErrorContains(t, err, tc.want)
		})
	}
}
