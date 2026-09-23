package chat

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// textMessage builds a finished assistant message carrying only text,
// so IsFinished() is true and no thinking section complicates the
// content-cache assertions.
func textMessage(id, text string) *message.Message {
	return &message.Message{
		ID:   id,
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: text},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: testFinishTime},
		},
	}
}

// streamingTextMessage builds an unfinished assistant message carrying
// only text, so IsFinished() is false.
func streamingTextMessage(id, text string) *message.Message {
	return &message.Message{
		ID:    id,
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	}
}

// TestContentKey_PlanAndFinishBitsAreIndependent pins the cache-key
// contract the upstream sync's conflict resolution relies on: the
// fork's finish-state bit and upstream's plan-state bit must occupy
// separate positions so no combination of the two can alias another.
// The bits are orthogonal by construction (planStreaming() is
// planAgent && !IsFinished()), but a future edit that collapses them
// into one bit would silently serve a streaming render for a finished
// plan card.
func TestContentKey_PlanAndFinishBitsAreIndependent(t *testing.T) {
	sty := styles.CharmtonePantera()

	unfinished := NewAssistantMessageItem(&sty, streamingTextMessage("k1", "body")).(*AssistantMessageItem)
	unfinishedSrc, unfinishedExtra := unfinished.contentKey()
	require.Equal(t, uint64(0), unfinishedExtra,
		"an unfinished, non-plan message must set neither bit")

	finished := NewAssistantMessageItem(&sty, textMessage("k2", "body")).(*AssistantMessageItem)
	finishedSrc, finishedExtra := finished.contentKey()
	require.Equal(t, uint64(1), finishedExtra,
		"a finished message must set only the finish bit")

	planAgent := NewAssistantMessageItem(&sty, streamingTextMessage("k3", "body")).(*AssistantMessageItem)
	planAgent.SetPlanAgent(true)
	planSrc, planExtra := planAgent.contentKey()
	require.Equal(t, uint64(2), planExtra,
		"a streaming plan must set only the plan bit")

	// Identical text hashes to the same source hash in every case;
	// only the extra bits may differ. Distinct bits are what stop the
	// three renders aliasing each other.
	require.Equal(t, unfinishedSrc, finishedSrc)
	require.Equal(t, unfinishedSrc, planSrc)
	require.NotEqual(t, unfinishedExtra, finishedExtra)
	require.NotEqual(t, unfinishedExtra, planExtra)
	require.NotEqual(t, finishedExtra, planExtra)

	// The fourth combination (finished plan) sets only the finish bit:
	// planStreaming() is planAgent && !IsFinished(), so a finished plan
	// is not "streaming". The plan bit therefore distinguishes the
	// open-card render from the closed-card one, which is the state it
	// exists to key.
	finishedPlan := NewAssistantMessageItem(&sty, textMessage("k4", "body")).(*AssistantMessageItem)
	finishedPlan.SetPlanAgent(true)
	_, finishedPlanExtra := finishedPlan.contentKey()
	require.Equal(t, uint64(1), finishedPlanExtra,
		"a finished plan sets only the finish bit, since it is no longer streaming")
	require.NotEqual(t, planExtra, finishedPlanExtra,
		"the open and closed plan-card renders must not share a cache key")
}

// TestAssistantSectionCache_PlanCardAndA2UISurfaceDoNotCollide checks
// the specific hazard called out in the sync review: both a plan card
// and an A2UI surface render through the same contentSec slot, so a
// plan reply and an A2UI reply whose text hashed identically would
// serve each other's render from cache. The two renders are gated on
// disjoint text markers (HTML-comment plan sentinels vs. an
// <a2ui-json> block), so identical source text cannot be both.
func TestAssistantSectionCache_PlanCardAndA2UISurfaceDoNotCollide(t *testing.T) {
	sty := styles.CharmtonePantera()
	const width = 76

	planText := "Here is the plan.\n" + common.PlanReadyMarker + "\n"
	a2uiText := "Here is the surface.\n<a2ui-json>{\"version\":\"v0.9\",\"updateComponents\":{\"surfaceId\":\"s1\",\"components\":[{\"component\":\"Text\",\"id\":\"root\",\"text\":\"hi\"}]}}</a2ui-json>\n"

	// The gates are mutually exclusive on any given text.
	require.True(t, common.PlanReadyMarkerPresent(planText))
	require.False(t, contentHasA2UI(planText),
		"a plan card must not be gated as A2UI content")
	require.False(t, common.PlanReadyMarkerPresent(a2uiText))
	require.True(t, contentHasA2UI(a2uiText),
		"an A2UI reply must not be gated as a plan card")

	// Render both on one item in sequence and confirm each produces
	// its own render rather than the other's cached output. Glamour
	// wraps each word in its own escape sequence, so compare on a
	// single word and assert the other body's distinctive word is
	// absent.
	item := NewAssistantMessageItem(&sty, textMessage("c1", planText)).(*AssistantMessageItem)
	item.SetPlanAgent(true)

	planRender := item.RawRender(width)
	require.Contains(t, planRender, "plan",
		"the plan card must render the plan body")
	require.NotContains(t, planRender, "surface",
		"the plan card must not render the A2UI body")

	item.SetMessage(textMessage("c1", a2uiText))
	_ = item.RawRender(width)
	a2uiRender := item.RawRender(width)

	require.Contains(t, a2uiRender, "surface",
		"the A2UI reply must render its own body")
	require.NotContains(t, a2uiRender, "plan.",
		"the A2UI render must not be served the plan card from cache")
}

// TestAssistantSectionCache_PlanBranchesDropStaleSurfaces asserts the
// other half of the review's question: when a message that previously
// rendered an A2UI surface later renders as a plan card, the plan
// branch must drop the held surface models rather than leave a stale
// surface alive behind the card.
func TestAssistantSectionCache_PlanBranchesDropStaleSurfaces(t *testing.T) {
	sty := styles.CharmtonePantera()
	const width = 76

	a2uiText := "Surface.\n<a2ui-json>{\"version\":\"v0.9\",\"updateComponents\":{\"surfaceId\":\"s1\",\"components\":[{\"component\":\"Text\",\"id\":\"root\",\"text\":\"hi\"}]}}</a2ui-json>\n"
	item := NewAssistantMessageItem(&sty, textMessage("d1", a2uiText)).(*AssistantMessageItem)

	_ = item.RawRender(width)
	require.True(t, item.hasLiveA2UISurfaces(),
		"an A2UI reply should hold a live surface model")

	// The same message becomes a finished plan card.
	planText := a2uiText + common.PlanReadyMarker + "\n"
	item.SetMessage(textMessage("d1", planText))
	item.SetPlanAgent(true)
	_ = item.RawRender(width)

	require.False(t, item.hasLiveA2UISurfaces(),
		"the plan branch must drop surfaces held from the earlier render")
}
