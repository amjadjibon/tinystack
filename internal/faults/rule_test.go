package faults

import (
	"math"
	"testing"
)

func TestRuleMatches(t *testing.T) {
	for _, tc := range []struct {
		rule Rule
		want bool
	}{
		{Rule{}, true},
		{Rule{Service: "eventbridge"}, true},
		{Rule{Service: "events"}, true},
		{Rule{Service: "s3"}, false},
		{Rule{Operation: "PutEvents", Region: "us-east-1", Account: "000000000000"}, true},
		{Rule{Operation: "putevents"}, false},
		{Rule{Region: "eu-west-1"}, false},
		{Rule{Account: "111111111111"}, false},
	} {
		if got := tc.rule.Matches("eventbridge", "events", "PutEvents", "us-east-1", "000000000000"); got != tc.want {
			t.Fatalf("%+v matches = %v, want %v", tc.rule, got, tc.want)
		}
	}
}

func TestRuleValidation(t *testing.T) {
	for _, r := range []Rule{
		{},
		{LatencyMS: -1},
		{LatencyMS: 3600001},
		{LatencyMS: 1, Probability: new(math.NaN())},
		{LatencyMS: 1, Probability: new(math.Inf(1))},
		{LatencyMS: 1, Probability: new(-0.1)},
		{LatencyMS: 1, Probability: new(1.1)},
		{LatencyMS: 1, Service: "s*"},
		{LatencyMS: 1, Operation: "bad name"},
		{LatencyMS: 1, Region: "anything"},
		{LatencyMS: 1, Account: "123"},
		{Error: &Error{}},
		{Error: &Error{Code: "X", StatusCode: 399}},
		{Error: &Error{Code: "X", StatusCode: 600}},
	} {
		if err := Validate([]Rule{r}); err == nil {
			t.Fatalf("accepted %+v", r)
		}
	}

	for _, r := range []Rule{
		{LatencyMS: 1, Probability: new(0.0)},
		{LatencyMS: 3600000, Probability: new(1.0)},
		{Error: &Error{Code: "SlowDown"}},
		{Error: &Error{Code: "ThrottlingException", StatusCode: 400}},
	} {
		if err := Validate([]Rule{r}); err != nil {
			t.Fatal(err)
		}
	}

	if err := Validate(make([]Rule, 1025)); err == nil {
		t.Fatal("accepted too many rules")
	}
}

func TestClone(t *testing.T) {
	original := []Rule{{Error: &Error{Code: "SlowDown"}, Probability: new(0.5)}}
	clone := Clone(original)
	clone[0].Error.Code = "X"
	*clone[0].Probability = 0
	if original[0].Error.Code != "SlowDown" || *original[0].Probability != 0.5 {
		t.Fatal("clone shares mutable fields")
	}
}
