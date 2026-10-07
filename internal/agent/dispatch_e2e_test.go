package agent_test

// End-to-end dispatch tests over the real A2A ServerFactory (#424):
// the dispatch_agent tool, a real dispatched agent, the a2a server
// serving it, the A2A client streaming the turn, and terminal assembly
// — the composition production wires in internal/app. Each scenario
// asserts the shared invariants (exactly one terminal status transition
// on the registry entry, exactly one parent delivery) plus its own
// outcome.

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/a2a"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/stretchr/testify/require"
)

// TestDispatchE2EOverServerFactory runs the dispatch scenarios through
// the real server factory, table-driven over the terminal outcomes that
// reached main with green CI while every side was tested against a fake
// of the other.
func TestDispatchE2EOverServerFactory(t *testing.T) {
	t.Parallel()

	defaultFactory := func() agent.DispatchHost { return a2a.NewServerFactory(t.TempDir()) }

	// shortClientDeadlineFactory injects the client #344's seam exists
	// for: a 250ms response-header deadline, with no total timeout. A
	// reintroduced client Timeout would cut the SSE body and fail the
	// long-run scenario. The custom transport dials the factory's unix
	// socket (the endpoint is a routing label), so the deadline is the
	// only thing under test.
	shortClientDeadlineFactory := func() agent.DispatchHost {
		var f *a2a.ServerFactory
		f = a2a.NewServerFactory(t.TempDir(), a2a.WithHTTPClient(&http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "unix", f.SocketPath())
				},
				ResponseHeaderTimeout: 250 * time.Millisecond,
			},
		}))
		return f
	}

	blockedModel := func(t *testing.T) (fantasy.LanguageModel, func()) {
		return agent.NewBlockingScriptedModel(agent.ScriptedStep{Text: "done"}), nil
	}

	scenarios := []struct {
		name     string
		settings config.TodoEnforcementSettings
		factory  func() agent.DispatchHost
		model    func(t *testing.T) (fantasy.LanguageModel, func())
		check    func(t *testing.T, entry dispatch.Entry, elapsed time.Duration)
	}{
		{
			name: "completed run writes a file and delivers findings",
			model: func(t *testing.T) (fantasy.LanguageModel, func()) {
				return agent.NewScriptedModel(
					agent.ScriptedStep{ToolCalls: []agent.ScriptedToolCall{{
						Name:  "write",
						Input: `{"file_path":"hello.txt","content":"written over a2a\n"}`,
					}}},
					agent.ScriptedStep{Text: "wrote hello.txt; findings here"},
				), nil
			},
			check: func(t *testing.T, entry dispatch.Entry, elapsed time.Duration) {
				require.Equal(t, dispatch.StatusCompleted, entry.Status)
				require.Equal(t, dispatch.StatusCompleted, entry.Result.Status)
				require.Equal(t, "wrote hello.txt; findings here", entry.Result.KeyFindings)
				require.Contains(t, entry.Result.DiffSummary, "hello.txt")
				written, err := os.ReadFile(filepath.Join(entry.Path, "hello.txt"))
				require.NoError(t, err)
				require.Equal(t, "written over a2a\n", string(written))
			},
		},
		{
			name: "out-of-band hard timeout kill ends killed in seconds",
			settings: config.TodoEnforcementSettings{
				HardTimeout: 200 * time.Millisecond,
			},
			model: blockedModel,
			check: func(t *testing.T, entry dispatch.Entry, elapsed time.Duration) {
				// Timed from the run's own start, not the dispatch call:
				// provisioning the worktree is no part of the kill, and on
				// a loaded Windows runner it alone can take seconds.
				require.False(t, entry.StartedAt.IsZero(), "the dispatched agent started running")
				require.False(t, entry.FinishedAt.IsZero(), "the kill recorded when the run ended")
				require.Less(t, entry.FinishedAt.Sub(entry.StartedAt), 10*time.Second,
					"the kill must end the dispatch in seconds, not the three-minute stream cut")
				require.Equal(t, dispatch.StatusKilled, entry.Status)
				require.Equal(t, dispatch.ReasonHardTimeout, entry.Result.KilledReason)
				require.Equal(t, dispatch.StatusKilled, entry.Result.Status)
				require.DirExists(t, entry.Path, "a kill preserves the workspace for salvage")
			},
		},
		{
			name:     "long run outlives a short client deadline",
			factory:  shortClientDeadlineFactory,
			settings: config.TodoEnforcementSettings{},
			model: func(t *testing.T) (fantasy.LanguageModel, func()) {
				m := agent.NewBlockingScriptedModel(agent.ScriptedStep{Text: "finally done"})
				return m, m.Release
			},
			check: func(t *testing.T, entry dispatch.Entry, elapsed time.Duration) {
				require.GreaterOrEqual(t, elapsed, 900*time.Millisecond,
					"the run must actually outlast the client's deadline")
				require.Equal(t, dispatch.StatusCompleted, entry.Status)
				require.Equal(t, "finally done", entry.Result.KeyFindings)
			},
		},
		{
			name: "tool loop ends killed with the loop reason",
			settings: config.TodoEnforcementSettings{
				Enabled:         true,
				NudgeThreshold:  50,
				KillAfterNudges: 0,
			},
			model: func(t *testing.T) (fantasy.LanguageModel, func()) {
				steps := make([]agent.ScriptedStep, 0, 13)
				for range 12 {
					steps = append(steps, agent.ScriptedStep{
						ToolCalls: []agent.ScriptedToolCall{{Name: "probe", Input: "{}"}},
					})
				}
				steps = append(steps, agent.ScriptedStep{Text: "done"})
				return agent.NewScriptedModel(steps...), nil
			},
			check: func(t *testing.T, entry dispatch.Entry, elapsed time.Duration) {
				require.Equal(t, dispatch.StatusKilled, entry.Status)
				require.Equal(t, dispatch.ReasonToolLoop, entry.Result.KilledReason)
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()

			factory := sc.factory
			if factory == nil {
				factory = defaultFactory
			}
			model, release := sc.model(t)
			h := agent.NewDispatchHarness(t, model, sc.settings, factory())

			start := time.Now()
			handle := h.Dispatch(t, "do the work")
			if release != nil {
				go func() {
					time.Sleep(time.Second)
					release()
				}()
			}
			entry := h.WaitTerminal(t, handle.DispatchID)
			elapsed := time.Since(start)

			// Every scenario records exactly one terminal status
			// transition on its registry entry and exactly one parent
			// delivery, whatever the outcome.
			require.Eventually(t, func() bool {
				return h.TerminalTransitions(handle.DispatchID) == 1
			}, 10*time.Second, 25*time.Millisecond, "exactly one terminal status transition expected")
			require.Eventually(t, func() bool {
				return h.ParentDeliveries() == 1
			}, 10*time.Second, 25*time.Millisecond, "exactly one parent delivery expected")
			require.NotNil(t, entry.Result)

			sc.check(t, entry, elapsed)
		})
	}
}

// TestDispatchListedOnAgentIndexE2E: a real dispatch through the real
// server factory is on the host's agent index (#421) with its handle,
// its child session as the context, and the parent session it was
// dispatched from, and the index follows it to its terminal state.
func TestDispatchListedOnAgentIndexE2E(t *testing.T) {
	t.Parallel()

	host := a2a.NewServerFactory(t.TempDir())
	model := agent.NewScriptedModel(agent.ScriptedStep{Text: "done"})
	h := agent.NewDispatchHarness(t, model, config.TodoEnforcementSettings{}, host)
	handle := h.Dispatch(t, "do the work")
	entry := h.WaitTerminal(t, handle.DispatchID)
	require.Equal(t, dispatch.StatusCompleted, entry.Status)

	conn, ok := host.AgentIndexConn()
	require.True(t, ok)
	var listed a2a.AgentDescriptor
	require.Eventually(t, func() bool {
		descriptors, err := conn.ListAgents(t.Context())
		if err != nil || len(descriptors) != 1 {
			return false
		}
		listed = descriptors[0]
		return listed.Terminal()
	}, 10*time.Second, 25*time.Millisecond)
	require.Equal(t, handle.DispatchID, listed.ID)
	require.Equal(t, handle.Handle, listed.Handle)
	require.Equal(t, handle.SessionID, listed.ContextID)
	require.Equal(t, h.ParentSessionID(), listed.ParentSessionID)
	require.Equal(t, a2a.DispatchStatusCompleted, listed.State)
}

// TestDispatchPermissionPromptE2E runs #353 through the real server
// factory: the dispatched agent's bash call requests permission on its
// scoped service, the served executor parks the run in input-required,
// the parent's transport puts the request through the parent's own
// permission service — labeled with the handle, typed params intact —
// and the parent's verdict decides whether the command runs.
func TestDispatchPermissionPromptE2E(t *testing.T) {
	t.Parallel()

	for _, grant := range []bool{true, false} {
		name := "deny keeps the command from running"
		if grant {
			name = "grant runs the command"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			model := agent.NewScriptedModel(
				agent.ScriptedStep{ToolCalls: []agent.ScriptedToolCall{{
					Name:  "bash",
					Input: `{"command":"touch granted.txt","description":"create the marker"}`,
				}}},
				agent.ScriptedStep{Text: "done"},
			)
			h := agent.NewDispatchHarness(t, model, config.TodoEnforcementSettings{}, a2a.NewServerFactory(t.TempDir()))
			parent := h.PromptingPermissions(t)
			requests := parent.Subscribe(t.Context())

			handle := h.Dispatch(t, "create the marker")

			select {
			case ev := <-requests:
				require.Equal(t, tools.BashToolName, ev.Payload.ToolName)
				require.True(t, strings.HasPrefix(ev.Payload.Description, "@"+handle.Handle+": "),
					"the request names the dispatch, got %q", ev.Payload.Description)
				params, ok := ev.Payload.Params.(tools.BashPermissionsParams)
				require.True(t, ok, "params cross the wire typed, got %T", ev.Payload.Params)
				require.Equal(t, "touch granted.txt", params.Command)
				if grant {
					require.True(t, parent.Grant(ev.Payload))
				} else {
					require.True(t, parent.Deny(ev.Payload))
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the dispatched agent's permission request never reached the parent")
			}

			entry := h.WaitTerminal(t, handle.DispatchID)
			require.Equal(t, dispatch.StatusCompleted, entry.Status)
			_, err := os.Stat(filepath.Join(entry.Path, "granted.txt"))
			if grant {
				require.NoError(t, err, "a granted command runs")
			} else {
				require.True(t, os.IsNotExist(err), "a denied command must not run")
			}
		})
	}
}
