package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"mvdan.cc/sh/v3/syntax"
)

// SidekickBashBlockedMessage prefixes every error returned when the Sidekick
// read-only filter rejects a command.
const SidekickBashBlockedMessage = "Sidekick cannot run write commands"

//go:embed sidekick_bash.md.tpl
var sidekickBashDescriptionTmpl []byte

var sidekickBashDescriptionTpl = template.Must(
	template.New("sidekickBashDescription").
		Parse(string(sidekickBashDescriptionTmpl)),
)

type sidekickBashDescriptionData struct {
	AllowedCommands string
	MaxOutputLength int
}

// sidekickAllowedCommands is the read-only command allowlist. Multi-word
// entries match command prefixes ("git log" allows `git log --oneline` but
// not `git push`); single words match the command name alone.
var sidekickAllowedCommands = []string{
	// File and text inspection
	"[",
	"awk",
	"base64",
	"basename",
	"cat",
	"cksum",
	"column",
	"comm",
	"cut",
	"diff",
	"dirname",
	"echo",
	"egrep",
	"expr",
	"false",
	"fgrep",
	"file",
	"find",
	"fold",
	"grep",
	"head",
	"hexdump",
	"jq",
	"less",
	"ls",
	"md5sum",
	"more",
	"nl",
	"od",
	"printf",
	"pwd",
	"readlink",
	"realpath",
	"rg",
	"sed",
	"seq",
	"sha1sum",
	"sha256sum",
	"sha512sum",
	"sleep",
	"sort",
	"stat",
	"strings",
	"tac",
	"tail",
	"test",
	"tr",
	"tree",
	"true",
	"uniq",
	"wc",
	"xxd",
	"yq",

	// System information
	"cal",
	"date",
	"df",
	"du",
	"env",
	"free",
	"groups",
	"hostname",
	"id",
	"printenv",
	"ps",
	"type",
	"uname",
	"uptime",
	"whatis",
	"whereis",
	"which",
	"whoami",

	// Crush and toolchain introspection
	"crush --help",
	"crush --version",
	"crush -v",
	"go env",
	"go list",
	"go version",

	// Git (read-only subcommands, mirroring safeCommands)
	"git blame",
	"git branch",
	"git config --get",
	"git config --list",
	"git describe",
	"git diff",
	"git grep",
	"git log",
	"git ls-files",
	"git ls-remote",
	"git remote",
	"git rev-parse",
	"git shortlog",
	"git show",
	"git status",
	"git tag",
}

// sidekickDeniedArgs lists argument prefixes that turn an otherwise
// read-only command into a write: sed -i edits in place, find -delete/-exec
// mutate or execute, sort -o and git --output write files, go env -w writes
// the Go configuration.
var sidekickDeniedArgs = map[string][]string{
	"find": {"-delete", "-exec", "-fls", "-fprint", "-ok"},
	"git":  {"--output"},
	"go":   {"-w"},
	"sed":  {"-i", "--in-place"},
	"sort": {"-o", "--output"},
}

// sidekickBlockReason statically analyzes a shell command and returns a
// human-readable reason it must be blocked for the Sidekick, or "" when
// every command in it is read-only. It is a pattern matcher on the command
// string, not a sandbox: it walks the parsed shell AST, requires each
// command name to be a literal on the allowlist, and rejects output
// redirection and function definitions (which could shadow allowed names).
func sidekickBlockReason(command string) string {
	prog, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return "command could not be parsed"
	}
	var reason string
	syntax.Walk(prog, func(node syntax.Node) bool {
		if reason != "" {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			reason = sidekickCallReason(n)
		case *syntax.Redirect:
			reason = sidekickRedirectReason(n)
		case *syntax.FuncDecl:
			reason = "function definitions are not allowed"
		}
		return true
	})
	return reason
}

func sidekickCallReason(call *syntax.CallExpr) string {
	if len(call.Args) == 0 {
		// Pure variable assignment (FOO=bar); shells are independent per
		// call so nothing persists.
		return ""
	}
	words := make([]string, len(call.Args))
	for i, arg := range call.Args {
		// Lit is "" for anything not fully literal ($VAR, $(cmd), quotes).
		words[i] = arg.Lit()
	}
	if words[0] == "" {
		return "dynamic command names are not allowed"
	}
	if !sidekickCommandAllowed(words) {
		return fmt.Sprintf("%q is not on the read-only command allowlist", strings.Join(nonEmptyPrefix(words, 2), " "))
	}
	for _, denied := range sidekickDeniedArgs[words[0]] {
		for _, arg := range words[1:] {
			if strings.HasPrefix(arg, denied) {
				return fmt.Sprintf("%q is not allowed with %q", words[0], denied)
			}
		}
	}
	return ""
}

func sidekickCommandAllowed(words []string) bool {
	for _, entry := range sidekickAllowedCommands {
		parts := strings.Fields(entry)
		if len(parts) > len(words) {
			continue
		}
		match := true
		for i, part := range parts {
			if words[i] != part {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// nonEmptyPrefix returns up to max leading words, stopping at the first
// non-literal (empty) one, for use in error messages.
func nonEmptyPrefix(words []string, max int) []string {
	out := make([]string, 0, max)
	for _, w := range words {
		if w == "" || len(out) == max {
			break
		}
		out = append(out, w)
	}
	return out
}

func sidekickRedirectReason(redir *syntax.Redirect) string {
	switch redir.Op {
	case syntax.RdrIn, syntax.DplIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		// Reading input is fine.
		return ""
	case syntax.DplOut:
		// Duplicating onto another file descriptor (2>&1, >&-) is fine;
		// >&file writes a file.
		if redir.Word != nil {
			if w := redir.Word.Lit(); w == "-" || isAllDigits(w) {
				return ""
			}
		}
		return "output redirection is not allowed"
	default:
		return "output redirection is not allowed"
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sidekickBashTool wraps the standard bash tool and rejects any command
// that is not provably read-only before delegating to it.
type sidekickBashTool struct {
	fantasy.AgentTool
	description string
}

func (t *sidekickBashTool) Info() fantasy.ToolInfo {
	info := t.AgentTool.Info()
	info.Description = t.description
	return info
}

func (t *sidekickBashTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var params BashParams
	if err := json.Unmarshal([]byte(call.Input), &params); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("error parsing parameters: %s", err)), nil
	}
	if params.Command == "" {
		return fantasy.NewTextErrorResponse("missing command"), nil
	}
	if reason := sidekickBlockReason(params.Command); reason != "" {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("%s: %s", SidekickBashBlockedMessage, reason)), nil
	}
	return t.AgentTool.Run(ctx, call)
}

// NewSidekickBashTool returns the Sidekick variant of the bash tool: same
// name and parameters, but commands are filtered against a read-only
// allowlist before execution. Write and mutation commands fail with
// [SidekickBashBlockedMessage] instead of running. The Sidekick's agent is
// built with this tool in place of the standard bash tool.
func NewSidekickBashTool(permissions permission.Service, workingDir string, attribution *config.Attribution, modelID string) fantasy.AgentTool {
	return &sidekickBashTool{
		AgentTool: NewBashTool(permissions, workingDir, attribution, modelID, nil, false),
		description: renderTemplate(sidekickBashDescriptionTpl, sidekickBashDescriptionData{
			AllowedCommands: strings.Join(sidekickAllowedCommands, ", "),
			MaxOutputLength: MaxOutputLength,
		}),
	}
}
