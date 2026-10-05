package dispatch

import (
	"fmt"
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

func TestSummarizeDiffExtendedHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		diff    string
		want    []string
		notWant []string
	}{
		{
			name: "content lines with dash-dash and plus-plus prefixes",
			diff: "diff --git a/script.lua b/script.lua\n" +
				"index 111..222 100644\n" +
				"--- a/script.lua\n" +
				"+++ b/script.lua\n" +
				"@@ -1,3 +1,3 @@\n" +
				" local x = 1\n" +
				"--- old comment\n" +
				"+++ new comment\n" +
				" local y = 2\n",
			// The removed "-- old comment" and added "++ new comment"
			// lines count against the enclosing file and open no phantom
			// rows of their own.
			want:    []string{"script.lua | +1 -1"},
			notWant: []string{"old comment |", "new comment |"},
		},
		{
			name: "binary file change",
			diff: "diff --git a/logo.png b/logo.png\n" +
				"index 111..222 100644\n" +
				"Binary files a/logo.png and b/logo.png differ\n",
			want:    []string{"logo.png | binary"},
			notWant: []string{"no per-file stats"},
		},
		{
			name: "git binary patch",
			diff: "diff --git a/data.bin b/data.bin\n" +
				"index 111..222 100644\n" +
				"GIT binary patch\n" +
				"literal 0\n" +
				"zc21ZQ\n",
			want:    []string{"data.bin | binary"},
			notWant: []string{"no per-file stats"},
		},
		{
			name: "rename only",
			diff: "diff --git a/old_name.txt b/new_name.txt\n" +
				"similarity index 100%\n" +
				"rename from old_name.txt\n" +
				"rename to new_name.txt\n",
			want:    []string{"old_name.txt \u2192 new_name.txt | renamed"},
			notWant: []string{"+0 -0"},
		},
		{
			name: "rename with edits",
			diff: "diff --git a/old_name.txt b/new_name.txt\n" +
				"similarity index 50%\n" +
				"rename from old_name.txt\n" +
				"rename to new_name.txt\n" +
				"--- a/old_name.txt\n" +
				"+++ b/new_name.txt\n" +
				"@@ -1 +1 @@\n" +
				"-old line\n" +
				"+new line\n",
			// A rename with content changes renders the new path with its
			// counts, not the rename marker.
			want:    []string{"new_name.txt | +1 -1"},
			notWant: []string{"renamed", "old_name.txt \u2192"},
		},
		{
			name: "deletion",
			diff: "diff --git a/gone.txt b/gone.txt\n" +
				"deleted file mode 100644\n" +
				"index 111..000\n" +
				"--- a/gone.txt\n" +
				"+++ /dev/null\n" +
				"@@ -1,2 +0,0 @@\n" +
				"-gone\n" +
				"-content\n",
			want:    []string{"gone.txt | +0 -2"},
			notWant: []string{"/dev/null |"},
		},
		{
			name: "mixed multi-file diff",
			diff: "diff --git a/a.go b/a.go\n" +
				"index 111..222 100644\n" +
				"--- a/a.go\n" +
				"+++ b/a.go\n" +
				"@@ -1 +1,2 @@\n" +
				" func a() {\n" +
				"+\treturn 1\n" +
				" }\n" +
				"diff --git a/img.png b/img.png\n" +
				"index 333..444 100644\n" +
				"Binary files a/img.png and b/img.png differ\n" +
				"diff --git a/old.txt b/new.txt\n" +
				"similarity index 100%\n" +
				"rename from old.txt\n" +
				"rename to new.txt\n",
			want: []string{
				"a.go | +1 -0",
				"img.png | binary",
				"old.txt \u2192 new.txt | renamed",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			summary := SummarizeDiff(tt.diff)
			for _, w := range tt.want {
				require.Contains(t, summary, w)
			}
			for _, nw := range tt.notWant {
				require.NotContains(t, summary, nw)
			}
		})
	}
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

// One 100 KB minified line must not land whole in the parent's
// context: the byte budget cuts it and the summary stays within the
// budget plus its truncation markers (#361).
func TestSummarizeDiffByteBudgetCutsLongLine(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/min.txt b/min.txt\n--- a/min.txt\n+++ b/min.txt\n@@ -1 +1,2 @@\n")
	b.WriteString("+" + strings.Repeat("x", 100_000))
	b.WriteString("\n")

	summary := SummarizeDiff(b.String())

	require.Contains(t, summary, "min.txt | +1 -0")
	require.Contains(t, summary, lineCutMarker)
	// The summary may exceed the budget only by its markers.
	marker := fmt.Sprintf("... (%d more diff lines truncated; read the workspace files for full detail)\n", 1)
	require.LessOrEqual(t, len(summary), MaxDiffSummaryBytes+len(lineCutMarker)+len(marker))
}

// A thousand-file refactor must not drown the summary in stat lines:
// the table stops at MaxDiffStatFiles and names the remaining count
// (#361).
func TestSummarizeDiffFileCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 150; i++ {
		fmt.Fprintf(&b, "diff --git a/f%03d.txt b/f%03d.txt\n--- a/f%03d.txt\n+++ b/f%03d.txt\n@@ -1 +1,2 @@\n+added\n", i, i, i, i)
	}

	summary := SummarizeDiff(b.String())

	require.Equal(t, MaxDiffStatFiles, strings.Count(summary, " | +1 -0\n"))
	require.Contains(t, summary, "f099.txt | +1 -0\n")
	require.Contains(t, summary, "... 50 more files\n")
	require.NotContains(t, summary, "f100.txt |")
}
