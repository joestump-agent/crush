package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
)

// Agent definitions at runtime (#432): the coordinator builds every agent
// from its resolved definition. These tests load real crush.json configs
// and check each field the issue's acceptance criteria name.

const (
	defLargeProvider = "def-large"
	defSmallProvider = "def-small"
	defPinProvider   = "def-pin"
)

// writeDefinitionConfig writes a hermetic crush.json into dir: three
// offline openai-typed providers (large, small, and one only a pin
// uses), the large and small slots selected, extra options, and the
// given agents block. Each provider has its own name so a test can tell
// from ModelCfg.Provider which model an agent runs on.
func writeDefinitionConfig(t *testing.T, dir, agentsJSON, optionsJSON string) {
	t.Helper()
	provider := func(id string) string {
		return `"` + id + `": {"id": "` + id + `", "name": "` + id + `", "type": "openai",
      "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
      "models": [{"id": "` + id + `-model", "name": "` + id + `", "context_window": 8192, "default_max_tokens": 128}]}`
	}
	body := `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true` + optionsJSON + `},
  "providers": {` + provider(defLargeProvider) + `, ` + provider(defSmallProvider) + `, ` + provider(defPinProvider) + `},
  "models": {"large": {"provider": "` + defLargeProvider + `", "model": "` + defLargeProvider + `-model"},
             "small": {"provider": "` + defSmallProvider + `", "model": "` + defSmallProvider + `-model"}}`
	if agentsJSON != "" {
		body += `,
  "agents": ` + agentsJSON
	}
	body += "\n}"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "crush.json"), []byte(body), 0o644))
}

// newDefinitionCoordinator loads the config writeDefinitionConfig wrote
// into env's working directory and returns an interactive coordinator
// with no agents built yet.
func newDefinitionCoordinator(t *testing.T, env fakeEnv, agentsJSON, optionsJSON string) *coordinator {
	t.Helper()
	writeDefinitionConfig(t, env.workingDir, agentsJSON, optionsJSON)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()
	return &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		agents:      make(map[string]SessionAgent),
		interactive: true,
	}
}

// buildDefinedAgent builds the agent with the given id from its
// resolved definition and waits for its prompt and tools.
func buildDefinedAgent(t *testing.T, c *coordinator, id string) SessionAgent {
	t.Helper()
	def, ok := c.cfg.Config().Agents[id]
	require.True(t, ok, "agent %q not resolved", id)
	built, err := c.buildAgent(t.Context(), def, def.Role == config.AgentRoleSubagent)
	require.NoError(t, err)
	require.NoError(t, built.WaitReady())
	return built
}

// pinJSON is a definition model pinned to the pin-only provider.
const pinJSON = `{"provider": "` + defPinProvider + `", "model": "` + defPinProvider + `-model"}`

func TestAgentDefinitionModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		agents string
		want   map[string]string
	}{
		{
			name: "no agents section keeps every agent on the large slot",
			want: map[string]string{
				config.AgentCoder: defLargeProvider,
				config.AgentPlan:  defLargeProvider,
				config.AgentTask:  defLargeProvider,
			},
		},
		{
			name:   "plan on the small slot",
			agents: `{"plan": {"model": "small"}}`,
			want: map[string]string{
				config.AgentCoder: defLargeProvider,
				config.AgentPlan:  defSmallProvider,
			},
		},
		{
			name:   "coder pinned to a provider and model",
			agents: `{"coder": {"model": ` + pinJSON + `}}`,
			want: map[string]string{
				config.AgentCoder: defPinProvider,
				config.AgentPlan:  defLargeProvider,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newDefinitionCoordinator(t, testEnv(t), tt.agents, "")
			for id, provider := range tt.want {
				got := buildDefinedAgent(t, c, id).Model().ModelCfg.Provider
				require.Equal(t, provider, got, "agent %q", id)
			}
		})
	}
}

// UpdateModels rebuilds the active agent's models whenever the model
// config changes, and once at startup after MCP settles. It must keep
// the agent's own slot or pin rather than reset it to the large slot.
func TestUpdateModelsKeepsTheAgentsModel(t *testing.T) {
	t.Parallel()

	c := newDefinitionCoordinator(t, testEnv(t),
		`{"coder": {"model": `+pinJSON+`}, "plan": {"model": "small"}}`, "")
	for id, provider := range map[string]string{
		config.AgentCoder: defPinProvider,
		config.AgentPlan:  defSmallProvider,
	} {
		built := buildDefinedAgent(t, c, id)
		require.NoError(t, c.updateAgentModels(t.Context(), built, id))
		require.Equal(t, provider, built.Model().ModelCfg.Provider, "agent %q", id)
	}
}

func TestAgentDefinitionPromptAndContextPaths(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	const (
		globalMarker = "global-agents-md-marker-432"
		agentMarker  = "agent-notes-marker-432"
		appendMarker = "prompt-append-marker-432"
	)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "AGENTS.md"), []byte(globalMarker), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "NOTES.md"), []byte(agentMarker), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "append.md"), []byte(appendMarker), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "plan.md.tpl"),
		[]byte("Custom plan prompt for {{.WorkingDir}}."), 0o644))

	// No definition overrides: the coder renders the global context
	// paths and no appended file.
	plain := newDefinitionCoordinator(t, env, "", "")
	baseline := buildDefinedAgent(t, plain, config.AgentCoder).(*sessionAgent).systemPrompt.Get()
	require.Contains(t, baseline, globalMarker)
	require.NotContains(t, baseline, agentMarker)
	require.NotContains(t, baseline, appendMarker)

	c := newDefinitionCoordinator(t, env, `{
	  "coder": {"context_paths": ["NOTES.md"], "prompt_append": "file:append.md"},
	  "plan": {"prompt": "file:plan.md.tpl"}
	}`, "")

	// context_paths replaces the global paths for this agent, and
	// prompt_append lands verbatim after the rendered prompt.
	coder := buildDefinedAgent(t, c, config.AgentCoder).(*sessionAgent).systemPrompt.Get()
	require.Contains(t, coder, agentMarker)
	require.NotContains(t, coder, globalMarker)
	require.True(t, strings.HasSuffix(coder, "\n\n"+appendMarker), "prompt_append must come last")

	// A file: prompt renders as a template against the working dir.
	plan := buildDefinedAgent(t, c, config.AgentPlan).(*sessionAgent).systemPrompt.Get()
	// The prompt data slash-normalizes WorkingDir (prompt.go), so the
	// expected string must too or the test fails on Windows.
	require.Equal(t, "Custom plan prompt for "+filepath.ToSlash(env.workingDir)+".", strings.TrimSpace(plan))
}

func TestAgentDefinitionDisabled(t *testing.T) {
	t.Parallel()

	toolNames := func(t *testing.T, c *coordinator) []string {
		t.Helper()
		built, err := c.buildTools(t.Context(), c.cfg.Config().Agents[config.AgentCoder], false)
		require.NoError(t, err)
		names := make([]string, 0, len(built))
		for _, tool := range built {
			names = append(names, tool.Info().Name)
		}
		return names
	}

	t.Run("default keeps the agent and dispatch tools", func(t *testing.T) {
		t.Parallel()
		names := toolNames(t, newDefinitionCoordinator(t, testEnv(t), "", ""))
		require.Contains(t, names, AgentToolName)
		require.Contains(t, names, DispatchAgentToolName)
		require.Contains(t, names, MessageAgentToolName)
		require.Contains(t, names, CancelDispatchToolName)
		require.Contains(t, names, ApplyDispatchToolName)
		require.Contains(t, names, DismissDispatchToolName)
	})

	t.Run("a disabled task removes the agent tool", func(t *testing.T) {
		t.Parallel()
		names := toolNames(t, newDefinitionCoordinator(t, testEnv(t), `{"task": {"disabled": true}}`, ""))
		require.NotContains(t, names, AgentToolName)
		require.Contains(t, names, DispatchAgentToolName)
	})

	t.Run("no enabled dispatch agent removes the dispatch tools", func(t *testing.T) {
		t.Parallel()
		names := toolNames(t, newDefinitionCoordinator(t, testEnv(t), `{"worker": {"disabled": true}}`, ""))
		require.NotContains(t, names, DispatchAgentToolName)
		require.NotContains(t, names, MessageAgentToolName)
		require.NotContains(t, names, CancelDispatchToolName)
		require.NotContains(t, names, ApplyDispatchToolName)
		require.NotContains(t, names, DismissDispatchToolName)
		require.Contains(t, names, AgentToolName)
	})

	t.Run("switching to a disabled plan agent says so", func(t *testing.T) {
		t.Parallel()
		c := newDefinitionCoordinator(t, testEnv(t), `{"plan": {"disabled": true}}`, "")
		err := c.SetMainAgent(config.AgentPlan)
		require.ErrorContains(t, err, `agent "plan" is disabled`)
	})
}

// preDefinitionDispatchTools is the dispatched toolset before #432: the
// task agent's read-only set, plus the write tools every dispatch was
// given (#64, #315), plus the support tools (#384).
func preDefinitionDispatchTools(c *config.Config) []string {
	return slices.Concat(
		c.Agents[config.AgentTask].AllowedTools,
		[]string{tools.BashToolName, tools.EditToolName, tools.MultiEditToolName, tools.WriteToolName, tools.TodosToolName},
		[]string{tools.JobOutputToolName, tools.JobKillToolName, tools.DiagnosticsToolName},
	)
}

func TestWorkerDefinitionToolset(t *testing.T) {
	t.Parallel()

	writeTools := []string{tools.BashToolName, tools.EditToolName, tools.MultiEditToolName, tools.WriteToolName}
	tests := []struct {
		name    string
		agents  string
		options string
		check   func(t *testing.T, c *config.Config, allowed, built []string)
	}{
		{
			name: "no agents section matches the pre-definition dispatch set",
			check: func(t *testing.T, c *config.Config, allowed, built []string) {
				require.ElementsMatch(t, preDefinitionDispatchTools(c), allowed)
				for _, name := range writeTools {
					require.Contains(t, built, name)
				}
			},
		},
		{
			name:   "a read-only worker gets no bash, edit or write",
			agents: `{"worker": {"tools": {"allow": ["@read"]}}}`,
			check: func(t *testing.T, _ *config.Config, allowed, built []string) {
				for _, name := range append(slices.Clone(writeTools), tools.TodosToolName) {
					require.NotContains(t, allowed, name)
					require.NotContains(t, built, name)
				}
				require.Contains(t, built, tools.ViewToolName)
			},
		},
		{
			name:    "disabled_tools beats the worker's @write",
			options: `, "disabled_tools": ["bash"]`,
			check: func(t *testing.T, _ *config.Config, allowed, built []string) {
				require.NotContains(t, allowed, tools.BashToolName)
				require.NotContains(t, built, tools.BashToolName)
				require.Contains(t, built, tools.EditToolName)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			initGitRepo(t, env.workingDir)
			c := newDefinitionCoordinator(t, env, tt.agents, tt.options)
			cfg := c.cfg.Config()

			allowed := dispatchAllowedTools(cfg.Agents[config.AgentWorker], cfg.Options.DisabledTools)

			entry, _ := provisionDispatchEntry(t, c, "")
			tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path})
			require.NoError(t, err)
			t.Cleanup(func() { tc.Close(context.Background()) })
			built := make([]string, 0, len(tc.Tools()))
			for _, tool := range tc.Tools() {
				built = append(built, tool.Info().Name)
			}

			tt.check(t, cfg, allowed, built)
		})
	}
}

// A worker defined read-only is a deliberate reviewer, not a dispatch
// the user's deny list gutted: the dispatch tool runs it rather than
// refusing with the #376 message.
func TestDispatchAgentToolRunsReadOnlyWorker(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)

	var defs map[string]config.AgentDefinition
	require.NoError(t, json.Unmarshal([]byte(`{"worker": {"tools": {"allow": ["@read"]}}}`), &defs))
	c.cfg.Config().AgentDefinitions = defs
	c.cfg.Config().SetupAgents()

	resp := runDispatchToolCall(t, c.dispatchTool(), DispatchAgentParams{Prompt: "review the diff"})
	handle := decodeDispatchHandle(t, resp)
	require.NotEmpty(t, handle.DispatchID)
}

func TestWorkerDefinitionModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		agents    string
		modelType config.SelectedModelType
		want      string
	}{
		{name: "default worker runs on the small slot", want: defSmallProvider},
		{name: "the dispatch's model parameter picks the slot", modelType: config.SelectedModelTypeLarge, want: defLargeProvider},
		{name: "the worker's slot is the default", agents: `{"worker": {"model": "large"}}`, want: defLargeProvider},
		{name: "the parameter overrides the worker's slot", agents: `{"worker": {"model": "large"}}`, modelType: config.SelectedModelTypeSmall, want: defSmallProvider},
		{name: "a pinned worker runs on its pin", agents: `{"worker": {"model": ` + pinJSON + `}}`, want: defPinProvider},
		// The tool refuses a model parameter on a pinned definition
		// (#433); this subtest covers the builder's defensive fallback
		// for callers that drive it directly.
		{name: "a pinned worker keeps its pin when the builder is driven directly", agents: `{"worker": {"model": ` + pinJSON + `}}`, modelType: config.SelectedModelTypeLarge, want: defPinProvider},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			initGitRepo(t, env.workingDir)
			c := newDefinitionCoordinator(t, env, tt.agents, "")

			entry, _ := provisionDispatchEntry(t, c, "")
			tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path})
			require.NoError(t, err)
			t.Cleanup(func() { tc.Close(context.Background()) })

			dispatched, err := c.buildDispatchedAgent(t.Context(), dispatchAgentOptions{
				Toolchain: tc,
				ModelType: tt.modelType,
			})
			require.NoError(t, err)
			require.NoError(t, dispatched.agent.WaitReady())
			require.Equal(t, tt.want, dispatched.model.ModelCfg.Provider)
			require.Equal(t, tt.want, dispatched.agent.Model().ModelCfg.Provider)
		})
	}
}

// The toolchain builder resolves opts.Agent (#433): the tools come from
// the named definition, an empty id means the worker, and an unknown id
// fails the build the way a bad default agent fails the load.
func TestBuildDispatchToolchainResolvesAgent(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	c := newDefinitionCoordinator(t, env, `{"reviewer": {"role": "dispatch"}}`, "")

	_, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: env.workingDir, Agent: "nope"})
	require.ErrorContains(t, err, `agent "nope" not configured`)

	entry, _ := provisionDispatchEntry(t, c, "")
	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path, Agent: "reviewer"})
	require.NoError(t, err)
	t.Cleanup(func() { tc.Close(context.Background()) })

	// A read-only reviewer stands out next to the worker's default
	// palette: bash is a worker write tool a read-only definition drops.
	c.cfg.Config().AgentDefinitions = map[string]config.AgentDefinition{
		"reviewer": {Role: ptr(config.AgentRoleDispatch), Tools: &config.AgentTools{Allow: []string{"@read"}}},
	}
	c.cfg.Config().SetupAgents()
	readOnly, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path, Agent: "reviewer"})
	require.NoError(t, err)
	t.Cleanup(func() { readOnly.Close(context.Background()) })

	names := func(tc *DispatchToolchain) []string {
		out := make([]string, 0, len(tc.Tools()))
		for _, tool := range tc.Tools() {
			out = append(out, tool.Info().Name)
		}
		return out
	}
	require.Contains(t, names(tc), tools.BashToolName, "the worker toolchain keeps the default palette")
	require.NotContains(t, names(readOnly), tools.BashToolName, "the reviewer toolchain is read-only")
}

// fakeDefinitionMCPTool stands in for an MCP tool in filterMCPTools: only the
// server and tool names matter.
type fakeDefinitionMCPTool struct {
	fantasy.AgentTool
	server, tool string
}

func (f fakeDefinitionMCPTool) Name() string        { return "mcp_" + f.server + "_" + f.tool }
func (f fakeDefinitionMCPTool) MCP() string         { return f.server }
func (f fakeDefinitionMCPTool) MCPToolName() string { return f.tool }

func TestFilterMCPTools(t *testing.T) {
	t.Parallel()

	all := []fakeDefinitionMCPTool{
		{server: "github", tool: "get_issue"},
		{server: "github", tool: "create_issue"},
		{server: "docs", tool: "search"},
	}
	names := func(got []fantasy.AgentTool) []string {
		out := make([]string, 0, len(got))
		for _, tool := range got {
			out = append(out, tool.(fakeDefinitionMCPTool).Name())
		}
		return out
	}

	tests := []struct {
		name  string
		allow map[string][]string
		want  []string
	}{
		{name: "nil allows every server", allow: nil, want: []string{"mcp_github_get_issue", "mcp_github_create_issue", "mcp_docs_search"}},
		{name: "empty allows none", allow: map[string][]string{}, want: []string{}},
		{name: "a server allows all its tools", allow: map[string][]string{"docs": nil}, want: []string{"mcp_docs_search"}},
		{name: "server:tool allows one tool", allow: map[string][]string{"github": {"get_issue"}}, want: []string{"mcp_github_get_issue"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := filterMCPTools(config.Agent{Name: "worker", AllowedMCP: tt.allow}, all)
			require.ElementsMatch(t, tt.want, names(got))
		})
	}

	// The built-in worker sees no MCP servers unless configured.
	c := newDefinitionCoordinator(t, testEnv(t), "", "")
	require.Empty(t, filterMCPTools(c.cfg.Config().Agents[config.AgentWorker], all))
}

func TestIntersectSkills(t *testing.T) {
	t.Parallel()
	require.Equal(t, []string{"jq", "go"}, intersectSkills([]string{"jq", "rust", "go"}, []string{"go", "jq"}))
	require.Empty(t, intersectSkills([]string{"rust"}, []string{"go"}))
}

func TestReadPromptFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ok.md"), []byte("prompt"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.log"), make([]byte, maxPromptFileBytes+1), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "prompts"), 0o755))

	got, err := readPromptFile(dir, "ok.md")
	require.NoError(t, err)
	require.Equal(t, "prompt", got)

	got, err = readPromptFile(t.TempDir(), filepath.Join(dir, "ok.md"))
	require.NoError(t, err, "an absolute path ignores the working dir")
	require.Equal(t, "prompt", got)

	_, err = readPromptFile(dir, "big.log")
	require.ErrorContains(t, err, "over the")

	_, err = readPromptFile(dir, "prompts")
	require.ErrorContains(t, err, "not a regular file")

	_, err = readPromptFile(dir, "missing.md")
	require.ErrorIs(t, err, os.ErrNotExist)
}
