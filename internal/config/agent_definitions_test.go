package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// agentsBaseConfig is the provider and MCP scaffolding every agent
// definition test loads against: two local providers so the store is
// configured and SetupAgents runs, and one HTTP MCP server so mcp.allow
// entries have something to name.
const agentsBaseConfig = `{
	"models": {"large": {"provider": "openai", "model": "gpt-4"}},
	"providers": {
		"openai": {
			"api_key": "test-key",
			"models": [{"id": "gpt-4", "name": "GPT-4"}]
		},
		"anthropic": {
			"api_key": "test-key-2",
			"models": [{"id": "claude-3", "name": "Claude 3"}]
		}
	},
	"mcp": {
		"docs": {"type": "http", "url": "http://localhost:1/mcp"}
	}
}`

// loadAgentsJSON writes a config file whose agents block is agentsJSON
// into an isolated global config directory and loads it through the
// real pipeline. It asserts success.
func loadAgentsJSON(t *testing.T, agentsJSON string) *config.ConfigStore {
	t.Helper()
	store, err := loadAgentsJSONErr(t, agentsJSON)
	require.NoError(t, err)
	return store
}

// loadAgentsJSONErr is loadAgentsJSON without asserting success, for
// configs expected to fail at load time.
func loadAgentsJSONErr(t *testing.T, agentsJSON string) (*config.ConfigStore, error) {
	t.Helper()
	workDir, dataDir := isolateReloadEnv(t)
	globalDir := os.Getenv("CRUSH_GLOBAL_CONFIG")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	configPath := filepath.Join(globalDir, "crush.json")
	body := agentsBaseConfig
	if agentsJSON != "" {
		body = agentsBaseConfig[:len(agentsBaseConfig)-1] + `,"agents":` + agentsJSON + `}`
	}
	require.NoError(t, os.WriteFile(configPath, []byte(body), 0o600))
	return config.Load(workDir, dataDir, false)
}

// TestAgentDefinitions_BuiltinsResolveLikeToday pins the no-agents-section
// behavior: coder, plan, and task resolve exactly as the old literal maps
// built them, and the worker definition is there for the dispatch runtime
// (#432, #433).
func TestAgentDefinitions_BuiltinsResolveLikeToday(t *testing.T) {
	store := loadAgentsJSON(t, "")
	cfg := store.Config()

	require.Len(t, cfg.Agents, 4)

	coder := cfg.Agents[config.AgentCoder]
	require.Equal(t, "Coder", coder.Name)
	require.Equal(t, "An agent that helps with executing coding tasks.", coder.Description)
	require.Equal(t, config.SelectedModelTypeLarge, coder.Model)
	require.Nil(t, coder.AllowedMCP, "coder sees every MCP server")
	require.Nil(t, coder.ContextPaths, "an unset definition inherits options.context_paths at render time")
	require.Contains(t, coder.AllowedTools, "bash")
	require.Contains(t, coder.AllowedTools, "view")
	require.Equal(t, config.AgentRoleMain, coder.Role)
	require.Equal(t, config.AgentRuntimeBuiltin, coder.Runtime)
	require.Equal(t, "builtin:coder", coder.Prompt)
	require.Nil(t, coder.TodoEnforcement)

	require.Equal(t, "Plan", cfg.Agents[config.AgentPlan].Name)
	require.Equal(t, config.AgentRoleMain, cfg.Agents[config.AgentPlan].Role)
	require.Equal(t, []string{
		"agent",
		"glob",
		"grep",
		"ls",
		"lsp_call_hierarchy",
		"lsp_definition",
		"lsp_symbols",
		"question",
		"sourcegraph",
		"view",
	}, cfg.Agents[config.AgentPlan].AllowedTools)
	require.Empty(t, cfg.Agents[config.AgentPlan].AllowedMCP)
	require.Len(t, cfg.Agents[config.AgentPlan].AllowedMCP, 0)

	require.Equal(t, "Task", cfg.Agents[config.AgentTask].Name)
	require.Equal(t, config.AgentRoleSubagent, cfg.Agents[config.AgentTask].Role)
	require.Equal(t, []string{
		"glob",
		"grep",
		"ls",
		"lsp_call_hierarchy",
		"lsp_definition",
		"lsp_symbols",
		"semantic_search",
		"sourcegraph",
		"view",
	}, cfg.Agents[config.AgentTask].AllowedTools)
	require.Len(t, cfg.Agents[config.AgentTask].AllowedMCP, 0)

	worker := cfg.Agents[config.AgentWorker]
	require.Equal(t, "Worker", worker.Name)
	require.Equal(t, config.SelectedModelTypeSmall, worker.Model)
	require.Equal(t, config.AgentRoleDispatch, worker.Role)
	require.Equal(t, config.AgentWorkspaceWorktree, worker.Workspace)
	require.Len(t, worker.AllowedMCP, 0)
	require.ElementsMatch(t, []string{
		"bash",
		"edit",
		"glob",
		"grep",
		"job_kill",
		"job_output",
		"ls",
		"lsp_call_hierarchy",
		"lsp_definition",
		"lsp_diagnostics",
		"lsp_symbols",
		"multiedit",
		"semantic_search",
		"sourcegraph",
		"todos",
		"view",
		"write",
	}, worker.AllowedTools)
}

// TestAgentDefinitions_CoderDenyRemovesOnlyThatTool covers the issue's
// example: a tools.deny entry on coder removes exactly that tool and
// leaves every other agent alone.
func TestAgentDefinitions_CoderDenyRemovesOnlyThatTool(t *testing.T) {
	store := loadAgentsJSON(t, `{"coder": {"tools": {"deny": ["sourcegraph"]}}}`)
	cfg := store.Config()

	coder := cfg.Agents[config.AgentCoder]
	require.NotContains(t, coder.AllowedTools, "sourcegraph")
	require.Contains(t, coder.AllowedTools, "bash")
	require.Contains(t, coder.AllowedTools, "view")

	require.Contains(t, cfg.Agents[config.AgentTask].AllowedTools, "sourcegraph",
		"the deny list applies to coder only")
}

// TestAgentDefinitions_DisabledToolsBeatAllows covers the security
// property: options.disabled_tools wins over any definition allow list,
// so a definition can narrow but never widen user policy.
func TestAgentDefinitions_DisabledToolsBeatAllows(t *testing.T) {
	workDir, dataDir := isolateReloadEnv(t)
	globalDir := os.Getenv("CRUSH_GLOBAL_CONFIG")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	configPath := filepath.Join(globalDir, "crush.json")

	body := `{
		"options": {"disabled_tools": ["bash"]},
		"models": {"large": {"provider": "openai", "model": "gpt-4"}},
		"providers": {
			"openai": {"api_key": "test-key", "models": [{"id": "gpt-4", "name": "GPT-4"}]}
		},
		"agents": {
			"coder": {"tools": {"allow": ["@write", "bash"]}},
			"worker": {"tools": {"allow": ["*"]}}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(body), 0o600))

	store, err := config.Load(workDir, dataDir, false)
	require.NoError(t, err)
	cfg := store.Config()

	coder := cfg.Agents[config.AgentCoder]
	require.NotContains(t, coder.AllowedTools, "bash",
		"disabled_tools removes bash even though the definition allows it")
	require.Contains(t, coder.AllowedTools, "edit")
	require.Contains(t, coder.AllowedTools, "write")

	worker := cfg.Agents[config.AgentWorker]
	require.NotContains(t, worker.AllowedTools, "bash",
		"allow * does not resurrect a disabled tool")
	require.Contains(t, worker.AllowedTools, "view")
}

// TestAgentDefinitions_DenyInteraction checks deny inside a definition:
// allow expands, deny removes from the expansion.
func TestAgentDefinitions_DenyInteraction(t *testing.T) {
	store := loadAgentsJSON(t, `{
		"coder": {"tools": {"allow": ["@write", "bash"], "deny": ["edit"]}}
	}`)
	cfg := store.Config()

	coder := cfg.Agents[config.AgentCoder]
	require.NotContains(t, coder.AllowedTools, "edit")
	require.Contains(t, coder.AllowedTools, "bash")
	require.Contains(t, coder.AllowedTools, "multiedit")
}

// TestAgentDefinitions_EmptyAllowMeansNoTools pins the difference
// between an omitted allow list, which inherits everything, and an
// explicit empty one, which grants nothing: a restriction typed as []
// cannot widen an agent.
func TestAgentDefinitions_EmptyAllowMeansNoTools(t *testing.T) {
	store := loadAgentsJSON(t, `{"task": {"tools": {"allow": []}}}`)
	require.Empty(t, store.Config().Agents[config.AgentTask].AllowedTools)

	store = loadAgentsJSON(t, `{
		"reviewer": {"role": "dispatch", "tools": {"deny": ["bash"]}}
	}`)
	reviewer := store.Config().Agents["reviewer"]
	require.NotContains(t, reviewer.AllowedTools, "bash", "a new agent without allow inherits every tool")
	require.Contains(t, reviewer.AllowedTools, "edit")
}

// TestAgentDefinitions_OverlayInherit checks field-by-field overlay: an
// override touches only the fields it names.
func TestAgentDefinitions_OverlayInherit(t *testing.T) {
	store := loadAgentsJSON(t, `{
		"coder": {"description": "Custom coder description"},
		"task": {"tools": {"allow": ["view"]}}
	}`)
	cfg := store.Config()

	coder := cfg.Agents[config.AgentCoder]
	require.Equal(t, "Custom coder description", coder.Description)
	require.Equal(t, "Coder", coder.Name, "name is inherited")
	require.Contains(t, coder.AllowedTools, "bash", "tools are inherited")

	task := cfg.Agents[config.AgentTask]
	require.Equal(t, []string{"view"}, task.AllowedTools, "lists replace, not append")
	require.Equal(t, "Task", task.Name)
}

// TestAgentDefinitions_DefaultsApplyTodos checks the $defaults key: its
// todos and kill blocks underlay every agent, including one the config
// overrides.
func TestAgentDefinitions_DefaultsApplyTodos(t *testing.T) {
	store := loadAgentsJSON(t, `{
		"$defaults": {"todos": {"hard_gate": true, "nudge_after_tool_calls": 7}},
		"coder": {"description": "overridden"}
	}`)
	cfg := store.Config()

	for _, id := range []string{config.AgentCoder, config.AgentPlan, config.AgentTask, config.AgentWorker} {
		agent := cfg.Agents[id]
		require.NotNil(t, agent.TodoEnforcement, "%s inherits the defaults", id)
		require.NotNil(t, agent.TodoEnforcement.HardGate)
		require.True(t, *agent.TodoEnforcement.HardGate, "%s hard gate", id)
		require.NotNil(t, agent.TodoEnforcement.NudgeThreshold)
		require.EqualValues(t, 7, *agent.TodoEnforcement.NudgeThreshold, "%s threshold", id)
	}
	require.Equal(t, "overridden", cfg.Agents[config.AgentCoder].Description,
		"$defaults does not stop per-agent overrides")
}

// TestAgentDefinitions_WorkerModelPinAndKill checks that an explicit
// model pin and kill thresholds load and resolve onto the agent.
func TestAgentDefinitions_WorkerModelPinAndKill(t *testing.T) {
	store := loadAgentsJSON(t, `{
		"worker": {
			"model": {"provider": "openai", "model": "gpt-4"},
			"kill": {"after_ignored_nudges": 3, "stall": "5m", "timeout": "30m"}
		}
	}`)
	cfg := store.Config()

	worker := cfg.Agents[config.AgentWorker]
	require.NotNil(t, worker.ModelRef)
	require.Equal(t, "openai", worker.ModelRef.Provider)
	require.Equal(t, "gpt-4", worker.ModelRef.Model)

	require.NotNil(t, worker.TodoEnforcement)
	require.EqualValues(t, 3, *worker.TodoEnforcement.KillAfterNudges)
	require.Equal(t, 5*time.Minute, time.Duration(*worker.TodoEnforcement.StallWindow))
	require.Equal(t, 30*time.Minute, time.Duration(*worker.TodoEnforcement.HardTimeout))
}

// TestAgentDefinitions_A2AAgentLoads checks the a2a reviewer shape: a new
// dispatch agent with an external card, bearer auth, no workspace, and a
// transport idle timeout loads and carries its fields.
func TestAgentDefinitions_A2AAgentLoads(t *testing.T) {
	store := loadAgentsJSON(t, `{
		"reviewer": {
			"role": "dispatch",
			"runtime": "a2a",
			"name": "Reviewer",
			"card": "https://example.com/agent.json",
			"workspace": "none",
			"auth": {"type": "bearer", "token": "$A2A_TOKEN"},
			"transport": {"idle_timeout": "10m"}
		}
	}`)
	cfg := store.Config()

	reviewer := cfg.Agents["reviewer"]
	require.Equal(t, config.AgentRoleDispatch, reviewer.Role)
	require.Equal(t, config.AgentRuntimeA2A, reviewer.Runtime)
	require.Equal(t, "https://example.com/agent.json", reviewer.Card)
	require.Equal(t, config.AgentWorkspaceNone, reviewer.Workspace)
	require.NotNil(t, reviewer.Auth)
	require.Equal(t, "bearer", *reviewer.Auth.Type)
	require.NotNil(t, reviewer.Transport)
	require.Equal(t, 10*time.Minute, time.Duration(*reviewer.Transport.IdleTimeout))

	require.Len(t, cfg.Agents, 5, "the a2a entry adds to the built-ins")
}

// TestAgentDefinitions_A2AAgentDefaults checks what a minimal runtime a2a
// definition resolves to (#434): no workspace set still means none, an
// http card is accepted on a loopback host, and no auth means no
// credential at all.
func TestAgentDefinitions_A2AAgentDefaults(t *testing.T) {
	store := loadAgentsJSON(t, `{
		"local": {"role": "dispatch", "runtime": "a2a", "card": "http://127.0.0.1:8080/.well-known/agent-card.json"},
		"named": {"role": "dispatch", "runtime": "a2a", "card": "http://localhost:9000"}
	}`)
	cfg := store.Config()

	local := cfg.Agents["local"]
	require.Equal(t, config.AgentRuntimeA2A, local.Runtime)
	require.Equal(t, config.AgentWorkspaceNone, local.Workspace, "an a2a agent never runs in a worktree")
	require.Nil(t, local.Auth)
	require.Equal(t, config.AgentWorkspaceNone, cfg.Agents["named"].Workspace)
	require.Equal(t, config.AgentWorkspaceWorktree, cfg.Agents[config.AgentWorker].Workspace, "the builtin worker keeps its worktree")
}

// TestAgentDefinitions_UnusableExternalAgentLoads checks that one bad
// external definition fails closed per agent rather than failing the
// whole load (#434): the config loads, the other agents are untouched,
// and the bad agent carries the reason dispatch refuses it with.
func TestAgentDefinitions_UnusableExternalAgentLoads(t *testing.T) {
	cases := []struct {
		name string
		def  string
		want string
	}{
		{
			name: "no card",
			def:  `{"role": "dispatch", "runtime": "a2a"}`,
			want: `agents.reviewer.card: a runtime a2a agent needs the URL of its Agent Card`,
		},
		{
			name: "http card off loopback",
			def:  `{"role": "dispatch", "runtime": "a2a", "card": "http://example.com/agent.json"}`,
			want: `agents.reviewer.card: must use https; plain http is allowed only for loopback hosts`,
		},
		{
			name: "file card",
			def:  `{"role": "dispatch", "runtime": "a2a", "card": "file:///etc/agent.json"}`,
			want: `agents.reviewer.card: must be an absolute URL with a host`,
		},
		{
			name: "card carrying credentials",
			def:  `{"role": "dispatch", "runtime": "a2a", "card": "https://user:secret@example.com/agent.json"}`,
			want: `agents.reviewer.card: must not carry credentials; set them in auth`,
		},
		{
			name: "auth with no token",
			def:  `{"role": "dispatch", "runtime": "a2a", "card": "https://example.com/agent.json", "auth": {"type": "bearer"}}`,
			want: `agents.reviewer.auth.token: a bearer token is required when auth is set`,
		},
		{
			name: "negative idle timeout",
			def:  `{"role": "dispatch", "runtime": "a2a", "card": "https://example.com/agent.json", "transport": {"idle_timeout": "-1m"}}`,
			want: `agents.reviewer.transport.idle_timeout: must not be negative (got -60)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := loadAgentsJSON(t, `{"reviewer": `+tc.def+`}`)
			cfg := store.Config()
			reviewer, ok := cfg.Agents["reviewer"]
			require.True(t, ok, "the bad external agent still resolves")
			require.Equal(t, tc.want, reviewer.Unusable)
			require.NotContains(t, reviewer.Unusable, "secret", "the reason never echoes the card URL")
			require.Empty(t, cfg.Agents[config.AgentWorker].Unusable, "the other agents are untouched")

			// The load ran before any logger existed, so the problem
			// survives as a diagnostic the command can show (#560).
			require.Equal(t, []config.LoadDiagnostic{{
				Severity: config.DiagnosticWarning,
				Agent:    "reviewer",
				Message:  "cannot be dispatched: " + tc.want,
			}}, store.LoadDiagnostics())
		})
	}

	good := loadAgentsJSON(t, `{"reviewer": {"role": "dispatch", "runtime": "a2a", "card": "https://example.com/agent.json", "auth": {"token": "$T"}}}`)
	require.Empty(t, good.Config().Agents["reviewer"].Unusable)
	require.Empty(t, good.LoadDiagnostics(), "a usable external agent loads silently")
}

// TestAgentDefinitions_LoadDiagnostics checks the diagnostics Load keeps
// on the store (#560): a field a builtin-runtime agent does not honor,
// kill knobs dropped from a non-dispatch agent, and an unusable external
// agent each name their agent and problem; they come out in a stable
// order; a clean config has none; and a reload replaces the set.
func TestAgentDefinitions_LoadDiagnostics(t *testing.T) {
	store := loadAgentsJSON(t, `{
		"worker": {"workspace": "none"},
		"coder": {"todo_enforcement": {"kill_after_nudges": 2}},
		"reviewer": {"role": "dispatch", "runtime": "a2a"}
	}`)
	require.Equal(t, []config.LoadDiagnostic{
		{
			Severity: config.DiagnosticWarning,
			Agent:    config.AgentCoder,
			Message:  "agents.coder.todo_enforcement: kill thresholds apply to dispatch agents only and are ignored",
		},
		{
			Severity: config.DiagnosticWarning,
			Agent:    "reviewer",
			Message:  "cannot be dispatched: agents.reviewer.card: a runtime a2a agent needs the URL of its Agent Card",
		},
		{
			Severity: config.DiagnosticWarning,
			Agent:    config.AgentWorker,
			Message:  "agents.worker.workspace: parsed but not honored by a builtin-runtime agent",
		},
	}, store.LoadDiagnostics())

	// The same file loaded again raises the same diagnostics: the log
	// line is deduped per process, the diagnostics are not.
	again := loadAgentsJSON(t, `{"reviewer": {"role": "dispatch", "runtime": "a2a"}}`)
	require.Len(t, again.LoadDiagnostics(), 1)
	require.Equal(t, "reviewer", again.LoadDiagnostics()[0].Agent)

	// Fixing the definition on disk and reloading clears it; breaking
	// it again brings it back.
	configPath := filepath.Join(os.Getenv("CRUSH_GLOBAL_CONFIG"), "crush.json")
	write := func(agentsJSON string) {
		t.Helper()
		body := agentsBaseConfig[:len(agentsBaseConfig)-1] + `,"agents":` + agentsJSON + `}`
		require.NoError(t, os.WriteFile(configPath, []byte(body), 0o600))
	}
	write(`{"reviewer": {"role": "dispatch", "runtime": "a2a", "card": "https://example.com/agent.json"}}`)
	require.NoError(t, again.ReloadFromDisk(context.Background()))
	require.Empty(t, again.LoadDiagnostics(), "a reload that fixes the agent clears its diagnostic")

	write(`{"reviewer": {"role": "dispatch", "runtime": "a2a", "card": "http://example.com/agent.json"}}`)
	require.NoError(t, again.ReloadFromDisk(context.Background()))
	diags := again.LoadDiagnostics()
	require.Len(t, diags, 1)
	require.Equal(t, "cannot be dispatched: agents.reviewer.card: must use https; plain http is allowed only for loopback hosts", diags[0].Message)

	require.Empty(t, loadAgentsJSON(t, "").LoadDiagnostics(), "an untouched config starts up silent")
}

// TestValidateAgentCardURL pins the card URL rule the loader and the
// dispatch-time resolver share (#434).
func TestValidateAgentCardURL(t *testing.T) {
	t.Parallel()
	ok := []string{
		"https://reviewer.example.net/.well-known/agent-card.json",
		"https://reviewer.example.net",
		"http://localhost:8080/card.json",
		"http://127.0.0.1/card.json",
		"http://[::1]:9000/card.json",
	}
	for _, raw := range ok {
		_, err := config.ValidateAgentCardURL(raw)
		require.NoError(t, err, raw)
	}
	bad := map[string]string{
		"http://reviewer.example.net/card.json":   "plain http is allowed only for loopback hosts",
		"http://10.0.0.1/card.json":               "plain http is allowed only for loopback hosts",
		"ftp://reviewer.example.net/card.json":    `must use https, not "ftp"`,
		"file:///tmp/card.json":                   "must be an absolute URL with a host",
		"/relative/card.json":                     "must be an absolute URL with a host",
		"https://token@reviewer.example.net/":     "must not carry credentials",
		"https://u:p@reviewer.example.net/":       "must not carry credentials",
		"https://reviewer.example.net:bad/x.json": "not a valid URL",
	}
	for raw, want := range bad {
		_, err := config.ValidateAgentCardURL(raw)
		require.ErrorContains(t, err, want, raw)
		require.NotContains(t, err.Error(), "@", "the error must not echo userinfo")
	}
}

// TestAgentDefinitions_MCPAllow checks the mcp.allow spellings: a
// server:tool entry resolves to one tool of one server, other agents
// keep their defaults.
func TestAgentDefinitions_MCPAllow(t *testing.T) {
	store := loadAgentsJSON(t, `{"coder": {"mcp": {"allow": ["docs:search"]}}}`)
	cfg := store.Config()

	require.Equal(t, map[string][]string{"docs": {"search"}}, cfg.Agents[config.AgentCoder].AllowedMCP)
	require.Len(t, cfg.Agents[config.AgentTask].AllowedMCP, 0, "task keeps its no-MCP default")
}

// TestAgentDefinitions_ValidationErrors walks one bad definition per
// structural rule; each must fail Load naming the offending path.
func TestAgentDefinitions_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		agents  string
		wantErr string
	}{
		{
			name:    "unknown tool",
			agents:  `{"coder": {"tools": {"allow": ["bogus"]}}}`,
			wantErr: `agents.coder.tools.allow[0]: unknown tool "bogus"`,
		},
		{
			name:    "unknown tool group",
			agents:  `{"coder": {"tools": {"deny": ["@nope"]}}}`,
			wantErr: `agents.coder.tools.deny[0]: unknown tool "@nope"`,
		},
		{
			name:    "unknown MCP server",
			agents:  `{"coder": {"mcp": {"allow": ["nosuch"]}}}`,
			wantErr: `agents.coder.mcp.allow[0]: unknown MCP server "nosuch"`,
		},
		{
			name:    "unknown role",
			agents:  `{"helper": {"role": "boss"}}`,
			wantErr: `agents.helper.role: unknown role "boss"`,
		},
		{
			name:    "role change on a built-in",
			agents:  `{"task": {"role": "main"}}`,
			wantErr: `agents.task.role: cannot change the role of built-in agent "task" from "subagent" to "main"`,
		},
		{
			name:    "new id must be dispatch",
			agents:  `{"helper": {"role": "subagent"}}`,
			wantErr: `agents.helper.role: a new agent must be a "dispatch" agent`,
		},
		{
			name:    "bad id",
			agents:  `{"Bad_ID": {"role": "dispatch"}}`,
			wantErr: `agents.Bad_ID: id must match`,
		},
		{
			name:    "coder cannot be disabled",
			agents:  `{"coder": {"disabled": true}}`,
			wantErr: `agents.coder.disabled: the coder agent cannot be disabled`,
		},
		{
			name:    "kill on a non-dispatch agent",
			agents:  `{"coder": {"kill": {"timeout": "5m"}}}`,
			wantErr: `agents.coder.kill: kill thresholds apply to "dispatch" agents only`,
		},
		{
			name:    "negative nudge threshold",
			agents:  `{"coder": {"todos": {"nudge_after_tool_calls": -1}}}`,
			wantErr: `agents.coder.todos.nudge_after_tool_calls: must not be negative (got -1)`,
		},
		{
			name:    "negative kill nudges",
			agents:  `{"worker": {"kill": {"after_ignored_nudges": -2}}}`,
			wantErr: `agents.worker.kill.after_ignored_nudges: must not be negative (got -2)`,
		},
		{
			name:    "negative kill stall",
			agents:  `{"worker": {"kill": {"stall": "-5m"}}}`,
			wantErr: `agents.worker.kill.stall: must not be negative (got -300)`,
		},
		{
			name:    "negative kill timeout",
			agents:  `{"worker": {"kill": {"timeout": "-1m"}}}`,
			wantErr: `agents.worker.kill.timeout: must not be negative (got -60)`,
		},
		{
			name:    "a2a agent may not set model",
			agents:  `{"worker": {"runtime": "a2a", "card": "https://example.com/agent.json", "model": "small"}}`,
			wantErr: `agents.worker.model: a runtime a2a agent is defined by its card and may not set this field`,
		},
		{
			name:    "a2a requires dispatch",
			agents:  `{"task": {"runtime": "a2a", "card": "https://example.com/agent.json"}}`,
			wantErr: `agents.task.runtime: a2a agents must be "dispatch" agents`,
		},
		{
			name:    "a2a agent may not run in a worktree",
			agents:  `{"reviewer": {"role": "dispatch", "runtime": "a2a", "card": "https://example.com/agent.json", "workspace": "worktree"}}`,
			wantErr: `agents.reviewer.workspace: a runtime a2a agent runs without a worktree; use "none"`,
		},
		{
			name:    "a2a auth type",
			agents:  `{"reviewer": {"role": "dispatch", "runtime": "a2a", "card": "https://example.com/agent.json", "auth": {"type": "basic"}}}`,
			wantErr: `agents.reviewer.auth.type: unknown auth type "basic"`,
		},
		{
			name:    "a2a kill limited to timeout",
			agents:  `{"worker": {"runtime": "a2a", "card": "https://example.com/agent.json", "kill": {"stall": "5m"}}}`,
			wantErr: `agents.worker.kill.stall: a runtime a2a agent may only set kill.timeout`,
		},
		{
			name:    "missing prompt file",
			agents:  `{"coder": {"prompt": "file:/nonexistent/nope.md"}}`,
			wantErr: `agents.coder: prompt file "/nonexistent/nope.md" does not exist`,
		},
		{
			name:    "prompt_append must be a file",
			agents:  `{"coder": {"prompt_append": "builtin:coder"}}`,
			wantErr: `agents.coder: prompt_append must be a file:<path> reference, not a builtin prompt`,
		},
		{
			name:    "defaults carry todos and kill only",
			agents:  `{"$defaults": {"name": "x"}}`,
			wantErr: `agents.$defaults: only todos and kill may be set`,
		},
		{
			name:    "unknown workspace",
			agents:  `{"worker": {"workspace": "bogus"}}`,
			wantErr: `agents.worker.workspace: unknown workspace "bogus"`,
		},
		{
			name:    "unknown model slot",
			agents:  `{"coder": {"model": "huge"}}`,
			wantErr: `unknown model slot "huge"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAgentsJSONErr(t, tc.agents)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestAgentDefinitions_ModelRefValidation checks the pin checks that
// need the provider catalog: an unknown provider and a known provider
// without the named model both fail the load.
func TestAgentDefinitions_ModelRefValidation(t *testing.T) {
	t.Run("unknown provider", func(t *testing.T) {
		_, err := loadAgentsJSONErr(t, `{"worker": {"model": {"provider": "nosuch", "model": "x"}}}`)
		require.Error(t, err)
		require.Contains(t, err.Error(), `agents.worker.model: unknown provider "nosuch"`)
	})

	t.Run("unknown model", func(t *testing.T) {
		_, err := loadAgentsJSONErr(t, `{"worker": {"model": {"provider": "openai", "model": "nope"}}}`)
		require.Error(t, err)
		require.Contains(t, err.Error(), `agents.worker.model: provider "openai" has no model "nope"`)
	})
}

// TestAgentDefinitions_ReloadPicksUpAndRollsBack checks the reload path:
// a valid agents edit is applied, and a bad one fails the reload while
// the previous config stays live.
func TestAgentDefinitions_ReloadPicksUpAndRollsBack(t *testing.T) {
	workDir, dataDir := isolateReloadEnv(t)
	globalDir := os.Getenv("CRUSH_GLOBAL_CONFIG")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	configPath := filepath.Join(globalDir, "crush.json")

	valid := agentsBaseConfig[:len(agentsBaseConfig)-1] +
		`,"agents":{"coder":{"tools":{"deny":["sourcegraph"]}}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(valid), 0o600))

	store, err := config.Load(workDir, dataDir, false)
	require.NoError(t, err)
	require.NotContains(t, store.Config().Agents[config.AgentCoder].AllowedTools, "sourcegraph")

	// A valid update lands on reload.
	updated := agentsBaseConfig[:len(agentsBaseConfig)-1] +
		`,"agents":{"coder":{"tools":{"deny":["view"]}}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(updated), 0o600))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	require.Contains(t, store.Config().Agents[config.AgentCoder].AllowedTools, "sourcegraph",
		"the new deny list replaces the old")
	require.NotContains(t, store.Config().Agents[config.AgentCoder].AllowedTools, "view")

	// A bad agents block fails the reload and rolls back.
	broken := agentsBaseConfig[:len(agentsBaseConfig)-1] +
		`,"agents":{"coder":{"tools":{"allow":["bogus"]}}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(broken), 0o600))
	err = store.ReloadFromDisk(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), `agents.coder.tools.allow[0]: unknown tool "bogus"`)
	require.NotContains(t, store.Config().Agents[config.AgentCoder].AllowedTools, "bogus",
		"the live config must not pick up the invalid definition")
	require.Contains(t, store.Config().Agents[config.AgentCoder].AllowedTools, "edit",
		"the live config keeps the last valid resolution")
}

// loadTodoJSON is loadAgentsJSON with an options block: the todo
// enforcement tests need both the global options.todo_enforcement
// defaults and a per-agent agents block in the same file, loaded
// through the real pipeline. Either argument may be empty.
func loadTodoJSON(t *testing.T, optionsJSON, agentsJSON string) *config.ConfigStore {
	t.Helper()
	workDir, dataDir := isolateReloadEnv(t)
	globalDir := os.Getenv("CRUSH_GLOBAL_CONFIG")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	configPath := filepath.Join(globalDir, "crush.json")
	body := agentsBaseConfig
	if optionsJSON != "" || agentsJSON != "" {
		body = agentsBaseConfig[:len(agentsBaseConfig)-1]
		if optionsJSON != "" {
			body += `,"options":` + optionsJSON
		}
		if agentsJSON != "" {
			body += `,"agents":` + agentsJSON
		}
		body += `}`
	}
	require.NoError(t, os.WriteFile(configPath, []byte(body), 0o600))
	store, err := config.Load(workDir, dataDir, false)
	require.NoError(t, err)
	return store
}

// resolvedNudge reads the nudge threshold an agent resolves to.
func resolvedNudge(cfg *config.Config, id string) int {
	return cfg.Agents[id].ResolvedTodoEnforcement(cfg.Options.TodoEnforcement).NudgeThreshold
}

// resolvedKill reads the kill threshold an agent resolves to.
func resolvedKill(cfg *config.Config, id string) int {
	return cfg.Agents[id].ResolvedTodoEnforcement(cfg.Options.TodoEnforcement).KillAfterNudges
}

// TestAgentDefinitions_TodoEnforcementPrecedence pins the per-agent
// precedence (#402): a todos block on one agent overrides the global
// options.todo_enforcement for that agent only, and every other agent
// resolves from the global value with the built-in kill default.
func TestAgentDefinitions_TodoEnforcementPrecedence(t *testing.T) {
	store := loadTodoJSON(t,
		`{"todo_enforcement": {"nudge_threshold": 5}}`,
		`{"coder": {"todos": {"nudge_after_tool_calls": 8}}}`)
	cfg := store.Config()

	require.Equal(t, 8, resolvedNudge(cfg, config.AgentCoder),
		"the per-agent todos block replaces the global threshold")
	for _, id := range []string{config.AgentPlan, config.AgentTask, config.AgentWorker} {
		require.Equal(t, 5, resolvedNudge(cfg, id), "%s resolves the global threshold", id)
	}
	for _, id := range []string{config.AgentCoder, config.AgentPlan, config.AgentTask, config.AgentWorker} {
		require.Equal(t, 2, resolvedKill(cfg, id),
			"%s keeps the default kill threshold with nothing set", id)
	}
}

// TestAgentDefinitions_TodoEnforcementDefaultsLayer checks the $defaults
// rung of the precedence ladder: $defaults todos and kill underlay every
// agent and sit above the global options, a per-agent block still wins
// for the agent that sets it.
func TestAgentDefinitions_TodoEnforcementDefaultsLayer(t *testing.T) {
	store := loadTodoJSON(t,
		`{"todo_enforcement": {"nudge_threshold": 5}}`,
		`{
			"$defaults": {"todos": {"nudge_after_tool_calls": 6}},
			"coder": {"todos": {"nudge_after_tool_calls": 8}}
		}`)
	cfg := store.Config()

	require.Equal(t, 8, resolvedNudge(cfg, config.AgentCoder), "per-agent beats $defaults")
	for _, id := range []string{config.AgentPlan, config.AgentTask, config.AgentWorker} {
		require.Equal(t, 6, resolvedNudge(cfg, id), "%s resolves the $defaults threshold", id)
	}
}

// TestAgentDefinitions_WorkerKillBothSpellings checks that the kill rung
// resolves identically whether it is set through the new-style kill
// block or the legacy todo_enforcement alias (#402), and that the kill
// default survives on agents with nothing set.
func TestAgentDefinitions_WorkerKillBothSpellings(t *testing.T) {
	store := loadTodoJSON(t, "",
		`{"worker": {"kill": {"after_ignored_nudges": 3}}}`)
	cfg := store.Config()
	require.Equal(t, 3, resolvedKill(cfg, config.AgentWorker),
		"the kill block configures the worker's kill threshold")
	require.Equal(t, 2, resolvedKill(cfg, config.AgentCoder),
		"other agents keep the default kill threshold")

	store = loadTodoJSON(t, "",
		`{"worker": {"todo_enforcement": {"kill_after_nudges": 3}}}`)
	cfg = store.Config()
	require.Equal(t, 3, resolvedKill(cfg, config.AgentWorker),
		"the legacy alias configures the same kill threshold")
	require.Equal(t, 2, resolvedKill(cfg, config.AgentCoder))
}

// TestAgentDefinitions_TodoEnforcementAliasConflicts walks the alias
// rules that are load errors: combining the legacy todo_enforcement
// alias with either the todos or kill block, setting the alias on
// $defaults, a negative knob through the alias, and the alias on a
// runtime a2a agent.
func TestAgentDefinitions_TodoEnforcementAliasConflicts(t *testing.T) {
	cases := []struct {
		name    string
		agents  string
		wantErr string
	}{
		{
			name:    "alias with todos",
			agents:  `{"coder": {"todo_enforcement": {"nudge_threshold": 9}, "todos": {"nudge_after_tool_calls": 1}}}`,
			wantErr: `agents.coder.todo_enforcement: set either todo_enforcement or agents.coder.todos, not both (todo_enforcement is the legacy alias)`,
		},
		{
			name:    "alias with kill",
			agents:  `{"worker": {"todo_enforcement": {"nudge_threshold": 9}, "kill": {"after_ignored_nudges": 2}}}`,
			wantErr: `agents.worker.todo_enforcement: set either todo_enforcement or agents.worker.kill, not both (todo_enforcement is the legacy alias)`,
		},
		{
			name:    "alias on $defaults",
			agents:  `{"$defaults": {"todo_enforcement": {"nudge_threshold": 9}}}`,
			wantErr: `agents.$defaults: only todos and kill may be set`,
		},
		{
			name:    "negative knob through the alias",
			agents:  `{"worker": {"todo_enforcement": {"kill_after_nudges": -1}}}`,
			wantErr: `agents.worker.todo_enforcement.kill_after_nudges: must not be negative (got -1)`,
		},
		{
			name:    "alias on a runtime a2a agent",
			agents:  `{"reviewer": {"role": "dispatch", "runtime": "a2a", "card": "https://example.com/agent.json", "todo_enforcement": {"nudge_threshold": 9}}}`,
			wantErr: `agents.reviewer.todo_enforcement: a runtime a2a agent is defined by its card and may not set this field`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAgentsJSONErr(t, tc.agents)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestAgentDefinitions_TodoEnforcementAliasOnNonDispatch checks the
// asymmetric rule the issue pins: the alias is accepted on a
// non-dispatch agent (unlike the new-style kill block, which is a load
// error), its nudge knobs are honored, and its kill knobs are dropped
// with a warning rather than failing the load.
func TestAgentDefinitions_TodoEnforcementAliasOnNonDispatch(t *testing.T) {
	store := loadTodoJSON(t, "",
		`{"coder": {"todo_enforcement": {"nudge_threshold": 9, "kill_after_nudges": 3}}}`)
	cfg := store.Config()

	coder := cfg.Agents[config.AgentCoder]
	require.NotNil(t, coder.TodoEnforcement, "the alias feeds the agent's override")
	require.Equal(t, 9, resolvedNudge(cfg, config.AgentCoder),
		"the alias's nudge knob is honored on a non-dispatch agent")
	require.Nil(t, coder.TodoEnforcement.KillAfterNudges,
		"the alias's kill knob is dropped on a non-dispatch agent")
	require.Equal(t, 2, resolvedKill(cfg, config.AgentCoder),
		"the kill rung stays at the default on a non-dispatch agent")
}

// TestAgentDefinitions_RelativePromptFileResolvesAgainstWorkingDir pins
// that load-time validation finds a relative file: prompt where the
// runtime reads it (#432): under the working directory, not the
// process's current directory.
func TestAgentDefinitions_RelativePromptFileResolvesAgainstWorkingDir(t *testing.T) {
	workDir, dataDir := isolateReloadEnv(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "prompts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "prompts", "worker.md"), []byte("extra"), 0o644))

	globalDir := os.Getenv("CRUSH_GLOBAL_CONFIG")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	body := agentsBaseConfig[:len(agentsBaseConfig)-1] +
		`,"agents":{"worker":{"prompt_append":"file:prompts/worker.md"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "crush.json"), []byte(body), 0o600))

	store, err := config.Load(workDir, dataDir, false)
	require.NoError(t, err)
	require.Equal(t, "file:prompts/worker.md", store.Config().Agents[config.AgentWorker].PromptAppend)
}

// The #433 load-time sanity checks and default-agent carve-outs, proven
// through the real load pipeline: an enabled agent whose definition
// resolves to no tools fails the load, while a disabled worker — the
// opt-out — and a read-only worker both load.
func TestAgentDefinitions_LoadTimeSanityChecks(t *testing.T) {
	t.Run("a tool-less coder fails at load", func(t *testing.T) {
		_, err := loadAgentsJSONErr(t, `{"coder": {"tools": {"allow": []}}}`)
		require.ErrorContains(t, err, "invalid agent definitions:")
		require.ErrorContains(t, err, "agents.coder: enabled agent resolves to no tools")
	})

	t.Run("a tool-less dispatch agent fails at load", func(t *testing.T) {
		_, err := loadAgentsJSONErr(t, `{"reviewer": {"role": "dispatch", "tools": {"allow": []}}}`)
		require.ErrorContains(t, err, "agents.reviewer: enabled agent resolves to no tools")
	})

	t.Run("a disabled worker and an @read worker load", func(t *testing.T) {
		store := loadAgentsJSON(t, `{"worker": {"disabled": true}}`)
		require.True(t, store.Config().Agents[config.AgentWorker].Disabled)

		store = loadAgentsJSON(t, `{"worker": {"tools": {"allow": ["@read"]}}}`)
		require.False(t, store.Config().Agents[config.AgentWorker].Disabled)
	})

	t.Run("an explicitly disabled default_agent fails at load", func(t *testing.T) {
		workDir, dataDir := isolateReloadEnv(t)
		globalDir := os.Getenv("CRUSH_GLOBAL_CONFIG")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		body := agentsBaseConfig[:len(agentsBaseConfig)-1] +
			`,"agents":{"reviewer":{"role":"dispatch","disabled":true}}` +
			`,"options":{"dispatch":{"default_agent":"reviewer"}}}`
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "crush.json"), []byte(body), 0o600))

		_, err := config.Load(workDir, dataDir, false)
		require.ErrorContains(t, err, "invalid dispatch configuration:")
		require.ErrorContains(t, err, `options.dispatch.default_agent: agent "reviewer" is disabled`)
	})

	t.Run("an unusable external default_agent fails at load", func(t *testing.T) {
		// On its own the cardless reviewer loads with a warning; as the
		// default every dispatch would refuse, it is an error that names
		// the agent and the problem (#560).
		workDir, dataDir := isolateReloadEnv(t)
		globalDir := os.Getenv("CRUSH_GLOBAL_CONFIG")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		body := agentsBaseConfig[:len(agentsBaseConfig)-1] +
			`,"agents":{"reviewer":{"role":"dispatch","runtime":"a2a"}}` +
			`,"options":{"dispatch":{"default_agent":"reviewer"}}}`
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "crush.json"), []byte(body), 0o600))

		_, err := config.Load(workDir, dataDir, false)
		require.ErrorContains(t, err, "invalid dispatch configuration:")
		require.ErrorContains(t, err, `options.dispatch.default_agent: agent "reviewer" cannot be dispatched: agents.reviewer.card: a runtime a2a agent needs the URL of its Agent Card`)
	})
}
