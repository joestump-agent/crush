// Package dispatch provides isolated, SCM-generic workspaces for
// dispatched agents together with the single registry that tracks them.
//
// A [Workspace] is a scratch working directory a dispatched agent runs
// in, forked from a base ref so its changes never collide with the main
// agent or with sibling dispatches. The git worktree [GitBackend] is the
// first backend, but [Backend] is deliberately generic: jj, sapling, or
// a plain directory copy can back a workspace without touching callers.
//
// The [Registry] owns the one map of live dispatches — their workspace
// path, backing session, A2A endpoint/card, and status. In-process
// dispatch (Phase 1) provisions against a local Registry; the A2A
// discovery layer reads the same Registry rather than keeping a second
// directory of its own.
package dispatch
