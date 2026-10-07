package agent

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filepathext"
)

// builtinPromptTemplates maps a builtin prompt id to its embedded
// template, the runtime side of the builtin:<id> prompt references a
// definition may carry (#432).
var builtinPromptTemplates = map[string]string{
	"coder":    string(coderPromptTmpl),
	"plan":     string(planPromptTmpl),
	"task":     string(taskPromptTmpl),
	"dispatch": string(dispatchPromptTmpl),
}

// agentPrompt resolves an agent definition's prompt (#432). The built-in
// agents all carry an explicit builtin: reference; an empty spec means
// the agent keeps the template it was born with by id, and a dispatch
// agent with no id-matched template falls back to the dispatch template.
// file:<path> reads the file — relative paths resolve against the
// working dir the definition loaded from — as the template. Anything
// else is a config validation bug reaching the runtime, so it errors.
func agentPrompt(agent config.Agent, workingDir string, opts ...prompt.Option) (*prompt.Prompt, error) {
	spec := agent.Prompt
	switch {
	case spec == "":
		id := agent.ID
		if _, ok := builtinPromptTemplates[id]; !ok && agent.Role == config.AgentRoleDispatch {
			id = "dispatch"
		}
		tmpl, ok := builtinPromptTemplates[id]
		if !ok {
			return nil, fmt.Errorf("agent %q has no builtin prompt", agent.ID)
		}
		return prompt.NewPrompt(id, tmpl, opts...)
	case strings.HasPrefix(spec, "builtin:"):
		id := strings.TrimPrefix(spec, "builtin:")
		tmpl, ok := builtinPromptTemplates[id]
		if !ok {
			return nil, fmt.Errorf("unknown builtin prompt %q", id)
		}
		return prompt.NewPrompt(id, tmpl, opts...)
	case strings.HasPrefix(spec, "file:"):
		path := strings.TrimPrefix(spec, "file:")
		if path == "" {
			return nil, fmt.Errorf("agent %q: empty file: prompt reference", agent.ID)
		}
		content, err := readPromptFile(workingDir, path)
		if err != nil {
			return nil, fmt.Errorf("read agent prompt: %w", err)
		}
		return prompt.NewPrompt(agent.ID, content, opts...)
	default:
		return nil, fmt.Errorf("invalid prompt %q: want builtin:<id> or file:<path>", spec)
	}
}

// agentSystemPrompt renders p and appends the definition's
// prompt_append file (#432) verbatim after it, separated by a blank
// line.
func agentSystemPrompt(ctx context.Context, p *prompt.Prompt, agent config.Agent, workingDir, provider, model string, store *config.ConfigStore) (string, error) {
	systemPrompt, err := p.Build(ctx, provider, model, store)
	if err != nil {
		return "", err
	}
	if agent.PromptAppend == "" {
		return systemPrompt, nil
	}
	path, ok := strings.CutPrefix(agent.PromptAppend, "file:")
	if !ok || path == "" {
		return "", fmt.Errorf("invalid prompt_append %q: want file:<path>", agent.PromptAppend)
	}
	content, err := readPromptFile(workingDir, path)
	if err != nil {
		return "", fmt.Errorf("read prompt_append: %w", err)
	}
	return strings.TrimRight(systemPrompt, "\n") + "\n\n" + content, nil
}

// maxPromptFileBytes caps a file: prompt or prompt_append (#432). The
// file lands in every request's system prompt, so a mistyped path to a
// log or a binary must fail instead of swallowing the context window.
const maxPromptFileBytes = 256 << 10

// readPromptFile reads a definition's file: prompt, resolved against the
// working directory unless absolute. The file must be a regular file no
// larger than maxPromptFileBytes.
func readPromptFile(workingDir, path string) (string, error) {
	full := filepathext.SmartJoin(workingDir, path)
	// By design: the path is the user's own config value, with the same
	// trust as options.context_paths, and reading it is the feature.
	// codeql[go/path-injection]
	info, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxPromptFileBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d-byte limit", path, info.Size(), maxPromptFileBytes)
	}
	// codeql[go/path-injection]
	content, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// agentPromptOptions assembles the options an agent's prompt render
// gets from its definition: the agent's context paths (#432) replace
// the global ones when set, and the agent's skill list filters the
// available-skills section when set.
func agentPromptOptions(agent config.Agent) []prompt.Option {
	var opts []prompt.Option
	if len(agent.ContextPaths) > 0 {
		opts = append(opts, prompt.WithContextPaths(agent.ContextPaths))
	}
	if len(agent.Skills) > 0 {
		opts = append(opts, prompt.WithSkills(agent.Skills))
	}
	return opts
}

// intersectSkills keeps the requested skills a worker definition
// allows (#432), preserving the request's order: a dispatch narrows a
// worker's skills, never widens them.
func intersectSkills(requested, allowed []string) []string {
	var out []string
	for _, s := range requested {
		if slices.Contains(allowed, s) {
			out = append(out, s)
		}
	}
	return out
}
