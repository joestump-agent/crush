package shellconfig

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
)

// builtinAgentIDs are the agent ids the config package treats as
// built-ins (config's AgentCoder, AgentPlan, AgentTask, AgentWorker).
// shellconfig cannot import config, which imports this package to run
// crushrc files, so the list is repeated here; the external test in
// agent_test.go pins the two against each other.
var builtinAgentIDs = []string{"coder", "plan", "task", "worker"}

// handleAgent implements the `agent` builtin.
//
// Usage:
//
//	agent defaults [--nudge B] [--nudge-after N|off] [--hard-gate B]
//	    [--kill-after-nudges N|off] [--stall D|off] [--timeout D|off]
//	agent add <id> [flags]    (defines or overlays <id>)
//	agent set <id> [flags]    (overlays an existing <id>; no --role/--runtime)
//	agent remove <id>         (alias: rm; built-ins are removed with
//	                          `agent set <id> --disabled true`)
//
// Definitions write into the agents section, the same JSON shape the
// config's agent validator checks at load time; unknown tools, role
// changes and other semantic errors are that validator's job.
func handleAgent(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	b := configBuilderFromCtx(ctx)
	if b == nil {
		return nil
	}
	if len(args) < 2 {
		return usage(stderr, "usage: agent defaults [flags] | agent add <id> [flags] | agent set <id> [flags] | agent remove <id>")
	}

	switch args[1] {
	case "defaults":
		return agentDefaults(b, args, stderr)
	case "add":
		return agentAdd(b, args, stderr)
	case "set":
		return agentSet(b, args, stderr)
	case "remove", "rm":
		return agentRemove(b, args, stderr)
	default:
		return usage(stderr, fmt.Sprintf("agent: unknown subcommand %q (expected defaults, add, set, or remove)", args[1]))
	}
}

// parseAgentModel stores --model: the large/small slot as a string, or a
// provider/model pin as an object, split on the first slash only.
func parseAgentModel(args []string, i int) (any, int, error) {
	v, err := nextArg(args, i, "model")
	if err != nil {
		return nil, 0, err
	}
	switch v {
	case "large", "small":
		return v, i + 2, nil
	}
	provider, model, _ := strings.Cut(v, "/")
	if provider == "" || model == "" {
		return nil, 0, fmt.Errorf("%s: --model expects large, small, or provider/model, got %q", args[0], v)
	}
	return map[string]any{"provider": provider, "model": model}, i + 2, nil
}

// parseAgentPrompt stores --prompt: builtin: and file: references pass
// through, a bare path is stored as a file: reference.
func parseAgentPrompt(args []string, i int) (any, int, error) {
	v, err := nextArg(args, i, "prompt")
	if err != nil {
		return nil, 0, err
	}
	if v == "" {
		return nil, 0, fmt.Errorf("%s: --prompt requires a value", args[0])
	}
	if strings.HasPrefix(v, "builtin:") || strings.HasPrefix(v, "file:") {
		return v, i + 2, nil
	}
	return "file:" + v, i + 2, nil
}

// parseAgentPromptAppend stores --prompt-append as a file: reference.
func parseAgentPromptAppend(args []string, i int) (any, int, error) {
	v, err := nextArg(args, i, "prompt-append")
	if err != nil {
		return nil, 0, err
	}
	if v == "" {
		return nil, 0, fmt.Errorf("%s: --prompt-append requires a value", args[0])
	}
	if strings.HasPrefix(v, "file:") {
		return v, i + 2, nil
	}
	return "file:" + v, i + 2, nil
}

// parseAgentBearer stores --bearer as the auth block: the value must be a
// $VAR reference, so a secret never sits in crushrc or the merged config.
func parseAgentBearer(args []string, i int) (any, int, error) {
	v, err := nextArg(args, i, "bearer")
	if err != nil {
		return nil, 0, err
	}
	if !strings.HasPrefix(v, "$") {
		return nil, 0, fmt.Errorf("%s: --bearer expects a $VAR reference, as in --bearer '$VAR', not a literal token", args[0])
	}
	return map[string]any{"type": "bearer", "token": v}, i + 2, nil
}

// agentFlags is the declarative flag surface shared by `agent add` and
// `agent set`.
var agentFlags = []flagSpec{
	{name: "--role", jsonKey: "role", kind: flagString, op: opSet},
	{name: "--runtime", jsonKey: "runtime", kind: flagString, op: opSet},
	{name: "--name", jsonKey: "name", kind: flagString, op: opSet},
	{name: "--description", jsonKey: "description", kind: flagString, op: opSet},
	{name: "--disabled", jsonKey: "disabled", kind: flagBool, op: opSet},
	{name: "--model", jsonKey: "model", kind: flagString, op: opSet, parse: parseAgentModel},
	{name: "--prompt", jsonKey: "prompt", kind: flagString, op: opSet, parse: parseAgentPrompt},
	{name: "--prompt-append", jsonKey: "prompt_append", kind: flagString, op: opSet, parse: parseAgentPromptAppend},
	{name: "--workspace", jsonKey: "workspace", kind: flagString, op: opSet},
	{name: "--card", jsonKey: "card", kind: flagString, op: opSet},
	{name: "--tools", jsonKey: "allow", child: "tools", kind: flagStringList, op: opSetChildValue},
	{name: "--deny-tool", jsonKey: "deny", child: "tools", kind: flagString, op: opAppendChild},
	{name: "--mcp", jsonKey: "allow", child: "mcp", kind: flagStringList, op: opSetChildValue},
	{name: "--skill", jsonKey: "skills", kind: flagString, op: opAppend},
	{name: "--context-path", jsonKey: "context_paths", kind: flagString, op: opAppend},
	{name: "--nudge", jsonKey: "nudge", child: "todos", kind: flagBool, op: opSetChildValue},
	{name: "--nudge-after", jsonKey: "nudge_after_tool_calls", child: "todos", kind: flagCountOrOff, op: opSetChildValue},
	{name: "--hard-gate", jsonKey: "hard_gate", child: "todos", kind: flagBool, op: opSetChildValue},
	{name: "--kill-after-nudges", jsonKey: "after_ignored_nudges", child: "kill", kind: flagCountOrOff, op: opSetChildValue},
	{name: "--stall", jsonKey: "stall", child: "kill", kind: flagDurationOrOff, op: opSetChildValue},
	{name: "--timeout", jsonKey: "timeout", child: "kill", kind: flagDurationOrOff, op: opSetChildValue},
	{name: "--bearer", jsonKey: "auth", child: "auth", kind: flagString, op: opMergeChild, parse: parseAgentBearer},
	{name: "--idle-timeout", jsonKey: "idle_timeout", child: "transport", kind: flagDurationOrOff, op: opSetChildValue},
}

// agentDefaultsFlags is the flag surface for `agent defaults`: only the
// todos and kill flags, the only fields the $defaults key accepts.
var agentDefaultsFlags = filterAgentFlags(func(s flagSpec) bool {
	return s.child == "todos" || s.child == "kill"
})

// agentSetFlags is the flag surface for `agent set`: everything but
// --role and --runtime, which would change what an existing agent is.
var agentSetFlags = filterAgentFlags(func(s flagSpec) bool {
	return s.name != "--role" && s.name != "--runtime"
})

func filterAgentFlags(keep func(flagSpec) bool) []flagSpec {
	var out []flagSpec
	for _, s := range agentFlags {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

func agentDefaults(b *ConfigBuilder, args []string, stderr io.Writer) error {
	target := childMap(b.section("agents"), "$defaults")
	if err := applyFlags(agentDefaultsFlags, args, 2, target, "agent defaults", stderr); err != nil {
		return err
	}
	slog.Info("Agent defaults set in shell config")
	return nil
}

func agentAdd(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: agent add <id> [flags]")
	}
	id := args[2]
	m := childMap(b.section("agents"), id)
	if err := applyFlags(agentFlags, args, 3, m, "agent add", stderr); err != nil {
		return err
	}
	slog.Info("Agent defined in shell config", "id", id)
	return nil
}

func agentSet(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: agent set <id> [flags]")
	}
	id := args[2]
	section := b.section("agents")
	if _, ok := section[id]; !ok && !slices.Contains(builtinAgentIDs, id) {
		return usage(stderr, fmt.Sprintf("agent set: unknown agent %q (define it first with agent add)", id))
	}
	m := childMap(section, id)
	if err := applyFlags(agentSetFlags, args, 3, m, "agent set", stderr); err != nil {
		return err
	}
	slog.Info("Agent updated in shell config", "id", id)
	return nil
}

func agentRemove(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: agent remove <id>")
	}
	id := args[2]
	if slices.Contains(builtinAgentIDs, id) {
		return usage(stderr, fmt.Sprintf("agent remove: %q is a built-in agent; disable it with `agent set %s --disabled true`", id, id))
	}
	delete(b.section("agents"), id)
	slog.Info("Agent removed in shell config", "id", id)
	return nil
}
