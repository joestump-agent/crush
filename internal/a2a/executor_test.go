package a2a

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/session"
)

// fakeRunner is a test double for the SessionAgent slice the Executor drives.
type fakeRunner struct {
	result *fantasy.AgentResult
	err    error

	// panicValue, when non-nil, makes Run panic with it instead of
	// returning (#345).
	panicValue any

	gotCall     agent.SessionAgentCall
	ran         bool
	canceledFor string
}

func (f *fakeRunner) Run(_ context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	f.ran = true
	f.gotCall = call
	if f.panicValue != nil {
		panic(f.panicValue)
	}
	return f.result, f.err
}

func (f *fakeRunner) Cancel(sessionID string) { f.canceledFor = sessionID }

func textResult(s string) *fantasy.AgentResult {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{fantasy.TextContent{Text: s}},
		},
	}
}

func newExecCtx(msg *a2aspec.Message) *a2asrv.ExecutorContext {
	return &a2asrv.ExecutorContext{
		Message:   msg,
		TaskID:    "task-1",
		ContextID: "ctx-1",
	}
}

func collect(t *testing.T, seq iter.Seq2[a2aspec.Event, error]) []a2aspec.Event {
	t.Helper()
	var evs []a2aspec.Event
	for ev, err := range seq {
		// The executor reports failures as events, never as the second
		// value of the sequence.
		require.NoError(t, err, "unexpected error from executor sequence")
		evs = append(evs, ev)
	}
	return evs
}

// artifactState is the sentinel states uses for artifact events so ordering
// can be asserted in one shot alongside status updates.
const artifactState a2aspec.TaskState = "<artifact>"

// states summarizes an event stream as a slice of task states.
func states(t *testing.T, evs []a2aspec.Event) []a2aspec.TaskState {
	t.Helper()
	out := make([]a2aspec.TaskState, 0, len(evs))
	for _, ev := range evs {
		switch e := ev.(type) {
		case *a2aspec.Task:
			out = append(out, e.Status.State)
		case *a2aspec.TaskStatusUpdateEvent:
			out = append(out, e.Status.State)
		case *a2aspec.TaskArtifactUpdateEvent:
			out = append(out, artifactState)
		default:
			t.Fatalf("unexpected event type %T", ev)
		}
	}
	return out
}

func statusUpdate(t *testing.T, ev a2aspec.Event) *a2aspec.TaskStatusUpdateEvent {
	t.Helper()
	sue, ok := ev.(*a2aspec.TaskStatusUpdateEvent)
	require.True(t, ok, "event is %T, want *TaskStatusUpdateEvent", ev)
	return sue
}

func statusMessageText(t *testing.T, ev a2aspec.Event) string {
	t.Helper()
	sue := statusUpdate(t, ev)
	if sue.Status.Message == nil {
		return ""
	}
	return partsText(sue.Status.Message.Parts)
}

func partsText(parts a2aspec.ContentParts) string {
	var s string
	for _, p := range parts {
		s += p.Text()
	}
	return s
}

func TestExecuteHappyPathWithDiff(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("all done")}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return "the diff", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		artifactState,
		artifactState,
		a2aspec.TaskStateCompleted,
	}
	require.Equal(t, want, states(t, evs))

	require.True(t, runner.ran, "runner.Run was not called")
	require.Equal(t, "sess-1", runner.gotCall.SessionID)
	require.Equal(t, "do the thing", runner.gotCall.Prompt)

	// The diff artifact carries the chunk (one, for a short diff), the
	// task's identifiers, and the diff identity.
	art, ok := evs[2].(*a2aspec.TaskArtifactUpdateEvent)
	require.True(t, ok, "event is %T, want *TaskArtifactUpdateEvent", evs[2])
	require.Equal(t, DiffArtifactID, art.Artifact.ID)
	require.Equal(t, DiffArtifactName, art.Artifact.Name)
	require.False(t, art.Append)
	require.True(t, art.LastChunk, "a single-chunk diff is also the last chunk")
	require.Equal(t, DiffMediaType, art.Artifact.Parts[0].MediaType)
	require.Equal(t, DiffFilename, art.Artifact.Parts[0].Filename)
	require.Equal(t, "the diff", partsText(art.Artifact.Parts))
	require.Equal(t, a2aspec.TaskID("task-1"), art.TaskID)
	require.Equal(t, "ctx-1", art.ContextID)

	// The dispatch-result artifact carries the typed outcome.
	res, ok := evs[3].(*a2aspec.TaskArtifactUpdateEvent)
	require.True(t, ok, "event is %T, want *TaskArtifactUpdateEvent", evs[3])
	require.Equal(t, ResultArtifactID, res.Artifact.ID)
	require.Equal(t, ResultArtifactName, res.Artifact.Name)
	require.False(t, res.Append)
	require.True(t, res.LastChunk)
	decoded, ok := decodeDispatchOutcome(res.Artifact.Parts[0])
	require.True(t, ok)
	require.Equal(t, DispatchOutcome{DiffBytes: len("the diff")}, decoded)

	// The terminal status carries the agent's text output, stamped with
	// the task's identifiers.
	terminal := statusUpdate(t, evs[4])
	require.Equal(t, a2aspec.TaskID("task-1"), terminal.TaskID)
	require.Equal(t, "ctx-1", terminal.ContextID)
	require.Equal(t, "all done", statusMessageText(t, evs[4]))
	require.NotNil(t, terminal.Status.Message)
	require.Equal(t, a2aspec.TaskID("task-1"), terminal.Status.Message.TaskID)
}

func TestExecuteNoDiffFunc(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("done")}
	exec := NewExecutor(runner, "sess-1")

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}
	require.Equal(t, want, states(t, evs), "no artifact expected")
}

func TestExecuteEmptyDiffEmitsNoArtifact(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("done")}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return "", nil // clean worktree
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	// No diff artifact for an empty diff; the dispatch-result artifact
	// still reports the zero work (#361).
	for _, ev := range evs {
		if art, isArtifact := ev.(*a2aspec.TaskArtifactUpdateEvent); isArtifact {
			require.Equal(t, ResultArtifactID, art.Artifact.ID, "an empty diff emits only the dispatch-result artifact")
			decoded, ok := decodeDispatchOutcome(art.Artifact.Parts[0])
			require.True(t, ok)
			require.Zero(t, decoded.DiffBytes)
			require.Zero(t, decoded.FilesChanged)
			require.Empty(t, decoded.DiffError)
		}
	}
}

func TestExecuteDiffErrorStillCompletes(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("done")}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return "", errors.New("not a git repo")
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	// The run still completes: no diff artifact, but the typed outcome
	// carries the capture error on the wire (#361).
	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		artifactState,
		a2aspec.TaskStateCompleted,
	}
	require.Equal(t, want, states(t, evs), "diff error must not fail the run")

	art, ok := evs[2].(*a2aspec.TaskArtifactUpdateEvent)
	require.True(t, ok)
	require.Equal(t, ResultArtifactID, art.Artifact.ID)
	decoded, ok := decodeDispatchOutcome(art.Artifact.Parts[0])
	require.True(t, ok)
	require.Equal(t, "not a git repo", decoded.DiffError)
}

func TestExecuteRunFailure(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{err: errors.New("model exploded")}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return "should not be called", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateFailed,
	}
	require.Equal(t, want, states(t, evs))

	// The error is surfaced in the failed status message.
	require.Equal(t, "model exploded", statusMessageText(t, evs[2]))
}

// A panic inside the runner (a tool or provider adapter bug) must fail
// the task and keep the process alive (#345): Execute yields Submitted,
// Working, then exactly one Failed with "panicked" in the message.
func TestExecuteRunnerPanicFails(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{panicValue: errors.New("tool blew up")}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return "should not be called", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateFailed,
	}
	require.Equal(t, want, states(t, evs))
	require.Contains(t, statusMessageText(t, evs[2]), "panicked")

	// The recovered error must not wrap context.Canceled, so the
	// canceled branch of Execute cannot swallow it (#342).
	_, err := exec.runWithTodos(context.Background(), newExecCtx(msg), "go",
		func(a2aspec.Event, error) bool { return true })
	require.Error(t, err)
	require.Contains(t, err.Error(), "panicked")
	require.False(t, errors.Is(err, context.Canceled))
}

func TestExecuteNilResultFails(t *testing.T) {
	t.Parallel()

	// SessionAgent.Run returns (nil, nil) without running anything when
	// the session is busy (prompt silently queued) or a cancel landed
	// during dispatch. The task must not report Completed.
	runner := &fakeRunner{result: nil, err: nil}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return "should not become an artifact", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateFailed,
	}
	require.Equal(t, want, states(t, evs), "a turn that never ran must not complete")
	require.Contains(t, statusMessageText(t, evs[2]), "did not start a turn")
}

// blockingCancelRunner blocks in Run until its kill channel fires, then
// returns context.Canceled — a kill delivered behind the executor's back
// unless the test calls Cancel itself.
type blockingCancelRunner struct {
	started chan struct{}
	kill    chan struct{}
}

func (f *blockingCancelRunner) Run(_ context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	if f.started != nil {
		close(f.started)
	}
	<-f.kill
	return nil, context.Canceled
}

func (f *blockingCancelRunner) Cancel(sessionID string) {
	close(f.kill)
}

func TestExecuteOutOfBandCancelEmitsCanceled(t *testing.T) {
	t.Parallel()

	// A runner killed behind the SDK's back — the wander ladder or the
	// watchdog canceling the agent directly — returns context.Canceled
	// on a live context with no executor Cancel in play. The executor
	// must surface exactly one terminal Canceled carrying the kill
	// reason, or the consumer waits forever (#342).
	tests := []struct {
		name   string
		opts   []Option
		reason string
	}{
		{
			name:   "default reason",
			reason: "canceled",
		},
		{
			name:   "kill reason",
			opts:   []Option{WithCancelReason(func() string { return "wander kill: no todo progress" })},
			reason: "wander kill: no todo progress",
		},
		{
			name:   "empty reason falls back",
			opts:   []Option{WithCancelReason(func() string { return "" })},
			reason: "canceled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{err: context.Canceled}
			exec := NewExecutor(runner, "sess-1", tt.opts...)

			msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
			evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

			want := []a2aspec.TaskState{
				a2aspec.TaskStateSubmitted,
				a2aspec.TaskStateWorking,
				a2aspec.TaskStateCanceled,
			}
			require.Equal(t, want, states(t, evs), "an out-of-band cancel must end the task canceled")
			require.Equal(t, tt.reason, statusMessageText(t, evs[2]))
		})
	}
}

func TestExecuteOwnCancelEmitsOneTerminal(t *testing.T) {
	t.Parallel()

	// The executor's own Cancel emits the terminal Canceled status; the
	// run ending on that same cancel must not emit a second one.
	runner := &blockingCancelRunner{started: make(chan struct{}), kill: make(chan struct{})}
	exec := NewExecutor(runner, "sess-1")

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evsCh := make(chan []a2aspec.Event, 1)
	go func() {
		evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
	}()

	<-runner.started
	cancelEvs := collect(t, exec.Cancel(context.Background(), newExecCtx(nil)))

	var executeEvs []a2aspec.Event
	select {
	case executeEvs = <-evsCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the canceled run to end")
	}

	canceled := 0
	for _, st := range append(states(t, cancelEvs), states(t, executeEvs)...) {
		if st == a2aspec.TaskStateCanceled {
			canceled++
		}
	}
	require.Equal(t, 1, canceled, "exactly one Canceled across the Execute and Cancel sequences")
}

func TestExecuteEmptyPromptRejects(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("never")}
	exec := NewExecutor(runner, "sess-1")

	// A message with no text parts carries nothing to run.
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewDataPart(map[string]any{"k": "v"}))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateRejected,
	}
	require.Equal(t, want, states(t, evs))
	require.False(t, runner.ran, "runner.Run must not be called for an empty prompt")
}

func TestExecuteConsumerStopsBeforeRun(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("never")}
	exec := NewExecutor(runner, "sess-1")

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	// Stop consuming after the first (Submitted) event, as a disconnecting
	// consumer would.
	for range exec.Execute(context.Background(), newExecCtx(msg)) {
		break
	}

	require.False(t, runner.ran, "runner.Run must not be called after the consumer stops")
}

func TestExecuteExistingTaskSkipsSubmitted(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("done")}
	exec := NewExecutor(runner, "sess-1")

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	execCtx := newExecCtx(msg)
	execCtx.StoredTask = &a2aspec.Task{ID: "task-1", ContextID: "ctx-1"}

	evs := collect(t, exec.Execute(context.Background(), execCtx))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}
	require.Equal(t, want, states(t, evs), "no submitted for an existing task")
}

func TestCancel(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	exec := NewExecutor(runner, "sess-1")

	evs := collect(t, exec.Cancel(context.Background(), newExecCtx(nil)))

	require.Equal(t, "sess-1", runner.canceledFor)
	require.Equal(t, []a2aspec.TaskState{a2aspec.TaskStateCanceled}, states(t, evs))
}

func TestMessageText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  *a2aspec.Message
		want string
	}{
		{"nil message", nil, ""},
		{
			"single part",
			a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("hello")),
			"hello",
		},
		{
			"multiple parts joined by newline",
			a2aspec.NewMessage(a2aspec.MessageRoleUser,
				a2aspec.NewTextPart("line one"),
				a2aspec.NewTextPart("line two"),
			),
			"line one\nline two",
		},
		{
			// A nil parts entry is constructible from a remote peer's
			// JSON ([null, ...]) and must be skipped, not dereferenced.
			"nil part entry skipped",
			&a2aspec.Message{Parts: a2aspec.ContentParts{nil, a2aspec.NewTextPart("x")}},
			"x",
		},
		{
			// Non-text parts are ignored without leaving stray
			// separators between the surviving text parts.
			"non-text parts ignored",
			a2aspec.NewMessage(a2aspec.MessageRoleUser,
				a2aspec.NewTextPart("a"),
				a2aspec.NewDataPart(map[string]any{"k": "v"}),
				a2aspec.NewRawPart([]byte("zz")),
				a2aspec.NewTextPart("b"),
			),
			"a\nb",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, messageText(tc.msg))
		})
	}
}

// initTestRepo creates a git repo in a temp dir with one committed file and
// returns the dir. Skips the test when git is unavailable.
func initTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{
			"-C", dir,
			"-c", "user.name=test",
			"-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false",
		}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("original\n"), 0o644))
	run("init", "-q")
	run("add", "tracked.txt")
	run("commit", "-q", "-m", "initial")
	return dir
}

func TestGitDiff(t *testing.T) {
	t.Parallel()

	dir := initTestRepo(t)

	// Unstaged change to a tracked file, plus a brand-new untracked file:
	// both are the dispatched agent's work product and must appear.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("modified\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "created.txt"), []byte("new file\n"), 0o644))

	diff, err := GitDiff(dir)(t.Context())
	require.NoError(t, err)
	require.Contains(t, diff, "tracked.txt")
	require.Contains(t, diff, "modified")
	require.Contains(t, diff, "created.txt")
	require.Contains(t, diff, "new file")

	// The real index must be untouched: the new file stays untracked and
	// nothing is staged.
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "status", "--porcelain").Output()
	require.NoError(t, err)
	require.Contains(t, string(out), "?? created.txt")
	staged, err := exec.CommandContext(t.Context(), "git", "-C", dir, "diff", "--cached", "--name-only").Output()
	require.NoError(t, err)
	require.Empty(t, string(staged), "GitDiff must not stage anything in the real index")
}

func TestGitDiffCleanWorktree(t *testing.T) {
	t.Parallel()

	dir := initTestRepo(t)
	diff, err := GitDiff(dir)(t.Context())
	require.NoError(t, err)
	require.Empty(t, diff)
}

func TestGitDiffNotARepo(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	diff, err := GitDiff(t.TempDir())(t.Context())
	require.Error(t, err)
	require.Empty(t, diff)
}

// TestGitDiffIgnoresUserDiffConfig verifies GitDiff returns a plain
// unified diff even when the user's global git config forces an external
// diff driver and always-on color: the diff keeps its a/ b/ headers and
// carries no external-tool output or escape sequences.
func TestGitDiffIgnoresUserDiffConfig(t *testing.T) {
	// A shell script only runs on Unix; on Windows git would fail to
	// invoke it, so the external-diff half of the test cannot apply.
	if runtime.GOOS == "windows" {
		t.Skip("diff.external script requires a Unix shell")
	}

	dir := initTestRepo(t)

	// A global config that forces an external diff driver (a script
	// that prints EXTERNAL) and always-on color.
	cfg := t.TempDir()
	script := filepath.Join(cfg, "ext-diff.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho EXTERNAL\n"), 0o755))
	global := filepath.Join(cfg, "gitconfig")
	require.NoError(t, os.WriteFile(global,
		[]byte("[diff]\n\texternal = "+script+"\n[color]\n\tui = always\n"), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	// An unstaged change and a new file: both are the agent's work
	// product and must appear as a unified diff.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("modified\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "created.txt"), []byte("new file\n"), 0o644))

	diff, err := GitDiff(dir)(t.Context())
	require.NoError(t, err)
	require.Contains(t, diff, "+++ b/")
	require.Contains(t, diff, "tracked.txt")
	require.Contains(t, diff, "created.txt")
	require.NotContains(t, diff, "EXTERNAL")
	require.NotContains(t, diff, "\x1b")
}

// inactivityRunner is a runner whose Run blocks until released, so a
// test can hold a run silent (or feed it progress) and count how many
// times the executor canceled it.
type inactivityRunner struct {
	result  *fantasy.AgentResult
	release chan struct{}

	mu          sync.Mutex
	cancelCount int
}

func newInactivityRunner(result string) *inactivityRunner {
	return &inactivityRunner{
		result:  textResult(result),
		release: make(chan struct{}),
	}
}

func (r *inactivityRunner) Run(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	<-r.release
	return r.result, nil
}

func (r *inactivityRunner) Cancel(string) {
	r.mu.Lock()
	r.cancelCount++
	r.mu.Unlock()
}

func (r *inactivityRunner) cancels() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelCount
}

// The backstop's core case (#360): a silent runner that outlives the
// timeout gets runner.Cancel called exactly once and the run ends with
// exactly one Failed carrying the reason — no Canceled, which is
// reserved for the executor's Cancel.
func TestExecuteInactivitySilentRunFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newInactivityRunner("never")
		exec := NewExecutor(runner, "sess-1", WithInactivityTimeout(5*time.Minute))

		msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
		evsCh := make(chan []a2aspec.Event, 1)
		go func() {
			evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
		}()

		// The run never yields an event: with every goroutine durably
		// blocked, virtual time runs the backstop (5m) and the settle
		// window (2s) past, well inside this sleep.
		time.Sleep(6 * time.Minute)

		var evs []a2aspec.Event
		select {
		case evs = <-evsCh:
		default:
			t.Fatal("the backstop did not end the run")
		}

		want := []a2aspec.TaskState{
			a2aspec.TaskStateSubmitted,
			a2aspec.TaskStateWorking,
			a2aspec.TaskStateFailed,
		}
		require.Equal(t, want, states(t, evs))
		require.Equal(t, "inactivity timeout: no progress for 5m0s", statusMessageText(t, evs[2]))
		require.Equal(t, 1, runner.cancels(), "runner.Cancel must be called exactly once")

		close(runner.release)
	})
}

// A runner that emits a changed todo snapshot every half the timeout
// keeps resetting the backstop and runs to completion: a run that is
// visibly making progress must not trip it.
func TestExecuteInactivityProgressResetsTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newInactivityRunner("done")
		source := newPipeTodoSource()
		exec := NewExecutor(runner, "sess-1", WithTodos(source), WithInactivityTimeout(10*time.Second))

		msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
		evsCh := make(chan []a2aspec.Event, 1)
		go func() {
			evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
		}()

		// A changed snapshot every 4 virtual seconds — half the
		// timeout — resets the backstop each time, so it never fires
		// while the run makes progress.
		for i := 0; i < 3; i++ {
			source.ch <- todoSnapshot(runTodos(
				session.Todo{Content: fmt.Sprintf("step %d", i), Status: session.TodoStatusInProgress, ActiveForm: fmt.Sprintf("stepping %d", i)},
			))
			<-time.After(4 * time.Second)
		}
		close(runner.release)
		synctest.Wait()

		var evs []a2aspec.Event
		select {
		case evs = <-evsCh:
		default:
			t.Fatal("the run did not end")
		}
		want := []a2aspec.TaskState{
			a2aspec.TaskStateSubmitted,
			a2aspec.TaskStateWorking,
			todoState,
			todoState,
			todoState,
			a2aspec.TaskStateCompleted,
		}
		require.Equal(t, want, statesWithTodos(t, evs))
		require.Equal(t, 0, runner.cancels(), "a run that keeps making progress must not be canceled")
	})
}

// With inactivity_timeout=0 no timer runs and behavior is unchanged: a
// silent run is left alone no matter how long it stays quiet, and
// completes normally when the runner finishes.
func TestExecuteInactivityDisabledNoTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newInactivityRunner("done")
		exec := NewExecutor(runner, "sess-1", WithInactivityTimeout(0))

		msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
		evsCh := make(chan []a2aspec.Event, 1)
		go func() {
			evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
		}()

		// An hour of virtual silence: with the backstop disabled nothing
		// may fire, so the run must still be in flight.
		time.Sleep(time.Hour)
		select {
		case <-evsCh:
			t.Fatal("the run must not end while the runner is silent and the backstop is disabled")
		default:
		}

		close(runner.release)
		synctest.Wait()
		var evs []a2aspec.Event
		select {
		case evs = <-evsCh:
		default:
			t.Fatal("the run did not end")
		}

		want := []a2aspec.TaskState{
			a2aspec.TaskStateSubmitted,
			a2aspec.TaskStateWorking,
			a2aspec.TaskStateCompleted,
		}
		require.Equal(t, want, states(t, evs))
		require.Equal(t, 0, runner.cancels())
	})
}

// The diff is chunked, not shipped as one SSE line (#361): every chunk
// stays within diffChunkSize, chunks break at line boundaries where the
// lines allow, and concatenating them reproduces the diff byte for byte.
func TestChunkDiff(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, chunkDiff(""))
	})

	t.Run("small diff is one chunk", func(t *testing.T) {
		t.Parallel()
		diff := "diff --git a/x b/x\n@@\n+y\n"
		require.Equal(t, []string{diff}, chunkDiff(diff))
	})

	t.Run("multi-line diff breaks at line boundaries", func(t *testing.T) {
		t.Parallel()
		line := strings.Repeat("x", 1024) + "\n"
		var b strings.Builder
		for range 300 {
			b.WriteString(line)
		}
		diff := b.String()
		require.Greater(t, len(diff), diffChunkSize, "the fixture must exceed one chunk")

		chunks := chunkDiff(diff)
		require.Greater(t, len(chunks), 1)
		var reassembled strings.Builder
		for _, chunk := range chunks {
			require.LessOrEqual(t, len(chunk), diffChunkSize)
			require.True(t, strings.HasSuffix(chunk, "\n"), "chunks break at line boundaries")
			reassembled.WriteString(chunk)
		}
		require.Equal(t, diff, reassembled.String())
	})

	t.Run("a line longer than the cap is hard-split", func(t *testing.T) {
		t.Parallel()
		diff := strings.Repeat("x", 1024*1024) // 1 MiB, no newline
		chunks := chunkDiff(diff)
		require.Len(t, chunks, 4)
		require.Equal(t, diff, strings.Join(chunks, ""))
	})

	t.Run("an 11 MiB diff streams in chunks within the cap", func(t *testing.T) {
		t.Parallel()
		line := strings.Repeat("+", 4096) + "\n"
		var b strings.Builder
		for len(b.String()) < 11*1024*1024 {
			b.WriteString(line)
		}
		diff := b.String()

		chunks := chunkDiff(diff)
		require.Greater(t, len(chunks), 40, "an 11 MiB diff needs many chunks")
		var reassembled strings.Builder
		for _, chunk := range chunks {
			require.LessOrEqual(t, len(chunk), diffChunkSize)
			reassembled.WriteString(chunk)
		}
		require.Equal(t, diff, reassembled.String())
	})
}

// The executor emits the diff as a sequence of chunk updates on one named
// artifact: the first replaces, the rest append, the last closes, and the
// typed dispatch-result artifact follows (#361).
func TestExecuteDiffChunkedArtifact(t *testing.T) {
	t.Parallel()

	line := strings.Repeat("+", 4096) + "\n"
	var b strings.Builder
	for len(b.String()) < 11*1024*1024 {
		b.WriteString(line)
	}
	diff := "diff --git a/big.txt b/big.txt\n" + b.String()

	runner := &fakeRunner{result: textResult("done")}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return diff, nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	var chunks []*a2aspec.TaskArtifactUpdateEvent
	var res *a2aspec.TaskArtifactUpdateEvent
	for _, ev := range evs {
		art, ok := ev.(*a2aspec.TaskArtifactUpdateEvent)
		if !ok {
			continue
		}
		switch art.Artifact.ID {
		case DiffArtifactID:
			chunks = append(chunks, art)
		case ResultArtifactID:
			res = art
		}
	}
	require.NotEmpty(t, chunks)
	require.NotNil(t, res)

	var reassembled strings.Builder
	for i, chunk := range chunks {
		require.LessOrEqual(t, len(chunk.Artifact.Parts[0].Text()), diffChunkSize, "no chunk exceeds the SSE-safe cap")
		require.Equal(t, i > 0, chunk.Append, "only chunks after the first append")
		require.Equal(t, i == len(chunks)-1, chunk.LastChunk, "exactly the last chunk closes the artifact")
		reassembled.WriteString(chunk.Artifact.Parts[0].Text())
	}
	require.Equal(t, diff, reassembled.String(), "the chunks reassemble byte for byte")

	decoded, ok := decodeDispatchOutcome(res.Artifact.Parts[0])
	require.True(t, ok)
	require.Equal(t, len(diff), decoded.DiffBytes)
	require.Empty(t, decoded.DiffError)
	require.Positive(t, decoded.FilesChanged)
}
