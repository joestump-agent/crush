package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/permission"
)

// dispatchedPermission is the permission request the tests' dispatched
// agent parks on.
func dispatchedPermission() PermissionPrompt {
	return PermissionPrompt{
		ID:          "perm-1",
		SessionID:   "claimed-by-the-wire",
		ToolCallID:  "call-1",
		ToolName:    tools.BashToolName,
		Description: "Execute command: make test",
		Action:      "execute",
		Params:      tools.BashPermissionsParams{Command: "make test"},
		Path:        "/work",
	}
}

// A dispatched agent's permission request goes through the parent's own
// permission service (#353), labeled with the dispatch's handle, and the
// parent's verdict is the answer. With no parent service it is denied.
func TestAnswerDispatchPermission(t *testing.T) {
	t.Parallel()

	t.Run("no parent service denies", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{}
		allowed, err := c.answerDispatchPermission(t.Context(), dispatchRun{sessionID: "dispatch-session", kill: &dispatchKill{}}, "tester", dispatchedPermission())
		require.NoError(t, err)
		require.False(t, allowed)
	})

	for _, grant := range []bool{true, false} {
		name := "parent denies"
		if grant {
			name = "parent grants"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parent := permission.NewPermissionService(t.TempDir(), false, nil)
			c := &coordinator{permissions: parent}
			requests := parent.Subscribe(t.Context())

			type outcome struct {
				allowed bool
				err     error
			}
			done := make(chan outcome, 1)
			go func() {
				allowed, err := c.answerDispatchPermission(t.Context(), dispatchRun{sessionID: "dispatch-session", kill: &dispatchKill{}}, "tester", dispatchedPermission())
				done <- outcome{allowed, err}
			}()

			ev := <-requests
			require.Equal(t, "@tester: Execute command: make test", ev.Payload.Description, "the request names the dispatch")
			require.Equal(t, "call-1", ev.Payload.ToolCallID)
			require.Equal(t, "dispatch-session", ev.Payload.SessionID, "the run's session, not the one the request claims")
			require.Equal(t, tools.BashPermissionsParams{Command: "make test"}, ev.Payload.Params,
				"the typed params reach the approval dialog")
			if grant {
				require.True(t, parent.Grant(ev.Payload))
			} else {
				require.True(t, parent.Deny(ev.Payload))
			}

			got := <-done
			require.NoError(t, got.err)
			require.Equal(t, grant, got.allowed)
		})
	}

	t.Run("a kill ends the wait with its reason", func(t *testing.T) {
		t.Parallel()
		parent := permission.NewPermissionService(t.TempDir(), false, nil)
		c := &coordinator{permissions: parent}
		requests := parent.Subscribe(t.Context())
		kill := &dispatchKill{}

		done := make(chan error, 1)
		go func() {
			_, err := c.answerDispatchPermission(t.Context(), dispatchRun{sessionID: "dispatch-session", kill: kill}, "tester", dispatchedPermission())
			done <- err
		}()
		<-requests
		kill.kill(dispatch.ReasonHardTimeout)

		select {
		case err := <-done:
			require.EqualError(t, err, dispatch.ReasonHardTimeout)
		case <-time.After(10 * time.Second):
			t.Fatal("the kill did not end the permission wait")
		}
	})
}

func TestLabelDispatchPermission(t *testing.T) {
	t.Parallel()
	require.Equal(t, "@tester: Execute command: ls", labelDispatchPermission("Execute command: ls", "tester"))
	require.Equal(t, "@tester", labelDispatchPermission("", "tester"))
	require.Equal(t, "Execute command: ls", labelDispatchPermission("Execute command: ls", ""))
}

// A kill ends a dispatched permission wait even while another request
// holds the parent's dialog, and nothing is published for the killed
// dispatch afterwards (#353).
func TestAnswerDispatchPermissionKillWhileParentDialogOpen(t *testing.T) {
	t.Parallel()
	parent := permission.NewPermissionService(t.TempDir(), false, nil)
	c := &coordinator{permissions: parent}
	requests := parent.Subscribe(t.Context())

	// The main agent's own request holds the parent's dialog.
	mainDone := make(chan struct{})
	go func() {
		defer close(mainDone)
		_, _ = parent.Request(context.Background(), permission.CreatePermissionRequest{SessionID: "main", ToolCallID: "main-call", ToolName: tools.BashToolName, Action: "execute"})
	}()
	held := <-requests

	kill := &dispatchKill{}
	done := make(chan error, 1)
	go func() {
		_, err := c.answerDispatchPermission(t.Context(), dispatchRun{sessionID: "dispatch-session", kill: kill}, "tester", dispatchedPermission())
		done <- err
	}()
	kill.kill(dispatch.ReasonHardTimeout)

	select {
	case err := <-done:
		require.EqualError(t, err, dispatch.ReasonHardTimeout)
	case <-time.After(10 * time.Second):
		t.Fatal("the kill did not end the wait while the parent's dialog was open")
	}

	require.True(t, parent.Grant(held.Payload))
	<-mainDone
	select {
	case ev := <-requests:
		t.Fatalf("a request was published for the killed dispatch: %+v", ev.Payload)
	default:
	}
}
