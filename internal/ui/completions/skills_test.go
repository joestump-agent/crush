package completions

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestSetSkillItemsOpensWithSkills(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	require.False(t, c.IsOpen())

	c.SetSkillItems([]SkillCompletionValue{
		{Name: "code-review", Path: "/skills/code-review/SKILL.md"},
		{Name: "commit", Path: "/skills/commit/SKILL.md"},
	}, nil)

	require.True(t, c.IsOpen())
	require.Len(t, c.filtered, 2)
	first, ok := c.filtered[0].(*CompletionItem)
	require.True(t, ok)
	require.Equal(t, "/code-review", first.Text())
}

func TestSkillCompletionFilter(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems([]SkillCompletionValue{
		{Name: "code-review"},
		{Name: "commit"},
		{Name: "coder"},
	}, nil)

	c.Filter("cod")

	require.NotEmpty(t, c.filtered)
	first, ok := c.filtered[0].(*CompletionItem)
	require.True(t, ok)
	// "coder" is an exact-stem/prefix tier winner over "code-review".
	require.Equal(t, "/coder", first.Text())
}

func TestSkillDescriptionShownAndSearchable(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems([]SkillCompletionValue{
		{Name: "skill-creator", Description: "Use for naming skills\nand writing frontmatter."},
		{Name: "commit", Description: "Write a conventional commit."},
	}, nil)

	first, ok := c.filtered[0].(*CompletionItem)
	require.True(t, ok)
	// The row reads "/name - description", with the frontmatter
	// description flattened onto one line.
	require.Equal(t, "/skill-creator - Use for naming skills and writing frontmatter.", first.Text())
	// The name leads; the description renders as dimmed detail.
	require.Equal(t, len("/skill-creator"), first.detailStart)
	require.Equal(t, "/skill-creator", first.SortKey())

	// The description is part of the filter text, so a skill is findable by
	// what it does and not only by what it is named.
	c.Filter("frontmatter")
	require.Len(t, c.filtered, 1)
	hit, ok := c.filtered[0].(*CompletionItem)
	require.True(t, ok)
	require.Equal(t, "skill-creator", hit.Value().(SkillCompletionValue).Name)
}

func TestSkillDescriptionTruncatedToPopupWidth(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems([]SkillCompletionValue{
		{Name: "commit", Description: strings.Repeat("long ", 200)},
	}, nil)

	first, ok := c.filtered[0].(*CompletionItem)
	require.True(t, ok)
	require.LessOrEqual(t, ansi.StringWidth(first.Text()), maxWidth-2)
	require.True(t, strings.HasSuffix(first.Text(), "…"), "expected an ellipsis, got %q", first.Text())
}

func TestSkillDescriptionDroppedWhenNameEatsTheRow(t *testing.T) {
	t.Parallel()

	// A name long enough to leave no room for a useful description renders
	// bare rather than as a row of ellipsis.
	long := strings.Repeat("a", maxWidth)
	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems([]SkillCompletionValue{{Name: long, Description: "does things"}}, nil)

	first, ok := c.filtered[0].(*CompletionItem)
	require.True(t, ok)
	require.Equal(t, "/"+long, first.Text())
	require.Equal(t, -1, first.detailStart)
}

func TestSelectCurrentReturnsSkillSelectionMsg(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems([]SkillCompletionValue{
		{Name: "commit", Path: "/skills/commit/SKILL.md"},
	}, nil)

	msg := c.selectCurrent(false)
	sel, ok := msg.(SelectionMsg[SkillCompletionValue])
	require.True(t, ok, "expected SelectionMsg[SkillCompletionValue], got %T", msg)
	require.Equal(t, "commit", sel.Value.Name)
	require.Equal(t, "/skills/commit/SKILL.md", sel.Value.Path)
	require.False(t, sel.KeepOpen)
	require.False(t, c.IsOpen(), "popup should close on non-keep-open select")
}

func TestSelectCurrentSkillKeepOpen(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems([]SkillCompletionValue{{Name: "commit"}}, nil)

	msg := c.selectCurrent(true)
	sel, ok := msg.(SelectionMsg[SkillCompletionValue])
	require.True(t, ok)
	require.True(t, sel.KeepOpen)
	require.True(t, c.IsOpen(), "popup should stay open on keep-open select")
}

func TestSetSkillItemsEmpty(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems(nil, nil)

	require.True(t, c.IsOpen())
	require.False(t, c.HasItems())
	require.Nil(t, c.selectCurrent(false))
}

func TestSkillItemsDoNotAffectFileSelectionDispatch(t *testing.T) {
	t.Parallel()

	// Regression: file/resource values must still dispatch their own
	// SelectionMsg types after the skill case was added.
	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetItems([]FileCompletionValue{{Path: "foo.go"}}, nil)

	msg := c.selectCurrent(false)
	sel, ok := msg.(SelectionMsg[FileCompletionValue])
	require.True(t, ok, "expected SelectionMsg[FileCompletionValue], got %T", msg)
	require.Equal(t, "foo.go", sel.Value.Path)
}

// TestSetSkillItemsIncludesPrompts pins that MCP prompts share the "/" popup
// with skills and are findable by name and description, the same way skills
// are.
func TestSetSkillItemsIncludesPrompts(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems(
		[]SkillCompletionValue{{Name: "code-review", Description: "Review a diff."}},
		[]PromptCompletionValue{
			{Name: "gitea:review", Description: "Review a pull request.", MCPName: "gitea", PromptID: "review"},
			{Name: "cairn:run_capture", Description: "Capture a run.", MCPName: "cairn", PromptID: "run_capture"},
		},
	)
	require.True(t, c.HasItems())

	// Server-qualified, so a server name narrows to that server's prompts.
	c.Filter("gitea:")
	require.True(t, c.HasItems(), "a server prefix must match its prompts")

	// Descriptions are part of the filter text for prompts too.
	c.Filter("capture")
	require.True(t, c.HasItems(), "a prompt must be findable by its description")

	// Skills still work alongside them.
	c.Filter("code-rev")
	require.True(t, c.HasItems(), "skills must remain in the popup")
}

// TestSetSkillItemsPromptsOnly covers a config with MCP prompts but no
// skills: the popup must still open rather than treating "no skills" as
// "nothing to offer".
func TestSetSkillItemsPromptsOnly(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems(nil, []PromptCompletionValue{
		{Name: "gitea:review", MCPName: "gitea", PromptID: "review"},
	})
	require.True(t, c.HasItems())
}

// TestSelectCurrentReturnsPromptSelection is the test whose absence let the
// whole feature ship inert: selectCurrent had no PromptCompletionValue case,
// so pressing enter on a prompt row returned nil, the model's type switch
// matched nothing, and the insertion path was dead code. Every other test
// stopped at SetSkillItems/Filter and never pressed enter.
func TestSelectCurrentReturnsPromptSelection(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems(nil, []PromptCompletionValue{
		{Name: "gitea:review", MCPName: "gitea", PromptID: "review"},
	})

	msg := c.selectCurrent(false)
	sel, ok := msg.(SelectionMsg[PromptCompletionValue])
	require.True(t, ok, "selecting a prompt must produce a prompt selection, got %T", msg)
	require.Equal(t, "gitea:review", sel.Value.Name)
	require.Equal(t, "gitea", sel.Value.MCPName)
	require.Equal(t, "review", sel.Value.PromptID)
}

// TestSelectCurrentStillReturnsSkillSelection is the control: adding prompts
// must not have displaced skills from the same popup.
func TestSelectCurrentStillReturnsSkillSelection(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetSkillItems([]SkillCompletionValue{{Name: "code-review"}}, nil)

	msg := c.selectCurrent(false)
	sel, ok := msg.(SelectionMsg[SkillCompletionValue])
	require.True(t, ok, "got %T", msg)
	require.Equal(t, "code-review", sel.Value.Name)
}
