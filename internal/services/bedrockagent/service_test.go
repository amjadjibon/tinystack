package bedrockagent

import (
	"context"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	"github.com/amjadjibon/tinystack/internal/services/bedrock"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestGeneratedInputConstraints(t *testing.T) {
	store := memory.New(nil, nil)
	service := New(store, bedrock.New(store))
	descriptor := api.Descriptor(service)
	for _, input := range []*api.CreateAgentRequest{
		{},
		{AgentName: new("invalid name"), AgentResourceRoleArn: new("arn:aws:iam::000000000000:role/role")},
		{AgentName: new("valid"), IdleSessionTTLInSeconds: new(int64(1))},
	} {
		if _, checkErr1 := descriptor.Operations["CreateAgent"].Invoke(
			context.Background(),
			input,
		); checkErr1 == nil ||
			!strings.Contains(checkErr1.Error(), "validation") {
			t.Errorf("expected generated validation: %v", checkErr1)
		}
	}
}

func TestOpenAPISchemaValidation(t *testing.T) {
	service := New(memory.New(nil, nil), nil)
	executor := &api.ActionGroupExecutor{CustomControl: new("RETURN_CONTROL")}
	for _, tc := range []struct {
		payload string
		valid   bool
	}{
		{`{"openapi":"3.0.0","info":{"title":"orders","version":"1"},"paths":{}}`, true},
		{"openapi: 3.0.0\ninfo:\n  title: orders\n  version: '1'\npaths: {}\n", true},
		{`{"openapi":"3.0.0"}`, false},
		{`{"swagger":"2.0","info":{"title":"orders","version":"1"},"paths":{}}`, false},
		{`not: [valid`, false},
	} {
		err := service.validateGroup(
			context.Background(),
			executor,
			&api.APISchema{Payload: new(tc.payload)},
			nil,
			nil,
			nil,
		)
		if (err == nil) != tc.valid {
			t.Errorf("schema %q: %v", tc.payload, err)
		}
	}
}
