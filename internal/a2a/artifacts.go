// The dispatched run's work product crosses the wire as two named
// artifacts (#361): "diff" carries the unified diff itself, chunked into
// SSE-safe pieces, and "dispatch-result" carries the typed outcome — byte
// count, capture error, changed files — so the work product's shape is
// known even when the diff is absent or unreadable.
package a2a

import (
	"encoding/gob"
	"encoding/json"
	"strings"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Artifact identity and transport budgets. diffChunkSize caps each diff
// artifact chunk far below the SDK's 10 MB SSE data-line cap, so even an
// 11 MB diff streams without tripping bufio.Scanner: token too long;
// maxReassembledDiffBytes bounds what the client will reassemble.
const (
	DiffArtifactID a2aspec.ArtifactID = "diff"

	ResultArtifactID a2aspec.ArtifactID = "dispatch-result"

	DiffArtifactName = "diff"
	DiffFilename     = "dispatch.diff"
	DiffMediaType    = "text/x-diff"

	ResultArtifactName = "dispatch-result"

	diffChunkSize           = 256 * 1024
	maxReassembledDiffBytes = 32 << 20
)

// DispatchOutcome is the typed payload the dispatch-result artifact
// carries: the work product's shape when the diff itself is absent or
// unreadable, so a capture error crosses the wire instead of surfacing
// as "(no changes)".
type DispatchOutcome struct {
	// DiffBytes is the diff's size in bytes; 0 when capture failed or
	// the run changed nothing.
	DiffBytes int `json:"diff_bytes"`
	// DiffError carries the diff-capture error's message, empty on
	// success. A run with a diff error still completes.
	DiffError string `json:"diff_error,omitempty"`
	// FilesChanged counts the per-file sections of the diff.
	FilesChanged int `json:"files_changed"`
}

// The SDK's task store persists artifact parts through encoding/gob, and a
// DataPart holding a concrete struct needs that struct registered.
func init() {
	gob.Register(DispatchOutcome{})
}

// chunkDiff splits a diff into chunks of at most diffChunkSize bytes,
// breaking at line boundaries where possible and hard-splitting lines
// longer than the cap. Concatenating the chunks reproduces the input
// byte for byte.
func chunkDiff(diff string) []string {
	if diff == "" {
		return nil
	}
	var chunks []string
	for len(diff) > diffChunkSize {
		cut := diffChunkSize
		if idx := strings.LastIndexByte(diff[:diffChunkSize], '\n'); idx > 0 {
			cut = idx + 1
		}
		chunks = append(chunks, diff[:cut])
		diff = diff[cut:]
	}
	return append(chunks, diff)
}

// countDiffFiles counts the per-file sections of a unified diff.
func countDiffFiles(diff string) int {
	n := strings.Count(diff, "\ndiff --git ")
	if strings.HasPrefix(diff, "diff --git ") {
		n++
	}
	return n
}

// decodeDispatchOutcome extracts the typed outcome from a DataPart of
// the dispatch-result artifact. In-process the part still holds the
// typed struct; over the wire the value is generic JSON, so it round
// trips through encoding/json before it fits the struct.
func decodeDispatchOutcome(part *a2aspec.Part) (DispatchOutcome, bool) {
	switch v := part.Data().(type) {
	case DispatchOutcome:
		return v, true
	case map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return DispatchOutcome{}, false
		}
		var out DispatchOutcome
		if err := json.Unmarshal(b, &out); err != nil {
			return DispatchOutcome{}, false
		}
		return out, true
	default:
		return DispatchOutcome{}, false
	}
}

// diffPart wraps one diff chunk as the artifact's text part.
func diffPart(chunk string) *a2aspec.Part {
	p := a2aspec.NewTextPart(chunk)
	p.MediaType = DiffMediaType
	p.Filename = DiffFilename
	return p
}

// resultArtifact builds the dispatch-result artifact update for one
// outcome. A fresh artifact, not an append, and the only chunk.
func resultArtifact(execCtx *a2asrv.ExecutorContext, outcome DispatchOutcome) *a2aspec.TaskArtifactUpdateEvent {
	ev := a2aspec.NewArtifactUpdateEvent(execCtx, ResultArtifactID, a2aspec.NewDataPart(outcome))
	ev.Append = false
	ev.LastChunk = true
	ev.Artifact.Name = ResultArtifactName
	return ev
}

// diffArtifact builds one chunk's diff artifact update: the first chunk
// replaces, later chunks append, and the last one closes the artifact.
func diffArtifact(execCtx *a2asrv.ExecutorContext, chunks []string, i int) *a2aspec.TaskArtifactUpdateEvent {
	ev := a2aspec.NewArtifactUpdateEvent(execCtx, DiffArtifactID, diffPart(chunks[i]))
	ev.Append = i > 0
	ev.LastChunk = i == len(chunks)-1
	ev.Artifact.Name = DiffArtifactName
	return ev
}
