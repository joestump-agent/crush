package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/ui/common"
	uistyles "github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestSkillStatusItemsIncludesBuiltinSkills verifies sidebar skills include
// both runtime-discovered skill states and builtin skills that may not have
// emitted a SkillState event yet.
func TestSkillStatusItemsIncludesBuiltinSkills(t *testing.T) {
	t.Parallel()

	st := uistyles.CharmtonePantera()
	ui := &UI{
		com: &common.Common{Styles: &st},
		skillStates: []*skills.SkillState{
			{Name: "go-doc", Path: "/tmp/go-doc/SKILL.md", State: skills.StateNormal},
		},
	}

	items := ui.skillStatusItems()
	require.NotEmpty(t, items)

	var hasGoDoc bool
	for _, item := range items {
		if item.title == st.Resource.Name.Render("go-doc") {
			hasGoDoc = true
			break
		}
	}
	require.True(t, hasGoDoc)

	builtinSkills := skills.DiscoverBuiltin()
	require.NotEmpty(t, builtinSkills)

	var hasBuiltin bool
	for _, skill := range builtinSkills {
		if skill.Name == "go-doc" {
			continue
		}
		expected := st.Resource.Name.Render(skill.Name)
		for _, item := range items {
			if item.title == expected {
				hasBuiltin = true
				break
			}
		}
		if hasBuiltin {
			break
		}
	}
	require.True(t, hasBuiltin)
}

// toggleSkillWorkspace records config writes and skill reloads so tests can
// assert on what toggleSkill persists.
type toggleSkillWorkspace struct {
	workspace.Workspace
	cfg         *config.Config
	setKeys     []string
	setValues   []any
	reloadCalls int
}

func (w *toggleSkillWorkspace) Config() *config.Config { return w.cfg }

func (w *toggleSkillWorkspace) SetConfigField(_ config.Scope, key string, value any) error {
	w.setKeys = append(w.setKeys, key)
	w.setValues = append(w.setValues, value)
	return nil
}

func (w *toggleSkillWorkspace) ReloadSkills() error {
	w.reloadCalls++
	return nil
}

// TestToggleSkillRejectsEmptyName verifies an empty skill name (as produced
// by broken SKILL.md files) never reaches config persistence.
func TestToggleSkillRejectsEmptyName(t *testing.T) {
	t.Parallel()

	ws := &toggleSkillWorkspace{cfg: &config.Config{Options: &config.Options{}}}
	ui := &UI{com: &common.Common{Workspace: ws}}

	cmd := ui.toggleSkill("")
	require.Nil(t, cmd, "toggleSkill with an empty name must be a no-op")
	require.Empty(t, ws.setKeys, "no config field should be written")
	require.Zero(t, ws.reloadCalls, "skills should not be reloaded")
	require.Empty(t, ws.cfg.Options.DisabledSkills)
}

// TestToggleSkillPersistsAndRefreshes verifies a normal toggle still writes
// disabled_skills, reloads skills, and requests a dialog refresh.
func TestToggleSkillPersistsAndRefreshes(t *testing.T) {
	t.Parallel()

	ws := &toggleSkillWorkspace{cfg: &config.Config{Options: &config.Options{}}}
	ui := &UI{com: &common.Common{Workspace: ws}}

	cmd := ui.toggleSkill("alpha")
	require.NotNil(t, cmd)

	msg := cmd()
	_, ok := msg.(skillsRefreshedMsg)
	require.True(t, ok, "toggle should request a skills dialog refresh")
	require.Equal(t, []string{"options.disabled_skills"}, ws.setKeys)
	require.Equal(t, 1, ws.reloadCalls)
	require.Len(t, ws.setValues, 1)
	require.Equal(t, []string{"alpha"}, ws.setValues[0])
}

func TestSkillStatusItemsExcludesDisabledSkills(t *testing.T) {
	t.Parallel()

	st := uistyles.CharmtonePantera()
	ui := &UI{
		com: &common.Common{
			Styles:    &st,
			Workspace: &testWorkspace{cfg: &config.Config{Options: &config.Options{DisabledSkills: []string{"go-doc", "crush-config"}}}},
		},
		skillStates: []*skills.SkillState{
			{Name: "go-doc", Path: "/tmp/go-doc/SKILL.md", State: skills.StateNormal},
		},
	}

	items := ui.skillStatusItems()

	for _, item := range items {
		require.NotEqual(t, "go-doc", item.name)
		require.NotEqual(t, "crush-config", item.name)
	}
}
