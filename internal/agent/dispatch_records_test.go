package agent

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
)

// fakeRecordStore stands in for the coordinator's durable dispatch
// records (#355): it answers the redelivery list from seeded memory and
// records the stamps it is given.
type fakeRecordStore struct {
	mu sync.Mutex
	// undelivered is what UndeliveredTerminal answers.
	undelivered []dispatch.UndeliveredDispatch
	// delivered records every RecordDelivered dispatch ID in order.
	delivered []string
	// errors, when set, fails every read with it.
	err error
}

func (f *fakeRecordStore) RecordStarted(dispatch.DispatchResult, string) error { return nil }

func (f *fakeRecordStore) RecordTask(_, _ string) error { return nil }

func (f *fakeRecordStore) RecordTerminal(dispatch.DispatchResult) error { return nil }

func (f *fakeRecordStore) RecordDelivered(dispatchID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, dispatchID)
	return nil
}

func (f *fakeRecordStore) UndeliveredTerminal() ([]dispatch.UndeliveredDispatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]dispatch.UndeliveredDispatch(nil), f.undelivered...), nil
}

func TestReconcileDispatchDeliveries(t *testing.T) {
	c, main, parentID := newDeliveryEnv(t)
	// The parent's dispatch_agent tool result still holds the running
	// handle: the run that would have stamped it died with the process.
	card, err := c.messages.Create(t.Context(), parentID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: "call-alive",
			Name:       DispatchAgentToolName,
			Content:    `{"dispatch_id":"d-alive","status":"running"}`,
		}},
	})
	require.NoError(t, err)
	records := &fakeRecordStore{
		undelivered: []dispatch.UndeliveredDispatch{
			{
				ParentSessionID: parentID,
				Result: dispatch.DispatchResult{
					DispatchID:  "d-alive",
					Branch:      "crush-dispatch-d-alive",
					SessionID:   "msg-alive$$call-alive",
					Status:      dispatch.StatusCompleted,
					KeyFindings: "finished before the crash",
				},
			},
			{
				ParentSessionID: "gone-parent",
				Result: dispatch.DispatchResult{
					DispatchID: "d-gone",
					Status:     dispatch.StatusCompleted,
				},
			},
		},
	}
	c.dispatchRecords = records

	c.ReconcileDispatchDeliveries(t.Context())

	// The surviving parent got exactly one delivery turn carrying the
	// undelivered payload; the gone parent's payload was dropped for
	// good, stamped so no restart retries it.
	require.Eventually(t, func() bool {
		return main.runCount() == 1
	}, 10*time.Second, 50*time.Millisecond)
	run := main.lastRun()
	require.Equal(t, parentID, run.SessionID)
	require.True(t, run.HiddenUserMessage)
	require.Contains(t, run.Prompt, `"dispatch_id": "d-alive"`)
	require.Contains(t, run.Prompt, `"key_findings": "finished before the crash"`)

	// The reconcile stamped the terminal result on the card's tool
	// result, the durable record a reloaded card renders (#410, #421).
	stamped, err := c.messages.Get(t.Context(), card.ID)
	require.NoError(t, err)
	var terminal dispatch.DispatchResult
	require.NoError(t, json.Unmarshal([]byte(stamped.ToolResults()[0].Metadata), &terminal))
	require.Equal(t, "d-alive", terminal.DispatchID)
	require.Equal(t, dispatch.StatusCompleted, terminal.Status)
	require.Equal(t, `{"dispatch_id":"d-alive","status":"running"}`, stamped.ToolResults()[0].Content, "the handle the model saw is untouched")

	require.Eventually(t, func() bool {
		records.mu.Lock()
		defer records.mu.Unlock()
		return len(records.delivered) == 2
	}, 10*time.Second, 50*time.Millisecond)
	records.mu.Lock()
	defer records.mu.Unlock()
	require.ElementsMatch(t, []string{"d-alive", "d-gone"}, records.delivered)
}

func TestReconcileDispatchDeliveriesNilRecords(t *testing.T) {
	c, main, _ := newDeliveryEnv(t)
	c.dispatchRecords = nil

	c.ReconcileDispatchDeliveries(t.Context())
	require.Zero(t, main.runCount())
}

// newAgentRecordStore opens the production record store (#355) over a
// session database of its own, so a test reads the same redelivery list
// the next startup's reconcile would.
func newAgentRecordStore(t *testing.T) *dispatch.SQLiteRecordStore {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return dispatch.NewSQLiteRecordStore(conn, "host|1|test")
}

// seedTerminalRecord writes dispatchID's record through start and
// terminal, as runDispatch leaves it before the parent's delivery, and
// returns the terminal payload to deliver.
func seedTerminalRecord(t *testing.T, records dispatch.DispatchRecords, parentID, dispatchID string) dispatch.DispatchResult {
	t.Helper()
	running := dispatch.DispatchResult{
		DispatchID: dispatchID,
		Branch:     "crush-dispatch-" + dispatchID,
		SessionID:  "s-" + dispatchID,
		Status:     dispatch.StatusRunning,
	}
	require.NoError(t, records.RecordStarted(running, parentID))
	terminal := running
	terminal.Status = dispatch.StatusCompleted
	terminal.KeyFindings = "finished " + dispatchID
	require.NoError(t, records.RecordTerminal(terminal))
	return terminal
}

// undeliveredIDs lists the dispatch IDs the next startup's reconcile
// would re-deliver.
func undeliveredIDs(t *testing.T, records dispatch.DispatchRecords) []string {
	t.Helper()
	undelivered, err := records.UndeliveredTerminal()
	require.NoError(t, err)
	ids := make([]string, 0, len(undelivered))
	for _, u := range undelivered {
		ids = append(ids, u.Result.DispatchID)
	}
	return ids
}

// trackDispatchSpawns installs the spawn seam (#422): every background
// run the coordinator starts — a delivery turn, a delivered stamp — is
// counted as it starts and signals done when it returns, so a test joins
// each one on a channel instead of polling for its effect.
func trackDispatchSpawns(c *coordinator) (*atomic.Int32, <-chan struct{}) {
	var started atomic.Int32
	done := make(chan struct{}, 8)
	c.spawnDispatch = func(f func()) {
		started.Add(1)
		go func() {
			defer func() { done <- struct{}{} }()
			f()
		}()
	}
	return &started, done
}

// awaitSpawn waits for the next tracked background run to return; the
// bound only turns a hang into a failure.
func awaitSpawn(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never returned", what)
	}
}

// queueingMainAgent runs a real sessionAgent as the coordinator's main
// agent, so a delivery that finds the parent busy goes through the real
// queue and its real OnConsumed fire sites (#351). The model and tool
// refresh c.run performs before every turn is a no-op, keeping the fake
// model in place. staleIdle answers the next busy check idle, once: the
// flush's check raced the parent's next turn, the window the queued
// delivery branch exists for.
type queueingMainAgent struct {
	*sessionAgent
	staleIdle atomic.Bool
}

func (a *queueingMainAgent) SetModels(_, _ Model) {}

func (a *queueingMainAgent) SetTools(_ []fantasy.AgentTool) {}

func (a *queueingMainAgent) IsSessionBusy(sessionID string) bool {
	if a.staleIdle.CompareAndSwap(true, false) {
		return false
	}
	return a.sessionAgent.IsSessionBusy(sessionID)
}

// The review gap on #355: a delivery queued behind a busy parent was
// never stamped, so every restart re-delivered it. The stamp now rides
// the queued call's OnConsumed: nothing is stamped while the call sits
// queued — a crash there must leave the payload to the reconcile — and
// once the parent's turn ends and the handoff consumes the call, the
// record is stamped, so neither the redelivery list nor a later
// startup's reconcile delivers it again.
func TestQueuedDeliveryStampedWhenConsumed(t *testing.T) {
	c, fake, parentID := newDeliveryEnv(t)
	records := newAgentRecordStore(t)
	c.dispatchRecords = records
	terminal := seedTerminalRecord(t, records, parentID, "d-queued")
	spawns, spawned := trackDispatchSpawns(c)

	// The large model keeps the fake's configured provider, so c.run
	// resolves it; the turns themselves run on the two-step model.
	model := &twoStepEchoModel{}
	echo, echoState := newGatedEchoTool()
	large := fake.model
	large.Model = model
	small := fake.model
	small.Model = &finishStreamModel{text: "title"}
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel: large,
		SmallModel: small,
		IsYolo:     true,
		IsSubAgent: true,
		Sessions:   c.sessions,
		Messages:   c.messages,
		Tools:      []fantasy.AgentTool{echo},
	}).(*sessionAgent)
	// Wait for its title generation before the env closes the database.
	t.Cleanup(sa.titles.Wait)
	main := &queueingMainAgent{sessionAgent: sa}
	c.agents[config.AgentCoder] = main
	c.mainAgent = main

	// The parent's own turn holds the session busy at its tool call.
	runDone := make(chan error, 1)
	go func() {
		_, err := sa.Run(t.Context(), SessionAgentCall{SessionID: parentID, Prompt: "work", NonInteractive: true})
		runDone <- err
	}()
	select {
	case <-echoState.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the parent's turn never reached its tool call")
	}

	main.staleIdle.Store(true)
	c.deliverDispatchResult(t.Context(), parentID, terminal)
	awaitSpawn(t, spawned, "the delivery flush")

	queued, _ := sa.messageQueue.Get(parentID)
	require.Len(t, queued, 1, "the delivery must wait in the parent's queue")
	require.True(t, queued[0].systemDelivery)
	require.NotNil(t, queued[0].OnConsumed)
	require.Equal(t, []string{"d-queued"}, undeliveredIDs(t, records),
		"a queued delivery must not be stamped before the queue consumes it")

	// The parent's turn ends; the handoff dequeues the delivery into its
	// own turn, and the verdict stamps the record.
	close(echoState.gate)
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the parent's turn and the delivery turn never finished")
	}
	awaitSpawn(t, spawned, "the delivered stamp")
	require.Empty(t, undeliveredIDs(t, records), "a consumed delivery must be stamped delivered")
	require.Equal(t, 4, model.stepsCount(), "the parent's turn and the delivery turn each serve two steps")

	msgs, err := c.messages.List(t.Context(), parentID)
	require.NoError(t, err)
	var persisted int
	for _, msg := range msgs {
		if msg.Role == message.User && strings.Contains(msg.Content().Text, `"dispatch_id": "d-queued"`) {
			persisted++
		}
	}
	require.Equal(t, 1, persisted, "the payload must reach the parent exactly once")

	// A restart's reconcile finds nothing to re-deliver: no turn starts.
	c.ReconcileDispatchDeliveries(t.Context())
	require.Equal(t, int32(2), spawns.Load(), "reconcile must not re-deliver a consumed delivery")
}

// A queued delivery the queue drops unrun reports false, and the record
// stays unstamped: the next startup's reconcile re-delivers it from the
// durable record. No removal path drops a system delivery today (#388
// exempts them from every one), so the fake hands the verdict over
// directly; the stamp must still never land on it.
func TestQueuedDeliveryDroppedStaysUndelivered(t *testing.T) {
	c, main, parentID := newDeliveryEnv(t)
	records := newAgentRecordStore(t)
	c.dispatchRecords = records
	terminal := seedTerminalRecord(t, records, parentID, "d-dropped")
	spawns, spawned := trackDispatchSpawns(c)

	main.queue.Store(true)
	c.deliverDispatchResult(t.Context(), parentID, terminal)
	awaitSpawn(t, spawned, "the delivery flush")

	queued := main.lastRun()
	require.True(t, queued.systemDelivery)
	require.NotNil(t, queued.OnConsumed, "the queued delivery must carry the stamp's verdict hook")

	queued.OnConsumed(false)
	require.Equal(t, int32(1), spawns.Load(), "a dropped delivery must not start a stamp")
	require.Equal(t, []string{"d-dropped"}, undeliveredIDs(t, records),
		"a dropped delivery must stay undelivered")

	// The next startup's reconcile re-delivers it, and that delivery's
	// success stamps it.
	main.queue.Store(false)
	c.ReconcileDispatchDeliveries(t.Context())
	awaitSpawn(t, spawned, "the reconcile's delivery")
	require.Equal(t, 2, main.runCount())
	require.Contains(t, main.lastRun().Prompt, `"dispatch_id": "d-dropped"`)
	require.Empty(t, undeliveredIDs(t, records))
}
