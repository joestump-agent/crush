package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaxDiffLines caps the diff text carried inside a DispatchResult's
// DiffSummary: the terminal payload lands in the parent session's
// context, so an enormous diff must not crowd out everything else. The
// parent agent can always read the workspace files for full detail.
const MaxDiffLines = 250

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
	// KeyFindings is the dispatched agent's final message — the
	// terminal status message text in the A2A mapping. Populated only
	// on completion.
	KeyFindings string `json:"key_findings,omitempty"`
	// DiffSummary is the condensed work product: a per-file change stat
	// followed by the diff itself, truncated at MaxDiffLines. Populated
	// only on completion; a capture failure is recorded inline rather
	// than failing the result.
	DiffSummary string `json:"diff_summary,omitempty"`
	// Error carries the failure reason when Status is failed. A
	// completed run leaves it empty.
	Error string `json:"error,omitempty"`
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

// TerminalMessage renders the payload delivered to the main agent when a
// dispatch finishes (#66): the review instruction plus the stable JSON
// shape. The result is informational — no automated merge, ever — the
// main agent reviews the diff and merges or dismisses; dismissal cleans
// the workspace up through Remove (#63).
func (r DispatchResult) TerminalMessage() string {
	var b strings.Builder
	fmt.Fprintf(&b, "A dispatched agent finished with status %q (dispatch %s, branch %s). Its result is below. Review the diff and decide whether to merge or dismiss it — the dispatch never merges itself; dismissal should clean up the workspace (git worktree remove and branch delete).\n\n", r.Status, r.DispatchID, r.Branch)
	b.WriteString(r.Render())
	return b.String()
}

// SummarizeDiff condenses a unified diff (Workspace.Diff's output) into
// the review-friendly form a DispatchResult carries: a per-file change
// stat, then the diff itself truncated at MaxDiffLines so a runaway diff
// cannot crowd the parent session's context.
func SummarizeDiff(diff string) string {
	if diff == "" {
		return ""
	}

	type fileStat struct {
		added   int
		removed int
	}
	var order []string
	stats := make(map[string]*fileStat)
	current := ""
	for _, line := range strings.Split(strings.TrimSuffix(diff, "\n"), "\n") {
		if after, ok := strings.CutPrefix(line, "+++ "); ok {
			// "+++ b/path" (and "+++ /dev/null" for deletions); the b/
			// prefix is cosmetic, drop it.
			current = strings.TrimPrefix(after, "b/")
			if _, ok := stats[current]; !ok {
				stats[current] = &fileStat{}
				order = append(order, current)
			}
			continue
		}
		if strings.HasPrefix(line, "--- ") {
			// The "--- a/file" half of the header, not a removal.
			continue
		}
		s, ok := stats[current]
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			s.added++
		case strings.HasPrefix(line, "-"):
			s.removed++
		}
	}

	var b strings.Builder
	if len(order) == 0 {
		// A diff with no per-file headers (e.g. a binary diff): fall
		// back to the raw text, still truncated.
		b.WriteString("(no per-file stats; raw diff)\n")
	} else {
		for _, file := range order {
			s := stats[file]
			fmt.Fprintf(&b, "%s | +%d -%d\n", file, s.added, s.removed)
		}
	}
	b.WriteString("\n")

	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	if len(lines) > MaxDiffLines {
		for _, line := range lines[:MaxDiffLines] {
			b.WriteString(line)
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "... (%d more diff lines truncated; read the workspace files for full detail)\n", len(lines)-MaxDiffLines)
	} else {
		b.WriteString(diff)
		if !strings.HasSuffix(diff, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}
