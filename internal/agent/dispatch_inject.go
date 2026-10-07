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
	SessionID string `json:"session_id,omitempty" description:"Session ID of the running dispatched agent (the \"session_id\" from its dispatch handle)"`
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
// routing (#313), and from an A2A client's follow-up message to a running
// task (#351). Only the front door differs; delivery goes over the A2A
// protocol for every caller, so an out-of-process agent is steered the
// same way an in-process one is. FromSessionID is the addressing caller's
// own session (#399): when it is set and does not match the dispatch's
// parent session, delivery refuses, so one session's model cannot steer
// another session's agent. It stays empty for callers with no session
// behind them.
type AgentMessage struct {
	SessionID     string
	FromSessionID string
	Text          string
	Attachments   []message.Attachment
}

// DeliverAgentMessage delivers one message to the dispatched agent
// running on msg.SessionID as its next input (#312): it is enqueued
// behind the busy session — folded into the active turn at the next
// model step, or run as the immediate follow-up turn — and the response
// streams back to the parent chat block through the child session's
// message events.
//
// The steer rides the A2A protocol (#351): a message on the dispatch's
// running context, delivered through the process host's client, so an
// out-of-process agent is steered exactly like an in-process one. The
// served executor resolves the context to the running agent and enqueues
// the message through EnqueueWhenBusy; "working" means it was accepted
// into the queue.
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

	reg := c.dispatchRegistry()
	entry, ok := reg.BySession(msg.SessionID)
	if !ok {
		return fmt.Errorf("no running agent for session %s; dispatch one first", msg.SessionID)
	}

	// Scope to the caller's session (#399): a message carrying a
	// FromSessionID that does not own this dispatch refuses exactly like
	// an unknown one, so the caller learns nothing about the other
	// session's agent.
	if msg.FromSessionID != "" && entry.ParentSessionID != "" && entry.ParentSessionID != msg.FromSessionID {
		return fmt.Errorf("no running agent for session %s in this session; dispatch one first", msg.SessionID)
	}

	// A terminal entry refuses like an unknown one, but says which one
	// happened: task sessions are never continuable. A non-terminal entry
	// (running or provisioned) is still steerable.
	if entry.Status != dispatch.StatusRunning && entry.Status != dispatch.StatusProvisioned {
		return fmt.Errorf("agent %s finished (%s); task sessions are never continuable — dispatch a new agent instead", msg.SessionID, entry.Status)
	}

	// An external agent (#434) is not steerable. #351's steer is a
	// message on the dispatch's running context that crush's own
	// executor folds into the running turn, with the reply streaming
	// back on the dispatch's local session. A third-party agent has no
	// such contract: the same message starts a second task there, whose
	// reply nothing here reads. Refuse rather than pretend.
	if entry.Source != "" {
		return fmt.Errorf("%w; cancel the agent and dispatch it again with the new instructions", ErrSteerExternal)
	}

	steerer, ok := c.dispatchHost.(DispatchSteerer)
	if !ok || steerer == nil {
		return fmt.Errorf("no running agent for session %s; dispatch one first", msg.SessionID)
	}
	// The server stands up after the handle is returned (#426): in that
	// window the entry runs but nothing is listening on its context yet.
	// The agent is starting, not gone, so the refusal says to send the
	// message again rather than inviting a duplicate dispatch (#398).
	if entry.Endpoint == "" || entry.AgentCard == nil {
		return fmt.Errorf("agent %s is not ready for messages yet; send the message again in a moment", msg.SessionID)
	}

	var referenceTasks []string
	if entry.TaskID != "" {
		referenceTasks = []string{entry.TaskID}
	}
	outcome, err := steerer.SteerDispatch(ctx, DispatchSteerParams{
		Endpoint:         entry.Endpoint,
		Card:             entry.AgentCard,
		ContextID:        msg.SessionID,
		Text:             msg.Text,
		Attachments:      msg.Attachments,
		ReferenceTaskIDs: referenceTasks,
	})
	if err != nil {
		return err
	}
	if err := SteerOutcomeError(msg.SessionID, outcome); err != nil {
		return err
	}
	slog.Debug("Steered running agent over A2A", "session_id", msg.SessionID)
	return nil
}

// SteerOutcomeError maps a steer's A2A outcome onto the sender's error
// (#351): nil when the agent accepted the message, the refusal otherwise.
// Every front door that steers a dispatched agent — message_agent, the
// editor's @handle over the agent index (#421) — answers with it.
func SteerOutcomeError(sessionID string, outcome DispatchSteerOutcome) error {
	switch outcome.Status {
	case steerStatusWorking, steerStatusCompleted:
		return nil
	case steerStatusRejected:
		// A live run that cannot take the message yet refuses for now
		// (#398): the agent is still working, so the message can go
		// again in a moment, and a new dispatch would duplicate it.
		if outcome.Reason == SteerRefusalNotReady {
			return fmt.Errorf("agent %s is not ready for messages yet; send the message again in a moment", sessionID)
		}
		// Otherwise the run ended between the registry lookup and the
		// delivery. Same refusal as a finished dispatch.
		return fmt.Errorf("agent %s is no longer running; dispatch a new agent instead", sessionID)
	default:
		return fmt.Errorf("agent %s: %s", sessionID, outcome.Text)
	}
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
			from := tools.GetSessionFromContext(ctx)
			var err error
			switch {
			case params.SessionID != "" && params.Handle != "":
				// Both set: accept only when the handle names the same
				// session — a contradictory address is refused rather
				// than silently preferred one way (#400).
				handle := dispatch.HandleSlug(params.Handle)
				snap, ok := c.DispatchByHandle(from, handle)
				switch {
				case !ok:
					err = fmt.Errorf("no agent with handle @%s in this session; dispatch one first", handle)
				case snap.Entry.SessionID != params.SessionID:
					err = fmt.Errorf("handle @%s and session_id %s name different agents; provide exactly one of handle or session_id", handle, params.SessionID)
				default:
					err = c.DeliverAgentMessage(ctx, AgentMessage{
						SessionID:     params.SessionID,
						FromSessionID: from,
						Text:          params.Message,
					})
				}
			case params.SessionID != "":
				err = c.DeliverAgentMessage(ctx, AgentMessage{
					SessionID:     params.SessionID,
					FromSessionID: from,
					Text:          params.Message,
				})
			case params.Handle != "":
				err = c.DeliverAgentMessageByHandle(ctx, from, dispatch.HandleSlug(params.Handle), params.Message, nil)
			default:
				err = errors.New("provide exactly one of handle or session_id")
			}
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			addressed := params.SessionID
			if addressed == "" {
				addressed = "@" + dispatch.HandleSlug(params.Handle)
			}
			// Queued, not delivered (#398): the message reaches the agent
			// at its next step, and a run that ends first never reads it —
			// the dispatch's terminal result then lists it under
			// undelivered_steers.
			return fantasy.NewTextResponse(fmt.Sprintf(
				"Message queued for the running agent (%s). It lands as the agent's next input; the agent's response appears on its dispatch block in the chat. If the agent's run ends before it reads the message, the dispatch result lists it under undelivered_steers.",
				addressed,
			)), nil
		},
	)
}
