// Package faults defines gateway fault rules shared by configuration and diagnostics.
package faults

import (
	"fmt"
	"math"
	"regexp"
	"slices"
)

// Error is an AWS error to inject. An omitted status defaults to 503.
type Error struct {
	Code       string `json:"code"                 yaml:"code"`
	Message    string `json:"message,omitempty"    yaml:"message"`
	StatusCode int    `json:"statusCode,omitempty" yaml:"statusCode"`
}

// Rule matches exact, case-sensitive scope fields; empty fields match anything.
// A nil Probability means 1, while an explicit zero disables the rule.
type Rule struct {
	Error       *Error   `json:"error,omitempty"       yaml:"error"`
	Probability *float64 `json:"probability,omitempty" yaml:"probability"`
	Service     string   `json:"service,omitempty"     yaml:"service"`
	Operation   string   `json:"operation,omitempty"   yaml:"operation"`
	Region      string   `json:"region,omitempty"      yaml:"region"`
	Account     string   `json:"account,omitempty"     yaml:"account"`
	LatencyMS   int      `json:"latencyMs,omitempty"   yaml:"latencyMs"`
}

var (
	name    = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	region  = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-\d+$`)
	account = regexp.MustCompile(`^\d{12}$`)
)

// Validate rejects invalid effects and selectors before publishing any rules.
func Validate(rules []Rule) error {
	if len(rules) > 1024 {
		return fmt.Errorf("faults permits at most 1024 rules")
	}

	for i, r := range rules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("faults[%d]: %w", i, err)
		}
	}

	return nil
}

func (r *Rule) Validate() error {
	for field, value := range map[string]string{"service": r.Service, "operation": r.Operation} {
		if value != "" && !name.MatchString(value) {
			return fmt.Errorf("%s must be an exact name without whitespace or wildcards", field)
		}
	}

	if r.Region != "" && !region.MatchString(r.Region) {
		return fmt.Errorf("region must be an AWS Region name")
	}

	if r.Account != "" && !account.MatchString(r.Account) {
		return fmt.Errorf("account must have 12 digits")
	}

	if r.LatencyMS < 0 || r.LatencyMS > 3600000 {
		return fmt.Errorf("latencyMs must be between 0 and 3600000")
	}

	if p := r.Probability; p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 || *p > 1) {
		return fmt.Errorf("probability must be between 0 and 1")
	}

	if r.Error == nil {
		if r.LatencyMS == 0 {
			return fmt.Errorf("an error or positive latencyMs is required")
		}

		return nil
	}

	if !name.MatchString(r.Error.Code) {
		return fmt.Errorf("error.code must be a nonempty AWS error name")
	}

	if status := r.Error.StatusCode; status != 0 && (status < 400 || status > 599) {
		return fmt.Errorf("error.statusCode must be between 400 and 599")
	}

	return nil
}

// Matches also accepts a service's signing name, which can name several services.
func (r *Rule) Matches(service, signing, operation, region, account string) bool {
	return (r.Service == "" || r.Service == service || r.Service == signing) &&
		(r.Operation == "" || r.Operation == operation) && (r.Region == "" || r.Region == region) &&
		(r.Account == "" || r.Account == account)
}

// Clone isolates the snapshot from configuration owners and API writers.
func Clone(rules []Rule) []Rule {
	out := slices.Clone(rules)
	for i := range out {
		if out[i].Error != nil {
			out[i].Error = new(*out[i].Error)
		}

		if out[i].Probability != nil {
			out[i].Probability = new(*out[i].Probability)
		}
	}

	return out
}
