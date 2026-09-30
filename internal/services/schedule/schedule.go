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

var (
	// ErrInvalid reports an expression that is not a valid rate or cron
	// expression.
	ErrInvalid = errors.New("the schedule expression is not valid")
	// ErrUnsupported reports L, W, or # in a cron expression, which AWS
	// accepts but TinyStack does not evaluate.
	ErrUnsupported = errors.New("TinyStack does not support L, W, or # in cron expressions")
)

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

		if *f.set, err = cronField(f.text, f.names, f.low, f.high); err != nil {
			return nil, err
		}
	}

	return &c, nil
}

// cronField parses a comma-separated list of "*", values, ranges "a-b", and
// increments "*/n", "a/n", or "a-b/n". names spell the values from low.
func cronField(text string, names []string, low, high int) ([]bool, error) {
	number := func(s string) (int, bool) {
		for i, name := range names {
			if strings.EqualFold(s, name) {
				return low + i, true
			}
		}

		n, err := strconv.Atoi(s)
		return n, err == nil && n >= low && n <= high
	}

	// bad reports a value that is neither a number nor a name; L, W, and #
	// are valid in AWS but not supported.
	bad := func(s string) error {
		if strings.ContainsAny(s, "LW#") {
			return ErrUnsupported
		}

		return ErrInvalid
	}

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
			if from, ok = number(start); !ok {
				return nil, bad(start)
			}

			// "a/n" runs from a to the end of the field.
			to = from
			if stepped {
				to = high
			}

			if isRange {
				if to, ok = number(end); !ok || to < from {
					return nil, bad(end)
				}
			}
		}

		for v := from; v <= to; v += step {
			set[v] = true
		}
	}

	return set, nil
}

// matchesDay reports whether t falls on an allowed day of the month or of the
// week, whichever the expression gives.
func (c *cron) matchesDay(t time.Time) bool {
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
