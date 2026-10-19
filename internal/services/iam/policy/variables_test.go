package policy

import "testing"

// Simulator context entries are caller-controlled strings, so a substituted value is literal:
// one that itself reads ${aws:username} is never expanded again.
func TestVariableValuesAreLiteral(t *testing.T) {
	document := `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"s3:GetObject",` +
		`"Resource":"arn:aws:s3:::bucket/${aws:username}"}}`
	values := map[string][]string{"aws:username": {"${aws:username}"}}
	for resource, want := range map[string]bool{
		"arn:aws:s3:::bucket/${aws:username}": true,
		"arn:aws:s3:::bucket/object":          false,
	} {
		decision := Evaluate([]Document{{JSON: document}}, "s3:GetObject", resource,
			Context{Simulation: true, Values: values})
		if decision.Allowed != want || len(decision.Unsupported) > 0 {
			t.Fatalf("%s: %+v", resource, decision)
		}
	}
}
