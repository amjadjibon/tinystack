package cloudformation

import "testing"

func TestRedshiftRejectsParameterValues(t *testing.T) {
	for _, value := range []any{
		nil, "invalid",
		[]any{map[string]any{"ParameterName": "enable_user_activity_logging", "ParameterValue": "true"}},
	} {
		if err := redshiftParameters(props{"Parameters": value}); err == nil {
			t.Fatal("accepted unsupported Parameters", value)
		}
	}

	for _, p := range []props{{}, {"Parameters": []any{}}} {
		if err := redshiftParameters(p); err != nil {
			t.Fatal(err)
		}
	}
}
