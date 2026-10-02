package dispatch

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const twoFileDiff = `diff --git a/a.go b/a.go
index 111..222 100644
--- a/a.go
+++ b/a.go
@@ -1,3 +1,5 @@
 line
+added one
+added two
-gone
diff --git a/b.txt b/b.txt
new file mode 100644
index 000..333
--- /dev/null
+++ b/b.txt
@@ -0,0 +1,2 @@
+new file
+content
`

func TestSummarizeDiffPerFileStats(t *testing.T) {
	summary := SummarizeDiff(twoFileDiff)

	require.Contains(t, summary, "a.go | +2 -1\n")
	require.Contains(t, summary, "b.txt | +2 -0\n")
	// The full diff is carried after the stat block.
	require.Contains(t, summary, "+added one")
	require.Contains(t, summary, "+++ b/a.go")
}

func TestSummarizeDiffEmpty(t *testing.T) {
	require.Empty(t, SummarizeDiff(""))
}

func TestSummarizeDiffTruncates(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/big.txt b/big.txt\n--- a/big.txt\n+++ b/big.txt\n@@ -1 +1,999 @@\n")
	for i := 0; i < MaxDiffLines+50; i++ {
		b.WriteString("+line\n")
	}

	summary := SummarizeDiff(b.String())

	require.Contains(t, summary, "big.txt | +300 -0")
	require.Contains(t, summary, "more diff lines truncated")
	// The truncation marker names how much was cut.
	require.Contains(t, summary, "(54 more diff lines truncated")
}

func TestSummarizeDiffDeletionNamesDeletedFile(t *testing.T) {
	deletion := `diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 111..000
--- a/gone.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-gone
-content
`

	summary := SummarizeDiff(deletion)

	// A deletion's new side is /dev/null; the stat must carry the
	// deleted file's own path, not the placeholder.
	require.Contains(t, summary, "gone.txt | +0 -2")
	require.NotContains(t, summary, "/dev/null |")
}

func TestSummarizeDiffWithoutFileHeaders(t *testing.T) {
	summary := SummarizeDiff("Binary files a/x and b/x differ\n")

	require.Contains(t, summary, "no per-file stats")
	require.Contains(t, summary, "Binary files a/x and b/x differ")
}

func TestDispatchResultTerminalMessage(t *testing.T) {
	r := DispatchResult{
		DispatchID:    "d-1",
		Branch:        "crush-dispatch-d-1",
		WorkspacePath: "/tmp/ws",
		SessionID:     "s-1",
		Status:        StatusCompleted,
		KeyFindings:   "fixed the bug",
		DiffSummary:   "a.go | +2 -1",
	}

	msg := r.TerminalMessage()
	require.Contains(t, msg, `finished with status "completed"`)
	require.Contains(t, msg, "d-1")
	// The instruction is the no-automated-merge contract: the main agent
	// reviews and decides.
	require.Contains(t, msg, "Review the diff and decide whether to merge or dismiss")
	require.Contains(t, msg, "never merges itself")
	require.Contains(t, msg, `"key_findings": "fixed the bug"`)
	require.Contains(t, msg, `"diff_summary": "a.go | +2 -1"`)
}

// A failed result carries the error and omits the completion-only
// fields from the JSON.
func TestDispatchResultFailedShape(t *testing.T) {
	r := DispatchResult{
		DispatchID: "d-2",
		Status:     StatusFailed,
		Error:      "boom",
	}
	rendered := r.Render()
	require.Contains(t, rendered, `"status": "failed"`)
	require.Contains(t, rendered, `"error": "boom"`)
	require.NotContains(t, rendered, "key_findings")
	require.NotContains(t, rendered, "diff_summary")
}
