// Package schedule parses the rate and cron schedule expressions EventBridge,
// EventBridge Scheduler, and Secrets Manager share, and finds their fire
// times, in UTC unless a schedule is given a time zone.
package schedule

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed schedule expression: a fixed rate or a cron
// expression, evaluated in UTC or the time zone set with In.
type Schedule struct {
	cron   *cron
	loc    *time.Location
	period time.Duration
}

// In evaluates a cron expression's fields in loc instead of UTC and returns
// the schedule.
func (s *Schedule) In(loc *time.Location) *Schedule {
	s.loc = loc
	return s
}

func (s *Schedule) location() *time.Location {
	if s.loc == nil {
		return time.UTC
	}

	return s.loc
}

// ErrInvalid reports an expression that is not a valid rate or cron expression.
var ErrInvalid = errors.New("the schedule expression is not valid")

var rateExpression = regexp.MustCompile(`^rate\((\d+) (minute|minutes|hour|hours|day|days)\)$`)

// Parse parses "rate(<n> <unit>)", whose unit (minutes, hours, or days) is
// singular only for a value of 1, or "cron(<minutes> <hours> <day-of-month>
// <month> <day-of-week> <year>)".
func Parse(text string) (*Schedule, error) {
	if len(text) > 256 {
		return nil, ErrInvalid
	}

	if body, ok := strings.CutPrefix(text, "cron("); ok && strings.HasSuffix(body, ")") {
		c, err := parseCron(strings.TrimSuffix(body, ")"))
		if err != nil {
			return nil, err
		}

		return &Schedule{cron: c}, nil
	}

	match := rateExpression.FindStringSubmatch(text)
	if match == nil {
		return nil, ErrInvalid
	}

	n, err := strconv.Atoi(match[1])
	if err != nil || n < 1 || (n == 1) != !strings.HasSuffix(match[2], "s") {
		return nil, ErrInvalid
	}

	unit := map[string]time.Duration{"minute": time.Minute, "hour": time.Hour, "day": 24 * time.Hour}
	return &Schedule{period: time.Duration(n) * unit[strings.TrimSuffix(match[2], "s")]}, nil
}

// Period is the period of a rate expression, and zero for a cron expression.
func (s *Schedule) Period() time.Duration { return s.period }

// First is the first fire time of a schedule set at now: one period later for
// a rate, the next matching minute for a cron expression. It reports false
// when a cron expression never fires again.
func (s *Schedule) First(now time.Time) (time.Time, bool) {
	if s.cron != nil {
		return s.cron.next(now, s.location())
	}

	return now.Add(s.period), true
}

// After is the first fire time later than now for a schedule that was due at
// due. Runs missed in between are skipped, not replayed.
func (s *Schedule) After(due, now time.Time) (time.Time, bool) {
	if s.cron != nil {
		return s.cron.next(now, s.location())
	}

	return due.Add(now.Sub(due).Truncate(s.period) + s.period), true
}

// cron is a parsed cron expression; each field is the set of values it
// allows. Exactly one of the day-of-month and day-of-week fields is "?", and
// that one is nil.
type cron struct {
	dayRule                                       func(time.Time) bool
	minutes, hours, days, months, weekdays, years []bool
}

const (
	firstYear = 1970
	lastYear  = 2199
)

var (
	monthNames = []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}
	dayNames   = []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}
)

func parseCron(body string) (*cron, error) {
	fields := strings.Fields(body)
	if len(fields) != 6 {
		return nil, ErrInvalid
	}

	// One of day-of-month and day-of-week must be "?", and only those may be.
	if (fields[2] == "?") == (fields[4] == "?") || strings.Contains(fields[0]+fields[1]+fields[3]+fields[5], "?") {
		return nil, ErrInvalid
	}

	var c cron
	var err error
	for _, f := range []struct {
		set       *[]bool
		text      string
		names     []string
		low, high int
	}{
		{&c.minutes, fields[0], nil, 0, 59},
		{&c.hours, fields[1], nil, 0, 23},
		{&c.days, fields[2], nil, 1, 31},
		{&c.months, fields[3], monthNames, 1, 12},
		{&c.weekdays, fields[4], dayNames, 1, 7},
		{&c.years, fields[5], nil, firstYear, lastYear},
	} {
		if f.text == "?" {
			continue
		}

		day := strings.ToUpper(f.text)
		if (f.set == &c.days && strings.ContainsAny(day, "LW")) ||
			(f.set == &c.weekdays && strings.ContainsAny(day, "L#")) {
			c.dayRule, err = parseDayRule(day, f.set == &c.days)
			if err != nil {
				return nil, err
			}

			continue
		}

		if *f.set, err = cronField(f.text, f.names, f.low, f.high); err != nil {
			return nil, err
		}
	}

	return &c, nil
}

func cronNumber(s string, names []string, low, high int) (int, bool) {
	for i, name := range names {
		if strings.EqualFold(s, name) {
			return low + i, true
		}
	}

	n, err := strconv.Atoi(s)
	return n, err == nil && n >= low && n <= high
}

func parseDayRule(text string, monthDay bool) (func(time.Time) bool, error) {
	if monthDay {
		if text == "L" {
			return func(t time.Time) bool { return t.Day() == monthLength(t) }, nil
		}

		if value, ok := strings.CutSuffix(text, "W"); ok {
			day, valid := cronNumber(value, nil, 1, 31)
			if !valid {
				return nil, ErrInvalid
			}

			return func(t time.Time) bool { return t.Day() == nearestWeekday(t, day) }, nil
		}

		return nil, ErrInvalid
	}

	if text == "L" {
		return func(t time.Time) bool { return t.Weekday() == time.Saturday }, nil
	}

	if value, ok := strings.CutSuffix(text, "L"); ok {
		day, valid := cronNumber(value, dayNames, 1, 7)
		if !valid {
			return nil, ErrInvalid
		}

		return func(t time.Time) bool { return int(t.Weekday())+1 == day && t.Day()+7 > monthLength(t) }, nil
	}

	if value, ordinal, ok := strings.Cut(text, "#"); ok {
		day, dayValid := cronNumber(value, dayNames, 1, 7)
		nth, nthValid := cronNumber(ordinal, nil, 1, 5)
		if !dayValid || !nthValid {
			return nil, ErrInvalid
		}

		return func(t time.Time) bool { return int(t.Weekday())+1 == day && (t.Day()-1)/7+1 == nth }, nil
	}

	return nil, ErrInvalid
}

func monthLength(t time.Time) int {
	return time.Date(t.Year(), t.Month()+1, 0, 12, 0, 0, 0, time.UTC).Day()
}

func nearestWeekday(t time.Time, day int) int {
	last := monthLength(t)
	if day > last {
		return 0
	}

	switch time.Date(t.Year(), t.Month(), day, 12, 0, 0, 0, time.UTC).Weekday() {
	case time.Saturday:
		if day == 1 {
			return 3
		}

		return day - 1
	case time.Sunday:
		if day == last {
			return day - 2
		}

		return day + 1
	default:
		return day
	}
}

// cronField parses a comma-separated list of "*", values, ranges "a-b", and
// increments "*/n", "a/n", or "a-b/n". names spell the values from low.
func cronField(text string, names []string, low, high int) ([]bool, error) {
	set := make([]bool, high+1)
	for _, part := range strings.Split(text, ",") {
		span, stepText, stepped := strings.Cut(part, "/")
		step := 1
		if stepped {
			var err error
			if step, err = strconv.Atoi(stepText); err != nil || step < 1 {
				return nil, ErrInvalid
			}
		}

		from, to := low, high
		if span != "*" {
			start, end, isRange := strings.Cut(span, "-")
			var ok bool
			if from, ok = cronNumber(start, names, low, high); !ok {
				return nil, ErrInvalid
			}

			// "a/n" runs from a to the end of the field.
			to = from
			if stepped {
				to = high
			}

			if isRange {
				if to, ok = cronNumber(end, names, low, high); !ok || to < from {
					return nil, ErrInvalid
				}
			}
		}

		for v := from; ; v += step {
			set[v] = true
			if step > to-v {
				break
			}
		}
	}

	return set, nil
}

// matchesDay reports whether t falls on an allowed day of the month or of the
// week, whichever the expression gives.
func (c *cron) matchesDay(t time.Time) bool {
	if c.dayRule != nil {
		return c.dayRule(t)
	}

	if c.days != nil {
		return c.days[t.Day()]
	}

	return c.weekdays[int(t.Weekday())+1]
}

// next is the first minute after now that the expression matches in loc,
// returned in UTC.
func (c *cron) next(now time.Time, loc *time.Location) (time.Time, bool) {
	t := now.In(loc).Truncate(time.Minute).Add(time.Minute)
	for t.Year() <= lastYear {
		switch {
		case t.Year() < firstYear || !c.years[t.Year()]:
			t = time.Date(t.Year()+1, time.January, 1, 0, 0, 0, 0, loc)
		case !c.months[int(t.Month())]:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !c.matchesDay(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case !c.hours[t.Hour()]:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case !c.minutes[t.Minute()]:
			t = t.Add(time.Minute)
		default:
			return t.UTC(), true
		}
	}

	return time.Time{}, false
}
