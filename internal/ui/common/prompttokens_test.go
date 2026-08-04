package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// scan is a readability helper: it returns each token's literal text
// alongside its kind, which is what the assertions below actually care
// about. The known-skill set is process-wide, so these tests are serial.
func scan(t *testing.T, line string, skills ...string) []string {
	t.Helper()
	SetPromptSkillNames(skills)
	t.Cleanup(func() { SetPromptSkillNames(nil) })

	runes := []rune(line)
	var out []string
	for _, tok := range ScanPromptTokens(runes) {
		kind := "file"
		if tok.Kind == PromptTokenSkill {
			kind = "skill"
		}
		out = append(out, kind+":"+string(runes[tok.Start:tok.End]))
	}
	return out
}

func TestScanPromptTokensFileMentions(t *testing.T) {
	require.Equal(t,
		[]string{"file:@foo.go", "file:@bar.md"},
		scan(t, "see @foo.go and @bar.md end"),
	)
}

func TestScanPromptTokensSkillsMustBeKnown(t *testing.T) {
	require.Equal(t, []string{"skill:/code-review"},
		scan(t, "please /code-review this", "code-review", "commit"))
	require.Empty(t, scan(t, "please /bogus this", "code-review"))
}

func TestScanPromptTokensSkillPrefixMatches(t *testing.T) {
	// Prefix matching is what gives live feedback mid-word, and using the
	// same rule after submit keeps the highlight from flickering.
	require.Len(t, scan(t, "/cod", "code-review"), 1)
	require.Empty(t, scan(t, "/codeX", "code-review"))
}

func TestScanPromptTokensRequireWordBoundary(t *testing.T) {
	// An email address, a relative path, and a URL are all prose here.
	require.Empty(t, scan(t, "mail me@foo.go or a/b or x/commit", "commit"))
	// Tab counts as a boundary.
	require.Len(t, scan(t, "x\t@foo.go"), 1)
	// So does start-of-line.
	require.Len(t, scan(t, "@foo.go trailing"), 1)
}

func TestScanPromptTokensBareTriggersIgnored(t *testing.T) {
	require.Empty(t, scan(t, "@ / @", "commit"))
	require.Empty(t, scan(t, "@"))
	require.Empty(t, scan(t, ""))
}

func TestScanPromptTokensAbsolutePathIsNotASkill(t *testing.T) {
	// "/usr/bin" starts a word but is not a skill, so it stays prose even
	// when a skill named "usr-something" exists.
	require.Empty(t, scan(t, "look in /usr/bin", "usr-tools"))
}

func TestIsPromptSkillPrefixEmptyName(t *testing.T) {
	SetPromptSkillNames([]string{"commit"})
	t.Cleanup(func() { SetPromptSkillNames(nil) })

	// A bare "/" has no name to match; it must not match everything.
	require.False(t, IsPromptSkillPrefix(""))
	require.True(t, IsPromptSkillPrefix("com"))
}

// TestScanPromptTokensPromptWithArguments pins that an MCP prompt token
// highlights with its arguments attached.
//
// The token carries its arguments inline — "/gitea:review(id=42)" — and
// IsPromptSkillPrefix asks whether the token is a prefix of a known name. A
// name with arguments appended is longer than the name, so without stripping
// the parenthesised tail the argument-bearing form never validates and the
// whole token renders as unstyled prose.
func TestScanPromptTokensPromptWithArguments(t *testing.T) {
	SetPromptSkillNames([]string{"gitea:review", "code-review"})
	t.Cleanup(func() { SetPromptSkillNames(nil) })

	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"bare prompt name", "do /gitea:review", true},
		{"with one argument", "do /gitea:review(id=42)", true},
		{"argument value containing a slash", "do /gitea:review(repo=stump.wtf/crush)", true},
		{"several arguments", "do /gitea:review(id=42,repo=a/b)", true},
		{"partially typed", "do /gitea:rev", true},
		{"unknown prompt is left alone", "do /nope:thing(id=1)", false},
		{"an ordinary path is left alone", "read /tmp/out.log", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toks := ScanPromptTokens([]rune(tc.line))
			var gotSkill bool
			for _, tok := range toks {
				if tok.Kind == PromptTokenSkill {
					gotSkill = true
					// The whole token, arguments included, is one span.
					require.Equal(t, len([]rune(tc.line)), tok.End,
						"the token must run to the end of the word")
				}
			}
			require.Equal(t, tc.want, gotSkill)
		})
	}
}
