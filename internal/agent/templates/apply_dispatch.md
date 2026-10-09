Bring a finished dispatched agent's work into your own checkout: merge, squash-merge, or cherry-pick the dispatch branch, then remove the workspace. Use it after you have reviewed the dispatched agent's terminal result and decided the work is worth keeping — this is the merge half of the review decision, and it replaces hand-run git.

Address the agent by its dispatch ID or its @handle (the "dispatch_id" and "handle" fields of its dispatch handle); exactly one. You can only apply dispatches from your own session — another session's agents are invisible to this tool. Only a finished dispatch can be applied: a running one refuses — cancel it first with cancel_dispatch.

Modes:
• "merge" (default): git merge --no-ff of the dispatch branch into your current branch — history and all work preserved as a merge commit.
• "squash": git merge --squash — the work lands staged in your checkout but is NOT committed; you shape the final commit yourself.
• "cherry-pick": git cherry-pick of the dispatch branch's commits since its base.

Uncommitted changes in the dispatch workspace count as part of the work: they are committed on the dispatch branch first (as "crush-dispatch <id>: uncommitted work"), so every mode brings both committed and uncommitted work in.

Refusals, none of which change anything: your own checkout is dirty (commit or stash first — a merge cannot safely start on top of your uncommitted work), a merge/rebase/cherry-pick is already in progress in your checkout, the dispatch is unknown or still running, or the work conflicts with yours — the conflicting paths are listed, the merge is aborted, and your checkout is left exactly as it was.
