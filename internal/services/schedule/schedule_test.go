package schedule

import (
	"errors"
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	for _, text := range []string{
		"rate(1 minute)", "rate(5 minutes)", "rate(1 hour)", "rate(2 days)",
		"cron(0 12 * * ? *)", "cron(0/15 * ? * MON-FRI *)", "cron(30 9 1,15 JAN,JUL ? 2026-2030)",
		"cron(5-55/10 */2 ? * sun *)",
	} {
		if _, err := Parse(text); err != nil {
			t.Errorf("%s: %v", text, err)
		}
	}

	for _, text := range []string{
		"", "rate(0 minutes)", "rate(1 minutes)", "rate(5 minute)", "rate(5 weeks)", "rate(-1 minutes)",
		"cron(0 12 * * *)", "cron(0 12 * * * *)", "cron(0 12 ? * ? *)", "cron(60 * * * ? *)",
		"cron(0 24 * * ? *)", "cron(0 0 32 * ? *)", "cron(0 0 ? * 8 *)", "cron(0 0 * 13 ? *)",
		"cron(? 0 * * ? *)", "cron(0 0 5-1 * ? *)", "cron(0 0 */0 * ? *)", "cron(0 0 * * ? 2200)",
		"cron(0 0 L * ? *)", "cron(0 0 ? * 6L *)", "cron(0 0 15W * ? *)", "cron(0 0 ? * 3#2 *)",
	} {
		if _, err := Parse(text); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: got %v", text, err)
		}
	}
}

func TestCronNext(t *testing.T) {
	now := time.Date(2026, time.March, 14, 10, 7, 30, 0, time.UTC) // a Saturday
	for text, want := range map[string]time.Time{
		"cron(* * * * ? *)":             time.Date(2026, time.March, 14, 10, 8, 0, 0, time.UTC),
		"cron(0/15 * * * ? *)":          time.Date(2026, time.March, 14, 10, 15, 0, 0, time.UTC),
		"cron(0 12 * * ? *)":            time.Date(2026, time.March, 14, 12, 0, 0, 0, time.UTC),
		"cron(0 9 ? * MON-FRI *)":       time.Date(2026, time.March, 16, 9, 0, 0, 0, time.UTC),
		"cron(0 0 1 * ? *)":             time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC),
		"cron(30 6 29 FEB ? *)":         time.Date(2028, time.February, 29, 6, 30, 0, 0, time.UTC),
		"cron(0 0 ? * 1 2027)":          time.Date(2027, time.January, 3, 0, 0, 0, 0, time.UTC),
		"cron(15 10 14 MAR ? 2026)":     time.Date(2026, time.March, 14, 10, 15, 0, 0, time.UTC),
		"cron(0 10 14 MAR ? 2026,2029)": time.Date(2029, time.March, 14, 10, 0, 0, 0, time.UTC),
	} {
		s, err := Parse(text)
		if err != nil {
			t.Fatal(err)
		}

		if got, ok := s.First(now); !ok || !got.Equal(want) {
			t.Errorf("%s: got %v, want %v", text, got, want)
		}
	}

	s, err := Parse("cron(0 10 14 MAR ? 2026)")
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := s.First(now); ok {
		t.Errorf("a past-only expression fires at %v", got)
	}
}

func TestRateAfter(t *testing.T) {
	s, err := Parse("rate(5 minutes)")
	if err != nil || s.Period() != 5*time.Minute {
		t.Fatalf("got %v, %v", s, err)
	}

	due := time.Date(2026, time.March, 14, 10, 0, 0, 0, time.UTC)
	// A late check fires once and keeps the cadence.
	if got, _ := s.After(due, due.Add(12*time.Minute)); !got.Equal(due.Add(15 * time.Minute)) {
		t.Errorf("got %v", got)
	}
}

func TestCronInTimeZone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip(err)
	}

	s, err := Parse("cron(0 9 * * ? *)")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.March, 14, 10, 0, 0, 0, time.UTC) // 19:00 in Tokyo
	got, ok := s.In(tokyo).First(now)
	if want := time.Date(
		2026,
		time.March,
		15,
		0,
		0,
		0,
		0,
		time.UTC,
	); !ok || !got.Equal(want) ||
		got.Location() != time.UTC {
		t.Fatalf("got %v, want %v", got, want)
	}
}
