package agent

// Permission prompts from dispatched agents (#353): a dispatched agent's
// tool requests approval through its scoped permission service, the
// served executor parks the run in input-required with the request, and
// the parent's transport hands it to the coordinator here. The
// coordinator puts it through the parent's own permission service — the
// same approval flow as the main agent's — labeled with the dispatch's
// @handle, and the decision travels back on the same A2A task.

import (
	"context"

	"github.com/charmbracelet/crush/internal/permission"
)

// answerDispatchPermission is the coordinator's OnPermissionRequired
// (#353). With no parent permission service there is nobody to approve,
// and the request is denied. Otherwise it is requested on the parent's
// service, which applies the parent's live yolo state and session grants
// and, when a person must decide, shows the approval dialog. The wait
// ends with the run: a kill (hard timeout, cancel, shutdown) returns an
// error carrying the kill reason, which the transport cancels the parked
// task with.
func (c *coordinator) answerDispatchPermission(ctx context.Context, run dispatchRun, handle string, req PermissionPrompt) (bool, error) {
	if c.permissions == nil {
		return false, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-run.kill.killed():
			cancel()
		case <-ctx.Done():
		}
	}()

	allowed, err := c.permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID:   req.SessionID,
		ToolCallID:  req.ToolCallID,
		ToolName:    req.ToolName,
		Description: labelDispatchPermission(req.Description, handle),
		Action:      req.Action,
		Params:      req.Params,
		Path:        req.Path,
	})
	if err != nil {
		return false, dispatchQuestionAbort(run, err)
	}
	return allowed, nil
}

// labelDispatchPermission prefixes a permission request's description
// with the requesting dispatch's @handle, so the parent's user sees which
// agent is asking.
func labelDispatchPermission(description, handle string) string {
	if handle == "" {
		return description
	}
	if description == "" {
		return "@" + handle
	}
	return "@" + handle + ": " + description
}
