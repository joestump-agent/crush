package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/stretchr/testify/require"
)

// fakeServerStarter records the A2A servers the coordinator asks it to
// start (#70) and hands back a canned endpoint/card pair with a counting
// stop.
type fakeServerStarter struct {
	mu      sync.Mutex
	fail    bool
	started []DispatchServerParams
	stopped int
}

func (f *fakeServerStarter) StartDispatchServer(ctx context.Context, params DispatchServerParams) (string, any, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", nil, nil, errors.New("no free port")
	}
	f.started = append(f.started, params)
	stop := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped++
	}
	return "http://127.0.0.1:19999", "fake-card", stop, nil
}

func (f *fakeServerStarter) snapshot() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started), f.stopped
}

func (f *fakeServerStarter) lastParams() DispatchServerParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.started) == 0 {
		panic("no server was started")
	}
	return f.started[len(f.started)-1]
}

// Dispatching starts an in-process A2A server for the agent (#70),
// stamps its endpoint and card on the registry entry — the in-memory
// discovery surface — and tears both down when the run completes: no
// dead endpoints, no leaked servers.
func TestDispatchStartsAndStopsA2AServer(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	starter := &fakeServerStarter{}
	c.SetDispatchServerStarter(starter)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the bug",
		Branch: "main",
		Handle: "tester",
		Role:   "writes tests",
	}))
	agent.waitRunning(t)

	require.Eventually(t, func() bool {
		entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
		return ok && entry.Endpoint == "http://127.0.0.1:19999"
	}, 10*time.Second, 50*time.Millisecond)
	entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
	require.True(t, ok)
	require.Equal(t, "fake-card", entry.AgentCard)

	// The server was told the dispatch's identity and wiring: the
	// assigned handle as the card name, the role as the description, the
	// task session, the dispatched agent, and the todo collector.
	params := starter.lastParams()
	require.Equal(t, handle.SessionID, params.SessionID)
	require.Equal(t, "tester", params.Name)
	require.Equal(t, "writes tests", params.Description)
	require.NotNil(t, params.Runner)
	require.NotNil(t, params.Todos)
	require.NotNil(t, params.Diff)

	// The run finishes: the server stops and the entry stops advertising
	// an endpoint — discovery must not hand out a dead one.
	agent.release()
	require.Eventually(t, func() bool {
		entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
		return ok && entry.Status.IsTerminal() && entry.Endpoint == "" && entry.AgentCard == nil
	}, 10*time.Second, 50*time.Millisecond)
	started, stopped := starter.snapshot()
	require.Equal(t, 1, started)
	require.Equal(t, 1, stopped)

	// Resolve-style reads on the cleared entry find nothing.
	entry, ok = c.dispatchRegistry().Get(handle.DispatchID)
	require.True(t, ok)
	_, _, ok = entryEndpoint(entry)
	require.False(t, ok)
}

// entryEndpoint is the agent-side view of a2a.Resolve for tests: an
// entry serves A2A only while it carries both an endpoint and a card.
func entryEndpoint(entry dispatch.Entry) (string, any, bool) {
	if entry.Endpoint == "" || entry.AgentCard == nil {
		return "", nil, false
	}
	return entry.Endpoint, entry.AgentCard, true
}

// A failed server start never fails the dispatch (#70): Phase 1 has no
// A2A client in the loop, so the dispatch runs unserved and the failure
// is only logged.
func TestDispatchServerStartFailureIsNonFatal(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	c.SetDispatchServerStarter(&fakeServerStarter{fail: true})
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	require.Equal(t, dispatch.StatusRunning, handle.Status)

	entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
	require.True(t, ok)
	require.Empty(t, entry.Endpoint)
	require.Nil(t, entry.AgentCard)

	agent.release()
}

// Without a wired starter (the default for direct-struct tests and any
// caller that does not opt in), dispatches simply run unserved.
func TestDispatchWithoutStarterRunsUnserved(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	require.Nil(t, c.dispatchServerStarter())
	tool := c.dispatchTool()

	decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)

	for _, entry := range c.dispatchRegistry().List() {
		require.Empty(t, entry.Endpoint)
	}
	agent.release()
}

// resolvedSkills loads every discovered skill when nothing was requested
// and exactly the requested ones otherwise.
func TestResolvedSkills(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	store := c.cfg
	all, _ := discoverSkills(store)
	if len(all) > 0 {
		require.Equal(t, all, resolvedSkills(store, nil), "no request means every discovered skill")
		require.Len(t, resolvedSkills(store, []string{all[0].Name}), 1, "a request narrows to exactly the named skill")
	}
	require.Empty(t, resolvedSkills(store, []string{"no-such-skill"}))
	require.Nil(t, resolvedSkills(nil, nil))
}
