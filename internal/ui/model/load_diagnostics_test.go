package model

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/crush/internal/workspace"
)

// diagnosticsWorkspace is a workspace.Workspace stub that serves fixed
// load diagnostics; the embedded interface panics on anything else.
type diagnosticsWorkspace struct {
	workspace.Workspace
	diags []config.LoadDiagnostic
}

func (w *diagnosticsWorkspace) LoadDiagnostics() []config.LoadDiagnostic { return w.diags }

// newDiagnosticsUI builds the smallest UI the notice reads: a Common
// around the stub, without DefaultCommon's theme lookup through
// Workspace.Config.
func newDiagnosticsUI(ws workspace.Workspace) *UI {
	return &UI{com: &common.Common{Workspace: ws}}
}

// The config load's agent-definition diagnostics become one startup
// warning in the status bar (#560): the first in full, the rest counted,
// and nothing at all when the load was clean.
func TestLoadDiagnosticsNotice(t *testing.T) {
	t.Parallel()

	rev := config.LoadDiagnostic{
		Severity: config.DiagnosticWarning,
		Agent:    "rev",
		Message:  "cannot be dispatched: agents.rev.card: must use https; plain http is allowed only for loopback hosts",
	}
	worker := config.LoadDiagnostic{
		Severity: config.DiagnosticWarning,
		Agent:    "worker",
		Message:  "agents.worker.workspace: parsed but not honored by a builtin-runtime agent",
	}

	t.Run("no diagnostics, no notice", func(t *testing.T) {
		t.Parallel()
		m := newDiagnosticsUI(&diagnosticsWorkspace{})
		require.Nil(t, m.loadDiagnosticsNotice())
	})

	t.Run("no workspace, no notice", func(t *testing.T) {
		t.Parallel()
		m := newDiagnosticsUI(nil)
		require.Nil(t, m.loadDiagnosticsNotice())
	})

	t.Run("one diagnostic is shown in full", func(t *testing.T) {
		t.Parallel()
		m := newDiagnosticsUI(&diagnosticsWorkspace{diags: []config.LoadDiagnostic{rev}})
		msg, ok := m.loadDiagnosticsNotice().(util.InfoMsg)
		require.True(t, ok, "the notice is a status-bar info message")
		require.Equal(t, util.InfoTypeWarn, msg.Type)
		require.Equal(t, loadDiagnosticsTTL, msg.TTL)
		require.Equal(t, `Agent "rev": cannot be dispatched: agents.rev.card: must use https; plain http is allowed only for loopback hosts (see crush.log)`, msg.Msg)
	})

	t.Run("several diagnostics show the first and count the rest", func(t *testing.T) {
		t.Parallel()
		m := newDiagnosticsUI(&diagnosticsWorkspace{diags: []config.LoadDiagnostic{rev, worker, worker}})
		msg, ok := m.loadDiagnosticsNotice().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeWarn, msg.Type)
		require.Equal(t, `Agent "rev": cannot be dispatched: agents.rev.card: must use https; plain http is allowed only for loopback hosts (+2 more in crush.log)`, msg.Msg)
	})

	t.Run("a diagnostic that names no agent is shown without an agent", func(t *testing.T) {
		t.Parallel()
		allowed := config.LoadDiagnostic{
			Severity: config.DiagnosticWarning,
			Message:  "ignoring allow-commands entries not in the default banned list: shh",
		}
		m := newDiagnosticsUI(&diagnosticsWorkspace{diags: []config.LoadDiagnostic{allowed}})
		msg, ok := m.loadDiagnosticsNotice().(util.InfoMsg)
		require.True(t, ok, "the notice is a status-bar info message")
		require.Equal(t, util.InfoTypeWarn, msg.Type)
		require.Equal(t, `ignoring allow-commands entries not in the default banned list: shh (see crush.log)`, msg.Msg)
	})
}
