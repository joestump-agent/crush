Dispatch an independent agent to work in an isolated git worktree copy of the repository. The dispatched agent gets its own clean workspace on a fresh branch, runs in the background, and its diff plus findings are delivered back when it finishes. The tool returns immediately with a running handle, so you can keep working — or dispatch more agents — while it runs.

Use this for subtasks that are independent of your own work and touch different files: running tests, updating docs, writing a probe, refactoring an unrelated module. Do NOT use it for work that depends on changes you have not made yet, or for read-only searches — use the agent tool for those.

The prompt should be a self-contained task description: the dispatched agent cannot ask you questions, and it starts from the repository's current state on the base branch, not from your uncommitted changes. It cannot merge or push; you review its diff when the result arrives and decide what to do with it.
