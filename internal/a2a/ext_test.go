package a2a

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/stretchr/testify/require"
)

// The static registry: the todos/v1 extension is registered with a typed
// payload and a schema, lookups miss for unknown URIs, and re-registering
// the same URI is rejected.
func TestExtensionRegistry(t *testing.T) {
	t.Parallel()

	registered := Registered()
	require.NotEmpty(t, registered)
	for i := 1; i < len(registered); i++ {
		require.Less(t, registered[i-1].URI, registered[i].URI, "extensions are sorted by URI")
	}

	ext, ok := Lookup(TodoExtensionURI)
	require.True(t, ok)
	require.Equal(t, "Structured dispatch todo progress in TaskStatusUpdateEvent metadata.", ext.Description)
	require.NotNil(t, ext.Type)
	require.NotNil(t, ext.Schema, "the todos/v1 extension ships its JSON Schema")

	_, ok = Lookup("https://crush.charm.land/ext/does-not-exist/v9")
	require.False(t, ok)

	require.Error(t, Register(Extension{URI: TodoExtensionURI, Type: ext.Type}), "duplicate URIs are rejected")
	require.Error(t, Register(Extension{URI: "https://example.com/not-crush/v1", Type: ext.Type}), "foreign URIs are rejected")
	require.Error(t, Register(Extension{Type: ext.Type}), "empty URIs are rejected")
	require.Error(t, Register(Extension{URI: "https://crush.charm.land/ext/typed/v1"}), "missing types are rejected")
}

// Encode produces the JSON-shaped values A2A metadata and the SDK's task
// store require (maps and slices, not typed Go values), and Decode round
// trips them back to the extension's typed payload — both by value and by
// pointer.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()

	progress := agent.TodoProgress{
		Current:   "writing the fix",
		Completed: 1,
		Total:     2,
		Todos: []agent.TodoItem{
			{Content: "read the code", Status: "completed"},
			{Content: "write the fix", Status: "in_progress", ActiveForm: "writing the fix"},
		},
	}
	encoded, err := Encode(TodoExt, progress)
	require.NoError(t, err)
	// JSON-shaped: a map, with a slice of maps under todos.
	meta := map[string]any{TodoExt.URI: encoded}

	byValue, err := Decode[agent.TodoProgress](meta, TodoExt)
	require.NoError(t, err)
	require.Equal(t, progress, byValue)

	byPointer, err := Decode[*agent.TodoProgress](meta, TodoExt)
	require.NoError(t, err)
	require.Equal(t, &progress, byPointer)
}

// Decode errors on a missing key and on a value that does not fit the
// extension's type; a mismatched T is an error, not a zero value.
func TestDecodeErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing key", func(t *testing.T) {
		t.Parallel()
		_, err := Decode[agent.TodoProgress](map[string]any{}, TodoExt)
		require.Error(t, err)
		require.Contains(t, err.Error(), TodoExtensionURI)
	})

	t.Run("malformed value", func(t *testing.T) {
		t.Parallel()
		_, err := Decode[agent.TodoProgress](map[string]any{TodoExt.URI: "not a todo progress"}, TodoExt)
		require.Error(t, err)
	})

	t.Run("mismatched type", func(t *testing.T) {
		t.Parallel()
		encoded, err := Encode(TodoExt, agent.TodoProgress{Current: "x"})
		require.NoError(t, err)
		_, err = Decode[agent.TodoItem](map[string]any{TodoExt.URI: encoded}, TodoExt)
		require.Error(t, err)
	})
}

// DecodeValue decodes raw metadata into the extension's declared type
// without compile-time knowledge of it, tolerating the float64 numbers a
// JSON wire round trip produces.
func TestDecodeValueDynamic(t *testing.T) {
	t.Parallel()

	decoded, err := DecodeValue(TodoExt, map[string]any{
		"current":   "reading the code",
		"completed": float64(1),
		"total":     float64(3),
		"todos":     []any{map[string]any{"content": "read the code", "status": "in_progress", "activeForm": "reading the code"}},
	})
	require.NoError(t, err)
	progress, ok := decoded.(*agent.TodoProgress)
	require.True(t, ok)
	require.Equal(t, &agent.TodoProgress{
		Current:   "reading the code",
		Completed: 1,
		Total:     3,
		Todos:     []agent.TodoItem{{Content: "read the code", Status: "in_progress", ActiveForm: "reading the code"}},
	}, progress)
}

// The question and answer payloads of an input-required round trip
// (#352) are declared extensions with typed payloads and schemas, and
// both survive the JSON-shaped encoding a DataPart carries.
func TestQuestionExtensionsRoundTrip(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{QuestionExtensionURI, AnswerExtensionURI} {
		ext, ok := Lookup(uri)
		require.True(t, ok, "%s is registered", uri)
		require.NotNil(t, ext.Type)
		require.NotNil(t, ext.Schema, "%s ships its JSON Schema", uri)
	}

	yes := true
	req := agent.QuestionRequest{
		ID: "batch-1",
		Questions: []question.Question{{
			ID:          "q-1",
			Type:        question.TypeSingleChoice,
			Text:        "Which database?",
			Description: "The schema differs per engine.",
			Choices:     []question.Choice{{ID: "pg", Label: "Postgres"}, {ID: "lite", Label: "SQLite"}},
		}},
	}
	encoded, err := Encode(QuestionExt, req)
	require.NoError(t, err)
	decodedReq, err := DecodeValue(QuestionExt, encoded)
	require.NoError(t, err)
	require.Equal(t, &req, decodedReq)

	answer := agent.QuestionAnswer{Answers: []question.Answer{{QuestionID: "q-1", SelectedIDs: []string{"pg"}, Yes: &yes}}}
	encoded, err = Encode(AnswerExt, answer)
	require.NoError(t, err)
	decodedAnswer, err := DecodeValue(AnswerExt, encoded)
	require.NoError(t, err)
	require.Equal(t, &answer, decodedAnswer)
}
