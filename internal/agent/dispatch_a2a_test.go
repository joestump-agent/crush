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

// fakeDispatchHost records the A2A servers the coordinator asks it to
// start (#70) and hands back a canned endpoint/card pair with a counting
// stop. The embedded runnerTransport supplies StreamDispatch, driving
// the recorded runner the way the executor does, so a dispatch served by
// this host runs to completion.
type fakeDispatchHost struct {
	runnerTransport

	mu      sync.Mutex
	fail    bool
	started []DispatchServerParams
	stopped int
}

func (f *fakeDispatchHost) StartDispatchServer(ctx context.Context, params DispatchServerParams) (string, any, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", nil, nil, errors.New("no free port")
	}
	f.started = append(f.started, params)
	// The embedded transport's StreamDispatch drives the last served
	// params, so the server half records them too.
	f.serve(params)
	stop := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped++
	}
	return "http://127.0.0.1:19999", "fake-card", stop, nil
}

func (f *fakeDispatchHost) snapshot() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started), f.stopped
}

func (f *fakeDispatchHost) lastParams() DispatchServerParams {
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
	host := &fakeDispatchHost{}
	c.SetDispatchHost(host)
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
	params := host.lastParams()
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
	started, stopped := host.snapshot()
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

// A failed server start is a dispatch failure (#347): the tool reports
// the error, the registry entry and workspace are removed, and the
// agent never runs — nothing runs unserved.
func TestDispatchServerStartFailureFailsDispatch(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	c.SetDispatchHost(&fakeDispatchHost{fail: true})
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "start A2A server: no free port")

	require.Empty(t, c.dispatchRegistry().List())
	require.False(t, agent.ranOnce())
}

// A coordinator with no A2A host refuses dispatches at the tool (#347):
// there is no execution path behind it, so nothing is provisioned and
// nothing runs.
func TestDispatchWithoutHostIsRefused(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	c.SetDispatchHost(nil)
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "dispatch unavailable: no A2A host")

	require.Empty(t, c.dispatchRegistry().List())
	require.False(t, agent.ranOnce())
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
