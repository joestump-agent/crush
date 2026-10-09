package prompt

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	a2ui "github.com/tmc/a2ui"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/gitutil"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/skills"
)

// Prompt represents a template-based prompt generator.
type Prompt struct {
	name         string
	template     string
	now          func() time.Time
	platform     string
	workingDir   string
	contextRoot  string
	skillFilter  []string
	contextPaths []string
	a2ui         bool
	scheduling   bool
}

type PromptDat struct {
	Provider           string
	Model              string
	Config             config.Config
	WorkingDir         string
	IsGitRepo          bool
	Platform           string
	Date               string
	Time               string
	GitStatus          string
	ContextFiles       []ContextFile
	GlobalContextFiles []ContextFile
	AvailSkillXML      string
	A2UI               bool
	A2UIVersion        string
	Scheduling         bool
}

type ContextFile struct {
	Path    string
	Content string
}

type Option func(*Prompt)

func WithTimeFunc(fn func() time.Time) Option {
	return func(p *Prompt) {
		p.now = fn
	}
}

func WithPlatform(platform string) Option {
	return func(p *Prompt) {
		p.platform = platform
	}
}

func WithWorkingDir(workingDir string) Option {
	return func(p *Prompt) {
		p.workingDir = workingDir
	}
}

// WithSkills restricts the available-skills section of the rendered
// prompt to the named skills. The default (nil or empty) includes every
// discovered skill. Dispatched agents take their skill set per dispatch
// (#64's skills parameter) through this option; unknown names are
// dropped, so callers validate them first.
func WithSkills(names []string) Option {
	return func(p *Prompt) {
		p.skillFilter = names
	}
}

// WithContextPaths replaces the config's context_paths for this
// prompt's render. An agent definition that sets context_paths (#432)
// renders the agent's own context files instead of the global options'
// paths; the default (nil or empty) keeps the config's paths.
func WithContextPaths(paths []string) Option {
	return func(p *Prompt) {
		p.contextPaths = paths
	}
}

// WithContextRoot sets the directory relative context paths resolve
// against — the project's and the agent definition's context_paths
// alike, and any relative global_context_paths. The default (empty) is
// the store's working directory, which is where every prompt but a
// dispatched agent's reads its notes. A dispatch renders against a
// store whose working directory is a worktree cut from a revision the
// model chose, so its context files must instead come from the
// parent's checkout, the one the user trusts (#561): the dispatched
// agent still works inside the worktree, only its project notes are
// read elsewhere.
func WithContextRoot(dir string) Option {
	return func(p *Prompt) {
		p.contextRoot = dir
	}
}

// WithA2UI enables the template's A2UI section, which tells the model it may
// emit <a2ui-json> surfaces. Crush's chat TUI renders those blocks via a2tea;
// consumers that only ever see plain text (scripted crush run, headless serve
// clients) receive them as raw markup, so deployments that need plain-text
// output can turn the section off with options.disable_a2ui — the coordinator
// applies that setting.
func WithA2UI() Option {
	return func(p *Prompt) {
		p.a2ui = true
	}
}

// WithScheduling enables the template's scheduling guidance, which tells the
// model to reach for the CronCreate / CronList / CronDelete tools instead of
// bash sleep loops. Guidance follows the tool: callers that build a coder
// prompt without registering the cron tools — the agent package's own tests
// among them — leave it off, so the model is never pointed at a tool it does
// not have.
func WithScheduling() Option {
	return func(p *Prompt) {
		p.scheduling = true
	}
}

// filterSkillsByName keeps only the skills whose names are in names.
func filterSkillsByName(all []*skills.Skill, names []string) []*skills.Skill {
	var filtered []*skills.Skill
	for _, s := range all {
		for _, name := range names {
			if s.Name == name {
				filtered = append(filtered, s)
				break
			}
		}
	}
	return filtered
}

func NewPrompt(name, promptTemplate string, opts ...Option) (*Prompt, error) {
	p := &Prompt{
		name:     name,
		template: promptTemplate,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *Prompt) Build(ctx context.Context, provider, model string, store *config.ConfigStore) (string, error) {
	t, err := template.New(p.name).Parse(p.template)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var sb strings.Builder
	d, err := p.promptData(ctx, provider, model, store)
	if err != nil {
		return "", err
	}
	if err := t.Execute(&sb, d); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return sb.String(), nil
}

func processFile(filePath string) *ContextFile {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	return &ContextFile{
		Path:    filePath,
		Content: string(content),
	}
}

// processContextPath reads the context file or directory at p, with a
// relative p resolved against root.
func processContextPath(p, root string) []ContextFile {
	var contexts []ContextFile
	fullPath := filepathext.SmartJoin(root, p)
	info, err := os.Stat(fullPath)
	if err != nil {
		return contexts
	}
	if info.IsDir() {
		filepath.WalkDir(fullPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				if result := processFile(path); result != nil {
					contexts = append(contexts, *result)
				}
			}
			return nil
		})
	} else {
		result := processFile(fullPath)
		if result != nil {
			contexts = append(contexts, *result)
		}
	}
	return contexts
}

// expandPath expands ~ and environment variables in file paths
func expandPath(path string, store *config.ConfigStore) string {
	path = home.Long(path)
	// Handle environment variable expansion using the same pattern as config
	if strings.HasPrefix(path, "$") {
		if expanded, err := store.Resolver().ResolveValue(path); err == nil {
			path = expanded
		}
	}

	return path
}

// loadContextFiles loads and deduplicates context files from a list of
// paths. Relative paths resolve against root; the store only expands
// variables in them.
func loadContextFiles(paths []string, root string, store *config.ConfigStore) map[string][]ContextFile {
	files := map[string][]ContextFile{}
	for _, pth := range paths {
		expanded := expandPath(pth, store)
		pathKey := strings.ToLower(expanded)
		if _, ok := files[pathKey]; ok {
			continue
		}
		files[pathKey] = processContextPath(expanded, root)
	}
	return files
}

func (p *Prompt) promptData(ctx context.Context, provider, model string, store *config.ConfigStore) (PromptDat, error) {
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	platform := cmp.Or(p.platform, runtime.GOOS)
	// Context files come from the store's working directory unless the
	// caller rooted them elsewhere (WithContextRoot, #561).
	contextRoot := cmp.Or(p.contextRoot, store.WorkingDir())

	cfg := store.Config()
	contextPaths := cfg.Options.ContextPaths
	if len(p.contextPaths) > 0 {
		contextPaths = p.contextPaths
	}
	contextFiles := loadContextFiles(contextPaths, contextRoot, store)
	globalContextFiles := loadContextFiles(cfg.Options.GlobalContextPaths, contextRoot, store)

	// Discover and load skills metadata.
	var availSkillXML string

	// Start with builtin skills.
	allSkills := skills.DiscoverBuiltin()
	builtinNames := make(map[string]bool, len(allSkills))
	for _, s := range allSkills {
		builtinNames[s.Name] = true
	}

	// Discover user skills from configured paths.
	if len(cfg.Options.SkillsPaths) > 0 {
		expandedPaths := make([]string, 0, len(cfg.Options.SkillsPaths))
		for _, pth := range cfg.Options.SkillsPaths {
			expandedPaths = append(expandedPaths, expandPath(pth, store))
		}
		for _, userSkill := range skills.Discover(expandedPaths) {
			if builtinNames[userSkill.Name] {
				slog.Warn("User skill overrides builtin skill", "name", userSkill.Name)
			}
			allSkills = append(allSkills, userSkill)
		}
	}

	// Deduplicate: user skills override builtins with the same name.
	allSkills = skills.Deduplicate(allSkills)

	// Filter out disabled skills.
	allSkills = skills.Filter(allSkills, cfg.Options.DisabledSkills)

	// Restrict to the caller's skill set when one was given.
	if len(p.skillFilter) > 0 {
		allSkills = filterSkillsByName(allSkills, p.skillFilter)
	}

	if len(allSkills) > 0 {
		availSkillXML = skills.ToPromptXML(allSkills)
	}

	isGit := isGitRepo(store.WorkingDir())
	data := PromptDat{
		Provider:      provider,
		Model:         model,
		Config:        *cfg,
		WorkingDir:    filepath.ToSlash(workingDir),
		IsGitRepo:     isGit,
		Platform:      platform,
		Date:          p.now().Format("1/2/2006"),
		Time:          p.now().Format("3:04:05 PM MST"),
		AvailSkillXML: availSkillXML,
		A2UI:          p.a2ui,
		A2UIVersion:   a2ui.Version,
		Scheduling:    p.scheduling,
	}
	if isGit {
		var err error
		data.GitStatus, err = getGitStatus(ctx, store.WorkingDir())
		if err != nil {
			return PromptDat{}, err
		}
	}

	for _, files := range contextFiles {
		data.ContextFiles = append(data.ContextFiles, files...)
	}
	for _, files := range globalContextFiles {
		data.GlobalContextFiles = append(data.GlobalContextFiles, files...)
	}
	return data, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func getGitStatus(ctx context.Context, dir string) (string, error) {
	sh := shell.NewShell(&shell.Options{
		WorkingDir: dir,
	})
	branch := getGitBranch(dir)
	status, err := getGitStatusSummary(ctx, sh)
	if err != nil {
		return "", err
	}
	commits, err := getGitRecentCommits(ctx, sh)
	if err != nil {
		return "", err
	}
	return branch + status + commits, nil
}

// getGitBranch reads the checked-out branch straight from the repository.
// This runs on every prompt build, so it stays off the shell: spawning git
// each turn is wasted work, and a detail of prompt assembly has no business
// passing through command blocking and permission policy.
func getGitBranch(dir string) string {
	branch := gitutil.CurrentBranch(dir)
	if branch == "" {
		return ""
	}
	return fmt.Sprintf("Current branch: %s\n", branch)
}

func getGitStatusSummary(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git status --short 2>/dev/null | head -20")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "Status: clean\n", nil
	}
	return fmt.Sprintf("Status:\n%s\n", out), nil
}

func getGitRecentCommits(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git log --oneline -n 3 2>/dev/null")
	if err != nil || out == "" {
		return "", nil
	}
	out = strings.TrimSpace(out)
	return fmt.Sprintf("Recent commits:\n%s\n", out), nil
}

func (p *Prompt) Name() string {
	return p.name
}
