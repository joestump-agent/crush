package dispatch

import "encoding/json"

// DispatchResult is the model-facing result of a dispatch: the running
// handle the DispatchAgent tool returns immediately (#64), and — once the
// dispatched agent finishes — the terminal payload carrying the work
// product back to the main agent (#66).
//
// The shape is pinned to the A2A task mapping from day one so #71's
// transport swap is invisible to the model: Status maps to the task
// state, KeyFindings to the terminal status message text, DiffSummary to
// the completion artifact, and SessionID is the follow-up query handle.
type DispatchResult struct {
	// DispatchID is the dispatch identifier — the registry key and the
	// suffix of the branch and directory names.
	DispatchID string `json:"dispatch_id"`
	// Branch is the workspace branch, crush-dispatch-{id}.
	Branch string `json:"branch"`
	// WorkspacePath is the absolute workspace directory the dispatched
	// agent is rooted at.
	WorkspacePath string `json:"workspace_path"`
	// SessionID is the ephemeral session backing the dispatched agent.
	SessionID string `json:"session_id"`
	// Status is the dispatch's lifecycle state: running while the agent
	// works, completed or failed as its terminal state.
	Status Status `json:"status"`
}

// Render returns the result as indented JSON, the stable wire shape the
// model sees for both the running handle and the terminal payload.
func (r DispatchResult) Render() string {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		// DispatchResult is all strings; marshaling cannot fail.
		return "{}"
	}
	return string(b)
}
