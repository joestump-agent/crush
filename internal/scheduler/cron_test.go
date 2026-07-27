package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseRejectsWrongFieldCount(t *testing.T) {
	t.Parallel()

	for _, expr := range []string{"", "* * * *", "* * * * * *", "0 0"} {
		_, err := Parse(expr)
		require.Error(t, err, expr)
	}
}

func TestParseRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	for _, expr := range []string{
		"60 * * * *",   // minute out of range
		"* 24 * * *",   // hour out of range
		"* * 0 * *",    // day-of-month out of range
		"* * * 13 *",   // month out of range
		"* * * * 8",    // day-of-week out of range
		"a * * * *",    // names not supported
		"* * * JAN *",  // month names not supported
		"*/0 * * * *",  // zero step
		"5-2 * * * *",  // reversed range
		"* * ? * *",    // extended syntax
		"* * L * *",    // extended syntax
		", * * * *",    // empty list entry
		"1,,2 * * * *", // empty list entry
	} {
		_, err := Parse(expr)
		require.Error(t, err, expr)
	}
}

func TestParseAcceptsValidExpressions(t *testing.T) {
	t.Parallel()

	for _, expr := range []string{
		"* * * * *",
		"*/15 * * * *",
		"0 9 * * 1-5",
		"30 14 27 7 *",
		"0,30 8-18/2 1,15 * 0",
		"5/10 * * * *",
		"0 0 * * 7", // Sunday as 7
	} {
		_, err := Parse(expr)
		require.NoError(t, err, expr)
	}
}

func TestNextEveryMinute(t *testing.T) {
	t.Parallel()

	sched, err := Parse("* * * * *")
	require.NoError(t, err)

	from := time.Date(2026, 7, 27, 10, 30, 25, 0, time.Local)
	next := sched.Next(from)
	require.Equal(t, time.Date(2026, 7, 27, 10, 31, 0, 0, time.Local), next)
}

func TestNextSpecificTime(t *testing.T) {
	t.Parallel()

	sched, err := Parse("30 14 * * *")
	require.NoError(t, err)

	from := time.Date(2026, 7, 27, 10, 30, 0, 0, time.Local)
	next := sched.Next(from)
	require.Equal(t, time.Date(2026, 7, 27, 14, 30, 0, 0, time.Local), next)

	// After 14:30 the same day, the next fire is the following day.
	from = time.Date(2026, 7, 27, 14, 30, 0, 0, time.Local)
	next = sched.Next(from)
	require.Equal(t, time.Date(2026, 7, 28, 14, 30, 0, 0, time.Local), next)
}

func TestNextStepsAndRanges(t *testing.T) {
	t.Parallel()

	sched, err := Parse("*/15 9-17 * * *")
	require.NoError(t, err)

	from := time.Date(2026, 7, 27, 9, 7, 0, 0, time.Local)
	require.Equal(t, time.Date(2026, 7, 27, 9, 15, 0, 0, time.Local), sched.Next(from))

	// After 17:45 (last fire of the day) it rolls to 09:00 next day.
	from = time.Date(2026, 7, 27, 17, 45, 0, 0, time.Local)
	require.Equal(t, time.Date(2026, 7, 28, 9, 0, 0, 0, time.Local), sched.Next(from))
}

func TestNextMonthBoundary(t *testing.T) {
	t.Parallel()

	sched, err := Parse("0 9 1 3 *")
	require.NoError(t, err)

	from := time.Date(2026, 7, 27, 10, 0, 0, 0, time.Local)
	require.Equal(t, time.Date(2027, 3, 1, 9, 0, 0, 0, time.Local), sched.Next(from))
}

func TestNextDayOfWeek(t *testing.T) {
	t.Parallel()

	// 2026-07-27 is a Monday. Fire at 09:00 on Fridays only.
	sched, err := Parse("0 9 * * 5")
	require.NoError(t, err)

	from := time.Date(2026, 7, 27, 10, 0, 0, 0, time.Local)
	next := sched.Next(from)
	require.Equal(t, time.Date(2026, 7, 31, 9, 0, 0, 0, time.Local), next)
	require.Equal(t, time.Friday, next.Weekday())
}

func TestNextDayOfWeekSundayAliases(t *testing.T) {
	t.Parallel()

	zero, err := Parse("0 9 * * 0")
	require.NoError(t, err)
	seven, err := Parse("0 9 * * 7")
	require.NoError(t, err)

	from := time.Date(2026, 7, 27, 10, 0, 0, 0, time.Local) // Monday
	nextZero := zero.Next(from)
	nextSeven := seven.Next(from)
	require.Equal(t, nextZero, nextSeven)
	require.Equal(t, time.Sunday, nextZero.Weekday())
}

func TestDayOfMonthAndDayOfWeekOrSemantics(t *testing.T) {
	t.Parallel()

	// Both restricted: fires when EITHER matches. The 15th OR any Monday.
	sched, err := Parse("0 9 15 * 1")
	require.NoError(t, err)

	// From Tuesday 2026-07-14: next Monday is the 20th, but the 15th
	// comes first and matches on day-of-month alone.
	from := time.Date(2026, 7, 14, 10, 0, 0, 0, time.Local)
	next := sched.Next(from)
	require.Equal(t, time.Date(2026, 7, 15, 9, 0, 0, 0, time.Local), next)

	// From Thursday the 16th: the 15th has passed; next hit is Monday
	// the 20th on day-of-week alone.
	from = time.Date(2026, 7, 16, 10, 0, 0, 0, time.Local)
	next = sched.Next(from)
	require.Equal(t, time.Date(2026, 7, 20, 9, 0, 0, 0, time.Local), next)
}

func TestNextWildcardDayOfWeekOnly(t *testing.T) {
	t.Parallel()

	// Day-of-month restricted, day-of-week wildcard: only day-of-month
	// constrains.
	sched, err := Parse("0 9 15 * *")
	require.NoError(t, err)

	from := time.Date(2026, 7, 16, 10, 0, 0, 0, time.Local)
	require.Equal(t, time.Date(2026, 8, 15, 9, 0, 0, 0, time.Local), sched.Next(from))
}

func TestNextIsStrictlyAfterFrom(t *testing.T) {
	t.Parallel()

	sched, err := Parse("30 14 * * *")
	require.NoError(t, err)

	// Exactly on a fire time: the next fire is tomorrow, not now.
	from := time.Date(2026, 7, 27, 14, 30, 0, 0, time.Local)
	next := sched.Next(from)
	require.True(t, next.After(from))
	require.Equal(t, time.Date(2026, 7, 28, 14, 30, 0, 0, time.Local), next)
}

func TestNextDSTGap(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	// 2026-03-08 02:30 does not exist in America/New_York (spring
	// forward). time.Date normalizes the gap; the important part is
	// that Next terminates and returns a future time.
	sched, err := Parse("30 2 * * *")
	require.NoError(t, err)

	from := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	next := sched.Next(from)
	require.True(t, next.After(from))
}
