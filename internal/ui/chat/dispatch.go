package chat

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/tree"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
)

// -----------------------------------------------------------------------------
// Dispatch Agent Tool
// -----------------------------------------------------------------------------

// DispatchSnapshotSetter is implemented by the dispatch agent block; the
// UI model feeds it live snapshots from the dispatch-todos event stream
// (#65).
type DispatchSnapshotSetter interface {
	SetDispatchSnapshot(snap dispatch.TodoSnapshot)
}

// DispatchToolMessageItem is the agent block for a dispatch_agent tool
// call (#65): a live status card, not a static tool-call record. While
// the dispatched agent runs it shows the dispatch state, elapsed time,
// tokens, and the agent's current todo as the one-line activity, with
// the child session's tool activity nested underneath. On completion it
// becomes the durable record — the terminal DispatchResult's findings
// and diff stat — and never clears or collapses.
type DispatchToolMessageItem struct {
	*baseToolMessageItem

	nestedTools []ToolMessageItem
	// snapshot is the latest collector snapshot; nil until one arrives.
	snapshot *dispatch.TodoSnapshot
	// fallback is the dispatch state parsed from the persisted tool
	// result (the running handle) when no live snapshot has arrived —
	// a reloaded session, client/server mode, or a dispatch from an
	// earlier process. Its state is static by definition.
	fallback *dispatch.DispatchResult
	// steers records the mid-run messages injected into the running
	// agent (#312) and the agent's latest reply to each, as observed on
	// the child session's message events. The initial dispatch prompt is
	// not a steer and is never recorded. In-memory only: a reloaded
	// session loses the live steer log; the durable record remains the
	// terminal result's findings.
	steers []dispatchSteer
}

// dispatchSteer is one injected message and the agent's answer so far.
// ResponseMessageID ties the answer to the assistant message currently
// streaming on the child session; Response is its text snapshot, updated
// as the message grows.
type dispatchSteer struct {
	Text              string
	ResponseMessageID string
	Response          string
}

var (
	_ ToolMessageItem        = (*DispatchToolMessageItem)(nil)
	_ NestedToolContainer    = (*DispatchToolMessageItem)(nil)
	_ DispatchSnapshotSetter = (*DispatchToolMessageItem)(nil)
)

// NewDispatchToolMessageItem creates a new [DispatchToolMessageItem].
func NewDispatchToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) *DispatchToolMessageItem {
	d := &DispatchToolMessageItem{}
	d.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &DispatchToolRenderContext{dispatch: d}, canceled)
	d.fallback = parseDispatchResult(result)
	// The dispatch_agent tool call returns as soon as the run is
	// backgrounded, so the card's spinner keys off the dispatch
	// lifecycle, not the tool call: it spins while the dispatched agent
	// works and stops on the terminal states.
	d.spinningFunc = func(state SpinningState) bool {
		if state.IsCanceled() {
			return false
		}
		status, live := d.dispatchStatus()
		if isTerminalDispatchStatus(status) {
			return false
		}
		// A stale running state never spins: nothing will advance the
		// card again, so a spinner would freeze mid-frame.
		return live || !state.HasResult()
	}
	return d
}

// SetResult overrides the base to re-parse the dispatch handle, so a
// result arriving live updates the fallback state.
func (d *DispatchToolMessageItem) SetResult(res *message.ToolResult) {
	d.baseToolMessageItem.SetResult(res)
	d.fallback = parseDispatchResult(res)
}

// SetDispatchSnapshot stores the latest collector snapshot (#65) and
// bumps the item so the card re-renders.
func (d *DispatchToolMessageItem) SetDispatchSnapshot(snap dispatch.TodoSnapshot) {
	d.snapshot = &snap
	d.clearCache()
	d.Bump()
}

// DispatchSessionID returns the dispatched agent's task session ID —
// live from the snapshot, else from the persisted handle — which the UI
// uses to seed the card from the registry on session load.
func (d *DispatchToolMessageItem) DispatchSessionID() string {
	if d.snapshot != nil {
		return d.snapshot.Entry.SessionID
	}
	if d.fallback != nil {
		return d.fallback.SessionID
	}
	return ""
}

// Advance implements [Animatable].
//
// Advances the card's own spinner and every spinning nested tool in one
// frame, bumping the parent's list-cache version, exactly like the
// agent tool item: nested tools are not list entries of their own, so
// only the parent's version gates the list cache. Unlike the agent
// tool, the card keeps animating after its tool result arrives — the
// result is the running handle and the dispatch is still working.
func (d *DispatchToolMessageItem) Advance() bool {
	if !d.isSpinning() {
		return false
	}
	changed := d.anim.Advance()
	changed = advanceNested(d.nestedTools) || changed
	if changed {
		d.Bump()
	}
	return changed
}

// NestedTools returns the nested tools.
func (d *DispatchToolMessageItem) NestedTools() []ToolMessageItem {
	return d.nestedTools
}

// SetNestedTools sets the nested tools.
//
// SetNestedTools always bumps the version, for the same reason as the
// agent tool item: the live update path in internal/ui/model/ui.go
// mutates existing children in place and calls this with the same
// slice, so pointer-equality dedupe would leave a stale parent entry in
// the list cache.
func (d *DispatchToolMessageItem) SetNestedTools(tools []ToolMessageItem) {
	d.nestedTools = tools
	d.clearCache()
	d.Bump()
}

// AddNestedTool adds a nested tool.
func (d *DispatchToolMessageItem) AddNestedTool(tool ToolMessageItem) {
	// Mark nested tools as simple (compact) rendering.
	if s, ok := tool.(Compactable); ok {
		s.SetCompact(true)
	}
	d.nestedTools = append(d.nestedTools, tool)
	d.clearCache()
	d.Bump()
}

// dispatchStatus resolves the dispatch lifecycle state to render: live
// from the collector snapshot, else the state recorded in the persisted
// running handle, else queued while the tool call is still open. live
// reports whether the state comes from the registry and can still
// change.
func (d *DispatchToolMessageItem) dispatchStatus() (dispatch.Status, bool) {
	if d.snapshot != nil {
		return d.snapshot.Entry.Status, true
	}
	if d.fallback != nil {
		return d.fallback.Status, false
	}
	return dispatch.StatusProvisioned, true
}

// IsLive reports whether the dispatched agent is believed to be running
// right now, judged from in-memory state only so the UI can enumerate
// live agents without probing the workspace (#314): a registry snapshot
// that has not reached a terminal state, or a tool call that has not
// returned its running handle yet. A card seeded only from its persisted
// handle is static by definition and never reports live: the run may
// have ended, and in client/server mode the registry lives in another
// process.
func (d *DispatchToolMessageItem) IsLive() bool {
	status, live := d.dispatchStatus()
	if !live {
		return false
	}
	return !isTerminalDispatchStatus(status)
}

// terminalResult returns the terminal DispatchResult to render as the
// durable record, from the registry snapshot or, failing that, the
// persisted handle.
func (d *DispatchToolMessageItem) terminalResult() *dispatch.DispatchResult {
	if d.snapshot != nil && isTerminalDispatchStatus(d.snapshot.Entry.Status) {
		return d.snapshot.Entry.Result
	}
	if d.fallback != nil && isTerminalDispatchStatus(d.fallback.Status) {
		return d.fallback
	}
	return nil
}

// AddSteer records one message injected into the running agent (#312),
// as observed on the child session's message events, and bumps the card
// so the steer renders immediately.
func (d *DispatchToolMessageItem) AddSteer(text string) {
	d.steers = append(d.steers, dispatchSteer{Text: text})
	d.clearCache()
	d.Bump()
}

// UpdateSteerAnswer records the assistant message currently answering the
// latest steer: the message ID is retargeted whenever a later assistant
// message streams text, so the answer shown is always the agent's most
// recent reply on the child session. Reports whether anything changed.
func (d *DispatchToolMessageItem) UpdateSteerAnswer(messageID, text string) bool {
	if len(d.steers) == 0 {
		return false
	}
	steer := &d.steers[len(d.steers)-1]
	if steer.ResponseMessageID == messageID && steer.Response == text {
		return false
	}
	steer.ResponseMessageID = messageID
	steer.Response = text
	d.clearCache()
	d.Bump()
	return true
}

// IsInitialDispatchPrompt reports whether a child-session user message is
// the dispatch's own initial prompt rather than an injected steer: before
// any steer exists, the only user message that can equal the prompt text
// is the prompt itself. A steer whose text happens to be identical is
// indistinguishable and treated as the prompt — cosmetic, never a
// delivery concern.
func (d *DispatchToolMessageItem) IsInitialDispatchPrompt(text string) bool {
	if len(d.steers) > 0 || text == "" {
		return false
	}
	var params agent.DispatchAgentParams
	_ = json.Unmarshal([]byte(d.ToolCall().Input), &params)
	return params.Prompt == text
}

// Steers returns the recorded steers — the injected messages and the
// agent's answers so far.
func (d *DispatchToolMessageItem) Steers() []dispatchSteer {
	return d.steers
}

// elapsed returns the dispatch's run time: FinishedAt - StartedAt once
// terminal, time since start while running. Timestamps live on the
// registry entry, so a stale fallback state has none.
func (d *DispatchToolMessageItem) elapsed() (time.Duration, bool) {
	if d.snapshot == nil {
		return 0, false
	}
	start, end := d.snapshot.Entry.StartedAt, d.snapshot.Entry.FinishedAt
	if start.IsZero() {
		return 0, false
	}
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(start), true
}

// parseDispatchResult extracts the dispatch handle from a tool result,
// which is the DispatchResult JSON the tool returns (running handle or,
// for runs from an earlier process, whatever was persisted).
func parseDispatchResult(result *message.ToolResult) *dispatch.DispatchResult {
	if result == nil || result.Content == "" {
		return nil
	}
	var handle dispatch.DispatchResult
	if err := json.Unmarshal([]byte(result.Content), &handle); err != nil {
		return nil
	}
	return &handle
}

// isTerminalDispatchStatus reports whether status ends the dispatch.
func isTerminalDispatchStatus(status dispatch.Status) bool {
	switch status {
	case dispatch.StatusCompleted, dispatch.StatusFailed, dispatch.StatusKilled:
		return true
	default:
		return false
	}
}

// dispatchStateLabel maps a registry status to its card label. The
// card's vocabulary is the interaction model's (queued / working /
// complete / failed / killed); "killed" is reserved for wander kill
// (#316) and unreachable for now.
func dispatchStateLabel(status dispatch.Status) string {
	switch status {
	case dispatch.StatusProvisioned:
		return "queued"
	case dispatch.StatusRunning:
		return "working"
	case dispatch.StatusCompleted:
		return "complete"
	case dispatch.StatusFailed:
		return "failed"
	case dispatch.StatusKilled:
		return "killed"
	default:
		return string(status)
	}
}

// dispatchToolStatus maps the dispatch state onto the tool icon status
// the header renders.
func dispatchToolStatus(status dispatch.Status, canceled bool) ToolStatus {
	if canceled {
		return ToolStatusCanceled
	}
	switch status {
	case dispatch.StatusCompleted:
		return ToolStatusSuccess
	case dispatch.StatusFailed:
		return ToolStatusError
	case dispatch.StatusKilled:
		return ToolStatusCanceled
	default:
		return ToolStatusRunning
	}
}

// formatDispatchElapsed renders a run duration compactly: 42s, 2m14s,
// 1h04m.
func formatDispatchElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// formatDispatchTokens renders a token count with K/M units, matching
// the assistant info line's formatting.
func formatDispatchTokens(n int64) string {
	var s string
	switch {
	case n >= 1_000_000:
		s = fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		s = fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
	s = strings.Replace(s, ".0K", "K", 1)
	s = strings.Replace(s, ".0M", "M", 1)
	return s
}

// DispatchToolRenderContext renders dispatch agent blocks.
type DispatchToolRenderContext struct {
	dispatch *DispatchToolMessageItem
}

// RenderTool implements the [ToolRenderer] interface.
func (r *DispatchToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	d := r.dispatch
	if opts.IsPending() && d.snapshot == nil {
		return pendingTool(sty, "Dispatch", opts.Anim, opts.Compact)
	}

	var params agent.DispatchAgentParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)
	prompt := params.Prompt
	if !opts.ExpandedContent {
		prompt = strings.ReplaceAll(prompt, "\n", " ")
	}

	status, _ := d.dispatchStatus()
	header := toolHeader(sty, dispatchToolStatus(status, opts.IsCanceled()), "Dispatch", cappedWidth, opts, d.statusParams()...)
	if opts.Compact {
		return header
	}

	// Build the task tag and prompt, mirroring the agent tool block so
	// the two sub-agent surfaces read identically.
	taskTag := sty.Tool.AgentTaskTag.Render("Task")
	taskTagWidth := lipgloss.Width(taskTag)
	remainingWidth := min(cappedWidth-taskTagWidth-3, maxTextWidth-taskTagWidth-3)
	promptText := sty.Tool.AgentPrompt.Width(remainingWidth).Render(prompt)

	header = lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		"",
		lipgloss.JoinHorizontal(
			lipgloss.Left,
			taskTag,
			" ",
			promptText,
		),
	)
	if activity := d.activityLine(sty, remainingWidth); activity != "" {
		header = lipgloss.JoinVertical(lipgloss.Left, header, activity)
	}
	if convo := d.renderSteers(sty, remainingWidth); convo != "" {
		header = lipgloss.JoinVertical(lipgloss.Left, header, convo)
	}

	// Build tree with nested tool calls.
	childTools := tree.Root(header)
	for _, nestedTool := range d.nestedTools {
		childView := nestedTool.Render(remainingWidth)
		childTools.Child(childView)
	}

	var parts []string
	parts = append(parts, childTools.Enumerator(roundedEnumerator(2, taskTagWidth-5)).String())

	// Show animation while the dispatched agent works.
	if opts.IsSpinning {
		parts = append(parts, "", opts.Anim.Render())
	}

	result := lipgloss.JoinVertical(lipgloss.Left, parts...)

	// Durable completion (#65): the block stays, carrying the terminal
	// result's findings and diff stat. It never clears or collapses;
	// drill-in is #314's job, so the body expands with the item's
	// existing expand toggle.
	if terminal := d.terminalResult(); terminal != nil {
		body := renderDispatchTerminal(sty, terminal, cappedWidth-toolBodyLeftPaddingTotal, opts.ExpandedContent)
		return joinToolParts(result, body)
	}

	return result
}

// statusParams builds the header's status segment: state, handle,
// elapsed, tokens, and todo ratio, joined with middots. Empty when no
// dispatch state is known.
func (d *DispatchToolMessageItem) statusParams() []string {
	status, _ := d.dispatchStatus()
	if d.snapshot == nil && d.fallback == nil {
		return nil
	}
	var parts []string
	if handle := d.handleLabel(); handle != "" {
		parts = append(parts, handle)
	}
	parts = append(parts, dispatchStateLabel(status))
	if elapsed, ok := d.elapsed(); ok {
		parts = append(parts, formatDispatchElapsed(elapsed))
	}
	if tokens := d.totalTokens(); tokens > 0 {
		parts = append(parts, formatDispatchTokens(tokens)+" tokens")
	}
	if d.snapshot != nil && d.snapshot.TodoTotal > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d todos", d.snapshot.TodoCompleted, d.snapshot.TodoTotal))
	}
	return []string{strings.Join(parts, " · ")}
}

// handleLabel returns the dispatch's @handle for the status line;
// handles are assigned at dispatch (#313). The live snapshot is
// authoritative; a reloaded session falls back to the persisted handle.
func (d *DispatchToolMessageItem) handleLabel() string {
	if d.snapshot != nil {
		return d.snapshot.Entry.Handle
	}
	if d.fallback != nil {
		return d.fallback.Handle
	}
	return ""
}

// totalTokens returns the dispatched session's token usage so far.
func (d *DispatchToolMessageItem) totalTokens() int64 {
	if d.snapshot == nil {
		return 0
	}
	return d.snapshot.PromptTokens + d.snapshot.CompletionTokens
}

// activityLine renders the dispatched agent's current todo as the
// one-line activity, using the todo list's idiom. Only a live
// non-terminal dispatch has a meaningful current todo.
func (d *DispatchToolMessageItem) activityLine(sty *styles.Styles, width int) string {
	if d.snapshot == nil || d.snapshot.CurrentTodo == "" || isTerminalDispatchStatus(d.snapshot.Entry.Status) {
		return ""
	}
	text := ansi.Truncate(d.snapshot.CurrentTodo, width-2, "…")
	return sty.Tool.TodoInProgressIcon.Render(styles.ArrowRightIcon+" ") +
		sty.Tool.TodoJustStarted.Render(text)
}

// renderSteers renders the mid-run injection conversation (#312): each
// injected message with the agent's answer so far beneath it. Plain text,
// not markdown — the answer streams token by token and a glamour
// re-render per delta would cost more than the line is worth; the full
// reply is in the terminal record's findings once the run completes.
func (d *DispatchToolMessageItem) renderSteers(sty *styles.Styles, width int) string {
	if len(d.steers) == 0 {
		return ""
	}
	var sections []string
	for _, steer := range d.steers {
		q := sty.Tool.TodoInProgressIcon.Render(styles.ArrowRightIcon+" ") +
			sty.Tool.TodoJustStarted.Render(steer.Text)
		sections = append(sections, q)
		if steer.Response != "" {
			sections = append(sections, toolOutputPlainContent(sty, steer.Response, width, true))
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

// renderDispatchTerminal renders the completed block's durable record:
// the failure reason on failed runs, the kill reason on killed runs
// (#316), the key findings, and the diff stat. Collapsed output is
// line-capped like any tool body; expanded shows everything.
func renderDispatchTerminal(sty *styles.Styles, res *dispatch.DispatchResult, width int, expanded bool) string {
	var sections []string
	if res.KilledReason != "" {
		note := sty.Tool.TodoStatusNote.Render("Killed")
		sections = append(sections, lipgloss.JoinHorizontal(lipgloss.Left, note, " "+sty.Tool.ErrorMessage.Render(res.KilledReason)))
	}
	if res.Error != "" {
		note := sty.Tool.TodoStatusNote.Render("Error")
		sections = append(sections, lipgloss.JoinHorizontal(lipgloss.Left, note, " "+sty.Tool.ErrorMessage.Render(res.Error)))
	}
	if res.KeyFindings != "" {
		note := sty.Tool.TodoStatusNote.Render("Findings")
		body := toolOutputMarkdownContent(sty, res.KeyFindings, width, expanded)
		sections = append(sections, lipgloss.JoinVertical(lipgloss.Left, note, body))
	}
	if res.DiffSummary != "" {
		note := sty.Tool.TodoStatusNote.Render("Diff")
		body := toolOutputPlainContent(sty, res.DiffSummary, width, expanded)
		sections = append(sections, lipgloss.JoinVertical(lipgloss.Left, note, body))
	}
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}
