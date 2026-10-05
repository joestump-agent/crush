package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/invopop/jsonschema"
)

// Agent ids for the built-in definitions. The worker is the dispatch
// agent definition; #432 and #433 wire it into the dispatch runtime.
const (
	AgentWorker = "worker"
)

// Roles an agent definition can have. A role change on a built-in is a
// load error, and a new agent id must be a dispatch agent.
const (
	AgentRoleMain     = "main"
	AgentRoleSubagent = "subagent"
	AgentRoleDispatch = "dispatch"
)

// Runtimes an agent definition can run on. Only builtin is honored
// today; a2a entries are validated and carried for #432 and #392.
const (
	AgentRuntimeBuiltin = "builtin"
	AgentRuntimeA2A     = "a2a"
)

// Workspaces a dispatch agent definition can run in.
const (
	AgentWorkspaceWorktree = "worktree"
	AgentWorkspaceNone     = "none"
)

// agentDefaultsKey is the special definition key whose todos and kill
// blocks become the defaults every agent inherits.
const agentDefaultsKey = "$defaults"

// agentIDPattern is the shape of a user agent id: a lowercase letter
// followed by up to 31 lowercase letters, digits, or hyphens.
var agentIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// builtinPromptIDs are the prompt templates a builtin: prompt can name.
var builtinPromptIDs = []string{"coder", "plan", "task", "dispatch"}

// readOnlyToolNames is the @read group: today's resolveReadOnlyTools
// list.
var readOnlyToolNames = []string{
	"glob",
	"grep",
	"ls",
	"lsp_call_hierarchy",
	"lsp_definition",
	"lsp_symbols",
	"semantic_search",
	"sourcegraph",
	"view",
}

// writeToolNames is the @write group: today's dispatchWriteTools, the
// tools a dispatch is useless without (#64).
var writeToolNames = []string{
	"bash",
	"edit",
	"multiedit",
	"write",
	"todos",
}

// agentToolGroups are the named tool groups a definition's allow and
// deny lists can reference.
var agentToolGroups = map[string][]string{
	"@read":  readOnlyToolNames,
	"@write": writeToolNames,
}

// AgentDefinition is the user-writable shape of an agent under the
// config's agents key (#333). Every field is optional: a null or
// omitted field inherits from the built-in with the same id, lists
// replace rather than append, and "off" disables the knob it configures.
// The resolved form lives in Agent; building agents from definitions is
// #432, so fields the runtime does not honor yet only load and warn.
type AgentDefinition struct {
	// Role classifies the agent: main, subagent, or dispatch. It cannot
	// be changed on a built-in, and a new id must be dispatch.
	Role *string `json:"role,omitempty" jsonschema:"description=Agent role,enum=main,enum=subagent,enum=dispatch"`
	// Runtime selects the execution path: builtin (in-process) or a2a
	// (an external Agent Card). Only a2a is restricted: dispatch agents
	// only, until #392.
	Runtime *string `json:"runtime,omitempty" jsonschema:"description=Execution runtime,enum=builtin,enum=a2a"`
	// Name and Description are display values resolved into the agent.
	Name        *string `json:"name,omitempty" jsonschema:"description=Display name"`
	Description *string `json:"description,omitempty" jsonschema:"description=Agent description"`
	// Disabled removes the agent. coder cannot be disabled.
	Disabled *bool `json:"disabled,omitempty" jsonschema:"description=Disable this agent"`
	// Model is the "large" or "small" slot, or an explicit provider and
	// model pin. The slot is honored; the pin is carried for #432.
	Model *AgentModel `json:"model,omitempty" jsonschema:"description=The large or small model slot, or an explicit provider and model pin"`
	// Prompt is builtin:<coder|plan|task|dispatch> or file:<path>, and
	// prompt_append is file:<path>. Both are carried for #432.
	Prompt       *string `json:"prompt,omitempty" jsonschema:"description=System prompt: builtin:<id> or file:<path>"`
	PromptAppend *string `json:"prompt_append,omitempty" jsonschema:"description=Appended system prompt: file:<path>"`
	// Tools allow and deny lists reference tool names or the @read and
	// @write groups. Effective tools are allow minus deny minus the
	// user's disabled_tools, so a definition never widens user policy.
	Tools *AgentTools `json:"tools,omitempty" jsonschema:"description=Tool allow and deny lists; entries are tool names or the @read and @write groups"`
	// MCP allow list entries are *, a server id, or server:tool.
	MCP *AgentMCP `json:"mcp,omitempty" jsonschema:"description=MCP servers available to the agent: *, a server id, or server:tool"`
	// Skills lists skill names for the agent; carried for #432.
	Skills []string `json:"skills,omitempty" jsonschema:"description=Skill names for this agent"`
	// ContextPaths overrides the agent's context file paths.
	ContextPaths []string `json:"context_paths,omitempty" jsonschema:"description=Context file paths for this agent"`
	// Workspace selects dispatch isolation: worktree or none. Only
	// meaningful on dispatch agents; carried for #432.
	Workspace *string `json:"workspace,omitempty" jsonschema:"description=Dispatch workspace isolation,enum=worktree,enum=none"`
	// Todos configures the nudge ladder knobs.
	Todos *AgentTodos `json:"todos,omitempty" jsonschema:"description=Todo enforcement knobs for this agent"`
	// Kill configures the deterministic kill thresholds; dispatch
	// agents only.
	Kill *AgentKill `json:"kill,omitempty" jsonschema:"description=Kill thresholds for this agent; dispatch agents only"`
	// Card, Auth, and Transport configure runtime a2a agents: the
	// external Agent Card URL, the bearer token, and the idle timeout.
	Card      *string         `json:"card,omitempty" jsonschema:"description=External Agent Card URL for runtime a2a agents"`
	Auth      *AgentAuth      `json:"auth,omitempty" jsonschema:"description=Bearer auth for runtime a2a agents"`
	Transport *AgentTransport `json:"transport,omitempty" jsonschema:"description=Transport tuning for runtime a2a agents"`
}

// AgentTools is a definition's tool allow and deny lists.
type AgentTools struct {
	// Allow entries are tool names, @read, @write, or *. Nil inherits.
	Allow []string `json:"allow,omitempty" jsonschema:"description=Tools to allow: names, @read, @write, or *"`
	// Deny entries are tool names or groups, removed from the effective
	// set after the user's disabled_tools.
	Deny []string `json:"deny,omitempty" jsonschema:"description=Tools to deny: names or groups"`
}

// AgentMCP is a definition's MCP allow list.
type AgentMCP struct {
	// Allow entries are *, a server id, or server:tool. Nil inherits;
	// an empty list means no MCP servers.
	Allow []string `json:"allow,omitempty" jsonschema:"description=MCP servers: *, a server id, or server:tool"`
}

// AgentTodos is a definition's nudge ladder configuration. The names
// are the definition-level spellings of the TodoEnforcementConfig
// fields.
type AgentTodos struct {
	// Nudge toggles nudge injection.
	Nudge *bool `json:"nudge,omitempty" jsonschema:"description=Inject a nudge when the agent works without a todo list"`
	// NudgeAfterToolCalls is how many tool calls without todos activity
	// trip the first nudge; 0 or off disables nudging.
	NudgeAfterToolCalls *IntOrOff `json:"nudge_after_tool_calls,omitempty" jsonschema:"description=Tool calls without todos activity before a nudge is injected; 0 or off disables"`
	// HardGate rejects mutating tools until a todo list exists.
	HardGate *bool `json:"hard_gate,omitempty" jsonschema:"description=Reject mutating tools until a todo list exists"`
}

// AgentKill is a definition's deterministic kill thresholds. Kill
// applies to dispatch agents only.
type AgentKill struct {
	// AfterIgnoredNudges is how many ignored nudges trip the wander
	// kill; 0 or off disables it.
	AfterIgnoredNudges *IntOrOff `json:"after_ignored_nudges,omitempty" jsonschema:"description=Ignored nudges before the agent is killed; 0 or off disables"`
	// Stall kills a run whose todo list has not been updated for this
	// long while it keeps running; 0 or off disables it.
	Stall *Duration `json:"stall,omitempty" jsonschema:"description=Time without a todo update that marks the run stalled; 0 or off disables"`
	// Timeout kills the run after this long whatever its progress; 0 or
	// off disables it.
	Timeout *Duration `json:"timeout,omitempty" jsonschema:"description=Total run time before the agent is killed; 0 or off disables"`
}

// AgentAuth configures bearer auth for a runtime a2a agent.
type AgentAuth struct {
	// Type is the auth scheme; bearer is the only one today.
	Type *string `json:"type,omitempty" jsonschema:"description=Auth scheme,enum=bearer"`
	// Token is the bearer token, typically a $VAR reference.
	Token *string `json:"token,omitempty" jsonschema:"description=Bearer token, usually a $VAR reference"`
}

// AgentTransport tunes an a2a agent's connection.
type AgentTransport struct {
	// IdleTimeout is how long a dispatched run may stay silent before
	// the inactivity backstop ends it; 0 or off disables the backstop.
	IdleTimeout *Duration `json:"idle_timeout,omitempty" jsonschema:"description=How long a run may stay silent before it is ended; 0 or off disables"`
}

// AgentModel is a definition's model reference: the large or small
// slot, or an explicit provider and model pin. It accepts a JSON
// string or object.
type AgentModel struct {
	// Type is the slot; meaningful only when Ref is nil.
	Type SelectedModelType `json:"-"`
	// Ref is the explicit pin; nil means the slot named by Type.
	Ref *AgentModelRef `json:"-"`
}

// AgentModelRef pins an agent to an explicit provider and model.
type AgentModelRef struct {
	Provider string `json:"provider" jsonschema:"description=Provider id"`
	Model    string `json:"model" jsonschema:"description=Model id"`
}

// UnmarshalJSON decodes a "large" or "small" string, or an object with
// provider and model.
func (m *AgentModel) UnmarshalJSON(data []byte) error {
	var slot string
	if err := json.Unmarshal(data, &slot); err == nil {
		switch SelectedModelType(slot) {
		case SelectedModelTypeLarge, SelectedModelTypeSmall:
			m.Type = SelectedModelType(slot)
			m.Ref = nil
			return nil
		default:
			return fmt.Errorf("unknown model slot %q: want \"large\", \"small\", or a provider and model", slot)
		}
	}
	var ref AgentModelRef
	if err := json.Unmarshal(data, &ref); err != nil {
		return fmt.Errorf("invalid model: want \"large\", \"small\", or {\"provider\", \"model\"}")
	}
	if ref.Provider == "" || ref.Model == "" {
		return fmt.Errorf("model pin needs both provider and model")
	}
	m.Type = SelectedModelTypeLarge
	m.Ref = &ref
	return nil
}

// MarshalJSON writes the slot as a string, or the pin as an object.
func (m AgentModel) MarshalJSON() ([]byte, error) {
	if m.Ref != nil {
		return json.Marshal(*m.Ref)
	}
	return json.Marshal(string(m.Type))
}

// JSONSchema describes the accepted forms: the named slot or an
// explicit provider and model object.
func (AgentModel) JSONSchema() *jsonschema.Schema {
	props := jsonschema.NewProperties()
	props.Set("provider", &jsonschema.Schema{Type: "string"})
	props.Set("model", &jsonschema.Schema{Type: "string"})
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Enum: []any{"large", "small"}},
			{Type: "object", Properties: props, Required: []string{"provider", "model"}},
		},
	}
}

// builtinAgentDefinitions returns the built-in agents as data. The
// values match what SetupAgents built from literals before #333: same
// names, descriptions, model slots, tool sets, and MCP visibility, plus
// the worker definition dispatch will use (#432, #433).
func builtinAgentDefinitions() map[string]AgentDefinition {
	return map[string]AgentDefinition{
		AgentCoder: {
			Role:        strPtr(AgentRoleMain),
			Runtime:     strPtr(AgentRuntimeBuiltin),
			Name:        strPtr("Coder"),
			Description: strPtr("An agent that helps with executing coding tasks."),
			Model:       &AgentModel{Type: SelectedModelTypeLarge},
			Prompt:      strPtr("builtin:coder"),
			Tools:       &AgentTools{Allow: []string{"*"}},
			MCP:         &AgentMCP{Allow: []string{"*"}},
		},
		AgentPlan: {
			Role:        strPtr(AgentRoleMain),
			Runtime:     strPtr(AgentRuntimeBuiltin),
			Name:        strPtr("Plan"),
			Description: strPtr("An agent that performs deep analysis and prepares implementation plans without modifying files."),
			Model:       &AgentModel{Type: SelectedModelTypeLarge},
			Prompt:      strPtr("builtin:plan"),
			Tools:       &AgentTools{Allow: slices.Clone(planToolNames())},
			MCP:         &AgentMCP{Allow: []string{}},
		},
		AgentTask: {
			Role:        strPtr(AgentRoleSubagent),
			Runtime:     strPtr(AgentRuntimeBuiltin),
			Name:        strPtr("Task"),
			Description: strPtr("An agent that helps with searching for context and finding implementation details."),
			Model:       &AgentModel{Type: SelectedModelTypeLarge},
			Prompt:      strPtr("builtin:task"),
			Tools:       &AgentTools{Allow: []string{"@read"}},
			MCP:         &AgentMCP{Allow: []string{}},
		},
		AgentWorker: {
			Role:        strPtr(AgentRoleDispatch),
			Runtime:     strPtr(AgentRuntimeBuiltin),
			Name:        strPtr("Worker"),
			Description: strPtr("An agent that produces work in an isolated workspace."),
			Model:       &AgentModel{Type: SelectedModelTypeSmall},
			Prompt:      strPtr("builtin:dispatch"),
			Tools:       &AgentTools{Allow: []string{"@read", "@write"}},
			MCP:         &AgentMCP{Allow: []string{}},
			Workspace:   strPtr(AgentWorkspaceWorktree),
		},
	}
}

// planToolNames is the plan agent's tool list, the literal behind
// resolvePlanTools.
func planToolNames() []string {
	return []string{
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
	}
}

func strPtr(s string) *string { return &s }

// expandToolRefs resolves allow and deny entries: @read and @write
// expand to their groups and * to every tool name. The returned list
// is fresh and deduplicated.
func expandToolRefs(refs []string) []string {
	if refs == nil {
		return nil
	}
	var out []string
	for _, ref := range refs {
		switch {
		case ref == "*":
			out = append(out, allToolNames()...)
		case strings.HasPrefix(ref, "@"):
			out = append(out, agentToolGroups[ref]...)
		default:
			out = append(out, ref)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(out)))
}

// expandMCPAllow turns a definition's mcp.allow list into the resolved
// AllowedMCP map: * means every server (a nil map), a bare id means
// every tool of that server (a nil slice), and server:tool names one
// tool. An empty allow list means no servers at all.
func expandMCPAllow(allow []string) map[string][]string {
	if allow == nil || slices.Contains(allow, "*") {
		return nil
	}
	out := make(map[string][]string, len(allow))
	for _, entry := range allow {
		server, tool, found := strings.Cut(entry, ":")
		if !found {
			out[server] = nil
			continue
		}
		if !slices.Contains(out[server], tool) {
			out[server] = append(out[server], tool)
		}
	}
	return out
}

// overlayDefinition returns base with every field over sets applied on
// top. Lists replace; they do not append. Nil or omitted fields leave
// the base value in place.
func overlayDefinition(base, over AgentDefinition) AgentDefinition {
	if over.Role != nil {
		base.Role = over.Role
	}
	if over.Runtime != nil {
		base.Runtime = over.Runtime
	}
	if over.Name != nil {
		base.Name = over.Name
	}
	if over.Description != nil {
		base.Description = over.Description
	}
	if over.Disabled != nil {
		base.Disabled = over.Disabled
	}
	if over.Model != nil {
		base.Model = over.Model
	}
	if over.Prompt != nil {
		base.Prompt = over.Prompt
	}
	if over.PromptAppend != nil {
		base.PromptAppend = over.PromptAppend
	}
	if over.Tools != nil {
		if base.Tools == nil {
			base.Tools = &AgentTools{}
		}
		if over.Tools.Allow != nil {
			base.Tools.Allow = over.Tools.Allow
		}
		if over.Tools.Deny != nil {
			base.Tools.Deny = over.Tools.Deny
		}
	}
	if over.MCP != nil {
		if base.MCP == nil {
			base.MCP = &AgentMCP{}
		}
		if over.MCP.Allow != nil {
			base.MCP.Allow = over.MCP.Allow
		}
	}
	if over.Skills != nil {
		base.Skills = over.Skills
	}
	if over.ContextPaths != nil {
		base.ContextPaths = over.ContextPaths
	}
	if over.Workspace != nil {
		base.Workspace = over.Workspace
	}
	if over.Todos != nil {
		if base.Todos == nil {
			base.Todos = &AgentTodos{}
		}
		if over.Todos.Nudge != nil {
			base.Todos.Nudge = over.Todos.Nudge
		}
		if over.Todos.NudgeAfterToolCalls != nil {
			base.Todos.NudgeAfterToolCalls = over.Todos.NudgeAfterToolCalls
		}
		if over.Todos.HardGate != nil {
			base.Todos.HardGate = over.Todos.HardGate
		}
	}
	if over.Kill != nil {
		if base.Kill == nil {
			base.Kill = &AgentKill{}
		}
		if over.Kill.AfterIgnoredNudges != nil {
			base.Kill.AfterIgnoredNudges = over.Kill.AfterIgnoredNudges
		}
		if over.Kill.Stall != nil {
			base.Kill.Stall = over.Kill.Stall
		}
		if over.Kill.Timeout != nil {
			base.Kill.Timeout = over.Kill.Timeout
		}
	}
	if over.Card != nil {
		base.Card = over.Card
	}
	if over.Auth != nil {
		if base.Auth == nil {
			base.Auth = &AgentAuth{}
		}
		if over.Auth.Type != nil {
			base.Auth.Type = over.Auth.Type
		}
		if over.Auth.Token != nil {
			base.Auth.Token = over.Auth.Token
		}
	}
	if over.Transport != nil {
		if base.Transport == nil {
			base.Transport = &AgentTransport{}
		}
		if over.Transport.IdleTimeout != nil {
			base.Transport.IdleTimeout = over.Transport.IdleTimeout
		}
	}
	return base
}

// effectiveAgentDefinitions layers the user's definitions over the
// built-ins: $defaults first, then each agent's own entry, field by
// field. A runtime a2a entry replaces its built-in wholesale.
func effectiveAgentDefinitions(user map[string]AgentDefinition) map[string]AgentDefinition {
	merged := builtinAgentDefinitions()

	if defaults, ok := user[agentDefaultsKey]; ok {
		for id, def := range merged {
			merged[id] = overlayDefinition(def, AgentDefinition{Todos: defaults.Todos, Kill: defaults.Kill})
		}
	}

	for id, def := range user {
		if id == agentDefaultsKey {
			continue
		}
		base := merged[id]
		if runtime := orString(def.Runtime, orString(base.Runtime, AgentRuntimeBuiltin)); runtime == AgentRuntimeA2A {
			// A runtime a2a entry replaces the built-in wholesale: the
			// external card defines the agent. The role still comes from
			// the built-in when the entry does not name one.
			if def.Role == nil {
				def.Role = base.Role
			}
			merged[id] = def
			continue
		}
		merged[id] = overlayDefinition(base, def)
	}
	return merged
}

func orString(s *string, fallback string) string {
	if s != nil {
		return *s
	}
	return fallback
}

// definitionRole returns the definition's role, falling back to the
// built-in's, else an empty string.
func definitionRole(id string, def AgentDefinition) string {
	if def.Role != nil {
		return *def.Role
	}
	if builtin, ok := builtinAgentDefinitions()[id]; ok {
		return *builtin.Role
	}
	return ""
}

// knownToolNames is allToolNames plus the group spellings, the set a
// tools entry may name.
func knownToolNames() []string {
	known := allToolNames()
	known = append(known, "@read", "@write", "*")
	return known
}

// ValidateAgents checks every agent definition: ids, roles, runtimes,
// workspaces, tool and MCP references, prompts, and the numeric knobs.
// It names the offending path in every error, for example
// agents.worker.tools.allow[2]: unknown tool "x". It runs on the
// merged config in Load, before SetupAgents, and on every reload.
func (c *Config) ValidateAgents() error {
	for id, def := range c.AgentDefinitions {
		path := "agents." + id
		if id == agentDefaultsKey {
			if err := validateDefaultsDefinition(def); err != nil {
				return err
			}
			continue
		}
		if !agentIDPattern.MatchString(id) {
			return fmt.Errorf("%s: id must match %s", path, agentIDPattern.String())
		}
		if err := validateAgentDefinition(c, id, def); err != nil {
			return err
		}
	}
	return nil
}

// validateDefaultsDefinition rejects anything but todos and kill on the
// $defaults key, and validates their values.
func validateDefaultsDefinition(def AgentDefinition) error {
	path := "agents." + agentDefaultsKey
	bare := def
	bare.Todos = nil
	bare.Kill = nil
	if !isZeroDefinition(bare) {
		return fmt.Errorf("%s: only todos and kill may be set", path)
	}
	return validateTodosAndKill(path, def.Todos, def.Kill)
}

// isZeroDefinition reports whether every field of def is unset.
func isZeroDefinition(def AgentDefinition) bool {
	return def.Role == nil && def.Runtime == nil && def.Name == nil &&
		def.Description == nil && def.Disabled == nil && def.Model == nil &&
		def.Prompt == nil && def.PromptAppend == nil && def.Tools == nil &&
		def.MCP == nil && def.Skills == nil && def.ContextPaths == nil &&
		def.Workspace == nil && def.Card == nil && def.Auth == nil &&
		def.Transport == nil
}

// validateAgentDefinition runs the structural rules on one definition.
func validateAgentDefinition(c *Config, id string, def AgentDefinition) error {
	path := "agents." + id
	builtin, isBuiltin := builtinAgentDefinitions()[id]

	role := definitionRole(id, def)
	switch role {
	case AgentRoleMain, AgentRoleSubagent, AgentRoleDispatch:
	default:
		return fmt.Errorf("%s.role: unknown role %q", path, role)
	}

	if isBuiltin && def.Role != nil && *def.Role != *builtin.Role {
		return fmt.Errorf("%s.role: cannot change the role of built-in agent %q from %q to %q", path, id, *builtin.Role, *def.Role)
	}
	if !isBuiltin && role != AgentRoleDispatch {
		return fmt.Errorf("%s.role: a new agent must be a %q agent", path, AgentRoleDispatch)
	}

	runtime := orString(def.Runtime, orString(builtin.Runtime, AgentRuntimeBuiltin))
	if runtime != AgentRuntimeBuiltin && runtime != AgentRuntimeA2A {
		return fmt.Errorf("%s.runtime: unknown runtime %q", path, runtime)
	}
	if runtime == AgentRuntimeA2A {
		if role != AgentRoleDispatch {
			return fmt.Errorf("%s.runtime: a2a agents must be %q agents until #392", path, AgentRoleDispatch)
		}
		if err := validateA2AFields(path, def); err != nil {
			return err
		}
	}

	if def.Disabled != nil && *def.Disabled && id == AgentCoder {
		return fmt.Errorf("%s.disabled: the coder agent cannot be disabled", path)
	}

	if def.Workspace != nil && *def.Workspace != AgentWorkspaceWorktree && *def.Workspace != AgentWorkspaceNone {
		return fmt.Errorf("%s.workspace: unknown workspace %q", path, *def.Workspace)
	}

	if err := validateTools(path, def.Tools); err != nil {
		return err
	}
	if err := c.validateMCPAllow(path, def.MCP); err != nil {
		return err
	}
	if err := validatePrompt(path, def.Prompt, false); err != nil {
		return err
	}
	if err := validatePrompt(path, def.PromptAppend, true); err != nil {
		return err
	}
	if role != AgentRoleDispatch && def.Kill != nil {
		return fmt.Errorf("%s.kill: kill thresholds apply to %q agents only", path, AgentRoleDispatch)
	}
	return validateTodosAndKill(path, def.Todos, def.Kill)
}

// a2aAllowedFields reports whether the named definition field may be
// set on a runtime a2a agent. Everything not on the list is an error:
// an a2a agent is defined by its external card, not by local fields.
// Role is allowed: a new a2a agent has no built-in to inherit a
// dispatch role from, and the runtime check has already pinned the
// role to dispatch by the time this runs.
func a2aAllowedFields(field string) bool {
	switch field {
	case "role", "card", "auth", "workspace", "transport", "kill", "name", "description", "disabled":
		return true
	}
	return false
}

// validateA2AFields rejects local fields a runtime a2a agent may not
// set, and requires workspace none when set.
func validateA2AFields(path string, def AgentDefinition) error {
	fields := []struct {
		name  string
		isSet bool
	}{
		{"role", def.Role != nil},
		{"runtime", false},
		{"model", def.Model != nil},
		{"prompt", def.Prompt != nil},
		{"prompt_append", def.PromptAppend != nil},
		{"tools", def.Tools != nil},
		{"mcp", def.MCP != nil},
		{"skills", def.Skills != nil},
		{"context_paths", def.ContextPaths != nil},
		{"card", def.Card != nil},
		{"auth", def.Auth != nil},
		{"transport", def.Transport != nil},
		{"todos", def.Todos != nil},
		{"kill", def.Kill != nil},
	}
	for _, field := range fields {
		if field.isSet && !a2aAllowedFields(field.name) {
			return fmt.Errorf("%s.%s: a runtime a2a agent is defined by its card and may not set this field", path, field.name)
		}
	}
	if def.Kill != nil && def.Kill.Stall != nil {
		return fmt.Errorf("%s.kill.stall: a runtime a2a agent may only set kill.timeout", path)
	}
	if def.Kill != nil && def.Kill.AfterIgnoredNudges != nil {
		return fmt.Errorf("%s.kill.after_ignored_nudges: a runtime a2a agent may only set kill.timeout", path)
	}
	if def.Workspace != nil && *def.Workspace != AgentWorkspaceNone {
		return fmt.Errorf("%s.workspace: a runtime a2a agent runs without a worktree; use %q", path, AgentWorkspaceNone)
	}
	if def.Auth != nil && def.Auth.Type != nil && *def.Auth.Type != "bearer" {
		return fmt.Errorf("%s.auth.type: unknown auth type %q", path, *def.Auth.Type)
	}
	return nil
}

// validateTools checks a tools block: every entry names a known tool,
// group, or *.
func validateTools(path string, tools *AgentTools) error {
	if tools == nil {
		return nil
	}
	known := knownToolNames()
	for _, list := range []struct {
		field string
		refs  []string
	}{
		{"tools.allow", tools.Allow},
		{"tools.deny", tools.Deny},
	} {
		for i, ref := range list.refs {
			if !slices.Contains(known, ref) {
				return fmt.Errorf("%s.%s[%d]: unknown tool %q", path, list.field, i, ref)
			}
		}
	}
	return nil
}

// validateMCPAllow checks an mcp.allow block: * is always valid, and
// every other entry names a configured server, with an optional tool.
func (c *Config) validateMCPAllow(path string, mcp *AgentMCP) error {
	if mcp == nil {
		return nil
	}
	for i, entry := range mcp.Allow {
		if entry == "*" {
			continue
		}
		server, _, _ := strings.Cut(entry, ":")
		if c.MCP != nil {
			if _, ok := c.MCP[server]; ok {
				continue
			}
		}
		return fmt.Errorf("%s.mcp.allow[%d]: unknown MCP server %q", path, i, server)
	}
	return nil
}

// validatePrompt checks a builtin: or file: prompt reference. A file
// reference must exist on disk; an appended prompt must be a file.
func validatePrompt(path string, prompt *string, appendOnly bool) error {
	if prompt == nil {
		return nil
	}
	value := *prompt
	switch {
	case strings.HasPrefix(value, "builtin:"):
		if appendOnly {
			return fmt.Errorf("%s: prompt_append must be a file:<path> reference, not builtin:", path)
		}
		id := strings.TrimPrefix(value, "builtin:")
		if !slices.Contains(builtinPromptIDs, id) {
			return fmt.Errorf("%s: unknown builtin prompt %q", path, id)
		}
		return nil
	case strings.HasPrefix(value, "file:"):
		file := strings.TrimPrefix(value, "file:")
		if _, err := os.Stat(file); err != nil {
			return fmt.Errorf("%s: prompt file %q does not exist", path, file)
		}
		return nil
	default:
		return fmt.Errorf("%s: prompt must be builtin:<id> or file:<path>, got %q", path, value)
	}
}

// validateTodosAndKill checks the numeric knobs: 0 or "off" disables,
// and a negative value is an error naming its path.
func validateTodosAndKill(path string, todos *AgentTodos, kill *AgentKill) error {
	if todos != nil {
		if todos.NudgeAfterToolCalls != nil && *todos.NudgeAfterToolCalls < 0 {
			return fmt.Errorf("%s.todos.nudge_after_tool_calls: must not be negative (got %d)", path, *todos.NudgeAfterToolCalls)
		}
	}
	if kill != nil {
		if kill.AfterIgnoredNudges != nil && *kill.AfterIgnoredNudges < 0 {
			return fmt.Errorf("%s.kill.after_ignored_nudges: must not be negative (got %d)", path, *kill.AfterIgnoredNudges)
		}
		if kill.Stall != nil && *kill.Stall < 0 {
			return fmt.Errorf("%s.kill.stall: must not be negative (got %s)", path, durationForError(*kill.Stall))
		}
		if kill.Timeout != nil && *kill.Timeout < 0 {
			return fmt.Errorf("%s.kill.timeout: must not be negative (got %s)", path, durationForError(*kill.Timeout))
		}
	}
	return nil
}

// ValidateAgentModelRefs checks explicit model pins against the
// configured providers. It runs after providers are merged, just
// before SetupAgents, so an unknown pin is a load error rather than a
// silently ignored definition.
func (c *Config) ValidateAgentModelRefs() error {
	if len(c.AgentDefinitions) == 0 {
		return nil
	}
	for id, def := range c.AgentDefinitions {
		if def.Model == nil || def.Model.Ref == nil {
			continue
		}
		path := "agents." + id + ".model"
		ref := *def.Model.Ref
		if c.Providers == nil {
			return fmt.Errorf("%s: unknown provider %q", path, ref.Provider)
		}
		provider, ok := c.Providers.Get(ref.Provider)
		if !ok {
			return fmt.Errorf("%s: unknown provider %q", path, ref.Provider)
		}
		if !slices.ContainsFunc(provider.Models, func(m catwalk.Model) bool {
			return m.ID == ref.Model
		}) {
			return fmt.Errorf("%s: provider %q has no model %q", path, ref.Provider, ref.Model)
		}
	}
	return nil
}

// warnedDefinitionFields deduplicates the not-honored-yet warnings so a
// reload or a config-field write does not repeat them.
var warnedDefinitionFields sync.Map

// warnUnhonoredFields logs one warning per definition field the runtime
// parses but does not honor yet, skipping values that merely restate
// the built-in default so an untouched config starts up silent. #432
// builds agents from definitions and retires these.
func warnUnhonoredFields(id string, def AgentDefinition) {
	builtin := builtinAgentDefinitions()[id]
	fields := []struct {
		name  string
		isSet bool
	}{
		{"runtime", orString(def.Runtime, AgentRuntimeBuiltin) == AgentRuntimeA2A},
		{"disabled", def.Disabled != nil && *def.Disabled},
		{"model", def.Model != nil && def.Model.Ref != nil},
		{"prompt", def.Prompt != nil && (builtin.Prompt == nil || *def.Prompt != *builtin.Prompt)},
		{"prompt_append", def.PromptAppend != nil},
		{"skills", def.Skills != nil},
		{"workspace", def.Workspace != nil && (builtin.Workspace == nil || *def.Workspace != *builtin.Workspace)},
		{"card", def.Card != nil},
		{"auth", def.Auth != nil},
		{"transport", def.Transport != nil},
	}
	for _, field := range fields {
		if !field.isSet {
			continue
		}
		key := id + "." + field.name
		if _, loaded := warnedDefinitionFields.LoadOrStore(key, struct{}{}); loaded {
			continue
		}
		slog.Warn("Agent definition field is parsed but not honored yet", "agent", id, "field", field.name)
	}
}
