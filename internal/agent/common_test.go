package agent

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/x/vcr"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"

	_ "github.com/joho/godotenv/autoload"
)

// fakeEnv is an environment for testing.
type fakeEnv struct {
	workingDir  string
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	history     history.Service
	filetracker *filetracker.Service
	lspClients  *csync.Map[string, *lsp.Client]
}

type builderFunc func(t *testing.T, r *vcr.Recorder) (fantasy.LanguageModel, error)

type modelPair struct {
	name       string
	largeModel builderFunc
	smallModel builderFunc
}

func hyperBuilder(model string) builderFunc {
	return func(t *testing.T, r *vcr.Recorder) (fantasy.LanguageModel, error) {
		provider, err := openaicompat.New(
			openaicompat.WithBaseURL("https://hyper.charm.land/v1"),
			openaicompat.WithAPIKey(os.Getenv("CRUSH_HYPER_API_KEY")),
			openaicompat.WithHTTPClient(&http.Client{Transport: r}),
		)
		if err != nil {
			return nil, err
		}
		return provider.LanguageModel(t.Context(), model)
	}
}

func testEnv(t *testing.T) fakeEnv {
	dir := t.TempDir()
	t.Cleanup(func() { flakeDiagRemove(t, dir) })
	return testEnvAtDir(t, dir)
}

// flakeDiagRemove is a throwaway diagnostic: it removes dir just before
// t.TempDir's own cleanup would, and on failure reports what was left
// under it and which processes still hold it.
func flakeDiagRemove(t *testing.T, dir string) {
	// Any git still running against dir at cleanup is the race, whether
	// or not this RemoveAll happens to lose it.
	if ps, _ := exec.Command("sh", "-c", "ps -axo pid,ppid,etime,command | grep -F '"+dir+"' | grep -v grep").Output(); len(ps) > 0 {
		buf := make([]byte, 1<<22)
		n := runtime.Stack(buf, true)
		var stacks []string
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, "os/exec.(*Cmd)") {
				stacks = append(stacks, g)
			}
		}
		fmt.Fprintf(os.Stderr, "FLAKEDIAG-LINGER %s:\nps:\n%s\ngoroutines in exec:\n%s\n", t.Name(), ps, strings.Join(stacks, "\n\n"))
	}
	err := os.RemoveAll(dir)
	if err == nil {
		return
	}
	var left []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, _ error) error {
		if info != nil {
			left = append(left, fmt.Sprintf("%s %s", strings.TrimPrefix(p, dir), info.ModTime().Format("15:04:05.000000")))
		}
		return nil
	})
	ps, _ := exec.Command("sh", "-c", "ps -axo pid,ppid,etime,command | grep -E 'git|crush|agent.test' | grep -v grep").Output()
	lsof, _ := exec.Command("lsof", "+D", dir).Output()
	fmt.Fprintf(os.Stderr, "FLAKEDIAG %s at %s: %v\nleft:\n  %s\nps:\n%s\nlsof:\n%s\n",
		t.Name(), time.Now().Format("15:04:05.000000"), err, strings.Join(left, "\n  "), ps, lsof)
}

// testEnvFixedDir is testEnv rooted at the pre-#422 fixed directory. The
// VCR cassettes under testdata/TestCoderAgent/** record
// "Working directory: /tmp/crush-test/..." in the request body, and
// charm.land/x/vcr matches request bodies exactly, so setupAgent must
// keep replaying against this path.
func testEnvFixedDir(t *testing.T) fakeEnv {
	t.Helper()
	workingDir := filepath.Join("/tmp/crush-test/", t.Name())
	os.RemoveAll(workingDir)
	require.NoError(t, os.MkdirAll(workingDir, 0o755))
	env := testEnvAtDir(t, workingDir)
	t.Cleanup(func() { os.RemoveAll(workingDir) })
	return env
}

// testEnvAtDir builds a fakeEnv working in workingDir. t.TempDir()
// cleanups remove the directory only after every t.Cleanup has run, so a
// background run a test started still finds its directory until it ends
// (#422).
func testEnvAtDir(t *testing.T, workingDir string) fakeEnv {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	permissions := permission.NewPermissionService(workingDir, true, []string{})
	history := history.NewService(q, conn)
	filetrackerService := filetracker.NewService(q)
	lspClients := csync.NewMap[string, *lsp.Client]()

	t.Cleanup(func() {
		conn.Close()
	})

	return fakeEnv{
		workingDir,
		sessions,
		messages,
		permissions,
		history,
		&filetrackerService,
		lspClients,
	}
}

func testSessionAgent(env fakeEnv, large, small fantasy.LanguageModel, systemPrompt string, tools ...fantasy.AgentTool) SessionAgent {
	largeModel := Model{
		Model: large,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
	}
	smallModel := Model{
		Model: small,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
	}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel:   largeModel,
		SmallModel:   smallModel,
		SystemPrompt: systemPrompt,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
		Tools:        tools,
	})
	return agent
}

func coderAgent(r *vcr.Recorder, env fakeEnv, large, small fantasy.LanguageModel) (SessionAgent, error) {
	fixedTime := func() time.Time {
		t, _ := time.Parse("1/2/2006", "1/1/2025")
		return t
	}
	prompt, err := coderPrompt(
		prompt.WithTimeFunc(fixedTime),
		prompt.WithPlatform("linux"),
		prompt.WithWorkingDir(filepath.ToSlash(env.workingDir)),
	)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Init(env.workingDir, "", false)
	if err != nil {
		return nil, err
	}

	// NOTE(@andreynering): Set a fixed config to ensure cassettes match
	// independently of user config on `$HOME/.config/crush/crush.json`.
	cfg.Config().Options.Attribution = &config.Attribution{
		TrailerStyle:  "co-authored-by",
		GeneratedWith: true,
	}

	// Clear some fields to avoid issues with VCR cassette matching.
	cfg.Config().Options.SkillsPaths = nil
	cfg.Config().Options.DisabledSkills = []string{"crush-config"}
	cfg.Config().Options.ContextPaths = nil
	cfg.Config().Options.GlobalContextPaths = nil
	cfg.Config().LSP = nil

	systemPrompt, err := prompt.Build(context.TODO(), large.Provider(), large.Model(), cfg)
	if err != nil {
		return nil, err
	}

	// Get the model name for the bash tool
	modelName := large.Model() // fallback to ID if Name not available
	if model := cfg.Config().GetModel(large.Provider(), large.Model()); model != nil {
		modelName = model.Name
	}

	allTools := []fantasy.AgentTool{
		tools.NewBashTool(env.permissions, env.workingDir, cfg.Config().Options.DataDirectory, cfg.Config().Options.Attribution, modelName, cfg.Config().Options.AllowedCommands, cfg.Config().Options.AllowAllCommands),
		tools.NewDownloadTool(env.permissions, env.workingDir, r.GetDefaultClient()),
		tools.NewEditTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
		tools.NewMultiEditTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
		tools.NewFetchTool(env.permissions, env.workingDir, r.GetDefaultClient()),
		tools.NewGlobTool(env.workingDir, cfg.Config().Tools.Glob),
		tools.NewGrepTool(env.workingDir, cfg.Config().Tools.Grep),
		tools.NewLsTool(env.permissions, env.workingDir, cfg.Config().Tools.Ls),
		tools.NewSourcegraphTool(r.GetDefaultClient()),
		tools.NewViewTool(nil, env.permissions, *env.filetracker, nil, env.workingDir),
		tools.NewWriteTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
	}

	return testSessionAgent(env, large, small, systemPrompt, allTools...), nil
}

// createSimpleGoProject creates a simple Go project structure in the given directory.
// It creates a go.mod file and a main.go file with a basic hello world program.
func createSimpleGoProject(t *testing.T, dir string) {
	goMod := `module example.com/testproject

go 1.23
`
	err := os.WriteFile(dir+"/go.mod", []byte(goMod), 0o644)
	require.NoError(t, err)

	mainGo := `package main

import "fmt"

func main() {
	fmt.Println("Hello, World!")
}
`
	err = os.WriteFile(dir+"/main.go", []byte(mainGo), 0o644)
	require.NoError(t, err)
}
