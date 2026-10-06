package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
)

//go:embed templates/message_agent.md
var messageAgentToolDescription string

// MessageAgentToolName is the registered name of the MessageAgent tool.
const MessageAgentToolName = "message_agent"

// MessageAgentParams are the MessageAgent tool's arguments. Address the
// agent by task session or by its @handle (#313); exactly one is needed.
type MessageAgentParams struct {
	// SessionID is the dispatched agent's task session — the "session_id"
	// field of the running handle the dispatch_agent tool returned.
	SessionID string `json:"session_id" description:"Session ID of the running dispatched agent (the \"session_id\" from its dispatch handle)"`
	// Handle is the dispatched agent's @handle (#313) — the "handle"
	// field of the dispatch handle, or the handle the user addressed.
	Handle string `json:"handle,omitempty" description:"@handle of the running dispatched agent (the \"handle\" from its dispatch handle)"`
	// Message is the text to deliver. It lands as the agent's next input
	// while the agent keeps running — a steer, not a new task.
	Message string `json:"message" description:"The message to deliver to the running agent; it lands as the agent's next input"`
}

// AgentMessage is one message addressed to a running dispatched agent
// (#312). SessionID is the agent's task session — the "session_id" of the
// dispatch handle — and Text is the body. Attachments ride along exactly
// like a typed prompt's.
//
// The shape is the transport-agnostic seam: the same message flows from
// the model's message_agent tool call, from the editor's leading @handle
// routing (#313), and — when #71 swaps the transport — from an A2A
// client's follow-up message to a running task. Only the front door
// differs; the queue and this payload are shared. FromSessionID is the
// addressing caller's own session (#399): when it is set and does not
// match the dispatch's parent session, delivery refuses, so one
// session's model cannot steer another session's agent. It stays empty
// for callers with no session behind them.
type AgentMessage struct {
	SessionID     string
	FromSessionID string
	Text          string
	Attachments   []message.Attachment
}

// injectableAgent is the slice of SessionAgent the injection queue
// delivers through: enqueue-only-while-busy, so a delivered message can
// never start a fresh turn on a task session that stopped running.
type injectableAgent interface {
	EnqueueWhenBusy(call SessionAgentCall) bool
}

// runningDispatch is one in-flight dispatched run the queue can address:
// the injectable agent plus the call shaping captured at dispatch time,
// so an injected message runs under the same model options as the run.
// Entries live from the background run's start to its return — handle
// lifetime is run lifetime (#312's refusal rule).
type runningDispatch struct {
	sessionID string
	agent     injectableAgent
	baseCall  SessionAgentCall
	// findings is the run's turn-text record (#397): a steer enqueued
	// behind a finished work turn runs as a follow-up turn, and its
	// reply is kept out of the findings.
	findings *dispatchFindings
}

// registerDispatchRun makes the dispatched agent running on sessionID
// addressable for mid-run injection, capturing the run's call shaping
// and findings record.
func (c *coordinator) registerDispatchRun(sessionID string, agent injectableAgent, baseCall SessionAgentCall, findings *dispatchFindings) {
	if sessionID == "" || agent == nil {
		return
	}
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	if c.dispatchRuns == nil {
		c.dispatchRuns = make(map[string]*runningDispatch)
	}
	c.dispatchRuns[sessionID] = &runningDispatch{sessionID: sessionID, agent: agent, baseCall: baseCall, findings: findings}
}

// unregisterDispatchRun drops the injection target for sessionID when the
// dispatch it belongs to finishes. The map key is the run's, so a stale
// entry from a removed dispatch never intercepts a later session's ID.
func (c *coordinator) unregisterDispatchRun(sessionID string) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	delete(c.dispatchRuns, sessionID)
}

// DeliverAgentMessage delivers one message to the dispatched agent
// running on msg.SessionID as its next input (#312): it is enqueued
// behind the busy session — folded into the active turn at the next
// model step, or run as the immediate follow-up turn — and the response
// streams back to the parent chat block through the child session's
// message events.
//
// Addressing a finished or unknown session is a clean refusal, never an
// error path that starts a new run: task sessions are never continuable;
// dispatch a new agent instead. Delivery order is the queue's FIFO order
// and the queue is safe for concurrent use.
func (c *coordinator) DeliverAgentMessage(ctx context.Context, msg AgentMessage) error {
	if msg.SessionID == "" {
		return errors.New("session id is required")
	}
	if strings.TrimSpace(msg.Text) == "" {
		return errors.New("message text is required")
	}

	c.dispatchMu.Lock()
	target := c.dispatchRuns[msg.SessionID]
	c.dispatchMu.Unlock()
	reg := c.dispatchRegistry()

	// Scope to the caller's session (#399): a message carrying a
	// FromSessionID that does not own this dispatch refuses exactly like
	// an unknown one, so the caller learns nothing about the other
	// session's agent.
	if msg.FromSessionID != "" {
		if entry, ok := reg.BySession(msg.SessionID); ok && entry.ParentSessionID != "" && entry.ParentSessionID != msg.FromSessionID {
			return fmt.Errorf("no running agent for session %s in this session; dispatch one first", msg.SessionID)
		}
	}

	if target == nil {
		// Distinguish a finished dispatch from an unknown session so the
		// refusal tells the caller which one happened. A non-terminal
		// registry entry (e.g. the window between the dispatch handle
		// being returned and the background run registering its injection
		// target) must not claim "finished": the entry is not done, and
		// the caller would be told to dispatch a duplicate.
		if entry, ok := reg.BySession(msg.SessionID); ok && entry.Status != dispatch.StatusRunning && entry.Status != dispatch.StatusProvisioned {
			return fmt.Errorf("agent %s finished (%s); task sessions are never continuable — dispatch a new agent instead", msg.SessionID, entry.Status)
		}
		return fmt.Errorf("no running agent for session %s; dispatch one first", msg.SessionID)
	}

	// The injected call inherits the run's shaping (model options, width,
	// non-interactive) with the message as its prompt. RunID and accept
	// state stay empty: a RunID-bearing queued call runs as its own turn
	// with its own lifecycle, while an untracked one folds into the
	// running agent's next input — the injection contract. Steer marks
	// the persisted message as an injection (#410), so the dispatch card
	// never mistakes it for the initial prompt or a todo nudge.
	call := target.baseCall
	call.Prompt = msg.Text
	call.Attachments = msg.Attachments
	call.RunID = ""
	call.Steer = true
	call.Accepted = nil
	call.OnComplete = nil
	// The steer's own turn observer (#397): a steer folded into the
	// running turn is part of the work turn and inherits its observer;
	// a steer that lands as the follow-up turn records its reply in the
	// run's findings instead of replacing the work's.
	call.turnText = target.findings.addSteerReply

	if !target.agent.EnqueueWhenBusy(call) {
		// The run ended between the registry lookup and the enqueue — the
		// target's unregister is racing us. Same refusal as a finished
		// dispatch.
		return fmt.Errorf("agent %s is no longer running; dispatch a new agent instead", msg.SessionID)
	}

	slog.Debug("Injected message into running agent", "session_id", msg.SessionID)
	return nil
}

// messageAgentTool builds the MessageAgent tool (#312): the model-facing
// front door of the injection queue. One mechanism, two front doors — the
// editor's leading @handle routing is the human's (#313); this is the
// model's. The tool returns once the message is queued; the agent's
// response arrives on the dispatch block in the chat, not as the tool's
// result.
func (c *coordinator) messageAgentTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		MessageAgentToolName,
		messageAgentToolDescription,
		func(ctx context.Context, params MessageAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var err error
			switch {
			case params.SessionID != "":
				err = c.DeliverAgentMessage(ctx, AgentMessage{
					SessionID:     params.SessionID,
					FromSessionID: tools.GetSessionFromContext(ctx),
					Text:          params.Message,
				})
			case params.Handle != "":
				err = c.DeliverAgentMessageByHandle(ctx, tools.GetSessionFromContext(ctx), dispatch.HandleSlug(params.Handle), params.Message, nil)
			default:
				err = errors.New("session id or handle is required")
			}
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			addressed := params.SessionID
			if addressed == "" {
				addressed = "@" + dispatch.HandleSlug(params.Handle)
			}
			return fantasy.NewTextResponse(fmt.Sprintf(
				"Message delivered to the running agent (%s). It lands as the agent's next input; the agent's response appears on its dispatch block in the chat.",
				addressed,
			)), nil
		},
	)
}
