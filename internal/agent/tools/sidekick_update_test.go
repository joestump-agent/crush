package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// recordingSurfacePublisher captures dashboard pushes synchronously so
// tests can assert on exactly what the tool published.
type recordingSurfacePublisher struct {
	events []pubsub.Event[SidekickSurface]
}

func (p *recordingSurfacePublisher) Publish(t pubsub.EventType, payload SidekickSurface) {
	p.events = append(p.events, pubsub.Event[SidekickSurface]{Type: t, Payload: payload})
}

func (p *recordingSurfacePublisher) PublishMustDeliver(_ context.Context, t pubsub.EventType, payload SidekickSurface) {
	p.Publish(t, payload)
}

// sidekickUpdatePayload is a minimal valid updateComponents payload.
const sidekickUpdatePayload = `{"version":"v0.9","updateComponents":{"surfaceId":"s1","components":[` +
	`{"component":"Card","id":"root","child":"t"},` +
	`{"component":"Text","id":"t","text":"Step 2/5"}` +
	`]}}`

func runSidekickUpdate(t *testing.T, pub pubsub.Publisher[SidekickSurface], surface string) fantasy.ToolResponse {
	t.Helper()
	tool := NewSidekickUpdateTool(pub)
	input, err := json.Marshal(SidekickUpdateParams{Surface: surface})
	require.NoError(t, err)
	resp, err := tool.Run(t.Context(), fantasy.ToolCall{
		ID:    "call1",
		Name:  SidekickUpdateToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	return resp
}

func TestSidekickUpdatePublishesWrappedSurface(t *testing.T) {
	t.Parallel()
	pub := &recordingSurfacePublisher{}

	resp := runSidekickUpdate(t, pub, sidekickUpdatePayload)
	require.False(t, resp.IsError)
	require.Equal(t, "rendered", resp.Content)

	require.Len(t, pub.events, 1)
	require.Equal(t, "<a2ui-json>"+sidekickUpdatePayload+"</a2ui-json>", pub.events[0].Payload.Content)
}

func TestSidekickUpdateAcceptsPreWrappedPayload(t *testing.T) {
	t.Parallel()
	pub := &recordingSurfacePublisher{}

	resp := runSidekickUpdate(t, pub, "<a2ui-json>"+sidekickUpdatePayload+"</a2ui-json>")
	require.False(t, resp.IsError)

	require.Len(t, pub.events, 1)
	require.Equal(t, "<a2ui-json>"+sidekickUpdatePayload+"</a2ui-json>", pub.events[0].Payload.Content,
		"an already-wrapped payload must not be double-wrapped")
}

func TestSidekickUpdateRejectsEmptySurface(t *testing.T) {
	t.Parallel()
	pub := &recordingSurfacePublisher{}

	resp := runSidekickUpdate(t, pub, "   ")
	require.True(t, resp.IsError)
	require.Empty(t, pub.events, "an invalid payload must not reach the dashboard")
}

func TestSidekickUpdateRejectsNonA2UIPayload(t *testing.T) {
	t.Parallel()
	pub := &recordingSurfacePublisher{}

	// Valid JSON, but not an A2UI message the parser recognizes: the tool
	// must fail loudly instead of pinning an empty dashboard.
	resp := runSidekickUpdate(t, pub, `{"foo":1}`)
	require.True(t, resp.IsError)
	require.Empty(t, pub.events)
}

func TestSidekickUpdateReplaceInPlaceEventStream(t *testing.T) {
	t.Parallel()
	pub := &recordingSurfacePublisher{}

	first := `{"version":"v0.9","updateComponents":{"surfaceId":"s1","components":[{"component":"Text","id":"t","text":"20%"}]}}`
	second := `{"version":"v0.9","updateComponents":{"surfaceId":"s1","components":[{"component":"Text","id":"t","text":"40%"}]}}`
	runSidekickUpdate(t, pub, first)
	runSidekickUpdate(t, pub, second)

	// The tool publishes every push; replacing is the subscriber's job
	// (the panel keeps only the latest surface).
	require.Len(t, pub.events, 2)
	require.Contains(t, pub.events[1].Payload.Content, "40%")
}
