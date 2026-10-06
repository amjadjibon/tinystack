package iot

import "testing"

func TestPolicyEvaluation(t *testing.T) {
	const arn = "arn:aws:iot:us-east-1:000000000000:"
	vars := map[string]string{
		varClientID: "probe-1", varThingName: "probe-1", varIsAttached: "true", varAttribute + "floor]": "3",
	}
	for _, tc := range []struct {
		vars                           map[string]string
		name, policy, action, resource string
		want                           bool
	}{
		{
			vars, "allowed",
			`{"Statement":{"Effect":"Allow","Action":"iot:Publish","Resource":"*"}}`,
			"iot:Publish", "topic/a", true,
		},
		{
			vars, "action wildcard, any case",
			`{"Statement":{"Effect":"Allow","Action":"IOT:*","Resource":"*"}}`,
			"iot:Receive", "topic/a", true,
		},
		{
			vars, "other action",
			`{"Statement":{"Effect":"Allow","Action":"iot:Publish","Resource":"*"}}`,
			"iot:Subscribe", "topicfilter/a", false,
		},
		{
			vars, "deny wins",
			`{"Statement":[{"Effect":"Allow","Action":"iot:*","Resource":"*"},` +
				`{"Effect":"Deny","Action":"iot:Publish","Resource":"` + arn + `topic/secret/*"}]}`,
			"iot:Publish", "topic/secret/x", false,
		},
		{
			vars, "variable",
			`{"Statement":{"Effect":"Allow","Action":"iot:Connect",` +
				`"Resource":"` + arn + `client/${iot:Connection.Thing.ThingName}"}}`,
			"iot:Connect", "client/probe-1", true,
		},
		{
			map[string]string{varClientID: "probe-1"},
			"unresolved variable",
			`{"Statement":{"Effect":"Allow","Action":"iot:Connect",` +
				`"Resource":"` + arn + `client/${iot:Connection.Thing.ThingName}"}}`,
			"iot:Connect", "client/probe-1", false,
		},
		{
			vars, "not action",
			`{"Statement":{"Effect":"Allow","NotAction":"iot:Publish","Resource":"*"}}`,
			"iot:Receive", "topic/a", true,
		},
		{
			vars, "not resource",
			`{"Statement":{"Effect":"Allow","Action":"iot:*","NotResource":"` + arn + `topic/a"}}`,
			"iot:Receive", "topic/a", false,
		},
		{
			vars, "attribute condition",
			`{"Statement":{"Effect":"Allow","Action":"iot:*","Resource":"*",` +
				`"Condition":{"StringEquals":{"iot:Connection.Thing.Attributes[floor]":"3"}}}}`,
			"iot:Publish", "topic/a", true,
		},
		{
			vars, "like condition",
			`{"Statement":{"Effect":"Allow","Action":"iot:*","Resource":"*",` +
				`"Condition":{"StringLike":{"iot:ClientId":"probe-*"}}}}`,
			"iot:Publish", "topic/a", true,
		},
		{
			vars, "bool condition",
			`{"Statement":{"Effect":"Allow","Action":"iot:*","Resource":"*",` +
				`"Condition":{"Bool":{"iot:Connection.Thing.IsAttached":false}}}}`,
			"iot:Publish", "topic/a", false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := parsePolicy(tc.policy)
			if err != nil {
				t.Fatal(err)
			}

			a := &attempt{vars: tc.vars, action: tc.action, resource: arn + tc.resource}
			if got := allows([]*policyDocument{doc}, a); got != tc.want {
				t.Fatalf("allows = %v, want %v", got, tc.want)
			}
		})
	}
}
