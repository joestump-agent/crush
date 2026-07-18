package tools

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func newSidekickBashToolForTest(workingDir string) fantasy.AgentTool {
	permissions := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	return NewSidekickBashTool(permissions, workingDir, attribution, "test-model")
}

func TestSidekickBlockReason_Allowed(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"ls -la",
		"cat crush.json",
		"grep -rn 'foo' .",
		"find . -name '*.go'",
		"head -20 main.go",
		"tail -n 5 log.txt",
		"wc -l main.go",
		"ps aux",
		"env",
		"pwd",
		"crush --version",
		"go version",
		"git log --oneline -10",
		"git status",
		"git diff HEAD~1",
		"git config --get user.name",
		"git log | head -20",
		"ps aux | grep crush",
		"sort file.txt | uniq -c | head",
		"cat a.txt b.txt | wc -l",
		"echo hi 2>&1",
		"ls; git status",
		"git log && git status",
		"grep foo main.go || echo missing",
		"for f in *.go; do wc -l \"$f\"; done",
		"test -f crush.json",
		"[ -f crush.json ]",
		"FOO=bar env",
		"sed 's/a/b/' file.txt",
		"diff a.txt b.txt",
		"wc -l < main.go",
	}
	for _, cmd := range allowed {
		require.Empty(t, sidekickBlockReason(cmd), "expected %q to be allowed", cmd)
	}
}

func TestSidekickBlockReason_Blocked(t *testing.T) {
	t.Parallel()

	blocked := []string{
		// Plain mutation commands.
		"rm -rf /",
		"mv a b",
		"cp a b",
		"mkdir x",
		"chmod +x script.sh",
		"chown root file",
		"touch file",
		"kill -9 123",
		"pkill crush",
		"tee out.txt",
		"vim main.go",
		"apt install cowsay",
		"curl https://example.com",
		"wget https://example.com",
		// Git write subcommands.
		"git push",
		"git commit -m msg",
		"git checkout main",
		"git reset --hard",
		"git config user.name joe",
		// Output redirection.
		"echo hi > out.txt",
		"cat a >> b",
		"echo hi &> out.txt",
		"ls 2> err.txt",
		"echo hi >& out.txt",
		"sort <> file.txt",
		// Write flags on otherwise read-only commands.
		"sed -i 's/a/b/' file.txt",
		"sed --in-place 's/a/b/' file.txt",
		"find . -delete",
		"find . -exec rm {} \\;",
		"find . -execdir rm {} \\;",
		"sort -o out.txt in.txt",
		"git log --output=f",
		"go env -w GOFLAGS=-mod=mod",
		// Blocked commands hidden inside allowed constructs.
		"ls; rm -rf /",
		"git log && rm x",
		"cat $(rm -rf /)",
		"echo `rm x`",
		"(rm x)",
		"ls | xargs rm",
		"FOO=$(rm x) ls",
		// Shadowing and dynamic command names.
		"ls() { rm -rf /; }; ls",
		"$CMD",
		"\"ls\" -la",
		// Bare git could be anything with a dynamic subcommand.
		"git $SUB",
		// Unparseable input.
		"ls 'unterminated",
	}
	for _, cmd := range blocked {
		require.NotEmpty(t, sidekickBlockReason(cmd), "expected %q to be blocked", cmd)
	}
}

func TestSidekickBashTool_BlockedCommand(t *testing.T) {
	workingDir := t.TempDir()
	tool := newSidekickBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "delete everything",
		Command:     "rm -rf /",
	})

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, SidekickBashBlockedMessage)
}

func TestSidekickBashTool_AllowedCommandExecutes(t *testing.T) {
	workingDir := t.TempDir()
	tool := newSidekickBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "say hello",
		Command:     "echo hello sidekick",
	})

	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "hello sidekick")
}

func TestSidekickBashTool_MissingCommand(t *testing.T) {
	workingDir := t.TempDir()
	tool := newSidekickBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{Description: "empty"})

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "missing command")
}

func TestSidekickBashTool_Info(t *testing.T) {
	t.Parallel()

	tool := newSidekickBashToolForTest(t.TempDir())
	info := tool.Info()

	require.Equal(t, BashToolName, info.Name)
	require.Contains(t, info.Description, "READ-ONLY")
	require.Contains(t, info.Description, "git log")
	// Parameters come from the wrapped bash tool.
	require.Contains(t, info.Parameters, "command")
}
