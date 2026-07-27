package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed 5-field cron expression evaluated in the local
// timezone. Supported syntax: wildcards (*), single values (5), steps
// (*/15), ranges (1-5), ranges with steps (1-10/2), and comma lists of
// any of those. Extended syntax (L, W, ?, MON, JAN) is not supported.
// Day-of-week accepts 0-7, with both 0 and 7 meaning Sunday. When both
// day-of-month and day-of-week are restricted, a time matches when
// either matches (standard cron OR semantics).
type Schedule struct {
	minutes    uint64 // bits 0-59
	hours      uint32 // bits 0-23
	daysOfM    uint32 // bits 1-31
	months     uint16 // bits 1-12
	daysOfW    uint8  // bits 0-6 (0 = Sunday)
	domAny     bool
	dowAny     bool
	expression string
}

// Parse parses a strict 5-field cron expression.
func Parse(expression string) (*Schedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return nil, fmt.Errorf("invalid cron expression %q: expected 5 fields (minute hour day-of-month month day-of-week), got %d", expression, len(fields))
	}

	minutes, err := parseField(fields[0], 0, 59, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: minute field: %w", expression, err)
	}
	hours, err := parseField(fields[1], 0, 23, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: hour field: %w", expression, err)
	}
	daysOfM, err := parseField(fields[2], 1, 31, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: day-of-month field: %w", expression, err)
	}
	months, err := parseField(fields[3], 1, 12, 16)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: month field: %w", expression, err)
	}
	daysOfWRaw, err := parseField(fields[4], 0, 7, 8)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: day-of-week field: %w", expression, err)
	}

	// Normalize day-of-week: 7 also means Sunday.
	daysOfW := uint8(daysOfWRaw & 0x7f)
	if daysOfWRaw&(1<<7) != 0 {
		daysOfW |= 1
	}

	return &Schedule{
		minutes:    minutes,
		hours:      uint32(hours),
		daysOfM:    uint32(daysOfM),
		months:     uint16(months),
		daysOfW:    daysOfW,
		domAny:     fields[2] == "*",
		dowAny:     fields[4] == "*",
		expression: expression,
	}, nil
}

// parseField parses one cron field into a bit set of matching values.
func parseField(field string, minValue, maxValue, bits uint) (uint64, error) {
	if field == "" {
		return 0, fmt.Errorf("empty field")
	}

	var set uint64
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return 0, fmt.Errorf("empty list entry in %q", field)
		}

		step := uint(1)
		base := part
		if idx := strings.Index(part, "/"); idx >= 0 {
			base = part[:idx]
			parsedStep, err := strconv.ParseUint(part[idx+1:], 10, 8)
			if err != nil || parsedStep == 0 {
				return 0, fmt.Errorf("invalid step %q", part[idx+1:])
			}
			step = uint(parsedStep)
		}

		lo, hi := minValue, maxValue
		switch {
		case base == "*":
			// full range
		case strings.Contains(base, "-"):
			bounds := strings.SplitN(base, "-", 2)
			parsedLo, errLo := strconv.ParseUint(bounds[0], 10, 8)
			parsedHi, errHi := strconv.ParseUint(bounds[1], 10, 8)
			if errLo != nil || errHi != nil {
				return 0, fmt.Errorf("invalid range %q", base)
			}
			lo, hi = uint(parsedLo), uint(parsedHi)
			if lo > hi {
				return 0, fmt.Errorf("range %q start exceeds end", base)
			}
		default:
			value, err := strconv.ParseUint(base, 10, 8)
			if err != nil {
				return 0, fmt.Errorf("invalid value %q", base)
			}
			lo, hi = uint(value), uint(value)
			if step > 1 {
				// "5/10" means 5,15,25,... through the field max.
				hi = maxValue
			}
		}

		if lo < minValue || hi > maxValue {
			return 0, fmt.Errorf("value out of range %d-%d in %q", minValue, maxValue, part)
		}
		for v := lo; v <= hi; v += step {
			if v >= bits {
				break
			}
			set |= 1 << v
		}
	}
	return set, nil
}

// Next returns the next time strictly after `from` at which the schedule
// fires, evaluated in the local timezone.
func (s *Schedule) Next(from time.Time) time.Time {
	next := from.Local().Truncate(time.Minute).Add(time.Minute)
	deadline := from.Add(maxLookahead)

	for !next.After(deadline) {
		if !s.matchesMonth(next) {
			next = firstOfNextMonth(next)
			continue
		}
		if !s.matchesDay(next) {
			next = startOfNextDay(next)
			continue
		}
		if !s.matchesHour(next) {
			next = nextHour(next)
			continue
		}
		if !s.matchesMinute(next) {
			next = next.Add(time.Minute)
			continue
		}
		return next
	}
	return next
}

func (s *Schedule) matchesMinute(t time.Time) bool {
	return s.minutes&(1<<uint(t.Minute())) != 0
}

func (s *Schedule) matchesHour(t time.Time) bool {
	return s.hours&(1<<uint(t.Hour())) != 0
}

func (s *Schedule) matchesDay(t time.Time) bool {
	domMatch := s.daysOfM&(1<<uint(t.Day())) != 0
	dowMatch := s.daysOfW&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domAny && s.dowAny:
		return true
	case s.domAny:
		return dowMatch
	case s.dowAny:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

func (s *Schedule) matchesMonth(t time.Time) bool {
	return s.months&(1<<uint(t.Month())) != 0
}

func firstOfNextMonth(t time.Time) time.Time {
	year, month, _ := t.Date()
	return time.Date(year, month+1, 1, 0, 0, 0, 0, t.Location())
}

func startOfNextDay(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day+1, 0, 0, 0, 0, t.Location())
}

func nextHour(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, t.Hour()+1, 0, 0, 0, t.Location())
}
