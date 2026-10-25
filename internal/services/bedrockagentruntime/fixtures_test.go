package bedrockagentruntime

import (
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
)

func TestFixtureMatching(t *testing.T) {
	for _, tc := range []struct {
		fixture      *Fixture
		input        string
		valid, match bool
	}{
		{&Fixture{AgentID: "agent", Contains: "hello", Text: "answer"}, "say hello", true, true},
		{&Fixture{AgentID: "agent", AliasID: "other", Contains: "hello"}, "hello", true, false},
		{&Fixture{AgentID: "agent", Regex: "^hello [a-z]+$"}, "hello world", true, true},
		{&Fixture{AgentID: "agent", Regex: "["}, "hello", false, false},
		{&Fixture{AgentID: "agent", Regex: "hello", Contains: "hello"}, "hello", false, true},
		{&Fixture{AgentID: "agent", Contains: "hello", Steps: []Step{{ActionGroup: "group"}}}, "hello", false, true},
	} {
		if checkErr1 := tc.fixture.validate(); (checkErr1 == nil) != tc.valid {
			t.Errorf("validate %+v: %v", tc.fixture, checkErr1)
		}

		if got := tc.fixture.matches("agent", "alias", tc.input); got != tc.match {
			t.Errorf("matches %+v: %v", tc.fixture, got)
		}
	}
}

func TestStepParametersAndLambdaResponse(t *testing.T) {
	step := Step{ActionGroup: "lookup", Function: "find", Parameters: map[string]string{"count": "2"}}

	group := &api.AgentActionGroup{
		FunctionSchema: &api.FunctionSchema{
			Functions: new(
				[]api.Function{
					{
						Name: new("find"),
						Parameters: map[string]api.ParameterDetail{
							"count": {Type: new("integer"), Required: new(true)},
						},
					},
				},
			),
		},
	}

	parameters, err := stepParameters(new(step), group)
	if err != nil || len(parameters) != 1 || parameters[0]["type"] != "integer" {
		t.Fatalf("parameters: %+v %v", parameters, err)
	}

	state := &conversation{}

	payload := []byte(
		`{"messageVersion":"1.0","response":{"actionGroup":"lookup","function":"find","functio` +
			`nResponse":{"responseBody":{"TEXT":{"body":"ok"}}}},"sessionAttributes":{"kept":"yes"` +
			`}}`,
	)
	if checkErr2 := applyLambdaResponse(payload, new(step), state); checkErr2 != nil {
		t.Fatal(checkErr2)
	}

	if state.Attributes["kept"] != "yes" {
		t.Fatal("session attributes were lost")
	}

	for _, payload := range []string{
		`{}`,
		`{"messageVersion":"1.0","response":{"actionGroup":"lookup","function":"wrong"}}`,
		`{"messageVersion":"1.0","response":{"actionGroup":"lookup","function":"find","functio` +
			`nResponse":{"responseBody":{"TEXT":{}}}}}`,
	} {
		if checkErr3 := applyLambdaResponse([]byte(payload), new(step), state); checkErr3 == nil {
			t.Errorf("accepted malformed response: %s", payload)
		}
	}

	step.Parameters = nil
	if _, checkErr4 := stepParameters(new(step), group); checkErr4 == nil {
		t.Fatal("accepted missing required parameter")
	}
}
