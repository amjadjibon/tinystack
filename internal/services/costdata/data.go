// Package costdata stores user-supplied, account-wide USD cost fixtures.
package costdata

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	MaxLines    = 10000
	MaxFixtures = 100
)

type Line struct {
	ID            string            `json:"id"`
	Start         string            `json:"start"`
	End           string            `json:"end"`
	Service       string            `json:"service"`
	Region        string            `json:"region,omitempty"`
	LinkedAccount string            `json:"linkedAccount,omitempty"`
	UsageType     string            `json:"usageType,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	UnblendedCost string            `json:"unblendedCost"`
	Currency      string            `json:"currency"`
	Estimated     bool              `json:"estimated,omitempty"`
}

type Input struct {
	Provenance string `json:"provenance"`
	Lines      []Line `json:"lines"`
}

type Fixture struct {
	UpdatedAt time.Time `json:"updatedAt"`
	ID        string    `json:"id"`
	Revision  string    `json:"revision"`
	Input
	LineCount int `json:"lineCount"`
}

var (
	DecimalPattern = regexp.MustCompile(`^-?(0|[1-9]\d{0,29})(\.\d{1,12})?$`)
	IDPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err == nil {
		return t, nil
	}

	t, err = time.Parse(time.RFC3339, s)
	if err != nil {
		return t, fmt.Errorf("time must be a UTC date or RFC3339 timestamp")
	}

	_, offset := t.Zone()
	if offset != 0 {
		return t, fmt.Errorf("timestamps must use UTC")
	}

	return t.UTC(), nil
}

func Validate(in *Input, account string) error {
	if in.Lines == nil || len(in.Lines) > MaxLines {
		return fmt.Errorf("lines must be an array of at most %d lines", MaxLines)
	}

	if len(in.Provenance) > 4096 {
		return fmt.Errorf("provenance exceeds 4096 bytes")
	}

	seen := map[string]bool{}
	for i := range in.Lines {
		l := &in.Lines[i]
		if !IDPattern.MatchString(l.ID) || seen[l.ID] {
			return fmt.Errorf("line IDs must be unique, 1-128 ASCII letters, digits, underscores or hyphens")
		}

		seen[l.ID] = true
		start, err := ParseTime(l.Start)
		if err != nil {
			return err
		}

		end, err := ParseTime(l.End)
		if err != nil {
			return err
		}

		day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
		if !end.After(start) || end.After(day.AddDate(0, 0, 1)) {
			return fmt.Errorf(
				"line %s must have start < end and fit within one UTC day (end may be next midnight)",
				l.ID,
			)
		}

		if l.Service == "" || len(l.Service) > 1024 || len(l.Region) > 256 || len(l.UsageType) > 1024 ||
			len(l.Tags) > 50 {
			return fmt.Errorf("invalid service/dimension/tag limits on line %s", l.ID)
		}

		for k, v := range l.Tags {
			if k == "" || len(k) > 128 || len(v) > 256 {
				return fmt.Errorf("tag keys require 1-128 bytes and values at most 256 bytes")
			}
		}

		if !DecimalPattern.MatchString(l.UnblendedCost) || l.Currency != "USD" {
			return fmt.Errorf(
				"line %s requires USD and a decimal string (up to 30 integer and 12 fractional digits)",
				l.ID,
			)
		}

		if l.LinkedAccount == "" {
			l.LinkedAccount = account
		}

		if l.LinkedAccount != account {
			return fmt.Errorf("linkedAccount must equal fixture account")
		}
	}

	return nil
}

func Load(ctx context.Context, store storage.Store, scope request.Scope) ([]*Fixture, error) {
	var rows []*Fixture
	err := store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		rows, err = storage.Records[Fixture](tx, scope.Prefix("costdata", "fixture"))
		return err
	})

	return rows, err
}

func Lines(fixtures []*Fixture) []Line {
	var lines []Line
	for _, f := range fixtures {
		lines = append(lines, f.Lines...)
	}

	return lines
}

func Dimension(l *Line, key string) (string, bool) {
	switch key {
	case "SERVICE":
		return l.Service, true
	case "REGION":
		return l.Region, true
	case "LINKED_ACCOUNT":
		return l.LinkedAccount, true
	case "USAGE_TYPE":
		return l.UsageType, true
	}

	return "", false
}

func Sum(lines []Line) string {
	total := new(big.Rat)
	for i := range lines {
		l := &lines[i]
		n, _ := new(big.Rat).SetString(l.UnblendedCost)
		total.Add(total, n)
	}

	s := total.FloatString(12)
	for s != "" && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}

	if s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}

	return s
}
