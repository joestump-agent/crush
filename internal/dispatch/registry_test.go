package dispatch

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// registeredEntry registers a minimal provisioned-shape entry under id,
// standing in for the provision-and-register composition the dispatch
// tool performs. Registry tests need no real workspace behind it.
func registeredEntry(reg *AgentRegistry, id string) Entry {
	entry := Entry{ID: id, Status: StatusProvisioned}
	reg.Register(entry)
	return entry
}

// A registry round trip pins the split's core claim (#391): the
// registry is pure in-memory state — it needs no git repository or
// filesystem behind it, and Register/Get/Update/List/Remove compose.
func TestAgentRegistryRoundTrip(t *testing.T) {
	reg := NewAgentRegistry()
	require.Empty(t, reg.List())

	entry := Entry{ID: "d-1", Status: StatusProvisioned, Path: "/unused"}
	reg.Register(entry)

	got, ok := reg.Get("d-1")
	require.True(t, ok)
	require.Equal(t, entry, got)

	require.True(t, reg.Update("d-1", func(e *Entry) { e.Status = StatusRunning }))
	got, ok = reg.Get("d-1")
	require.True(t, ok)
	require.Equal(t, StatusRunning, got.Status)

	require.Len(t, reg.List(), 1)
	require.True(t, reg.Remove("d-1"))
	require.False(t, reg.Remove("d-1"), "removing an absent entry reports false")
	require.Empty(t, reg.List())
}

// Registry mutations on unknown entries report false rather than
// panicking or silently succeeding.
func TestRegistryUpdateUnknownEntry(t *testing.T) {
	reg := NewAgentRegistry()

	require.False(t, reg.SetStatus("nope", StatusRunning))
	require.False(t, reg.SetSession("nope", "s"))
	require.False(t, reg.SetHandle("nope", "h"))
	require.False(t, reg.SetEndpoint("nope", "e", nil))
	require.False(t, reg.Update("nope", func(e *Entry) {}))
	require.False(t, reg.Update("", nil))

	_, ok := reg.ByHandle("")
	require.False(t, ok)
	_, ok = reg.BySession("")
	require.False(t, ok)
}

// HandleSlug normalizes candidates into handle form and leaves
// unhandle-able ones empty so the caller can fall through.
func TestHandleSlug(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"tester", "tester"},
		{"@Tester", "tester"},
		{"  @Team Lead  ", "team-lead"},
		{"Docs_Writer", "docs-writer"},
		{"---weird__name---", "weird-name"},
		{"🤖 robot", "robot"},
		{"@__", ""},
		{"", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, HandleSlug(tc.in), "HandleSlug(%q)", tc.in)
	}
}

// AssignHandle assigns the requested handle, derives one from the role,
// falls back to "agent", and suffixes collisions numerically against the
// running agents only (#399): a finished dispatch releases its handle for
// reuse. Reserved names are suffixed like collisions, and the 32-byte
// cap holds even with a suffix.
func TestAssignHandle(t *testing.T) {
	reg := NewAgentRegistry()

	a := registeredEntry(reg, "a")
	b := registeredEntry(reg, "b")
	c := registeredEntry(reg, "c")

	// Explicit handle, with the leading @ tolerated.
	handle, ok := reg.AssignHandle(a.ID, "@Tester", "writes tests")
	require.True(t, ok)
	require.Equal(t, "tester", handle)

	// Derived from the role when no handle was requested.
	handle, ok = reg.AssignHandle(b.ID, "", "Docs Writer")
	require.True(t, ok)
	require.Equal(t, "docs-writer", handle)

	// Default when neither handle nor role was given.
	handle, ok = reg.AssignHandle(c.ID, "", "")
	require.True(t, ok)
	require.Equal(t, "agent", handle)

	// The entry carries handle and role, and ByHandle resolves it.
	entry, ok := reg.Get(a.ID)
	require.True(t, ok)
	require.Equal(t, "tester", entry.Handle)
	require.Equal(t, "writes tests", entry.Role)
	got, ok := reg.ByHandle("tester")
	require.True(t, ok)
	require.Equal(t, a.ID, got.ID)

	// A fourth dispatch asking for an in-use handle gets a suffix.
	d := registeredEntry(reg, "d")
	handle, ok = reg.AssignHandle(d.ID, "tester", "")
	require.True(t, ok)
	require.Equal(t, "tester-2", handle)

	// A finished dispatch releases its handle: the next dispatch asking
	// for it gets the bare handle again, even though the finished entry
	// still carries it (#399).
	reg.SetStatus(a.ID, StatusCompleted)
	e := registeredEntry(reg, "e")
	handle, ok = reg.AssignHandle(e.ID, "tester", "")
	require.True(t, ok)
	require.Equal(t, "tester", handle)

	// A reserved name is never assigned outright: it is suffixed like a
	// collision (#399).
	f := registeredEntry(reg, "f")
	handle, ok = reg.AssignHandle(f.ID, "task", "")
	require.True(t, ok)
	require.Equal(t, "task-2", handle)

	// A 60-character role yields a handle of at most 32 bytes (#399).
	g := registeredEntry(reg, "g")
	long := strings.Repeat("x", 60)
	handle, ok = reg.AssignHandle(g.ID, long, "")
	require.True(t, ok)
	require.Equal(t, strings.Repeat("x", MaxHandleLength), handle)

	// A collision on an already-capped handle suffixes with the base
	// truncated so "-2" fits: the result stays within the cap.
	h := registeredEntry(reg, "h")
	handle, ok = reg.AssignHandle(h.ID, long, "")
	require.True(t, ok)
	require.Equal(t, strings.Repeat("x", MaxHandleLength-2)+"-2", handle)
	require.LessOrEqual(t, len(handle), MaxHandleLength)

	// A removed entry releases its handle for reuse.
	require.True(t, reg.Remove(b.ID))
	handle, ok = reg.AssignHandle(e.ID, "", "docs writer")
	require.True(t, ok)
	require.Equal(t, "docs-writer", handle)

	// Unknown entry: not assigned.
	_, ok = reg.AssignHandle("no-such-id", "x", "")
	require.False(t, ok)
}

// IsTerminal marks exactly the terminal statuses.
func TestStatusIsTerminal(t *testing.T) {
	t.Parallel()

	for _, s := range []Status{StatusCompleted, StatusFailed, StatusKilled} {
		require.True(t, s.IsTerminal(), "%s must be terminal", s)
	}
	for _, s := range []Status{StatusProvisioned, StatusRunning} {
		require.False(t, s.IsTerminal(), "%s must not be terminal", s)
	}
}

// HandleSlug caps a slug at MaxHandleLength bytes (#399), trimming a
// trailing dash the cap leaves.
func TestHandleSlugLengthCap(t *testing.T) {
	t.Parallel()

	capped := HandleSlug(strings.Repeat("a", 60))
	require.Len(t, capped, MaxHandleLength)
	require.Equal(t, strings.Repeat("a", MaxHandleLength), capped)

	// The cap lands after a dash run: the trailing dash is trimmed.
	dashed := HandleSlug(strings.Repeat("a", 30) + "-" + strings.Repeat("b", 30))
	require.LessOrEqual(t, len(dashed), MaxHandleLength)
	require.False(t, strings.HasSuffix(dashed, "-"), "a capped slug never ends with a dash")
	require.Equal(t, strings.Repeat("a", 30)+"-b", dashed)
}

// ByHandle prefers the live entry carrying the handle; when only
// finished entries carry it, the most recently finished one answers
// (#399), so a mention of a finished @handle still renders its card
// until the handle is reused.
func TestByHandlePrefersLiveEntry(t *testing.T) {
	reg := NewAgentRegistry()

	a := registeredEntry(reg, "a")
	_, ok := reg.AssignHandle(a.ID, "tester", "")
	require.True(t, ok)

	// Finish a; its handle still resolves, as the finished fallback.
	require.True(t, reg.SetStatus(a.ID, StatusCompleted))
	require.True(t, reg.Update(a.ID, func(e *Entry) { e.FinishedAt = time.Now().Add(-time.Minute) }))
	got, ok := reg.ByHandle("tester")
	require.True(t, ok, "a finished handle still resolves")
	require.Equal(t, a.ID, got.ID)

	// A newer finished entry wins the fallback over an older one.
	b := registeredEntry(reg, "b")
	_, ok = reg.AssignHandle(b.ID, "tester", "")
	require.True(t, ok)
	require.True(t, reg.SetStatus(b.ID, StatusKilled))
	require.True(t, reg.Update(b.ID, func(e *Entry) { e.FinishedAt = time.Now() }))
	got, ok = reg.ByHandle("tester")
	require.True(t, ok)
	require.Equal(t, b.ID, got.ID, "the most recently finished entry answers")

	// A new dispatch reuses the released handle; the live entry now wins.
	c := registeredEntry(reg, "c")
	handle, ok := reg.AssignHandle(c.ID, "tester", "")
	require.True(t, ok)
	require.Equal(t, "tester", handle, "the handle was free again")
	got, ok = reg.ByHandle("tester")
	require.True(t, ok)
	require.Equal(t, c.ID, got.ID, "the live entry answers over finished ones")
}
