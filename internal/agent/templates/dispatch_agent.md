Dispatch an independent agent to work on a task in its own isolated
workspace, in parallel with your other work.

The dispatched agent runs in a fresh git worktree branched from the
current tree, with its own coding toolchain rooted at that workspace, so
its file changes never collide with yours or with sibling dispatches. It
runs to completion and returns a DispatchResult: the dispatch id, the
workspace path, a status (completed / failed), the agent's key findings,
and a unified diff of everything it changed (committed and uncommitted).

Use this to fan out independent, self-contained subtasks — each dispatch
gets a clean workspace, so you can launch several at once and they run
concurrently. Review the returned diff and decide whether to fold the
work in; nothing is merged automatically.

Parameters:
- prompt (required): the task for the dispatched agent, self-contained
  enough to act on without your conversation.
- model (optional): "large" or "small"; defaults to the small model.
- branch (optional): base ref to fork the workspace from; defaults to
  the current tree.
